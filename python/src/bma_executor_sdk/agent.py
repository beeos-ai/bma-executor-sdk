"""The agent capability: a complete local agent served over the executor protocol.

Executor side (Go: agent.go): ``ExecutorClient.serve_agent(handler)`` with an
:class:`AgentHandler`; the SDK owns turn.start/input/cancel, session.close,
event ids and sequences, ACK tracking and resend after reconnect,
interactions, and the session.status_running / session.status_idle events that
frame every turn.

Harness side (Go: agent_harness.go): ``Harness(agent=platform)`` with an
:class:`AgentPlatform`, plus ``Executor.start_turn`` and friends.
"""

from __future__ import annotations

import asyncio
from collections import deque
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any, Protocol

from ._rpc import ClientStoppedError, ExecutorError, NotConnectedError, RPCError, rpc_error
from .generated import (
    METHOD_INTERACTION_CANCEL,
    METHOD_INTERACTION_REQUEST,
    METHOD_INTERACTION_RESPONSE,
    METHOD_SESSION_CLOSE,
    METHOD_SESSION_ENDED,
    METHOD_SESSION_EVENT,
    METHOD_SESSION_EVENT_DELTA,
    METHOD_TURN_CANCEL,
    METHOD_TURN_INPUT,
    METHOD_TURN_START,
    AgentCapability,
    AgentEventType,
    ConnectionState,
    InteractionCancelParams,
    InteractionCancelReason,
    InteractionKind,
    InteractionOutcome,
    InteractionRequestParams,
    InteractionResponseParams,
    JsonRpcErrorCode,
    SessionEventDeltaParams,
    TurnInputParams,
    TurnOutcome,
    TurnStartParams,
    TurnStartResult,
)

if TYPE_CHECKING:
    from .client import ExecutorClient
    from .harness import Executor, Harness

_AGENT_INPUT_BUFFER = 16
_AGENT_RESEND_DELAY = 0.2
_AGENT_CLOSE_FLUSH_WAIT = 30.0


class AgentSessionClosedError(ExecutorError):
    """An AgentTurn method was used after session.close."""

    def __init__(self) -> None:
        super().__init__("agent session closed")


# --- executor side ---


class AgentHandler(Protocol):
    async def open_turn(self, params: TurnStartParams) -> TurnStartResult:
        """Decide whether to accept turn.start and report the configuration the
        agent will apply. It runs before turn.start is answered, so it must not
        do the turn's work. A raised RPCError keeps its code."""
        ...

    async def run_turn(self, turn: AgentTurn) -> None:
        """Do the turn's work. turn.cancel and session.close cancel this task.
        Returning ends the turn as completed, cancellation as canceled, any
        exception as failed (the SDK emits session.error first)."""
        ...

    async def close_session(self, session_id: str, reason: str) -> None:
        """Release the local conversation after session.close."""
        ...


