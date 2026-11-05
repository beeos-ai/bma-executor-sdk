# Migrating to typed tool handlers

ADR 0012 replaces string-dispatched tool handlers with one typed slot per
built-in tool, in TypeScript, Python and Go (one release, 1.0.0).

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

## Python

```python
# before
async def on_tool(call):
    out = await run(call["input"].get("command", ""))
    return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"],
            "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": out}]}
client.on_tool_call(on_tool)
result = await session.call_tool({"tool_call_id": "c1", "tool": "bash", "input": {"command": "ls"}})

# now
@client.tools.bash.handler
async def bash(input: BashInput, ctx: ToolContext) -> BashResult:  # input["command"] is required
    out, err, code = await run(input["command"])
    return BashResult(stdout=out, stderr=err, exit_code=code)

result = await session.tools.bash.call({"command": "ls"}, tool_call_id="c1")
```

| before | now |
| --- | --- |
| `client.on_tool_call(handler)` | `@client.tools.<name>.handler` (or `client.tools.<name>.handler(fn)`); `fn(input, ctx)` with `ctx.session_id`, `ctx.tool_call_id`, `ctx.permission`, `ctx.progress(msg)` |
| return a `ToolResultParams` dict | return `str`, `list[ContentBlock]`, `ToolResult(content, is_error=, artifacts=)`, or (bash) `BashResult` |
| `is_error: True` | `raise ToolError(text)` |
| capabilities `shell` / `files` / `computer_use` by hand | derived from the registered handlers |
| `session.call_tool({"tool_call_id", "tool", "input"})` | `session.tools.<name>.call(input, tool_call_id=, deadline_ms=, permission=)` or `session.call_tool(tool, input, ...)` |

## Go

```go
// before
client.OnToolCall(func(ctx context.Context, call bmaexecutor.ToolCallParams) (bmaexecutor.ToolResultParams, error) {
	input, _ := call.DecodeInput()
	...
})
result, err := session.CallTool(ctx, bmaexecutor.ToolCallParams{Tool: bmaexecutor.ToolNameBash, Input: bmaexecutor.NewToolInput(bmaexecutor.ToolInput{Command: "ls"})})

// now
client.Tools.Bash.Handle(func(ctx context.Context, in bmaexecutor.BashInput, tool bmaexecutor.ToolContext) (bmaexecutor.ToolOutput, error) {
	return bmaexecutor.BashResult{Stdout: out, ExitCode: code}.Output(), nil
})
result, err := session.Tools.Bash.Call(ctx, bmaexecutor.BashInput{Command: "ls"}, bmaexecutor.CallToolOptions{})
```

| before | now |
| --- | --- |
| `Client.OnToolCall(ToolCallHandler)` | `Client.Tools.<Name>.Handle(func(ctx, <Name>Input, ToolContext) (ToolOutput, error))`; an unregistered tool answers `outcome: "failed"` |
| return `ToolResultParams` | return `TextOutput(s)`, `BashResult{...}.Output()` or `ToolOutput{Content, IsError, Artifacts}`; ids and `duration_ms` are filled in |
| `IsError: true` | `return ToolOutput{}, bmaexecutor.NewToolError(text)` |
| capabilities `Shell` / `Files` / `ComputerUse` by hand | derived from the registered handlers (other fields kept) |
| `Core.Attach` served every tool through one handler | `Core.Attach` registers bash, read, write, edit, glob and grep; behavior unchanged |
| `session.CallTool(ctx, ToolCallParams{Tool, Input: NewToolInput(...)})` | `session.Tools.<Name>.Call(ctx, input, CallToolOptions{ToolCallID, DeadlineMS, Permission})`; `CallTool` stays for raw params |
