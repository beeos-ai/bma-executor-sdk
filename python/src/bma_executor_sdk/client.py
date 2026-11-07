"""The executor ("hand") end of the executor protocol: ExecutorClient.

Mirrors go/executor/client.go: token bootstrap, outbound channel, JSON-RPC,
registration with resume, heartbeat, reconnect with backoff, dispatch, session
lifecycle, tool cancellation, retried tool results, and the MCP channel.
"""

from __future__ import annotations

import asyncio
import random as _random
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, TypeVar

import httpx

from ._methods import Call, Notification, Request
from ._rpc import ClientStoppedError, ExecutorError, NotConnectedError, Peer, RPCError
from .session_context import SessionContext, SessionContexts
from .generated import (
    METHOD_EXECUTOR_CAPABILITIES_UPDATE,
    METHOD_EXECUTOR_CLOSE,
    METHOD_EXECUTOR_DRAIN,
    METHOD_EXECUTOR_HEARTBEAT,
    METHOD_EXECUTOR_REGISTER,
    METHOD_EXECUTOR_TOKEN_REFRESH,
    METHOD_SESSION_ASSIGN,
    METHOD_SESSION_ENDED,
    METHOD_SESSION_READY,
    METHOD_SESSION_STOP,
    METHOD_TOOL_CALL,
    METHOD_TOOL_CANCEL,
    METHOD_TOOL_PROGRESS,
    METHOD_TOOL_RESULT,
    METHODS,
    Ack,
    Capabilities,
    ChannelScope,
    ConnectionState,
    ExecutorDrainParams,
    ExecutorDrainResult,
    ExecutorInfo,
    ExecutorLoad,
    ExecutorRegisterResult,
    ExecutorTokenResponse,
    JsonRpcErrorCode,
    ProtocolVersion,
    SessionAssignParams,
    SessionAssignResult,
    SessionEndedParams,
    SessionReadyParams,
    SessionStopParams,
    SessionStopResult,
    ToolCallParams,
    ToolOutcome,
    ToolResultParams,
)
from .tools import ExecutorTools, ToolContext, ToolError, missing_required, render
from .transport import CLOSE_NORMAL, MAX_FRAME_BYTES, TERMINAL_CLOSE_CODES, ChannelClosed, Connection, Dialer, channel_url, dial

if TYPE_CHECKING:
    from .agent import AgentHandler, _AgentRuntime
    from .generated import AgentCapability

DEFAULT_BASE_URL = "https://api.bma.beeos.ai"
DEFAULT_REQUEST_TIMEOUT = 30.0
# 0 reconnects until stop(), as gRPC connection backoff and long-running
# agents (GitHub Actions runner, Buildkite agent) do.
DEFAULT_MAX_RECONNECT_ATTEMPTS = 0
DEFAULT_RECONNECT_BASE_DELAY = 1.0
DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS = 5
DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY = 0.25
DEFAULT_HEARTBEAT_INTERVAL = 15.0
PROTOCOL_V1 = ProtocolVersion.BmaExecutorV1.value
PROTOCOL_V11 = ProtocolVersion.BmaExecutorV11.value
_MAX_TRACKED_TOOL_RESULTS = 8192
_MAX_TOOL_RESULT_RETRY_DELAY = 10.0
_MAX_RECONNECT_DELAY = 60.0
# Registrations the platform may refuse in a row before the client gives up.
_MAX_REFUSED_REGISTRATIONS = 3
# Fraction of a token's lifetime after which executor.token.refresh is sent.
_TOKEN_REFRESH_AT = 0.7

TokenSource = Callable[[], Awaitable[ExecutorTokenResponse]]
SessionAssignHandler = Callable[[SessionAssignParams], Awaitable[SessionAssignResult]]
SessionStopHandler = Callable[[SessionStopParams], Awaitable[SessionStopResult]]
DrainHandler = Callable[[ExecutorDrainParams], Awaitable[ExecutorDrainResult]]
P = TypeVar("P")
R = TypeVar("R")


@dataclass(frozen=True)
class LogEvent:
    level: str  # debug | info | warn | error
    message: str
    error: BaseException | None = None


def static_token(ws_url: str, token: str = "") -> TokenSource:
    """TokenSource for a harness reached directly (a local harness on
    ``ws://127.0.0.1``, ``unix:///path.sock``, ``npipe:////./pipe/name`` or in-process). token may be empty
    when the harness has no authenticate hook."""

    async def source() -> ExecutorTokenResponse:
        return {"executor_id": "", "token": token, "expires_in": 0, "ws_url": ws_url, "channel_scope": []}

    return source


class _RunningToolCall:
    __slots__ = ("task", "handler", "canceled", "reason")

    def __init__(self) -> None:
        self.task: asyncio.Task[Any] | None = None
        self.handler: asyncio.Task[Any] | None = None
        self.canceled = False
        self.reason = ""


