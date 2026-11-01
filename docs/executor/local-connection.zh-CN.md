# 本机连接与鉴权

[English](local-connection.md) · [总览](README.zh-CN.md)

本机脑 ↔ 本机手用的是与云端同一套 Executor Protocol 消息，只是传输与鉴权不同。除特别标注外，本文内容均已在 bma-executor-sdk 0.4.0 提供。

| 传输 | 谁监听 | Go | TypeScript | Python |
| --- | --- | --- | --- | --- |
| WebSocket over loopback TCP | 脑（`Harness`） | `http.Serve(ln, harness)` + `StaticToken("ws://…", token)` | `harness.listen()` | `await harness.serve(host, port)` |
| WebSocket over Unix domain socket（`unix://`） | 脑（`Harness`） | 在 `unix` listener 上服务 `Harness` + `StaticToken("unix:///path.sock", token)` | `harness.listenLocal(path)` | `await harness.serve_unix(path)` |
| WebSocket over Unix domain socket | 手（executor） | `ListenUnixSocket(path, token)` + `Harness.DialExecutor(ctx, path, token)` | `listenExecutor({ path, token })` + `harness.dialExecutor(url, { token })` | 未提供 |
| Windows named pipe（`npipe:////./pipe/<name>`），脑监听 | 脑（`Harness`） | 在 `NamedPipeListener(name)` 上服务 `Harness` + `StaticToken("npipe:////./pipe/<name>", token)` | `harness.listenLocal("\\\\.\\pipe\\<name>")` | `await harness.serve_npipe(name)` |
| Windows named pipe，手（executor）监听 | 手（executor） | `ListenNamedPipe(name, token)` + `Harness.DialExecutor(ctx, "npipe:////./pipe/<name>", token)` | 同上“执行端监听”行，路径换成 pipe 路径 | 未提供 |
| 进程内 | — | `InProcessDialer(harness, principal)` | `harness.inProcessDialer()` | `harness.inprocess_dialer()` |

---

## 1. 脑监听（执行端拨号）

### 1.1 拓扑

```text
本机脑进程                                   执行端进程（Desktop，或日后的 bmad）
┌──────────────────────┐  ws://127.0.0.1:<port>  ┌──────────────────────────────┐
│ Harness (http.Handler)│  或 unix:///path.sock   │ Client "local" ─┐             │
│  WaitExecutor/Assign/ │◀────────────────────────│                 ├─ Core
│  CallTool/MCP         │  bma.executor.v1.1 / v1 │ Client "cloud" ─┘             │
└──────────────────────┘                         └──────────┬───────────────────┘
                                                            │ wss://…/v1/executors/channel
                                                            ▼
                                                         BMA 平台
```

- 本机脑把 `Harness` 挂在 **loopback** 地址（`127.0.0.1` / `::1`）或 Unix socket 上，不监听网络接口。
- 执行端为每个 harness 接一个 `Client` 到同一个 `Core`：`core.Attach("cloud", cloudClient)`、`core.Attach("local", localClient)`；两条连接上执行端都是 WebSocket 客户端。
- 本机调用不经过云端。

```go
// 本机脑
harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{
	Authenticate: func(r *http.Request) (bmaexecutor.Principal, error) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+localToken)) != 1 {
			return bmaexecutor.Principal{}, errors.New("unauthorized")
		}
		return bmaexecutor.Principal{ExecutorID: "exec_local"}, nil
	},
})
ln, _ := net.Listen("tcp", "127.0.0.1:0")
go http.Serve(ln, harness)

// 执行端
local, _ := bmaexecutor.NewClient(bmaexecutor.Options{
	EnvironmentID: "local", Executor: info, Capabilities: core.Capabilities(),
	TokenSource: bmaexecutor.StaticToken("ws://"+ln.Addr().String(), localToken),
})
_ = core.Attach("local", local)
```

可运行版本见 [`executor/examples/local-brain`](../../executor/examples/local-brain/main.go)（`cd executor && go run ./examples/local-brain`）。改用 Unix socket 时，把同一个 `Harness` 挂到 `net.Listen("unix", path)`，执行端用 `StaticToken("unix://"+path, localToken)`；默认 dialer（与 `UnixSocketDialer(path)` 一样）在 socket 上跑 WebSocket。Python 的 [`harness_loopback.py --unix PATH`](../../executor/python/examples/harness_loopback.py) 做的是同一件事。

### 1.2 鉴权

| 项 | 规则 |
| --- | --- |
| 本机 token | 每次本机脑启动随机生成（≥32 字节），通过只有当前用户可读的途径交给执行端（例如 0600 文件、启动参数传递的继承 fd、或同进程变量）；不写日志 |
| 校验 | `HarnessOptions.Authenticate` 校验每一次 WebSocket 升级（控制通道与 MCP 通道）。`Authenticate: nil` 表示不鉴权，**只允许**在仅本用户可达的 loopback 场景下使用；SDK 不替你检查监听地址 |
| 身份固定 | `Authenticate` 返回的 `Principal.ExecutorID` 固定执行端身份：`resume.executor_id` 必须与之相符，只有该执行端能为它的会话打开 MCP 通道 |
| 凭据隔离 | 本机 token 与平台凭据（机器 token / environment key）无关，不能互换；平台凭据永远不发给本机脑 |