class AgentTurn:
    """The handle run_turn uses to talk to the platform."""

    def __init__(self, session: _AgentSession, params: TurnStartParams) -> None:
        self._session = session
        self.params = params
        self.inputs: asyncio.Queue[TurnInputParams] = asyncio.Queue(_AGENT_INPUT_BUFFER)
        """turn.input messages in arrival order."""
        self._seen = {params.get("input_id", "")}
        self._task: asyncio.Task[Any] | None = None
        self._canceled = False
        self._done = asyncio.Event()
        self.ended = False

    @property
    def session_id(self) -> str:
        return self._session.id

    @property
    def turn_id(self) -> str:
        return self.params["turn_id"]

    def new_event_id(self) -> str:
        """A fresh event id, for a message streamed with delta() before it is
        emitted with emit_with_id()."""
        return self._session.new_event_id()

    def emit(self, event: Any) -> str:
        """Queue one durable event (an Agent*Event / Session*Event dict) and
        return its event id. Delivery is asynchronous, ordered and
        at-least-once; the platform deduplicates by event id."""
        return self.emit_with_id(self.new_event_id(), event)

    def emit_with_id(self, event_id: str, event: Any) -> str:
        if self.ended:
            raise ExecutorError("turn already ended")
        self._session.enqueue(self.turn_id, event_id, event)
        return event_id

    async def delta(self, event_id: str, event_type: str, index: int, text: str) -> None:
        """Stream a transient preview of the durable event event_id; dropped
        while disconnected and never resent."""
        client = self._session.runtime.client
        params: SessionEventDeltaParams = {
            "session_id": self.session_id, "turn_id": self.turn_id, "event_id": event_id,
            "event_type": event_type, "index": index, "text": text,
        }
        try:
            await client.notify(METHOD_SESSION_EVENT_DELTA, params)
        except (NotConnectedError, ClientStoppedError) as error:
            client._log("debug", "session.event_delta dropped", error)  # noqa: SLF001

    async def flush(self) -> None:
        """Wait until every event emitted so far is acknowledged."""
        await self._session.drained.wait()

    async def request_interaction(self, params: InteractionRequestParams) -> InteractionResponseParams:
        """Ask the platform for an approval or an answer and wait for
        interaction.response. When the task is canceled or expires_at passes,
        the interaction is withdrawn; expiry returns outcome expired, which the
        agent must treat as a deny."""
        s = self._session
        params = dict(params, session_id=s.id, turn_id=self.turn_id)  # type: ignore[assignment]
        if not params.get("interaction_id"):
            params["interaction_id"] = "int_" + self.new_event_id()[4:]
        interaction_id = params["interaction_id"]
        answer: asyncio.Future[InteractionResponseParams] = asyncio.get_running_loop().create_future()
        s.pending[interaction_id] = answer
        try:
            await self.flush()
            await s.call_until_connected(lambda: s.runtime.client.call(METHOD_INTERACTION_REQUEST, params))
            timeout = None
            if params.get("expires_at"):
                expires = datetime.fromisoformat(params["expires_at"].replace("Z", "+00:00"))
                timeout = max(0.0, (expires - datetime.now(timezone.utc)).total_seconds())
            try:
                return await asyncio.wait_for(asyncio.shield(answer), timeout)
            except asyncio.TimeoutError:
                await s.withdraw(interaction_id, InteractionCancelReason.Expired.value)
                return {"session_id": s.id, "interaction_id": interaction_id, "outcome": InteractionOutcome.Expired.value}  # type: ignore[typeddict-item]
            except asyncio.CancelledError:
                s.runtime.client._spawn(s.withdraw(interaction_id, InteractionCancelReason.TurnCanceled.value))  # noqa: SLF001
                raise
        finally:
            s.pending.pop(interaction_id, None)

    async def cancel_interaction(self, interaction_id: str, reason: str) -> None:
        """Withdraw an open interaction, for example after the user answered it
        locally (reason resolved_locally)."""
        await self._session.withdraw(interaction_id, reason)


