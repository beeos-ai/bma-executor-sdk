"""Agent executor: a scripted local agent served over the executor protocol,
driven by an in-process harness with an in-memory platform.

    python agent_minimal.py

Against BMA, keep EchoAgent and serve_agent() and build the client as in
executor_minimal.py; BMA is then the platform.
"""

from __future__ import annotations

import asyncio
from typing import Any

from bma_sdk.executor import AgentEventRecord, AgentTurn, ExecutorClient, Harness, static_token


class EchoAgent:
    async def open_turn(self, params: dict[str, Any]) -> dict[str, Any]:
        return {"accepted": True, "turn_id": params["turn_id"], "executor_session_ref": "echo_" + params["session_id"], "resumed": False, "effective": {"model": "echo"}}

    async def run_turn(self, turn: AgentTurn) -> None:
        text = turn.params["input"][0]["text"]
        message_id = turn.new_event_id()
        await turn.delta(message_id, "agent.message", 0, "Echo: ")  # streaming preview
        turn.emit_with_id(message_id, {"type": "agent.message", "content": [{"type": "text", "text": "Echo: " + text}]})
        tool_use_id = turn.emit({"type": "agent.tool_use", "name": "bash", "input": {"command": "date"}, "evaluated_permission": {"type": "ask"}})
        answer = await turn.request_interaction({"kind": "permission", "tool_use_id": tool_use_id, "tool_name": "bash", "tool_input": {"command": "date"}, "scopes": ["once"]})  # type: ignore[typeddict-item]
        allowed = answer["outcome"] == "allow"
        turn.emit({"type": "agent.tool_result", "tool_use_id": tool_use_id, "is_error": not allowed, "content": [{"type": "text", "text": "ran" if allowed else "denied"}]})

    async def close_session(self, session_id: str, reason: str) -> None:
        print("closed", session_id, reason)


class PrintingPlatform:
    """AgentPlatform that prints events and allows every interaction."""

    def __init__(self) -> None:
        self.harness: Harness | None = None
        self.idle = asyncio.Event()

    async def acked_sequence(self, session_id: str) -> int:
        return 0

    async def agent_event(self, executor: Any, record: AgentEventRecord) -> None:
        print(f"#{record.sequence} {record.type}")
        if record.type == "session.status_idle":
            self.idle.set()

    def agent_delta(self, executor: Any, delta: dict[str, Any]) -> None:
        print("  delta:", delta["text"])

    async def interaction_requested(self, executor: Any, request: dict[str, Any]) -> str:
        print("  permission asked for", request["tool_name"])
        asyncio.ensure_future(executor.respond_interaction({"session_id": request["session_id"], "interaction_id": request["interaction_id"], "outcome": "allow"}))
        return "pint_" + request["interaction_id"]

    async def interaction_canceled(self, executor: Any, cancel: dict[str, Any]) -> None:
        return None


async def main() -> None:
    platform = PrintingPlatform()
    harness = Harness(agent=platform)
    client = ExecutorClient(environment_id="env_local", executor={"name": "echo-agent", "os": "local", "arch": "local", "version": "0.1.0"}, token_source=static_token("inprocess://"), dialer=harness.inprocess_dialer())  # type: ignore[typeddict-item]
    client.serve_agent(EchoAgent(), {"models": [{"id": "echo"}], "permission_modes": ["default"], "interaction_kinds": ["permission"]})  # type: ignore[arg-type,typeddict-item]
    await client.start()
    try:
        executor = await harness.wait_executor()
        await executor.start_turn({"session_id": "ses_demo", "turn_id": "turn_1", "input_id": "in_1", "input": [{"type": "text", "text": "hello"}]})  # type: ignore[typeddict-item]
        await asyncio.wait_for(platform.idle.wait(), 10)
        await executor.close_agent_session({"session_id": "ses_demo", "reason": "done"})  # type: ignore[typeddict-item]
    finally:
        await client.stop("done")


if __name__ == "__main__":
    asyncio.run(main())
