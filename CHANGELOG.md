# Changelog

## bma-executor-sdk v0.4.3

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.3) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.3) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.3).

Go tag `executor/v0.4.3`, npm `@beeos-ai/bma-executor-sdk@0.4.3`, PyPI
`bma-executor-sdk` 0.4.3 (tag `executor-python/v0.4.3`).

### Fixes (Go `Core`)

- **stdio MCP servers get the MCP SDKs' safe default environment.** 0.4.2
  started servers with `transport.env` only, so `npx`/`uvx`-style servers had
  no `PATH`. The server now inherits `PATH`, `SHELL`, `TERM`, `USER`,
  `LOGNAME` (the official MCP SDK `getDefaultEnvironment` list, `DenyEnv`
  filtered; the Windows list likewise) and `transport.env` overlays them.
  `HOME` stays the host's and is not inherited, nor is any `BMA_*` or other
  host variable; `secret:` refusal is unchanged.

The TypeScript and Python packages start no processes; they are released at
0.4.3 to keep the family in lockstep.

## bma-executor-sdk v0.4.2

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.2) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.2) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.2).

Go tag `executor/v0.4.2`, npm `@beeos-ai/bma-executor-sdk@0.4.2`, PyPI
`bma-executor-sdk` 0.4.2 (tag `executor-python/v0.4.2`).

### Security fixes (Go `Core`)

- **stdio MCP servers get only their configured env.** An
  environment-origin stdio MCP server now starts with exactly
  `transport.env` (protocol v1 §8.1); 0.4.0–0.4.1 passed the executor's whole
  environment minus `DenyEnv`, so host variables such as `HOME`, `BMA_HOME`
  or unrelated credentials reached the server. Bash tools, processes and
  terminals are unchanged (base env + `CoreOptions.Env` /
  `ProcessOptions.Env`, `DenyEnv` filtered).
- **`secret:` env references are refused.** `session.assign` fails with
  `-32602 invalid_params` naming the server and variable (never the value)
  when an environment-origin server's env holds a `secret:` reference, which
  the executor cannot resolve, instead of handing the literal reference to
  the server. A loader variable (`LD_PRELOAD`, …) or a `DenyEnv` name in that
  env is refused the same way, at assign time rather than on every restart.

The TypeScript and Python packages start no processes; they are released at
0.4.2 to keep the family in lockstep.

## bma-executor-sdk v0.4.1

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.1) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.1) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.1).

Go tag `executor/v0.4.1`, npm `@beeos-ai/bma-executor-sdk@0.4.1`, PyPI
`bma-executor-sdk` 0.4.1 (tag `executor-python/v0.4.1`).

### Fixes

- **Contract compatibility: unregistered capability methods answer
  `capability_unavailable`.** A harness → executor contract method behind a
  capability the executor did not register (`terminal.*`, `skill.install`,
  `workspace.backup`, the agent methods, …) now answers `-32012
  capability_unavailable` with `data.capability` set to the contract's
  `x-bma-capability` (for example `terminal.enabled`), as the contract
  requires and pre-SDK executors did. 0.4.0 answered `-32601
  method_not_found`. Methods outside the contract still answer
  `method_not_found`. Go, TypeScript and Python executors behave the same.

## v0.4.0

| Family | Released | Tags | GitHub Releases |
| --- | --- | --- | --- |
| bma-executor-sdk 0.4.0 | 2026-10-31 (UTC) | `executor/v0.4.0`, `executor-ts/v0.4.0`, `executor-python/v0.4.0` | [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.0) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.0) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.0) |
| bma-sdk 0.4.0 | 2026-10-31 (UTC) | `go/v0.4.0`, `ts/v0.4.0`, `python/v0.4.0` | [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/go%2Fv0.4.0) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/ts%2Fv0.4.0) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/python%2Fv0.4.0) |

bma-executor-sdk 0.4.0 was released first (Go tag `executor/v0.4.0`, npm
`@beeos-ai/bma-executor-sdk@0.4.0`, PyPI `bma-executor-sdk` via
`executor-python/v0.4.0`). bma-sdk 0.4.0 (`go/v0.4.0`, `ts/v0.4.0`,
`python/v0.4.0`) follows with the REST snapshot regenerated from BMA main
(beeos-ai/beeos-managed-agents@ceee7dcf).

Two SDK families now ship from this repository, generated from the same BMA
contract and versioned independently:

| Family | Go | npm | PyPI |
| --- | --- | --- | --- |
| bma-sdk (REST + façade extensions) | `github.com/beeos-ai/bma-sdks/go` (tag `go/v0.4.0`) | `@beeos-ai/bma-sdk@0.4.0` (tag `ts/v0.4.0`) | `bma-sdk` 0.4.0 (tag `python/v0.4.0`) |
| bma-executor-sdk (executor protocol) | `github.com/beeos-ai/bma-sdks/executor` (tag `executor/v0.4.0`) | `@beeos-ai/bma-executor-sdk@0.4.0` (tag `executor-ts/v0.4.0`) | `bma-executor-sdk` 0.4.0 (tag `executor-python/v0.4.0`) |