class _AgentRuntime:
    def __init__(self, client: ExecutorClient, handler: AgentHandler, capability: AgentCapability) -> None:
        self.client = client
        self.handler = handler
        self.capability = capability
        self.sessions: dict[str, _AgentSession] = {}
        self.methods: dict[str, Callable[[Any], Awaitable[Any]]] = {
            METHOD_TURN_START: self.turn_start,
            METHOD_TURN_INPUT: self.turn_input,
            METHOD_TURN_CANCEL: self.turn_cancel,
            METHOD_SESSION_CLOSE: self.session_close,
            METHOD_INTERACTION_RESPONSE: self.interaction_response,
        }

    def session(self, session_id: str, create: bool) -> _AgentSession | None:
        session = self.sessions.get(session_id)
        if session is not None or not create:
            return session
        limit = self.capability.get("max_sessions") or 0
        if limit > 0 and len(self.sessions) >= limit:
            raise RPCError(JsonRpcErrorCode.ExecutorBusy, "executor_busy")
        session = _AgentSession(self, session_id)
        self.sessions[session_id] = session
        self.client._sessions.add(session_id)  # noqa: SLF001
        session.sender = self.client._spawn(session.send_loop())  # noqa: SLF001
        return session

    async def turn_start(self, params: TurnStartParams) -> TurnStartResult:
        if not params.get("session_id") or not params.get("turn_id") or not params.get("input"):
            raise RPCError(JsonRpcErrorCode.InvalidParams, "session_id, turn_id and input are required")
        s = self.session(params["session_id"], True)
        assert s is not None
        if params["turn_id"] in s.results:
            return s.results[params["turn_id"]]
        if s.turn is not None:
            raise RPCError(JsonRpcErrorCode.Conflict, "a turn is already running; use turn.input")
        turn = AgentTurn(s, params)
        s.turn = turn  # reserve the session before open_turn
        try:
            result = dict(await self.handler.open_turn(params))
            if not result.get("accepted"):
                raise RPCError(JsonRpcErrorCode.ExecutorBusy, "turn not accepted")
        except BaseException:
            s.turn = None
            raise
        result["turn_id"] = params["turn_id"]
        s.results[params["turn_id"]] = result  # type: ignore[assignment]
        self.client._spawn(self.run_turn(turn))  # noqa: SLF001
        return result  # type: ignore[return-value]

    async def run_turn(self, turn: AgentTurn) -> None:
        s = turn._session  # noqa: SLF001
        turn_id = turn.turn_id
        s.enqueue(turn_id, turn.new_event_id(), {"type": AgentEventType.SessionStatusRunning.value})
        inner = asyncio.ensure_future(self.handler.run_turn(turn))
        turn._task = inner  # noqa: SLF001
        if turn._canceled:  # noqa: SLF001  canceled before the work started
            inner.cancel()
        deadline_ms = turn.params.get("deadline_ms") or 0
        done, _ = await asyncio.wait({inner}, timeout=deadline_ms / 1000 if deadline_ms > 0 else None)
        turn.ended = True
        outcome = TurnOutcome.Completed
        if not done:
            inner.cancel()
            await asyncio.gather(inner, return_exceptions=True)
            outcome = TurnOutcome.Failed
            s.enqueue(turn_id, turn.new_event_id(), _session_error("timeout", "turn deadline_ms exceeded"))
        elif turn._canceled or inner.cancelled():  # noqa: SLF001
            outcome = TurnOutcome.Canceled
        elif inner.exception() is not None:
            outcome = TurnOutcome.Failed
            s.enqueue(turn_id, turn.new_event_id(), _session_error("agent_error", str(inner.exception())))
        s.enqueue(turn_id, turn.new_event_id(), {
            "type": AgentEventType.SessionStatusIdle.value, "stop_reason": {"type": "end_turn"}, "turn_outcome": outcome.value,
        })
        if s.turn is turn:
            s.turn = None
        turn._done.set()  # noqa: SLF001

    async def turn_input(self, params: TurnInputParams) -> dict[str, bool]:
        s = self.session(params.get("session_id", ""), False)
        if s is None:
            raise RPCError(JsonRpcErrorCode.SessionNotActive, "session_not_active")
        turn = s.turn
        if turn is None or turn.turn_id != params.get("turn_id") or turn.ended:
            raise RPCError(JsonRpcErrorCode.Conflict, "turn is not running; start a new turn")
        if params.get("input_id") in turn._seen:  # noqa: SLF001
            return {"accepted": True}
        turn._seen.add(params.get("input_id", ""))  # noqa: SLF001
        put = asyncio.ensure_future(turn.inputs.put(params))
        ended = asyncio.ensure_future(turn._done.wait())  # noqa: SLF001
        await asyncio.wait({put, ended}, return_when=asyncio.FIRST_COMPLETED)
        ended.cancel()
        if not put.done():
            put.cancel()
            raise RPCError(JsonRpcErrorCode.Conflict, "turn ended; start a new turn")
        return {"accepted": True}

    async def turn_cancel(self, params: dict[str, Any]) -> dict[str, bool]:
        s = self.session(params.get("session_id", ""), False)
        turn = s.turn if s is not None else None
        if turn is None or turn.turn_id != params.get("turn_id") or turn.ended:
            return {"canceled": False}
        _cancel_turn(turn)
        return {"canceled": True}

    async def session_close(self, params: dict[str, Any]) -> dict[str, bool]:
        s = self.session(params.get("session_id", ""), False)
        if s is None:
            return {"closed": False}
        turn = s.turn
        if turn is not None:
            _cancel_turn(turn)
            await turn._done.wait()  # noqa: SLF001
        try:
            await asyncio.wait_for(s.drained.wait(), _AGENT_CLOSE_FLUSH_WAIT)
        except asyncio.TimeoutError as error:
            self.client._log("warn", f"session.close: events of {s.id} still unacknowledged", error)  # noqa: SLF001
        await self.handler.close_session(s.id, params.get("reason", ""))
        s.closed = True
        s.wake.set()
        self.sessions.pop(s.id, None)
        self.client._sessions.discard(s.id)  # noqa: SLF001

        async def ended() -> None:
            try:
                await asyncio.wait_for(
                    s.call_until_connected(lambda: self.client.call(METHOD_SESSION_ENDED, {"session_id": s.id, "reason": params.get("reason", "")})),
                    _AGENT_CLOSE_FLUSH_WAIT,
                )
            except Exception as error:
                self.client._log("warn", f"session.ended for {s.id} failed", error)  # noqa: SLF001

        self.client._spawn(ended())  # noqa: SLF001
        return {"closed": True}

    async def interaction_response(self, params: InteractionResponseParams) -> dict[str, bool]:
        s = self.session(params.get("session_id", ""), False)
        if s is None:
            return {"accepted": False}
        interaction_id = params.get("interaction_id", "")
        waiter = s.pending.get(interaction_id)
        if waiter is None or interaction_id in s.answered or waiter.done():
            return {"accepted": False}
        s.answered.add(interaction_id)
        waiter.set_result(params)
        return {"accepted": True}


