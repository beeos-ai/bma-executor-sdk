"""Executor, harness and agent ends against each other (go/executor
harness_test.go, lifecycle_test.go and agent_test.go scenarios)."""

from __future__ import annotations

import asyncio
import contextlib
import os
import tempfile
from collections.abc import AsyncIterator
from typing import Any

import pytest

import bma_executor_sdk as bx
from bma_executor_sdk.generated import JsonRpcErrorCode
from executor_helpers import RecordingPlatform

EXECUTOR = {"name": "py-test", "os": "linux", "arch": "arm64", "version": "0.1.0"}


@contextlib.asynccontextmanager
async def listen(harness: bx.Harness, transport: str) -> AsyncIterator[tuple[str, bx.Dialer | None]]:
    """Yield (ws_url, dialer) for the transport under test."""
    if transport == "inprocess":
        yield "inprocess://harness", harness.inprocess_dialer()
    elif transport == "ws":
        server = await harness.serve("127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        try:
            yield f"ws://127.0.0.1:{port}/v1/executors/channel", None
        finally:
            server.close()
            await server.wait_closed()
    else:
        directory = tempfile.mkdtemp(prefix="bmaexec")
        path = os.path.join(directory, "h.sock")
        server = await harness.serve_unix(path)
        try:
            yield f"unix://{path}", None
        finally:
            server.close()
            await server.wait_closed()
            os.unlink(path) if os.path.exists(path) else None
            os.rmdir(directory)


def make_client(url: str, dialer: bx.Dialer | None, token: str = "", **options: Any) -> bx.ExecutorClient:
    options.setdefault("request_timeout", 2.0)
    options.setdefault("reconnect_base_delay", 0.02)
    options.setdefault("random", lambda: 0.0)
    return bx.ExecutorClient(environment_id="env_local", executor=EXECUTOR, token_source=bx.static_token(url, token), dialer=dialer, **options)


def text_result(call: dict[str, Any], text: str) -> dict[str, Any]:
    return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"], "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": text}]}


@pytest.mark.parametrize("transport", ["inprocess", "ws", "unix"])
async def test_harness_drives_executor_client(transport: str) -> None:
    harness = bx.Harness(request_timeout=2.0)
    runs: list[str] = []
    started = asyncio.Event()

    async def on_tool(call: dict[str, Any]) -> dict[str, Any]:
        runs.append(call["tool_call_id"])
        if call["tool"] == "sleep":
            started.set()
            await asyncio.sleep(30)
        return text_result(call, f"ran {call['input']['command']}")

    async with listen(harness, transport) as (url, dialer):
        client = make_client(url, dialer)
        client.on_tool_call(on_tool)
        registered = await client.start()
        assert registered["protocol"] == bx.PROTOCOL_V11
        executor = await harness.wait_executor()
        assert executor.id == client.executor_id and executor.info["name"] == "py-test"
        assert executor.protocol == bx.PROTOCOL_V11

        session = await executor.assign_session({"session_id": "ses_1"})  # type: ignore[typeddict-item]
        assert client.active_sessions == ["ses_1"]
        await client.session_ready({"session_id": "ses_1", "work_id": session.work_id, "lease_token": session._lease_token})  # noqa: SLF001
        await asyncio.wait_for(session.ready(), 2)

        result = await session.call_tool({"tool_call_id": "call_1", "tool": "bash", "input": {"command": "ls"}})  # type: ignore[typeddict-item]
        assert result["outcome"] == "succeeded" and result["content"][0]["text"] == "ran ls"
        # Idempotent by tool_call_id: the recorded result, no second run.
        again = await session.call_tool({"tool_call_id": "call_1", "tool": "bash", "input": {"command": "ls"}})  # type: ignore[typeddict-item]
        assert again == result and runs == ["call_1"]

        slow = asyncio.ensure_future(session.call_tool({"tool_call_id": "call_2", "tool": "sleep", "input": {}}))  # type: ignore[typeddict-item]
        await asyncio.wait_for(started.wait(), 2)
        assert await session.cancel_tool("call_2", "user") is True
        canceled = await asyncio.wait_for(slow, 2)
        assert canceled["outcome"] == "canceled" and canceled["content"][0]["text"] == "canceled: user"

        await session.stop("done")
        assert session.ended and session.end_reason == "done" and client.active_sessions == []
        await client.stop("test done")
        assert client.state == bx.generated.ConnectionState.Closed


async def test_harness_authenticate_rejects_and_pins_identity() -> None:
    def authenticate(request: bx.ConnectRequest) -> bx.Principal:
        if request.bearer_token != "secret":
            raise bx.Unauthorized()
        return bx.Principal(executor_id="exe_pinned")

    harness = bx.Harness(authenticate=authenticate)
    async with listen(harness, "ws") as (url, _):
        bad = make_client(url, None, token="wrong")
        with pytest.raises(Exception, match="401"):
            await bad.start()
        good = make_client(url, None, token="secret")
        result = await good.start()
        assert result["executor_id"] == "exe_pinned"
        await good.stop()

    # In-process channels go through the same hook.
    with pytest.raises(ConnectionRefusedError):
        await make_client("inprocess://", harness.inprocess_dialer(), token="wrong").start()


async def test_tool_result_semantics_and_lost_results() -> None:
    harness = bx.Harness()
    async with listen(harness, "inprocess") as (url, dialer):
        lost: list[tuple[dict[str, Any], BaseException]] = []
        client = make_client(url, dialer, tool_result_retry_attempts=1, on_tool_result_lost=lambda r, e: lost.append((r, e)))
        await client.start()
        executor = await harness.wait_executor()
        await executor.assign_session({"session_id": "ses_1"})  # type: ignore[typeddict-item]

        with pytest.raises(bx.RPCError) as unknown:
            await client.report_tool_result({"session_id": "ses_1", "tool_call_id": "never", "outcome": "succeeded", "is_error": False, "content": []})  # type: ignore[typeddict-item]
        assert unknown.value.code == JsonRpcErrorCode.NotFound and unknown.value.retryable is False
        with pytest.raises(bx.RPCError) as foreign:
            await client.report_tool_result({"session_id": "ses_other", "tool_call_id": "x", "outcome": "succeeded", "is_error": False, "content": []})  # type: ignore[typeddict-item]
        assert foreign.value.code == JsonRpcErrorCode.SessionNotActive

        # A result the harness cannot accept is handed to on_tool_result_lost.
        client.on_tool_call(lambda call: asyncio.sleep(0, text_result(call, "late")))
        await client._dispatch_request("r1", "tool.call", {"session_id": "ses_gone", "tool_call_id": "c9", "tool": "bash", "input": {}})  # noqa: SLF001
        for _ in range(100):
            if lost:
                break
            await asyncio.sleep(0.01)
        assert lost and lost[0][0]["tool_call_id"] == "c9" and isinstance(lost[0][1], bx.RPCError)
        await client.stop()


async def test_resume_delivers_pending_result_and_lease_expiry_is_unknown() -> None:
    harness = bx.Harness(lease_ttl=0.3, request_timeout=2.0)
    release = asyncio.Event()

    async def on_tool(call: dict[str, Any]) -> dict[str, Any]:
        await release.wait()
        return text_result(call, "after reconnect")

    async with listen(harness, "ws") as (url, _):
        client = make_client(url, None)
        client.on_tool_call(on_tool)
        await client.start()
        executor = await harness.wait_executor()
        session = await executor.assign_session({"session_id": "ses_1"})  # type: ignore[typeddict-item]
        pending = asyncio.ensure_future(session.call_tool({"tool_call_id": "call_1", "tool": "bash", "input": {}}))  # type: ignore[typeddict-item]
        await asyncio.sleep(0.05)
        first_connection = client.connection_id

        await client._peer.conn.close(1011, "network drop")  # noqa: SLF001
        for _ in range(200):
            if client.state == bx.generated.ConnectionState.Ready and client.connection_id != first_connection:
                break
            await asyncio.sleep(0.01)
        assert client.connection_id != first_connection
        assert client.executor_id == executor.id and not session.ended
        release.set()
        result = await asyncio.wait_for(pending, 2)
        assert result["content"][0]["text"] == "after reconnect"

        # Lease expiry: the executor is gone for good, pending calls are unknown.
        release.clear()
        pending = asyncio.ensure_future(session.call_tool({"tool_call_id": "call_2", "tool": "bash", "input": {}}))  # type: ignore[typeddict-item]
        await asyncio.sleep(0.05)
        await client.stop()  # sends executor.close: no reconnect grace
        with pytest.raises(bx.ToolOutcomeUnknownError):
            await asyncio.wait_for(pending, 2)
        assert session.end_reason == "executor_closed" and harness.executors() == []


async def test_lease_expiry_after_disconnect() -> None:
    harness = bx.Harness(lease_ttl=0.1)
    async with listen(harness, "inprocess") as (url, dialer):
        client = make_client(url, dialer, max_reconnect_attempts=1, reconnect_base_delay=5)
        client.on_tool_call(lambda call: asyncio.sleep(30))
        await client.start()
        executor = await harness.wait_executor()
        session = await executor.assign_session({"session_id": "ses_1"})  # type: ignore[typeddict-item]
        pending = asyncio.ensure_future(session.call_tool({"tool_call_id": "c", "tool": "bash", "input": {}}))  # type: ignore[typeddict-item]
        await asyncio.sleep(0.02)
        await client._peer.conn.close(1011, "drop")  # noqa: SLF001
        with pytest.raises(bx.ToolOutcomeUnknownError):
            await asyncio.wait_for(pending, 2)
        assert session.end_reason == "lease_expired"
        await client.stop()


async def test_newer_connection_replaces_older_with_4004() -> None:
    harness = bx.Harness()
    async with listen(harness, "ws") as (url, _):
        first = make_client(url, None)
        await first.start()
        second = make_client(url, None)
        second._executor_id = first.executor_id  # noqa: SLF001  resume as the same executor
        await second.start()
        for _ in range(200):
            if first.state == bx.generated.ConnectionState.Closed:
                break
            await asyncio.sleep(0.01)
        assert first.state == bx.generated.ConnectionState.Closed  # 4004 is terminal: no reconnect
        assert second.executor_id == first.executor_id
        await second.stop()
        await first.stop()


async def test_mcp_channel_is_isolated_by_session_and_server_key() -> None:
    harness = bx.Harness()
    async with listen(harness, "unix") as (url, _):
        client = make_client(url, None)
        await client.start()
        executor = await harness.wait_executor()
        session = await executor.assign_session({"session_id": "ses_1"})  # type: ignore[typeddict-item]
        channel = await client.open_mcp_channel("ses_1", "github")
        harness_end = await asyncio.wait_for(session.mcp("github"), 2)
        await harness_end.write('{"jsonrpc":"2.0","id":1,"method":"tools/list"}')
        assert await channel.read() == '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
        await channel.write('{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}')
        assert await harness_end.read() == '{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}'
        with pytest.raises(Exception, match="403"):
            await client.open_mcp_channel("ses_unknown", "github")
        await session.stop("done")
        with pytest.raises(bx.ChannelClosed):
            await asyncio.wait_for(channel.read(), 2)
        await client.stop()


# --- agent capability ---


class ScriptedAgent:
    """Deterministic local agent (Go: scriptedAgent)."""

    def __init__(self) -> None:
        self.closed: list[str] = []
        self.holding = asyncio.Event()

    async def open_turn(self, params: dict[str, Any]) -> dict[str, Any]:
        model = (params.get("config") or {}).get("model") or "local-model"
        return {
            "accepted": True, "turn_id": params["turn_id"], "executor_session_ref": "local_" + params["session_id"],
            "resumed": bool(params.get("executor_session_ref")),
            "effective": {"model": model, "thinking_level": "medium", "permission_mode": "default"},
        }

    async def run_turn(self, turn: bx.AgentTurn) -> None:
        text = turn.params["input"][0]["text"]
        if "hold" in text:
            self.holding.set()
            await asyncio.sleep(60)
        if "fail" in text:
            raise RuntimeError("model unavailable")
        message_id = turn.new_event_id()
        await turn.delta(message_id, "agent.message", 0, "Working on: ")
        turn.emit_with_id(message_id, {"type": "agent.message", "content": [{"type": "text", "text": "Working on: " + text}]})
        if "run" in text:
            tool_use_id = turn.emit({"type": "agent.tool_use", "name": "bash", "input": {"command": "pytest"}, "evaluated_permission": {"type": "ask"}})
            answer = await turn.request_interaction({
                "kind": "permission", "tool_use_id": tool_use_id, "tool_name": "bash", "tool_input": {"command": "pytest"}, "scopes": ["once", "session"],
            })  # type: ignore[typeddict-item]
            allowed = answer["outcome"] == "allow"
            turn.emit({"type": "agent.tool_result", "tool_use_id": tool_use_id, "content": [{"type": "text", "text": "ok" if allowed else "denied"}], "is_error": not allowed})
        while not turn.inputs.empty():
            extra = turn.inputs.get_nowait()
            turn.emit({"type": "agent.message", "content": [{"type": "text", "text": "also: " + extra["input"][0]["text"]}]})

    async def close_session(self, session_id: str, reason: str) -> None:
        self.closed.append(session_id)


def turn_start(session_id: str, turn_id: str, text: str) -> dict[str, Any]:
    return {"session_id": session_id, "turn_id": turn_id, "input_id": "in_" + turn_id, "input": [{"type": "text", "text": text}]}


@contextlib.asynccontextmanager
async def agent_pair(transport: str = "inprocess") -> AsyncIterator[tuple[bx.Harness, bx.Executor, bx.ExecutorClient, RecordingPlatform, ScriptedAgent]]:
    platform = RecordingPlatform()
    harness = bx.Harness(agent=platform, request_timeout=2.0)
    agent = ScriptedAgent()
    async with listen(harness, transport) as (url, dialer):
        client = make_client(url, dialer)
        client.serve_agent(agent, {"models": [{"id": "local-model", "thinking_levels": ["low", "medium", "high"]}], "permission_modes": ["default"], "interaction_kinds": ["permission", "question"]})  # type: ignore[arg-type,typeddict-item]
        await client.start()
        executor = await harness.wait_executor()
        try:
            yield harness, executor, client, platform, agent
        finally:
            await client.stop("test done")


def idle_of(platform: RecordingPlatform, turn_id: str) -> Any:
    return next((e for e in platform.events if e.turn_id == turn_id and e.type == "session.status_idle"), None)


@pytest.mark.parametrize("transport", ["inprocess", "ws"])
async def test_agent_turn_lifecycle(transport: str) -> None:
    async with agent_pair(transport) as (harness, executor, client, platform, agent):
        capability = executor.agent_capability()
        assert capability and capability["enabled"] and capability["models"][0]["id"] == "local-model"

        result = await executor.start_turn(turn_start("ses_a", "turn_1", "run the tests"))  # type: ignore[arg-type]
        assert result["accepted"] and result["effective"]["model"] == "local-model"
        assert await executor.start_turn(turn_start("ses_a", "turn_1", "run the tests")) == result  # type: ignore[arg-type]  # idempotent

        await platform.wait_for(lambda: platform.interactions)
        (interaction_id,) = platform.interactions
        answer = await executor.respond_interaction({"session_id": "ses_a", "interaction_id": interaction_id, "outcome": "allow"})  # type: ignore[typeddict-item]
        assert answer["accepted"] is True
        stale = await executor.respond_interaction({"session_id": "ses_a", "interaction_id": interaction_id, "outcome": "deny"})  # type: ignore[typeddict-item]
        assert stale["accepted"] is False

        await platform.wait_for(lambda: idle_of(platform, "turn_1"))
        assert platform.types("ses_a") == ["session.status_running", "agent.message", "agent.tool_use", "agent.tool_result", "session.status_idle"]
        assert [e.sequence for e in platform.events] == [1, 2, 3, 4, 5]
        assert idle_of(platform, "turn_1").event["turn_outcome"] == "completed"
        assert platform.events[3].event["is_error"] is False
        assert platform.deltas and platform.deltas[0]["event_id"] == platform.events[1].event_id

        # Cancel a held turn; a second turn.start while it runs is a conflict.
        await executor.start_turn(turn_start("ses_a", "turn_2", "hold on"))  # type: ignore[arg-type]
        await asyncio.wait_for(agent.holding.wait(), 2)
        with pytest.raises(bx.RPCError) as busy:
            await executor.start_turn(turn_start("ses_a", "turn_3", "next"))  # type: ignore[arg-type]
        assert busy.value.code == JsonRpcErrorCode.Conflict
        ack = await executor.send_turn_input({"session_id": "ses_a", "turn_id": "turn_2", "input_id": "in_x", "input": [{"type": "text", "text": "more"}]})  # type: ignore[typeddict-item]
        assert ack["accepted"]
        assert (await executor.cancel_turn({"session_id": "ses_a", "turn_id": "turn_2"}))["canceled"] is True  # type: ignore[typeddict-item]
        await platform.wait_for(lambda: idle_of(platform, "turn_2"))
        assert idle_of(platform, "turn_2").event["turn_outcome"] == "canceled"
        with pytest.raises(bx.RPCError) as over:
            await executor.send_turn_input({"session_id": "ses_a", "turn_id": "turn_2", "input_id": "in_y", "input": [{"type": "text", "text": "late"}]})  # type: ignore[typeddict-item]
        assert over.value.code == JsonRpcErrorCode.Conflict

        await executor.start_turn(turn_start("ses_a", "turn_4", "fail please"))  # type: ignore[arg-type]
        await platform.wait_for(lambda: idle_of(platform, "turn_4"))
        assert platform.types("ses_a")[-2:] == ["session.error", "session.status_idle"]
        assert idle_of(platform, "turn_4").event["turn_outcome"] == "failed"

        closed = await executor.close_agent_session({"session_id": "ses_a", "reason": "archived"})  # type: ignore[typeddict-item]
        assert closed["closed"] is True and agent.closed == ["ses_a"]
        assert (await executor.close_agent_session({"session_id": "ses_a"}))["closed"] is False  # type: ignore[typeddict-item]


async def test_agent_events_resent_after_reconnect() -> None:
    async with agent_pair("ws") as (harness, executor, client, platform, agent):
        platform.fail_next_event = True  # retryable internal error: the event stays queued
        await executor.start_turn(turn_start("ses_b", "turn_1", "hello"))  # type: ignore[arg-type]
        await platform.wait_for(lambda: idle_of(platform, "turn_1"))
        await client._peer.conn.close(1011, "drop")  # noqa: SLF001
        for _ in range(200):
            if client.state == bx.generated.ConnectionState.Ready and executor.connected:
                break
            await asyncio.sleep(0.01)
        await executor.start_turn(turn_start("ses_b", "turn_2", "again"))  # type: ignore[arg-type]
        await platform.wait_for(lambda: idle_of(platform, "turn_2"))
        sequences = [e.sequence for e in platform.events if e.session_id == "ses_b"]
        assert sequences == list(range(1, len(sequences) + 1))


async def test_harness_agent_sequence_rules() -> None:
    platform = RecordingPlatform()
    harness = bx.Harness(agent=platform)
    event = {"type": "agent.message", "content": [{"type": "text", "text": "hi"}]}

    async def send(sequence: int) -> dict[str, Any]:
        params = {"session_id": "ses", "turn_id": "t", "event_id": f"e{sequence}", "sequence": sequence, "event": event}
        return await bx.agent.agent_event(harness, None, params)

    assert (await send(1))["acked_sequence"] == 1
    assert (await send(1))["acked_sequence"] == 1  # duplicate: acked, not persisted twice
    with pytest.raises(bx.RPCError) as gap:
        await send(3)
    assert gap.value.code == JsonRpcErrorCode.Conflict and gap.value.data["expected_sequence"] == 2
    assert (await send(2))["acked_sequence"] == 2
    assert [e.sequence for e in platform.events] == [1, 2]
    with pytest.raises(bx.RPCError) as invalid:
        await bx.agent.agent_event(harness, None, {"session_id": "ses", "turn_id": "t", "event_id": "x", "sequence": 3, "event": {"type": "user.message"}})
    assert invalid.value.code == JsonRpcErrorCode.InvalidParams


async def test_start_turn_requires_agent_capability() -> None:
    harness = bx.Harness()
    async with listen(harness, "inprocess") as (url, dialer):
        client = make_client(url, dialer)
        await client.start()
        executor = await harness.wait_executor()
        with pytest.raises(bx.RPCError) as error:
            await executor.start_turn(turn_start("s", "t", "hi"))  # type: ignore[arg-type]
        assert error.value.code == JsonRpcErrorCode.CapabilityUnavailable
        await client.stop()
