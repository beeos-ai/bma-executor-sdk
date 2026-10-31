// Cross-language conformance: the TypeScript executor end against the Go SDK
// harness end (go/internal/conformance/harness), over a real WebSocket.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { resolve } from "node:path";
import { test } from "node:test";

import { ExecutorClient } from "../src/executor.js";

test("TypeScript executor passes the Go harness conformance scenario", { timeout: 120_000 }, async () => {
  const harness = spawn("go", ["run", "./internal/conformance/harness"], { cwd: resolve(process.cwd(), "../go") });
  const lines = createInterface({ input: harness.stdout });
  const output: string[] = [];
  const exited = new Promise<number | null>((done) => harness.on("exit", done));
  const wsUrl = await new Promise<string>((done, fail) => {
    lines.on("line", (line) => {
      output.push(line);
      if (line.startsWith("LISTEN ")) done(line.slice("LISTEN ".length));
    });
    harness.on("exit", () => fail(new Error(`harness exited early: ${output.join("\n")}`)));
  });

  const client = new ExecutorClient({
    environmentId: "local",
    executor: { name: "ts-conformance", os: process.platform, arch: process.arch, version: "0" },
    capabilities: { shell: { enabled: true } },
    tokenSource: async () => ({ token: "conformance", ws_url: wsUrl }),
  });
  let runs = 0;
  client.onSessionAssign = async (assign) => {
    setTimeout(() => {
      void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token });
    });
    return { accepted: true, session_id: assign.session_id };
  };
  client.onToolCall = async (call, signal) => {
    runs++;
    if (call.tool === "block") {
      await new Promise((done) => signal.addEventListener("abort", done, { once: true }));
      return { outcome: "succeeded", is_error: false, content: [] };
    }
    return { outcome: "succeeded", is_error: false, content: [{ type: "text", text: `ts:${call.input.command}` }] };
  };
  const registered = await client.start();
  assert.ok(registered.executor_id.startsWith("exe_"));

  const code = await exited;
  client.stop();
  assert.equal(code, 0, output.join("\n"));
  assert.ok(output.includes("PASS"));
  assert.equal(runs, 2, "the repeated tool_call_id must not run twice");
});