class ExecutorClient:
    """Outbound executor client (Go: ``Client``)."""

    def __init__(
        self,
        *,
        environment_id: str,
        executor: ExecutorInfo,
        capabilities: Capabilities | None = None,
        environment_key: str = "",
        base_url: str = DEFAULT_BASE_URL,
        ws_url: str = "",
        token_source: TokenSource | None = None,
        dialer: Dialer | None = None,
        http_client: httpx.AsyncClient | None = None,
        request_timeout: float = DEFAULT_REQUEST_TIMEOUT,
        heartbeat_interval: float = DEFAULT_HEARTBEAT_INTERVAL,
        heartbeat_load: Callable[[], ExecutorLoad | None] | None = None,
        max_reconnect_attempts: int = DEFAULT_MAX_RECONNECT_ATTEMPTS,
        reconnect_base_delay: float = DEFAULT_RECONNECT_BASE_DELAY,
        reconnect_max_delay: float = _MAX_RECONNECT_DELAY,
        random: Callable[[], float] = _random.random,
        tool_result_retry_attempts: int = DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS,
        tool_result_retry_base_delay: float = DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY,
        on_tool_result_lost: Callable[[ToolResultParams, BaseException], None] | None = None,
        on_log: Callable[[LogEvent], None] | None = None,
    ) -> None:
        # environment_id may be empty: a machine-scoped executor registers
        # without one (executor.register in the v1.1 contract).
        if token_source is None and not environment_key.strip():
            raise ValueError("bma_executor_sdk: environment_key is required without token_source")
        if not executor.get("name"):
            raise ValueError("bma_executor_sdk: executor['name'] is required")
        self._environment_id = environment_id
        self._executor = executor
        self._capabilities: Capabilities = capabilities or {}
        self._environment_key = environment_key
        # The deployment origin (as BMA_BASE_URL in bma-sdk); a trailing
        # /api/v1 is dropped so the /v1/executors paths are not nested.
        self._base_url = base_url.rstrip("/").removesuffix("/api/v1")
        self._ws_url = ws_url
        self._token_source = token_source
        self._dialer: Dialer = dialer or dial
        self._http_client = http_client
        self._request_timeout = request_timeout
        self._default_heartbeat = heartbeat_interval
        self._heartbeat_load = heartbeat_load
        self._max_reconnect_attempts = max_reconnect_attempts
        self._reconnect_base_delay = reconnect_base_delay
        self._reconnect_max_delay = reconnect_max_delay
        self._random = random
        self._tool_result_retry_attempts = max(1, tool_result_retry_attempts)
        self._tool_result_retry_base_delay = tool_result_retry_base_delay
        self._on_tool_result_lost = on_tool_result_lost
        self._on_log = on_log

        self._peer: Peer | None = None
        self._state = ConnectionState.Disconnected
        self._stopped = False
        self._stop_called = False
        self._closed = asyncio.Event()
        self._close_error: BaseException | None = None
        self._register_result: ExecutorRegisterResult | None = None
        self._connection_id = ""
        self._executor_id = ""
        self._heartbeat_interval = heartbeat_interval
        self._sessions: set[str] = set()
        self._reconnect_attempts = 0
        # Registrations refused in a row; the peer that stayed registered for
        # one heartbeat interval; set (then replaced) whenever one does.
        self._refused_streak = 0
        self._stable_peer: Peer | None = None
        self._stable = asyncio.Event()
        self._completed_tool_results: dict[str, None] = {}
        self._seen_tool_calls: dict[str, None] = {}
        self._running_tool_calls: dict[str, _RunningToolCall] = {}
        self.tools = ExecutorTools(self._tools_changed)
        """Typed tool handlers; the shell and files capabilities follow what is registered."""
        self._session_assign_handler: SessionAssignHandler | None = None
        self._session_stop_handler: SessionStopHandler | None = None
        self._drain_handler: DrainHandler | None = None
        self._request_handlers: dict[str, Callable[[Any], Awaitable[Any]]] = {}
        self._tasks: set[asyncio.Task[Any]] = set()
        # Set after every successful executor.register, then replaced, so agent
        # outboxes resend after a reconnect.
        self._registered = asyncio.Event()
        self._agent: _AgentRuntime | None = None
        self._session_contexts = SessionContexts(
            base_url=self._base_url, environment_id=environment_id, environment_key=environment_key,
            http_client=http_client, timeout=request_timeout,
        )

    # --- lifecycle ---

    async def start(self) -> ExecutorRegisterResult:
        """Fetch a token, connect, register, and start heartbeats."""
        if self._stopped:
            raise ClientStoppedError()
        if self._peer is not None:
            raise RuntimeError("bma_executor_sdk: client already started")
        self._state = ConnectionState.Connecting
        try:
            token = await self._fetch_token()
            await self._connect(token)
        except BaseException:
            if not self._stopped:
                self._state = ConnectionState.Disconnected
            raise
        self._spawn(self._heartbeat_loop())
        assert self._register_result is not None
        return self._register_result

    async def stop(self, reason: str = "") -> None:
        """Send executor.close, close the channel and fail pending calls."""
        if self._stop_called:
            return
        self._stop_called = True
        self._end(None)
        peer, self._peer = self._peer, None
        if peer is not None:
            try:
                await asyncio.wait_for(peer.notify(METHOD_EXECUTOR_CLOSE, {"reason": reason}), 2)
            except Exception as error:  # best effort: the close below still ends the channel
                self._log("debug", "executor.close not sent", error)
            await peer.conn.close(CLOSE_NORMAL, reason)
            peer.fail_pending(ClientStoppedError())
            peer.cancel_tasks()
        for running in self._running_tool_calls.values():
            if running.task is not None:
                running.task.cancel()
        for task in list(self._tasks):
            task.cancel()
        if self._tasks:
            await asyncio.gather(*self._tasks, return_exceptions=True)

    async def wait_ready(self, timeout: float | None = None) -> None:
        """Wait until the executor is registered and the registration stayed
        open for one heartbeat interval. ``state`` is Ready as soon as
        executor.register returns, but a machine-scoped executor that declared
        neither the process nor the agent capability is closed with 4002 right
        after that, so only wait_ready() separates a usable executor from a
        registration about to be refused. Raises the error of a client that
        ended (see wait_closed), ClientStoppedError after stop() and
        TimeoutError after ``timeout`` seconds."""

        async def wait() -> None:
            while True:
                if self._stopped:
                    raise self._close_error or ClientStoppedError()
                if self._peer is not None and self._stable_peer is self._peer:
                    return
                stable = asyncio.ensure_future(self._stable.wait())
                closed = asyncio.ensure_future(self._closed.wait())
                try:
                    await asyncio.wait({stable, closed}, return_when=asyncio.FIRST_COMPLETED)
                finally:
                    stable.cancel()
                    closed.cancel()

        try:
            await asyncio.wait_for(wait(), timeout)
        except asyncio.TimeoutError as error:  # not the builtin TimeoutError before Python 3.11
            raise TimeoutError("wait_ready timed out") from error

    async def wait_closed(self) -> None:
        """Wait until the client stops for good (Go: ``Done`` and ``Err``).
        Returns after stop() or executor.close; raises ChannelClosed for a
        terminal close code (4003, 4004), CredentialRejectedError when the
        token endpoint rejects the credential on reconnect,
        RegistrationRefusedError after 3 refused registrations in a row, and
        ReconnectExhaustedError after a positive max_reconnect_attempts."""
        await self._closed.wait()
        if self._close_error is not None:
            raise self._close_error

    def _end(self, error: BaseException | None) -> None:
        """Stop for good; wait_closed() then returns or raises error."""
        if self._closed.is_set():
            return
        self._stopped = True
        self._state = ConnectionState.Closed
        self._close_error = error
        self._closed.set()

    @property
    def state(self) -> ConnectionState:
        return self._state

    @property
    def executor_id(self) -> str:
        return self._executor_id

    @property
    def connection_id(self) -> str:
        return self._connection_id

    @property
    def active_sessions(self) -> list[str]:
        return list(self._sessions)

    # --- handlers ---

    def on_session_assign(self, handler: SessionAssignHandler) -> None:
        self._session_assign_handler = handler

    def on_session_stop(self, handler: SessionStopHandler) -> None:
        self._session_stop_handler = handler

    def on_drain(self, handler: DrainHandler) -> None:
        self._drain_handler = handler

    async def session_context(self, process_id: str) -> SessionContext:
        """The context of the agent session ``process_id`` runs for
        (process.start metadata agent_session_id): its environment and the
        metadata its creator set. Read it in a process.start handler to
        resolve a metadata reference or check an owner before spawning, and
        refuse with ``RPCError.with_reason(...)``. Needs a machine-scoped
        executor (``MachineTokenRequiredError``); cached per session, failures
        are not. Raises ``SessionContextNotFoundError`` for a process not on
        this machine."""
        return await self._session_contexts.by_process(process_id)

    async def session_context_by_id(self, session_id: str) -> SessionContext:
        """The context of an agent session bound to this machine (for an agent
        capability without a process); cached like ``session_context``."""
        return await self._session_contexts.by_id(session_id)

    def on_request(self, method: Request[P, R], handler: Callable[[P], Awaitable[R]]) -> None:
        """Serve a harness-to-executor request (process.*, terminal.*,
        skill.install, workspace.backup) with the contract's params and result
        types: ``client.on_request(methods.PROCESS_LIST, handler)``. Raise
        RPCError to answer with a JSON-RPC error."""
        self._request_handlers[method.method] = handler

    def serve_agent(self, handler: AgentHandler, capability: AgentCapability | None = None) -> None:
        """Advertise the agent capability and route the agent methods to
        handler. Call it before start()."""
        from .agent import _AgentRuntime

        self._agent = _AgentRuntime(self, handler, dict(capability or {}, enabled=True))  # type: ignore[arg-type]

    # --- RPC ---

    async def call(self, method: Call[P, R], params: P) -> R:
        """Send an executor-to-harness request typed by the contract:
        ``await client.call(methods.SESSION_EVENT, params)``."""
        result: R = await self._call(method.method, params)
        return result

    async def notify(self, method: Notification[P], params: P) -> None:
        """Send an executor-to-harness notification typed by the contract."""
        await self._notify(method.method, params)

    async def _call(self, method: str, params: Any) -> Any:
        peer = self._peer
        if peer is None:
            raise NotConnectedError()
        return await peer.call(method, params, self._request_timeout)

    async def _notify(self, method: str, params: Any) -> None:
        peer = self._peer
        if peer is None:
            raise NotConnectedError()
        await peer.notify(method, params)

    def _registered_capabilities(self) -> dict[str, Any]:
        """The configured capabilities with shell and files enabled while a
        handler of the group is registered and the option does not turn the
        group off (``enabled: False`` wins, as MCP's RegisteredTool.enabled),
        so nothing unimplemented is advertised. computer_use stays off: its
        tools have no slot yet."""
        capabilities: dict[str, Any] = dict(self._capabilities)
        tools = self.tools
        shell = dict(capabilities.get("shell") or {})
        shell["enabled"] = bool(shell.get("enabled", True)) and tools.registered("bash")
        files = dict(capabilities.get("files") or {})
        files["enabled"] = bool(files.get("enabled", True)) and tools.registered("read", "write", "edit", "glob", "grep", "list_files")
        capabilities["shell"], capabilities["files"], capabilities["computer_use"] = shell, files, False
        if self._agent is not None:
            capabilities["agent"] = self._agent.capability
        return capabilities

    def _tools_changed(self) -> None:
        """A handler registered on a registered connection is announced with
        executor.capabilities.update (as an MCP server sends
        tools/list_changed for a tool registered after connecting)."""
        if self._peer is None or self._state not in (ConnectionState.Ready, ConnectionState.Draining):
            return
        params = {"capabilities": self._registered_capabilities()}
        self._spawn(_best_effort(self._notify(METHOD_EXECUTOR_CAPABILITIES_UPDATE, params)))

    async def report_tool_result(self, params: ToolResultParams) -> Ack:
        """Send tool.result once per tool_call_id, retrying retryable failures
        with exponential backoff. A duplicate for an accepted tool_call_id
        returns accepted without sending another frame."""
        call_id = params["tool_call_id"]
        if call_id in self._completed_tool_results:
            return {"accepted": True}
        _remember(self._completed_tool_results, call_id)
        last_error: BaseException | None = None
        for attempt in range(1, self._tool_result_retry_attempts + 1):
            try:
                return await self._call(METHOD_TOOL_RESULT, params)
            except (RPCError, NotConnectedError, TimeoutError) as error:
                last_error = error
                if attempt == self._tool_result_retry_attempts or not _retryable_tool_result_error(error):
                    break
                self._log("warn", f"tool.result for {call_id} failed on attempt {attempt}, retrying", error)
                if self._stopped:
                    last_error = ClientStoppedError()
                    break
                await asyncio.sleep(self._tool_result_retry_delay(attempt))
        self._completed_tool_results.pop(call_id, None)
        assert last_error is not None
        raise last_error

    async def session_ready(self, params: SessionReadyParams) -> Ack:
        """Report that the session is prepared (after workspace and MCP setup)."""
        return await self._call(METHOD_SESSION_READY, params)

    async def session_ended(self, params: SessionEndedParams) -> Ack:
        """Report that every process and MCP server of the session stopped."""
        result: Ack = await self._call(METHOD_SESSION_ENDED, params)
        self._sessions.discard(params["session_id"])
        return result

    async def open_mcp_channel(self, session_id: str, server_key: str) -> MCPChannel:
        """Open the MCP channel for one session.assign mcp_servers entry with a
        fresh executor token; frames are raw MCP JSON-RPC."""
        if not session_id.strip() or not server_key.strip():
            raise ValueError("bma_executor_sdk: session_id and server_key are required")
        if self._stopped:
            raise ClientStoppedError()
        token = await self._fetch_token()
        scopes = token.get("channel_scope") or []
        if scopes and ChannelScope.Mcp.value not in scopes:
            raise ExecutorTokenScopeError("executor token does not allow the mcp channel")
        url = channel_url(self._ws_url or token["ws_url"], channel="mcp", session_id=session_id, server_key=server_key)
        conn = await self._dialer(url, token["token"], [PROTOCOL_V11])
        return MCPChannel(conn)

    # --- internals ---

    def _spawn(self, coro: Awaitable[Any]) -> asyncio.Task[Any]:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return task

    def _log(self, level: str, message: str, error: BaseException | None = None) -> None:
        if self._on_log is not None:
            self._on_log(LogEvent(level, message, error))

    async def _fetch_token(self) -> ExecutorTokenResponse:
        if self._token_source is not None:
            token = await self._token_source()
            if not token.get("ws_url"):
                raise ValueError("bma_executor_sdk: token source returned no ws_url")
            return token
        client = self._http_client or httpx.AsyncClient()
        try:
            response = await client.post(
                # A machine daemon token (no environment id) is exchanged by
                # bma-api, an environment key by managed-agents.
                f"{self._base_url}{'/v1' if self._environment_id else '/api/v1'}/executors/token",
                json={"environment_id": self._environment_id, "executor": self._executor},
                headers={"Authorization": f"Bearer {self._environment_key}"},
                timeout=self._request_timeout,
            )
        finally:
            if self._http_client is None:
                await client.aclose()
        if response.status_code != 200:
            message = f"token request failed: {response.status_code}: {response.text[:4096].strip()}"
            if response.status_code in (401, 403):
                raise CredentialRejectedError(message, response.status_code)
            raise ExecutorTokenError(message)
        token = response.json()
        if not token.get("token") or not token.get("ws_url"):
            raise ExecutorTokenError("token response is missing token or ws_url")
        return token

    async def _connect(self, token: ExecutorTokenResponse) -> None:
        self._state = ConnectionState.Connecting
        conn = await self._dialer(self._ws_url or token["ws_url"], token["token"], [PROTOCOL_V11])
        if self._stopped:
            await conn.close(CLOSE_NORMAL, "stopped")
            raise ClientStoppedError()
        peer = Peer(conn, id_prefix="req", on_request=self._dispatch_request, on_notification=self._dispatch_notification, on_log=self._log)
        self._peer = peer
        self._spawn(self._read(peer))
        capabilities = self._registered_capabilities()
        params: dict[str, Any] = {
            "protocol": PROTOCOL_V11,
            "environment_id": self._environment_id,
            "executor": self._executor,
            "capabilities": capabilities,
        }
        resume = self._resume_state()
        if resume is not None:
            params["resume"] = resume
        try:
            result: ExecutorRegisterResult = await peer.call(METHOD_EXECUTOR_REGISTER, params, self._request_timeout)
        except BaseException:
            if self._peer is peer:
                self._peer = None
            await conn.close(1011, "registration failed")
            raise
        interval_ms = result.get("heartbeat_interval_ms") or 0
        self._heartbeat_interval = interval_ms / 1000 if interval_ms > 0 else self._default_heartbeat
        self._register_result = result
        self._connection_id = result["connection_id"]
        self._executor_id = result["executor_id"]
        self._state = ConnectionState.Ready
        self._reconnect_attempts = 0
        self._spawn(self._mark_stable_later(peer, self._heartbeat_interval))
        registered, self._registered = self._registered, asyncio.Event()
        registered.set()
        self._log("info", f"executor registered as {result['executor_id']}")
        if token.get("expires_in"):
            self._spawn(self._refresh_token_loop(peer, float(token["expires_in"])))

    async def _refresh_token_loop(self, peer: Peer, lifetime: float) -> None:
        """Keep peer open across token lifetimes: at 70% of the token's
        lifetime fetch a new token and send executor.token.refresh. A harness
        without the method (-32601) closes with 4001 at expiry and the client
        reconnects as before."""
        loop = asyncio.get_running_loop()
        expires_at = loop.time() + lifetime
        wait = lifetime * _TOKEN_REFRESH_AT
        while True:
            await asyncio.sleep(wait)
            if self._stopped or self._peer is not peer:
                return
            try:
                fresh = await self._fetch_token()
                await self._call(METHOD_EXECUTOR_TOKEN_REFRESH, {"token": fresh["token"]})
            except RPCError as error:
                if error.code in (-32601, -32012):
                    self._log("info", "harness does not refresh executor tokens; reconnecting at expiry")
                    return
                failure: BaseException = error
            except asyncio.CancelledError:
                raise
            except Exception as error:
                failure = error
            else:
                lifetime = float(fresh.get("expires_in") or 0)
                if lifetime <= 0:
                    return
                expires_at = loop.time() + lifetime
                wait = lifetime * _TOKEN_REFRESH_AT
                continue
            self._log("warn", "executor token refresh failed", failure)
            remaining = expires_at - loop.time()
            if remaining < 2:
                return
            wait = remaining / 2

    def _resume_state(self) -> dict[str, Any] | None:
        if not self._executor_id and not self._connection_id and not self._sessions:
            return None
        resume: dict[str, Any] = {"active_sessions": sorted(self._sessions)}
        if self._executor_id:
            resume["executor_id"] = self._executor_id
        if self._connection_id:
            resume["last_connection_id"] = self._connection_id
        return resume

    async def _mark_stable_later(self, peer: Peer, delay: float) -> None:
        """peer stayed registered for one heartbeat interval: the platform did not refuse its runtime."""
        await asyncio.sleep(delay)
        if self._stopped or self._peer is not peer:
            return
        self._stable_peer = peer
        self._refused_streak = 0
        stable, self._stable = self._stable, asyncio.Event()
        stable.set()

    def _count_refused_registration(self, cause: BaseException) -> bool:
        """Record one refused registration; end the client at the limit."""
        self._refused_streak += 1
        if self._refused_streak < _MAX_REFUSED_REGISTRATIONS:
            return False
        error = RegistrationRefusedError(
            f"executor registration refused {self._refused_streak} times in a row "
            "(a machine-scoped executor must declare the process or agent capability)"
        )
        error.__cause__ = cause
        self._log("error", "executor registration refused; not reconnecting", cause)
        self._end(error)
        return True

    async def _read(self, peer: Peer) -> None:
        closed = await peer.run()
        await self._disconnected(peer, closed)

    async def _disconnected(self, peer: Peer, closed: ChannelClosed) -> None:
        if self._peer is not peer:
            return
        was_stable = self._stable_peer is peer
        self._peer = None
        if self._stopped:
            return
        if closed.code in TERMINAL_CLOSE_CODES:
            self._log("error", f"executor channel closed with terminal code {closed.code}: {closed.reason}")
            self._end(closed)
            return
        if not was_stable and _is_registration_refusal(closed):
            if self._count_refused_registration(closed):
                return
        else:
            self._refused_streak = 0
        self._state = ConnectionState.Disconnected
        self._spawn(self._reconnect_loop())

    async def _reconnect_loop(self) -> None:
        cause: BaseException | None = None
        while not self._stopped:
            if 0 < self._max_reconnect_attempts <= self._reconnect_attempts:
                error = ReconnectExhaustedError("executor reconnect attempts exhausted")
                error.__cause__ = cause
                self._log("error", "executor reconnect attempts exhausted", cause)
                self._end(error)
                return
            attempt = self._reconnect_attempts
            self._reconnect_attempts += 1
            # The exponent is capped: unlimited attempts must not overflow the float.
            delay = min(self._reconnect_base_delay * (1 << min(attempt, 32)), self._reconnect_max_delay)
            delay *= 0.5 + self._random() * 0.5
            self._log("info", f"reconnecting executor channel in {delay:.3f}s")
            await asyncio.sleep(delay)
            if self._stopped:
                return
            try:
                await self._connect(await self._fetch_token())
                return
            except asyncio.CancelledError:
                raise
            except CredentialRejectedError as error:
                # Retrying cannot help a revoked or invalid credential (GitHub
                # Actions runner and Buildkite agent stop on 401/403 too).
                self._log("error", "executor credential rejected; not reconnecting", error)
                self._end(error)
                return
            except Exception as error:
                if _is_registration_refusal(error) and self._count_refused_registration(error):
                    return
                cause = error
                self._state = ConnectionState.Disconnected
                self._log("warn", "executor reconnect failed", error)

    async def _heartbeat_loop(self) -> None:
        while True:
            await asyncio.sleep(self._heartbeat_interval)
            if self._state not in (ConnectionState.Ready, ConnectionState.Draining):
                continue
            params: dict[str, Any] = {"connection_id": self._connection_id, "active_sessions": sorted(self._sessions)}
            if self._heartbeat_load is not None:
                load = self._heartbeat_load()
                if load is not None:
                    params["load"] = load
            peer = self._peer
            try:
                result = await self._call(METHOD_EXECUTOR_HEARTBEAT, params)
            except (RPCError, NotConnectedError, TimeoutError) as error:
                self._log("warn", "executor heartbeat failed", error)
                if isinstance(error, TimeoutError) and peer is not None and peer is self._peer:
                    # No answer within request_timeout: the connection is dead
                    # (a half-open TCP connection after a network cut). Close it
                    # so the client reconnects, as gRPC keepalive closes a
                    # transport whose ping times out.
                    self._log("warn", "executor heartbeat timed out; closing the connection to reconnect")
                    await peer.conn.close(1001, "heartbeat timeout")
                continue
            if result.get("drain") and self._state == ConnectionState.Ready:
                self._state = ConnectionState.Draining

    async def _dispatch_request(self, request_id: str, method: str, params: Any) -> Any:
        if not isinstance(params, dict):
            raise RPCError(JsonRpcErrorCode.InvalidParams, f"invalid {method} params")
        if method == METHOD_TOOL_CALL:
            if not params.get("tool_call_id") or not params.get("session_id"):
                raise RPCError(JsonRpcErrorCode.InvalidParams, "invalid tool.call params")
            if self.tools.get(params.get("tool", "")) is None:
                # As Go Core: a tool without a handler is refused before the ack.
                raise RPCError(JsonRpcErrorCode.CapabilityUnavailable, "capability_unavailable", {"capability": f"tool:{params.get('tool')}"})
            if self._begin_tool_call(params):  # type: ignore[arg-type]
                return {"accepted": True}
            self._log("debug", f"duplicate tool.call for {params['tool_call_id']} ignored")
            return {"accepted": True}
        if method == METHOD_TOOL_CANCEL:
            return self._cancel_tool_call(params.get("tool_call_id", ""), params.get("reason", ""))
        if method == METHOD_SESSION_ASSIGN:
            return await self._assign_session(params)  # type: ignore[arg-type]
        if method == METHOD_SESSION_STOP:
            result: SessionStopResult = {"stopped": True}
            if self._session_stop_handler is not None:
                result = await self._session_stop_handler(params)  # type: ignore[arg-type]
            self._sessions.discard(params.get("session_id", ""))
            return result
        if method == METHOD_EXECUTOR_DRAIN:
            self._state = ConnectionState.Draining
            if self._drain_handler is not None:
                return await self._drain_handler(params)  # type: ignore[arg-type]
            return {"accepted": True, "active_sessions": sorted(self._sessions)}
        if self._agent is not None and method in self._agent.methods:
            return await self._agent.methods[method](params)
        handler = self._request_handlers.get(method)
        if handler is None:
            raise _unregistered_method_error(method)
        return await handler(params)

    def _dispatch_notification(self, method: str, params: Any) -> None:
        if method == METHOD_EXECUTOR_CLOSE:
            # The harness closed the executor deliberately: no reconnect.
            self._end(None)
            peer, self._peer = self._peer, None
            if peer is not None:
                peer.fail_pending(ClientStoppedError())
                self._spawn(peer.conn.close(CLOSE_NORMAL, "executor.close"))
            return
        # executor.close is the only harness-to-executor notification.

    async def _assign_session(self, params: SessionAssignParams) -> SessionAssignResult:
        if self._state == ConnectionState.Draining:
            return {"accepted": False, "session_id": params["session_id"]}
        result: SessionAssignResult = {"accepted": True, "session_id": params["session_id"]}
        if self._session_assign_handler is not None:
            result = await self._session_assign_handler(params)
        if result.get("accepted"):
            self._sessions.add(params["session_id"])
        return result

    def _begin_tool_call(self, params: ToolCallParams) -> bool:
        call_id = params["tool_call_id"]
        if call_id in self._seen_tool_calls or call_id in self._running_tool_calls:
            return False
        _remember(self._seen_tool_calls, call_id)
        running = _RunningToolCall()
        self._running_tool_calls[call_id] = running
        running.task = self._spawn(self._run_tool_call(params, running))
        return True

    def _cancel_tool_call(self, call_id: str, reason: str) -> dict[str, bool]:
        running = self._running_tool_calls.get(call_id)
        if running is None or running.task is None:
            return {"canceled": False}
        running.canceled = True
        running.reason = reason
        if running.handler is not None:  # otherwise the call sees canceled before it starts
            running.handler.cancel()
        self._log("info", f"canceled tool.call {call_id}")
        return {"canceled": True}

    async def _run_tool_call(self, params: ToolCallParams, running: _RunningToolCall) -> None:
        call_id = params["tool_call_id"]
        handler = self.tools.get(params["tool"])
        result: ToolResultParams
        loop = asyncio.get_running_loop()
        started = loop.time()
        if running.canceled:
            result = _canceled_tool_result(params, running.reason)
        elif handler is None:
            result = _failed_tool_result(params, f"no handler registered for tool {params['tool']}")
        elif (missing := missing_required(params["tool"], params.get("input"))) is not None:
            result = _failed_tool_result(params, f"{params['tool']} requires {missing}")
        else:
            context = ToolContext(
                session_id=params["session_id"],
                tool_call_id=call_id,
                progress=lambda message: self._progress(params, message),
                permission=params.get("permission"),
            )
            inner = running.handler = asyncio.ensure_future(handler(params.get("input") or {}, context))
            deadline_ms = params.get("deadline_ms") or 0
            done, _ = await asyncio.wait({inner}, timeout=deadline_ms / 1000 if deadline_ms > 0 else None)
            if not done:
                inner.cancel()
                await asyncio.gather(inner, return_exceptions=True)
                result = _failed_tool_result(params, "tool.call deadline_ms exceeded")
            elif inner.cancelled():
                result = _failed_tool_result(params, "tool handler canceled")
            elif isinstance(inner.exception(), ToolError):
                error = inner.exception()
                result = {"outcome": "succeeded", "is_error": True, "content": error.content}  # type: ignore[union-attr,typeddict-item]
            elif inner.exception() is not None:
                result = _failed_tool_result(params, str(inner.exception()))
            else:
                result = render(inner.result())  # type: ignore[assignment]
            result["duration_ms"] = int((loop.time() - started) * 1000)
        self._running_tool_calls.pop(call_id, None)
        if running.canceled:
            result = _canceled_tool_result(params, running.reason)
        # The result identifies the call the harness dispatched, whatever the
        # handler put in it.
        result["tool_call_id"] = call_id
        result["session_id"] = params["session_id"]
        try:
            await self.report_tool_result(result)
        except Exception as error:
            self._log("error", f"failed to report tool result for {call_id}", error)
            if self._on_tool_result_lost is not None:
                self._on_tool_result_lost(result, error)

    def _progress(self, params: ToolCallParams, message: str) -> None:
        peer = self._peer
        if peer is None:
            return
        progress = {"session_id": params["session_id"], "tool_call_id": params["tool_call_id"], "message": message}
        self._spawn(_best_effort(peer.notify(METHOD_TOOL_PROGRESS, progress)))

    def _tool_result_retry_delay(self, attempt: int) -> float:
        delay = min(self._tool_result_retry_base_delay * (2 ** (attempt - 1)), _MAX_TOOL_RESULT_RETRY_DELAY)
        return delay * (0.5 + self._random() * 0.5)

    async def _wait_registered(self) -> None:
        await self._registered.wait()