def _cancel_turn(turn: AgentTurn) -> None:
    turn._canceled = True  # noqa: SLF001
    if turn._task is not None:  # noqa: SLF001
        turn._task.cancel()  # noqa: SLF001


def _session_error(kind: str, message: str) -> dict[str, Any]:
    return {"type": AgentEventType.SessionError.value, "error": {"type": kind, "message": message}}


class _AgentSession:
    """One platform session on this executor: its outbox of durable events and
    at most one running turn."""

    def __init__(self, runtime: _AgentRuntime, session_id: str) -> None:
        self.runtime = runtime
        self.id = session_id
        self.next_seq = 1
        self.next_id = 0
        self.outbox: deque[dict[str, Any]] = deque()
        self.wake = asyncio.Event()
        self.drained = asyncio.Event()
        self.drained.set()
        self.closed = False
        self.turn: AgentTurn | None = None
        self.results: dict[str, TurnStartResult] = {}
        self.pending: dict[str, asyncio.Future[InteractionResponseParams]] = {}
        self.answered: set[str] = set()
        self.sender: asyncio.Task[Any] | None = None

    def new_event_id(self) -> str:
        self.next_id += 1
        return f"evt_{self.id}_{self.next_id}"

    def enqueue(self, turn_id: str, event_id: str, event: Any) -> None:
        if self.closed:
            raise AgentSessionClosedError()
        self.outbox.append({"session_id": self.id, "turn_id": turn_id, "event_id": event_id, "sequence": self.next_seq, "event": event})
        self.next_seq += 1
        self.drained.clear()
        self.wake.set()

    async def withdraw(self, interaction_id: str, reason: str) -> None:
        self.answered.add(interaction_id)
        client = self.runtime.client
        params: InteractionCancelParams = {"session_id": self.id, "interaction_id": interaction_id, "reason": reason}  # type: ignore[typeddict-item]
        try:
            await asyncio.wait_for(self.call_until_connected(lambda: client.call(METHOD_INTERACTION_CANCEL, params)), _AGENT_CLOSE_FLUSH_WAIT)
        except Exception as error:
            client._log("warn", f"interaction.cancel for {interaction_id} failed", error)  # noqa: SLF001

    async def send_loop(self) -> None:
        """Deliver the outbox head first, one event at a time, so the platform
        sees every session in sequence order. A transport failure keeps the
        event and waits for the next registration: the resend after reconnect."""
        client = self.runtime.client
        while True:
            if not self.outbox:
                if self.closed:
                    return
                self.wake.clear()
                await self.wake.wait()
                continue
            head = self.outbox[0]
            try:
                await self.call_until_connected(lambda: client.call(METHOD_SESSION_EVENT, head))
            except ClientStoppedError:
                return
            except RPCError as error:
                # A permanent rejection cannot be fixed by resending; surface it
                # instead of blocking the session.
                client._log("error", f"session.event {head['event_id']} rejected", error)  # noqa: SLF001
            self.outbox.popleft()
            if not self.outbox:
                self.drained.set()

    async def call_until_connected(self, call: Callable[[], Awaitable[Any]]) -> Any:
        """Retry call across disconnects. Only transport failures,
        executor_busy and retryable JSON-RPC errors are retried."""
        client = self.runtime.client
        while True:
            registered = client._registered  # noqa: SLF001
            try:
                return await call()
            except RPCError as error:
                if not error.retryable and error.code != JsonRpcErrorCode.ExecutorBusy:
                    raise
            except (NotConnectedError, TimeoutError):
                pass
            if client._stopped:  # noqa: SLF001
                raise ClientStoppedError()
            connected = client.state in (ConnectionState.Ready, ConnectionState.Draining)
            try:
                await asyncio.wait_for(registered.wait(), _AGENT_RESEND_DELAY if connected else None)
            except asyncio.TimeoutError:
                pass


