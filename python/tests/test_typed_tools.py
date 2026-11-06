"""Typed tool handlers (client.tools.<name>.handler) and typed harness calls
(session.tools.<name>.call), ADR 0012 §4; mirrors
typescript/test/typed-tools.test.ts."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Callable
from contextlib import asynccontextmanager
from typing import Any

import pytest

import bma_executor_sdk as bx
from bma_executor_sdk import BashInput, BashResult, ReadInput, ToolContext, ToolError
from bma_executor_sdk.generated import JsonRpcErrorCode, ToolName

EXECUTOR = {"name": "py-typed", "os": "linux", "arch": "arm64", "version": "0.1.0"}


@asynccontextmanager
async def pair(register: Callable[[bx.ExecutorClient], None], capabilities: dict[str, Any] | None = None) -> AsyncIterator[tuple[bx.Executor, bx.Session]]:
    harness = bx.Harness(request_timeout=2.0)
    client = bx.ExecutorClient(
        environment_id="env_test", executor=EXECUTOR, capabilities=capabilities or {},  # type: ignore[arg-type]
        token_source=bx.static_token("inprocess://harness"), dialer=harness.inprocess_dialer(),
    )

    async def assign(params: Any) -> Any:
        async def ready() -> None:
            await client.session_ready({"session_id": params["session_id"], "work_id": params["work_id"], "lease_token": params["lease"]["token"]})

        asyncio.get_running_loop().call_soon(lambda: asyncio.ensure_future(ready()))
        return {"accepted": True, "session_id": params["session_id"]}

    client.on_session_assign(assign)
    register(client)
    await client.start()
    executor = await harness.wait_executor()
    session = await executor.assign_session({"session_id": "sess_typed"})  # type: ignore[typeddict-item]
    await asyncio.wait_for(session.ready(), 5)
    try:
        yield executor, session
    finally:
        await client.stop()


def test_every_built_in_tool_but_computer_has_a_slot() -> None:
    client = bx.ExecutorClient(environment_id="e", executor=EXECUTOR, environment_key="k")  # type: ignore[arg-type]
    slots = {t.value for t in ToolName} - {"computer_observe", "computer_action"}
    assert {getattr(client.tools, name).name for name in slots} == slots
    assert not hasattr(client.tools, "computer_action") and not hasattr(client.tools, "computer_observe")


async def test_bash_result_renders_like_go_core() -> None:
    seen: list[BashInput] = []

    def register(client: bx.ExecutorClient) -> None:
        @client.tools.bash.handler
        async def bash(input: BashInput, ctx: ToolContext) -> BashResult:
            seen.append(input)
            assert ctx.session_id == "sess_typed"
            return BashResult(stdout="out\n", stderr="warn\n", exit_code=3)

    async with pair(register) as (executor, session):
        assert executor.capabilities["shell"] == {"enabled": True}
        result = await session.tools.bash.call({"command": "false", "cwd": "sub"})
        assert seen == [{"command": "false", "cwd": "sub"}]
        assert result["outcome"] == "succeeded" and result["is_error"] is False
        assert result["content"] == [
            {"type": "text", "text": "out\n"},
            {"type": "text", "text": "warn\n"},
            {"type": "text", "text": "exit_code: 3"},
        ]
        again = await session.call_tool("bash", {"command": "true"}, tool_call_id="call_quiet")
        assert again["outcome"] == "succeeded"


async def test_string_blocks_tool_error_and_exceptions() -> None:
    def register(client: bx.ExecutorClient) -> None:
        @client.tools.read.handler
        async def read(input: ReadInput, ctx: ToolContext) -> bx.ToolOutput:
            path = input["path"]
            if path == "missing":
                raise ToolError(f"no such file: {path}")
            if path == "boom":
                raise RuntimeError("disk on fire")
            if path == "img":
                return [{"type": "image", "data": "AAAA", "mime_type": "image/png"}]  # type: ignore[list-item]
            return f"contents of {path} from {input.get('offset', 0)}"

    async with pair(register) as (_, session):
        ok = await session.tools.read.call({"path": "a.txt", "offset": 2})
        assert (ok["outcome"], ok["is_error"], ok["content"]) == ("succeeded", False, [{"type": "text", "text": "contents of a.txt from 2"}])
        assert "duration_ms" in ok

        img = await session.tools.read.call({"path": "img"})
        assert img["content"][0]["type"] == "image"

        missing = await session.tools.read.call({"path": "missing"})
        assert (missing["outcome"], missing["is_error"], missing["content"][0].get("text")) == ("succeeded", True, "no such file: missing")

        boom = await session.tools.read.call({"path": "boom"})
        assert (boom["outcome"], boom["is_error"], boom["content"][0].get("text")) == ("failed", True, "disk on fire")

        # A tool without a handler is refused before the ack, as Go Core refuses it.
        with pytest.raises(bx.RPCError) as excinfo:
            await session.tools.write.call({"path": "x", "content": "y"})
        assert excinfo.value.code == JsonRpcErrorCode.CapabilityUnavailable
        assert excinfo.value.data == {"capability": "tool:write"}


async def test_tool_result_with_is_error_and_artifacts() -> None:
    def register(client: bx.ExecutorClient) -> None:
        @client.tools.grep.handler
        async def grep(input: Any, ctx: ToolContext) -> bx.ToolOutput:
            return bx.ToolResult(content=[{"type": "text", "text": "partial"}], is_error=True)  # type: ignore[list-item]

    async with pair(register) as (_, session):
        result = await session.tools.grep.call({"pattern": "x"})
        assert (result["outcome"], result["is_error"]) == ("succeeded", True)


async def test_capabilities_advertise_only_registered_tools() -> None:
    def register(client: bx.ExecutorClient) -> None:
        @client.tools.read.handler
        async def read(input: ReadInput, ctx: ToolContext) -> str:
            return "x"

        @client.tools.bash.handler
        async def bash(input: BashInput, ctx: ToolContext) -> str:
            return "x"

    # An explicit enabled: False wins over a registered handler.
    async with pair(register, {"shell": {"enabled": False}, "files": {"read_roots": ["/w"]}}) as (executor, _):
        assert executor.capabilities["shell"] == {"enabled": False}
        assert executor.capabilities["files"] == {"enabled": True, "read_roots": ["/w"]}
        assert executor.capabilities["computer_use"] is False


def test_on_tool_call_is_gone() -> None:
    assert not hasattr(bx.ExecutorClient, "on_tool_call")
    with pytest.raises(AttributeError):
        getattr(bx.ExecutorClient(environment_id="e", executor=EXECUTOR, environment_key="k").tools, "nope")  # type: ignore[arg-type]
