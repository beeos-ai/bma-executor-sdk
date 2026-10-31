# bma-executor-sdk (Go)

[![Go Reference](https://pkg.go.dev/badge/github.com/beeos-ai/bma-sdks/executor.svg)](https://pkg.go.dev/github.com/beeos-ai/bma-sdks/executor)

Go module `github.com/beeos-ai/bma-sdks/executor` (package `bmaexecutor`): the executor, harness and agent ends of the BMA Executor Protocol `bma.executor.v1.1` (the harness also accepts `v1`). Protocol types are generated from the contract in [`contracts/bma/executor`](../contracts/bma/executor); the runtime is handwritten. bmad's execution model and the BMA server speak the same contract.

| | |
| --- | --- |
| Version | 0.4.1 (tag `executor/v0.4.1`), see [CHANGELOG](../CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (contract 1.1.0, [`lock.json`](../contracts/bma/executor/lock.json)) |
| Go | 1.24 or newer |
| Integration guide | [docs/executor](../docs/executor/README.md) ([中文](../docs/executor/README.zh-CN.md)) |

```sh
go get github.com/beeos-ai/bma-sdks/executor@v0.4.1
```

The REST / façade client is the separate module `github.com/beeos-ai/bma-sdks/go` ([repository README](../README.md)).

## Executor quick start

An executor registers with BMA (or a local harness) and answers tool calls:

```go
package main

import (
	"context"
	"log"
	"os"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

func main() {
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		EnvironmentKey: os.Getenv("BMA_ENVIRONMENT_KEY"),
		EnvironmentID:  os.Getenv("BMA_ENVIRONMENT_ID"),
		Executor:       bmaexecutor.ExecutorInfo{Name: "my-executor", OS: "linux", Arch: "arm64", Version: "0.1.0"},
	})
	if err != nil {
		log.Fatal(err)
	}
	client.OnToolCall(func(ctx context.Context, call bmaexecutor.ToolCallParams) (bmaexecutor.ToolResultParams, error) {
		return bmaexecutor.ToolResultParams{
			SessionID:  call.SessionID,
			ToolCallID: call.ToolCallID,
			Outcome:    bmaexecutor.ToolOutcomeSucceeded,
			Content:    []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: "ok"}},
		}, nil
	})
	if _, err := client.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	select {}
}
```

A machine-scoped executor uses a machine token through `Options.TokenSource` and no environment id. Go clients negotiate `bma.executor.v1.1` by default (`Options.Protocol`).

Executor types follow one mapping: required members are values, optional members are values with `omitempty` (named structs and nullable members are pointers), enums are untyped constants (`Code*` error codes on `RPCError.Code`), and opaque payloads such as `ToolCallParams.Input` stay `json.RawMessage` (`call.DecodeInput()` decodes the built-in `ToolInput`; `NewToolInput(ToolInput{...})` builds it).

### Lifecycle, cancellation, and results

- `tool.cancel` from the harness cancels the `context.Context` passed to the `OnToolCall` handler. The client responds `{"canceled":true}` and reports the result with `outcome: "canceled"`; handlers must stop their process group when the context is done.
- A repeated `tool.call` with the same `tool_call_id` is acknowledged but not executed twice.
- Call `SessionReady` after workspace and MCP setup, and `SessionEnded` after every child process and MCP server has stopped. The SDK does not infer these transitions because only the adapter knows when setup and cleanup finished.
- `tool.result` is retried with exponential backoff on `-32009` (`executor_busy`), `-32004` (`timeout`), or `data.retryable: true`. Results are idempotent by `tool_call_id`. When every attempt fails, `OnToolResultLost` is called with the original result so the adapter can persist and replay it; the client never drops a result silently.
- Large output is reported as artifact references, not base64 in `tool.result`: `Client.UploadArtifact` uploads the bytes, `NewArtifactRef` and `NewArtifactContentBlock` build the blocks.
- `OpenMCPChannel` opens the MCP channel for a session and server key and exchanges raw MCP JSON-RPC frames over whatever transport the control channel uses.
- Session handler errors that are an `*RPCError` keep their code and data on the wire (for example retryable `executor_busy`).

## Execution core: one hand, any brain

`Core` is the executor's execution core, independent of who drives it. `core.Attach(brain, client)` installs the session and tool handlers on a `Client`; attach one `Client` per harness, so one executor process serves the BMA cloud and a local harness at the same time:

```go
core, _ := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: dir, MaxSessions: 8, MaxSessionsPerBrain: 4})
_ = core.Register(bmaexecutor.NewProcessCapability(core, bmaexecutor.ProcessOptions{}))
cloud, _ := bmaexecutor.NewClient(bmaexecutor.Options{EnvironmentKey: key, EnvironmentID: env, Executor: info, Capabilities: core.Capabilities()})
local, _ := bmaexecutor.NewClient(bmaexecutor.Options{EnvironmentID: "local", Executor: info, Capabilities: core.Capabilities(),
	TokenSource: bmaexecutor.StaticToken("ws://127.0.0.1:7777", localToken)})
_ = core.Attach("cloud", cloud)
_ = core.Attach("local", local)
```

- **Tools**: `bash` (login shell in its own process group; env limited to PATH/HOME/USER/LANG/TMPDIR plus request env and `CoreOptions.Env`; `tool.cancel`, the deadline, `timeout_ms` and `session.stop` kill the whole group; a non-zero exit is a succeeded call reporting `exit_code`) and `read`/`write`/`edit`/`glob`/`grep`, confined to the session workspace `WorkRoot/<brain>/<session>` (override with `CoreOptions.Workspace`). Other tools return `capability_unavailable`.
- **Capabilities**: `core.Register(cap)` before `Attach` adds methods behind capability bits; `core.Capabilities()` advertises them. `NewProcessCapability(core, ProcessOptions{...})` serves `process.*` (PTY where supported; `FileTransfer` and `GitCredentials` hooks enable those `process.start` fields); `NewSkillInstallCapability(installer)` serves `skill.install`; implement `Capability` for your own methods (for example `terminal.*`). A `SessionScoped` capability gets the brain's `*CoreSession` and `OnSessionEnd` runs before `session.ended`.
- **Private environment**: `CoreOptions.DenyEnv` names executor-private variables (for example machine tokens) that no bash tool, process, terminal or stdio MCP server inherits and no request may set.
- **stdio MCP**: every `session.assign.mcp_servers[]` entry with `connection_origin: environment` starts one process per (brain, session, `server_key`), verifies `command_sha256` when given, runs the command without a shell, and bridges stdin/stdout lines to that server's MCP channel. If either side ends both are restarted with backoff; after 5 consecutive failures the core sends `session.status` `error` / `mcp_unavailable: <key>`.
- **Isolation**: sessions are keyed by (brain, `session_id`); a `tool.call` reaches only sessions its own brain assigned; MCP channels and workspaces are per brain and session. Bash is not a sandbox: every brain runs as the executor's OS user, so only attach brains you trust with this machine.
- **Capacity**: `MaxSessions` (default 16) across brains and `MaxSessionsPerBrain` (default `MaxSessions`) per brain. A full core answers `session.assign` with `executor_busy` (`-32009`, `retryable: true`).

## Harness quick start

`Harness` is the harness ("brain") end: an `http.Handler` that accepts executor control and MCP channels, plus the API that drives them. It speaks the same protocol as the BMA service, so an executor cannot tell them apart.

```go
harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{
	Authenticate: authenticate, // func(*http.Request) (bmaexecutor.Principal, error); nil only on loopback
})
go http.Serve(listener, harness)

executor, _ := harness.WaitExecutor(ctx)
session, _ := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{SessionID: "sess_1"})
_ = session.Ready(ctx)
result, _ := session.CallTool(ctx, bmaexecutor.ToolCallParams{
	Tool: bmaexecutor.ToolNameBash, Input: bmaexecutor.NewToolInput(bmaexecutor.ToolInput{Command: "ls"}),
})
mcp, _ := session.MCP(ctx, "echo") // raw MCP JSON-RPC: Write/Read
_ = session.Stop(ctx, "done")
```

- `Authenticate` sees every upgrade (control and MCP). A returned `Principal.ExecutorID` pins the identity: `resume.executor_id` must match and only that executor may open MCP channels for its sessions.
- `CallTool` is idempotent by `tool_call_id` (generated when empty): a repeat joins the in-flight call or returns the recorded result. Canceling its context stops waiting only; `CancelTool` sends `tool.cancel`.
- Resume: a disconnected executor keeps its sessions and pending calls for `LeaseTTL` (default 60s). A newer connection replaces the old one with close code 4004. Lease expiry, `executor.close` or a dropped session fails pending calls with `ErrToolOutcomeUnknown`: the tool may or may not have run.
- Pending calls live in harness memory; there is no durable result store.

## Local socket and in-process transports

`Options.Dial` defaults to WebSocket on `ws://`, `wss://` or `unix:///path.sock`.

| Shape | Executor | Harness |
| --- | --- | --- |
| Executor dials a local harness | `StaticToken("ws://127.0.0.1:port", token)` or `StaticToken("unix:///path.sock", token)` (`UnixSocketDialer(path)`) | `http.Serve(listener, harness)` on a TCP or `unix` listener |
| Harness dials the executor (executor listens) | `dial, closeFn, _ := ListenUnixSocket(path, token)` → `Options.Dial: dial` (0700 directory, 0600 socket, bearer token) | `harness.DialExecutor(ctx, path, token)`; the harness also dials the sessions' MCP channels |
| Same process | `Options.Dial: InProcessDialer(harness, principal)` | `Harness.ServeConn` (used by `InProcessDialer`) |

Details and authentication rules: [local connection](../docs/executor/local-connection.md).

## Agent executors

An executor that runs its own model loop and tools (for example a desktop app) serves the `agent` capability: the platform sends natural-language turns, the executor streams Anthropic Managed Agents events back.

```go
client.ServeAgent(myAgent, bmaexecutor.AgentCapability{
	Models:          []bmaexecutor.AgentModel{{ID: "my-model", ThinkingLevels: []string{"low", "high"}}},
	PermissionModes: []string{"default", "accept_edits"},
})
```

`myAgent` implements `AgentHandler`: `OpenTurn` reports the effective configuration, `RunTurn` does the work with `turn.Emit` (durable events), `turn.Delta` (streaming previews), `turn.Inputs()` (`turn.input`) and `turn.RequestInteraction` (approvals and questions), and `CloseSession` releases the local conversation. The SDK assigns event ids and sequences, resends unacknowledged events after a reconnect, and frames every turn with `session.status_running` and a final `session.status_idle` whose `turn_outcome` is `completed`, `canceled` or `failed`.

The harness side is `HarnessOptions.Agent` (`AgentPlatform`) plus `Executor.StartTurn`, `SendTurnInput`, `CancelTurn`, `CloseAgentSession` and `RespondInteraction`. The BMA platform side of the agent capability is not available yet; see [agent capability](../docs/executor/agent-capability.md).

## Examples

Run from this directory (`cd executor`):

| Example | What it shows | Run |
| --- | --- | --- |
| [`examples/local-brain`](examples/local-brain/main.go) | a local harness with a scripted "model" drives `Core` over loopback: bash, then the stdio [`echo-mcp`](examples/echo-mcp/main.go) server through the MCP channel | `go run ./examples/local-brain` |
| [`examples/cloud-hand`](examples/cloud-hand/main.go) | the same `Core` as a hand for the BMA cloud harness | `BMA_BASE_URL=... BMA_ENVIRONMENT_ID=... BMA_ENVIRONMENT_KEY=... go run ./examples/cloud-hand -work-root ~/bma-work` |
| [`examples/agentexecutor`](examples/agentexecutor/main.go) | the agent capability against a simulated platform, including a dropped connection | `go run ./examples/agentexecutor` |

## Tests

```sh
go vet ./... && go test -race ./...
```

The tests cover the conformance examples in [`contracts/bma/executor/examples`](../contracts/bma/executor/examples), executor ↔ harness over every transport with reconnect and lease, `Core`, capabilities and the agent capability. The TypeScript and Python packages run interop suites against the Go peers in [`internal/interop`](internal/interop).
