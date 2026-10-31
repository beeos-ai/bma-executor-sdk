# agent 能力

[English](agent-capability.md) · [总览](README.zh-CN.md) · [协议参考](protocol-reference.zh-CN.md)

状态：契约与 SDK **【已有】**，平台 **【即将提供】**。

| 部分 | 位置 | 状态 |
| --- | --- | --- |
| 契约（方法、[`schemas/agent.json`](../../contracts/bma/executor/schemas/agent.json)、一致性示例） | [`contracts/bma/executor`](../../contracts/bma/executor)（`bma.executor.v1.1`，契约 1.1.0） | 【已有】 |
| SDK：executor 侧 `ServeAgent` / `AgentHandler`，harness 侧 `AgentPlatform` | bma-executor-sdk 0.4.0（Go、TypeScript、Python） | 【已有】 |
| 平台侧（BMA API 下发任务、事件入库与 SSE、interactions 映射） | BMA 平台 | 【即将提供】 |

本章描述外部接入方需要实现/依赖的线上契约。方法名、字段以 [`contracts/bma/executor`](../../contracts/bma/executor) 中的契约为准。

---

## 1. 目标与边界

- 让一个本机**完整 Agent**（模型循环 + 工具）以 executor 身份接入平台：平台下发自然语言任务，本机 Agent 回传完整 Agent 事件；审批/问答走平台 interactions（Desktop、手机、集成看到同一请求）。
- 手的能力（`tool.call`、`process.*`、`terminal.*`）不变；agent 能力是叠加的 capability。
- 语义对齐：会话/轮次/事件名与字段/`stop_reason`/流式预览对齐 Anthropic Managed Agents session events；连接、注册、心跳、重连沿用现有 Executor Protocol；审批/问答对齐 BMA agent interactions。
- 不新增协议版本、传输或事件名：只新增 capability `agent`、8 个方法、`session.event` 的 3 个可选字段。

## 2. 能力协商

`executor.register.capabilities.agent`：

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

- 平台只向 `agent.enabled=true` 的 v1.1 executor 发 agent 方法；否则 executor 答 `-32012`。
- `permission_modes` 的取值由 executor 定义，平台只回传其中之一。
- 身份：用这台电脑的机器身份（方案 A 由 bmad 在本机脑挂上后声明；方案 B 由 Desktop 声明），不另建身份。

## 3. 方法

| 方法 | 方向 | 类型 | 幂等键 | 作用 |
| --- | --- | --- | --- | --- |
| `turn.start` | 平台→executor | req | `turn_id` | 打开/恢复会话并开始一轮，返回实际生效配置 |
| `turn.input` | 平台→executor | req | `input_id` | 执行中追加输入 |
| `turn.cancel` | 平台→executor | req | — | 停止当前一轮，会话保留 |
| `session.close` | 平台→executor | req | — | 关闭会话并释放本地对话 |
| `interaction.response` | 平台→executor | req | `interaction_id` | 审批/问答结果回到原请求 |
| `session.event`（扩展） | executor→平台 | req | `(session_id, event_id)` | 持久 Agent 事件；响应即 ACK |
| `session.event_delta` | executor→平台 | notif | — | 流式预览，可丢 |
| `interaction.request` | executor→平台 | req | `interaction_id` | 发起审批/问答 |
| `interaction.cancel` | executor→平台 | req | `interaction_id` | 撤回未决请求 |

agent 会话**不需要** `session.assign`：首个 `turn.start` 即打开；executor 把 agent 会话计入 heartbeat 与 `resume.active_sessions`。

### 3.1 `turn.start`

```json
{"session_id": "sesn_…", "turn_id": "turn_…", "input_id": "ain_…",
 "input": [{"type": "text", "text": "修复 pkg/foo 的失败测试"},
           {"type": "file", "file_id": "file_…", "name": "log.txt", "mime_type": "text/plain", "size_bytes": 2048}],
 "executor_session_ref": "local_conv_42",
 "config": {"model": "claude-opus-4-5", "thinking_level": "medium", "permission_mode": "default", "cwd": "/Users/me/src/foo", "system": "…"},
 "deadline_ms": 3600000, "metadata": {}}
```

