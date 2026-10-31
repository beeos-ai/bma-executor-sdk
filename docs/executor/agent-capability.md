# Agent capability

[中文](agent-capability.zh-CN.md) · [Overview](README.md) · [Protocol reference](protocol-reference.md)

Status: contract and SDK **[Available]**, platform **[Coming]**.

| Part | Where | Status |
| --- | --- | --- |
| Contract (methods, [`schemas/agent.json`](../../contracts/bma/executor/schemas/agent.json), conformance examples) | [`contracts/bma/executor`](../../contracts/bma/executor) (`bma.executor.v1.1`, contract 1.1.0) | [Available] |
| SDK: executor side `ServeAgent` / `AgentHandler`, harness side `AgentPlatform` | bma-executor-sdk 0.4.0 (Go, TypeScript, Python) | [Available] |
| Platform side (dispatch from the BMA API, event storage and SSE, interactions) | BMA platform | [Coming] |

This chapter is the wire contract an integrator implements or relies on. Names and fields follow the contract in [`contracts/bma/executor`](../../contracts/bma/executor).

---

## 1. Goal and scope

- A local **full agent** (model loop + tools) connects as an executor: the platform sends natural-language turns, the local agent streams full agent events back; approvals and questions use platform interactions (desktop, mobile and integrations see the same request).
- Hand methods (`tool.call`, `process.*`, `terminal.*`) are unchanged; `agent` is an additional capability.
- Alignment: sessions, turns, event names and fields, `stop_reason`, streaming previews follow Anthropic Managed Agents session events; connection, registration, heartbeat and reconnect are the existing Executor Protocol; approvals and questions follow BMA agent interactions.
- No new protocol version, transport or event names: one capability, 8 methods, 3 optional `session.event` fields.

## 2. Negotiation

`executor.register.capabilities.agent`:

```json
{
  "enabled": true,
  "models": [{"id": "claude-opus-4-5", "display_name": "Opus", "thinking_levels": ["off", "low", "medium", "high"], "default_thinking_level": "medium"}],
  "permission_modes": ["default", "accept_edits", "plan"],
  "interaction_kinds": ["permission", "question"],
  "file_inputs": true,
  "max_sessions": 4
}
```

- The platform sends agent methods only to v1.1 executors with `agent.enabled=true`; otherwise the executor answers `-32012`.
- `permission_modes` values are defined by the executor; the platform sends back one of them.
- Identity: the computer's machine identity (option A: bmad advertises it once the local brain attaches; option B: the desktop app). No separate identity.

## 3. Methods

| Method | Direction | Kind | Idempotency | Purpose |
| --- | --- | --- | --- | --- |
| `turn.start` | platform → executor | req | `turn_id` | open/resume the session and start a turn; returns the effective config |
| `turn.input` | platform → executor | req | `input_id` | add input to the running turn |
| `turn.cancel` | platform → executor | req | — | stop the running turn; the session stays |
| `session.close` | platform → executor | req | — | close the session, release the local conversation |
| `interaction.response` | platform → executor | req | `interaction_id` | approval/answer back to the request |
| `session.event` (extended) | executor → platform | req | `(session_id, event_id)` | durable agent event; the response is the ACK |
| `session.event_delta` | executor → platform | notif | — | streaming preview, may be lost |
| `interaction.request` | executor → platform | req | `interaction_id` | ask for approval/answer |
| `interaction.cancel` | executor → platform | req | `interaction_id` | withdraw a pending request |

Agent sessions need **no** `session.assign`: the first `turn.start` opens one; the executor includes agent sessions in heartbeats and `resume.active_sessions`.

### 3.1 `turn.start`

```json
{"session_id": "sesn_…", "turn_id": "turn_…", "input_id": "ain_…",
 "input": [{"type": "text", "text": "Fix the failing tests in pkg/foo"},
           {"type": "file", "file_id": "file_…", "name": "log.txt", "mime_type": "text/plain", "size_bytes": 2048}],
 "executor_session_ref": "local_conv_42",
 "config": {"model": "claude-opus-4-5", "thinking_level": "medium", "permission_mode": "default", "cwd": "/Users/me/src/foo", "system": "…"},
 "deadline_ms": 3600000, "metadata": {}}
```