class ExecutorTokenError(Exception):
    """POST /v1/executors/token failed."""


class CredentialRejectedError(ExecutorTokenError):
    """The token endpoint answered 401 or 403: the environment key or machine
    token is invalid or revoked. A reconnect does not retry it."""

    def __init__(self, message: str, status_code: int) -> None:
        super().__init__(message)
        self.status_code = status_code


class RegistrationRefusedError(ExecutorError):
    """The platform refused the executor's registration 3 times in a row: the
    close 4002 "runtime registration refused" right after executor.register,
    or an executor.register error for the executor's capabilities. A
    configuration error (a machine-scoped executor must declare the process
    or agent capability) that reconnecting cannot fix; ``__cause__`` is the
    last refusal."""


def _is_registration_refusal(error: BaseException) -> bool:
    if isinstance(error, RPCError):
        return error.code in (JsonRpcErrorCode.CapabilityUnavailable, JsonRpcErrorCode.InvalidParams)
    return isinstance(error, ChannelClosed) and error.code == 4002 and "registration refused" in error.reason.lower()


class ReconnectExhaustedError(ExecutorError):
    """A positive max_reconnect_attempts failed in a row; ``__cause__`` is the
    last failure."""


class ExecutorTokenScopeError(Exception):
    """The executor token does not allow the requested channel."""