Executor contract: `bma.executor.v1.1` (contract 1.1.0), snapshot and types
generated by BMA `make contract-sdk`; `contracts/bma/executor/lock.json`
records the BMA commit (beeos-ai/beeos-managed-agents@69c05b65, #278).

### Breaking changes

- **Package split.** Executor code moved out of bma-sdk:
  - Go: `github.com/beeos-ai/bma-sdks/go/executor` → `github.com/beeos-ai/bma-sdks/executor`
    (its own module; `go get github.com/beeos-ai/bma-sdks/executor@v0.4.0`).
  - TypeScript: `@beeos-ai/bma-sdk/executor` → `@beeos-ai/bma-executor-sdk`;
    `@beeos-ai/bma-sdk` no longer depends on `ws`.
  - Python: `bma_sdk.executor` / `bma-sdk[executor]` → `bma_executor_sdk` /
    `bma-executor-sdk`.
- **Go generated types use value fields.** Optional scalars, strings and
  structs are values with `omitempty` instead of pointers
  (e.g. `ContentBlock.Text string`, `JSONRPCErrorData.Retryable bool`);
  drop `&x` / `*x` at call sites.
- **Go error codes are `Code*` untyped constants** (`CodeInvalidParams`,
  `CodeExecutorBusy`, `CodeConflict`, …) on `RPCError.Code int`; the
  `JSONRPCErrorCode*` names are gone.
- **Go protocol constant `ProtocolV11`** (`"bma.executor.v1.1"`) replaces
  `ProtocolVersionV11`. Go clients negotiate v1.1 by default
  (`Options.Protocol`); a harness still accepts v1.
- **Go `ToolCallParams.Input` is `json.RawMessage`.** Decode the built-in
  shape with `params.DecodeInput()`; build it with
  `NewToolInput(ToolInput{...})`.
- **Go capabilities type is `Capabilities`** (was `ExecutorCapabilities`),
  including `Agent`, `Process`, `Terminal`, `Skills`, `Workspace`.
- **TypeScript JSON-RPC error codes use semantic names**
  (`JsonRpcErrorCode.ExecutorBusy`, `.CapabilityUnavailable`, …) generated
  from the contract's `x-bma-enum-names`; numeric `Value32009`-style names are
  gone. Python `JsonRpcErrorCode` uses the same names.
- `session.event_delta.index` is required on the wire (an index of 0 was
  previously dropped).

### New

- **Harness end** in Go, TypeScript and Python: accept executors, assign
  sessions, call/cancel tools, MCP channels (`Harness`).
- **Agent capability** (`bma.executor.v1.1`, capability `agent`): executor
  `ServeAgent`/`serve_agent` with turns, inputs, cancel, durable ordered
  session events with resend after reconnect, transient deltas,
  interactions; harness-side `AgentPlatform`. Machine-scoped executors
  (machine token via `TokenSource`, no environment id).
- **Go execution `Core`** (bmad runs on it): bash, file tools
  (read/write/edit/glob/grep), stdio MCP supervisor, cleanup, multi-harness
  isolation, registrable capabilities (`process.*` with PTY / file transfer /
  Git credential hooks, `skill.install`, custom). `DenyEnv` keeps
  executor-private variables (e.g. machine tokens) out of bash, processes,
  MCP servers and terminals.
- **Local transports**: WebSocket over a Unix domain socket (`unix://`) or
  Windows named pipe (`npipe://`, TypeScript), executor-listens local socket,
  in-process pipe; MCP channels on every transport.
- Cross-language conformance and interop suites (Go ↔ TypeScript,
  Go ↔ Python, TypeScript ↔ Python over `ws://` and `unix://`).

### bma-sdk REST and façade (contract at BMA ceee7dcf)

Breaking:

- The machine-daemon runtime operations were removed upstream
  (`registerMachineDaemonRuntime`, `endMachineDaemonRuntime`,
  `sleepMachineDaemonRuntime`, `socketMachineDaemonRuntime`) together with the
  `DaemonRuntime*` and `Process*Reconciliation*` schemas.
- `AgentSession.status` is required: `idle`, `running`, `rescheduling` or
  `terminated` (Go `AgentSessionStatus*` constants).
- Python: the REST `File` schema is generated as `ProjectFile`
  (`bma_sdk.rest.models.ProjectFile`), so it no longer shadows the generator's
  `bma_sdk.rest.types.File` upload payload; `upload_file` previously crashed
  parsing its own response.

New REST operations:

- Files API: `uploadFile`, `listFiles`, `getFile`, `getFileContent`,
  `deleteFile`; session resources `addSessionResource`,
  `listSessionResources`, `deleteSessionResource`; artifacts
  `uploadSessionArtifact`, `listSessionArtifacts`,
  `getSessionArtifactContent`, `promoteSessionArtifact`.
- Agent machines: `createAgentPersistentMachine`, `listAgentMachines`,
  `deleteAgentMachine`; `resumeMachine`; backups `createMachineBackup`,
  `listMachineBackups`, `restoreMachineFromBackup`.
- `cancelAgentSession`.
- Machine-daemon: session files/outputs and machine backup archive/restore
  endpoints.

Façade extensions: `POST/GET /v1/sessions/{id}/resources` and
`DELETE /v1/sessions/{id}/resources/{resource_id}` (`AddSessionResource`,
`ListSessionResources`, `DeleteSessionResource`), session files download;
`/v1/files` is the standard Anthropic surface. README shows upload → mount
into a session → list outputs in all three languages.

### Behavior notes for bmad / BMA

- `tool.cancel` results follow the SDK shape.
- `BMA_HOME` stays visible to bash and `process.*` (skills paths depend on
  it); `BMA_MACHINE_TOKEN` and other internal `BMA_*` variables are hidden
  from bash, processes, stdio MCP and PTY terminals.
