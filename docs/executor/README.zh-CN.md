# BMA Executor 接入指南（总览）

[English](README.md) · [仓库 README](../../README.md)

本文给要实现执行端（“手”）、驱动执行端（“脑” / harness）或把完整本机 Agent 接入 BMA 的接入方一个完整入口：Executor Protocol 是什么、有哪些部署形态、SDK 怎么分模块、怎么连、现在能用什么。

| 文档 | 内容 |
| --- | --- |
| 本文 | 角色模型、部署形态、“一台电脑一个执行端”、SDK 结构与连接方式、能力现状 |
| [协议参考 v1.1](protocol-reference.zh-CN.md) | 连接与鉴权、全部方法/参数/错误码、能力位、幂等与断线恢复、取消层级、限额 |
| [agent 能力](agent-capability.zh-CN.md) | 本机完整 Agent 接入：`turn.*`、`session.close`、Agent 事件、ACK/补报、interaction |
| [本机连接与鉴权](local-connection.zh-CN.md) | 本机脑 ↔ 本机手的连接方式、token、多 harness 并存规则 |
| [快速开始与示例](quickstart.zh-CN.md) | 配对/安装、示例 1（云端脑→手）、示例 2（本机脑→本机手→事件回平台）、排错 |

状态标注（全套文档统一）：

- **【已有】**：bma-executor-sdk 0.4.0 已发布，或 BMA 平台已上线。
- **【即将提供】**：契约中已定义、尚未提供服务；按能力位（capability-gated）发布，未声明对应能力位的一方不会收到这些方法。
- **【设计中】**：只有设计，没有实现。

> 基线：bma-executor-sdk **0.4.0**，协议 `bma.executor.v1.1`（契约 1.1.0，[`contracts/bma/executor`](../../contracts/bma/executor)）。

---

## 1. Executor Protocol 是什么

Executor Protocol 定义“指挥方”和“执行方”之间的通信：

```text
          Executor Protocol（JSON-RPC 2.0）
 harness 端（脑） ◀──────────────────────▶ executor 端（手）
  分配会话、调工具、读进程输出               执行工具/进程/PTY，回结果和事件
```

