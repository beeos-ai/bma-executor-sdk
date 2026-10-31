# Quick start and examples

[中文](quickstart.zh-CN.md) · [Overview](README.md)

| Example | What it does | Needs | Status |
| --- | --- | --- | --- |
| 1A: cloud brain → bmad (local hand) | platform agents run `run_command`/process tools/terminals/skills/file transfers on your computer | bmad ≥ 0.2.11, machine token | [Available] |
| 1B: cloud brain → SDK hand (`Core`) | the platform dispatches bash/file tools via `tool.call`, plus stdio MCP | selfhost environment + key; Go SDK 0.4.0 | [Available] (the platform does not dispatch MCP servers yet [Coming]) |
| 2: local brain → local hand → events to the platform | the brain drives the same core over loopback; agent events go to the platform | SDK 0.4.0 (Go, TypeScript, Python); platform agent capability | local part [Available]; platform part [Coming], use the simulated platform |

---

## 0. Pairing and installation

### 0.1 Create the machine and get its token

Any of:

- Dashboard: **All projects** → **Machines** → **Connect machine**; the name is the `machine_name` agent configs use; optionally share with projects; the next screen shows the token and install command.
- CLI: `npx @beeos-ai/bma-cli machines create-local --display-name my-mac --project-ids "$PROJ"` (create + install), or `machines create … --json` for `token` and `install_command` to run elsewhere.
- API: the connect-a-BYO-machine endpoint.

The token is shown once; store it as a secret. Grant the machine to the projects whose agents should use it.

### 0.2 Option A: install bmad

```sh
curl -fsSL https://api.bma.beeos.ai/install/bmad.sh | sh   # paste the machine token when prompted
```

Installs under `~/.bmad` as a user service (systemd/launchd). The **default service still runs the legacy daemon socket**; Executor Protocol mode is explicit:

```sh
bmad stop                                   # exclusive with daemon mode (shared BMA_HOME lock)
export BMA_MACHINE_TOKEN=…                  # read from secure storage, do not echo
bmad executor --executor-url https://api.bma.beeos.ai/api --work-root "$HOME/work"
```

- `--executor-url` is the origin including `/api`.
- Omitting `--environment` selects the machine path; `--work-root` bounds process working directories (default: current directory).
- Pooled cloud machines start this automatically with `BMAD_LAUNCH_MODE=executor`.

### 0.3 Option B: embedded in the desktop app

