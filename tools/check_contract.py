#!/usr/bin/env python3
"""Contract drift check for the BMA executor SDKs.

Default mode (CI, needs only this repository): the executor contract snapshot
(contracts/bma/executor) and the Go, TypeScript and Python executor types
generated from it by the BMA repository (`make contract-sdk`) match the
digests in its lock.json, so a hand edit or a partial refresh fails.

`--source <beeos-managed-agents checkout> [--ref origin/main]` additionally
compares the snapshot with the upstream executor contract files, so a release
can prove the SDK matches the server.

`--source ... --regenerate` (the release gate) instead proves where the
snapshot comes from: the lock's source_commit must be an ancestor of --ref
(a merged BMA commit, not a pull request's), and `make contract-sdk` run at
that commit (tools/contract/generate.mjs in a temporary worktree, into a
copy of this repository) must give exactly the snapshot, the generated types
and lock.json that are here. A snapshot generated from an unmerged BMA pull
request fails it (SDK 1.6.24-1.6.32 carried contract 1.14.0 from one).

Requires Python 3.10+.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
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


def git(source: Path, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["git", "-C", str(source), *args], check=check, capture_output=True, text=True)


def check_executor_regenerated(report: Report, executor_lock: dict[str, Any], source: Path, ref: str) -> None:
    before = len(report.failures)
    commit = executor_lock["source_commit"]
    if git(source, "cat-file", "-e", commit + "^{commit}", check=False).returncode != 0:
        report.fail(f"regenerate: source_commit {commit} is not in {source} (fetch it, or it was never merged)")
        report.section(f"regenerate: executor contract from {commit[:12]}", before)
        return
    if git(source, "merge-base", "--is-ancestor", commit, ref, check=False).returncode != 0:
        report.fail(f"regenerate: source_commit {commit[:12]} is not an ancestor of {ref}: the snapshot was generated from an unmerged BMA change")
        report.section(f"regenerate: executor contract from {commit[:12]}", before)
        return
    # Resolved: generate.mjs runs only when its path is the real one (macOS
    # /var is a link to /private/var).
    work = Path(tempfile.mkdtemp(prefix="bma-contract-")).resolve()
    bma, sdk = work / "bma", work / "sdk"
    try:
        git(source, "worktree", "add", "--detach", str(bma), commit)
        subprocess.run(["npm", "ci", "--prefix", str(bma / "tools" / "contract"), "--ignore-scripts", "--no-audit", "--no-fund"],
                       check=True, capture_output=True)
        shutil.copytree(ROOT, sdk, ignore=shutil.ignore_patterns(".git", "node_modules", ".cache", "dist"))
        result = subprocess.run(["node", str(bma / "tools" / "contract" / "generate.mjs"), "--sdk", str(sdk)], capture_output=True, text=True)
        if result.returncode != 0 or "contract snapshot" not in result.stdout:
            report.fail(f"regenerate: generate.mjs at {commit[:12]} did not write the snapshot: {(result.stderr or result.stdout).strip()[-500:]}")
        else:
            paths = ["contracts/bma/executor/lock.json", *executor_lock["contract"], *executor_lock["generated"]]
            fresh = json.loads((sdk / "contracts/bma/executor/lock.json").read_text())
            paths += [p for group in ("contract", "generated") for p in fresh[group] if p not in paths]
            for path in paths:
                here, there = ROOT / path, sdk / path
                if not here.is_file() or not there.is_file() or here.read_bytes() != there.read_bytes():
                    report.fail(f"regenerate: {path} differs from `make contract-sdk` at {commit[:12]}")
    except subprocess.CalledProcessError as err:
        stderr = err.stderr.decode() if isinstance(err.stderr, bytes) else (err.stderr or "")
        report.fail(f"regenerate: {' '.join(map(str, err.cmd[:3]))} failed: {stderr.strip()[-500:]}")
    finally:
        git(source, "worktree", "remove", "--force", str(bma), check=False)
        shutil.rmtree(work, ignore_errors=True)
    report.section(f"regenerate: executor contract from {source.name}@{commit[:12]} (ancestor of {ref})", before)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--source", type=Path, help="beeos-managed-agents checkout to compare against")
    parser.add_argument("--ref", default="origin/main", help="git ref in --source (default origin/main)")
    parser.add_argument("--regenerate", action="store_true",
                        help="with --source: the lock's source_commit is an ancestor of --ref and regenerating at it gives this snapshot")
    args = parser.parse_args()
    if args.regenerate and not args.source:
        parser.error("--regenerate needs --source")

    executor_lock = json.loads((EXECUTOR / "lock.json").read_text())
    report = Report()
    check_executor(report, executor_lock)
    if args.source and args.regenerate:
        check_executor_regenerated(report, executor_lock, args.source, args.ref)
    elif args.source:
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
