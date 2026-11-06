# bma-executor-sdk

Go, TypeScript and Python SDKs for the BMA (BeeOS Managed Agents) Executor
Protocol `bma.executor.v1.1`: the executor, harness and agent ends, with
protocol types generated from the contract in
[`contracts/bma/executor`](contracts/bma/executor) and handwritten runtimes.
bmad's execution model and the BMA server speak the same contract.

> Moved from [`beeos-ai/bma-sdks`](https://github.com/beeos-ai/bma-sdks) (archived) in 0.4.9.
> The Go import path changed from `github.com/beeos-ai/bma-executor-sdk` to
> `github.com/beeos-ai/bma-executor-sdk`; the npm and PyPI package names are unchanged.
> The REST / façade client is [`beeos-ai/bma-sdk`](https://github.com/beeos-ai/bma-sdk).

| Language | Package | Install |
| --- | --- | --- |
| Go | [![Go Reference](https://pkg.go.dev/badge/github.com/beeos-ai/bma-executor-sdk.svg)](https://pkg.go.dev/github.com/beeos-ai/bma-executor-sdk) `github.com/beeos-ai/bma-executor-sdk` (package `bmaexecutor`, this directory) | `go get github.com/beeos-ai/bma-executor-sdk@v1.0.0` |
| TypeScript | [![npm](https://img.shields.io/npm/v/@beeos-ai/bma-executor-sdk.svg)](https://www.npmjs.com/package/@beeos-ai/bma-executor-sdk) [`@beeos-ai/bma-executor-sdk`](typescript) | `npm install @beeos-ai/bma-executor-sdk@1.0.0` |
| Python | [![PyPI](https://img.shields.io/pypi/v/bma-executor-sdk.svg)](https://pypi.org/project/bma-executor-sdk/) [`bma-executor-sdk`](python) (`bma_executor_sdk`) | `pip install bma-executor-sdk==1.0.0` |

| | |
| --- | --- |
| Version | 1.0.0 (tag `v1.0.0`, all three languages), see [CHANGELOG](CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (the harness also accepts `v1`; contract 1.1.0, [`lock.json`](contracts/bma/executor/lock.json)) |
| Runtimes | Go 1.24+, Node.js 20+ (ESM), Python 3.10+ |
| Integration guide | [docs/executor](docs/executor/README.md) ([中文](docs/executor/README.zh-CN.md)) |

| I want to | Start here |
| --- | --- |
| Understand roles, deployment shapes and what is available | [Overview](docs/executor/README.md) ([中文](docs/executor/README.zh-CN.md)) |
| Write an executor (a "hand" the BMA cloud or a local brain drives) | [Quick start](docs/executor/quickstart.md) ([中文](docs/executor/quickstart.zh-CN.md)), then the [protocol reference](docs/executor/protocol-reference.md) ([中文](docs/executor/protocol-reference.zh-CN.md)) |
| Drive an executor (be the harness / local brain) | [Local connection and auth](docs/executor/local-connection.md) ([中文](docs/executor/local-connection.zh-CN.md)) and `Harness` below and in the [TypeScript](typescript/README.md) and [Python](python/README.md) READMEs |
| Connect a full local agent (turns in, agent events out) | [Agent capability](docs/executor/agent-capability.md) ([中文](docs/executor/agent-capability.zh-CN.md)) |

The rest of this README is the Go module; see [`typescript/README.md`](typescript/README.md) and [`python/README.md`](python/README.md) for the other languages.

## Executor quick start

An executor registers with BMA (or a local harness) and answers tool calls:

```go
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func main() {
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL:        os.Getenv("BMA_BASE_URL"),
		EnvironmentKey: os.Getenv("BMA_ENVIRONMENT_KEY"),
		EnvironmentID:  os.Getenv("BMA_ENVIRONMENT_ID"),
		Executor:       bmaexecutor.ExecutorInfo{Name: "my-executor", OS: "linux", Arch: "arm64", Version: "0.1.0"},
	})
	if err != nil {
		log.Fatal(err)
	}
	// One typed slot per built-in tool; only registered tools are advertised.
	client.Tools.Bash.Handle(func(ctx context.Context, in bmaexecutor.BashInput, tool bmaexecutor.ToolContext) (bmaexecutor.ToolOutput, error) {
		out, err := exec.CommandContext(ctx, "bash", "-lc", in.Command).Output()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			return bmaexecutor.ToolOutput{}, err // outcome failed
		}
		return bmaexecutor.BashResult{Stdout: string(out), ExitCode: code}.Output(), nil
	})
	if _, err := client.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	select {}
}
```

Set `Options.BaseURL` to your BMA deployment: the hosted service is in early access and the default `DefaultBaseURL` (`https://api.bma.beeos.ai`) does not resolve yet. It is the deployment origin, the same value as `BMA_BASE_URL` in bma-sdk (for example `https://api.staging.bma.beeos.ai`); the SDK posts the environment key to `{BaseURL}/v1/executors/token` and uploads artifacts below it, and drops a trailing `/api/v1` (the 0.x bma-sdk API-root form) instead of nesting the paths. The SDK does not read `BMA_BASE_URL` itself; the example passes it through. A `TokenSource` (for example `StaticToken`) carries its own channel URL.

A machine-scoped executor uses a machine token through `Options.TokenSource` and no environment id. Go clients negotiate `bma.executor.v1.1` by default (`Options.Protocol`).

Executor types follow one mapping: required members are values, optional members are values with `omitempty` (named structs and nullable members are pointers), enums are untyped constants (`Code*` error codes on `RPCError.Code`), and opaque payloads such as `ToolCallParams.Input` stay `json.RawMessage`. Tool handlers receive the decoded per-tool input (`BashInput`, `ReadInput`, ...) through `client.Tools.<Name>.Handle`; return `TextOutput`, `BashResult{...}.Output()` (rendered like `Core`: stdout, stderr, `exit_code: N`) or a `ToolOutput`, `NewToolError(text)` for a tool-level error the model sees (`is_error: true`), any other error for `outcome: "failed"`. The harness calls them as `session.Tools.Bash.Call(ctx, BashInput{Command: "ls"}, CallToolOptions{})`. Other protocol methods are typed too: `Requests.ProcessStart.Handle(client, fn)`, `Calls.SessionEvent.Call(ctx, client, params)`, `Notifications.ToolProgress.Notify(ctx, client, params)` (tables generated from the contract by `tools/gen_methods.py`).

### Lifecycle, cancellation, and results

- `tool.cancel` from the harness cancels the `context.Context` passed to the tool handler. The client responds `{"canceled":true}` and reports the result with `outcome: "canceled"`; handlers must stop their process group when the context is done.
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
- **stdio MCP**: every `session.assign.mcp_servers[]` entry with `connection_origin: environment` starts one process per (brain, session, `server_key`), verifies `command_sha256` when given, runs the command without a shell with only its configured `transport.env` plus `transport.secret_env` (server-resolved secret values, advertised as `capabilities.mcp.secret_env`, injected into that process only; nothing is inherited from the executor; a `secret:` value left in env, a loader variable or a `DenyEnv` name fails `session.assign` with `invalid_params`), and bridges stdin/stdout lines to that server's MCP channel. If either side ends both are restarted, until the session ends, with jittered exponential backoff capped at 30s (reset once a channel stays open for 1s); after 5 consecutive failures the core sends `session.status` `error` / `mcp_unavailable: <key>` once per outage and keeps retrying.
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

`Options.Dial` defaults to WebSocket on `ws://`, `wss://`, `unix:///path.sock` or `npipe:////./pipe/<name>` (Windows; `ListenNamedPipe`, `NamedPipeListener`, `NamedPipeDialer`).

| Shape | Executor | Harness |
| --- | --- | --- |
| Executor dials a local harness | `StaticToken("ws://127.0.0.1:port", token)` or `StaticToken("unix:///path.sock", token)` (`UnixSocketDialer(path)`) or `StaticToken("npipe:////./pipe/<name>", token)` (`NamedPipeDialer(name)`) | `http.Serve(listener, harness)` on a TCP or `unix` listener |
| Harness dials the executor (executor listens) | `dial, closeFn, _ := ListenUnixSocket(path, token)` → `Options.Dial: dial` (0700 directory, 0600 socket, bearer token) | `harness.DialExecutor(ctx, path, token)`; the harness also dials the sessions' MCP channels |
| Same process | `Options.Dial: InProcessDialer(harness, principal)` | `Harness.ServeConn` (used by `InProcessDialer`) |

Details and authentication rules: [local connection](docs/executor/local-connection.md).

## Agent executors

An executor that runs its own model loop and tools (for example a desktop app) serves the `agent` capability: the platform sends natural-language turns, the executor streams Anthropic Managed Agents events back.

```go
client.ServeAgent(myAgent, bmaexecutor.AgentCapability{
	Models:          []bmaexecutor.AgentModel{{ID: "my-model", ThinkingLevels: []string{"low", "high"}}},
	PermissionModes: []string{"default", "accept_edits"},
})
```

`myAgent` implements `AgentHandler`: `OpenTurn` reports the effective configuration, `RunTurn` does the work with `turn.Emit` (durable events), `turn.Delta` (streaming previews), `turn.Inputs()` (`turn.input`) and `turn.RequestInteraction` (approvals and questions), and `CloseSession` releases the local conversation. The SDK assigns event ids and sequences, resends unacknowledged events after a reconnect, and frames every turn with `session.status_running` and a final `session.status_idle` whose `turn_outcome` is `completed`, `canceled` or `failed`.

The harness side is `HarnessOptions.Agent` (`AgentPlatform`) plus `Executor.StartTurn`, `SendTurnInput`, `CancelTurn`, `CloseAgentSession` and `RespondInteraction`. The BMA platform side is in preview: bind an agent to the machine with `PUT /orgs/{orgID}/projects/{projectID}/agents/{agentID}/executor-binding`; its sessions accept text input only. See [agent capability](docs/executor/agent-capability.md#7-platform-preview).

## Examples

Run from the repository root:

| Example | What it shows | Run |
| --- | --- | --- |
| [`examples/local-brain`](examples/local-brain/main.go) | a local harness with a scripted "model" drives `Core` over loopback: bash, then the stdio [`echo-mcp`](examples/echo-mcp/main.go) server through the MCP channel | `go run ./examples/local-brain` |
| [`examples/cloud-hand`](examples/cloud-hand/main.go) | the same `Core` as a hand for the BMA cloud harness | `BMA_BASE_URL=... BMA_ENVIRONMENT_ID=... BMA_ENVIRONMENT_KEY=... go run ./examples/cloud-hand -work-root ~/bma-work` |
| [`examples/agentexecutor`](examples/agentexecutor/main.go) | the agent capability against a simulated platform, including a dropped connection | `go run ./examples/agentexecutor` |

## Tests

```sh
go vet ./... && go test -race ./...
```

The tests cover the conformance examples in [`contracts/bma/executor/examples`](contracts/bma/executor/examples), executor ↔ harness over every transport with reconnect and lease, `Core`, capabilities and the agent capability. The TypeScript and Python packages run interop suites against the Go peers in [`internal/interop`](internal/interop).

## Development

The executor contract snapshot and the generated types (`generated.go`,
`typescript/src/generated/executor.ts`, `python/src/bma_executor_sdk/generated.py`)
are generated in the BMA repository: `make contract-sdk SDK_DIR=<this checkout>`.
`tools/check_contract.py` verifies them against `contracts/bma/executor/lock.json`;
`--source <beeos-managed-agents checkout>` also compares the snapshot with BMA.

```sh
python3 tools/check_contract.py [--source ../beeos-managed-agents]
go vet ./... && go test -race ./...
(cd python && uv sync && uv run pytest -q)                        # interop tests need Go
(cd typescript && npm ci && npm run build && npm test)            # Go + Python interop
```

## Release

One tag `v<version>` releases all three languages at the same version: it must
be on `main` and match `typescript/package.json` and `python/pyproject.toml`.
[`release.yaml`](.github/workflows/release.yaml) first runs
[`ci.yaml`](.github/workflows/ci.yaml), then publishes.

| Channel | Publishes | Credentials |
| --- | --- | --- |
| Go | nothing; the module comes from the tag | none |
| npm | `@beeos-ai/bma-executor-sdk` with provenance | secret `NPM_TOKEN` in the `releases` environment |
| PyPI | `bma-executor-sdk` | a PyPI Trusted Publisher on `bma-executor-sdk`: owner `beeos-ai`, repository `bma-executor-sdk`, workflow `release.yaml`, environment `releases` |

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