# --- harness side ---


@dataclass(frozen=True)
class AgentEventRecord:
    """One validated durable agent event."""

    session_id: str
    turn_id: str
    event_id: str
    sequence: int
    type: str
    event: dict[str, Any]


class AgentPlatform(Protocol):
    """Where durable agent events, previews and interactions go."""

    async def acked_sequence(self, session_id: str) -> int:
        """Highest contiguous sequence already persisted (0 when none); asked
        once per session per process so a restarted platform keeps
        deduplicating resent events."""
        ...

    async def agent_event(self, executor: Executor | None, record: AgentEventRecord) -> None:
        """Persist one durable event; called once per event, in sequence order
        per session. Only after it returns is the event acknowledged."""
        ...

    def agent_delta(self, executor: Executor, delta: SessionEventDeltaParams) -> None:
        """Relay a transient preview. Never persisted."""
        ...

    async def interaction_requested(self, executor: Executor, request: InteractionRequestParams) -> str:
        """Open (or, for a repeated interaction_id, return) the platform
        interaction; answer it later with Executor.respond_interaction."""
        ...

    async def interaction_canceled(self, executor: Executor, cancel: InteractionCancelParams) -> None:
        ...


_AGENT_EVENT_TYPES = frozenset(member.value for member in AgentEventType)


class _AgentLedger:
    def __init__(self) -> None:
        self.acked: dict[str, int] = {}
        self.locks: dict[str, asyncio.Lock] = {}

    def lock(self, session_id: str) -> asyncio.Lock:
        return self.locks.setdefault(session_id, asyncio.Lock())


