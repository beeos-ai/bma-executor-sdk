"""Cross-language interop: Python executor <-> Go harness and Python harness
<-> Go executor, built from executor/internal/interop. Skipped without Go."""

from __future__ import annotations

import asyncio
import os
import shutil
import subprocess
import tempfile
from pathlib import Path
from typing import Any

import pytest

import bma_executor_sdk as bx
from executor_helpers import RecordingPlatform

GO_DIR = Path(__file__).resolve().parents[2]

pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="Go toolchain not installed")


@pytest.fixture(scope="module")
def go_bin(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("interop")
    subprocess.run(["go", "build", "-o", str(out) + os.sep, "./internal/interop/..."], cwd=GO_DIR, check=True)
    return out


async def _read_line(stream: asyncio.StreamReader, prefix: str) -> str:
    while True:
        line = (await asyncio.wait_for(stream.readline(), 30)).decode().strip()
        if not line:
            raise AssertionError(f"peer exited before {prefix}")
        if line.startswith(prefix):
            return line[len(prefix):].strip()


@pytest.mark.parametrize("transport", ["ws", "unix"])
async def test_python_executor_with_go_harness(go_bin: Path, transport: str) -> None:
    args = [str(go_bin / "goharness")]
    directory = tempfile.mkdtemp(prefix="bmaexec")
    if transport == "unix":
        args += ["-unix", os.path.join(directory, "h.sock")]
    proc = await asyncio.create_subprocess_exec(*args, stdout=asyncio.subprocess.PIPE)
    assert proc.stdout is not None
    try:
        url = await _read_line(proc.stdout, "LISTEN")
        client = bx.ExecutorClient(environment_id="env_interop", executor={"name": "py-interop", "os": "linux", "arch": "arm64", "version": "0.0.0"}, token_source=bx.static_token(url), request_timeout=5)  # type: ignore[typeddict-item]

        async def on_assign(params: dict[str, Any]) -> dict[str, Any]:
            client._spawn(client.session_ready({"session_id": params["session_id"], "work_id": params["work_id"], "lease_token": params["lease"]["token"]}))  # noqa: SLF001
            return {"accepted": True, "session_id": params["session_id"]}

        async def on_tool(call: dict[str, Any]) -> dict[str, Any]:
            if call["input"].get("command") == "sleep":
                await asyncio.sleep(60)
            return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"], "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": "py:" + call["input"]["command"]}]}

        class Agent:
            async def open_turn(self, params: dict[str, Any]) -> dict[str, Any]:
                return {"accepted": True, "turn_id": params["turn_id"], "executor_session_ref": "py", "resumed": False, "effective": {"model": "py-model"}}

            async def run_turn(self, turn: bx.AgentTurn) -> None:
                turn.emit({"type": "agent.message", "content": [{"type": "text", "text": "py says hi"}]})

            async def close_session(self, session_id: str, reason: str) -> None:
                return None

        client.on_session_assign(on_assign)  # type: ignore[arg-type]
        client.on_tool_call(on_tool)  # type: ignore[arg-type]
        client.serve_agent(Agent(), {"models": [{"id": "py-model"}]})  # type: ignore[arg-type,typeddict-item]
        await client.start()
        verdict = await _read_line(proc.stdout, "")
        assert verdict == "PASS", verdict
        await client.stop()
        assert await asyncio.wait_for(proc.wait(), 10) == 0
    finally:
        if proc.returncode is None:
            proc.kill()
            await proc.wait()
        shutil.rmtree(directory, ignore_errors=True)


async def test_python_harness_with_go_executor(go_bin: Path) -> None:
    platform = RecordingPlatform()
    harness = bx.Harness(agent=platform, request_timeout=5)
    server = await harness.serve("127.0.0.1", 0)
    port = server.sockets[0].getsockname()[1]
    proc = await asyncio.create_subprocess_exec(
        str(go_bin / "goexecutor"), "-url", f"ws://127.0.0.1:{port}/v1/executors/channel",
        stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
    )
    assert proc.stdout is not None and proc.stdin is not None
    try:
        await _read_line(proc.stdout, "REGISTERED")
        executor = await asyncio.wait_for(harness.wait_executor(), 10)
        assert executor.info["name"] == "go-interop"
        assert executor.protocol == bx.PROTOCOL_V11

        session = await executor.assign_session({"session_id": "ses_py"})  # type: ignore[typeddict-item]
        await asyncio.wait_for(session.ready(), 5)
        result = await session.call_tool({"tool_call_id": "c1", "tool": "bash", "input": {"command": "uname"}})  # type: ignore[typeddict-item]
        assert result["outcome"] == "succeeded" and result["content"][0]["text"] == "go:uname"

        slow = asyncio.ensure_future(session.call_tool({"tool_call_id": "c2", "tool": "bash", "input": {"command": "sleep"}}))  # type: ignore[typeddict-item]
        await asyncio.sleep(0.2)
        assert await session.cancel_tool("c2", "interop") is True
        assert (await asyncio.wait_for(slow, 5))["outcome"] == "canceled"
        await session.stop("done")

        turn = await executor.start_turn({"session_id": "ses_agent", "turn_id": "t1", "input_id": "i1", "input": [{"type": "text", "text": "hello"}]})  # type: ignore[typeddict-item]
        assert turn["accepted"] and turn["effective"]["model"] == "go-model"
        await platform.wait_for(lambda: "session.status_idle" in platform.types("ses_agent"))
        assert platform.types("ses_agent") == ["session.status_running", "agent.message", "session.status_idle"]
        assert platform.events[1].event["content"][0]["text"] == "go says: hello"
        assert (await executor.close_agent_session({"session_id": "ses_agent", "reason": "done"}))["closed"] is True  # type: ignore[typeddict-item]
    finally:
        proc.stdin.close()
        try:
            await asyncio.wait_for(proc.wait(), 10)
        except asyncio.TimeoutError:
            proc.kill()
            await proc.wait()
        server.close()
        await server.wait_closed()
    assert proc.returncode == 0