Result `TurnStartResult{accepted, turn_id, executor_session_ref, resumed, effective{model, thinking_level, permission_mode, cwd}}`. `executor_session_ref` is the local conversation handle; the platform stores it and sends it with the next turn so the executor can resume after a restart.

- Mapping: one platform session ↔ one local conversation; turns use the platform `turn_id` / `input_id`.
- Repeating a `turn_id` returns the first result; nothing runs twice.
- A running turn → `-32013` (use `turn.input`); a new session beyond `max_sessions` → `-32009`; `cwd` outside allowed roots → `-32001`.
- Files are `file_id` references downloaded through the files API; no bytes on the control channel.
- `deadline_ms` expiry: `session.error{error:{type:"timeout"}}` + `session.status_idle turn_outcome:"failed"`.

### 3.2 `turn.input` / `turn.cancel` / `session.close`

- `turn.input{session_id, turn_id, input_id, input[]}`: idempotent by `input_id`; `turn_id` not running → `-32013` and the platform sends a new `turn.start`.
- `turn.cancel{session_id, turn_id, reason?}` → `{canceled}`: stops the loop and its local tools, withdraws pending interactions, emits `session.status_idle turn_outcome:"canceled"`; unknown/finished turns give `canceled:false`.
- `session.close{session_id, reason?}` → `{closed}`: `turn.cancel`, wait for every event ACK, release the local conversation, then `session.ended`.

Compared with hand methods: `tool.cancel` targets one `tool.call`; `session.stop` targets a hand session from `session.assign`; `session.stop`/`session.ended` only reclaim executor resources and **do not end the platform session** — the next `turn.start` continues it.

## 4. Events

### 4.1 Types (Anthropic names and fields)

| `event.type` | Fields |
| --- | --- |
| `agent.message` | `content[]` |
| `agent.thinking` | `content[]` (may be empty) |
| `agent.tool_use` | `name`, `input`, `evaluated_permission{type: allow\|ask\|deny}`; its `event_id` is the `tool_use_id` |
| `agent.tool_result` | `tool_use_id`, `content[]`, `is_error` |
| `agent.mcp_tool_use` / `agent.mcp_tool_result` | `mcp_server_name`, `name`, `input` / `mcp_tool_use_id`, `content[]`, `is_error` |
| `session.status_running` | — |
| `session.status_idle` | `stop_reason{type: end_turn\|retries_exhausted}`, `turn_outcome: completed\|canceled\|failed` (BMA extension) |
| `session.error` | `error{type, message}` |

`user.*` events are written by the platform (from `turn.start`/`turn.input`/`turn.cancel`/interaction answers); executors must not send them. `stop_reason:"requires_action"` is projected by the platform from pending interactions, not sent by the executor.

### 4.2 Durable events, sequence, ACK and resend

```json
{"jsonrpc": "2.0", "id": "exe_42", "method": "session.event",
 "params": {"session_id": "sesn_…", "turn_id": "turn_…", "event_id": "evt_0004", "sequence": 4,
            "event": {"type": "agent.tool_use", "name": "bash", "input": {"command": "go test ./pkg/foo"}, "evaluated_permission": {"type": "ask"}}}}
```

- `event_id`: assigned by the executor, unique in the session, unchanged on resend. `sequence`: +1 per durable event, from 1.
- The platform answers `{"accepted": true, "event_id": "…", "acked_sequence": 4}` only after persisting — **the response is the ACK**.
- Platform: `sequence <= acked_sequence` is a duplicate (accepted); `sequence != acked_sequence + 1` → `-32013` with `data.expected_sequence`.
- Executor: one ordered outbox per session, one event at a time; after re-registering, unacknowledged events are resent in order before any new event.
- Each turn is framed by `session.status_running` and `session.status_idle` (preceded by `session.error` on failure). The SDK emits both so every `turn_id` has exactly one terminal state.