- 协议管：注册与能力协商、会话分配、工具调用、进程与 PTY、文件/凭据/技能等机器能力、取消、MCP 通道、断线恢复、事件回传。
- 协议不管：脑内部怎么思考（模型调用、规划、选工具）。
- 线格式：一条 WebSocket 上的 JSON-RPC 2.0，子协议 `bma.executor.v1` / `bma.executor.v1.1`。没有 CloudEvents、没有自定义信封。
- 契约单一来源：本仓库中的机器可读契约 [`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml)、[`executor.asyncapi.yaml`](../../contracts/bma/executor/executor.asyncapi.yaml) 与 [`schemas/`](../../contracts/bma/executor/schemas)，一致性示例在 [`examples/conformance.json`](../../contracts/bma/executor/examples/conformance.json)。BMA 服务端、bmad 和三种语言的 SDK 都由它生成或以它为测试基准。

### 1.1 角色是相对的

| 角色 | 职责 | 例子 |
| --- | --- | --- |
| harness | 发 `session.assign`、`tool.call`、`process.*`、`terminal.*`；收 `tool.result`、`session.event` | BMA 平台、你的本机脑（用 SDK `Harness`） |
| executor | 注册、心跳、执行、回结果 | `bmad executor`、selfhost worker、用 SDK `Client`/`Core` 写的进程 |
| agent 提供方 | executor 声明 `agent` 能力，接收自然语言任务、回传完整 Agent 事件 | 你的本机 Agent 经 executor 接入（SDK【已有】；平台【即将提供】） |

**谁指挥谁就是 harness**。同一个本机脑，对平台而言是“agent 提供方”（经 executor 接收任务），对本机手而言是“harness”（驱动它执行工具）。

### 1.2 “谁先连”与角色解耦

WebSocket 上谁是客户端、谁是服务端，与协议角色无关：

- 连云端：executor 永远是 WebSocket **客户端**，主动出站（NAT 后可用，平台从不拨入）。
- 本机，执行端拨号：本机脑把 SDK `Harness` 挂在 loopback TCP 或本机 socket 上，执行端接一个 `Client` 拨过去【已有】。
- 本机，harness 拨号：执行端监听 Unix domain socket（TypeScript 还支持 Windows named pipe），每次启动生成本机 token，本机脑用 `Harness.DialExecutor` / `dialExecutor` 连进来；连上后仍按 harness / executor 角色收发【Go 与 TypeScript SDK 已有；bmad 尚未监听】。见[本机连接与鉴权](local-connection.zh-CN.md)。

---

## 2. 部署形态

| 形态 | executor 进程 | 凭据 | 连到哪里 | 状态 |
| --- | --- | --- | --- | --- |
| 云端沙箱（池化机器） | 机器模板里的 `bmad executor` | 机器 daemon token | BMA API `/api/v1/executors/*` | 【已有】 |
| BYO 机器（用户自己的电脑/服务器） | `bmad`：默认服务仍是旧 daemon socket；`bmad executor` 为 Executor Protocol 的显式入口 | 机器 daemon token | BMA API `/api/v1/executors/*` | 【已有】 |
| Desktop 内嵌（方案 B） | Desktop 进程用 SDK 自己当 executor（`Client` + `Core` + `NewProcessCapability`） | 本机配对得到的机器 token | BMA API | 【已有】（Go SDK） |
| selfhost worker | selfhost worker | environment key | Managed Agents API `/v1/executors/*` | 【已有】 |

两条服务端路径：

- **机器路径（machine-scoped）**：BMA API 的 `/api/v1/executors/{token,channel}`。机器 token 换出的 JWT 只有 `control` 通道；连接注册后被绑定为这台机器的 daemon runtime，平台的 `run_command` / 进程工具经 `process.*` 执行。
- **environment 路径（selfhost）**：Managed Agents API 的 `/v1/executors/*`。environment key 换出的 JWT 有 `control` + `mcp` 通道；工作队列按“一项工作 = 一次性会话 + 一个 `tool.call`”派发。

---

## 3. 一台电脑一个执行端

**每台电脑只有一个执行端进程连平台，用这台电脑的机器身份。** 这由平台强制：

- 机器 token 换出的 executor id 固定为 `exec_<machine_id>`；同一 executor id 的新连接注册后，旧连接被以 close code `4004` 关闭，旧端收到 `4004` 必须停止、不重连。
- 机器路径上，新注册会替换这台机器当前的 daemon runtime（旧 daemon socket 或旧 executor 二者之一）。
- 同一台机器上 `bmad executor` 与 `bmad daemon` 共用运行锁，互斥启动。

所以本机脑 + 本机手有两种合规方案：

### 方案 A（推荐）：复用 bmad 当手

```text
平台（harness）
   │ Executor Protocol / WSS，bmad 主动出站，机器身份
   ▼
bmad（这台电脑唯一的执行端）
   ├─ 手的能力：process.* / terminal.* / skill.install / 文件传输 / Git 凭据 …… 【已有】
   └─ agent 能力：把平台任务转给本机脑、把事件转回平台 ………………………… 【即将提供】
          ▲
          │ Executor Protocol / 本机连接
          ▼
你的本机脑
   ├─ 作为 harness：驱动 bmad 执行工具（不经云端）…………………………………… 【bmad 即将提供】
   └─ 作为 agent 提供方：接收平台任务、产生 Agent 事件 ………………………… 【平台即将提供】
```

现状：bmad 目前只有到平台的一条出站连接，还不能接受本机脑连入，也不能转发 agent 能力。所需的 SDK 部件（`ListenUnixSocket`、`Harness.DialExecutor`、agent 能力）已在 0.4.0 发布。

### 方案 B：Desktop 内嵌手

Desktop 用 SDK 的 executor 端自己当执行端，并完成这台电脑的配对，拿到机器 token；**这台电脑不再运行 bmad**（否则两者互相 `4004` 替换）。本机脑在进程内（`InProcessDialer`）或经本机 socket 驱动同一个 `Core`。

机器路径要求 executor 声明 `process.enabled`，否则注册后以 `4002` 关闭。Go SDK 已提供：在 `Core` 上注册 `NewProcessCapability(core, ProcessOptions{...})`（含 PTY、文件传输与 Git 凭据钩子）；Go 客户端默认协商 `bma.executor.v1.1`。TypeScript 与 Python 没有 `Core` 和 `process.*` 辅助实现，需要自己用 `onRequest` / `on_request` 实现 `process.*`，或先用 environment key 连 selfhost environment 路径联调（`tool.call` 模式），见[快速开始](quickstart.zh-CN.md)。

两种方案对平台是一样的：平台只看到一个 `exec_<machine_id>` 执行端。

---

## 4. SDK 结构

SDK 按协议命名、按角色分模块。Go 是参考实现（`github.com/beeos-ai/bma-sdks/executor`），TypeScript 与 Python 提供同样的三端：

```text
bma-sdks/executor（Go）
├── executor 端   Client     token 交换（environment key 或机器 token）、注册、心跳、带 resume 的重连、
│                            会话、tool.call/cancel、结果重试、产物上传、MCP 通道
│   └── Core                 与“谁驱动”无关的执行核心：bash、read/write/edit/glob/grep、stdio MCP supervisor、清理、
│                            多 harness 隔离、可注册能力（process.*、skill.install、自定义）
├── harness 端    Harness    http.Handler：接受 executor 连接，驱动会话、tool.call、MCP 通道；断线恢复与租约
└── agent         ServeAgent / AgentHandler（executor 侧），AgentPlatform / StartTurn…（harness 侧）
```

| 部件 | Go [`executor`](../../executor) | TypeScript [`@beeos-ai/bma-executor-sdk`](../../executor/typescript) | Python [`bma-executor-sdk`](../../executor/python) |
| --- | --- | --- | --- |
| executor 端（`Client` / `ExecutorClient`） | 【已有】 | 【已有】 | 【已有】 |
| 执行核心 `Core`（bash、文件工具、stdio MCP） | 【已有】 | 未提供 | 未提供 |
| `process.*` 与 `skill.install` 辅助实现 | 【已有】 | 未提供（用 `onRequest`） | 未提供（用 `on_request`） |
| `terminal.*` 辅助实现 | 未提供（自行注册 `Capability`） | 未提供 | 未提供 |
| harness 端（`Harness`） | 【已有】 | 【已有】 | 【已有】 |
| agent 能力（两侧） | 【已有】 | 【已有】 | 【已有】 |

- SDK 不包含 agent 循环本身（模型调用、规划），那部分用你自己的实现；agent 模块是薄胶水：事件编号/ACK/补报、running/idle 框架、interaction 等待。
- 三种语言的协议类型都由 [`contracts/bma/executor`](../../contracts/bma/executor) 生成；[`lock.json`](../../contracts/bma/executor/lock.json) 记录契约版本与哈希。
- bmad 使用由同一份契约生成的自有执行端实现，没有直接依赖 SDK 包；保证来自同一契约 + 跨实现的一致性测试。

### 4.1 连接方式

| 方式 | 适用 | 鉴权 | 状态 |
| --- | --- | --- | --- |
| WebSocket（WSS）出站 | executor → 平台 | 机器 token / environment key 换短期 JWT | 【已有】 |
| WebSocket over loopback TCP | 本机脑（`Harness`）↔ 本机手（`Client`+`Core`） | `StaticToken` + `HarnessOptions.Authenticate` | 【已有】 |
| WebSocket over Unix domain socket（`unix://`），执行端拨号 | 脑监听、手拨号 | 0700 目录 + 0600 socket + 本机 token | 【已有】Go、TypeScript、Python |
| WebSocket over Unix domain socket，harness 拨号 | 手监听（`ListenUnixSocket` / `listenExecutor`），脑拨号（`DialExecutor` / `dialExecutor`） | 0700 目录 + 0600 socket + 每次启动的 token | 【已有】Go、TypeScript |
| Windows named pipe（`npipe://`） | 同上，Windows | 当前用户的 pipe + 本机 token | 【已有】TypeScript |
| 进程内 | 脑和手在同一进程 | 无 | 【已有】（`InProcessDialer`、`inProcessDialer`、`inprocess_dialer`） |

协议消息在所有方式下不变。

---

## 5. 能力现状一览

| 能力 | 协议 | 平台 | bmad | Go SDK 0.4.0 |
| --- | --- | --- | --- | --- |
| 注册/心跳/重连/drain/4004 替换 | v1 | 已有 | 已有 | 已有（两端） |
| `tool.call`（bash、文件工具） | v1 | environment 路径已有；机器路径不用 `tool.call` | 只实现 `bash` | `Core`：bash/read/write/edit/glob/grep |
| `process.*` | v1.1 | 已有（机器路径的工具执行通道） | 已有 | 已有（`NewProcessCapability`） |
| `terminal.*`（PTY） | v1.1 | 已有 | 已有（非 Windows） | 无辅助实现 |
| `skill.install` | v1.1 | 已有 | 已有（机器路径） | 已有（`NewSkillInstallCapability`） |
| 文件传输 / Git 凭据（`process.start` 附加字段） | v1.1 | 已有 | 已有（机器路径） | 已有（`ProcessOptions` 钩子） |
| 产物上传 `POST /v1/executors/artifacts` | v1.1 | environment 路径已有 | environment 路径已有 | 已有（`Client.UploadArtifact`） |
| MCP 通道（`channel=mcp`） | v1 | 服务端通道已有（environment 路径）；平台 harness 尚未在生产路径向 executor 派发 MCP server | 未实现 | `OpenMCPChannel` 与 `Core` stdio supervisor |
| session files（`/mnt/session/uploads\|outputs`） | 机器 token HTTP 路由 | 已有 | 已有 | — |
| `workspace.backup` | v1.1 附加 | 已有 | 已有 | — |
| agent 能力（`turn.*`、interaction） | v1.1 + `agent` | 即将提供（契约已定义，平台侧未实现） | 未实现 | 已有（两侧） |
| 本机 socket | — | — | 即将提供 | 已有 |

---

## 6. 身份、配对与凭据

- **机器身份**：一台电脑 = 一条 machine 记录 + 一个或多个机器 daemon token。可在 BMA Dashboard（“Connect machine”）、`bma machines create-local` 或 API 创建。token 只显示一次。
- **执行端凭据**：机器 token（机器路径）或 environment key（selfhost 路径）只用于 `POST …/v1/executors/token`，换出 5 分钟有效的 HS256 JWT；WebSocket 上只带 JWT，长期凭据不上 WebSocket。
- **本地脑访问模型的凭据**与执行端凭据分开管理，SDK 不读也不转发。
- 方案 B 的 Desktop 配对：用上述同一套 machine 创建流程拿到机器 token，由 Desktop 安全保存（例如系统钥匙串），不要同时安装 bmad。

详细的鉴权流程、错误与 close code 见[协议参考 §2](protocol-reference.zh-CN.md#2-连接与鉴权)。
