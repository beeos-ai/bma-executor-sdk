"""Named pipe (npipe://) transport: addresses, a frame near the size limit and
the platform guard. The end-to-end scenarios run in test_executor_runtime.py
on a real pipe on Windows and on a Unix socket stand-in elsewhere."""

from __future__ import annotations

import asyncio
import os
import sys

import pytest

import bma_executor_sdk as bx
from bma_executor_sdk import _npipe
from executor_helpers import fake_named_pipes
from test_executor_runtime import listen, make_client


def test_pipe_addresses() -> None:
    for address in ("bma-x", "\\\\.\\pipe\\bma-x", "npipe:////./pipe/bma-x", "npipe:////./pipe/bma-x?channel=mcp"):
        assert _npipe.pipe_path(address) == "\\\\.\\pipe\\bma-x"
    assert _npipe.pipe_url("\\\\.\\pipe\\bma-x") == "npipe:////./pipe/bma-x"
    for bad in ("", "\\\\.\\pipe\\", "a\\b", "npipe:////./pipe/a/b", "npipe:///other"):
        with pytest.raises(ValueError):
            _npipe.pipe_path(bad)
    assert bx.transport.channel_url("npipe:////./pipe/bma-x", channel="mcp") == "npipe:////./pipe/bma-x?channel=mcp"


@pytest.mark.skipif(sys.platform == "win32", reason="Windows has the pipe loop methods")
async def test_named_pipes_need_the_windows_event_loop() -> None:
    with pytest.raises(NotImplementedError, match="ProactorEventLoop"):
        await bx.transport.dial("npipe:////./pipe/bma-x", "", [bx.PROTOCOL_V11], max_size=bx.transport.MAX_FRAME_BYTES)
    with pytest.raises(NotImplementedError, match="ProactorEventLoop"):
        bx.Harness().serve_npipe("bma-x")


async def test_a_large_frame_and_a_closing_harness_over_a_named_pipe() -> None:
    harness = bx.Harness(request_timeout=5.0)
    async with listen(harness, "npipe") as (url, _):
        client = make_client(url, None, request_timeout=5.0)

        async def on_tool(call: dict[str, object]) -> dict[str, object]:
            return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"], "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": "x" * 700_000}]}

        client.on_tool_call(on_tool)  # type: ignore[arg-type]
        await client.start()
        executor = await harness.wait_executor()
        session = await executor.assign_session({"session_id": "ses_big"})  # type: ignore[typeddict-item]
        result = await session.call_tool({"tool_call_id": "call_big", "tool": "bash", "input": {"command": "big"}})  # type: ignore[typeddict-item]
        assert len(result["content"][0]["text"]) == 700_000
        await client.stop()
        assert client.state == bx.generated.ConnectionState.Closed
