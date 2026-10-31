"""Replay contracts/bma/executor/examples/conformance.json against the SDK.

Every example must decode strictly (no unknown or missing members, matching
types) into the generated TypedDicts, and the agent session events must pass
the harness validation, as in go/executor/agent_conformance_test.go.
"""

from __future__ import annotations

import enum
import json
import types
import typing
from pathlib import Path
from typing import Any, Literal, Union

import pytest

import bma_executor_sdk as bx
from bma_executor_sdk import agent as bx_agent
from bma_executor_sdk import generated as g
from executor_helpers import RecordingPlatform

FIXTURES = Path(__file__).resolve().parents[3] / "contracts/bma/executor/examples/conformance.json"


class _NotRequired:
    def __init__(self, inner: Any) -> None:
        self.inner = inner


class _NotRequiredMarker:
    def __getitem__(self, inner: Any) -> _NotRequired:
        return _NotRequired(inner)


def _hints(cls: type) -> dict[str, Any]:
    namespace = dict(vars(g))
    namespace["NotRequired"] = _NotRequiredMarker()
    hints: dict[str, Any] = {}
    for base in reversed(cls.__mro__):
        for name, annotation in getattr(base, "__annotations__", {}).items():
            if isinstance(annotation, typing.ForwardRef):  # Python 3.14 TypedDict
                annotation = annotation.__forward_arg__
            hints[name] = eval(annotation, namespace) if isinstance(annotation, str) else annotation  # noqa: S307
    return hints


def _check(value: Any, annotation: Any, path: str) -> None:
    if annotation is Any:
        return
    if isinstance(annotation, type) and typing.is_typeddict(annotation):
        assert isinstance(value, dict), f"{path}: expected object, got {value!r}"
        hints = _hints(annotation)
        unknown = set(value) - set(hints)
        assert not unknown, f"{path}: unknown members {sorted(unknown)}"
        for name, hint in hints.items():
            optional = isinstance(hint, _NotRequired)
            if name not in value:
                assert optional, f"{path}: missing required member {name}"
                continue
            _check(value[name], hint.inner if optional else hint, f"{path}.{name}")
        return
    if isinstance(annotation, type) and issubclass(annotation, enum.Enum):
        assert value in {member.value for member in annotation}, f"{path}: {value!r} is not a {annotation.__name__}"
        return
    origin = typing.get_origin(annotation)
    if origin is Literal:
        assert value in typing.get_args(annotation), f"{path}: {value!r} not in {typing.get_args(annotation)}"
        return
    if origin in (Union, types.UnionType):
        errors = []
        for variant in typing.get_args(annotation):
            try:
                _check(value, variant, path)
                return
            except AssertionError as error:
                errors.append(str(error))
        raise AssertionError(f"{path}: no variant matched: {errors}")
    if origin is list:
        assert isinstance(value, list), f"{path}: expected array"
        (item,) = typing.get_args(annotation)
        for index, element in enumerate(value):
            _check(element, item, f"{path}[{index}]")
        return
    if origin is dict:
        assert isinstance(value, dict), f"{path}: expected object"
        _, item = typing.get_args(annotation)
        for key, element in value.items():
            _check(element, item, f"{path}.{key}")
        return
    if annotation is type(None):
        assert value is None, f"{path}: expected null"
        return
    if annotation is float:
        assert isinstance(value, (int, float)) and not isinstance(value, bool), f"{path}: expected number"
        return
    if annotation is int:
        assert isinstance(value, int) and not isinstance(value, bool), f"{path}: expected integer"
        return
    assert isinstance(value, annotation), f"{path}: expected {annotation}, got {value!r}"


def _result_type(method: str) -> Any:
    namespace = dict(vars(g))
    result_type = g.METHODS[method].result_type
    assert result_type, f"{method} has no result type"
    return eval(result_type, namespace)  # noqa: S307


FIXTURE = json.loads(FIXTURES.read_text())


def test_fixture_protocol_and_method_catalog() -> None:
    assert FIXTURE["protocol"] == bx.PROTOCOL_V11
    assert all((spec.result_type is not None) == (spec.kind == "request") for spec in g.METHODS.values())
    covered = {entry["method"] for entry in FIXTURE["messages"]}
    assert covered == set(g.METHODS), "every contract method has an example"


@pytest.mark.parametrize("entry", FIXTURE["messages"], ids=[entry["name"] for entry in FIXTURE["messages"]])
def test_example_decodes_into_generated_types(entry: dict[str, Any]) -> None:
    spec = g.METHODS[entry["method"]]
    if entry["kind"] == "result":
        _check(entry["result"], _result_type(entry["method"]), entry["name"])
        return
    assert spec.kind == entry["kind"]
    message = entry["message"]
    assert message["jsonrpc"] == "2.0" and message["method"] == entry["method"]
    assert ("id" in message) == (entry["kind"] == "request")
    params = message["params"]
    if entry["method"] == "session.event" and "event_id" in params:
        # SessionEvent allows additional members; an agent executor's event is
        # one of the AgentEvent shapes (schemas/agent.json).
        _check(params["event"], g.AgentEvent, f"{entry['name']}.event")
        params = dict(params, event={"type": params["event"]["type"]})
    _check(params, getattr(g, spec.params_type), entry["name"])


@pytest.mark.parametrize(
    "entry",
    [e for e in FIXTURE["messages"] if e["method"] == "session.event" and e["kind"] == "request" and "event_id" in e["message"]["params"]],
    ids=lambda e: e["name"],
)
async def test_agent_session_events_pass_harness_validation(entry: dict[str, Any]) -> None:
    platform = RecordingPlatform()
    harness = bx.Harness(agent=platform)
    params = dict(entry["message"]["params"], sequence=1, session_id=entry["name"])
    result = await bx_agent.agent_event(harness, None, params)
    assert result["accepted"] is True and result["acked_sequence"] == 1
    assert platform.events[-1].event_id == params["event_id"]
