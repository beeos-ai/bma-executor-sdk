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

`process.start` 是谁发起的，由 metadata 区分，语义由平台定义（[executor-session-files](https://github.com/beeos-ai/beeos-managed-agents/blob/main/docs/architecture/executor-session-files.md)，契约 [`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml)）：

| 发起方 | 消息 | metadata |
| --- | --- | --- |
| Agent 工具：命令（`run_command`、shell 工具）、文件传输（`write_file`、`edit_file`、存储上传下载）、包安装（`install_packages`）、脑托管的工具命令 | `process.start` | `agent_session_id` 与 `tool_call_id`；无 `initiator` |
| 平台自发，例如机器桌面 agent | `process.start` | `initiator: "platform"`；无 `agent_session_id` |
| 人在控制台打开的终端 | `terminal.attach`，不是 `process.start` | 无；终端恒为人工打开，没有 agent 工具会发起 |

两个键都没有的 `process.start` 不是 harness 发出的。按会话决策的执行端（`Launch`、会话文件、主人校验）必须显式处理：返回 `session_rejected`，而不是退回默认目录。`initiator` 是开放枚举，目前只定义了 `platform`；无 `agent_session_id` 且值未知的，与缺失同样拒绝。

平台自发进程不属于任何会话，不能落进会话目录。要承载它的执行端按自己的策略处理（自己的目录与限制，如示例的 `-platform-dir`）；不承载的返回 `session_rejected`。人的终端以 `terminal.attach` 到达，由执行端自己的终端能力（自己的认证与策略）处理，不走会话路径。Go 中键表现为 `ProcessContext.AgentSessionID`（缺失为空）与 `ProcessContext.Metadata["initiator"]`（`LaunchRequest` 内同样可取）；TypeScript / Python 中为 `params.metadata?.agent_session_id` / `params.metadata?.initiator`（`params["metadata"].get(...)`），未发送时缺失或为空。

## 带原因拒绝

`process.start` 被拒（或拒绝 `tool.call`）时返回 JSON-RPC 错误，`data.reason` 为机器可读码；BMA 原样带到工具结果 `error_code` 与回合的 `session.error.code`。推荐值与三语言常量见[英文版表格](session-context.md#refusing-with-a-reason)：`workspace_unavailable`、`workspace_boundary`、`workspace_read_only`、`session_rejected`、`local_confirmation_denied`、`local_confirmation_expired`、`spawn_failed`、`process_id_spec_mismatch`（Go `Reason*`；TS / Python `ErrorReason.*`，同名同值）。开放枚举，未知值原样透传。构造：Go `NewReasonError(code, reason, msg)`、TS `RpcError.withReason(code, reason, msg)`、Python `RPCError.with_reason(code, reason, msg)`。只有启动时拒绝能带结构化码；运行中被系统沙箱拒绝只有退出码与输出。

## Go：`ProcessOptions.Launch`

每个新命令进程或包安装调用一次（重放、文件传输不调用），在会话文件同步之后、spawn 之前。`LaunchRequest` 含执行端会话 id、进程 id、`AgentSessionID`、`Metadata`、请求的 `Argv`、未解析的 `Cwd`、会话工作区 `DefaultCwd` 与计算出的 `Env`。包安装时 `PackageInstall` 为管理器与包名，`Argv`、`Cwd`、`Env` 是安装器的固定命令（其 `Dir` 作为 `Cwd`）；`PackageInstall == nil` 即命令。`LaunchSpec{Dir, Argv, Env}` 替换之，空字段保持原值（无 `Dir` 时照旧把请求 cwd 限制在工作区内；`Dir` 必须是绝对路径且不受工作区限制）。返回 `*RPCError` 原样拒绝；其他错误为 `spawn_failed`。包安装会调用包管理器、可能执行安装脚本并写系统或全局目录，所以限制或拒绝命令的钩子应显式处理 `req.PackageInstall != nil`：像命令一样包装、改环境，或带原因拒绝（例如需要用户在本机确认时返回 `local_confirmation_denied`）。返回空 `LaunchSpec` 则保持安装器自己的命令、目录与环境。没有 `Launch` 钩子时包安装照旧运行。

### bash 工具：`CoreOptions.Launch`

`CoreOptions.Launch` 用同一个 `LaunchFunc` 类型，每次 bash 工具调用在起命令前调用一次：包括 `tool.call` 的 bash，以及 `LocalToolRoute` 的 bash（`ACPAgent` 自己的工具就走它）。两个选项传同一个函数，脑自己的命令就和 `process.*` 走同一套准入和沙箱。请求里 `Tool` 为 `"bash"`，`ToolCallID` 为调用 id，`ProcessID` 为空；`Argv` 是 shell 命令（`bash -lc <命令>`，没有 bash 时用 `sh`；Windows 上是 `cmd.exe /d /s /c <命令>`）；`Cwd` 是调用的 cwd（`LocalToolRoute` 为空）。`tool.call` 的 `AgentSessionID`、`Metadata`、`Client` 取自该会话的 `session.assign`；`LocalToolRoute` 的 `SessionID` 与 `AgentSessionID` 都是路由的会话，没有 `Metadata` 和 `Client`（需要会话上下文时用自己的 client 去查）。返回的 spec 按进程的规则生效。拒绝时不起进程：`tool.call` 返回失败结果，文本为 `<原因>: <说明>`；`LocalToolRoute` 返回 `not_forwarded`，`Code` 为原因（普通错误为 `spawn_failed`），同一调用 id 再来时原样返回。文件工具（read/write/edit/glob/grep）在执行端进程内完成，不调用钩子，仍限制在会话工作区内。不设 `CoreOptions.Launch` 时 bash 工具行为不变。

实现 `SessionFileTransferer` 的文件传输器改收 `TransferSessionFile(ctx, ProcessContext, transfer)`。

示例 [`examples/sandboxed-folder`](../../examples/sandboxed-folder)：metadata `owner` 校验（`session_rejected`）、`workspace_ref` → 本机目录（`workspace_unavailable`）、cwd 越界（`workspace_boundary`）、macOS `sandbox-exec` 包装（只允许写该目录与会话 outputs）、缺 `agent_session_id` 的进程返回 `session_rejected`（平台自发进程在设置了 `-platform-dir` 时运行于该独立目录，不进任何会话目录）、包安装返回 `local_confirmation_denied`。

TypeScript / Python 没有 process 运行时，因此不提供 Launch 钩子：执行端自己的 `process.start` handler 就是等价物（读上下文、校验、再 spawn），其 params 里能看到 `package_install`，所以包安装与命令有同样的决策点。见 [`typescript/examples/session-context`](../../typescript/examples/session-context/main.mjs) 与 [`python/examples/session_context.py`](../../python/examples/session_context.py)：缺 `agent_session_id`（承载的 `initiator: "platform"` 除外）与 `package_install` 在读取会话上下文之前就被拒绝。

## 会话文件根

声明 `capabilities.session_files.root`（展开后的绝对路径）的执行端，平台在提示、工具说明和写入规则中使用 `<root>/<会话ID>/uploads`（只读）与 `<root>/<会话ID>/outputs`，代替 `/mnt/session`，同一台机器上的并发会话互不共享文件。不声明则保持 `/mnt/session`。

只有这样的执行端才会收到 `write_file` / `edit_file` 的相对路径：不含 `.`、`..` 段且不以 `~` 开头的 `file_path` 原样进入 `process.start` 的 `file_transfer.local_path`，由执行端在该会话的目录（与会话命令同一个）中解析。其他机器的相对路径在审批提示之前、进程创建之前就被平台拒绝。`uploads/` 下的相对路径与只读输入一样被拒；其余的限制由执行端负责（平台的检查不是沙箱）。绝对路径与 `~/` 路径不变。

Go：`files, _ := NewSessionFiles(SessionFilesOptions{Root: root})`，传 `ProcessOptions{SessionFiles: files}`（自动声明 root），并 `go files.Run(ctx)`。会话的每个进程启动前同步挂载文件到 uploads（sha256 校验、0444、其余条目删除）；outputs 稳定后每 2 秒上传，`process.read` 报告进程结束时立即上传。全部经 `os.Root`，不跟随符号链接，只上传普通文件。`files.Dirs(sessionID)` 返回两个目录（例如写进沙箱配置）。根目录不要放在执行端状态库与 IPC 目录下。TypeScript / Python 声明 `capabilities: { session_files: { root } }`，用同一组 daemon 路由自行同步。

Go SDK 最低版本升至 Go 1.25（`os.Root.MkdirAll` / `Rename`）。
