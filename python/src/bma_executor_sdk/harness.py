"""The harness ("brain") end of the executor protocol.

Mirrors go/executor/harness.go and harness_mcp.go: it accepts executor control
and MCP channels (WebSocket on TCP, WebSocket on a Unix domain socket, or
in-process), and drives them with session.assign / tool.call / tool.cancel /
session.stop / executor.drain. It speaks the same protocol as the BMA service,
so an executor cannot tell a local harness from the cloud.
"""

from __future__ import annotations

import asyncio
import inspect
import secrets
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from datetime import datetime, timezone
from http import HTTPStatus
from typing import TYPE_CHECKING, Any
from urllib.parse import parse_qsl, urlsplit

from ._rpc import ExecutorError, NotConnectedError, Peer, RPCError, rpc_error
from .client import DEFAULT_REQUEST_TIMEOUT, PROTOCOL_V1, PROTOCOL_V11, LogEvent
from .generated import (
    METHOD_EXECUTOR_CAPABILITIES_UPDATE,
    METHOD_EXECUTOR_CLOSE,
    METHOD_EXECUTOR_DRAIN,
    METHOD_EXECUTOR_HEARTBEAT,
    METHOD_EXECUTOR_REGISTER,
    METHOD_SESSION_ASSIGN,
    METHOD_SESSION_ENDED,
    METHOD_SESSION_EVENT,
    METHOD_SESSION_READY,
    METHOD_SESSION_STATUS,
    METHOD_SESSION_STOP,
    METHOD_TOOL_CALL,
    METHOD_TOOL_CANCEL,
    METHOD_TOOL_PROGRESS,
    METHOD_TOOL_RESULT,
    METHOD_INTERACTION_RESPONSE,
    METHOD_SESSION_CLOSE,
    METHOD_TURN_CANCEL,
    METHOD_TURN_INPUT,
    METHOD_TURN_START,
    Ack,
    AgentCapability,
    ExecutorDrainResult,
    InteractionResponseParams,
    InteractionResponseResult,
    SessionCloseParams,
    SessionCloseResult,
    TurnCancelParams,
    TurnCancelResult,
    TurnInputParams,
    TurnStartParams,
    TurnStartResult,
    ExecutorInfo,
    JsonRpcErrorCode,
    SessionAssignParams,
    SessionEventParams,
    SessionEventResult,
    SessionStatusParams,
    ToolCallParams,
    ToolProgressParams,
    ToolResultParams,
)
from .transport import (
    CLOSE_NORMAL,
    CLOSE_POLICY_VIOLATION,
    CLOSE_REPLACED,
    MAX_FRAME_BYTES,
    ChannelClosed,
    ConnectRequest,
    Connection,
    Dialer,
    WebSocketConnection,
    pipe,
)

if TYPE_CHECKING:
    from .agent import AgentPlatform

DEFAULT_HARNESS_HEARTBEAT_INTERVAL = 15.0
DEFAULT_HARNESS_LEASE_TTL = 60.0
_MAX_REMEMBERED_EVENT_IDS = 1024


class SessionEndedError(ExecutorError):
    """A request on a session that has ended."""

    def __init__(self) -> None:
        super().__init__("session ended")


class ToolOutcomeUnknownError(ExecutorError):
    """The executor was lost (lease expiry, executor.close, session ended, or a
    restarted executor that no longer reports the session) before tool.result
    arrived. The tool may or may not have run."""

    def __init__(self) -> None:
        super().__init__("tool outcome unknown: executor lost before tool.result")


class AssignRejectedError(ExecutorError):
    """session.assign answered accepted=false."""

    def __init__(self) -> None:
        super().__init__("executor rejected session.assign")


class Unauthorized(Exception):
    """Raised by an authenticate hook to refuse a channel."""


@dataclass(frozen=True)
class Principal:
    """What authenticate learned. A non-empty executor_id pins the executor
    identity: register and MCP channels must belong to it."""

    executor_id: str = ""


