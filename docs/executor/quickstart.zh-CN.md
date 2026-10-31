# 快速开始与示例

[English](quickstart.md) · [总览](README.zh-CN.md)

| 示例 | 能做什么 | 依赖 | 状态 |
| --- | --- | --- | --- |
| 1A：云端脑 → bmad（本机手） | 平台 agent 在你的电脑上跑 `run_command`/进程工具/终端/技能/文件传输 | bmad ≥ 0.2.11、机器 token | 【已有】 |
| 1B：云端脑 → SDK 手（`Core`） | 平台按 `tool.call` 派发 bash/文件工具与 stdio MCP | selfhost environment + environment key；Go SDK 0.4.0 | 【已有】（平台派发 MCP server【即将提供】） |
| 2：本机脑 → 本机手 → 事件回平台 | 本机脑经 loopback 驱动同一执行核心；agent 事件回传平台 | SDK 0.4.0（Go、TypeScript、Python）；平台 agent 能力 | 本机部分【已有】；回平台部分【即将提供】，可用模拟平台联调 |

---

## 0. 配对与安装

### 0.1 创建机器、拿到机器 token

任选其一：

- Dashboard：**All projects** → **Machines** → **Connect machine**，设置名称（即 agent 配置里的 `machine_name`），可选择共享到项目；下一屏显示机器 token 和安装命令。
- CLI：`npx @beeos-ai/bma-cli machines create-local --display-name my-mac --project-ids "$PROJ"`（创建 + 安装一步完成）；或 `machines create … --json` 拿到 `token` 和 `install_command` 去目标电脑执行。
- API：connect-a-byo-machine 端点。

token 只显示一次，按机密保存（不要打印到日志）。机器需要被授予到项目，项目里的 agent 才能使用它。

### 0.2 方案 A：安装 bmad

```sh
curl -fsSL https://api.bma.beeos.ai/install/bmad.sh | sh   # 按提示粘贴机器 token
```

安装到 `~/.bmad` 并注册为用户服务（Linux systemd / macOS launchd）。注意：**默认服务目前运行旧 daemon socket 模式**；Executor Protocol 模式是显式入口：

```sh
bmad stop                                   # 与 daemon 模式互斥（共用 BMA_HOME 运行锁）
export BMA_MACHINE_TOKEN=…                  # 从安全存储读入，不要回显
bmad executor --executor-url https://api.bma.beeos.ai/api --work-root "$HOME/work"
```

- `--executor-url` 是带 `/api` 的 origin（bma-api 在 `/api/v1/executors/*` 提供服务）。
- `--environment` 省略 = 机器路径；`--work-root` 限定进程工作目录（默认当前目录）。
- 云端池化机器由模板以 `BMAD_LAUNCH_MODE=executor` 自动这样启动。

### 0.3 方案 B：Desktop 内嵌

