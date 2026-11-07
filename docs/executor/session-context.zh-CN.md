# 会话上下文、启动钩子、会话文件与拒绝原因（1.1）

[English](session-context.md)

机器范围的执行端（`EnvironmentKey` / `environmentKey` / `environment_key` 为机器 daemon token，environment id 为空）可以按会话决定进程在哪里、以什么方式运行，并以机器可读原因拒绝。平台不存本地路径：建会话的一方把不透明引用写进会话 `metadata`（与 Claude Managed Agents 在会话 metadata 中传文件引用的做法一致），执行端用本机自己的映射表解析。

## 会话上下文

| | Go | TypeScript | Python |
| --- | --- | --- | --- |
| 按进程（agent 会话的 `process.start`） | `client.SessionContext(ctx, processID)` | `client.sessionContext(processId)` | `await client.session_context(process_id)` |
| 按会话（无进程的 agent 能力） | `client.SessionContextByID(ctx, sessionID)` | `client.sessionContextById(sessionId)` | `await client.session_context_by_id(session_id)` |
| 不在本机 | `ErrSessionContextNotFound` | `SessionContextNotFoundError` | `SessionContextNotFoundError` |
| 不是机器范围执行端 | `ErrMachineTokenRequired` | `MachineTokenRequiredError` | `MachineTokenRequiredError` |

