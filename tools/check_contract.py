#!/usr/bin/env python3
"""Contract drift check for the BMA executor SDKs.

Default mode (CI, needs only this repository): the executor contract snapshot
(contracts/bma/executor) and the Go, TypeScript and Python executor types
generated from it by the BMA repository (`make contract-sdk`) match the
digests in its lock.json, so a hand edit or a partial refresh fails.

`--source <beeos-managed-agents checkout> [--ref origin/main]` additionally
compares the snapshot with the upstream executor contract files, so a release
can prove the SDK matches the server.

Requires Python 3.10+.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import subprocess
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent
EXECUTOR = ROOT / "contracts" / "bma" / "executor"


class Report:
    def __init__(self) -> None:
        self.failures: list[str] = []

    def fail(self, message: str) -> None:
        self.failures.append(message)

    def section(self, name: str, problems_before: int) -> None:
        status = "ok" if len(self.failures) == problems_before else "FAIL"
        print(f"[{status}] {name}")


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


def git_show(source: Path, ref: str, path: str) -> bytes | None:
    result = subprocess.run(["git", "-C", str(source), "show", f"{ref}:{path}"], capture_output=True)
    return result.stdout if result.returncode == 0 else None


def check_executor_upstream(report: Report, executor_lock: dict[str, Any], source: Path, ref: str) -> None:
    before = len(report.failures)
    commit = subprocess.run(
        ["git", "-C", str(source), "rev-parse", ref], check=True, capture_output=True, text=True
    ).stdout.strip()
    for path in executor_lock["contract"]:
        upstream_path = "contracts/bma/" + path.removeprefix("contracts/bma/executor/")
        upstream = git_show(source, commit, upstream_path)
        if upstream is None:
            report.fail(f"upstream: {upstream_path} is missing")
        elif upstream != (ROOT / path).read_bytes():
            report.fail(f"upstream: {upstream_path} changed; run `make contract-sdk` in BMA")
    report.section(f"upstream: executor contract matches {source.name}@{commit[:12]} ({ref})", before)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--source", type=Path, help="beeos-managed-agents checkout to compare against")
    parser.add_argument("--ref", default="origin/main", help="git ref in --source (default origin/main)")
    args = parser.parse_args()

    executor_lock = json.loads((EXECUTOR / "lock.json").read_text())
    report = Report()
    check_executor(report, executor_lock)
    if args.source:
        check_executor_upstream(report, executor_lock, args.source, args.ref)

    for failure in report.failures:
        print(f"  - {failure}", file=sys.stderr)
    if report.failures:
        print(f"contract check failed: {len(report.failures)} problem(s)", file=sys.stderr)
        return 1
    print("contract check passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
