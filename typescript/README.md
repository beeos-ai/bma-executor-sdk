# @beeos-ai/bma-executor-sdk (TypeScript)

[![npm](https://img.shields.io/npm/v/@beeos-ai/bma-executor-sdk.svg)](https://www.npmjs.com/package/@beeos-ai/bma-executor-sdk)

Connect your machine (hands) or your own agent (brain) to BMA — the executor, harness and agent ends of the `bma.executor` protocol.
[`@beeos-ai/bma-sdk`](https://github.com/beeos-ai/bma-sdk) drives and uses agents (REST / façade client); `bma-executor-sdk` connects hands or brains to BMA.

Executor (`ExecutorClient`), harness (`Harness`) and agent ends of the BMA
Executor Protocol `bma.executor.v1.1` (and `v1`), with the same three parts and
behavior as the [Go `executor` module](../README.md). Protocol types are
generated from the contract ([`src/generated/executor.ts`](src/generated/executor.ts),
snapshot and lock in [`contracts/bma/executor`](../contracts/bma/executor));
the runtime is handwritten. Transports: WebSocket, WebSocket over a Unix domain
socket or Windows named pipe (`unix://`, `npipe://`), in-process.

| | |
| --- | --- |
| Version | 1.0.0 (tag `v1.0.0`), see [CHANGELOG](../CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (contract 1.1.0, [`lock.json`](../contracts/bma/executor/lock.json)) |
| Node.js | 20 or newer; ESM, depends on `ws` and `@types/ws` (the declarations use `ws` types) |
| Integration guide | [docs/executor](../docs/executor/README.md) ([中文](../docs/executor/README.zh-CN.md)) |

```sh
npm install @beeos-ai/bma-executor-sdk@1.0.0
```

The REST / façade client is the separate package
[`@beeos-ai/bma-sdk`](https://github.com/beeos-ai/bma-sdk/tree/main/typescript).

### Executor quick start

```ts
import { ExecutorClient } from "@beeos-ai/bma-executor-sdk";

const client = new ExecutorClient({
  baseUrl: process.env.BMA_BASE_URL, // required: see below
  environmentKey: process.env.BMA_ENVIRONMENT_KEY,
  environmentId: process.env.BMA_ENVIRONMENT_ID!,
  executor: { name: "my-executor", os: process.platform, arch: process.arch, version: "0.1.0" },
});
client.tools.bash.handle(async (input, { signal, progress }) => {   // input: BashInput
  progress("running");
  const r = await run(input.command, { cwd: input.cwd, env: input.env, signal });
  return { stdout: r.stdout, stderr: r.stderr, exitCode: r.code };    // rendered like Go Core
});
client.tools.read.handle(async ({ path, offset, limit }) => readText(path, offset, limit)); // string or content blocks
await client.start();
```

Set `baseUrl` to your BMA deployment: the hosted service is in early access and the default
`DEFAULT_BASE_URL` (`https://api.bma.beeos.ai`) does not resolve yet. It is the deployment
origin, the same value as `BMA_BASE_URL` in bma-sdk (for example `https://api.staging.bma.beeos.ai`);
the client posts the environment key to `{baseUrl}/v1/executors/token` and drops a trailing `/api/v1`. The SDK does not read
`BMA_BASE_URL` itself. A `tokenSource` carries its own channel URL and ignores `baseUrl`.

### Executor lifecycle, cancellation, and results

- `tool.cancel` aborts the handler's `signal` (so do `deadline_ms` and
  `stop()`); the client answers `{"canceled":true}` and reports
  `outcome: "canceled"`. A repeated `tool.call` id is acknowledged, not run
  twice. Each built-in tool has a typed slot `client.tools.<name>.handle(fn)`
  (`bash`, `read`, `write`, `edit`, `glob`, `grep`, `list_files`; the
  `computer_*` tools get slots once the contract defines their input) whose
  input is the contract's `<Tool>Input`. Input missing a required member
  fails the call (`bash requires command`) before the handler runs. A
  handler returns a string, content blocks or `{ content, isError,
  artifacts }`; bash may return `{ stdout, stderr, exitCode, truncated? }`,
  rendered as Go Core does (stdout, stderr, `exit_code: N`, cut to Core's
  preview beyond `INLINE_RESULT_BYTES`). `throw new ToolError(msg)` reports
  a tool-level error (`is_error: true`); any other exception is
  `outcome: "failed"`. A `tool.call` for a tool without a handler is refused
  before the ack with `-32012` (`data.capability: "tool:<name>"`), as Go
  Core refuses it.
- The `shell` and `files` capabilities are enabled while a handler of the
  group is registered, unless you set `enabled: false` (which wins, like
  MCP's `RegisteredTool.enabled`); a handler registered after `start()`
  sends `executor.capabilities.update`, as an MCP server sends
  `tools/list_changed`.
- Call `sessionReady` after workspace/MCP setup and `sessionEnded` after
  cleanup. `tool.result` is retried on `-32009`, `-32004` or
  `data.retryable: true`, idempotent by `tool_call_id`; when every attempt
  fails `onToolResultLost` receives the result to persist and replay.
- Reconnect: exponential backoff (500 ms base, ×2, 30 s cap, jitter) with a
  fresh token per dial and `resume` (`executor_id`, `last_connection_id`,
  `active_sessions`). Close codes 1008, 4002, 4003, 4004 and 4005 are terminal;
  4001 (token expired) reconnects with a new token.
- `openMcpChannel(sessionId, serverKey)` opens the raw MCP channel
  (`?channel=mcp&session_id=&server_key=`); `read()`/`write()` carry MCP
  JSON-RPC frames unchanged.
- A handler that throws `RpcError` keeps its code and data on the wire.
  `onRequest('process.start', (params) => …)` serves `process.*`,
  `terminal.*`, `skill.install` and `workspace.backup` with the contract's
  params and result types; `call(method, params)` and
  `notify(method, params)` send executor-to-harness methods typed the same
  way (`MethodParamsMap` / `MethodResultMap`).

### Harness quick start

```ts
import { Harness } from "@beeos-ai/bma-executor-sdk";

const harness = new Harness({ authenticate: (request) => verify(request) }); // omit only on loopback
const { url } = await harness.listen(); // or harness.attach(httpServer)

const executor = await harness.waitExecutor();
const session = await executor.assignSession({ session_id: "sess_1" });
await session.ready();
const result = await session.tools.bash.call({ command: "ls" }); // or callTool("bash", { command: "ls" }, { toolCallId })
const mcp = await session.mcp("echo"); // raw MCP JSON-RPC
await session.stop("done");
```

Semantics match the Go `Harness`: `callTool` is idempotent by `toolCallId`;
an unknown call's `tool.result` is `-32002`, a foreign session `-32007`;
sessions and pending calls survive a reconnect within `leaseTtlMs`; a newer
connection replaces the old one with 4004; lease expiry or `executor.close`
fails pending calls with `ToolOutcomeUnknownError`.

### Transports

Messages are identical on every transport; who dials is independent of the
protocol role (the executor always sends `executor.register` first).

| Transport | Executor end | Harness end |
| --- | --- | --- |
| WebSocket (cloud, executor dials) | default, or `webSocketDialer(...)` | `listen()`, `attach(server)`, `handleUpgrade(...)` |
| Local socket, executor dials | `webSocketDialer({ ..., tokenSource: staticToken('unix:///path.sock', token) })` | `new Harness({ authenticate: bearerAuthenticator(token) }).listenLocal(path)` |
| Local socket, harness dials (bmad listens) | `listenExecutor({ path, token })` → `new ExecutorClient({ dialer: listener.dialer })` | `harness.dialExecutor(listener.url, { token })` |
| In-process | `dialer: harness.inProcessDialer()` | — |

Local sockets carry the same WebSocket protocol as the cloud (subprotocol
negotiation, bearer token, close codes) over a Unix domain socket
(`unix:///path.sock`, 0700 directory, 0600 socket) or a Windows named pipe
(`npipe:////./pipe/<name>`), as the Go and Python SDKs do. Each start writes a
fresh local token (`createLocalToken`, 0600); environment keys are never used
locally (see [`src/local.ts`](src/local.ts)). When the harness
dials the executor it also dials the session MCP channels (`session.mcp`).

### Agent executors

```ts
client.serveAgent(
  {
    openTurn: async (p) => ({ accepted: true, turn_id: p.turn_id, executor_session_ref: "conv_1", resumed: false, effective: {} }),
    runTurn: async (turn, signal) => {
      const answer = await turn.requestInteraction({ kind: "permission", tool_use_id: "tu_1", tool_name: "bash" }, signal);
      turn.emit({ type: "agent.message", content: [{ type: "text", text: answer.outcome }] });
    },
    closeSession: async () => {},
  },
  { models: [{ id: "my-model" }] },
);
```

The SDK assigns event ids and sequences, delivers events in order with
ACKs, resends after a reconnect, and frames each turn with
`session.status_running` and `session.status_idle` (`completed`, `canceled`,
`failed`). The harness side is `HarnessOptions.agent` (`AgentPlatform`) plus
`executor.startTurn / sendTurnInput / cancelTurn / closeAgentSession /
respondInteraction`; a sequence gap answers `-32013` with
`data.expected_sequence`.

### Examples

[`examples`](examples) (`npm install` there first):
`executor-minimal` (cloud executor with bash), `harness-loopback` (bmad-style
local socket: executor listens, harness dials), `agent-minimal` (agent
executor and in-memory platform in one process).

### Go ↔ TypeScript

| Go (`github.com/beeos-ai/bma-executor-sdk`) | TypeScript (`@beeos-ai/bma-executor-sdk`) |
| --- | --- |
| `NewClient(Options)` / `Start` / `Stop` | `new ExecutorClient(options)` / `start()` / `stop()` |
| `Options.TokenSource`, `StaticToken` | `tokenSource`, `staticToken` (or `dialer`) |
| `Tools.<Name>.Handle(func(ctx, input, tool))` | `tools.<name>.handle(input, { signal, progress })` |
| `OnSessionAssign` / `OnSessionStop` / `OnDrain` | `onSessionAssign` / `onSessionStop` / `onDrain` |
| `Requests.ProcessStart.Handle(client, fn)` / `Calls.X.Call(ctx, client, p)` / `Notifications.X.Notify(ctx, client, p)` | `onRequest('process.start', fn)` / `call('x', p)` / `notify('x', p)` |
| `SessionReady` / `SessionEnded` / `ReportToolResult` | `sessionReady` / `sessionEnded` / `reportToolResult` |
| `SendSessionStatus` / `SendToolProgress` / `SendExecutorCapabilitiesUpdate` | `sessionStatus` / `toolProgress` / `updateCapabilities` |
| `OpenMCPChannel` → `MCPChannel` | `openMcpChannel` → `FrameChannel` |
| `ServeAgent(AgentHandler, AgentCapability)` | `serveAgent(handler, capability)` |
| `AgentTurn.Emit/EmitWithID/Delta/Flush/Inputs/RequestInteraction/CancelInteraction` | `emit/emitWithId/delta/flush/inputs/requestInteraction/cancelInteraction` |
| `NewHarness(HarnessOptions)` (`http.Handler`) | `new Harness(options)` + `listen/listenLocal/attach/handleUpgrade/accept/dialExecutor/inProcessDialer` |
| `Harness.WaitExecutor` / `Executors` | `waitExecutor` / `executors` |
| `Executor.AssignSession/Drain/StartTurn/SendTurnInput/CancelTurn/CloseAgentSession/RespondInteraction` | `HarnessExecutor.assignSession/drain/startTurn/sendTurnInput/cancelTurn/closeAgentSession/respondInteraction` |
| `Session.Ready/CallTool/CancelTool/Stop/MCP/Done/EndReason` | `HarnessSession.ready/callTool/cancelTool/stop/mcp/done/endReason` |
| `AgentPlatform` | `AgentPlatform` |
| `*RPCError`, `ErrNotConnected`, `ErrClientStopped`, `ErrSessionEnded`, `ErrToolOutcomeUnknown`, `ErrAssignRejected` | `RpcError`, `NotConnectedError`, `ClientStoppedError`, `SessionEndedError`, `ToolOutcomeUnknownError`, `AssignRejectedError` |
| `Core` (bash/files/stdio MCP) | not provided |
| `StaticToken("unix://…")`, `ListenUnixSocket` + `Harness.DialExecutor`, `InProcessDialer` | `Harness.listenLocal` / `listenExecutor` + `Harness.dialExecutor` / `inProcessDialer` (named pipes in Go: `ListenNamedPipe`, `NamedPipeListener`; Python: `serve_npipe`) |

### Tests

`npm test` runs the conformance examples against
both TS ends ([`test/conformance.test.ts`](test/conformance.test.ts)), executor ↔
harness over every transport with reconnect and lease
([`test/executor.test.ts`](test/executor.test.ts)), the agent capability
([`test/agent.test.ts`](test/agent.test.ts)), and interop with the Go SDK
([`test/interop.test.ts`](test/interop.test.ts): TS executor ↔ Go harness, TS
agent ↔ Go platform, TS harness ↔ Go executor; needs Go), and interop with the
Python SDK over WebSocket on a Unix socket
([`test/interop-python.test.ts`](test/interop-python.test.ts): TS executor ↔
Python harness, Python executor ↔ TS harness; `BMA_PYTHON_SDK=<python dir with
.venv>`, skipped without it), and TS ↔ Go over a Unix socket
([`test/interop-go-unix.test.ts`](test/interop-go-unix.test.ts)).
