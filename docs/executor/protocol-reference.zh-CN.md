# Executor Protocol 参考（v1 + v1.1）

[English](protocol-reference.md) · [总览](README.zh-CN.md)

本文是 BMA 平台提供、bma-executor-sdk 0.4.0 实现的 `bma.executor.v1` 与 `bma.executor.v1.1` 的完整参考。凡与旧文档不一致处，以本文和契约为准。状态标注：**【已有】**已发布或已上线；**【即将提供】**契约已定义、能力位门控、尚未提供；**【设计中】**无实现。

机器可读契约在本仓库：[`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml)（方法目录、`x-bma-since`、`x-bma-capability`、幂等键、错误）、[`executor.asyncapi.yaml`](../../contracts/bma/executor/executor.asyncapi.yaml) 与 [`schemas/`](../../contracts/bma/executor/schemas)；SDK 的协议类型由它生成。

---

## 1. 版本与通用规则

| 项 | 值 |
| --- | --- |
| 协议 / WebSocket 子协议 | `bma.executor.v1`、`bma.executor.v1.1` |
| 服务端选择 | 两者都提供时优先 v1.1，只回显一个 |
| 消息 | JSON-RPC 2.0，`params` 为对象（具名参数），不用 batch |
| `id` | 字符串；每个连接内唯一。服务端发出的形如 `srv_N`，bmad 发出的形如 `exe_N` |
| 未知字段 | 双方忽略（additive 演进） |
| 未知方法 | `-32601 method_not_found` |
| `jsonrpc` 不是 `"2.0"` | `-32600` |
| 非法 JSON | `-32700`（id 为空） |

版本规则：

- `executor.register.protocol` 必须等于协商出的子协议，否则 `-32010` 并以 `4003` 关闭。
- v1.1 是 additive：v1.1 服务端仍服务 v1 executor，且**从不向 v1 连接发送 v1.1 方法**；即使 v1 连接声明了 v1.1 能力也不生效。
- bmad 只接受 v1.1：服务端未选 v1.1 或 register 结果 `protocol` 不是 v1.1 时以 `1008` 关闭。

---

## 2. 连接与鉴权

### 2.1 端点

| 路径 | 机器路径（bma-api） | environment 路径（managed-agents） |
| --- | --- | --- |
| token | `POST /api/v1/executors/token` | `POST /v1/executors/token` |
| 通道 | `GET /api/v1/executors/channel`（WebSocket） | `GET /v1/executors/channel` |
| 产物 | — | `POST /v1/executors/artifacts`、`GET /v1/executors/artifacts/{id}` |

bma-api 只有在配置了 executor token secret 时才挂载。`bmad executor --executor-url` 传的是带 `/api` 的 origin，例如 `https://api.bma.beeos.ai/api`。

### 2.2 换 token：`POST …/v1/executors/token`

```http
POST /api/v1/executors/token
Authorization: Bearer <机器 daemon token 或 environment key>
Content-Type: application/json

{"environment_id": "", "executor": {"name": "bmad", "os": "darwin", "arch": "arm64", "version": "0.2.11"}}
```

- `environment_id` 为空 → 机器路径：用机器 daemon token 鉴权，只在安装了机器 hooks 的服务端可用（否则 404）。
- `environment_id` 非空 → environment 路径：接受 environment key，或对该 environment 所属项目有有效授权的机器 token。

响应：

```json
{"executor_id": "exec_<machine uuid>", "token": "<jwt>", "expires_in": 299,
 "ws_url": "wss://api.bma.beeos.ai/api/v1/executors/channel", "channel_scope": ["control"]}
```

| 凭据 | `executor_id` | `channel_scope` | JWT 关键 claim |
| --- | --- | --- | --- |
| 机器 token（机器路径） | `exec_<machine_id>`，跨刷新稳定 | `["control"]` | `org_id`、`machine_id`、`daemon_token_id`；无 project/environment |
| 机器 token（environment 路径） | `exec_<machine_id>` | `["control","mcp"]` | `org_id`、`project_id`、`environment_id`、`machine_id` |
| environment key | `exec_<key_id>`，跨刷新稳定 | `["control","mcp"]` | `org_id`、`project_id`、`environment_id` |

JWT：HS256，`iss=bma`、`aud=bma.executor.v1`（两个版本都用这个 audience）、`nbf=now-5s`、TTL 默认 5 分钟。

错误：缺少/无效凭据 `401`；environment 不存在或不在凭据范围内 `404`（不泄露是否存在）；服务端未提供该路径 `404`。

### 2.3 打开通道

```http
GET /api/v1/executors/channel?channel=control
Sec-WebSocket-Protocol: bma.executor.v1.1
Authorization: Bearer <jwt>
```

- `channel` 缺省为 `control`；另一个取值 `mcp`（见 §7）。JWT 只放在 `Authorization` 头，不进 URL。
- 升级前的 HTTP 错误：

| 状态 | 原因 |
| --- | --- |
| 400 | `channel` 非 `control`/`mcp`；未提供 `bma.executor.v1` 或 `bma.executor.v1.1` 子协议 |
| 401 | 缺少 / 无效 JWT，或 claim 不完整 |
| 403 | JWT 的 `channel_scope` 不含该通道（例如机器 token 打开 `mcp`） |
| 404 | 机器 JWT 连到不服务机器 executor 的服务端 |
| 503 | 服务端未配置 executor 通道 |

- 第一帧必须是 executor 发出的 `executor.register`（§4.1）。
- 服务端在 JWT 到期时以 `4001` 关闭连接；executor 重新换 token 并重连。

### 2.4 WebSocket close code

| code | 谁发 | 含义 | executor 应对 |
| --- | --- | --- | --- |
| 1000 | 双方 | 正常关闭（含 `executor.close`） | 按自身意图 |
| 1001 | 服务端（MCP 通道） | MCP 通道被同 key 的新连接替换，或服务端 drain | 需要时重开 |
| 1008 | executor（bmad） | 注册失败 / 未协商到 v1.1 | 退避重连 |
| 1009 | 服务端（MCP 通道） | MCP 帧超过 1 MiB | 修正后重开 |
| 4001 | 服务端 | JWT 到期 | 重新换 token 后重连 |
| 4002 | 服务端（机器路径） | 机器运行时注册被拒（例如未声明 `process.enabled`），或这台机器的 daemon runtime 已结束（权威丢失） | 重新换 token；若 token 端点返回 401（token 被撤销）则停止 |
| 4003 | 服务端 | `register.protocol` 与子协议不一致 | 修正实现，不要盲目重连 |
| 4004 | 服务端 | 同一 executor id 的新连接已注册，本连接被替换 | **停止，不重连**（另一个执行端是权威） |
| 4005 | 服务端 | drain 完成 | 重连（会落到其他副本/新版本） |

> 旧 v1 文档把 4002 写成“token revoked”、把 1011 列为内部错误，与代码不符；以本表为准。selfhost worker 收到 4002 停止重连，bmad 对 4002 按普通断线退避重连，重连时 token 端点决定是否还能继续。

### 2.5 重连

bmad 的参考行为：任何非 `4004` 的断开都以 1s 起、翻倍、上限约 30s 的退避重连；每次重连都重新换 token。重连时 `executor.register` 带 `resume`（§8.1）。

SDK 客户端（Go、TypeScript、Python，0.4.6 起）只在 `4003`、`4004` 时停止；其他关闭（含服务端重启时的 `4002`、`4005`、`1001`、`1008`、`1012`、`1013`）同样退避重连。

---

## 3. 能力位

`executor.register.capabilities` 是对象；未知成员被忽略。服务端**据以门控**的只有下表“门控”列为是的成员：

| 能力位 | 形状 | 门控 | 打开后服务端会发送 | 状态 |
| --- | --- | --- | --- | --- |
| `process.enabled` | `{enabled, max_processes?, buffer_bytes?, pty?, file_transfer?, git_credentials?}` | 是 | `process.*`；机器路径**必需**（否则 4002） | 【已有】 |
| `process.pty` | bool | 是 | `process.start` 的 `io_mode:"pty"` | 【已有】 |
| `process.file_transfer` | bool | 是 | `process.start` 的 `file_transfer` | 【已有】 |
| `process.git_credentials` | bool | 是 | `process.start` 的 `git_credentials:true` | 【已有】 |
| `terminal.enabled` | `{enabled, kinds?, max_sessions?}` | 是 | `terminal.open/attach/close` | 【已有】 |
| `skills.install` | `{install}` | 是 | `skill.install` | 【已有】 |
| `artifacts.upload` | `{upload, max_bytes?}` | 否（信息） | — | 【已有】 |
| `shell`、`files`、`mcp`、`chrome`、`computer_use`、`display` | 见契约 `schemas/executor.json` | 否（信息） | — | 【已有】（v1 字段；`display` 为 v1 bool 或 v1.1 `{rfb}`） |
| `workspace.backup` | `{backup}` | 是 | `workspace.backup`；启动前恢复 | 【已有】 |
| `agent` | `AgentCapability` | 是 | `turn.*`、`session.close`、`interaction.response` | 契约与 SDK【已有】；平台【即将提供】，见 [agent 能力](agent-capability.zh-CN.md) |

规则：

- 能力门控只在 v1.1 连接上生效；v1 连接永远收不到 v1.1 方法。
- 没有声明能力位的一方若仍收到对应方法，应答 `-32012 capability_unavailable`；`tool.call` 收到不服务的工具时，`data.capability = "tool:<name>"`。
- harness 侧发现 executor 未声明所需能力时不会换一条传输重试（`ErrCapabilityUnavailable`）。
- `executor.capabilities.update`：服务端**接受但当前不应用**（只回 `accepted`）。能力变化需要重连重新注册才生效。

bmad 实际声明的能力：`shell`、`process{enabled, max_processes:64, buffer_bytes:1048576, pty: 非 Windows, file_transfer/git_credentials: 机器路径}`、`terminal`（PTY 宿主可用时）、`display{rfb:false}`、`skills{install}`（机器路径）、`artifacts`（仅 environment 路径）。

---

## 4. 方法

方向：H→E = harness 发给 executor；E→H = executor 发给 harness。“req”需要响应；“notif”无 `id`。v1.1 列为空表示 v1 起就有。

### 4.0 方法总表

| 方法 | 方向 | 类型 | 始于 | 能力位 | 幂等键 |
| --- | --- | --- | --- | --- | --- |
| `executor.register` | E→H | req | v1 | — | —（每连接一次） |
| `executor.heartbeat` | E→H | req | v1 | — | — |
| `executor.capabilities.update` | E→H | notif | v1 | — | — |
| `executor.drain` | H→E | req | v1 | — | — |
| `executor.close` | 双向 | notif | v1 | — | — |
| `session.assign` | H→E | req | v1 | — | `session_id` |
| `session.ready` | E→H | req | v1 | — | — |
| `session.event` | E→H | req | v1 | — | JSON-RPC `id` |
| `session.status` | E→H | notif | v1 | — | — |
| `session.stop` | H→E | req | v1 | — | — |
| `session.ended` | E→H | req | v1 | — | — |
| `tool.call` | H→E | req | v1 | 工具名 | `tool_call_id` |
| `tool.result` | E→H | req | v1 | — | `tool_call_id` |
| `tool.cancel` | H→E | req | v1 | — | — |
| `tool.progress` | E→H | notif | v1 | — | — |
| `process.start` | H→E | req | v1.1 | `process.enabled` | `(session_id, process_id)` |
| `process.read` | H→E | req | v1.1 | `process.enabled` | 只读 |
| `process.write` | H→E | req | v1.1 | `process.enabled` | JSON-RPC `id` |
| `process.interrupt` | H→E | req | v1.1 | `process.enabled` | 对已结束进程无副作用 |
| `process.terminate` | H→E | req | v1.1 | `process.enabled` | 对已结束进程无副作用 |
| `process.list` | H→E | req | v1.1 | `process.enabled` | 只读 |
| `process.status` | E→H | notif | v1.1 | `process.enabled` | 建议性 |
| `terminal.open` | H→E | req | v1.1 | `terminal.enabled` | `terminal_id` |
| `terminal.attach` | H→E | req | v1.1 | `terminal.enabled` | — |
| `terminal.close` | H→E | req | v1.1 | `terminal.enabled` | `terminal_id` |
| `skill.install` | H→E | req | v1.1 | `skills.install` | `(skill_id, revision_id, digest)` |
| `workspace.backup` | H→E | req | v1.1 | `workspace.backup` | `backup_id` |
| `turn.start` / `turn.input` / `turn.cancel` / `session.close` / `interaction.response` | H→E | req | v1.1 | `agent` | 见 agent 文档 |
| `session.event_delta` | E→H | notif | v1.1 | `agent` | — |
| `interaction.request` / `interaction.cancel` | E→H | req | v1.1 | `agent` | `interaction_id` |

> 注意方法名是 `skill.install`（单数），不是 `skills.install`；`skills.install` 是能力位名。

### 4.1 `executor.register`（E→H，req）

参数：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `protocol` | 是 | 必须等于协商的子协议 |
| `environment_id` | 是 | 机器路径传 `""`；environment 路径必须等于 JWT 中的 environment，否则 `-32002` |
| `executor` | 是 | `{name, os, arch, version}`。机器路径的 `version` 会作为 daemon runtime 版本校验，必须是 `MAJOR.MINOR.PATCH` 或 `0.0.0-dev`，否则注册被拒（4002）；`os` 用于决定默认 shell |
| `capabilities` | 是 | §3 |
| `resume` | 否 | `{executor_id, last_connection_id, active_sessions[]}`，§8.1 |

结果：

```json
{"executor_id": "exec_…", "connection_id": "…", "server_time": "2026-10-31T15:00:00Z",
 "heartbeat_interval_ms": 15000, "lease_ttl_ms": 60000,
 "max_control_frame_bytes": 65536, "max_mcp_frame_bytes": 1048576, "drain": false,
 "protocol": "bma.executor.v1.1",
 "limits": {"max_inflight_requests": 8, "max_concurrent_process_reads": 4, "max_process_read_bytes": 32768,
            "max_process_write_bytes": 32768, "max_processes_per_session": 64, "max_artifact_bytes": 16777216}}
```

`protocol`、`limits` 只在 v1.1 返回。

错误：`-32602`（参数/能力对象非法、`resume.executor_id` 格式非法）、`-32600`（重复 register）、`-32001`（机器凭据下 `resume.executor_id` 与 JWT 不符）、`-32002`（environment 不匹配）、`-32010`（协议不符，随后 4003）、`-32603`。

注册成功后：同 executor id 的旧连接以 4004 关闭；机器路径调用平台 hooks 绑定 daemon runtime，失败以 4002 关闭。

### 4.2 `executor.heartbeat`（E→H，req）

参数：`{connection_id, active_sessions[], load?{cpu_percent, memory_percent}}`；`connection_id` 必须是本连接的，否则 `-32602`。结果：`{server_time, lease_ttl_ms, drain}`。按 `heartbeat_interval_ms`（默认 15s）发送；每次心跳续期该连接上工作项的租约，机器路径续期 daemon runtime 租约与在线状态。租约到期（默认 60s，最大 10 分钟）后，服务端对相关会话发 `tool.cancel` + `session.stop`（reason `lease_expired`）。`drain:true` 表示不要再接新会话。

### 4.3 `executor.capabilities.update`（E→H，notif）

`{capabilities}`。见 §3：当前服务端不应用。

### 4.4 `executor.drain`（H→E，req）

参数 `{reason, deadline_ms?}`；结果 `{accepted, active_sessions[]}`。服务端 drain 流程：停止派发新会话 → 发 `executor.drain` → 等活跃会话结束（或截止）→ 以 4005 关闭。

### 4.5 `executor.close`（双向，notif）

`{reason}`，之后以 1000/1001 关闭。服务端收到后以 1000 关闭。

### 4.6 `session.assign`（H→E，req）

参数：

| 字段 | 说明 |
| --- | --- |
| `session_id`、`work_id` | 会话与工作项 id。harness 会话中二者相同；机器路径固定为 `sess_machine_<machine public id>` |
| `agent_id` | 可选 |
| `environment_id` | 机器路径为 `""` |
| `deadline_ms` | 可选；会话寿命。机器进程会话不设（0），executor 不得用寿命/空闲计时器杀掉存活进程 |
| `lease` | `{token, fencing_token}` |
| `resources[]` | 可选；`{type:"memory_store", memory_store_id, mount_path}`。当前平台不发送 |
| `mcp_servers[]` | 可选；`{server_key, connection_origin: service\|environment, transport{type: stdio\|http, command, args, cwd, env, timeout_ms, command_sha256, url}}`。在契约中定义，平台目前不发送【即将提供】 |
| `metadata` | 字符串 map。`source:"harness"` = 长寿命 harness 会话（多个调用）；其他 = 一次性会话，完成唯一的工具调用后结束 |

结果 `{accepted, session_id}`。超过容量答 `-32009`（带 `data.retryable:true`）。重复 assign 同一 `session_id` 是幂等的（bmad 不重建）。

### 4.7 `session.ready`（E→H，req）

`{session_id, work_id, lease_token}` → `{accepted}`。在工作区/MCP 准备好之后发送；`work_id`/`lease_token` 不符答 `-32003`，会话不存在答 `-32007`。harness 在收到 ready 前不会派发调用。

### 4.8 `session.event`（E→H，req）

`{session_id, event}` → `{accepted, event_id}`。`event` 必须是带非空 `type` 的 JSON 对象，类型名沿用 Anthropic Managed Agents；**必须带 JSON-RPC `id`**，服务端以该 id 作为幂等键持久化（同一 id 重发返回同一事件）。只在 environment 路径存储；v1 契约中唯一列出的类型是 `agent.tool_result`。agent 能力在此基础上增加 `turn_id`/`event_id`/`sequence` 与按序 ACK（见 [agent 能力](agent-capability.zh-CN.md)）。

### 4.9 `session.status`（E→H，notif）

`{session_id, status: starting|running|idle|stopping|stopped|error, detail?}`。服务端当前只接受、不处理。

### 4.10 `session.stop`（H→E，req）

`{session_id, reason, deadline_ms?}` → `{stopped}`。executor 终止该会话全部进程与工具调用（SIGTERM，宽限后 SIGKILL；bmad 宽限 2s），然后发 `session.ended`。未知会话 `-32007`。

### 4.11 `session.ended`（E→H，req）

`{session_id, reason}` → `{accepted}`。在会话所有进程都已退出后发送，**必须重试直到被接受**（包括跨重连）。bmad 的 reason：`stopped`（因 `session.stop`）、`completed`（一次性会话完成）。

### 4.12 `tool.call`（H→E，req）

参数：`{session_id, tool_call_id, tool, input, deadline_ms?, permission?{mode: always_allow|always_ask}}`。结果 `{accepted}`——只表示已受理，结果另由 `tool.result` 回报。

- 工具名枚举：`bash`、`read`、`write`、`edit`、`glob`、`grep`、`list_files`、`computer_observe`、`computer_action`（契约 `schemas/tool.json`）。executor 只需实现自己声明的工具；其他答 `-32012`，`data.capability:"tool:<name>"`。
- `bash` 输入：`{command, cwd?, timeout_ms?, env?}`；bmad 只实现 `bash`。
- 同一 `tool_call_id` 重复：答 `accepted:true`，**不重复执行**。注意：bmad 不会因为重复的 `tool.call` 重发已完成的结果；结果的送达靠 executor 侧对 `tool.result` 的重试（§4.13）。SDK `Harness` 的 `CallTool` 会在本地记录结果，重复调用直接返回记录。
- 平台使用：environment 路径的工作项映射为 `bash`/`read`/`write`/`list_files`；**机器路径不使用 `tool.call`**，平台工具全部走 `process.*`。

### 4.13 `tool.result`（E→H，req）

参数：`{session_id, tool_call_id, outcome: succeeded|failed|canceled, is_error, content[], duration_ms?, artifacts[]?}` → `{accepted}`。

- 按 `tool_call_id` 幂等；重复结果答 `accepted:true`。
- executor 必须在 `-32009` 等可重试错误上退避重试，并跨重连重试直到被接受（bmad：`-32009` 最多重试 10 次、每次翻倍；跨重连最长等一个租约 TTL）。
- `content[]` 块：`{type: text|image|resource|artifact, text?, data?, mime_type?, url?, artifact_id?}`。大输出不要塞 base64：environment 路径先 `POST /v1/executors/artifacts` 再用 `artifact` 块引用（§6.1）；`artifacts[]` 中的每个引用都必须属于本会话，否则 `-32002`。
- 超过 64 KiB 的 `tool.result` 服务端仍会接收（入站上限约 17 MiB）并转存为产物，而不是断开连接。bmad 自己把超过 48 KiB 的 bash 结果转为产物（environment 路径）或截断并注明（机器路径）。
- 错误：`-32007`（会话不活跃）、`-32002`（harness 会话中未知的 `tool_call_id`，不可重试）、`-32003`（租约已被新的 fencing 代替）、`-32602`（缺 `outcome`）。

### 4.14 `tool.cancel`（H→E，req）

`{session_id, tool_call_id, reason?}` → `{canceled}`。executor 杀掉该调用的进程组；调用未知或已完成时 `canceled:false`。被取消的调用仍要回报 `tool.result`，`outcome:"canceled"`。

### 4.15 `tool.progress`（E→H，notif）

`{session_id, tool_call_id, message}`，可选，服务端当前只接受。

### 4.16 `process.*`（v1.1，`process.enabled`）

所有 `process.*` 都作用于一个已分配会话内的进程；`process_id` 由 harness 分配。

#### `process.start`

参数：

| 字段 | 说明 / 校验 |
| --- | --- |
| `session_id`、`process_id` | `process_id` 必填，≤128 字节 |
| `argv` | 1–256 项，每项 ≤4 KiB，合计 ≤16 KiB；不经 shell。`file_transfer` 时必须为空 |
| `cwd` | 解析到工作根之内 |
| `env` | 显式环境，≤64 项、≤16 KiB；禁止 `LD_PRELOAD`、`DYLD_INSERT_LIBRARIES`、`NODE_OPTIONS` 等注入变量；只继承宿主的 `PATH`、`HOME`、`USER`、`LANG`、`TMPDIR` |
| `stdin` | `open` / `closed`；字节通过 `process.write` 发送 |
| `stderr` | 空 = stdout/stderr 两个流；`"merge"` = OS 级 dup2 合并到 stdout（一个游标）。机器路径总是用 `merge`，executor 必须在结果里回显 `stderr:"merge"`，否则平台按 `-32012` 失败 |
| `wait_ms` | 0–300000；等待进程结束最多这么久再应答 |
| `timeout_ms` | 0–86400000；到时杀进程，状态 `failed`、`reason:"timeout"` |
| `metadata` | 字符串 map；平台会带 `tool_call_id`；session files 引入 `agent_session_id` |
| `io_mode` | `"pty"`：在 24x80 PTY 上运行；需要 `process.pty`；`close_stdin` 被拒 |
| `file_transfer` | `{direction: upload\|download, local_path}`，替代 `argv`；需要 `process.file_transfer`；与 `argv`/`io_mode`/`git_credentials` 互斥（§6.2） |
| `git_credentials` | `true`：为该进程安装 `https://github.com` 的 Git credential helper；需要 `process.git_credentials`（§6.3） |

结果 `ProcessState`：`{process_id, state: running|exited|failed|killed, pid?, started_at?, ended_at?, reason?, cursors{stdout, stderr}, exit_code, exit_signal, stderr?, accepted?, file_transfer?}`。

规则：

- 按 `(session_id, process_id)` 幂等：同 id 同规格返回现有状态；同 id 不同规格 `-32013`，`data.reason:"process_id_spec_mismatch"`。
- 运行中进程达到 64 个：`-32009`，`data.retryable:true`。已结束进程不计入，保留状态与输出直到会话结束（超过 64 MiB 保留输出时最老的进程丢弃输出、读到 gap；超过 1024 个已结束进程时最老的被遗忘）。
- 拉起失败不产生进程状态，返回错误且 `data.reason:"spawn_failed"`。
- 参数里出现 `pty` 字段（而不是 `io_mode`）答 `-32602`。

#### `process.read`

参数 `{session_id, process_id, stream: stdout|stderr, cursor, max_bytes?(1–32768，默认 32768), wait_ms?(0–300000)}`。结果 `{process_id, stream, data(base64), from_cursor, next_cursor, earliest_cursor, gap, truncated, eof, state, ended_at?, reason?, exit_code, exit_signal, file_transfer?}`。

- 游标是流内字节偏移；每个流有 1 MiB 环形缓冲。`cursor < earliest_cursor` 时 `gap:true`，返回从 `earliest_cursor` 开始的位置。
- 只有在调用方已读到活流末尾时才长轮询到 `wait_ms`；同一连接上带 `wait_ms` 的并发读最多 4 个，超过 `-32009` 可重试。
- `cursor` 越界 `-32602`；进程未知 `-32002`。

#### `process.write`

`{session_id, process_id, data?(base64, ≤32 KiB 解码后), close_stdin?}`，二者恰有其一 → `{accepted}`。按 JSON-RPC `id` 去重（重放同一请求不会二次写入）。stdin 已关闭 `-32013`（`data.reason:"stdin_closed"`）；PTY 进程 `close_stdin` 答 `-32012`。

#### `process.interrupt` / `process.terminate`

参数 `{session_id, process_id, grace_ms?}`，结果 `ProcessState`（`accepted:true`）。`interrupt` 发 SIGINT；`terminate` 向进程组发 SIGTERM，`grace_ms`（默认 5000，最大 30000）后 SIGKILL，返回时进程已退出；对已退出进程无副作用。

#### `process.list`

`{session_id}` → `{processes: ProcessState[]}`，按开始时间排序。平台在 executor 每次（重新）注册时用它做对账（§8.2）。

#### `process.status`（E→H，notif）

`{session_id, process_id, state, exit_code?, exit_signal?, ended_at?, reason?, observed_at}`。**建议性**：harness 以 `process.read`/`process.list` 为准。

### 4.17 `terminal.*`（v1.1，`terminal.enabled`，机器路径）

控制面在控制通道，PTY 字节**不走控制通道**。

| 方法 | 参数 | 结果 | 规则 |
| --- | --- | --- | --- |
| `terminal.open` | `{terminal_id, kind: human\|agent, shell?, argv?, cwd?, env?, rows?, cols?}` | `{terminal_id, epoch, pid, rows, cols, earliest_seq, latest_seq, created_at}` | 按 `terminal_id` 幂等；`rows/cols` 只用于新建，其余规格与现存不符 `-32013`；PTY 退出/关闭后同 id 重建时 `epoch` 递增 |
| `terminal.attach` | `{terminal_id, epoch, stream_id, can_write, from_seq, ack_seq, host{url, ticket}}` | `{accepted, writer_epoch}` | executor 用 `Authorization: Bearer <ticket>`（一次性短期票据，不进 URL）、子协议 `bma.terminal.v1` 拨出到 `host.url`，写出 `attached` 帧后才应答；关闭该 socket = detach，PTY 保留 |
| `terminal.close` | `{terminal_id, reason?}` | `{closed, exit_code?}` | 幂等，未知/已关闭也返回 `closed:true` |

错误 `data.terminal_code` 携带 `bma.terminal.v1` 错误码（如 `terminal_writer_busy`），平台原样转给 attach 端。host socket 上的帧（`attached/output/gap/exit/closed/resized/error` 与 `input/ack/resize`）与浏览器终端合同相同，见。

### 4.18 `skill.install`（v1.1，`skills.install`，机器路径）

参数 `{machine_id, skill_id, revision_id, digest, download_token, download_expires_at(unix 秒)}` → `{state: ready|failed, error_code?, error?}`。executor 用 `download_token` 从 `GET /api/v1/daemon/skills/{id}/archive` 下载、校验 digest、安全解包；按 `(skill_id, revision_id, digest)` 幂等。

### 4.19 `workspace.backup`（v1.1 附加，`workspace.backup`）【已有】

参数 `{backup_id, paths[]（绝对路径）, exclude[]?, skip_if_fingerprint?}` → `{state: uploaded|unchanged|failed, fingerprint?, digest?, size_bytes?, entries?, skipped?, error?}`。executor 打包 tar+zstd，`PUT /api/v1/daemon/machine-backups/{backup_id}/archive`（机器 token，头 `X-BMA-Backup-Fingerprint`）；指纹等于 `skip_if_fingerprint` 时不上传、返回 `unchanged`。从不包含 executor 自己的 `BMA_HOME`。恢复没有方法：重建后的 executor 在 `executor.register` **之前** `GET /api/v1/daemon/machine-backup-restore`，解包后 `POST …/machine-backups/{id}/restore-result`，机器带着文件上线。

---

## 5. 错误码

JSON-RPC 错误：`{code, message, data?}`；`data` 可含 `retryable`、`detail`、`capability`、`reason`、`terminal_code`。

| code | message | 含义 / 何时出现 |
| --- | --- | --- |
| -32700 | parse_error | 非法 JSON |
| -32600 | invalid_request | 非 JSON-RPC 2.0；重复 register |
| -32601 | method_not_found | 未知方法 |
| -32602 | invalid_params | 参数校验失败（含限额超出，如 argv、write 大小） |
| -32603 | internal_error | 对端内部错误 |
| -32000 | unauthorized | 凭据无效 |
| -32001 | forbidden | 租户/策略拒绝；`resume.executor_id` 与机器凭据不符 |
| -32002 | not_found | 资源不存在（跨租户也返回它，不泄露存在性）；未知进程/调用/产物 |
| -32003 | lease_expired | 租约/fencing 失效 |
| -32004 | timeout | 截止时间到 |
| -32005 | canceled | 被对端取消 |
| -32006 | payload_too_large | 超过帧上限 |
| -32007 | session_not_active | 会话未分配或已结束 |
| -32008 | mcp_unavailable | MCP server 不可用 |
| -32009 | executor_busy | 容量满；一般带 `data.retryable:true`，**必须退避重试，不得静默丢弃** |
| -32010 | unsupported_protocol | 协议版本不符 |
| -32011 | resource_unavailable | v1.1：资源不可用 |
| -32012 | capability_unavailable | v1.1：未声明该能力；`data.capability` 指明 |
| -32013 | conflict | v1.1：与现状冲突；`data.reason` 指明（如 `process_id_spec_mismatch`、`stdin_closed`） |

**“确定拒绝”与“结果未知”必须区分**：

- 对端返回的 JSON-RPC 错误 = 确定的答复。
- 本端传输失败（超时、连接关闭、发送失败、无法解码的答复）= 结果未知：请求可能已经生效。BMA harness 把它当作传输失败，不会当成拒绝、也不会对有副作用的请求盲目重发，交给重连对账决定。唯一例外：本地出站队列满时什么都没发出去，返回可重试的 `-32009`。
- 协议里**没有** `outcome:"unknown"` 这样的线上取值；“unknown”是 harness 侧状态：SDK `Harness` 用 `ErrToolOutcomeUnknown` 表示 executor 丢失前没收到 `tool.result`；平台机器路径把对账时找不到的已授予进程记为 unknown，绝不重跑。

---

## 6. 数据面与机器 HTTP 路由

控制通道只传控制消息。字节走以下 HTTP/WebSocket 路由。

### 6.1 产物（environment 路径）【已有】

`POST /v1/executors/artifacts`，`Authorization: Bearer <executor JWT>`，头 `X-BMA-Session-ID`、`X-BMA-Artifact-Digest`（`sha256:<hex>`）、`Content-Type`、可选 `X-BMA-Artifact-Name`；body 为原始字节，≤16 MiB。digest 不符 `409`、过大 `413`；按 `(session, digest)` 幂等，新建 `201`、重放 `200`，返回 `ArtifactRef{artifact_id, name, mime_type, size_bytes, digest, created_at}`。之后在 `tool.result` 中引用。机器路径暂无产物存储。

### 6.2 文件传输（机器路径）【已有】

`process.start` 带 `file_transfer` 时，executor 在进程内把一个文件在机器与该进程的平台文件端点之间搬运：`POST`（上传，`?filename=`）/ `GET`（下载）`/api/v1/daemon/processes/{process_id}/file`，机器 token 鉴权。

- 上传：只接受普通文件；executor 侧上限 1 GiB（服务端对特定目标可能更严）；边传边算 sha256，与服务端存储的 digest 比对。
- 下载：写入目标同目录临时文件，长度与 sha256（响应头 digest）都匹配后才原子替换，保留原文件权限；上限 1 GiB。
- 结果在 `ProcessState.file_transfer`：`{path?, digest?, error?{code, error, current_digest?}}`。

### 6.3 Git 凭据（机器路径）【已有】

`git_credentials:true` 时，executor 为该进程设置 `GIT_CONFIG_PARAMETERS` 指向 helper（`bmad __bma_git_credential`），helper 通过 executor 私有本地 socket（带每进程随机能力串，仅在进程运行期间有效）取凭据，executor 再从 `/api/v1/daemon/processes/{id}/git-credentials` 取短期 token。凭据不落盘。

### 6.4 session files（`/mnt/session`）【已有】

对齐 Anthropic Managed Agents files：挂到会话的文件在机器上可读于 `/mnt/session/uploads/<mount_path 或 file_id>`；agent 写到 `/mnt/session/outputs/` 下的文件成为会话作用域文件。

- 平台在每个 shell 进程的 `process.start.metadata.agent_session_id` 中标明会话；机器显示“最后一个启动 shell 进程的会话”，切换会话时先上传旧会话 outputs，再清空并同步新会话。
- uploads：进程启动前 `GET /api/v1/daemon/processes/{id}/session-files` 拿清单，不在清单内的内容被删除；缺失/变化的文件从 `…/session-files/{resource_id}/content` 下载，校验长度与 sha256 后 `chmod 0444` 原子落盘。同步失败则进程启动失败（`session files: …`）。
- outputs：每 2s 扫描，两次扫描间大小与 mtime 不变且自上次上传后有变化的普通非空文件，`POST …/session-outputs?path=<相对路径>` 带 `X-BMA-File-Digest`；按 (会话, sha256, 路径) 幂等。限额：单文件 1 GiB、路径 512 字节、每次扫描 10000 个文件。`413/422/400` 不重试直到文件变化；传输错误、`404`、`409` 下次扫描重试。
- 鉴权：机器 token，锚定在调用机器上一个已授予、会话活跃的进程；挂载路由要求进程 starting/running，outputs 允许进程结束后 15 分钟内上传。

### 6.5 PTY 数据面

见 §4.17：executor 按 `terminal.attach` 给出的 `host.url` 与一次性 ticket 外连，帧协议 `bma.terminal.v1`。

---

## 7. MCP 通道

【已有：服务端通道（environment 路径）、SDK executor 端 `OpenMCPChannel`、SDK `Core` stdio supervisor、SDK `Harness` MCP 通道】【即将提供：平台发送 `session.assign.mcp_servers`】【未实现：bmad 侧 MCP；平台 harness 在生产路径向 executor 派发 MCP server】

- 边界：脑运行 MCP client；手启动或连接 MCP server，原样转发 MCP JSON-RPC。没有 `mcp.start/frame/stop` 等 BMA 方法，没有包装。
- 打开：`GET …/v1/executors/channel?channel=mcp&session_id=<id>&server_key=<key>`，子协议 `bma.executor.v1`，需要 JWT 含 `mcp` 通道（机器 token 的机器路径 JWT 不含，403）。
- 每个 `(environment, session_id, server_key)` 至多一条通道；新连接替换旧连接（旧的以 1001 关闭）。`session_id` ≤256、`server_key` ≤128 字节。帧上限 1 MiB（超出以 1009 关闭）。
- 每帧是一条 MCP JSON-RPC 消息；harness 做 `initialize`、`notifications/initialized`、`tools/list`、`tools/call`；executor 把 stdio server 的换行分隔 JSON-RPC 与通道帧双向桥接。MCP 错误原样透传；不得伪造空 `tools/list`。
- 配置（契约 `schemas/mcp.json`）：`connection_origin:"service"` 由 harness 自己连，executor 不启动进程；`"environment"` 由 executor 启动/连接。stdio：不经 shell，`cwd` 在会话工作区内，`env` 显式（不继承宿主环境），给了 `command_sha256` 就校验。
- 生命周期（SDK `Core` 行为）：每个 (brain, session, server_key) 一个进程；任一侧结束则带退避重启（通道上看到的是新 server，需要重新 initialize）；连续 5 次失败发 `session.status` `error`、`detail:"mcp_unavailable: <key>"`；会话结束关闭全部；不同会话的调用、取消、结果互不共享 stdio 管道。

### 7.1 Chrome MCP 与 computer-use（执行端要求）

平台不检查以下规则，但接入方的执行端必须遵守：

- Chrome 运行在执行端；CDP 只绑定 `127.0.0.1`（`--remote-debugging-port=0`、`--remote-debugging-address=127.0.0.1`，读 `DevToolsActivePort`），绝不暴露到网络；每会话独立 user-data-dir（0700），会话结束删除。Chrome MCP 工具就是标准 MCP 工具，没有 BMA 专有方法。
- computer-use：优先用 BMA 原生桌面工具；第三方 computer-use MCP 跑在执行端并受同样的授权约束。人工接管期间执行端必须拒绝输入动作；断线、取消、交还控制时释放按键与鼠标。

---

## 8. 幂等与断线恢复

### 8.1 重连时的 `resume`

executor 重新注册时带 `resume{executor_id, last_connection_id, active_sessions[]}`：

- 机器路径：`resume.executor_id` 必须等于凭据决定的 `exec_<machine_id>`，否则 `-32001`。平台若在 `active_sessions` 里看到机器进程会话 `sess_machine_<id>`，就 `AttachSession` 接回同一会话，而不是新建。
- environment 路径：服务端释放旧连接持有的这些会话的租约，让新连接重新认领；下一次认领推进 fencing token，旧 executor 迟到的结果会被拒。selfhost worker 目前不发 `resume`。
- executor 侧（bmad）：会话属于进程而非某条连接；断线后保留会话与进程一个租约 TTL，期间重连就带着它们恢复；超过租约 TTL 才以 `lease_expired` 停止全部会话。

### 8.2 机器路径的对账

每次注册，平台调用 `process.list`，把 executor 的进程表作为 daemon runtime 的对账声明：

- 平台已授予但 executor 没有的进程（executor 重启、`process.start` 丢失）→ 记为 unknown，**不重跑**；
- 仍在运行的进程 → 保留并继续观察；断线期间结束的进程 → 现在补报终态；
- executor 不保存动作日志，PostgreSQL 是日志：已受理但未确认生效的有副作用动作（写 stdin、发信号）按“已生效”声明，不重放，并记为 unknown。

### 8.3 幂等键汇总

| 请求 | 幂等键 | 重复时 |
| --- | --- | --- |
| `session.assign` | `session_id` | 不重建 |
| `tool.call` | `tool_call_id` | `accepted:true`，不重复执行 |
| `tool.result` | `tool_call_id` | `accepted:true` |
| `session.event` | JSON-RPC `id` | 返回同一 `event_id` |
| `process.start` | `(session_id, process_id)` + 规格 | 返回现状；规格不同 `-32013` |
| `process.write` | JSON-RPC `id` | 不二次写入 |
| `terminal.open/close` | `terminal_id` | 返回现存 PTY / `closed:true` |
| `skill.install` | `(skill_id, revision_id, digest)` | 不重复安装 |
| `turn.start` / `turn.input` / `interaction.*` | `turn_id` / `input_id` / `interaction_id` | 返回首次结果 |

### 8.4 必须重试的消息

`tool.result`、`session.ended`（以及 agent 能力的持久 `session.event`）丢失会破坏正确性：executor 在可重试错误与断线后都必须重发直到被接受；SDK executor 端在所有重试失败后调用 `OnToolResultLost` 交给适配器持久化重放，绝不静默丢弃（见 [Go executor README](../../executor/README.md)）。

---

## 9. 取消层级

| 层级 | 方法 | 作用范围 | 结果 |
| --- | --- | --- | --- |
| 单个工具调用 | `tool.cancel` | 一个 `tool_call_id` 的进程组 | `tool.result outcome:"canceled"` |
| 单个进程 | `process.interrupt` / `process.terminate` | 一个进程（SIGINT / SIGTERM→宽限→SIGKILL） | `ProcessState` |
| 当前一轮（agent） | `turn.cancel` | agent 会话当前轮次及其本地工具、未决 interaction | `session.status_idle turn_outcome:"canceled"`，会话可继续 |
| 会话 | `session.stop` | 会话内全部进程、工具调用、MCP server | `session.ended` |
| agent 会话 | `session.close` | 先等同 `turn.cancel`，等事件全部 ACK，释放本地对话 | `session.ended` |
| 连接 | `executor.drain` | 不接新会话，等现有会话结束 | 4005 |
| 租约到期 | — | 该连接上的会话 | 服务端 `tool.cancel`+`session.stop`（`lease_expired`）；executor 断线超过租约 TTL 自行停止 |
| 被替换 | 4004 | 整个 executor | executor 停止一切并退出 |

两个并行会话互不影响：取消、停止只作用于指定的会话/调用。

---

## 10. 限额

| 项 | 值 |
| --- | --- |
| 服务端→executor 控制帧 | 64 KiB（`max_control_frame_bytes`） |
| executor→服务端控制帧（服务端读上限） | 16 MiB + 1 MiB（为了把超大 `tool.result` 转存产物） |
| bmad 读上限 | 4 MiB |
| MCP 帧 | 1 MiB |
| 心跳间隔 / 租约 TTL | 15s / 默认 60s、最大 10 分钟 |
| JWT TTL | 5 分钟 |
| 注册超时 | 10s |
| 服务端→executor 并发请求 | 8（满时等待；本地队列满 `-32009` 可重试） |
| executor→服务端并发处理 | 8（满时 `-32009` 可重试） |
| `process.*` | 见 §4.16；运行中 64/会话；流缓冲 1 MiB；读/写 32 KiB；并发长轮询读 4；保留输出 64 MiB、已结束进程 1024 |
| bash 工具 | 每流捕获 1 MiB；内联 48 KiB；默认超时 10 分钟 |
| 产物上传 | 16 MiB |
| 文件传输 / session files 单文件 | 1 GiB |
| `executor_id` | `exec_` 前缀，≤128，`[A-Za-z0-9_-]` |

---

## 11. 安全与租户

- executor 永不开入站端口；平台永不拨入私网 IP。所有连接 TLS/WSS（`bmad` 只允许 loopback/`host.docker.internal` 用 http）。
- 长期凭据（机器 token / environment key）只用于换 token 与机器 HTTP 路由；控制通道只带短期 JWT。
- 每个方法都带 `session_id`/`tool_call_id` 并在服务端按 org/project/environment 或机器授权；跨租户返回 `-32002`。
- 命令不经 shell 拼接；显式环境；拒绝加载器注入变量；文件路径在符号链接解析后限定在允许根内。
- MCP 进程输出不可信；stderr 有界、不进入协议。
- 本机授权检查留在执行端：平台的答复（包括 interaction）只是输入，不直接等于本机授权。

---

## 12. 与 v1 文档的差异（代码为准）

| 旧描述 | 实际 |
| --- | --- |
| 4002 = token revoked；1011 内部错误 | 4002 = 机器 runtime 注册被拒/已结束；服务端不发 1011（§2.4） |
| 重连后 harness 回复最后确认的事件并重新下发挂起的 `tool_call_id` | 服务端不重新下发；结果靠 executor 重试送达；机器路径用 `process.list` 对账（§8） |
| “两次心跳未到即租约过期” | 租约按 `lease_ttl_ms` 续期/过期（§4.2） |
| `session.assign` 带 `mcp_servers` | 契约定义，main 服务端尚不发送（§4.6） |
| `executor.capabilities.update` 更新能力 | 服务端当前不应用（§3） |
| SDK `createHand({environmentKey})` | 实际 SDK 结构见[总览 §4](README.zh-CN.md#4-sdk-结构) |