Authenticate = Callable[[ConnectRequest], "Principal | Awaitable[Principal]"]


def _now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _random_id(n: int) -> str:
    return secrets.token_hex(n)


class Harness:
    """Harness end of the executor protocol (Go: ``Harness``)."""

    def __init__(
        self,
        *,
        authenticate: Authenticate | None = None,
        heartbeat_interval: float = DEFAULT_HARNESS_HEARTBEAT_INTERVAL,
        lease_ttl: float = DEFAULT_HARNESS_LEASE_TTL,
        request_timeout: float = DEFAULT_REQUEST_TIMEOUT,
        protocols: Sequence[str] = (PROTOCOL_V11, PROTOCOL_V1),
        on_executor: Callable[[Executor], Any] | None = None,
        on_session_event: Callable[[Executor, SessionEventParams], Awaitable[SessionEventResult]] | None = None,
        on_session_status: Callable[[Executor, SessionStatusParams], None] | None = None,
        on_tool_progress: Callable[[Executor, ToolProgressParams], None] | None = None,
        on_log: Callable[[LogEvent], None] | None = None,
        agent: AgentPlatform | None = None,
    ) -> None:
        """authenticate verifies every channel (control and MCP); None accepts
        every connection: use that only on a loopback listener or in-process."""
        self._authenticate = authenticate
        self.heartbeat_interval = heartbeat_interval
        self.lease_ttl = lease_ttl
        self.request_timeout = request_timeout
        self.protocols = tuple(protocols)
        self._on_executor = on_executor
        self._on_session_event = on_session_event
        self._on_session_status = on_session_status
        self._on_tool_progress = on_tool_progress
        self._on_log = on_log
        self.agent = agent
        self._executors: dict[str, Executor] = {}
        self._sessions: dict[str, Session] = {}
        self._changed = asyncio.Event()
        self._next_conn = 0
        self._tasks: set[asyncio.Task[Any]] = set()
        from .agent import _AgentLedger

        self._agent_ledger = _AgentLedger()

    # --- registry ---

    def executors(self) -> list[Executor]:
        """Executors currently registered (connected or within their lease)."""
        return list(self._executors.values())

    async def wait_executor(self) -> Executor:
        """Return a connected executor, waiting for one to register."""
        while True:
            for executor in self._executors.values():
                if executor.connected:
                    return executor
            await self._changed.wait()

    def _signal(self) -> None:
        changed, self._changed = self._changed, asyncio.Event()
        changed.set()

    def _log(self, level: str, message: str, error: BaseException | None = None) -> None:
        if self._on_log is not None:
            self._on_log(LogEvent(level, message, error))

    def _spawn(self, coro: Awaitable[Any]) -> asyncio.Task[Any]:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return task

    # --- listeners ---

    async def serve(self, host: str = "127.0.0.1", port: int = 0) -> Any:
        """Listen for WebSocket channels on TCP; returns the websockets Server
        (``server.sockets[0].getsockname()`` has the bound port)."""
        from websockets.asyncio.server import serve

        return await serve(self._ws_handler, host, port, **self._ws_options())

    async def serve_unix(self, path: str) -> Any:
        """Listen for WebSocket channels on a Unix domain socket; executors use
        ``unix://<path>``."""
        from websockets.asyncio.server import unix_serve

        return await unix_serve(self._ws_handler, path, **self._ws_options())

    def inprocess_dialer(self) -> Dialer:
        """A Dialer that connects an ExecutorClient in the same event loop to
        this harness without a socket (use with static_token("inprocess://"))."""

        async def dialer(url: str, token: str, subprotocols: Sequence[str]) -> Connection:
            parts = urlsplit(url)
            request = ConnectRequest(
                path=parts.path or "/", query=dict(parse_qsl(parts.query)),
                headers={"Authorization": f"Bearer {token}"} if token else {},
            )
            protocol = next((p for p in self.protocols if p in subprotocols), None)
            if protocol is None:
                raise ConnectionRefusedError("unsupported executor protocol")
            principal, refusal = await self._admit(request)
            if refusal is not None:
                raise ConnectionRefusedError(refusal[1])
            ours, theirs = pipe(protocol)
            self._spawn(self._serve_channel(ours, request, principal))
            return theirs

        return dialer

    def _ws_options(self) -> dict[str, Any]:
        return {
            "subprotocols": list(self.protocols),
            "process_request": self._process_request,
            "max_size": MAX_FRAME_BYTES,
        }

    async def _process_request(self, connection: Any, request: Any) -> Any:
        offered = ",".join(request.headers.get_all("Sec-WebSocket-Protocol"))
        if not any(value.strip() in self.protocols for value in offered.split(",")):
            return connection.respond(HTTPStatus.BAD_REQUEST, "unsupported executor protocol\n")
        parts = urlsplit(request.path)
        connect = ConnectRequest(path=parts.path, query=dict(parse_qsl(parts.query)), headers=dict(request.headers.raw_items()))
        principal, refusal = await self._admit(connect)
        if refusal is not None:
            return connection.respond(refusal[0], refusal[1] + "\n")
        connection.bma_connect = (connect, principal)
        return None

    async def _ws_handler(self, ws: Any) -> None:
        connect, principal = ws.bma_connect
        await self._serve_channel(WebSocketConnection(ws), connect, principal)

    async def _admit(self, request: ConnectRequest) -> tuple[Principal, tuple[HTTPStatus, str] | None]:
        principal = Principal()
        if self._authenticate is not None:
            try:
                outcome = self._authenticate(request)
                principal = await outcome if inspect.isawaitable(outcome) else outcome  # type: ignore[assignment]
            except Exception:
                return principal, (HTTPStatus.UNAUTHORIZED, "unauthorized")
        channel = request.query.get("channel", "")
        if channel in ("", "control"):
            return principal, None
        if channel != "mcp":
            return principal, (HTTPStatus.BAD_REQUEST, "unknown channel")
        session_id, server_key = request.query.get("session_id", ""), request.query.get("server_key", "")
        if not session_id or not server_key:
            return principal, (HTTPStatus.BAD_REQUEST, "session_id and server_key are required")
        session = self._sessions.get(session_id)
        # Only the executor holding the session may serve its MCP servers.
        if session is None or (principal.executor_id and principal.executor_id != session.executor.id):
            return principal, (HTTPStatus.FORBIDDEN, "session is not active for this executor")
        return principal, None

    async def _serve_channel(self, conn: Connection, request: ConnectRequest, principal: Principal) -> None:
        if request.query.get("channel") == "mcp":
            await self._serve_mcp(conn, request)
            return
        self._next_conn += 1
        await _HarnessConn(self, conn, principal, f"conn_{self._next_conn}_{_random_id(4)}").run()

    async def _serve_mcp(self, conn: Connection, request: ConnectRequest) -> None:
        session = self._sessions.get(request.query["session_id"])
        if session is None:
            await conn.close(CLOSE_POLICY_VIOLATION, "session ended")
            return
        channel = HarnessMCPChannel(conn)
        if not session._attach_mcp(request.query["server_key"], channel):  # noqa: SLF001
            await conn.close(CLOSE_POLICY_VIOLATION, "session ended")
            return
        # The handler owns the connection until the channel is closed.
        await channel._closed.wait()  # noqa: SLF001

    # --- sessions ---

    def _bind(self, executor: Executor, session: Session) -> None:
        if session.id in self._sessions:
            raise ExecutorError(f"session {session.id} is already bound")
        if executor._gone:  # noqa: SLF001
            raise NotConnectedError()
        self._sessions[session.id] = session
        executor._sessions[session.id] = session  # noqa: SLF001

    def _unbind(self, session: Session) -> None:
        if self._sessions.get(session.id) is session:
            del self._sessions[session.id]
        if session.executor._sessions.get(session.id) is session:  # noqa: SLF001
            del session.executor._sessions[session.id]  # noqa: SLF001