Get the machine token with the same flow, keep it in secure storage (for example the OS keychain), and use the SDK executor end against `…/api/v1/executors/token` with `environment_id: ""`. **Do not** install or run bmad. The machine path requires `process.enabled` and `process.*`: with the Go SDK register `NewProcessCapability` on the `Core` (see [overview, option B](README.md#option-b-the-desktop-app-embeds-the-hand)); with TypeScript or Python implement `process.*` with `onRequest` / `on_request`, or integrate over the environment path (1B).

---

## 1A. Cloud brain → bmad

1. Start `bmad executor` (0.2). `executor registered executor_id=exec_… connection_id=…` in the log means it registered.
2. In a project the machine is granted to, reference it:

   ```yaml
   machine_sources:
     - machine_name: my-mac
       cwd: /Users/me/work
   ```

3. Give the agent a task (for example "run the tests"). `run_command` becomes `process.start` (`stderr:"merge"`), output is read with `process.read`, and process actions (read, stdin write, interrupt, terminate) map to `process.read`/`process.write`/`process.interrupt`/`process.terminate`.
4. Network loss: bmad reconnects with `resume.active_sessions`; the platform reconciles with `process.list`: running processes continue, processes that ended meanwhile report their final state, lost ones become unknown and are not re-run.

Not supported yet: stdio MCP / CUA tools through bmad (the platform does not dispatch MCP servers to machine executors).

## 1B. Cloud brain → SDK hand (`Core`) [Available]

BMA provisions the selfhost environment and environment key for you; ask your BMA contact.

```sh
cd bma-sdks/executor
BMA_BASE_URL=https://api.<env>.bma.beeos.ai \
BMA_ENVIRONMENT_ID=shenv_… BMA_ENVIRONMENT_KEY=… \
go run ./examples/cloud-hand -work-root ~/bma-work
```

Core of [`examples/cloud-hand`](../../executor/examples/cloud-hand/main.go):

```go
core, _ := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: workRoot})
client, _ := bmaexecutor.NewClient(bmaexecutor.Options{
	BaseURL: baseURL, EnvironmentID: envID, EnvironmentKey: envKey,
	Executor:     bmaexecutor.ExecutorInfo{Name: "my-hand", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"},
	Capabilities: core.Capabilities(),
})
_ = core.Attach("cloud", client)
registered, _ := client.Start(ctx)
```

The platform dispatches one-shot sessions per work item: `session.assign` → `session.ready` → `tool.call` (`bash`/`read`/`write`/`list_files`) → `tool.result` → `session.ended`. `Core` starts stdio servers listed in `session.assign.mcp_servers[]` with `connection_origin:"environment"` and bridges them to MCP channels (the platform does not send `mcp_servers` yet [Coming]).

Without `Core`, implement execution with `client.OnToolCall(handler)`, kill the process group when the handler's `ctx` is canceled, call `SessionReady` after setup and `SessionEnded` after every child exited (see the [Go executor README](../../executor/README.md#executor-quick-start)).

## 2. Local brain → local hand → events to the platform

### 2.1 Local brain drives the local hand (runs offline) [Available]

```sh
cd bma-sdks/executor
go run ./examples/local-brain -mcp-command "go run ./examples/echo-mcp"
```

Flow ([source](../../executor/examples/local-brain/main.go)): the brain serves `Harness` on `127.0.0.1:0` with a per-run token; the executor's `Core` attaches a `Client` with `StaticToken`; the brain runs `WaitExecutor` → `AssignSession` (with a stdio MCP server) → `Ready` → `CallTool(bash)` ×2 → `session.MCP("echo")` raw MCP JSON-RPC → `Stop`. Replace the script with your model loop. Auth and sharing rules: [local connection](local-connection.md). TypeScript: [`harness-loopback`](../../executor/typescript/examples/harness-loopback/main.mjs); Python: [`harness_loopback.py`](../../executor/python/examples/harness_loopback.py).

### 2.2 Agent events to the platform [SDK Available; platform Coming]

```sh
cd bma-sdks/executor
go run ./examples/agentexecutor
```

A simulated platform walks the whole lifecycle: `turn.start` → streamed `session.event_delta` and durable `session.event` → `interaction.request` → `interaction.response` → result → `turn.input` → resend by sequence after a dropped connection → `turn.cancel` → `session.close`. You implement `AgentHandler`, see [agent capability](agent-capability.md#6-sdk-usage). TypeScript and Python equivalents: [`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs), [`agent_minimal.py`](../../executor/python/examples/agent_minimal.py).

Once the platform implements the agent capability, the same program points its credentials at the platform (machine token); clients subscribe to the platform session event stream (SSE).

---

## 3. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| token endpoint `401` | machine token / environment key invalid or revoked | create a new token; the executor should stop reconnecting |
| token endpoint `404` | empty `environment_id` on a server that does not serve machine executors (for example managed-agents instead of bma-api), or environment outside the credential | use `https://<host>/api` for the machine path; check the environment |
| upgrade `400 missing … subprotocol` | no `Sec-WebSocket-Protocol: bma.executor.v1.1` (or v1) | set the subprotocol |
| upgrade `403 … does not allow channel mcp` | machine-path JWTs only allow `control` | no MCP channel on the machine path |
| close `4003` right after register | `executor.register.protocol` ≠ negotiated subprotocol | use `bma.executor.v1.1` for both |
| close `4002` after register | machine path: no `capabilities.process.enabled`; `executor.version` not `MAJOR.MINOR.PATCH` (or `0.0.0-dev`); or the runtime was ended | advertise the capability; fix the version; reconnect |
| periodic close `4001` | 5-minute JWT expired | expected: new token, reconnect |
| repeated close `4004` | two executors on one computer (bmad + desktop, or two bmads) | keep one; the side that gets 4004 must stop |
| `bmad executor cannot start: the bmad daemon (or another executor) is running` | daemon mode is running | `bmad stop` first |
| `BMA_MACHINE_TOKEN is required` | `bmad executor` reads the token only from the environment | set the variable |
| `-32009 executor_busy` | capacity (sessions/processes/reads) | back off per `data.retryable`; never drop results |
| `-32012`, `data.capability:"tool:<name>"` | the executor does not serve that tool | implement and advertise it, or use another tool |
| `-32013` `process_id_spec_mismatch` | same `process_id`, different spec | new process, new id |
| `-32007 session_not_active` | session ended, unassigned, or another harness's | `session.assign` again |
| platform: "executor did not merge stderr into stdout" | `process.start` result did not echo `stderr:"merge"` | implement and echo it |
| a call timed out and you do not know whether it ran | transport failure = unknown outcome | do not blindly resend side effects; confirm with `process.list`/reconciliation |