### 1.3 多 harness 并存规则（Go `Core`）

| 规则 | 行为 |
| --- | --- |
| 会话隔离 | 会话按 `(brain, session_id)` 区分；两个 harness 可以用同一个 `session_id` 而互不影响 |
| 调用隔离 | `tool.call` 只能命中本 harness 分配的会话，否则失败结果 `session_not_active` |
| 工作区 | 每个会话独立工作区 `WorkRoot/<brain>/<session>`（可用 `CoreOptions.Workspace` 覆盖）；文件工具限定在内 |
| MCP | MCP 通道与 stdio server 按 (brain, session, server_key) 隔离 |
| 容量 | `MaxSessions`（默认 16）跨 harness 共享上限；`MaxSessionsPerBrain`（默认等于 `MaxSessions`）防止云端饿死本机；满时 `session.assign` 答 `-32009`（`retryable:true`） |
| 取消与清理 | `tool.cancel`、截止时间、`timeout_ms`、`session.stop` 只杀对应会话的进程组；一个 harness 断开只影响它自己的会话 |
| 信任边界 | bash 不是沙箱：所有 harness 都以执行端的 OS 用户身份运行，只接你信任能操作这台机器的脑 |

### 1.4 平台侧的“一台电脑一个执行端”

本机多个 harness 只在**本机**并存；对平台而言，这台电脑仍只有一条出站连接、一个 `exec_<machine_id>`。不要为本机脑再起一个连平台的执行端（会触发 4004 互相替换，见[协议参考 §2.4](protocol-reference.zh-CN.md#24-websocket-close-code)）。

---

## 2. 手监听（harness 拨号）

让本机脑挂到已经在运行的手（方案 A），不占 TCP 端口，权限由文件系统控制。

| 项 | 行为 |
| --- | --- |
| 方向 | 执行端监听、本机脑连入；连上后仍按 harness / executor 角色收发（执行端仍先发 `executor.register`） |
| 位置 | 每用户运行目录；SDK 把目录设为 `0700`、socket 设为 `0600`。Windows：named pipe `\\.\pipe\<name>`（URL `npipe:////./pipe/<name>`），拒绝远程客户端；Go 把 ACL 限定为当前用户，TypeScript / Python 使用平台默认的 pipe ACL，因此本机 token 才是鉴权依据 |
| 本机 token | 每次启动生成（TypeScript 用 `createLocalToken` 写 `0600` 文件、`readLocalToken` 读取）；每次连接都必须带 `Authorization: Bearer` |
| 网络 | 不监听任何网络接口 |
| MCP | 拨号的 harness 也负责拨会话的 MCP 通道，server 重启后再次拨号 |
| 多 harness | 同 §1.3 |

```go
// 执行端（手）
dial, closeListener, _ := bmaexecutor.ListenUnixSocket(sockPath, localToken)
defer closeListener()
client, _ := bmaexecutor.NewClient(bmaexecutor.Options{
	Executor: info, Capabilities: core.Capabilities(),
	TokenSource: bmaexecutor.StaticToken("ws://local/", ""), Dial: dial,
})
_ = core.Attach("local", client)

// 本机脑
harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{})
go harness.DialExecutor(ctx, sockPath, localToken) // 服务该连接直到关闭
executor, _ := harness.WaitExecutor(ctx)
```

TypeScript：[`executor/typescript/examples/harness-loopback`](../../executor/typescript/examples/harness-loopback/main.mjs)（`listenExecutor` + `dialExecutor`，Unix socket 或 named pipe）。

Windows named pipe（Go）：调用相同，把 `ListenUnixSocket` 换成 `ListenNamedPipe(name, token)`，本机脑用 `harness.DialExecutor(ctx, "npipe:////./pipe/"+name, localToken)`（`\\.\pipe\<name>` 路径也可）。脑监听时，在 `NamedPipeListener(name)` 上服务 `Harness`，执行端用默认 dialer 或 `NamedPipeDialer(name)` 拨 `npipe:////./pipe/<name>`。非 Windows 平台这些调用返回 `ErrNamedPipeUnsupported`。Python 需要 Windows 的 `ProactorEventLoop`（Windows 上 asyncio 默认）：脑端 `await harness.serve_npipe(name)`，执行端 `static_token("npipe:////./pipe/<name>", token)`。

三种语言 SDK 的 named pipe 传输都在 CI 的 Windows runner 上测试（job `windows-named-pipe`）：Go 双向 loopback、TypeScript 本机 socket 测试、Go ↔ TypeScript ↔ Python 经 pipe 互通。

bmad 目前还不监听本机 socket【即将提供】。支持后，本机脑用同样的 `DialExecutor` 连入；当本机脑注册为 agent 提供方时，bmad 在它的平台连接上声明 `capabilities.agent` 并转发 `turn.*` / 事件。

---

## 3. 进程内

脑和手在同一进程（方案 B 的典型情况）时，用进程内传输连接：Go 用 `InProcessDialer(harness, principal)`（harness 侧为 `Harness.ServeConn`），TypeScript 用 `harness.inProcessDialer()`，Python 用 `harness.inprocess_dialer()`。语义与远程完全一致，测试也可复用。示例：[`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs)、[`harness_loopback.py`](../../executor/python/examples/harness_loopback.py)。
