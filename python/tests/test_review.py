"""Regressions of research/typed-sdk-review-p12.md and typed-sdk-review-1.0.md
(EM1, EM2 and the adopted suggestions); mirrors review_test.go and
typescript/test/typed-tools.test.ts."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import bma_executor_sdk as bx
from bma_executor_sdk import BashInput, BashResult, ToolContext, methods
from bma_executor_sdk.generated import ProcessListParams, ProcessListResult
from bma_executor_sdk.tools import INLINE_RESULT_BYTES, REQUIRED_TOOL_INPUT, render

from test_typed_tools import EXECUTOR, pair


async def test_handler_registered_after_start_sends_capabilities_update() -> None:
    holder: list[bx.ExecutorClient] = []
    async with pair(holder.append) as (executor, _):
        assert executor.capabilities["shell"]["enabled"] is False

        @holder[0].tools.bash.handler
        async def bash(input: BashInput, ctx: ToolContext) -> str:  # pyright: ignore[reportUnusedFunction]
            return "x"

        for _ in range(200):
            if executor.capabilities["shell"]["enabled"]:
                break
            await asyncio.sleep(0.01)
        assert executor.capabilities["shell"]["enabled"] is True


async def test_missing_required_input_fails_before_the_handler() -> None:
    called: list[bool] = []

    def register(client: bx.ExecutorClient) -> None:
        @client.tools.bash.handler
        async def bash(input: BashInput, ctx: ToolContext) -> str:  # pyright: ignore[reportUnusedFunction]
            called.append(True)
            return "ran"

    async with pair(register) as (_, session):
        result = await session.call_tool("bash", {"cwd": "x"})  # type: ignore[arg-type,typeddict-item]
        assert (result["outcome"], result["content"][0].get("text"), called) == ("failed", "bash requires command", [])


def test_large_bash_output_is_truncated_like_go_core() -> None:
    big = render(BashResult(stdout="o" * 100_000, stderr="e" * 30_000, exit_code=1))
    texts = [b["text"] for b in big["content"]]
    assert [len(t) for t in texts[:2]] == [INLINE_RESULT_BYTES // 2, INLINE_RESULT_BYTES // 4]
    assert texts[2] == "exit_code: 1 (output truncated to fit the result; 130000 bytes total)"
    marked = render(BashResult(stdout="head", stderr="", exit_code=0, truncated=True))
    assert marked["content"][-1]["text"] == "exit_code: 0 (output truncated to fit the result; 4 bytes total)"


async def test_typed_on_request_call_and_notify() -> None:
    statuses: list[Any] = []
    harness = bx.Harness(request_timeout=2.0, on_session_status=lambda _e, p: statuses.append(p))
    client = bx.ExecutorClient(
        environment_id="env_test", executor=EXECUTOR,  # type: ignore[arg-type]
        token_source=bx.static_token("inprocess://harness"), dialer=harness.inprocess_dialer(),
    )

    async def process_list(params: ProcessListParams) -> ProcessListResult:
        return {"processes": [{"process_id": "p_" + params["session_id"]}]}  # type: ignore[typeddict-item]

    client.on_request(methods.PROCESS_LIST, process_list)
    await client.start()
    try:
        executor = await harness.wait_executor()
        listed = await executor._call("process.list", {"session_id": "s1"})  # pyright: ignore[reportPrivateUsage]
        assert listed["processes"][0]["process_id"] == "p_s1"
        await client.notify(methods.SESSION_STATUS, {"session_id": "s1", "status": "running"})
        for _ in range(100):
            if statuses:
                break
            await asyncio.sleep(0.01)
        assert statuses and statuses[0]["session_id"] == "s1"
    finally:
        await client.stop()


def test_required_input_table_matches_the_contract() -> None:
    schema = json.loads((Path(__file__).parents[2] / "contracts/bma/executor/schemas/tool.json").read_text())
    defs = {"bash": "BashInput", "read": "ReadInput", "write": "WriteInput", "edit": "EditInput",
            "glob": "GlobInput", "grep": "GrepInput", "list_files": "ListFilesInput"}
    for tool, name in defs.items():
        assert REQUIRED_TOOL_INPUT[tool] == tuple(schema["$defs"][name].get("required", [])), tool


def test_weak_rpc_entry_points_are_gone() -> None:
    assert not hasattr(bx.ExecutorClient, "on_notification")
