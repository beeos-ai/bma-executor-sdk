# Migrating to typed tool handlers

ADR 0012 replaces string-dispatched tool handlers with one typed slot per
built-in tool, and string-named protocol calls with methods typed by the
contract, in TypeScript, Python and Go (one release, **1.0.0**).

## 1.1.0

No breaking change; 1.0 code builds and behaves the same. Everything new is
opt-in ([guide](docs/executor/session-context.md)):

- Go needs **Go 1.25** or later (the module's `go` directive).
- `ProcessOptions` gains `Launch` and `SessionFiles`; leaving them nil keeps
  1.0 behavior (cwd confined to the workspace, no `session_files`
  capability, `/mnt/session` on the platform side).
- `Launch` is also called for a package install (`LaunchRequest.PackageInstall`
  is non-nil, `Argv`/`Env` are the installer's command); a hook that only
  handles commands should return an empty `LaunchSpec` for them or refuse
  them explicitly.
- `FileTransferer` is unchanged; implement `SessionFileTransferer` as well
  to receive the session with each transfer.
- `Capabilities.SessionFiles` / `session_files` is a new optional field;
  executors that do not set it are unaffected.
- `NewToolError(text)` keeps its meaning (a tool-level error the model sees);
  a protocol refusal with `data.reason` is `NewReasonError(code, reason, msg)`.

## All languages

- **Register tool handlers before `start()`** if the first registration
  should already advertise them; a handler registered later is announced
  with `executor.capabilities.update`.
- **Capabilities.** `shell` and `files` are enabled while a handler of the
  group is registered; an explicit `enabled: false` wins (Go: pass the
  `ShellCapability` / `FilesCapability` with `Enabled: false`; a struct you
  pass is used as given, so set `Enabled: true` when you pass one to set
  other fields). `updateCapabilities` replaces the configured capabilities
  and the same derivation applies. `computer_use` is off and the
  `computer_observe` / `computer_action` slots are gone until the contract
  defines their input members.
- **Unregistered tools** are refused before the ack with JSON-RPC `-32012`
  (`data.capability: "tool:<name>"`), as Go Core refuses them, instead of an
  acknowledged call that fails.
- **Inputs** missing a required member (`bash` without `command`, ...)
  fail the call (`outcome: "failed"`, `<tool> requires <member>`) before the
  handler runs.
- **bash output** larger than 48 KiB, or marked truncated by the handler, is
  cut to Go Core's preview (`exit_code: N (output truncated to fit the
  result; M bytes total)`).
- **Two `ToolError`s.** The executor SDK's `ToolError` is a tool-level error
  the model sees as `outcome: "succeeded"` with `is_error: true` (the
  executor protocol has `is_error`). The bma-sdk custom-tool `ToolError`
  submits `outcome: "failed"`, because the REST contract's tool result has no
  `is_error`.
- **Protocol methods are typed.** `onRequest` / `on_request` / Go `OnRequest`
  with a method string and untyped params, `onNotification` /
  `on_notification` / `OnNotification`, and untyped `call` / `notify` /
  `Call` / `Notify` are replaced:

| before | now |
| --- | --- |
| TS `client.onRequest("process.list", (params: unknown) => ...)` | `client.onRequest('process.list', (params: ProcessListParams) => ProcessListResult)` |
| TS `client.call<T>(method, params)` / `notify(method, params)` / harness `executor.call<T>(method, params)` | the same names, typed by `MethodParamsMap` / `MethodResultMap`; only contract methods of the right direction compile |
| Python `client.on_request("process.list", handler)` | `client.on_request(methods.PROCESS_LIST, handler)` (`from bma_executor_sdk import methods`) |
| Python `await client.call(method, params)` / `notify(method, params)` | `await client.call(methods.SESSION_EVENT, params)` / `await client.notify(methods.TOOL_PROGRESS, params)` |
| Go `client.OnRequest(method, func(ctx, json.RawMessage) (any, error))` | `bmaexecutor.Requests.ProcessList.Handle(client, func(ctx, ProcessListParams) (ProcessListResult, error))` |
| Go `client.Call(ctx, method, params, &out)` / `client.Notify(ctx, method, params)` | `bmaexecutor.Calls.SessionEvent.Call(ctx, client, params)` / `bmaexecutor.Notifications.ExecutorCapabilitiesUpdate.Notify(ctx, client, params)` |
| `onNotification` / `on_notification` / `OnNotification` | removed: `executor.close` is the only harness-to-executor notification and the client handles it |

The Go harness keeps `Executor.Call(ctx, method, params, &out)` unchanged in
1.0 as the low-level call for harness-to-executor requests; typed wrappers
come in 1.1 next to it, so code written against it keeps compiling.

- **Base URL.** `baseUrl` / `base_url` / `Options.BaseURL` is the deployment
  origin, the same value as `BMA_BASE_URL` in bma-sdk 1.0 (for example
  `https://api.staging.bma.beeos.ai`). A value ending in `/api/v1` (bma-sdk
  0.x API root) is accepted: the suffix is dropped before
  `/v1/executors/token` is added.

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
| `onToolCall(handler)` (catch-all) | removed: register each tool; an unregistered tool is refused with `-32012` |
| `call.input: ToolInput` (every field optional) | `input: BashInput / ReadInput / …` (required fields per tool) |
| return `{ outcome, is_error, content }` | return a string, `ContentBlock[]`, `{ content, isError?, artifacts? }`, or (bash) `{ stdout, stderr, exitCode }` |
| return `is_error: true` for a tool-level error | `throw new ToolError(message)` or return `{ content, isError: true }` |
| `capabilities.shell / files / computer_use` set by hand | derived from the registered handlers; `enabled: false` wins (other capability fields are kept) |
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
| `Client.OnToolCall(ToolCallHandler)` | `Client.Tools.<Name>.Handle(func(ctx, <Name>Input, ToolContext) (ToolOutput, error))`; an unregistered tool is refused with `-32012` |
| return `ToolResultParams` | return `TextOutput(s)`, `BashResult{...}.Output()` or `ToolOutput{Content, IsError, Artifacts}`; ids and `duration_ms` are filled in |
| `IsError: true` | `return ToolOutput{}, bmaexecutor.NewToolError(text)` |
| capabilities `Shell` / `Files` / `ComputerUse` by hand | derived from the registered handlers (other fields kept) |
| `Core.Attach` served every tool through one handler | `Core.Attach` registers bash, read, write, edit, glob and grep; behavior unchanged |
| `session.CallTool(ctx, ToolCallParams{Tool, Input: NewToolInput(...)})` | `session.Tools.<Name>.Call(ctx, input, CallToolOptions{ToolCallID, DeadlineMS, Permission})`; `CallTool` stays for raw params |