class Executor:
    """One registered executor. It outlives a single connection: a reconnect
    with resume.executor_id within the lease keeps its sessions and pending
    tool calls."""

    def __init__(self, harness: Harness, executor_id: str) -> None:
        self._h = harness
        self.id = executor_id
        self._conn: _HarnessConn | None = None
        self.info: ExecutorInfo = {}  # type: ignore[typeddict-item]
        self.capabilities: dict[str, Any] = {}
        self.environment_id = ""
        self.protocol = ""
        self._sessions: dict[str, Session] = {}
        self._lease_timer: asyncio.TimerHandle | None = None
        self._draining = False
        self._gone = False
        self._event_ids: dict[str, SessionEventResult] = {}

    @property
    def connected(self) -> bool:
        return self._conn is not None

    def sessions(self) -> list[Session]:
        return list(self._sessions.values())

    async def _call(self, method: str, params: Any) -> Any:
        conn = self._conn
        if conn is None:
            raise NotConnectedError()
        return await conn.peer.call(method, params, self._h.request_timeout)

    async def drain(self, reason: str = "") -> ExecutorDrainResult:
        """Ask the executor to stop accepting sessions; heartbeats report
        drain=true from now on."""
        self._draining = True
        return await self._call(METHOD_EXECUTOR_DRAIN, {"reason": reason})

    async def assign_session(self, params: SessionAssignParams) -> Session:
        """Bind a session and send session.assign. work_id and the lease token
        are generated when empty. The session is bound before the request is
        sent, so session.ready and MCP channels racing the answer are accepted."""
        params = dict(params)  # type: ignore[assignment]
        if not str(params.get("session_id", "")).strip():
            raise ValueError("bma_sdk.executor: session_id is required")
        if not params.get("work_id"):
            params["work_id"] = f"work_{_random_id(8)}"
        lease = dict(params.get("lease") or {})
        if not lease.get("token"):
            lease["token"] = f"lease_{_random_id(16)}"
        lease.setdefault("fencing_token", 0)
        params["lease"] = lease  # type: ignore[typeddict-item]
        if not params.get("environment_id"):
            params["environment_id"] = self.environment_id
        session = Session(self, params["session_id"], params["work_id"], lease["token"])
        self._h._bind(self, session)  # noqa: SLF001
        try:
            result = await self._call(METHOD_SESSION_ASSIGN, params)
            if not result.get("accepted"):
                raise AssignRejectedError()
        except BaseException:
            session._end("assign_failed", SessionEndedError)  # noqa: SLF001
            raise
        return session

    def _session(self, session_id: str) -> Session | None:
        return self._sessions.get(session_id)

    def _forget(self, reason: str) -> None:
        self._gone = True
        for session in self.sessions():
            session._end(reason, ToolOutcomeUnknownError)  # noqa: SLF001
        if self._h._executors.get(self.id) is self:  # noqa: SLF001
            del self._h._executors[self.id]  # noqa: SLF001
        self._h._signal()  # noqa: SLF001

    # --- agent capability (Go: agent_harness.go) ---

    def agent_capability(self) -> AgentCapability | None:
        """The advertised agent capability, None when absent."""
        return self.capabilities.get("agent")

    def _require_agent(self) -> None:
        capability = self.agent_capability()
        if not capability or not capability.get("enabled"):
            raise RPCError(JsonRpcErrorCode.CapabilityUnavailable, "executor does not advertise the agent capability")

    async def start_turn(self, params: TurnStartParams) -> TurnStartResult:
        """Send turn.start; safe to repeat with the same turn_id."""
        self._require_agent()
        return await self._call(METHOD_TURN_START, params)

    async def send_turn_input(self, params: TurnInputParams) -> Ack:
        """Send turn.input; a conflict means the turn is over and the input
        belongs in a new turn.start."""
        return await self._call(METHOD_TURN_INPUT, params)

    async def cancel_turn(self, params: TurnCancelParams) -> TurnCancelResult:
        """Send turn.cancel; the turn's end still arrives as session.status_idle."""
        return await self._call(METHOD_TURN_CANCEL, params)

    async def close_agent_session(self, params: SessionCloseParams) -> SessionCloseResult:
        return await self._call(METHOD_SESSION_CLOSE, params)

    async def respond_interaction(self, params: InteractionResponseParams) -> InteractionResponseResult:
        """Deliver the platform's answer; accepted=false means the executor no
        longer waits for it."""
        return await self._call(METHOD_INTERACTION_RESPONSE, params)


