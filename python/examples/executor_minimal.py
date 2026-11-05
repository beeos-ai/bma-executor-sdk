"""Minimal executor: registers with BMA and answers bash tool calls.

    BMA_ENVIRONMENT_KEY=... BMA_ENVIRONMENT_ID=... python executor_minimal.py

Set BMA_HARNESS_URL (ws://..., unix:///path.sock) to connect to a local
harness instead of BMA, for example the one harness_loopback.py starts.
"""

from __future__ import annotations

import asyncio
import os
import platform

from bma_executor_sdk import BashInput, BashResult, ExecutorClient, ToolContext, static_token


async def run_bash(input: BashInput, ctx: ToolContext) -> BashResult:
    proc = await asyncio.create_subprocess_shell(
        input["command"], cwd=input.get("cwd"), stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    try:
        stdout, stderr = await proc.communicate()
    except asyncio.CancelledError:  # tool.cancel or the deadline: stop the process
        proc.kill()
        raise
    return BashResult(stdout=stdout.decode(errors="replace"), stderr=stderr.decode(errors="replace"), exit_code=proc.returncode or 0)


def make_client() -> ExecutorClient:
    info = {"name": "py-executor", "os": platform.system().lower(), "arch": platform.machine(), "version": "0.1.0"}
    harness_url = os.environ.get("BMA_HARNESS_URL")
    if harness_url:
        client = ExecutorClient(environment_id="env_local", executor=info, token_source=static_token(harness_url))  # type: ignore[arg-type]
    else:
        client = ExecutorClient(
            environment_id=os.environ["BMA_ENVIRONMENT_ID"],
            environment_key=os.environ["BMA_ENVIRONMENT_KEY"],
            base_url=os.environ.get("BMA_BASE_URL", "https://api.bma.beeos.ai"),
            executor=info,  # type: ignore[arg-type]
        )
    client.tools.bash.handler(run_bash)  # advertises the shell capability
    return client


async def main() -> None:
    client = make_client()
    registered = await client.start()
    print("registered as", registered["executor_id"])
    try:
        await asyncio.Event().wait()
    finally:
        await client.stop("shutdown")


if __name__ == "__main__":
    asyncio.run(main())
