"""Python harness peer for test/interop-python.test.ts: serves WebSocket over a
Unix domain socket (Harness.serve_unix), drives one TypeScript executor through
a session (idempotent tool.call, MCP echo, session.stop), prints PASS."""

import asyncio
import sys

import bma_executor_sdk as bx


async def main(path: str, token: str) -> None:
    def authenticate(request: bx.ConnectRequest) -> bx.Principal:
        if request.bearer_token != token:
            raise bx.Unauthorized()
        return bx.Principal()

    harness = bx.Harness(authenticate=authenticate, request_timeout=5)
    server = await harness.serve_unix(path)
    print(f"LISTEN unix://{path}", flush=True)
    executor = await asyncio.wait_for(harness.wait_executor(), 10)
    assert executor.protocol == bx.PROTOCOL_V11, executor.protocol
    session = await executor.assign_session({"session_id": "sess_py"})  # type: ignore[typeddict-item]
    await asyncio.wait_for(session.ready(), 5)
    for _ in range(2):
        result = await session.call_tool({"tool_call_id": "call_1", "tool": "bash", "input": {"command": "hello"}})  # type: ignore[typeddict-item]
        assert result["content"][0]["text"] == "ts:hello", result
    channel = await asyncio.wait_for(session.mcp("echo"), 5)
    await channel.write('{"jsonrpc":"2.0","id":1,"method":"ping"}')
    reply = await asyncio.wait_for(channel.read(), 5)
    assert reply == '{"jsonrpc":"2.0","id":1,"method":"pong"}', reply
    await channel.close()
    await session.stop("done")
    print("PASS", flush=True)
    server.close()
    await server.wait_closed()


asyncio.run(main(sys.argv[1], sys.argv[2]))