class MCPChannel:
    """Executor end of one MCP channel: raw MCP JSON-RPC frames. MCP process
    supervision and reconnection stay with the adapter."""

    def __init__(self, conn: Connection) -> None:
        self._conn = conn

    async def read(self) -> str:
        return await self._conn.recv()

    async def write(self, frame: str) -> None:
        await self._conn.send(frame)

    async def close(self) -> None:
        await self._conn.close(CLOSE_NORMAL, "mcp channel closed")


async def _best_effort(coro: Awaitable[Any]) -> None:
    try:
        await coro
    except Exception:  # tool.progress is best-effort by contract
        pass


def _remember(tracked: dict[str, None], key: str) -> None:
    if len(tracked) >= _MAX_TRACKED_TOOL_RESULTS:
        tracked.pop(next(iter(tracked)))
    tracked[key] = None


def _retryable_tool_result_error(error: BaseException) -> bool:
    if isinstance(error, RPCError):
        if error.retryable is not None:
            return bool(error.retryable)
        return error.code in (JsonRpcErrorCode.ExecutorBusy, JsonRpcErrorCode.Timeout)
    return isinstance(error, (NotConnectedError, TimeoutError))


def _failed_tool_result(params: ToolCallParams, message: str) -> ToolResultParams:
    return {
        "session_id": params["session_id"],
        "tool_call_id": params["tool_call_id"],
        "outcome": ToolOutcome.Failed.value,  # type: ignore[typeddict-item]
        "is_error": True,
        "content": [{"type": "text", "text": message}],  # type: ignore[list-item]
    }