class Session:
    """One session bound to an executor."""

    def __init__(self, executor: Executor, session_id: str, work_id: str, lease_token: str) -> None:
        self.executor = executor
        self.id = session_id
        self.work_id = work_id
        self._lease_token = lease_token
        self._ready = asyncio.Event()
        self._done = asyncio.Event()
        self.end_reason = ""
        self._calls: dict[str, asyncio.Future[ToolResultParams]] = {}
        self._mcp: dict[str, HarnessMCPChannel] = {}
        self._mcp_changed = asyncio.Event()

    @property
    def ended(self) -> bool:
        return self._done.is_set()

    async def wait_ended(self) -> str:
        await self._done.wait()
        return self.end_reason

    async def ready(self) -> None:
        """Wait for session.ready."""
        ready = asyncio.ensure_future(self._ready.wait())
        done = asyncio.ensure_future(self._done.wait())
        try:
            await asyncio.wait({ready, done}, return_when=asyncio.FIRST_COMPLETED)
        finally:
            ready.cancel()
            done.cancel()
        if not self._ready.is_set():
            raise SessionEndedError()

    async def call_tool(self, params: ToolCallParams) -> ToolResultParams:
        """Send tool.call and wait for its tool.result. Idempotent by
        tool_call_id (generated when empty): repeating an id joins the in-flight
        call or returns the recorded result. Canceling the awaiting task stops
        waiting only; use cancel_tool to abort the tool."""
        params = dict(params, session_id=self.id)  # type: ignore[assignment]
        if not params.get("tool_call_id"):
            params["tool_call_id"] = f"call_{_random_id(8)}"
        call_id = params["tool_call_id"]
        future = self._calls.get(call_id)
        if future is None:
            if self.end_reason:
                raise SessionEndedError()
            future = asyncio.get_running_loop().create_future()
            self._calls[call_id] = future
            try:
                ack = await self.executor._call(METHOD_TOOL_CALL, params)  # noqa: SLF001
                if not ack.get("accepted"):
                    raise ExecutorError("executor did not accept tool.call")
            except BaseException:
                # Not dispatched: forget the call so the same id can be retried.
                if self._calls.get(call_id) is future:
                    del self._calls[call_id]
                if not future.done():
                    future.cancel()
                raise
        return await asyncio.shield(future)

    async def cancel_tool(self, tool_call_id: str, reason: str = "") -> bool:
        """Send tool.cancel. The waiting call_tool still receives the
        executor's tool.result (outcome canceled when the abort won)."""
        result = await self.executor._call(METHOD_TOOL_CANCEL, {"session_id": self.id, "tool_call_id": tool_call_id, "reason": reason})  # noqa: SLF001
        return bool(result.get("canceled"))

    async def stop(self, reason: str = "") -> None:
        """Send session.stop; the session ends locally on success."""
        await self.executor._call(METHOD_SESSION_STOP, {"session_id": self.id, "reason": reason})  # noqa: SLF001
        self._end(reason, ToolOutcomeUnknownError)

    async def mcp(self, server_key: str) -> HarnessMCPChannel:
        """Wait for the executor to open the MCP channel for server_key. A newer
        channel replaces the old one; the session's end closes all of them."""
        while True:
            if self.end_reason:
                raise SessionEndedError()
            channel = self._mcp.get(server_key)
            if channel is not None:
                return channel
            await self._mcp_changed.wait()

    def _mark_ready(self) -> None:
        self._ready.set()

    def _complete(self, result: ToolResultParams) -> bool:
        future = self._calls.get(result["tool_call_id"])
        if future is None:
            return False
        if not future.done():
            future.set_result(result)
        return True

    def _end(self, reason: str, pending_error: type[Exception]) -> None:
        if self.end_reason:
            return
        self.end_reason = reason or "ended"
        for future in self._calls.values():
            if not future.done():
                future.set_exception(pending_error())
                future.exception()  # retrieved: nobody may be waiting
        channels, self._mcp = self._mcp, {}
        self._done.set()
        self._signal_mcp()
        for channel in channels.values():
            channel._spawn_close()  # noqa: SLF001
        self.executor._h._unbind(self)  # noqa: SLF001

    def _signal_mcp(self) -> None:
        changed, self._mcp_changed = self._mcp_changed, asyncio.Event()
        changed.set()

    def _attach_mcp(self, server_key: str, channel: HarnessMCPChannel) -> bool:
        if self.end_reason:
            return False
        previous = self._mcp.get(server_key)
        self._mcp[server_key] = channel

        def detach() -> None:
            if self._mcp.get(server_key) is channel:
                del self._mcp[server_key]

        channel._detach = detach  # noqa: SLF001
        self._signal_mcp()
        if previous is not None:
            previous._spawn_close()  # noqa: SLF001
        return True


