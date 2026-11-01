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
from typing import TYPE_CHECKING, Any

import httpx

from ._rpc import ClientStoppedError, NotConnectedError, Peer, RPCError
from .generated import (
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
from .transport import CLOSE_NORMAL, MAX_FRAME_BYTES, TERMINAL_CLOSE_CODES, ChannelClosed, Connection, Dialer, channel_url, dial

if TYPE_CHECKING:
    from .agent import AgentHandler, _AgentRuntime
    from .generated import AgentCapability

DEFAULT_BASE_URL = "https://api.bma.beeos.ai"
DEFAULT_REQUEST_TIMEOUT = 30.0
DEFAULT_MAX_RECONNECT_ATTEMPTS = 10
DEFAULT_RECONNECT_BASE_DELAY = 0.5
DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS = 5
DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY = 0.25
DEFAULT_HEARTBEAT_INTERVAL = 15.0
PROTOCOL_V1 = ProtocolVersion.BmaExecutorV1.value
PROTOCOL_V11 = ProtocolVersion.BmaExecutorV11.value
_MAX_TRACKED_TOOL_RESULTS = 8192
_MAX_TOOL_RESULT_RETRY_DELAY = 10.0
_MAX_RECONNECT_DELAY = 30.0
# Fraction of a token's lifetime after which executor.token.refresh is sent.
_TOKEN_REFRESH_AT = 0.7

TokenSource = Callable[[], Awaitable[ExecutorTokenResponse]]
ToolCallHandler = Callable[[ToolCallParams], Awaitable[ToolResultParams]]
SessionAssignHandler = Callable[[SessionAssignParams], Awaitable[SessionAssignResult]]
SessionStopHandler = Callable[[SessionStopParams], Awaitable[SessionStopResult]]
DrainHandler = Callable[[ExecutorDrainParams], Awaitable[ExecutorDrainResult]]
RawRequestHandler = Callable[[Any], Awaitable[Any]]
RawNotificationHandler = Callable[[Any], None]


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
        self._base_url = base_url.rstrip("/")
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
        self._register_result: ExecutorRegisterResult | None = None
        self._connection_id = ""
        self._executor_id = ""
        self._heartbeat_interval = heartbeat_interval
        self._sessions: set[str] = set()
        self._reconnect_attempts = 0
        self._completed_tool_results: dict[str, None] = {}
        self._seen_tool_calls: dict[str, None] = {}
        self._running_tool_calls: dict[str, _RunningToolCall] = {}
        self._tool_call_handler: ToolCallHandler | None = None
        self._session_assign_handler: SessionAssignHandler | None = None
        self._session_stop_handler: SessionStopHandler | None = None
        self._drain_handler: DrainHandler | None = None
        self._request_handlers: dict[str, RawRequestHandler] = {}
        self._notification_handlers: dict[str, RawNotificationHandler] = {}
        self._tasks: set[asyncio.Task[Any]] = set()
        # Set after every successful executor.register, then replaced, so agent
        # outboxes resend after a reconnect.
        self._registered = asyncio.Event()
        self._agent: _AgentRuntime | None = None

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
        if self._stopped:
            return
        self._stopped = True
        peer, self._peer = self._peer, None
        self._state = ConnectionState.Closed
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

    def on_tool_call(self, handler: ToolCallHandler) -> None:
        """Register the tool.call handler. tool.cancel cancels the handler's
        task; the client then reports outcome=canceled."""
        self._tool_call_handler = handler

    def on_session_assign(self, handler: SessionAssignHandler) -> None:
        self._session_assign_handler = handler

    def on_session_stop(self, handler: SessionStopHandler) -> None:
        self._session_stop_handler = handler

    def on_drain(self, handler: DrainHandler) -> None:
        self._drain_handler = handler

    def on_request(self, method: str, handler: RawRequestHandler) -> None:
        """Register a handler for another harness-to-executor request method."""
        self._request_handlers[method] = handler

    def on_notification(self, method: str, handler: RawNotificationHandler) -> None:
        self._notification_handlers[method] = handler

    def serve_agent(self, handler: AgentHandler, capability: AgentCapability | None = None) -> None:
        """Advertise the agent capability and route the agent methods to
        handler. Call it before start()."""
        from .agent import _AgentRuntime

        self._agent = _AgentRuntime(self, handler, dict(capability or {}, enabled=True))  # type: ignore[arg-type]

    # --- RPC ---

    async def call(self, method: str, params: Any) -> Any:
        """Send a JSON-RPC request and return its result."""
        peer = self._peer
        if peer is None:
            raise NotConnectedError()
        return await peer.call(method, params, self._request_timeout)

    async def notify(self, method: str, params: Any) -> None:
        peer = self._peer
        if peer is None:
            raise NotConnectedError()
        await peer.notify(method, params)

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
                return await self.call(METHOD_TOOL_RESULT, params)
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
        return await self.call(METHOD_SESSION_READY, params)

    async def session_ended(self, params: SessionEndedParams) -> Ack:
        """Report that every process and MCP server of the session stopped."""
        result: Ack = await self.call(METHOD_SESSION_ENDED, params)
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
                f"{self._base_url}/v1/executors/token",
                json={"environment_id": self._environment_id, "executor": self._executor},
                headers={"Authorization": f"Bearer {self._environment_key}"},
                timeout=self._request_timeout,
            )
        finally:
            if self._http_client is None:
                await client.aclose()
        if response.status_code != 200:
            raise ExecutorTokenError(f"token request failed: {response.status_code}: {response.text[:4096].strip()}")
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
        capabilities: dict[str, Any] = dict(self._capabilities)
        if self._agent is not None:
            capabilities["agent"] = self._agent.capability
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
                await self.call(METHOD_EXECUTOR_TOKEN_REFRESH, {"token": fresh["token"]})
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

    async def _read(self, peer: Peer) -> None:
        closed = await peer.run()
        await self._disconnected(peer, closed)

    async def _disconnected(self, peer: Peer, closed: ChannelClosed) -> None:
        if self._peer is not peer:
            return
        self._peer = None
        if self._stopped:
            self._state = ConnectionState.Closed
            return
        if closed.code in TERMINAL_CLOSE_CODES:
            self._state = ConnectionState.Closed
            self._log("error", f"executor channel closed with terminal code {closed.code}: {closed.reason}")
            return
        self._state = ConnectionState.Disconnected
        self._spawn(self._reconnect_loop())

    async def _reconnect_loop(self) -> None:
        while not self._stopped:
            if self._reconnect_attempts >= self._max_reconnect_attempts:
                self._state = ConnectionState.Closed
                self._log("error", "executor reconnect attempts exhausted")
                return
            attempt = self._reconnect_attempts
            self._reconnect_attempts += 1
            delay = min(self._reconnect_base_delay * (1 << attempt), self._reconnect_max_delay)
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
            except Exception as error:
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
            try:
                result = await self.call(METHOD_EXECUTOR_HEARTBEAT, params)
            except (RPCError, NotConnectedError, TimeoutError) as error:
                self._log("warn", "executor heartbeat failed", error)
                continue
            if result.get("drain") and self._state == ConnectionState.Ready:
                self._state = ConnectionState.Draining

    async def _dispatch_request(self, request_id: str, method: str, params: Any) -> Any:
        if not isinstance(params, dict):
            raise RPCError(JsonRpcErrorCode.InvalidParams, f"invalid {method} params")
        if method == METHOD_TOOL_CALL:
            if not params.get("tool_call_id") or not params.get("session_id"):
                raise RPCError(JsonRpcErrorCode.InvalidParams, "invalid tool.call params")
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
            self._stopped = True
            self._state = ConnectionState.Closed
            peer, self._peer = self._peer, None
            if peer is not None:
                peer.fail_pending(ClientStoppedError())
                self._spawn(peer.conn.close(CLOSE_NORMAL, "executor.close"))
            return
        handler = self._notification_handlers.get(method)
        if handler is not None:
            handler(params)

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
        handler = self._tool_call_handler
        result: ToolResultParams
        if running.canceled:
            result = _canceled_tool_result(params, running.reason)
        elif handler is None:
            result = _failed_tool_result(params, "no tool handler registered")
        else:
            inner = running.handler = asyncio.ensure_future(handler(params))
            deadline_ms = params.get("deadline_ms") or 0
            done, _ = await asyncio.wait({inner}, timeout=deadline_ms / 1000 if deadline_ms > 0 else None)
            if not done:
                inner.cancel()
                await asyncio.gather(inner, return_exceptions=True)
                result = _failed_tool_result(params, "tool.call deadline_ms exceeded")
            elif inner.cancelled():
                result = _failed_tool_result(params, "tool handler canceled")
            elif inner.exception() is not None:
                result = _failed_tool_result(params, str(inner.exception()))
            else:
                result = dict(inner.result())  # type: ignore[assignment]
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

    def _tool_result_retry_delay(self, attempt: int) -> float:
        delay = min(self._tool_result_retry_base_delay * (2 ** (attempt - 1)), _MAX_TOOL_RESULT_RETRY_DELAY)
        return delay * (0.5 + self._random() * 0.5)

    async def _wait_registered(self) -> None:
        await self._registered.wait()


class ExecutorTokenError(Exception):
    """POST /v1/executors/token failed."""


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