def _canceled_tool_result(params: ToolCallParams, reason: str) -> ToolResultParams:
    text = f"canceled: {reason}" if reason else "canceled"
    return {
        "session_id": params["session_id"],
        "tool_call_id": params["tool_call_id"],
        "outcome": ToolOutcome.Canceled.value,  # type: ignore[typeddict-item]
        "is_error": False,
        "content": [{"type": "text", "text": text}],  # type: ignore[list-item]
    }


__all__ = [
    "DEFAULT_BASE_URL",
    "PROTOCOL_V1",
    "PROTOCOL_V11",
    "ExecutorClient",
    "ExecutorTokenError",
    "ExecutorTokenScopeError",
    "LogEvent",
    "MCPChannel",
    "MAX_FRAME_BYTES",
    "static_token",
]


def _unregistered_method_error(method: str) -> RPCError:
    """A contract method behind a capability this executor did not register is
    capability_unavailable naming that capability; anything else is
    method_not_found."""
    spec = METHODS.get(method)
    if spec is None or not spec.capability or spec.direction == "executor-to-harness":
        return RPCError(JsonRpcErrorCode.MethodNotFound, f"unknown method {method}")
    return RPCError(JsonRpcErrorCode.CapabilityUnavailable, "capability_unavailable", {"capability": spec.capability})
