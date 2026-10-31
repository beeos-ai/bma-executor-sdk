"""Python executor peer for test/interop-python.test.ts: dials a TypeScript
harness at a unix:// URL, answers bash with "py:<command>", echoes one MCP
frame (ping -> pong) and exits after session.stop, printing PASS."""

import asyncio
import sys
from typing import Any

import bma_executor_sdk as bx


async def main(url: str, token: str) -> None:
    client = bx.ExecutorClient(
        environment_id="env_interop",
        executor={"name": "py-interop", "os": "linux", "arch": "arm64", "version": "0.0.0"},  # type: ignore[typeddict-item]
        token_source=bx.static_token(url, token),
        request_timeout=5,
    )
    stopped = asyncio.Event()

    async def echo(session_id: str) -> None:
        channel = await client.open_mcp_channel(session_id, "echo")
        frame = await channel.read()
        await channel.write(frame.replace('"ping"', '"pong"'))

    async def on_assign(params: dict[str, Any]) -> dict[str, Any]:
        client._spawn(client.session_ready({"session_id": params["session_id"], "work_id": params["work_id"], "lease_token": params["lease"]["token"]}))  # noqa: SLF001
        client._spawn(echo(params["session_id"]))  # noqa: SLF001
        return {"accepted": True, "session_id": params["session_id"]}

    async def on_tool(call: dict[str, Any]) -> dict[str, Any]:
        text = "py:" + call["input"]["command"]
        return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"], "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": text}]}

    async def on_stop(params: dict[str, Any]) -> dict[str, Any]:
        stopped.set()
        return {"stopped": True}

    client.on_session_assign(on_assign)  # type: ignore[arg-type]
    client.on_tool_call(on_tool)  # type: ignore[arg-type]
    client.on_session_stop(on_stop)  # type: ignore[arg-type]
    registered = await client.start()
    assert registered["protocol"] == bx.PROTOCOL_V11, registered
    await asyncio.wait_for(stopped.wait(), 20)
    await asyncio.sleep(0.05)
    await client.stop("done")
    print("PASS", flush=True)


asyncio.run(main(sys.argv[1], sys.argv[2]))