class HarnessMCPChannel:
    """Harness end of one MCP channel, isolated by (session_id, server_key).
    Frames are raw MCP JSON-RPC; the harness is the MCP client."""

    def __init__(self, conn: Connection) -> None:
        self._conn = conn
        self._closed = asyncio.Event()
        self._detach: Callable[[], None] | None = None

    async def read(self) -> str:
        """Next raw MCP frame. A transport error closes the channel; the
        executor reopens a fresh one, so call Session.mcp again."""
        try:
            return await self._conn.recv()
        except ChannelClosed:
            await self.close()
            raise

    async def write(self, frame: str) -> None:
        await self._conn.send(frame)

    async def close(self) -> None:
        if self._closed.is_set():
            return
        self._closed.set()
        if self._detach is not None:
            self._detach()
        await self._conn.close(CLOSE_NORMAL, "mcp channel closed")

    def _spawn_close(self) -> None:
        asyncio.ensure_future(self.close())


class _HarnessConn:
    def __init__(self, harness: Harness, conn: Connection, principal: Principal, conn_id: str) -> None:
        self.h = harness
        self.conn = conn
        self.principal = principal
        self.id = conn_id
        self.executor: Executor | None = None
        self.peer = Peer(
            conn, id_prefix="h", on_request=self._handle_request, on_notification=self._handle_notification,
            on_log=harness._log, inline=frozenset({METHOD_EXECUTOR_REGISTER}),  # noqa: SLF001
        )

    async def run(self) -> None:
        await self.peer.run()
        self.peer.cancel_tasks()
        self._disconnected()

    def _disconnected(self) -> None:
        executor = self.executor
        if executor is None or executor._conn is not self or executor._gone:  # noqa: SLF001
            return
        executor._conn = None  # noqa: SLF001

        def expire() -> None:
            if executor._conn is None and not executor._gone:  # noqa: SLF001
                executor._forget("lease_expired")  # noqa: SLF001

        executor._lease_timer = asyncio.get_running_loop().call_later(self.h.lease_ttl, expire)  # noqa: SLF001

    async def _handle_request(self, request_id: str, method: str, params: Any) -> Any:
        if method == METHOD_EXECUTOR_REGISTER:
            return self._register(params)
        executor = self.executor
        if executor is None:
            raise rpc_error(JsonRpcErrorCode.InvalidRequest, "executor.register must come first", False)
        if not isinstance(params, dict):
            raise rpc_error(JsonRpcErrorCode.InvalidParams, f"invalid {method} params", False)
        from .agent import _dispatch_agent

        handled, result = await _dispatch_agent(self.h, executor, method, params)
        if handled:
            return result
        return await self._dispatch(executor, request_id, method, params)

    def _register(self, params: Any) -> dict[str, Any]:
        if self.executor is not None:
            raise rpc_error(JsonRpcErrorCode.InvalidRequest, "already registered", False)
        if not isinstance(params, dict) or not isinstance(params.get("executor"), dict):
            raise rpc_error(JsonRpcErrorCode.InvalidParams, "invalid executor.register params", False)
        if params.get("protocol") != self.conn.subprotocol:
            raise rpc_error(JsonRpcErrorCode.UnsupportedProtocol, "protocol must match the negotiated subprotocol", False)
        resume = params.get("resume") or {}
        resume_id = resume.get("executor_id") or ""
        executor_id = self.principal.executor_id
        if executor_id and resume_id and resume_id != executor_id:
            raise rpc_error(JsonRpcErrorCode.Forbidden, "resume.executor_id does not match the authenticated executor", False)
        if not executor_id:
            executor_id = resume_id or f"exe_{_random_id(8)}"

        h = self.h
        executor = h._executors.get(executor_id)  # noqa: SLF001
        if executor is None:
            executor = Executor(h, executor_id)
            h._executors[executor_id] = executor  # noqa: SLF001
        previous = executor._conn  # noqa: SLF001
        executor._conn = self  # noqa: SLF001
        executor.info = params["executor"]
        executor.capabilities = params.get("capabilities") or {}
        executor.environment_id = params.get("environment_id", "")
        executor.protocol = params["protocol"]
        if executor._lease_timer is not None:  # noqa: SLF001
            executor._lease_timer.cancel()  # noqa: SLF001
            executor._lease_timer = None  # noqa: SLF001
        self.executor = executor
        if previous is not None:
            # The new connection is authoritative: 4004 replaced.
            h._spawn(previous.conn.close(CLOSE_REPLACED, "replaced by a newer connection"))  # noqa: SLF001
        # Sessions the executor no longer reports were lost with its old process.
        keep = set(resume.get("active_sessions") or [])
        for session in executor.sessions():
            if session.id not in keep:
                session._end("executor_lost", ToolOutcomeUnknownError)  # noqa: SLF001
        h._signal()  # noqa: SLF001
        if h._on_executor is not None:  # noqa: SLF001
            outcome = h._on_executor(executor)  # noqa: SLF001
            if inspect.isawaitable(outcome):
                h._spawn(outcome)  # noqa: SLF001
        return {
            "executor_id": executor_id,
            "connection_id": self.id,
            "server_time": _now(),
            "heartbeat_interval_ms": int(h.heartbeat_interval * 1000),
            "lease_ttl_ms": int(h.lease_ttl * 1000),
            "max_control_frame_bytes": MAX_FRAME_BYTES,
            "max_mcp_frame_bytes": MAX_FRAME_BYTES,
            "drain": executor._draining,  # noqa: SLF001
            "protocol": params["protocol"],
        }

    async def _dispatch(self, executor: Executor, request_id: str, method: str, params: dict[str, Any]) -> Any:
        if method == METHOD_EXECUTOR_HEARTBEAT:
            return {"server_time": _now(), "lease_ttl_ms": int(self.h.lease_ttl * 1000), "drain": executor._draining}  # noqa: SLF001
        if method == METHOD_SESSION_READY:
            session = executor._session(params.get("session_id", ""))  # noqa: SLF001
            if session is None:
                raise rpc_error(JsonRpcErrorCode.SessionNotActive, "session is not active on this executor", False)
            if params.get("lease_token") and params["lease_token"] != session._lease_token:  # noqa: SLF001
                raise rpc_error(JsonRpcErrorCode.LeaseExpired, "lease token does not match", False)
            session._mark_ready()  # noqa: SLF001
            return {"accepted": True}
        if method == METHOD_SESSION_ENDED:
            session = executor._session(params.get("session_id", ""))  # noqa: SLF001
            if session is not None:
                session._end(params.get("reason", ""), ToolOutcomeUnknownError)  # noqa: SLF001
            return {"accepted": True}
        if method == METHOD_TOOL_RESULT:
            if not params.get("tool_call_id"):
                raise rpc_error(JsonRpcErrorCode.InvalidParams, "invalid tool.result params", False)
            session = executor._session(params.get("session_id", ""))  # noqa: SLF001
            if session is None:
                raise rpc_error(JsonRpcErrorCode.SessionNotActive, "session is not active on this executor", False)
            if not session._complete(params):  # type: ignore[arg-type]  # noqa: SLF001
                raise rpc_error(JsonRpcErrorCode.NotFound, "tool_call_id was not dispatched by this harness", False)
            return {"accepted": True}
        if method == METHOD_SESSION_EVENT:
            if executor._session(params.get("session_id", "")) is None:  # noqa: SLF001
                raise rpc_error(JsonRpcErrorCode.SessionNotActive, "session is not active on this executor", False)
            previous = executor._event_ids.get(request_id)  # noqa: SLF001
            if previous is not None:
                return previous
            result: SessionEventResult = {"accepted": True, "event_id": f"evt_{_random_id(8)}"}
            if self.h._on_session_event is not None:  # noqa: SLF001
                try:
                    result = await self.h._on_session_event(executor, params)  # type: ignore[arg-type]  # noqa: SLF001
                except Exception as error:
                    raise rpc_error(JsonRpcErrorCode.InternalError, str(error), True) from error
            if len(executor._event_ids) >= _MAX_REMEMBERED_EVENT_IDS:  # noqa: SLF001
                executor._event_ids.clear()  # noqa: SLF001
            executor._event_ids[request_id] = result  # noqa: SLF001
            return result
        raise rpc_error(JsonRpcErrorCode.MethodNotFound, f"unknown method {method}", False)

    def _handle_notification(self, method: str, params: Any) -> None:
        executor = self.executor
        if executor is None or not isinstance(params, dict):
            return
        from .agent import _handle_agent_notification

        if _handle_agent_notification(self.h, executor, method, params):
            return
        h = self.h
        if method == METHOD_SESSION_STATUS and h._on_session_status is not None:  # noqa: SLF001
            h._on_session_status(executor, params)  # type: ignore[arg-type]  # noqa: SLF001
        elif method == METHOD_TOOL_PROGRESS and h._on_tool_progress is not None:  # noqa: SLF001
            h._on_tool_progress(executor, params)  # type: ignore[arg-type]  # noqa: SLF001
        elif method == METHOD_EXECUTOR_CAPABILITIES_UPDATE and isinstance(params.get("capabilities"), dict):
            executor.capabilities = params["capabilities"]
        elif method == METHOD_EXECUTOR_CLOSE:
            # A deliberate shutdown: no reconnect grace.
            executor._forget("executor_closed")  # noqa: SLF001
            h._spawn(self.conn.close(CLOSE_NORMAL, "executor.close"))  # noqa: SLF001


__all__ = [
    "AssignRejectedError",
    "Executor",
    "Harness",
    "HarnessMCPChannel",
    "Principal",
    "Session",
    "SessionEndedError",
    "ToolOutcomeUnknownError",
    "Unauthorized",
]
