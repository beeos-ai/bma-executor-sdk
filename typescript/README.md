# @beeos-ai/bma-executor-sdk (TypeScript)

[![npm](https://img.shields.io/npm/v/@beeos-ai/bma-executor-sdk.svg)](https://www.npmjs.com/package/@beeos-ai/bma-executor-sdk)

Executor (`ExecutorClient`), harness (`Harness`) and agent ends of the BMA
Executor Protocol `bma.executor.v1.1` (and `v1`), with the same three parts and
behavior as the [Go `executor` module](../README.md). Protocol types are
generated from the contract ([`src/generated/executor.ts`](src/generated/executor.ts),
snapshot and lock in [`contracts/bma/executor`](../../contracts/bma/executor));
the runtime is handwritten. Transports: WebSocket, WebSocket over a Unix domain
socket or Windows named pipe (`unix://`, `npipe://`), in-process.

| | |
| --- | --- |
| Version | 0.4.4 (tag `executor-ts/v0.4.4`), see [CHANGELOG](../../CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (contract 1.1.0, [`lock.json`](../../contracts/bma/executor/lock.json)) |
| Node.js | 20 or newer; ESM, depends on `ws` |
| Integration guide | [docs/executor](../../docs/executor/README.md) ([中文](../../docs/executor/README.zh-CN.md)) |

```sh
npm install @beeos-ai/bma-executor-sdk@0.4.4
```

The REST / façade client is the separate package
[`@beeos-ai/bma-sdk`](../../typescript).

### Executor quick start

```ts
import { ExecutorClient } from "@beeos-ai/bma-executor-sdk";

const client = new ExecutorClient({
  environmentKey: process.env.BMA_ENVIRONMENT_KEY,
  environmentId: process.env.BMA_ENVIRONMENT_ID!,
  executor: { name: "my-executor", os: process.platform, arch: process.arch, version: "0.1.0" },
  capabilities: { shell: { enabled: true } },
});
client.handleTool("bash", async (call, { signal, progress }) => {
  progress("running");
  return { outcome: "succeeded", is_error: false, content: [{ type: "text", text: "ok" }] };
});
await client.start();
```

### Executor lifecycle, cancellation, and results

- `tool.cancel` aborts the handler's `signal` (so do `deadline_ms` and
  `stop()`); the client answers `{"canceled":true}` and reports
  `outcome: "canceled"`. A repeated `tool.call` id is acknowledged, not run
  twice. `handleTool(name, …)` routes one tool, `onToolCall` the rest.
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
  `onRequest(method, …)` serves `process.*`, `terminal.*` and `skill.install`.

### Harness quick start

```ts
import { Harness } from "@beeos-ai/bma-executor-sdk";

const harness = new Harness({ authenticate: (request) => verify(request) }); // omit only on loopback
const { url } = await harness.listen(); // or harness.attach(httpServer)

const executor = await harness.waitExecutor();
const session = await executor.assignSession({ session_id: "sess_1" });
await session.ready();
const result = await session.callTool({ tool: "bash", input: { command: "ls" } });
const mcp = await session.mcp("echo"); // raw MCP JSON-RPC
await session.stop("done");
```

Semantics match the Go `Harness`: `callTool` is idempotent by `tool_call_id`;
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

| Go (`github.com/beeos-ai/bma-sdks/executor`) | TypeScript (`@beeos-ai/bma-executor-sdk`) |
| --- | --- |
| `NewClient(Options)` / `Start` / `Stop` | `new ExecutorClient(options)` / `start()` / `stop()` |
| `Options.TokenSource`, `StaticToken` | `tokenSource`, `staticToken` (or `dialer`) |
| `OnToolCall(ctx, call)` | `onToolCall(call, { signal, progress })`, `handleTool(name, …)` |
| `OnSessionAssign` / `OnSessionStop` / `OnDrain` / `OnRequest` / `OnNotification` | `onSessionAssign` / `onSessionStop` / `onDrain` / `onRequest` / `onNotification` |
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
| `StaticToken("unix://…")`, `ListenUnixSocket` + `Harness.DialExecutor`, `InProcessDialer` | `Harness.listenLocal` / `listenExecutor` + `Harness.dialExecutor` / `inProcessDialer` (named pipes only in TypeScript) |

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