Desktop 用同一套 0.1 流程拿机器 token，保存在系统钥匙串之类的安全存储，用 SDK executor 端连 `…/api/v1/executors/token`（`environment_id: ""`）。**不要**再安装或运行 bmad。机器路径要求声明 `process.enabled` 并实现 `process.*`：Go SDK 在 `Core` 上注册 `NewProcessCapability` 即可（见[总览方案 B](README.zh-CN.md#方案-bdesktop-内嵌手)）；TypeScript / Python 需用 `onRequest` / `on_request` 自行实现 `process.*`，或先用示例 1B 的 environment 路径联调。

---

## 1A. 云端脑 → bmad

1. 按 0.2 启动 `bmad executor`。日志出现 `executor registered executor_id=exec_… connection_id=…` 即注册成功。
2. 在已授予该机器的项目里，agent 配置引用它：

   ```yaml
   machine_sources:
     - machine_name: my-mac
       cwd: /Users/me/work
   ```

3. 给 agent 发任务（例如“在仓库里跑测试”）。平台的 `run_command` 被映射为 `process.start`（`stderr:"merge"`），输出经 `process.read` 读回；进程动作（读、写 stdin、中断、终止）分别映射到 `process.read`/`process.write`/`process.interrupt`/`process.terminate`。
4. 断网重连：bmad 退避重连并带 `resume.active_sessions`；平台用 `process.list` 对账，运行中的进程继续，断线期间结束的补报终态，丢失的记为 unknown 而不重跑。

当前不支持：经 bmad 运行 stdio MCP / CUA 工具（平台未向机器 executor 派发 MCP server）。

## 1B. 云端脑 → SDK 手（`Core`）【已有】

selfhost environment 与 environment key 由 BMA 为你开通，请联系 BMA 对接人。

```sh
cd bma-sdks/executor
BMA_BASE_URL=https://api.<env>.bma.beeos.ai \
BMA_ENVIRONMENT_ID=shenv_… BMA_ENVIRONMENT_KEY=… \
go run ./examples/cloud-hand -work-root ~/bma-work
```

核心代码（[`examples/cloud-hand`](../../executor/examples/cloud-hand/main.go)）：

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

平台按工作项派发一次性会话：`session.assign` → `session.ready` → `tool.call`（`bash`/`read`/`write`/`list_files`）→ `tool.result` → `session.ended`。`Core` 会为 `session.assign.mcp_servers[]` 中 `connection_origin:"environment"` 的 stdio server 起进程并桥接到 MCP 通道（平台目前尚不发送 `mcp_servers`【即将提供】）。

如果你不用 `Core`，而是自己实现执行：用 `client.OnToolCall(handler)`，在 handler 的 `ctx` 被取消时杀掉进程组；在准备好后调用 `SessionReady`、全部子进程退出后调用 `SessionEnded`（见 [Go executor README](../../executor/README.md#executor-quick-start)）。

## 2. 本机脑 → 本机手 → 事件回平台

### 2.1 本机脑驱动本机手（离线可跑）【已有】

```sh
cd bma-sdks/executor
go run ./examples/local-brain -mcp-command "go run ./examples/echo-mcp"
```

流程（[源码](../../executor/examples/local-brain/main.go)）：本机脑在 `127.0.0.1:0` 起 `Harness`（每次运行随机 token）；执行端 `Core` 接一个 `Client`，用 `StaticToken` 连过去；本机脑 `WaitExecutor` → `AssignSession`（带 stdio MCP server）→ `Ready` → `CallTool(bash)` ×2 → `session.MCP("echo")` 收发原始 MCP JSON-RPC → `Stop`。把脚本换成你的模型循环，就是一个本机 Agent 的“手”部分。鉴权与并存规则见[本机连接](local-connection.zh-CN.md)。TypeScript：[`harness-loopback`](../../executor/typescript/examples/harness-loopback/main.mjs)；Python：[`harness_loopback.py`](../../executor/python/examples/harness_loopback.py)。

### 2.2 Agent 事件回平台【SDK 已有；平台即将提供】

```sh
cd bma-sdks/executor
go run ./examples/agentexecutor
```

示例用模拟平台演示完整生命周期：`turn.start` → 流式 `session.event_delta` 与持久 `session.event` → `interaction.request` 审批 → `interaction.response` → 结果 → `turn.input` 追加 → 断线（TCP 代理断开）后按 sequence 补报 → `turn.cancel` → `session.close`。你的实现只需提供 `AgentHandler`，见 [agent 能力](agent-capability.zh-CN.md#6-sdk-用法)。TypeScript 与 Python 对应示例：[`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs)、[`agent_minimal.py`](../../executor/python/examples/agent_minimal.py)。

平台实现 agent 能力后，同一个程序把 `TokenSource`/凭据换成机器 token 指向平台即可；客户端从平台会话事件流（SSE）订阅事件。

---

## 3. 常见错误排查

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| token 端点 `401` | 机器 token / environment key 无效或已撤销 | 重新创建 token；executor 应停止重连 |
| token 端点 `404` | `environment_id` 为空但该服务端不服务机器 executor（例如连到 managed-agents 而不是 bma-api），或 environment 不在凭据范围 | 机器路径用 `https://<host>/api`；核对 environment |
| 升级 `400 missing … subprotocol` | 没有提供 `Sec-WebSocket-Protocol: bma.executor.v1.1`（或 v1） | 设置子协议 |
| 升级 `403 … does not allow channel mcp` | 机器路径 JWT 只有 `control` | 机器路径不开 MCP 通道 |
| 注册后立即 close `4003` | `executor.register.protocol` 与协商的子协议不一致 | 两者都用 `bma.executor.v1.1` |
| 注册后 close `4002` | 机器路径：未声明 `capabilities.process.enabled`；`executor.version` 不是 `MAJOR.MINOR.PATCH`（或 `0.0.0-dev`）；或运行时被平台结束 | 声明能力；修正版本号；重连 |
| 周期性 close `4001` | JWT 5 分钟到期 | 正常：重新换 token 重连 |
| close `4004` 后反复被顶 | 同一台机器上两个执行端（bmad + Desktop，或两个 bmad） | 只保留一个；收到 4004 的一方必须停止 |
| `bmad executor cannot start: the bmad daemon (or another executor) is running` | daemon 模式仍在运行 | `bmad stop` 后再启动 |
| `BMA_MACHINE_TOKEN is required` | `bmad executor` 只从环境变量读机器 token | 设置环境变量 |
| `-32009 executor_busy` | 容量满（会话/进程/并发读） | 按 `data.retryable` 退避重试，不要丢结果 |
| `-32012 capability_unavailable`，`data.capability:"tool:<name>"` | executor 不服务该工具 | 声明并实现，或让脑换工具 |
| `-32013 conflict` `process_id_spec_mismatch` | 同一 `process_id` 不同规格 | 新进程用新 id |
| `-32007 session_not_active` | 会话已结束、未分配，或跨 harness 调用 | 重新 `session.assign` |
| 平台报 “executor did not merge stderr into stdout” | `process.start` 结果没有回显 `stderr:"merge"` | 实现 `stderr:"merge"` 并回显 |
| 调用超时但不知道是否执行 | 传输失败 = 结果未知 | 不要盲目重发有副作用的调用；用 `process.list`/对账确认 |