async def _dispatch_agent(h: Harness, executor: Executor, method: str, params: dict[str, Any]) -> tuple[bool, Any]:
    platform = h.agent
    if platform is None:
        return False, None
    if method == METHOD_SESSION_EVENT:
        if not params.get("event_id"):
            return False, None  # a tool executor's event
        return True, await agent_event(h, executor, params)
    if method == METHOD_INTERACTION_REQUEST:
        kind = params.get("kind")
        if (
            not params.get("session_id") or not params.get("interaction_id")
            or kind not in (InteractionKind.Permission.value, InteractionKind.Question.value)
            or (kind == InteractionKind.Permission.value and not params.get("tool_use_id"))
            or (kind == InteractionKind.Question.value and params.get("question") is None)
        ):
            raise rpc_error(JsonRpcErrorCode.InvalidParams, "invalid interaction.request params", False)
        try:
            platform_id = await platform.interaction_requested(executor, params)  # type: ignore[arg-type]
        except Exception as error:
            raise rpc_error(JsonRpcErrorCode.InternalError, str(error), True) from error
        return True, {"accepted": True, "platform_interaction_id": platform_id}
    if method == METHOD_INTERACTION_CANCEL:
        if not params.get("session_id") or not params.get("interaction_id"):
            raise rpc_error(JsonRpcErrorCode.InvalidParams, "invalid interaction.cancel params", False)
        try:
            await platform.interaction_canceled(executor, params)  # type: ignore[arg-type]
        except Exception as error:
            raise rpc_error(JsonRpcErrorCode.InternalError, str(error), True) from error
        return True, {"accepted": True}
    return False, None


async def agent_event(h: Harness, executor: Executor | None, params: dict[str, Any]) -> dict[str, Any]:
    """Validate and persist one agent session.event (sequence-checked)."""
    platform = h.agent
    assert platform is not None
    event = params.get("event")
    sequence = params.get("sequence")
    if (
        not params.get("session_id") or not params.get("turn_id") or not isinstance(sequence, int) or sequence < 1
        or not isinstance(event, dict) or event.get("type") not in _AGENT_EVENT_TYPES
    ):
        raise rpc_error(JsonRpcErrorCode.InvalidParams, "invalid agent session.event", False)
    session_id = params["session_id"]
    ledger = h._agent_ledger  # noqa: SLF001
    async with ledger.lock(session_id):
        acked = ledger.acked.get(session_id)
        if acked is None:
            try:
                acked = await platform.acked_sequence(session_id)
            except Exception as error:
                raise rpc_error(JsonRpcErrorCode.InternalError, str(error), True) from error
        result = {"accepted": True, "event_id": params["event_id"], "acked_sequence": acked}
        if sequence <= acked:
            return result
        if sequence != acked + 1:
            h._log("warn", f"agent session.event sequence gap in {session_id}")  # noqa: SLF001
            raise RPCError(JsonRpcErrorCode.Conflict, "conflict", {"retryable": False, "expected_sequence": acked + 1, "detail": f"expected_sequence={acked + 1}"})
        record = AgentEventRecord(session_id, params["turn_id"], params["event_id"], sequence, event["type"], event)
        try:
            await platform.agent_event(executor, record)
        except Exception as error:
            raise rpc_error(JsonRpcErrorCode.InternalError, str(error), True) from error
        ledger.acked[session_id] = sequence
        result["acked_sequence"] = sequence
        return result


def _handle_agent_notification(h: Harness, executor: Executor, method: str, params: dict[str, Any]) -> bool:
    if h.agent is None or method != METHOD_SESSION_EVENT_DELTA:
        return False
    if params.get("session_id") and params.get("event_id"):
        h.agent.agent_delta(executor, params)  # type: ignore[arg-type]
    return True


__all__ = ["AgentEventRecord", "AgentHandler", "AgentPlatform", "AgentSessionClosedError", "AgentTurn"]