结果 `TurnStartResult`：`{accepted, turn_id, executor_session_ref, resumed, effective{model, thinking_level, permission_mode, cwd}}`。`executor_session_ref` 是本地对话句柄，平台保存并在下一轮回传，用于 executor 重启后恢复。

- ID 映射：平台 session ↔ 本地对话一对一；一轮用平台 `turn_id` / `input_id` 关联。
- 同一 `turn_id` 重发返回首次结果，不会跑第二次。
- 会话已有运行中的轮次 → `-32013`（改用 `turn.input`）；新会话超过 `max_sessions` → `-32009`；`cwd` 不在允许根内 → `-32001`。
- 文件输入只传 `file_id`，executor 经文件 API 下载，控制通道不传字节。
- `deadline_ms` 到期：`session.error{error:{type:"timeout"}}` + `session.status_idle turn_outcome:"failed"`。

### 3.2 `turn.input` / `turn.cancel` / `session.close`

- `turn.input{session_id, turn_id, input_id, input[]}`：按 `input_id` 幂等；`turn_id` 不是运行中轮次 → `-32013`，平台改发新 `turn.start`。
- `turn.cancel{session_id, turn_id, reason?}` → `{canceled}`：停 Agent 循环及其本地工具、撤回未决 interaction，发 `session.status_idle turn_outcome:"canceled"`；未知/已结束轮次 `canceled:false`。
- `session.close{session_id, reason?}` → `{closed}`：先等同 `turn.cancel`，等事件全部 ACK，释放本地对话，再 `session.ended`。

与手的方法区别：`tool.cancel` 针对一个 `tool.call`；`session.stop` 针对 `session.assign` 建的手会话；`session.stop`/`session.ended` 只是执行端资源回收，**不代表平台会话结束**，下一轮 `turn.start` 继续同一会话。

## 4. 事件

### 4.1 类型（Anthropic 名称与字段）

| `event.type` | 主要字段 |
| --- | --- |
| `agent.message` | `content[]` |
| `agent.thinking` | `content[]`（可空） |
| `agent.tool_use` | `name`、`input`、`evaluated_permission{type: allow\|ask\|deny}`；该事件的 `event_id` 即 `tool_use_id` |
| `agent.tool_result` | `tool_use_id`、`content[]`、`is_error` |
| `agent.mcp_tool_use` / `agent.mcp_tool_result` | `mcp_server_name`、`name`、`input` / `mcp_tool_use_id`、`content[]`、`is_error` |
| `session.status_running` | — |
| `session.status_idle` | `stop_reason{type: end_turn\|retries_exhausted}`、`turn_outcome: completed\|canceled\|failed`（BMA 扩展） |
| `session.error` | `error{type, message}` |

`user.*` 事件由平台写（来自 `turn.start`/`turn.input`/`turn.cancel`/interaction 答复），executor 不得发送；`stop_reason:"requires_action"` 也由平台按未决 interaction 投影，executor 不发。

### 4.2 持久事件、sequence、ACK 与补报

```json
{"jsonrpc": "2.0", "id": "exe_42", "method": "session.event",
 "params": {"session_id": "sesn_…", "turn_id": "turn_…", "event_id": "evt_0004", "sequence": 4,
            "event": {"type": "agent.tool_use", "name": "bash", "input": {"command": "go test ./pkg/foo"}, "evaluated_permission": {"type": "ask"}}}}
```

- `event_id`：executor 分配，会话内唯一，重发不变。`sequence`：会话内从 1 起每个持久事件 +1。
- 平台持久化后才响应 `{"accepted": true, "event_id": "…", "acked_sequence": 4}`——**响应即 ACK**。
- 平台规则：`sequence <= acked_sequence` 视为重复，直接 accepted；`sequence != acked_sequence + 1` → `-32013`，`data.expected_sequence` 指明下一个。
- executor 规则：每会话一个有序 outbox，逐条发送；断线后，重新注册成功后先按序重发未 ACK 事件，再发新事件。
- 每轮固定框架：以 `session.status_running` 开头、`session.status_idle` 收尾（失败时先 `session.error`）。SDK 自动发出这两个事件，保证每个 `turn_id` 恰有一个终态。

### 4.3 终态确认

