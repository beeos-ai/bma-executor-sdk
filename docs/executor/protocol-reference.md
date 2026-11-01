# Executor Protocol reference (v1 + v1.1)

[中文](protocol-reference.zh-CN.md) · [Overview](README.md)

The complete reference for `bma.executor.v1` and `bma.executor.v1.1` as served by the BMA platform and implemented by bma-executor-sdk 0.4.0. Where older documents disagree, this page and the contract win. Labels: **[Available]** shipped or live; **[Coming]** defined in the contract, capability-gated, not served yet; **[Design]** not implemented.

The machine-readable contract is in this repository: [`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml) (method catalog, `x-bma-since`, `x-bma-capability`, idempotency keys, errors), [`executor.asyncapi.yaml`](../../contracts/bma/executor/executor.asyncapi.yaml) and [`schemas/`](../../contracts/bma/executor/schemas); the SDK protocol types are generated from it.

---

## 1. Versions and general rules

| Item | Value |
| --- | --- |
| Protocol / WebSocket subprotocol | `bma.executor.v1`, `bma.executor.v1.1` |
| Server choice | prefers v1.1 when both are offered, echoes exactly one |
| Messages | JSON-RPC 2.0, named object `params`, no batches |
| `id` | string, unique per connection (server: `srv_N`, bmad: `exe_N`) |
| Unknown members | ignored by both peers |
| Unknown method | `-32601` |
| `jsonrpc` not `"2.0"` | `-32600` |
| Invalid JSON | `-32700` (empty id) |

- `executor.register.protocol` must equal the negotiated subprotocol, otherwise `-32010` and close `4003`.
- v1.1 is additive: a v1.1 server serves v1 executors and **never sends v1.1 methods to a v1 connection**, even if it claims v1.1 capabilities.
- bmad requires v1.1: it closes with `1008` when the server did not select v1.1 or the register result `protocol` is not v1.1.

---

## 2. Connection and authentication

### 2.1 Endpoints

| Path | Machine path (bma-api) | Environment path (managed-agents) |
| --- | --- | --- |
| token | `POST /api/v1/executors/token` | `POST /v1/executors/token` |
| channel | `GET /api/v1/executors/channel` (WebSocket) | `GET /v1/executors/channel` |
| artifacts | — | `POST /v1/executors/artifacts`, `GET /v1/executors/artifacts/{id}` |

bma-api mounts them only when the executor token secret is configured. `bmad executor --executor-url` takes the origin including `/api`, for example `https://api.bma.beeos.ai/api`.

### 2.2 Token exchange: `POST …/v1/executors/token`

```http
POST /api/v1/executors/token
Authorization: Bearer <machine daemon token or environment key>
Content-Type: application/json

{"environment_id": "", "executor": {"name": "bmad", "os": "darwin", "arch": "arm64", "version": "0.2.11"}}
```

- Empty `environment_id` → machine path: machine daemon token, only on a server with machine hooks (otherwise 404).
- Non-empty → environment path: an environment key, or a machine token whose machine has an active grant on the environment's project.

Response:

```json
{"executor_id": "exec_<machine uuid>", "token": "<jwt>", "expires_in": 299,
 "ws_url": "wss://api.bma.beeos.ai/api/v1/executors/channel", "channel_scope": ["control"]}
```

| Credential | `executor_id` | `channel_scope` | Key JWT claims |
| --- | --- | --- | --- |
| machine token, machine path | `exec_<machine_id>`, stable | `["control"]` | `org_id`, `machine_id`, `daemon_token_id`; no project/environment |
| machine token, environment path | `exec_<machine_id>` | `["control","mcp"]` | `org_id`, `project_id`, `environment_id`, `machine_id` |
| environment key | `exec_<key_id>`, stable | `["control","mcp"]` | `org_id`, `project_id`, `environment_id` |

JWT: HS256, `iss=bma`, `aud=bma.executor.v1` (both versions), `nbf=now-5s`, 5-minute TTL. Errors: `401` missing/invalid credential; `404` environment not found or outside the credential (existence is not leaked), or path not served.

### 2.3 Opening the channel

```http
GET /api/v1/executors/channel?channel=control
Sec-WebSocket-Protocol: bma.executor.v1.1
Authorization: Bearer <jwt>
```

`channel` defaults to `control`; the other value is `mcp` (§7). The JWT goes only in `Authorization`. HTTP errors before the upgrade: `400` bad channel or no supported subprotocol; `401` missing/invalid JWT; `403` channel not in `channel_scope`; `404` machine JWT on a server that does not serve machines; `503` not configured. The first frame must be the executor's `executor.register`. The server closes with `4001` when the JWT expires, unless the executor sent a fresh token first with `executor.token.refresh` (same executor, scope and machine credential; the SDK clients do this at 70% of `expires_in`), which moves the deadline to the new token's expiry.

### 2.4 WebSocket close codes

| Code | Sent by | Meaning | Executor action |
| --- | --- | --- | --- |
| 1000 | both | normal close (including `executor.close`) | as intended |
| 1001 | server (MCP) | MCP channel replaced for the same key, or server drain | reopen if needed |
| 1008 | executor (bmad) | register failed / v1.1 not negotiated | back off and reconnect |
| 1009 | server (MCP) | MCP frame over 1 MiB | fix and reopen |
| 4001 | server | JWT expired | fetch a new token, reconnect |
| 4002 | server (machine path) | runtime registration refused (for example no `process.enabled`) or the machine's daemon runtime ended (authority lost) | fetch a new token; stop if the token endpoint answers 401 |
| 4003 | server | `register.protocol` ≠ subprotocol | fix the implementation |
| 4004 | server | a newer connection for the same executor id registered | **stop, do not reconnect** |
| 4005 | server | drain complete | reconnect |

> The old v1 document called 4002 "token revoked" and listed 1011; neither matches the code. The selfhost worker stops on 4002; bmad treats it as a disconnect and reconnects, and the token endpoint decides whether it may continue.

### 2.5 Reconnect

bmad: any close other than `4004` reconnects with backoff from 1s doubling to about 30s, fetching a new token each time, and registers with `resume` (§8.1).

The SDK clients (Go, TypeScript, Python, 0.4.6+) stop only on `4003` and `4004`; every other close, including `4002`, `4005`, `1001`, `1008`, `1012` and `1013` from a restarting server, reconnects the same way.

---

## 3. Capability bits

`executor.register.capabilities` is an object; unknown members are ignored. The server **gates** only on the bits marked so below:

| Bit | Shape | Gates | Server then sends | Status |
| --- | --- | --- | --- | --- |
| `process.enabled` | `{enabled, max_processes?, buffer_bytes?, pty?, file_transfer?, git_credentials?}` | yes | `process.*`; **required** on the machine path (else 4002) | [Available] |
| `process.pty` | bool | yes | `process.start io_mode:"pty"` | [Available] |
| `process.file_transfer` | bool | yes | `process.start file_transfer` | [Available] |
| `process.git_credentials` | bool | yes | `process.start git_credentials:true` | [Available] |
| `terminal.enabled` | `{enabled, kinds?, max_sessions?}` | yes | `terminal.open/attach/close` | [Available] |
| `skills.install` | `{install}` | yes | `skill.install` | [Available] |
| `artifacts.upload` | `{upload, max_bytes?}` | no (informational) | — | [Available] |
| `shell`, `files`, `mcp`, `chrome`, `computer_use`, `display` | see contract `schemas/executor.json` | no | — | [Available] (`display` is the v1 bool or v1.1 `{rfb}`) |
| `workspace.backup` | `{backup}` | yes | `workspace.backup`; restore before register | [Available] |
| `agent` | `AgentCapability` | yes | `turn.*`, `session.close`, `interaction.response` | contract and SDK [Available]; platform [Coming], see [agent capability](agent-capability.md) |

- Gating only applies on v1.1 connections.
- A peer receiving a method for a bit it did not advertise answers `-32012 capability_unavailable`; an unserved tool in `tool.call` uses `data.capability = "tool:<name>"`.
- The harness does not retry on another transport when a capability is missing (`ErrCapabilityUnavailable`).
- `executor.capabilities.update` is **accepted but not applied** by the server today; reconnect to change capabilities.

bmad advertises: `shell`, `process{enabled, max_processes:64, buffer_bytes:1048576, pty: not on Windows, file_transfer/git_credentials: machine path}`, `terminal` (when the PTY host is available), `display{rfb:false}`, `skills{install}` (machine path), `artifacts` (environment path only).

---

## 4. Methods

H→E = harness to executor; E→H = executor to harness; "req" needs a response, "notif" has no `id`.

### 4.0 Method table

| Method | Dir | Kind | Since | Capability | Idempotency |
| --- | --- | --- | --- | --- | --- |
| `executor.register` | E→H | req | v1 | — | once per connection |
| `executor.heartbeat` | E→H | req | v1 | — | — |
| `executor.capabilities.update` | E→H | notif | v1 | — | — |
| `executor.drain` | H→E | req | v1 | — | — |
| `executor.close` | both | notif | v1 | — | — |
| `session.assign` | H→E | req | v1 | — | `session_id` |
| `session.ready` | E→H | req | v1 | — | — |
| `session.event` | E→H | req | v1 | — | JSON-RPC `id` |
| `session.status` | E→H | notif | v1 | — | — |
| `session.stop` | H→E | req | v1 | — | — |
| `session.ended` | E→H | req | v1 | — | — |
| `tool.call` | H→E | req | v1 | tool name | `tool_call_id` |
| `tool.result` | E→H | req | v1 | — | `tool_call_id` |
| `tool.cancel` | H→E | req | v1 | — | — |
| `tool.progress` | E→H | notif | v1 | — | — |
| `process.start` | H→E | req | v1.1 | `process.enabled` | `(session_id, process_id)` |
| `process.read` / `process.list` | H→E | req | v1.1 | `process.enabled` | read-only |
| `process.write` | H→E | req | v1.1 | `process.enabled` | JSON-RPC `id` |
| `process.interrupt` / `process.terminate` | H→E | req | v1.1 | `process.enabled` | no effect on an ended process |
| `process.status` | E→H | notif | v1.1 | `process.enabled` | advisory |
| `terminal.open` / `terminal.close` | H→E | req | v1.1 | `terminal.enabled` | `terminal_id` |
| `terminal.attach` | H→E | req | v1.1 | `terminal.enabled` | — |
| `skill.install` | H→E | req | v1.1 | `skills.install` | `(skill_id, revision_id, digest)` |
| `workspace.backup` | H→E | req | v1.1 | `workspace.backup` | `backup_id` |
| `turn.start` / `turn.input` / `turn.cancel` / `session.close` / `interaction.response` | H→E | req | v1.1 | `agent` | see agent doc |
| `session.event_delta` | E→H | notif | v1.1 | `agent` | — |
| `interaction.request` / `interaction.cancel` | E→H | req | v1.1 | `agent` | `interaction_id` |

> The method is `skill.install` (singular); `skills.install` is the capability bit.

### 4.1 `executor.register`

Params: `protocol` (= subprotocol); `environment_id` (`""` on the machine path; must equal the JWT environment otherwise, else `-32002`); `executor{name, os, arch, version}` — on the machine path `version` becomes the daemon runtime version and must be `MAJOR.MINOR.PATCH` or `0.0.0-dev`, otherwise registration is refused (4002); `os` picks the default shell; `capabilities` (§3); `resume{executor_id, last_connection_id, active_sessions[]}` (§8.1).

Result:

```json
{"executor_id": "exec_…", "connection_id": "…", "server_time": "2026-10-31T15:00:00Z",
 "heartbeat_interval_ms": 15000, "lease_ttl_ms": 60000,
 "max_control_frame_bytes": 65536, "max_mcp_frame_bytes": 1048576, "drain": false,
 "protocol": "bma.executor.v1.1",
 "limits": {"max_inflight_requests": 8, "max_concurrent_process_reads": 4, "max_process_read_bytes": 32768,
            "max_process_write_bytes": 32768, "max_processes_per_session": 64, "max_artifact_bytes": 16777216}}
```

`protocol` and `limits` only on v1.1. Errors: `-32602`, `-32600` (second register), `-32001` (`resume.executor_id` ≠ machine credential), `-32002`, `-32010` (then 4003), `-32603`. After success the older connection with the same id gets 4004; on the machine path the platform hooks bind the daemon runtime and close with 4002 on failure.

### 4.2 `executor.heartbeat`

`{connection_id, active_sessions[], load?{cpu_percent, memory_percent}}` → `{server_time, lease_ttl_ms, drain}`. Wrong `connection_id` is `-32602`. Sent every `heartbeat_interval_ms` (15s); renews the leases of the connection's work items and, on the machine path, the daemon runtime lease and presence. When a lease expires (default 60s, max 10 min) the server sends `tool.cancel` + `session.stop` (`lease_expired`). `drain:true` means accept no new sessions.

### 4.3 `executor.capabilities.update` (notif)

`{capabilities}`; not applied today (§3).

### 4.4 `executor.drain`

`{reason, deadline_ms?}` → `{accepted, active_sessions[]}`. Server drain: stop dispatching → `executor.drain` → wait for active sessions (or the deadline) → close 4005.

### 4.5 `executor.close` (notif, both)

`{reason}`, then close 1000/1001.

### 4.6 `session.assign`

Params: `session_id`, `work_id` (equal in harness sessions; the machine path uses `sess_machine_<machine public id>`); `agent_id?`; `environment_id` (`""` on the machine path); `deadline_ms?` (session lifetime; machine process sessions set none and an executor must not kill live processes with a lifetime/idle timer); `lease{token, fencing_token}`; `resources[]?` (`{type:"memory_store", memory_store_id, mount_path}`; not sent today); `mcp_servers[]?` (`{server_key, connection_origin: service|environment, transport{type: stdio|http, command, args, cwd, env, timeout_ms, command_sha256, url}}`; defined in the contract, not sent by the platform yet [Coming]); `metadata` (`source:"harness"` = long-lived multi-call session; anything else = one-shot, ends after its single tool call).

Result `{accepted, session_id}`. Over capacity: `-32009` with `retryable:true`. A repeated assign of the same `session_id` is idempotent.

### 4.7 `session.ready`

`{session_id, work_id, lease_token}` → `{accepted}`; sent after workspace/MCP setup. Mismatch `-32003`, unknown session `-32007`. The harness dispatches nothing before ready.

### 4.8 `session.event`

`{session_id, event}` → `{accepted, event_id}`. `event` is an object with a non-empty Anthropic-style `type`; the request **must have a JSON-RPC `id`**, which is the idempotency key. Stored on the environment path; the v1 contract lists `agent.tool_result`. The agent capability adds `turn_id`/`event_id`/`sequence` and in-order ACKs (see [agent capability](agent-capability.md)).

### 4.9 `session.status` (notif)

`{session_id, status: starting|running|idle|stopping|stopped|error, detail?}`; accepted, not processed today.

### 4.10 `session.stop`

`{session_id, reason, deadline_ms?}` → `{stopped}`. Terminates every process and tool call of the session (SIGTERM, SIGKILL after a grace; bmad uses 2s), then `session.ended`. Unknown session `-32007`.

### 4.11 `session.ended`

`{session_id, reason}` → `{accepted}`, after every process exited; **retried until accepted, across reconnects**. bmad reasons: `stopped`, `completed`.

### 4.12 `tool.call`

`{session_id, tool_call_id, tool, input, deadline_ms?, permission?{mode: always_allow|always_ask}}` → `{accepted}` (acceptance only; the outcome comes in `tool.result`).

- Tool names: `bash`, `read`, `write`, `edit`, `glob`, `grep`, `list_files`, `computer_observe`, `computer_action`. Unserved tools: `-32012`, `data.capability:"tool:<name>"`.
- `bash` input `{command, cwd?, timeout_ms?, env?}`; bmad implements only `bash`.
- A repeated `tool_call_id` is `accepted:true` and **not re-executed**. bmad does not resend a finished result on a duplicate call; delivery relies on executor-side `tool.result` retries (§4.13). SDK `Harness.CallTool` records results and returns them on repeats.
- The environment path maps work items to `bash`/`read`/`write`/`list_files`; **the machine path does not use `tool.call`**, it uses `process.*`.

### 4.13 `tool.result`

`{session_id, tool_call_id, outcome: succeeded|failed|canceled, is_error, content[], duration_ms?, artifacts[]?}` → `{accepted}`.

- Idempotent by `tool_call_id`. Retry on retryable errors and across reconnects until accepted (bmad: up to 10 doubling retries on `-32009`, waits up to one lease TTL for a connection).
- Content blocks `{type: text|image|resource|artifact, text?, data?, mime_type?, url?, artifact_id?}`. Large output: upload first (§6.1) and reference with an `artifact` block; every `artifacts[]` entry must belong to the session, else `-32002`.
- A result over 64 KiB is still accepted (inbound limit about 17 MiB) and offloaded to an artifact. bmad turns bash results over 48 KiB into an artifact (environment path) or truncates with a note (machine path).
- Errors: `-32007`, `-32002` (unknown `tool_call_id` in a harness session, not retryable), `-32003` (fenced), `-32602` (no `outcome`).

### 4.14 `tool.cancel`

`{session_id, tool_call_id, reason?}` → `{canceled}`; kills the call's process group; `false` when unknown or finished. The call still reports `tool.result` with `outcome:"canceled"`.

### 4.15 `tool.progress` (notif)

`{session_id, tool_call_id, message}`; optional, accepted only.

### 4.16 `process.*` (v1.1, `process.enabled`)

Every `process.*` acts on a process in an assigned session; the harness allocates `process_id`.

**`process.start`** (validation ):

| Field | Rule |
| --- | --- |
| `process_id` | required, ≤128 bytes |
| `argv` | 1–256 entries, ≤4 KiB each, ≤16 KiB total; no shell; empty with `file_transfer` |
| `cwd` | resolved (symlinks included) inside the work root |
| `env` | explicit, ≤64 entries/16 KiB; loader/injection variables (`LD_PRELOAD`, `DYLD_INSERT_LIBRARIES`, `NODE_OPTIONS`, …) are rejected; only `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR` are inherited |
| `stdin` | `open` / `closed`; bytes go through `process.write` |
| `stderr` | empty = separate streams; `"merge"` = OS-level dup2 into stdout (one cursor). The machine path always uses `merge` and requires the result to echo `stderr:"merge"`, else it fails as `-32012`  |
| `wait_ms` | 0–300000: wait that long for the process to end before answering |
| `timeout_ms` | 0–86400000: kill and end as `failed`, `reason:"timeout"` |
| `metadata` | string map; the platform sends `tool_call_id`; session files add `agent_session_id` |
| `io_mode` | `"pty"`: run on a 24x80 PTY; needs `process.pty`; `close_stdin` rejected |
| `file_transfer` | `{direction: upload\|download, local_path}` instead of `argv`; needs `process.file_transfer`; excludes `argv`/`io_mode`/`git_credentials` (§6.2) |
| `git_credentials` | `true`: per-process GitHub credential helper; needs `process.git_credentials` (§6.3) |

Result `ProcessState{process_id, state: running|exited|failed|killed, pid?, started_at?, ended_at?, reason?, cursors{stdout, stderr}, exit_code, exit_signal, stderr?, accepted?, file_transfer?}`. Same id + same spec returns the current state; same id + different spec is `-32013` `data.reason:"process_id_spec_mismatch"`. 64 running processes → `-32009` retryable; ended processes keep state and output until the session ends (bounded at 64 MiB of output and 1024 ended processes). A spawn failure creates no state and is an error with `data.reason:"spawn_failed"`. A `pty` member (instead of `io_mode`) is `-32602`.

**`process.read`**: `{session_id, process_id, stream: stdout|stderr, cursor, max_bytes?(1–32768, default 32768), wait_ms?(0–300000)}` → `{process_id, stream, data(base64), from_cursor, next_cursor, earliest_cursor, gap, truncated, eof, state, ended_at?, reason?, exit_code, exit_signal, file_transfer?}`. Cursors are byte offsets; each stream has a 1 MiB ring buffer; `cursor < earliest_cursor` gives `gap:true`. Long-polls only at the end of a live stream; at most 4 concurrent waiting reads per connection (`-32009` retryable). Out-of-range cursor `-32602`; unknown process `-32002`.

**`process.write`**: `{session_id, process_id, data?(base64, ≤32 KiB decoded), close_stdin?}` (exactly one) → `{accepted}`; deduplicated by JSON-RPC `id`. Closed stdin `-32013` `stdin_closed`; `close_stdin` on a PTY `-32012`.

**`process.interrupt` / `process.terminate`**: `{session_id, process_id, grace_ms?}` → `ProcessState` (`accepted:true`). Interrupt = SIGINT; terminate = SIGTERM to the process group, SIGKILL after `grace_ms` (default 5000, max 30000), returns after exit; no effect on an ended process.

**`process.list`**: `{session_id}` → `{processes[]}` ordered by start time; the platform reconciles with it on every registration (§8.2).

**`process.status`** (notif): `{session_id, process_id, state, exit_code?, exit_signal?, ended_at?, reason?, observed_at}`; advisory, the harness confirms with `process.read`/`process.list`.

### 4.17 `terminal.*` (v1.1, `terminal.enabled`, machine path)

Control on the control channel; PTY bytes **never** on it.

| Method | Params | Result | Rules |
| --- | --- | --- | --- |
| `terminal.open` | `{terminal_id, kind: human\|agent, shell?, argv?, cwd?, env?, rows?, cols?}` | `{terminal_id, epoch, pid, rows, cols, earliest_seq, latest_seq, created_at}` | idempotent by `terminal_id`; `rows/cols` only size a new PTY; other spec mismatches are `-32013`; `epoch` increments when the PTY is recreated |
| `terminal.attach` | `{terminal_id, epoch, stream_id, can_write, from_seq, ack_seq, host{url, ticket}}` | `{accepted, writer_epoch}` | the executor dials `host.url` with `Authorization: Bearer <ticket>` (single-use, never in the URL), subprotocol `bma.terminal.v1`, and answers after writing the `attached` frame; closing that socket detaches, the PTY survives |
| `terminal.close` | `{terminal_id, reason?}` | `{closed, exit_code?}` | idempotent |

Errors carry `data.terminal_code` (a `bma.terminal.v1` code such as `terminal_writer_busy`). Host-socket frames match the browser terminal contract.

### 4.18 `skill.install` (v1.1, `skills.install`, machine path)

`{machine_id, skill_id, revision_id, digest, download_token, download_expires_at(unix s)}` → `{state: ready|failed, error_code?, error?}`. Downloads `GET /api/v1/daemon/skills/{id}/archive` with the token, verifies the digest, extracts safely; idempotent by `(skill_id, revision_id, digest)`.

### 4.19 `workspace.backup` [Available]

`{backup_id, paths[] (absolute), exclude[]?, skip_if_fingerprint?}` → `{state: uploaded|unchanged|failed, fingerprint?, digest?, size_bytes?, entries?, skipped?, error?}`. The executor streams a tar+zstd archive with `PUT /api/v1/daemon/machine-backups/{backup_id}/archive` (machine token, header `X-BMA-Backup-Fingerprint`); an equal fingerprint uploads nothing (`unchanged`). Never includes the executor's own `BMA_HOME`. Restore has no method: a recreated executor `GET`s `/api/v1/daemon/machine-backup-restore` **before** `executor.register`, extracts, and `POST`s `…/machine-backups/{id}/restore-result`.

---

## 5. Error codes

`{code, message, data?}`; `data` may carry `retryable`, `detail`, `capability`, `reason`, `terminal_code`.

| Code | Message | Meaning |
| --- | --- | --- |
| -32700 | parse_error | invalid JSON |
| -32600 | invalid_request | not JSON-RPC 2.0; second register |
| -32601 | method_not_found | unknown method |
| -32602 | invalid_params | validation failure, including limits |
| -32603 | internal_error | peer error |
| -32000 | unauthorized | bad credential |
| -32001 | forbidden | tenant/policy denial; resume id ≠ machine credential |
| -32002 | not_found | not found (also cross-tenant); unknown process/call/artifact |
| -32003 | lease_expired | lease/fencing invalid |
| -32004 | timeout | deadline exceeded |
| -32005 | canceled | canceled by peer |
| -32006 | payload_too_large | frame limit |
| -32007 | session_not_active | session not assigned or ended |
| -32008 | mcp_unavailable | MCP server failed |
| -32009 | executor_busy | capacity; usually `retryable:true`; **retry with backoff, never drop silently** |
| -32010 | unsupported_protocol | version mismatch |
| -32011 | resource_unavailable | v1.1 |
| -32012 | capability_unavailable | v1.1; `data.capability` |
| -32013 | conflict | v1.1; `data.reason` (for example `process_id_spec_mismatch`, `stdin_closed`) |

**Definite rejection vs. unknown outcome**: a JSON-RPC error from the peer is a definite answer. A local transport failure (deadline, closed connection, failed send, undecodable answer) means the request may or may not have taken effect: the BMA harness treats it as a transport failure, never treats it as a rejection and never blindly resends a side-effecting request; reconnect reconciliation decides. Exception: a full local outbound queue sent nothing and returns a retryable `-32009`. There is **no** `outcome:"unknown"` on the wire; "unknown" is harness state: SDK `Harness` reports `ErrToolOutcomeUnknown` when the executor was lost before `tool.result`; the machine path records a granted process the executor no longer has as unknown and never re-runs it.

---

## 6. Data planes and machine HTTP routes

### 6.1 Artifacts (environment path) [Available]

`POST /v1/executors/artifacts`, `Authorization: Bearer <executor JWT>`, headers `X-BMA-Session-ID`, `X-BMA-Artifact-Digest` (`sha256:<hex>`), `Content-Type`, optional `X-BMA-Artifact-Name`; raw body ≤16 MiB. Digest mismatch `409`, too large `413`; idempotent by `(session, digest)`: `201` new, `200` replay; returns `ArtifactRef{artifact_id, name, mime_type, size_bytes, digest, created_at}`. No artifact store on the machine path yet.

### 6.2 File transfer (machine path) [Available]

With `file_transfer`, the executor moves one file between the machine and the process's file endpoint: `POST` (upload, `?filename=`) / `GET` (download) `/api/v1/daemon/processes/{process_id}/file`, machine token. Upload: regular files only, executor-side limit 1 GiB (the server may be stricter for some targets), sha256 compared with the stored digest. Download: written to a temporary file next to the destination and atomically renamed only after length and sha256 (response digest header) match; keeps the destination's permissions; 1 GiB limit. Outcome in `ProcessState.file_transfer{path?, digest?, error?{code, error, current_digest?}}`.

### 6.3 Git credentials (machine path) [Available]

With `git_credentials:true` the executor sets `GIT_CONFIG_PARAMETERS` to the helper (`bmad __bma_git_credential`), which asks a private local executor socket (random per-process capability, valid only while the process runs); the executor fetches short-lived tokens from `/api/v1/daemon/processes/{id}/git-credentials`. Nothing is written to disk.

### 6.4 Session files (`/mnt/session`) [Available]

Matches Anthropic Managed Agents files: mounted files are readable at `/mnt/session/uploads/<mount_path or file_id>`; files written under `/mnt/session/outputs/` become session-scoped files.

- The platform names the session in `process.start.metadata.agent_session_id`; the machine shows the session whose shell process started last; on a switch it uploads the old outputs, then empties and resyncs.
- Uploads: before each shell process, `GET /api/v1/daemon/processes/{id}/session-files` returns the manifest; anything not in it is removed; missing/changed files are downloaded from `…/session-files/{resource_id}/content`, checked for length and sha256, `chmod 0444` and renamed in. A sync failure fails the process start (`session files: …`).
- Outputs: every 2 s, regular non-empty files whose size and mtime were stable across two walks and changed since the last upload are sent with `POST …/session-outputs?path=<relative path>` and `X-BMA-File-Digest`; idempotent by (session, sha256, path). Limits: 1 GiB per file, 512-byte paths, 10 000 files per walk. `413/422/400` are not retried until the file changes; transport errors, `404`, `409` retry next walk.
- Auth: machine token, anchored on a granted process of the calling machine with an active session; mount routes need a starting/running process, outputs are accepted up to 15 minutes after it ended.

### 6.5 PTY data plane

See §4.17.

---

## 7. MCP channel

[Available: server channel (environment path), SDK executor `OpenMCPChannel`, SDK `Core` stdio supervisor, SDK `Harness` MCP channels] [Coming: the platform sending `session.assign.mcp_servers`] [Not implemented: MCP in bmad; the platform harness dispatching MCP servers to executors in production]

- Boundary: the brain runs the MCP client; the hand starts or connects the MCP server and forwards raw MCP JSON-RPC. No BMA MCP methods, no wrapper.
- Open `GET …/v1/executors/channel?channel=mcp&session_id=<id>&server_key=<key>`, subprotocol `bma.executor.v1`, JWT with the `mcp` channel (machine-path JWTs do not have it: 403).
- At most one channel per `(environment, session_id, server_key)`; a newer one replaces the older (closed with 1001). `session_id` ≤256, `server_key` ≤128 bytes. Frames ≤1 MiB (else 1009).
- Each frame is one MCP JSON-RPC message; the harness does `initialize`, `notifications/initialized`, `tools/list`, `tools/call`; the executor bridges newline-delimited stdio JSON-RPC. MCP errors pass through unchanged; never fake an empty `tools/list`.
- Config (`schemas/mcp.json`): `connection_origin:"service"` = the harness connects itself; `"environment"` = the executor starts/connects. stdio: no shell, `cwd` inside the session workspace, explicit `env`, verify `command_sha256` when given.
- Lifecycle (SDK `Core`): one process per (brain, session, server_key); restart with backoff when either side ends (the channel sees a fresh server: re-initialize); after 5 consecutive failures `session.status` `error` / `mcp_unavailable: <key>`; all closed when the session ends; sessions never share stdio pipes.

### 7.1 Chrome MCP and computer-use (executor requirements)

The platform does not check these; an integrator's executor must follow them:

- Chrome runs on the executor; CDP binds `127.0.0.1` only (`--remote-debugging-port=0`, `--remote-debugging-address=127.0.0.1`, read `DevToolsActivePort`) and is never exposed; one user-data-dir per session (0700), deleted at session end. Chrome MCP tools are standard MCP tools, no BMA-specific methods.
- Computer-use: prefer BMA's native desktop tools; a third-party computer-use MCP runs on the executor under the same authority checks. During human takeover the executor rejects input actions; release keys and buttons on disconnect, cancel and handback.

---

## 8. Idempotency and reconnect

### 8.1 `resume`

On re-registration the executor sends `resume{executor_id, last_connection_id, active_sessions[]}`:

- Machine path: `resume.executor_id` must equal `exec_<machine_id>` from the credential, else `-32001`. If `active_sessions` contains `sess_machine_<id>` the platform reattaches that session instead of opening a new one.
- Environment path: the server releases the old connection's leases on these sessions so the new connection can claim them; the next claim advances the fencing token and a late result from the old executor is rejected. The selfhost worker does not send `resume`.
- Executor side (bmad): sessions belong to the process, not one connection; after a disconnect they and their processes are kept for one lease TTL; only then are all sessions stopped with `lease_expired`.

### 8.2 Machine-path reconciliation

On every registration the platform calls `process.list` and uses the executor's process table as the daemon runtime's reconciliation claims: a granted process the executor no longer has becomes **unknown and is never re-run**; running processes are retained and observed; processes that ended while disconnected report their terminal state now. The executor keeps no action journal; PostgreSQL is the journal: an accepted side-effecting action (stdin write, signal) without evidence is claimed as applied, never replayed, and recorded unknown.

### 8.3 Idempotency keys

| Request | Key | On repeat |
| --- | --- | --- |
| `session.assign` | `session_id` | not recreated |
| `tool.call` | `tool_call_id` | `accepted:true`, not re-run |
| `tool.result` | `tool_call_id` | `accepted:true` |
| `session.event` | JSON-RPC `id` | same `event_id` |
| `process.start` | `(session_id, process_id)` + spec | current state; different spec `-32013` |
| `process.write` | JSON-RPC `id` | not written twice |
| `terminal.open/close` | `terminal_id` | live PTY / `closed:true` |
| `skill.install` | `(skill_id, revision_id, digest)` | not reinstalled |
| `turn.start` / `turn.input` / `interaction.*` | `turn_id` / `input_id` / `interaction_id` | first result |

### 8.4 Messages that must be retried

`tool.result`, `session.ended` (and durable agent `session.event`s): retry after retryable errors and reconnects until accepted. The SDK executor end calls `OnToolResultLost` after all retries fail so the adapter can persist and replay; it never drops a result silently (see the [Go executor README](../../executor/README.md)).

---

## 9. Cancellation levels

| Level | Method | Scope | Outcome |
| --- | --- | --- | --- |
| tool call | `tool.cancel` | one `tool_call_id`'s process group | `tool.result outcome:"canceled"` |
| process | `process.interrupt` / `process.terminate` | one process | `ProcessState` |
| agent turn | `turn.cancel` | the running turn, its local tools and pending interactions | `session.status_idle turn_outcome:"canceled"`, session continues |
| session | `session.stop` | every process, tool call and MCP server of the session | `session.ended` |
| agent session | `session.close` | `turn.cancel`, wait for all ACKs, release the local conversation | `session.ended` |
| connection | `executor.drain` | no new sessions, wait for current ones | 4005 |
| lease expiry | — | the connection's sessions | server `tool.cancel` + `session.stop` (`lease_expired`); executor stops after a disconnect longer than the lease TTL |
| replaced | 4004 | the whole executor | stop everything and exit |

Parallel sessions are independent.

---

## 10. Limits

| Item | Value |
| --- | --- |
| server → executor control frame | 64 KiB |
| executor → server control frame (server read limit) | 16 MiB + 1 MiB |
| bmad read limit | 4 MiB |
| MCP frame | 1 MiB |
| heartbeat / lease TTL | 15s / 60s default, 10 min max |
| JWT TTL / register timeout | 5 min / 10s |
| concurrent server → executor requests | 8 |
| concurrent executor → server handlers | 8 (`-32009` retryable when full) |
| `process.*` | §4.16; 64 running per session; 1 MiB per stream; 32 KiB read/write; 4 waiting reads; 64 MiB retained output, 1024 ended processes |
| bash tool | 1 MiB per stream captured; 48 KiB inline; 10 min default timeout |
| artifact upload | 16 MiB |
| file transfer / session file | 1 GiB |
| `executor_id` | `exec_` prefix, ≤128, `[A-Za-z0-9_-]` |

---

## 11. Security and tenancy

- No inbound port on the executor; the platform never dials private IPs. TLS/WSS everywhere (bmad allows plain http only for loopback/`host.docker.internal`).
- Long-lived credentials only for the token exchange and machine HTTP routes; the control channel carries only the short-lived JWT.
- Every method carries `session_id`/`tool_call_id` and is authorized against org/project/environment or the machine grant; cross-tenant access is `-32002`.
- No shell string building; explicit env; loader injection variables denied; file paths confined after symlink resolution.
- MCP process output is untrusted; stderr is bounded and never part of the protocol.
- Local authorization stays with the executor: platform answers (including interactions) are inputs, not local authorization.

---

## 12. Differences from the v1 document (code wins)

| Old statement | Actual |
| --- | --- |
| 4002 = token revoked; 1011 internal error | 4002 = machine runtime refused/ended; the server sends no 1011 (§2.4) |
| on reconnect the harness replies with the last acked event and reassigns pending `tool_call_id`s | nothing is reassigned; results arrive by executor retries; the machine path reconciles with `process.list` (§8) |
| "two missed heartbeats expire the lease" | leases renew/expire by `lease_ttl_ms` (§4.2) |
| `session.assign` carries `mcp_servers` | in the contract; not sent by main (§4.6) |
| `executor.capabilities.update` updates capabilities | not applied today (§3) |
| SDK `createHand({environmentKey})` | see the real SDK layout in the [overview §4](README.md#4-sdk-layout) |
