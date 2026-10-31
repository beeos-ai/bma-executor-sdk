"""Check the generated executor types against the vendored contract examples."""

from __future__ import annotations

import json
from pathlib import Path

from bma_sdk import executor

EXAMPLES = Path(__file__).resolve().parents[2] / "contracts/bma/executor/examples/conformance.json"


def test_every_example_matches_the_catalog_and_params_type() -> None:
    messages = json.loads(EXAMPLES.read_text())["messages"]
    covered = set()
    for example in messages:
        spec = executor.METHODS[example["method"]]
        covered.add(spec.name)
        if example["kind"] == "result":
            assert spec.kind == "request", example["name"]
            continue
        assert spec.kind == example["kind"], example["name"]
        params_type = getattr(executor, spec.params_type)
        unknown = set(example["message"]["params"]) - set(params_type.__annotations__)
        assert not unknown, f"{example['name']}: {unknown} not in {spec.params_type}"
    assert covered == set(executor.METHODS)


def test_protocols_and_v11_error_codes() -> None:
    assert [p.value for p in executor.ProtocolVersion] == ["bma.executor.v1", "bma.executor.v1.1"]
    assert executor.JsonRpcErrorCode.Value32012 == -32012
    assert executor.METHODS["process.start"].capability == "process.enabled"
    assert executor.METHODS["process.start"].since == "bma.executor.v1.1"
