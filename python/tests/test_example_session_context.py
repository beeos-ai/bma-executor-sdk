"""examples/session_context.py admit(): which process.start reaches the session
path. Mirrors examples/sandboxed-folder (Go) TestAdmit*."""

from __future__ import annotations

import importlib.util
from pathlib import Path
from typing import Any

import pytest

from bma_executor_sdk import ErrorReason, RPCError

_PATH = Path(__file__).resolve().parent.parent / "examples" / "session_context.py"
_SPEC = importlib.util.spec_from_file_location("session_context_example", _PATH)
assert _SPEC and _SPEC.loader
example = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(example)


def reason(error: pytest.ExceptionInfo[RPCError]) -> str | None:
    return (error.value.data or {}).get("reason")


def test_a_process_without_a_session_is_refused() -> None:
    for params in ({}, {"metadata": {}}, {"metadata": {"initiator": "other"}}, {"metadata": {"initiator": "platform"}}):
        with pytest.raises(RPCError) as raised:
            example.admit(params)
        assert reason(raised) == ErrorReason.SessionRejected
    assert example.admit({"metadata": {"agent_session_id": "sesn_1"}}) is False


def test_platform_processes_are_hosted_only_with_a_platform_dir(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(example, "PLATFORM_DIR", str(tmp_path))
    params: dict[str, Any] = {"metadata": {"initiator": "platform"}}
    assert example.admit(params) is True
    assert example.platform_cwd({"cwd": None}) == tmp_path.resolve()
    for cwd in ("..", "/etc"):
        with pytest.raises(RPCError) as raised:
            example.platform_cwd({"cwd": cwd})
        assert reason(raised) == ErrorReason.WorkspaceBoundary
    with pytest.raises(RPCError) as raised:
        example.admit({**params, "package_install": {"manager": "apt", "packages": ["jq"]}})
    assert reason(raised) == ErrorReason.LocalConfirmationDenied


def test_package_installs_are_refused() -> None:
    with pytest.raises(RPCError) as raised:
        example.admit({"metadata": {"agent_session_id": "sesn_1"}, "package_install": {"manager": "apt", "packages": ["jq"]}})
    assert reason(raised) == ErrorReason.LocalConfirmationDenied
