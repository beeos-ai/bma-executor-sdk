// Checks the generated executor types against the vendored contract examples.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { test } from "node:test";

import {
  methods,
  type MethodName,
  type MethodParamsMap,
  type ProcessStartParams,
} from "../src/generated/executor.js";

interface Example {
  name: string;
  method: string;
  kind: "request" | "notification" | "result";
}

const examples = (
  JSON.parse(
    readFileSync(resolve(process.cwd(), "../contracts/bma/executor/examples/conformance.json"), "utf8"),
  ) as { messages: Example[] }
).messages;

test("every executor method has an example of its kind", () => {
  const covered = new Set<string>();
  for (const example of examples) {
    const spec = methods[example.method as MethodName];
    assert.ok(spec, `${example.name}: unknown method`);
    covered.add(spec.name);
    assert.equal(example.kind === "result" ? "request" : example.kind, spec.kind, example.name);
  }
  assert.deepEqual([...covered].sort(), Object.keys(methods).sort());
});

test("v1.1 process.start is capability gated and typed", () => {
  assert.equal(methods["process.start"].since, "bma.executor.v1.1");
  assert.equal(methods["process.start"].capability, "process.enabled");
  const params: MethodParamsMap["process.start"] = {
    session_id: "sess",
    process_id: "proc",
    argv: ["sh", "-c", "true"],
    stderr: "merge",
    stdin: "open",
  } satisfies ProcessStartParams;
  assert.equal(params.stderr, "merge");
});
