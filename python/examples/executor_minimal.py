"""Minimal executor: registers with BMA and answers bash tool calls.

    BMA_ENVIRONMENT_KEY=... BMA_ENVIRONMENT_ID=... python executor_minimal.py

Set BMA_HARNESS_URL (ws://..., unix:///path.sock) to connect to a local
harness instead of BMA, for example the one harness_loopback.py starts.
"""

from __future__ import annotations

import asyncio
import os
import platform

from bma_sdk.executor import ExecutorClient, static_token
from bma_sdk.executor.generated import ToolCallParams, ToolResultParams


async def run_bash(call: ToolCallParams) -> ToolResultParams:
    proc = await asyncio.create_subprocess_shell(
        call["input"].get("command", ""), stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.STDOUT,
    )
    try:
        output, _ = await proc.communicate()
    except asyncio.CancelledError:  # tool.cancel or the deadline: stop the process
        proc.kill()
        raise
    return {
        "session_id": call["session_id"],
        "tool_call_id": call["tool_call_id"],
        "outcome": "succeeded" if proc.returncode == 0 else "failed",
        "is_error": proc.returncode != 0,
        "content": [{"type": "text", "text": output.decode(errors="replace")}],
    }  # type: ignore[typeddict-item]


def make_client() -> ExecutorClient:
    info = {"name": "py-executor", "os": platform.system().lower(), "arch": platform.machine(), "version": "0.1.0"}
    capabilities = {"shell": {"enabled": True}}
    harness_url = os.environ.get("BMA_HARNESS_URL")
    if harness_url:
        return ExecutorClient(environment_id="env_local", executor=info, capabilities=capabilities, token_source=static_token(harness_url))  # type: ignore[arg-type]
    return ExecutorClient(
        environment_id=os.environ["BMA_ENVIRONMENT_ID"],
        environment_key=os.environ["BMA_ENVIRONMENT_KEY"],
        base_url=os.environ.get("BMA_BASE_URL", "https://api.bma.beeos.ai"),
        executor=info,  # type: ignore[arg-type]
        capabilities=capabilities,  # type: ignore[arg-type]
    )


async def main() -> None:
    client = make_client()
    client.on_tool_call(run_bash)
    registered = await client.start()
    print("registered as", registered["executor_id"])
    try:
        await asyncio.Event().wait()
    finally:
        await client.stop("shutdown")


if __name__ == "__main__":
    asyncio.run(main())
