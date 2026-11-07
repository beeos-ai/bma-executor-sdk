"""A machine-scoped executor whose processes run in a local folder chosen per session.

The caller puts opaque references in the session metadata, for example
{"workspace_ref": "ws_7", "owner": "u_42"}; before spawning, the process.start
handler reads the session context, refuses another owner's session
(session_rejected) or an unknown reference (workspace_unavailable), and runs the
command in this machine's folder for the reference. BMA never stores the path.

A process.start without metadata.agent_session_id has no session to check against:
it is refused with session_rejected, never run in a default folder (the harness
does not send one; a person's console terminal is terminal.attach, not
process.start). The exception is a process the platform starts on its own
(metadata initiator "platform", for example the machine desktop agent): with
EXAMPLE_PLATFORM_DIR set it runs in that directory by this machine's policy,
never in a session folder; without it, it is refused too. A package install runs
a package manager that writes outside the folder: this example has no local
confirmation, so it refuses with local_confirmation_denied.

Python has no built-in process runtime (Go's ProcessOptions.Launch): the executor
serves process.* itself. This example keeps output in memory and serves
process.start and process.read only.

    BMA_BASE_URL=https://api.<env>.bma.beeos.ai BMA_MACHINE_TOKEN=... \\
    EXAMPLE_OWNER=u_42 EXAMPLE_FOLDERS='{"ws_7": "/Users/me/projects/site"}' \\
    EXAMPLE_PLATFORM_DIR=/Users/me/bma-platform \\
    python session_context.py
"""

from __future__ import annotations

import asyncio
import base64
import json
import os
import platform
from pathlib import Path
from typing import Any

from bma_executor_sdk import ErrorReason, ExecutorClient, RPCError, methods
from bma_executor_sdk.generated import JsonRpcErrorCode

OWNER = os.environ.get("EXAMPLE_OWNER")
FOLDERS: dict[str, str] = json.loads(os.environ.get("EXAMPLE_FOLDERS", "{}"))
# Where platform-initiated processes run; empty means this machine hosts none.
PLATFORM_DIR = os.environ.get("EXAMPLE_PLATFORM_DIR", "")
PROCESSES: dict[str, dict[str, Any]] = {}


def refuse(reason: str, message: str) -> RPCError:
    return RPCError.with_reason(JsonRpcErrorCode.Forbidden, reason, message)


def admit(params: dict[str, Any]) -> bool:
    """Refuse what no session or local policy can allow, before reading the session context.

    Returns True for a process the platform started on its own (no session,
    handled by platform_cwd), False for a process of an agent session.
    """
    metadata = params.get("metadata") or {}
    platform = False
    if not metadata.get("agent_session_id"):
        if not PLATFORM_DIR or metadata.get("initiator") != "platform":
            raise refuse(ErrorReason.SessionRejected, "the process names no agent session")
        platform = True
    if params.get("package_install") is not None:
        raise refuse(ErrorReason.LocalConfirmationDenied, "package installs need a local confirmation this executor does not offer")
    return platform


def confine(folder: str, cwd: str | None) -> Path:
    """Resolve cwd (relative to folder) and refuse one outside it."""
    root = Path(folder).resolve()
    resolved = (root / (cwd or ".")).resolve()
    if not resolved.is_relative_to(root):
        raise refuse(ErrorReason.WorkspaceBoundary, "cwd is outside the allowed folder")
    return resolved


def platform_cwd(params: dict[str, Any]) -> Path:
    return confine(PLATFORM_DIR, params.get("cwd"))


def state(proc: dict[str, Any]) -> dict[str, Any]:
    return {
        "accepted": True, "process_id": proc["id"], "state": proc["state"],
        "cursors": {"stdout": len(proc["stdout"]), "stderr": len(proc["stderr"])},
        "exit_code": proc["exit_code"], "exit_signal": None,
    }


async def pump(stream: asyncio.StreamReader | None, sink: bytearray) -> None:
    while stream and (chunk := await stream.read(65536)):
        sink.extend(chunk)


async def run(proc: dict[str, Any], child: asyncio.subprocess.Process) -> None:
    await asyncio.gather(pump(child.stdout, proc["stdout"]), pump(child.stderr, proc["stderr"]))
    proc["exit_code"], proc["state"] = await child.wait(), "exited"


def make_client() -> ExecutorClient:
    client = ExecutorClient(
        # A machine daemon token and no environment id: a machine-scoped executor.
        environment_id="",
        environment_key=os.environ["BMA_MACHINE_TOKEN"],
        base_url=os.environ.get("BMA_BASE_URL", "https://api.bma.beeos.ai"),
        executor={"name": "session-context-example", "os": platform.system().lower(), "arch": platform.machine(), "version": "0.1.0"},
        capabilities={"process": {"enabled": True, "max_processes": 8, "buffer_bytes": 1 << 20}},
    )

    async def process_start(params: Any) -> Any:
        if existing := PROCESSES.get(params["process_id"]):
            return state(existing)  # a replayed start
        if admit(params):
            cwd = platform_cwd(params)  # not a session: its own directory, never a session folder
        else:
            session = await client.session_context(params["process_id"])
            if session["metadata"].get("owner") != OWNER:
                raise refuse(ErrorReason.SessionRejected, "this machine does not serve the session owner")
            folder = FOLDERS.get(session["metadata"].get("workspace_ref", ""))
            if not folder:
                raise refuse(ErrorReason.WorkspaceUnavailable, "unknown workspace on this machine")
            cwd = confine(folder, params.get("cwd"))
        child = await asyncio.create_subprocess_exec(
            *params["argv"], cwd=cwd, env={**os.environ, **(params.get("env") or {})},
            stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE, stdin=asyncio.subprocess.DEVNULL,
        )
        proc: dict[str, Any] = {"id": params["process_id"], "stdout": bytearray(), "stderr": bytearray(), "exit_code": None, "state": "running"}
        PROCESSES[params["process_id"]] = proc
        asyncio.create_task(run(proc, child))
        return state(proc)

    async def process_read(params: Any) -> Any:
        proc = PROCESSES.get(params["process_id"])
        if proc is None:
            raise RPCError(JsonRpcErrorCode.NotFound, "process not found")
        data = bytes(proc[params["stream"]])[params["cursor"]:]
        return {
            "process_id": params["process_id"], "stream": params["stream"], "data": base64.b64encode(data).decode(),
            "from_cursor": params["cursor"], "next_cursor": params["cursor"] + len(data), "earliest_cursor": 0,
            "gap": False, "truncated": False, "eof": proc["state"] != "running", "state": proc["state"],
            "exit_code": proc["exit_code"], "exit_signal": None,
        }

    client.on_request(methods.PROCESS_START, process_start)
    client.on_request(methods.PROCESS_READ, process_read)
    return client


async def main() -> None:
    if PLATFORM_DIR:
        Path(PLATFORM_DIR).mkdir(parents=True, exist_ok=True)
    client = make_client()
    registered = await client.start()
    print("registered as", registered["executor_id"])
    try:
        await client.wait_closed()
    finally:
        await client.stop("shutdown")


if __name__ == "__main__":
    asyncio.run(main())
