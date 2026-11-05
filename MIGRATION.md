# Migrating to typed tool handlers

ADR 0012 replaces string-dispatched tool handlers with one typed slot per
built-in tool. This page covers TypeScript; Python and Go follow in the same
release.

## Executor end

```ts
// before
client.handleTool("bash", async (call, { signal, progress }) => {
  const r = await run(call.input.command ?? "", { cwd: call.input.cwd, signal });
  return { outcome: "succeeded", is_error: false, content: [{ type: "text", text: r.stdout + r.stderr }] };
});
client.onToolCall(async (call) => { /* every other tool */ });

// now
client.tools.bash.handle(async (input, { signal, progress }) => {  // input: BashInput, command is required
  const r = await run(input.command, { cwd: input.cwd, signal });
  return { stdout: r.stdout, stderr: r.stderr, exitCode: r.code };
});
client.tools.read.handle(async ({ path, offset, limit }) => readText(path, offset, limit));
```

| before | now |
| --- | --- |
| `handleTool(name, (call, ctx) => …)` | `tools.<name>.handle((input, ctx) => …)`; `ctx` adds `sessionId`, `toolCallId`, `permission` |
| `onToolCall(handler)` (catch-all) | removed: register each tool; an unregistered tool answers `outcome: "failed"` |
| `call.input: ToolInput` (every field optional) | `input: BashInput / ReadInput / …` (required fields per tool) |
| return `{ outcome, is_error, content }` | return a string, `ContentBlock[]`, `{ content, isError?, artifacts? }`, or (bash) `{ stdout, stderr, exitCode }` |
| return `is_error: true` for a tool-level error | `throw new ToolError(message)` or return `{ content, isError: true }` |
| `capabilities.shell / files / computer_use` set by hand | derived from the registered handlers (other capability fields are kept) |
| `ToolCallHandler` type | `ToolHandler<N>` |

`reportToolResult(params)` stays for results produced outside a handler.

## Harness end

| before | now |
| --- | --- |
| `session.callTool({ tool_call_id, tool: "bash", input: { command } })` | `session.tools.bash.call({ command }, { toolCallId })` or `session.callTool("bash", { command }, { toolCallId })` |
| `callTool(params, signal)` | `callTool(tool, input, { toolCallId, deadlineMs, permission, signal })` |
