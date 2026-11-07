"""Session context over the machine token (GET /api/v1/daemon/processes/{id}/session
and /api/v1/daemon/sessions/{id}), cached per session; RPCError.with_reason and
ErrorReason match Go; mirrors typescript/test/session-context.test.ts."""

from __future__ import annotations

import re
from typing import Any
from urllib.parse import unquote

import httpx
import pytest

import bma_executor_sdk as bx
from bma_executor_sdk import methods
from bma_executor_sdk.generated import JsonRpcErrorCode, ProcessStartParams, ProcessState

EXECUTOR = {"name": "py-ctx", "os": "linux", "arch": "arm64", "version": "0.1.0"}
SESSION_A = {"id": "sesn_a", "environment_id": "mch_1", "metadata": {"owner": "u_1", "app_ref": "ws_7"},
             "created_at": "2026-11-07T00:00:00Z"}


class FakeDaemon:
    def __init__(self, processes: dict[str, str], sessions: dict[str, Any]) -> None:
        self.processes, self.sessions, self.calls = processes, sessions, []

    def handler(self, request: httpx.Request) -> httpx.Response:
        self.calls.append(request.url.path)
        if request.headers.get("authorization") != "Bearer mtok":
            return httpx.Response(401)
        session_id = None
        if m := re.fullmatch(r"/api/v1/daemon/processes/([^/]+)/session", request.url.path):
            session_id = self.processes.get(unquote(m.group(1)))
        elif m := re.fullmatch(r"/api/v1/daemon/sessions/([^/]+)", request.url.path):
            session_id = unquote(m.group(1))
        session = self.sessions.get(session_id or "")
        if session is None:
            return httpx.Response(404, json={"code": "not_found", "message": "session not bound to this machine"})
        return httpx.Response(200, json=session)


def client(daemon: FakeDaemon | None = None, handler: Any = None, **overrides: Any) -> bx.ExecutorClient:
    transport = httpx.MockTransport(handler or daemon.handler)  # type: ignore[union-attr]
    options: dict[str, Any] = {"environment_id": "", "environment_key": "mtok", "base_url": "https://bma.test/api/v1",
                               "executor": EXECUTOR, "http_client": httpx.AsyncClient(transport=transport)}
    options.update(overrides)
    return bx.ExecutorClient(**options)


async def test_by_process_and_by_id_are_cached_per_session() -> None:
    daemon = FakeDaemon({"proc_1": "sesn_a", "proc_2": "sesn_a"}, {"sesn_a": SESSION_A})
    executor = client(daemon)
    got = await executor.session_context("proc_1")
    assert got == SESSION_A
    await executor.session_context("proc_1")
    await executor.session_context_by_id("sesn_a")
    assert daemon.calls == ["/api/v1/daemon/processes/proc_1/session"]
    await executor.session_context("proc_2")
    assert len(daemon.calls) == 2
    got["metadata"]["owner"] = "u_2"
    assert (await executor.session_context_by_id("sesn_a"))["metadata"]["owner"] == "u_1"


async def test_not_found_is_not_cached() -> None:
    daemon = FakeDaemon({}, {})
    executor = client(daemon)
    with pytest.raises(bx.SessionContextNotFoundError):
        await executor.session_context("proc_other")
    with pytest.raises(bx.SessionContextNotFoundError):
        await executor.session_context_by_id("sesn_b")
    daemon.sessions["sesn_b"] = {**SESSION_A, "id": "sesn_b"}
    assert (await executor.session_context_by_id("sesn_b"))["id"] == "sesn_b"


async def test_needs_a_machine_token_and_reports_server_errors() -> None:
    daemon = FakeDaemon({}, {})
    with pytest.raises(bx.MachineTokenRequiredError):
        await client(daemon, environment_id="env_1").session_context_by_id("sesn_a")
    failing = client(handler=lambda _r: httpx.Response(502, text="upstream down"))
    with pytest.raises(bx.DaemonHTTPError) as raised:
        await failing.session_context_by_id("sesn_a")
    assert raised.value.status == 502
    with pytest.raises(ValueError, match="invalid session id"):
        await client(daemon).session_context_by_id("../processes/x")
    assert daemon.calls == []


def test_error_reason_and_with_reason_match_go() -> None:
    assert {r.name: r.value for r in bx.ErrorReason} == {
        "WorkspaceUnavailable": "workspace_unavailable",
        "WorkspaceBoundary": "workspace_boundary",
        "WorkspaceReadOnly": "workspace_read_only",
        "SessionRejected": "session_rejected",
        "LocalConfirmationDenied": "local_confirmation_denied",
        "LocalConfirmationExpired": "local_confirmation_expired",
        "SpawnFailed": "spawn_failed",
        "ProcessIdSpecMismatch": "process_id_spec_mismatch",
    }
    error = bx.RPCError.with_reason(JsonRpcErrorCode.Forbidden, bx.ErrorReason.SessionRejected, "not this machine")
    assert error.to_wire() == {"code": -32001, "message": "not this machine", "data": {"reason": "session_rejected"}}
    assert type(error.to_wire()["data"]["reason"]) is str


async def test_process_start_handler_reads_context_and_refuses_with_reason() -> None:
    daemon = FakeDaemon({"proc_mine": "sesn_a", "proc_theirs": "sesn_b"},
                        {"sesn_a": SESSION_A, "sesn_b": {**SESSION_A, "id": "sesn_b", "metadata": {"owner": "u_9"}}})
    harness = bx.Harness(request_timeout=2.0)
    root = "/Users/me/Library/Application Support/example/sessions"
    executor = client(daemon, token_source=bx.static_token("inprocess://harness"), dialer=harness.inprocess_dialer(),
                      capabilities={"session_files": {"root": root}})

    async def start(params: ProcessStartParams) -> ProcessState:
        context = await executor.session_context(params["process_id"])
        if context["metadata"].get("owner") != "u_1":
            raise bx.RPCError.with_reason(JsonRpcErrorCode.Forbidden, bx.ErrorReason.SessionRejected, "not this machine")
        assert params.get("metadata", {}).get(bx.METADATA_AGENT_SESSION_ID) == context["id"]
        return {"accepted": True, "process_id": params["process_id"], "state": "running",  # type: ignore[typeddict-item]
                "cursors": {"stdout": 0, "stderr": 0}, "exit_code": None, "exit_signal": None}

    executor.on_request(methods.PROCESS_START, start)
    await executor.start()
    try:
        remote = await harness.wait_executor()
        assert remote.capabilities["session_files"]["root"] == root
        call = remote._call  # pyright: ignore[reportPrivateUsage]
        started = await call("process.start", {"session_id": "s1", "process_id": "proc_mine", "argv": ["true"],
                                               "metadata": {"agent_session_id": "sesn_a"}})
        assert started["process_id"] == "proc_mine"
        with pytest.raises(bx.RPCError) as raised:
            await call("process.start", {"session_id": "s1", "process_id": "proc_theirs", "argv": ["true"],
                                         "metadata": {"agent_session_id": "sesn_b"}})
        assert raised.value.code == JsonRpcErrorCode.Forbidden and (raised.value.data or {}).get("reason") == "session_rejected"
    finally:
        await executor.stop()
