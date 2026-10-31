"""Local brain and hand in one process: a harness and an executor connected
in-process (or over a Unix socket with --unix PATH), one bash tool call.

    python harness_loopback.py [--unix /tmp/bma-harness.sock]
"""

from __future__ import annotations

import argparse
import asyncio

from bma_sdk.executor import ExecutorClient, Harness, static_token
from executor_minimal import run_bash


async def main(unix_path: str | None) -> None:
    harness = Harness()  # no authenticate hook: in-process or a private socket only
    server = None
    if unix_path:
        server = await harness.serve_unix(unix_path)
        client = ExecutorClient(environment_id="env_local", executor={"name": "loopback", "os": "local", "arch": "local", "version": "0.1.0"}, token_source=static_token(f"unix://{unix_path}"))  # type: ignore[typeddict-item]
    else:
        client = ExecutorClient(environment_id="env_local", executor={"name": "loopback", "os": "local", "arch": "local", "version": "0.1.0"}, token_source=static_token("inprocess://"), dialer=harness.inprocess_dialer())  # type: ignore[typeddict-item]

    async def on_assign(params):  # type: ignore[no-untyped-def]
        # Report ready once the workspace is prepared (here: nothing to do).
        asyncio.ensure_future(client.session_ready({"session_id": params["session_id"], "work_id": params["work_id"], "lease_token": params["lease"]["token"]}))
        return {"accepted": True, "session_id": params["session_id"]}

    client.on_session_assign(on_assign)
    client.on_tool_call(run_bash)
    await client.start()
    try:
        executor = await harness.wait_executor()
        session = await executor.assign_session({"session_id": "ses_local"})  # type: ignore[typeddict-item]
        await session.ready()
        result = await session.call_tool({"tool": "bash", "input": {"command": "echo hello from the hand"}})  # type: ignore[typeddict-item]
        print(result["outcome"], result["content"][0]["text"].strip())
        await session.stop("done")
    finally:
        await client.stop("done")
        if server is not None:
            server.close()
            await server.wait_closed()  # also removes the socket file


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--unix")
    asyncio.run(main(parser.parse_args().unix))