平台只有在接受了某轮的 `session.status_idle`、且其之前所有 sequence 都已持久化（由按序 ACK 天然保证）之后，才把该轮标记结束并对客户端发布。客户端（Desktop、手机）从平台会话事件流订阅（SSE，支持 `Last-Event-ID` 补读），不直连执行端。

### 4.4 流式预览

`session.event_delta{session_id, turn_id, event_id, event_type: agent.message|agent.thinking, index, text}`：通知，不 ACK、不重发、断线即丢。`event_id` 等于最终持久事件的 `event_id`；平台转成 Anthropic `event_start`（首个 delta）+ `event_delta`，持久事件到达后客户端以其替换预览。

## 5. 审批与问答

1. 本地 Agent 判定需要审批：先发 `agent.tool_use`（`evaluated_permission.type:"ask"`），等其 ACK 后再发 `interaction.request`，保证平台已有被引用的 tool_use（SDK 自动保证顺序）。
2. `interaction.request{session_id, turn_id, interaction_id, kind: permission|question, tool_use_id?, tool_name?, tool_input?, scopes?[once|session], question?{prompt, options[{id,label}], multi_select, allow_free_text}, expires_at}` → `{accepted, platform_interaction_id}`。平台映射为一条现有 agent interaction。
3. 用户在任一端答复后，平台发 `interaction.response{session_id, interaction_id, outcome: allow|deny|answered|expired|canceled, scope?, deny_message?, answers?, responder?{actor_id, surface}}` → `{accepted}`。

规则：

- 重复：`interaction.request` 按 `interaction_id` 幂等；`interaction.response` 先到者生效，之后同 id 答 `accepted:false`，平台据此把自己的副本标记为过时。
- 过期：平台到 `expires_at` 发 `outcome:"expired"`；executor 本地同样计时，到期视为拒绝并发 `interaction.cancel{reason:"expired"}`。
- 取消：`turn.cancel`/`session.close` 时 executor 撤回该轮全部未决请求（`reason:"turn_canceled"`）；本机 UI 先答复时发 `resolved_locally`；工具被取消时 `tool_canceled`。
- **本地授权**：平台答复只是输入，最终是否执行由本机 Agent 的本地策略决定（平台 allow 后仍可拒绝，以 `agent.tool_result is_error:true` 体现）。

## 6. SDK 用法

executor 侧：

```go
client.ServeAgent(myAgent, bmaexecutor.AgentCapability{
	Models:          []bmaexecutor.AgentModel{{ID: "my-model", ThinkingLevels: []string{"low", "high"}}},
	PermissionModes: []string{"default", "accept_edits"},
})
```

`myAgent` 实现 `AgentHandler{OpenTurn, RunTurn, CloseSession}`：`OpenTurn` 回报生效配置；`RunTurn` 用 `turn.Emit`（持久事件）、`turn.Delta`（预览）、`turn.Inputs()`（`turn.input`）、`turn.RequestInteraction`（审批/问答）完成工作；`CloseSession` 释放本地对话。SDK 负责 `event_id`/`sequence`、outbox、重连补报、running/idle 框架、interaction 等待/过期/撤回。

TypeScript：`client.serveAgent(handler, capability)` 与 `new Harness({ agent })`，示例 [`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs)。Python：`client.serve_agent(handler, capability)` 与 `Harness(agent=...)`，示例 [`agent_minimal.py`](../../executor/python/examples/agent_minimal.py)。

harness 侧（平台或测试用的模拟平台）：`HarnessOptions.Agent`（`AgentPlatform`）+ `Executor.StartTurn / SendTurnInput / CancelTurn / CloseAgentSession / RespondInteraction`。

完整流程示例：`cd executor && go run ./examples/agentexecutor`（[源码](../../executor/examples/agentexecutor/main.go)）（脚本化本机 agent + 模拟平台 + 可断网 TCP 代理：任务 → 流式事件 → 审批 → 结果 → 追问 → 追加输入 → 断线补报 → 取消 → 关闭）。

## 7. 待平台实现后才能端到端

- agent 配置选择“本机执行端”、平台把输入转成 `turn.start`/`turn.input`、事件入库并经 SSE/facade 发布、interactions 映射与过期——尚未实现。
- 在此之前，用 SDK 示例的模拟平台联调 executor 侧实现。
