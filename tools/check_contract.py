#!/usr/bin/env python3
"""Contract drift check for the BMA SDKs.

Default mode (CI, needs only this repository):

1. lock: contracts/bma/rest.openapi.yaml matches the sha256 in lock.json.
2. facade: the operations in facade-extensions.openapi.yaml are exactly the
   facade routes lock.json marks `bma_extension`.
3. fixtures: every recorded exchange in fixtures/sdk-contract.json names a
   real operation, uses its path and parameters, and every JSON body validates
   against the operation's request/response schema.
4. coverage: every contract operation is present in the generated Go,
   TypeScript and Python clients, and every extension operation has a
   hand-written wrapper in each SDK's extension namespace.

5. executor: the executor contract snapshot (contracts/bma/executor) and the
   Go, TypeScript and Python executor types generated from it by the BMA
   repository (`make contract-sdk`) match the digests in its lock.json, so a
   hand edit or a partial refresh fails.

`--source <beeos-managed-agents checkout> [--ref origin/main]` additionally
compares the snapshots with the upstream OpenAPI file, the executor contract
files and the routes registered in the facade's server.go, so a release can prove the SDK matches the server.

Generated code freshness is checked separately by running tools/generate.sh and
`git diff --exit-code` (see .github/workflows/contract.yaml).

Requires Python 3.10+, PyYAML and jsonschema.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

import yaml
from jsonschema import Draft202012Validator
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

ROOT = Path(__file__).resolve().parent.parent
CONTRACTS = ROOT / "contracts" / "bma"
EXECUTOR = CONTRACTS / "executor"
METHODS = ("get", "post", "put", "patch", "delete")

# Operations a generated client cannot express, with the hand-written or
# out-of-scope reason. Anything not listed here must be generated.
PYTHON_EXEMPT = {
    "recordMachineFailure": "machine-daemon-only text/plain endpoint (x-hidden); not a tenant API",
}


class Report:
    def __init__(self) -> None:
        self.failures: list[str] = []
        self.warnings: list[str] = []

    def fail(self, message: str) -> None:
        self.failures.append(message)

    def section(self, name: str, problems_before: int) -> None:
        status = "ok" if len(self.failures) == problems_before else "FAIL"
        print(f"[{status}] {name}")


def load_yaml(path: Path) -> dict[str, Any]:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def operations(document: dict[str, Any]) -> dict[str, tuple[str, str, dict[str, Any]]]:
    """operationId -> (METHOD, path, operation with path-level parameters merged)."""
    out = {}
    for path, item in document["paths"].items():
        for method in METHODS:
            operation = item.get(method)
            if operation is None:
                continue
            merged = dict(operation)
            merged["parameters"] = list(item.get("parameters", [])) + list(operation.get("parameters", []))
            out[operation["operationId"]] = (method.upper(), path, merged)
    return out


def normalize_legacy_nullable(report: Report, label: str, node: Any, path: str = "") -> None:
    """Turn OpenAPI 3.0 `nullable: true` into a 3.1 null type, with a warning.

    OpenAPI 3.1 ignores `nullable`, so a 3.1 contract that uses it declares a
    non-null field the server actually sends as null. Validation follows the
    evident intent; the warning asks for an upstream fix.
    """
    if isinstance(node, list):
        for index, item in enumerate(node):
            normalize_legacy_nullable(report, label, item, f"{path}/{index}")
        return
    if not isinstance(node, dict):
        return
    if node.get("nullable") is True:
        report.warnings.append(f"{label}: {path.lstrip('/')} uses OpenAPI 3.0 `nullable: true` in a 3.1 document")
        del node["nullable"]
        if isinstance(node.get("type"), str):
            node["type"] = [node["type"], "null"]
    for key, value in node.items():
        normalize_legacy_nullable(report, label, value, f"{path}/{key}")


def resolve(document: dict[str, Any], node: dict[str, Any]) -> dict[str, Any]:
    while isinstance(node, dict) and "$ref" in node:
        target: Any = document
        for part in node["$ref"].removeprefix("#/").split("/"):
            target = target[part]
        node = target
    return node


def check_lock(report: Report, lock: dict[str, Any]) -> None:
    before = len(report.failures)
    actual = hashlib.sha256((ROOT / lock["rest"]["contract"]).read_bytes()).hexdigest()
    if actual != lock["rest"]["sha256"]:
        report.fail(f"lock: {lock['rest']['contract']} sha256 {actual} != lock {lock['rest']['sha256']}")
    report.section(f"lock: REST snapshot of {lock['source_repository']}@{lock['source_commit'][:12]}", before)


def check_facade(report: Report, lock: dict[str, Any], facade: dict[str, Any]) -> None:
    before = len(report.failures)
    declared = {f"{m} {p}" for m, p, _ in operations(facade).values()}
    marked = {r["route"] for r in lock["facade"]["routes"] if r["surface"] == "bma_extension"}
    for route in sorted(marked - declared):
        report.fail(f"facade: extension route {route} is missing from facade-extensions.openapi.yaml")
    for route in sorted(declared - marked):
        report.fail(f"facade: {route} is not a facade route marked bma_extension in lock.json")
    for op_id, (_, _, operation) in operations(facade).items():
        if operation.get("x-bma-extension") is not True:
            report.fail(f"facade: {op_id} must set x-bma-extension: true")
    report.section(f"facade: {len(declared)} BMA extension routes of {len(lock['facade']['routes'])} facade routes", before)


def path_regex(template: str) -> re.Pattern[str]:
    return re.compile("^" + re.sub(r"\\\{[^}]+\\\}", "[^/]+", re.escape(template)) + "$")


def validator(document: dict[str, Any], uri: str, schema: dict[str, Any]) -> Draft202012Validator:
    registry = Registry().with_resource(uri, Resource(contents=document, specification=DRAFT202012))
    if "$ref" in schema:
        schema = {"$ref": uri + schema["$ref"]}
    return Draft202012Validator(schema, registry=registry)


def validate(report: Report, label: str, document: dict[str, Any], uri: str, schema: dict[str, Any], value: Any) -> None:
    for error in validator(document, uri, schema).iter_errors(value):
        where = "/".join(str(p) for p in error.absolute_path) or "<root>"
        report.fail(f"fixtures: {label}: {where}: {error.message[:200]}")


def check_fixtures(report: Report, rest: dict[str, Any], facade: dict[str, Any]) -> None:
    before = len(report.failures)
    fixtures = json.loads((CONTRACTS / "fixtures" / "sdk-contract.json").read_text())
    count = 0
    for group, document, uri in (("rest", rest, "urn:bma:rest"), ("facade", facade, "urn:bma:facade")):
        ops = operations(document)
        for case in fixtures[group]:
            count += 1
            label = f"{group}/{case['name']}"
            if case["operationId"] not in ops:
                report.fail(f"fixtures: {label}: unknown operationId {case['operationId']}")
                continue
            method, template, operation = ops[case["operationId"]]
            request, response = case["request"], case["response"]
            if request["method"] != method or not path_regex(template).match(urlsplit(request["path"]).path):
                report.fail(f"fixtures: {label}: {request['method']} {request['path']} is not {method} {template}")
            params = {(p["in"], p["name"]) for p in (resolve(document, x) for x in operation["parameters"])}
            for name in request.get("query", {}):
                if ("query", name) not in params:
                    report.fail(f"fixtures: {label}: query parameter {name} is not in the contract")
            for name in request.get("headers", {}):
                if name.lower() in ("x-api-key", "authorization"):
                    continue
                if ("header", name) not in params and ("header", name.lower()) not in {(i, n.lower()) for i, n in params}:
                    report.fail(f"fixtures: {label}: header {name} is not in the contract")
            if "json" in request:
                body = resolve(document, operation.get("requestBody", {}))
                schema = body.get("content", {}).get("application/json", {}).get("schema")
                if schema is None:
                    report.fail(f"fixtures: {label}: operation has no JSON request body")
                else:
                    validate(report, label + " request", document, uri, schema, request["json"])
            status = str(response["status"])
            responses = operation["responses"]
            declared = responses.get(status) or responses.get(f"{status[0]}XX") or responses.get("default")
            if declared is None:
                report.fail(f"fixtures: {label}: status {status} is not declared")
                continue
            content = resolve(document, declared).get("content", {})
            if "json" in response:
                schema = content.get("application/json", {}).get("schema")
                if schema is None:
                    report.fail(f"fixtures: {label}: status {status} has no JSON body")
                else:
                    validate(report, f"{label} response", document, uri, schema, response["json"])
            for index, frame in enumerate(response.get("sse", [])):
                if "data" in frame:
                    schema = content.get("text/event-stream", {}).get("schema")
                    validate(report, f"{label} sse[{index}]", document, uri, schema, frame["data"])
    report.section(f"fixtures: {count} recorded exchanges validate against the contracts", before)


def snake(name: str) -> str:
    """openapi-python-client module name: connectBYOMachine -> connect_byo_machine."""
    name = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1_\2", name)
    return re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name).lower()


def check_coverage(report: Report, rest: dict[str, Any], facade: dict[str, Any]) -> None:
    before = len(report.failures)
    rest_ids = set(operations(rest))
    facade_ids = set(operations(facade))

    go_rest = set(re.findall(r"^func \(c \*GeneratedClient\) (\w+)\(", (ROOT / "go/rest/generated.go").read_text(), re.M))
    go_ext = set(re.findall(r"^func \(c \*GeneratedClient\) (\w+)\(", (ROOT / "go/facadeext/generated.go").read_text(), re.M))
    go_wrappers = set(re.findall(r"^func \(c \*Client\) (\w+)\(", (ROOT / "go/facadeext/client.go").read_text(), re.M))
    for ids, generated, label in ((rest_ids, go_rest, "go/rest"), (facade_ids, go_ext, "go/facadeext")):
        for op_id in sorted(ids):
            name = op_id[0].upper() + op_id[1:]
            if name not in generated and name + "WithBody" not in generated:
                report.fail(f"coverage: {label} has no generated method for {op_id}")
    for op_id in sorted(facade_ids):
        if op_id[0].upper() + op_id[1:] not in go_wrappers:
            report.fail(f"coverage: go/facadeext.Client has no wrapper for {op_id}")

    ts_rest = (ROOT / "typescript/src/generated/rest.ts").read_text()
    ts_ext = (ROOT / "typescript/src/generated/facade-extensions.ts").read_text()
    ts_wrappers = (ROOT / "typescript/src/facade-extensions.ts").read_text()
    for ids, text, label in ((rest_ids, ts_rest, "typescript rest"), (facade_ids, ts_ext, "typescript facade-extensions")):
        declared = set(re.findall(r"^    (\w+): \{$", text.split("export interface operations")[1], re.M))
        for op_id in sorted(ids - declared):
            report.fail(f"coverage: {label} has no generated operation {op_id}")
    for op_id in sorted(facade_ids):
        if not re.search(rf"^  async {op_id}\(", ts_wrappers, re.M):
            report.fail(f"coverage: FacadeExtensionsClient (typescript) has no method {op_id}")

    py_rest = {p.stem for p in (ROOT / "python/src/bma_sdk/rest/api").rglob("*.py")}
    py_ext = {p.stem for p in (ROOT / "python/src/bma_sdk/facade_extensions/_generated/api").rglob("*.py")}
    py_wrappers = (ROOT / "python/src/bma_sdk/facade_extensions/__init__.py").read_text()
    for op_id in sorted(rest_ids):
        if snake(op_id) not in py_rest and op_id not in PYTHON_EXEMPT:
            report.fail(f"coverage: python bma_sdk.rest has no generated module for {op_id}")
    for op_id in sorted(set(PYTHON_EXEMPT) - rest_ids):
        report.fail(f"coverage: PYTHON_EXEMPT lists {op_id}, which is not in the contract")
    for op_id in sorted(facade_ids):
        if snake(op_id) not in py_ext:
            report.fail(f"coverage: python facade_extensions has no generated module for {op_id}")
        if not re.search(rf"^    def {snake(op_id)}\(", py_wrappers, re.M):
            report.fail(f"coverage: FacadeExtensionsClient (python) has no method {snake(op_id)}")

    exempt = ", ".join(f"{k} ({v})" for k, v in PYTHON_EXEMPT.items())
    report.section(
        f"coverage: {len(rest_ids)} REST + {len(facade_ids)} extension operations in Go, TypeScript, Python"
        f" (python exempt: {exempt})",
        before,
    )


def check_executor(report: Report, executor_lock: dict[str, Any]) -> None:
    before = len(report.failures)
    for group in ("contract", "generated"):
        for path, digest in executor_lock[group].items():
            target = ROOT / path
            if not target.is_file():
                report.fail(f"executor: {path} is missing")
                continue
            actual = hashlib.sha256(target.read_bytes()).hexdigest()
            if actual != digest:
                report.fail(f"executor: {path} sha256 {actual} != lock {digest}; regenerate with `make contract-sdk` in BMA")
    report.section(
        f"executor: contract {executor_lock['contract_version']} from "
        f"{executor_lock['source_repository']}@{executor_lock['source_commit'][:12]}",
        before,
    )


def check_executor_upstream(report: Report, executor_lock: dict[str, Any], source: Path, commit: str) -> None:
    before = len(report.failures)
    for path in executor_lock["contract"]:
        upstream_path = "contracts/bma/" + path.removeprefix("contracts/bma/executor/")
        upstream = git_show(source, commit, upstream_path)
        if upstream is None:
            report.fail(f"upstream: {upstream_path} is missing")
        elif upstream != (ROOT / path).read_bytes():
            report.fail(f"upstream: {upstream_path} changed; run `make contract-sdk` in BMA")
    report.section(f"upstream: executor contract matches {source.name}@{commit[:12]}", before)


def git_show(source: Path, ref: str, path: str) -> bytes | None:
    result = subprocess.run(["git", "-C", str(source), "show", f"{ref}:{path}"], capture_output=True)
    return result.stdout if result.returncode == 0 else None


def check_upstream(report: Report, lock: dict[str, Any], rest: dict[str, Any], source: Path, ref: str) -> None:
    before = len(report.failures)
    commit = subprocess.run(
        ["git", "-C", str(source), "rev-parse", ref], check=True, capture_output=True, text=True
    ).stdout.strip()
    upstream_raw = git_show(source, commit, lock["rest"]["source_path"])
    server_raw = git_show(source, commit, lock["facade"]["source_path"])
    if upstream_raw is None or server_raw is None:
        report.fail(f"upstream: {lock['rest']['source_path']} or {lock['facade']['source_path']} is missing at {ref}")
        report.section(f"upstream: {source.name}@{commit[:12]} ({ref})", before)
        return
    if upstream_raw != (ROOT / lock["rest"]["contract"]).read_bytes():
        upstream = operations(yaml.safe_load(upstream_raw))
        ours = operations(rest)
        for op_id in sorted(set(upstream) - set(ours)):
            report.fail(f"upstream: {op_id} ({' '.join(upstream[op_id][:2])}) is missing from the SDK contract")
        for op_id in sorted(set(ours) - set(upstream)):
            report.fail(f"upstream: {op_id} was removed upstream")
        if set(upstream) == set(ours):
            report.fail("upstream: same operations but schemas or parameters changed; refresh the snapshot")
    server = server_raw.decode()
    routes = set(re.findall(r'mux\.HandleFunc\("([A-Z]+ /v1/[^"]+)"', server))
    locked = {r["route"] for r in lock["facade"]["routes"]}
    for route in sorted(routes - locked):
        report.fail(f"upstream: facade route {route} is not classified in lock.json")
    for route in sorted(locked - routes):
        report.fail(f"upstream: facade route {route} no longer exists")
    report.section(f"upstream: {source.name}@{commit[:12]} ({ref}) matches the SDK contracts", before)
    check_executor_upstream(report, json.loads((EXECUTOR / "lock.json").read_text()), source, commit)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--source", type=Path, help="beeos-managed-agents checkout to compare against")
    parser.add_argument("--ref", default="origin/main", help="git ref in --source (default origin/main)")
    args = parser.parse_args()

    lock = json.loads((CONTRACTS / "lock.json").read_text())
    rest = load_yaml(CONTRACTS / "rest.openapi.yaml")
    facade = load_yaml(CONTRACTS / "facade-extensions.openapi.yaml")
    report = Report()
    normalize_legacy_nullable(report, "rest.openapi.yaml", rest)
    normalize_legacy_nullable(report, "facade-extensions.openapi.yaml", facade)
    check_lock(report, lock)
    check_facade(report, lock, facade)
    check_fixtures(report, rest, facade)
    check_coverage(report, rest, facade)
    check_executor(report, json.loads((EXECUTOR / "lock.json").read_text()))
    if args.source:
        check_upstream(report, lock, rest, args.source, args.ref)

    for warning in report.warnings:
        print(f"  warning: {warning}")
    for failure in report.failures:
        print(f"  - {failure}", file=sys.stderr)
    if report.failures:
        print(f"contract check failed: {len(report.failures)} problem(s)", file=sys.stderr)
        return 1
    print("contract check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
