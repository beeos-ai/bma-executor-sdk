"""session.event failures must never leave a hole in a session's sequence: a
platform failure is resent, a gap answer renumbers from expected_sequence, and a
permanent rejection ends the turn visibly at the rejected sequence (11-12: one
rejected event left a session stuck on "sequence gap" for good). Same
scenarios as the Go agent_event_resync_test.go and the TypeScript
agent-event-resync.test.ts."""

from __future__ import annotations

import asyncio
from typing import Any

import bma_executor_sdk as bx
from bma_executor_sdk.agent import _AgentRuntime
from bma_executor_sdk.generated import ConnectionState, JsonRpcErrorCode


class FakeClient:
    """The platform side of session.event, scripted per call."""

    def __init__(self) -> None:
        self.stored: list[dict[str, Any]] = []
        self.sent: list[int] = []
        self.acked = 0
        self.failures: dict[int, bx.RPCError] = {}
        """Answers the n-th call (0-based) with an error instead of storing it."""
        self.refuse: set[str] = set()
        """Event types refused for good, every time."""
        self.calls = 0
        self._registered = asyncio.Event()
        self._registered.set()
        self._stopped = False
        self._sessions: set[str] = set()
        self.state = ConnectionState.Ready
        self.tasks: set[asyncio.Task[Any]] = set()

    async def _call(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        if method != "session.event":
            return {"accepted": True}
        self.sent.append(params["sequence"])
        call, self.calls = self.calls, self.calls + 1
        if call in self.failures:
            raise self.failures[call]
        if params["event"]["type"] in self.refuse:
            raise bx.RPCError(JsonRpcErrorCode.InvalidParams, "bad event")
        if params["sequence"] <= self.acked:
            return {"accepted": True, "acked_sequence": self.acked}
        if params["sequence"] != self.acked + 1:
            raise bx.RPCError(JsonRpcErrorCode.Conflict, "conflict", {"retryable": False, "expected_sequence": self.acked + 1})
        self.acked = params["sequence"]
        self.stored.append({"sequence": params["sequence"], "type": params["event"]["type"], "event": params["event"]})
        return {"accepted": True, "acked_sequence": self.acked}

    def _spawn(self, coro: Any) -> asyncio.Task[Any]:
        task = asyncio.ensure_future(coro)
        self.tasks.add(task)
        return task

    def _log(self, *_: Any) -> None:
        pass

    def types(self) -> list[str]:
        return [s["type"] for s in self.stored]

    def sequences(self) -> list[int]:
        return [s["sequence"] for s in self.stored]

    async def close(self) -> None:
        self._stopped = True
        for task in self.tasks:
            task.cancel()
        await asyncio.gather(*self.tasks, return_exceptions=True)


class Agent:
    """Emits three events; then waits for release or cancellation and records
    the turn's event_rejected."""

    def __init__(self, release: asyncio.Event) -> None:
        self.release = release
        self.rejected: list[Any] = []

    async def open_turn(self, params: Any) -> Any:
        return {"accepted": True, "turn_id": params["turn_id"], "executor_session_ref": "ref", "resumed": False, "effective": {}}

    async def run_turn(self, turn: bx.AgentTurn) -> None:
        turn.emit({"type": "agent.message", "content": [{"type": "text", "text": "one"}]})
        turn.emit({"type": "agent.tool_use", "name": "bash", "input": {}})
        turn.emit({"type": "agent.message", "content": [{"type": "text", "text": "two"}]})
        try:
            await self.release.wait()
        finally:
            self.rejected.append(turn.event_rejected)

    async def close_session(self, session_id: str, reason: str) -> None:
        pass


async def run(client: FakeClient, release: asyncio.Event) -> Agent:
    agent = Agent(release)
    runtime = _AgentRuntime(client, agent, {"enabled": True})  # type: ignore[arg-type,typeddict-item]
    await runtime.turn_start({"session_id": "ses_1", "turn_id": "t_1", "input_id": "in_1", "input": [{"type": "text", "text": "go"}]})  # type: ignore[typeddict-item]
    return agent


async def wait_for(predicate: Any, timeout: float = 5.0) -> None:
    async def loop() -> None:
        while not predicate():
            await asyncio.sleep(0.01)

    await asyncio.wait_for(loop(), timeout)


def has_idle(client: FakeClient) -> bool:
    return "session.status_idle" in client.types()


async def test_platform_failure_is_resent_in_place() -> None:
    client = FakeClient()
    # -32603 not marked retryable: the platform's own failure, the event is idempotent.
    client.failures[2] = bx.RPCError(JsonRpcErrorCode.InternalError, "internal error")
    release = asyncio.Event()
    release.set()
    try:
        await run(client, release)
        await wait_for(lambda: has_idle(client))
        assert client.types() == ["session.status_running", "agent.message", "agent.tool_use", "agent.message", "session.status_idle"]
        assert client.sequences() == [1, 2, 3, 4, 5]
    finally:
        await client.close()


async def test_gap_answer_renumbers_from_expected_sequence() -> None:
    client = FakeClient()
    release = asyncio.Event()
    try:
        await run(client, release)
        await wait_for(lambda: len(client.stored) == 4)
        # The platform starts a new epoch for the session: it expects 1 again.
        client.acked = 0
        client.stored.clear()
        release.set()
        await wait_for(lambda: has_idle(client))
        assert client.sequences() == [1], "the next event is renumbered to the expected sequence"
        assert client.sent[-2] == 5, "it was first sent as 5 and refused as a gap"
    finally:
        await client.close()


async def test_permanent_rejection_ends_the_turn_at_that_sequence() -> None:
    client = FakeClient()
    client.refuse.add("agent.tool_use")
    release = asyncio.Event()
    try:
        agent = await run(client, release)
        await wait_for(lambda: has_idle(client))
        assert client.types() == ["session.status_running", "agent.message", "session.error", "session.status_idle"]
        assert client.sequences() == [1, 2, 3, 4], "the terminal events take the rejected sequence on"
        error = client.stored[2]["event"]["error"]
        assert error["type"] == "agent_error" and error["message"].startswith("event_rejected: ")
        assert client.stored[3]["event"]["stop_reason"] == {"type": "retries_exhausted"}
        assert client.stored[3]["event"]["turn_outcome"] == "failed"
        await wait_for(lambda: agent.rejected)
        assert isinstance(agent.rejected[0], bx.EventRejectedError), "the turn is stopped"
        release.set()
        await asyncio.sleep(0.05)
        assert len(client.stored) == 4, "nothing of the ended turn follows"
    finally:
        await client.close()
