# BMA executor integration guide (overview)

[中文](README.zh-CN.md) · [Repository README](../../README.md)

Connect your machine (hands) or your own agent (brain) to BMA — the executor, harness and agent ends of the `bma.executor` protocol. [`bma-sdk`](https://github.com/beeos-ai/bma-sdk) drives and uses agents (REST / façade client); `bma-executor-sdk` connects hands or brains to BMA.

The entry point for integrators who build an executor (a "hand"), drive one (a "brain" / harness), or connect a complete local agent to BMA: what the Executor Protocol is, the deployment shapes, how the SDK is split, how to connect, and what works today.

| Document | Content |
| --- | --- |
| This page | Roles, deployment shapes, "one executor per computer", SDK layout and transports, capability status |
| [Protocol reference v1.1](protocol-reference.md) | Connection and auth, every method/param/error, capability bits, idempotency and reconnect, cancellation levels, limits |
| [Agent capability](agent-capability.md) | Local full-agent integration: `turn.*`, `session.close`, agent events, ACK/resend, interactions |
| [Local connection and auth](local-connection.md) | Local brain ↔ local hand transports, tokens, rules for several harnesses |
| [Quick start and examples](quickstart.md) | Pairing/install, example 1 (cloud brain → hand), example 2 (local brain → local hand → events to the platform), troubleshooting |

Status labels used across these documents:

- **[Available]**: shipped in bma-executor-sdk 1.0.0 or live on the BMA platform.
- **[Preview]**: live on the BMA platform with the limits stated where it is used; may change before general availability.
- **[Coming]**: defined in the contract but not served yet; capability-gated, so a peer that does not advertise the capability bit never receives those methods.
- **[Design]**: designed, not implemented.

> Baseline: bma-executor-sdk **1.0.0**, protocol `bma.executor.v1.1` (contract 1.1.0, [`contracts/bma/executor`](../../contracts/bma/executor)).

---

## 1. What the Executor Protocol is

```text
             Executor Protocol (JSON-RPC 2.0)
 harness (brain) ◀────────────────────────▶ executor (hand)
 assigns sessions, calls tools, reads output    runs tools/processes/PTYs, reports results and events
```

- In scope: registration and capability negotiation, session assignment, tool calls, processes and PTYs, machine services (files, credentials, skills), cancellation, the MCP channel, reconnect, event reporting.
- Out of scope: how the brain thinks (model calls, planning, tool choice).
- Wire: JSON-RPC 2.0 over one WebSocket, subprotocol `bma.executor.v1` / `bma.executor.v1.1`. No CloudEvents, no custom envelope.
- Single source of truth: the machine-readable contract in this repository, [`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml), [`executor.asyncapi.yaml`](../../contracts/bma/executor/executor.asyncapi.yaml) and [`schemas/`](../../contracts/bma/executor/schemas), with conformance examples in [`examples/conformance.json`](../../contracts/bma/executor/examples/conformance.json). The BMA server, bmad and all three SDKs are generated from or tested against it.

### 1.1 Roles are relative

| Role | Does | Examples |
| --- | --- | --- |
| harness | sends `session.assign`, `tool.call`, `process.*`, `terminal.*`; receives `tool.result`, `session.event` | the BMA platform, your local brain (SDK `Harness`) |
| executor | registers, heartbeats, executes, reports | `bmad executor`, the selfhost worker, a process built on SDK `Client`/`Core` |
| agent provider | an executor advertising the `agent` capability: takes natural-language turns, streams full agent events | your local agent behind an executor (SDK [Available]; platform [Preview]) |

**Whoever directs is the harness.** The same local brain is an agent provider towards the platform and a harness towards the local hand.

### 1.2 Who dials is independent of the role

- Cloud: the executor is always the WebSocket **client** and connects outbound (works behind NAT; the platform never dials in).
- Local, executor dials: the local brain serves SDK `Harness` on loopback TCP or a local socket and the executor attaches a `Client` that dials it [Available].
- Local, harness dials: the executor listens on a Unix domain socket (or a Windows named pipe in Go and TypeScript) with a per-start token and the brain dials it with `Harness.DialExecutor` / `dialExecutor`; roles stay harness/executor [Available in the Go and TypeScript SDKs; bmad does not listen yet]. See [local connection](local-connection.md).

---

## 2. Deployment shapes

| Shape | Executor process | Credential | Connects to | Status |
| --- | --- | --- | --- | --- |
| Cloud sandbox (pooled machines) | the machine template runs `bmad executor` | machine daemon token | BMA API `/api/v1/executors/*` | [Available] |
| BYO machine | `bmad`: the default service still runs the legacy daemon socket; `bmad executor` is the explicit Executor Protocol entry point | machine daemon token | BMA API `/api/v1/executors/*` | [Available] |
| Embedded in a desktop app (option B) | the app is the executor via the SDK (`Client` + `Core` + `NewProcessCapability`) | machine token from pairing | BMA API | [Available] with the Go SDK |
| Selfhost worker | the selfhost worker | environment key | Managed Agents API `/v1/executors/*` | [Available] |

Two server paths:

- **Machine path**: the BMA API at `/api/v1/executors/{token,channel}`. A machine-token JWT only allows the `control` channel; the connection becomes the machine's daemon runtime and platform `run_command`/process tools run through `process.*`.
- **Environment path (selfhost)**: the Managed Agents API at `/v1/executors/*`. An environment-key JWT allows `control` + `mcp`; the work queue dispatches "one work item = one one-shot session + one `tool.call`".

---

## 3. One executor per computer

**Exactly one executor process per computer connects to the platform, with that computer's machine identity.** The platform enforces it:

- A machine token always yields executor id `exec_<machine_id>`. When a new connection with the same id registers, the old one is closed with `4004`; on `4004` an executor must stop and not reconnect.
- On the machine path a new registration replaces the machine's current daemon runtime (legacy socket or older executor).
- `bmad executor` and the `bmad` daemon share one runtime lock and never run together.

Two compliant ways to run a local brain plus a local hand:

### Option A (recommended): bmad is the hand

```text
platform (harness)
   │ Executor Protocol / WSS, outbound from bmad, machine identity
   ▼
bmad (the only executor on this computer)
   ├─ hand: process.* / terminal.* / skill.install / file transfer / Git credentials ...... [Available]
   └─ agent: forwards platform turns to the local brain, events back .................... [Coming]
          ▲
          │ Executor Protocol / local transport
          ▼
your local brain
   ├─ as harness: drives bmad's tools locally, no cloud round trip ...................... [Coming in bmad]
   └─ as agent provider: takes platform turns, produces agent events ................... [Preview on the platform]
```

Today bmad holds only its outbound platform connection; it does not accept local brains or forward the agent capability yet. The SDK pieces this needs (`ListenUnixSocket`, `Harness.DialExecutor`, the agent capability) ship in 1.0.0.

### Option B: the desktop app embeds the hand

The app is the executor (SDK executor end), pairs this computer and holds its machine token; **bmad does not run on that computer** (otherwise they replace each other with `4004`). The brain drives the same `Core` in process (`InProcessDialer`) or over a local socket.

**The machine path requires `process.enabled` (or the `agent` capability): an executor advertising neither is refused: `executor.register` is answered, then the connection is closed with `4002` "runtime registration refused", on every attempt.** The SDKs end such a client with a configuration error after 3 refusals in a row (Go `ErrRegistrationRefused`, TypeScript `RegistrationRefusedError`, Python `RegistrationRefusedError`) and report ready (`WaitReady` / `waitReady` / `wait_ready`) only after the registration stayed open for one heartbeat interval; see the language READMEs for a minimal example. The Go SDK provides it: register `NewProcessCapability(core, ProcessOptions{...})` on the `Core` (PTY, file transfer and Git credential hooks included); Go clients negotiate `bma.executor.v1.1` by default. TypeScript and Python have no `Core` or `process.*` helper; serve `process.*` yourself with `onRequest` / `on_request`, or integrate over the selfhost environment path (`tool.call` mode), see [quick start](quickstart.md).

The platform cannot tell the two options apart: it sees one `exec_<machine_id>`.

---

## 4. SDK layout

Named after the protocol, split by role. Go is the reference implementation (`github.com/beeos-ai/bma-executor-sdk`); TypeScript and Python provide the same three ends.

```text
bma-executor-sdk (Go)
├── executor end   Client     token exchange (environment key or machine token), register, heartbeat, reconnect with resume,
│                             sessions, tool.call/cancel, result retries, artifact upload, MCP channel
│   └── Core                  brain-independent execution core: bash, read/write/edit/glob/grep, stdio MCP supervisor, cleanup,
│                             multi-harness isolation, registrable capabilities (process.*, skill.install, custom)
├── harness end    Harness    http.Handler that accepts executors and drives sessions, tool calls, MCP channels; reconnect and lease
└── agent          ServeAgent / AgentHandler (executor side), AgentPlatform / StartTurn ... (harness side)
```

| Part | Go [`executor`](../../README.md) | TypeScript [`@beeos-ai/bma-executor-sdk`](../../typescript) | Python [`bma-executor-sdk`](../../python) |
| --- | --- | --- | --- |
| Executor end (`Client` / `ExecutorClient`) | [Available] | [Available] | [Available] |
| Execution `Core` (bash, file tools, stdio MCP) | [Available] | not provided | not provided |
| `process.*` and `skill.install` helpers | [Available] | not provided (`onRequest`) | not provided (`on_request`) |
| `terminal.*` helper | not provided (register your own `Capability`) | not provided | not provided |
| Harness end (`Harness`) | [Available] | [Available] | [Available] |
| Agent capability (both sides) | [Available] | [Available] | [Available] |

- The SDK does not contain the agent loop; bring your own. The agent module is glue: event ids/ACK/resend, running/idle framing, interaction waits.
- Protocol types in all three languages are generated from [`contracts/bma/executor`](../../contracts/bma/executor); [`lock.json`](../../contracts/bma/executor/lock.json) records the contract version and hashes.
- bmad is built on the Go SDK: the BMA backend's `internal/machineexecutor` imports `github.com/beeos-ai/bma-executor-sdk` and adds the machine-specific parts (machine token, file transfers, Git credentials, skill installs, `terminal.*`); it reaches `bmad` through `internal/bmad` and `cmd/daemon`. Other implementations (the BMA server, the selfhost worker) are held to the same contract by cross-implementation conformance tests.

### 4.1 Transports

| Transport | Use | Auth | Status |
| --- | --- | --- | --- |
| Outbound WebSocket (WSS) | executor → platform | machine token / environment key exchanged for a short-lived JWT | [Available] |
| WebSocket over loopback TCP | local brain (`Harness`) ↔ local hand (`Client` + `Core`) | `StaticToken` + `HarnessOptions.Authenticate` | [Available] |
| WebSocket over a Unix domain socket (`unix://`), executor dials | brain listens, hand dials | 0700 dir + 0600 socket + local token | [Available] Go, TypeScript, Python |
| WebSocket over a Unix domain socket, harness dials | hand listens (`ListenUnixSocket` / `listenExecutor`), brain dials (`DialExecutor` / `dialExecutor`) | 0700 dir + 0600 socket + per-start token | [Available] Go, TypeScript |
| Windows named pipe (`npipe://`), executor dials | brain listens, hand dials | current-user pipe + local token | [Available] Go, TypeScript, Python (Windows) |
| Windows named pipe, harness dials | hand listens (`ListenNamedPipe` / `listenExecutor`), brain dials | current-user pipe + per-start token | [Available] Go, TypeScript |
| In process | brain and hand in one process | none | [Available] (`InProcessDialer`, `inProcessDialer`, `inprocess_dialer`) |

Protocol messages are the same on every transport.

---

## 5. Capability status

| Capability | Protocol | Platform | bmad | Go SDK 1.0.0 / 1.1.0 |
| --- | --- | --- | --- | --- |
| register/heartbeat/reconnect/drain/4004 | v1 | available | available | available (both ends) |
| `tool.call` (bash, file tools) | v1 | environment path; the machine path does not use `tool.call` | `bash` only | `Core`: bash/read/write/edit/glob/grep |
| `process.*` | v1.1 | available (the machine tool path) | available | available (`NewProcessCapability`) |
| `terminal.*` (PTY) | v1.1 | available | available (not on Windows) | no helper |
| `skill.install` | v1.1 | available | available (machine path) | available (`NewSkillInstallCapability`) |
| file transfer / Git credentials (`process.start` fields) | v1.1 | available | available (machine path) | available (hooks in `ProcessOptions`) |
| artifact upload `POST /v1/executors/artifacts` | v1.1 | environment path | environment path | available (`Client.UploadArtifact`) |
| MCP channel (`channel=mcp`) | v1 | server channel available (environment path); the platform harness does not dispatch MCP servers to executors in production yet | no | `OpenMCPChannel` and the `Core` stdio supervisor |
| session files (`/mnt/session/uploads\|outputs`) | machine-token HTTP routes | available | available | — |
| executor-declared session files root (`capabilities.session_files.root`, `<root>/<session>/{uploads,outputs}`) | v1.1 additive | pending (#411 F2) | no (keeps `/mnt/session`) | 1.1: `SessionFiles` in `ProcessOptions` (TS/Python: declare the capability, sync yourself) |
| session context (`GET …/daemon/processes/{id}/session`, `…/daemon/sessions/{id}`) | machine-token HTTP routes | pending (#411 F1) | — | 1.1: `Client.SessionContext` / `SessionContextByID` (TS `sessionContext`, Python `session_context`) |
| per-process launch hook (cwd/argv/env, sandbox wrapper, refusal) | — | — | — | 1.1: `ProcessOptions.Launch` (TS/Python: your `process.start` handler) |
| refusal reasons (`error.data.reason`: `workspace_boundary`, `session_rejected`, …) | v1.1 | pending (#411 F3) | `spawn_failed` | 1.1: `Reason*` + `NewReasonError` (TS `ErrorReason` + `RpcError.withReason`, Python `ErrorReason` + `RPCError.with_reason`) |
| `workspace.backup` | v1.1 additive | available | available | — |
| agent (`turn.*`, interactions) | v1.1 + `agent` | preview (machine executors, bound with `PUT …/agents/{agentID}/executor-binding`; text input only) | no | available (both sides) |
| local socket | — | — | coming | available |

---

## 6. Identity, pairing and credentials

- **Machine identity**: a computer = one machine record + one or more machine daemon tokens. Create one in the BMA dashboard ("Connect machine"), with `bma machines create-local`, or through the API. The token is shown once.
- **Executor credential**: the machine token (machine path) or environment key (selfhost path) is only sent to `POST …/v1/executors/token`, which returns a 5-minute HS256 JWT. Only the JWT goes on the WebSocket.
- **Model credentials** of the local brain are separate from executor credentials; the SDK never reads or forwards them.
- Option B pairing: the same machine-creation flow yields the machine token; store it securely (for example the OS keychain) and do not install bmad.

Details: [protocol reference §2](protocol-reference.md#2-connection-and-authentication).