### 4.3 Terminal state

The platform marks a turn finished and publishes it only after accepting its `session.status_idle` with every earlier sequence persisted (guaranteed by in-order ACKs). Clients (desktop, mobile) subscribe to the platform session event stream (SSE with `Last-Event-ID`), never to the executor.

### 4.4 Streaming previews

`session.event_delta{session_id, turn_id, event_id, event_type: agent.message|agent.thinking, index, text}`: a notification, no ACK, no resend, lost on disconnect. `event_id` is that of the final durable event; the platform maps deltas to Anthropic `event_start` + `event_delta`, and clients replace the preview with the durable event.

## 5. Approvals and questions

1. Before asking, send `agent.tool_use` (`evaluated_permission.type:"ask"`) and wait for its ACK, then send `interaction.request` (the SDK enforces this order).
2. `interaction.request{session_id, turn_id, interaction_id, kind: permission|question, tool_use_id?, tool_name?, tool_input?, scopes?[once|session], question?{prompt, options[{id,label}], multi_select, allow_free_text}, expires_at}` → `{accepted, platform_interaction_id}`; the platform maps it to an agent interaction.
3. After an answer on any surface the platform sends `interaction.response{session_id, interaction_id, outcome: allow|deny|answered|expired|canceled, scope?, deny_message?, answers?, responder?{actor_id, surface}}` → `{accepted}`.

Rules: `interaction.request` is idempotent by `interaction_id`; the first `interaction.response` wins and later ones get `accepted:false` (the platform marks its copy stale). On `expires_at` the platform sends `outcome:"expired"`; the executor times out locally too, treats it as a denial and sends `interaction.cancel{reason:"expired"}`. On `turn.cancel`/`session.close` the executor withdraws pending requests (`turn_canceled`); a local answer first sends `resolved_locally`; a canceled tool sends `tool_canceled`. **Local authorization**: a platform answer is input; the local agent's policy decides (it may still refuse after an allow, reported as `agent.tool_result is_error:true`).

## 6. SDK usage

Executor side:

```go
client.ServeAgent(myAgent, bmaexecutor.AgentCapability{
	Models:          []bmaexecutor.AgentModel{{ID: "my-model", ThinkingLevels: []string{"low", "high"}}},
	PermissionModes: []string{"default", "accept_edits"},
})
```

`myAgent` implements `AgentHandler{OpenTurn, RunTurn, CloseSession}`: `OpenTurn` reports the effective config; `RunTurn` works with `turn.Emit` (durable), `turn.Delta` (previews), `turn.Inputs()` (`turn.input`) and `turn.RequestInteraction`; `CloseSession` releases the local conversation. The SDK owns `event_id`/`sequence`, the outbox, resend after reconnect, running/idle framing and interaction waits/expiry/withdrawal.

TypeScript: `client.serveAgent(handler, capability)` and `new Harness({ agent })`, example [`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs). Python: `client.serve_agent(handler, capability)` and `Harness(agent=...)`, example [`agent_minimal.py`](../../executor/python/examples/agent_minimal.py).

Harness side (the platform, or a simulated one in tests): `HarnessOptions.Agent` (`AgentPlatform`) plus `Executor.StartTurn / SendTurnInput / CancelTurn / CloseAgentSession / RespondInteraction`.

Full flow: `cd executor && go run ./examples/agentexecutor` ([source](../../executor/examples/agentexecutor/main.go)) (scripted local agent + simulated platform + a TCP proxy that drops the connection: turn → streamed events → approval → result → follow-up → added input → resend after disconnect → cancel → close).

## 7. Needed on the platform before end to end

Selecting a "local executor" in the agent config, turning inputs into `turn.start`/`turn.input`, storing events and publishing them over SSE/the facade, interaction mapping and expiry are not implemented yet. Until then, integrate the executor side against the SDK example's simulated platform.