返回 `GET /api/v1/daemon/processes/{process_id}/session` 或 `GET /api/v1/daemon/sessions/{session_id}` 的 `{id, environment_id, metadata, created_at}`，不含 agent 配置、凭据或其他会话。按会话缓存，所以 metadata 是首次读取的值（见[缓存语义](#缓存语义)）；失败不缓存。`process.start` metadata 的 `agent_session_id`（`MetadataAgentSessionID`）指明会话。

## 缓存语义

返回值是**首次读取时**的会话：执行端按会话缓存（Go、TypeScript、Python 一致），该会话之后的所有进程看到同一份 `metadata`。会话 `metadata` 之后被补丁修改（API 允许，与 Claude Managed Agents 一致）**不保证**传到执行端。调用方不得依赖中途修改来改变会话的执行位置或主人：`workspace_ref`、`owner` 等在建会话时就写好，要改就新建会话。执行端自己也应把首次的值当作最终值并据此校验，不要为了追随变化而重复读取。失败（不存在、临时错误）不缓存，下一个进程会重新读取。

这与 Claude Managed Agents 一致：worker 领取的工作项只带会话 ID、不带 metadata，worker 在领取时读一次会话并用这个值完成整次运行（[Stage files for a session](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-workers#stage-files-for-a-session)，读取 metadata 是官方明说；“只读一次、不再重读”是据其 spawn 脚本推断，官方未明说）。

## 哪些进程带 `agent_session_id`

Agent 工具发起的进程都带 `process.start` metadata 的 `agent_session_id`：命令（`run_command`、shell 工具）、文件传输（`write_file`、`edit_file`、存储上传下载）、包安装（`install_packages`）以及 agent 打开的终端。不带它的进程不是 agent 工具发起的，例如人在控制台打开的终端：没有会话，也没有可校验的主人。按会话决策的执行端（`Launch`、会话文件、主人校验）必须显式处理缺失：返回 `session_rejected`，而不是退回默认目录。人的终端应走执行端自己掌控的另一条本地路径（自己的认证与策略），不要走会话路径。Go 中表现为 `ProcessContext.AgentSessionID == ""`；TypeScript / Python 中为 `params.metadata?.agent_session_id` / `params["metadata"].get("agent_session_id")` 缺失或为空。

## 带原因拒绝

`process.start` 被拒（或拒绝 `tool.call`）时返回 JSON-RPC 错误，`data.reason` 为机器可读码；BMA 原样带到工具结果 `error_code` 与回合的 `session.error.code`。推荐值与三语言常量见[英文版表格](session-context.md#refusing-with-a-reason)：`workspace_unavailable`、`workspace_boundary`、`workspace_read_only`、`session_rejected`、`local_confirmation_denied`、`local_confirmation_expired`、`spawn_failed`、`process_id_spec_mismatch`（Go `Reason*`；TS / Python `ErrorReason.*`，同名同值）。开放枚举，未知值原样透传。构造：Go `NewReasonError(code, reason, msg)`、TS `RpcError.withReason(code, reason, msg)`、Python `RPCError.with_reason(code, reason, msg)`。只有启动时拒绝能带结构化码；运行中被系统沙箱拒绝只有退出码与输出。

## Go：`ProcessOptions.Launch`

每个新命令进程或包安装调用一次（重放、文件传输不调用），在会话文件同步之后、spawn 之前。`LaunchRequest` 含执行端会话 id、进程 id、`AgentSessionID`、`Metadata`、请求的 `Argv`、未解析的 `Cwd`、会话工作区 `DefaultCwd` 与计算出的 `Env`。包安装时 `PackageInstall` 为管理器与包名，`Argv`、`Cwd`、`Env` 是安装器的固定命令（其 `Dir` 作为 `Cwd`）；`PackageInstall == nil` 即命令。`LaunchSpec{Dir, Argv, Env}` 替换之，空字段保持原值（无 `Dir` 时照旧把请求 cwd 限制在工作区内；`Dir` 必须是绝对路径且不受工作区限制）。返回 `*RPCError` 原样拒绝；其他错误为 `spawn_failed`。包安装会调用包管理器、可能执行安装脚本并写系统或全局目录，所以限制或拒绝命令的钩子应显式处理 `req.PackageInstall != nil`：像命令一样包装、改环境，或带原因拒绝（例如需要用户在本机确认时返回 `local_confirmation_denied`）。返回空 `LaunchSpec` 则保持安装器自己的命令、目录与环境。没有 `Launch` 钩子时包安装照旧运行。

实现 `SessionFileTransferer` 的文件传输器改收 `TransferSessionFile(ctx, ProcessContext, transfer)`。

示例 [`examples/sandboxed-folder`](../../examples/sandboxed-folder)：metadata `owner` 校验（`session_rejected`）、`workspace_ref` → 本机目录（`workspace_unavailable`）、cwd 越界（`workspace_boundary`）、macOS `sandbox-exec` 包装（只允许写该目录与会话 outputs）、缺 `agent_session_id` 的进程返回 `session_rejected`、包安装返回 `local_confirmation_denied`。

TypeScript / Python 没有 process 运行时，因此不提供 Launch 钩子：执行端自己的 `process.start` handler 就是等价物（读上下文、校验、再 spawn），其 params 里能看到 `package_install`，所以包安装与命令有同样的决策点。见 [`typescript/examples/session-context`](../../typescript/examples/session-context/main.mjs) 与 [`python/examples/session_context.py`](../../python/examples/session_context.py)：缺 `agent_session_id` 与 `package_install` 在读取会话上下文之前就被拒绝。

## 会话文件根

声明 `capabilities.session_files.root`（展开后的绝对路径）的执行端，平台在提示、工具说明和写入规则中使用 `<root>/<会话ID>/uploads`（只读）与 `<root>/<会话ID>/outputs`，代替 `/mnt/session`，同一台机器上的并发会话互不共享文件。不声明则保持 `/mnt/session`。

Go：`files, _ := NewSessionFiles(SessionFilesOptions{Root: root})`，传 `ProcessOptions{SessionFiles: files}`（自动声明 root），并 `go files.Run(ctx)`。会话的每个进程启动前同步挂载文件到 uploads（sha256 校验、0444、其余条目删除）；outputs 稳定后每 2 秒上传，`process.read` 报告进程结束时立即上传。全部经 `os.Root`，不跟随符号链接，只上传普通文件。`files.Dirs(sessionID)` 返回两个目录（例如写进沙箱配置）。根目录不要放在执行端状态库与 IPC 目录下。TypeScript / Python 声明 `capabilities: { session_files: { root } }`，用同一组 daemon 路由自行同步。

Go SDK 最低版本升至 Go 1.25（`os.Root.MkdirAll` / `Rename`）。
