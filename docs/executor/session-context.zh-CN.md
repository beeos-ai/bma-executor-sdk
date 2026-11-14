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

`CoreOptions.Launch` 用同一个 `LaunchFunc` 类型，每次 bash 工具调用在起命令前调用一次：包括 `tool.call` 的 bash，以及 `LocalToolRoute` 的 bash（`ACPAgent` 自己的工具就走它）。两个选项传同一个函数，脑自己的命令就和 `process.*` 走同一套准入和沙箱。请求里 `Tool` 为 `"bash"`，`ToolCallID` 为调用 id，`ProcessID` 为空；`Argv` 是 shell 命令（`bash -lc <命令>`，没有 bash 时用 `sh`；Windows 上是 `cmd.exe /d /s /c <命令>`）；`Cwd` 是调用的 cwd（`LocalToolRoute` 为空）。`tool.call` 的 `AgentSessionID`、`Metadata`、`Client` 取自该会话的 `session.assign`；`LocalToolRoute` 的 `SessionID` 与 `AgentSessionID` 都是路由的会话，没有 `Metadata` 和 `Client`（需要会话上下文时用自己的 client 去查）。返回的 spec 按进程的规则生效。拒绝时不起进程：`tool.call` 返回失败结果，文本为 `<原因>: <说明>`；`LocalToolRoute` 返回 `not_forwarded`，`Code` 为原因（普通错误为 `spawn_failed`），同一调用 id 再来时原样返回。文件工具（read/write/edit/glob/grep）在执行端进程内完成，不调用这个钩子，见下一节。不设 `CoreOptions.Launch` 时 bash 工具行为不变。

`LocalToolRoute` 的 bash 把输出写进一个文件（后台运行的回答里给出路径）。默认这个文件在本轮工作目录的 `.bma-bash/` 下，由执行端进程写入，不在沙箱里。给命令加了沙箱的宿主应设 `CoreOptions.ToolOutputDir`，指向不在任何会话可写范围内（也不对任何会话沙箱开放）的目录：输出文件改放 `ToolOutputDir/<会话的短哈希>/`（权限 0700，设了 `RunAs` 时归该用户），不再往用户文件夹里写；该会话的 `read` 工具仍可读自己的输出文件；`CloseSession` 时删掉这个目录。`tool.call` 的 bash 输出留在内存里，不写文件。

### 文件工具：工作区边界与 `CoreOptions.FileAccess`

`read`、`write`、`edit`、`glob`、`grep`（`tool.call` 和 `LocalToolRoute`，所以 `ACPAgent` 也是）在执行端进程内运行，绕过 `Launch` 搭的沙箱。无论宿主怎么做，它们都由系统限制在会话工作区内：所有文件都通过在工作区上打开的 `os.Root` 访问，`..`、工作区外的绝对路径、指向工作区外的符号链接都会被拒绝，检查之后目录被换成链接也一样。指向工作区内部的链接照常可用。`glob` 和 `grep` 不穿过指向工作区外的链接。会话自己在 `ToolOutputDir` 下的输出文件仍可用 `read` 读取。

`CoreOptions.FileAccess`（`func(ctx, FileAccessRequest) error`）让宿主在工具碰任何文件之前按调用裁决。`FileAccessRequest` 含 `SessionID`、`AgentSessionID`（`LocalToolRoute` 与会话 id 相同；`tool.call` 取自 `session.assign` 的 metadata `agent_session_id`）、`Tool`、`ToolCallID`、`Operation`（`FileOperationRead` 或 `FileOperationWrite`）和 `Path`（相对工作区、已规范化、用 `/` 分隔，工作区本身为 `.`）。`read`、`glob`、`grep` 为读（后两者问的是搜索目录，不是其下每个文件）；`write` 为写；`edit` 又读又写，钩子调用两次，先读后写。路径在工作区外时不问钩子，工具自己失败。返回 error 即拒绝：`*RPCError`（`NewReasonError`）的 `data.reason` 作为拒绝码，例如只读会话用 `workspace_read_only`、`.git/hooks` 之类保护路径用 `workspace_boundary`；其他错误为 `permission_denied`。拒绝时不读不写：`tool.call` 返回失败结果，文本为 `<码>: <说明>`；`LocalToolRoute` 返回 `not_forwarded`，`Code` 为拒绝码，并像结果一样入账，同一调用 id 再来时原样返回、不再问钩子。宿主也可以用它在授权撤销或回合停止后拒绝。不设该选项时文件工具行为不变（仍限制在工作区内）。

#### 会话挂载的文件（`ACPAgentOptions.SessionFilesRoot`）

声明了 `capabilities.session_files` 的机器上，平台把每个会话的文件挂载为 `<root>/<会话 id>/uploads`（用户文件，只读）、`.../skills`（会话技能，只读）和 `.../outputs`（交付物，可写）。把声明的 `root` 交给 `ACPAgentOptions.SessionFilesRoot`（或 `LocalToolRouteOptions.SessionFilesRoot`），该会话回合的文件工具除工作区外，按绝对路径只可访问这三个目录：`read`、`glob`、`grep` 三个都可，`write`、`edit` 仅限 `outputs`。其余一律拒绝：其他会话的目录、根下的其他路径、`..`、指向目录之外的符号链接，以及顶替会话目录或挂载目录的链接（每个目录各自以 `os.Root` 打开，且必须是真目录）。`glob`、`grep` 对挂载目录报告绝对路径。`CoreOptions.FileAccess` 对这些路径同样被询问，`FileAccessRequest.Mount` 为 `uploads`/`skills`/`outputs`，`Path` 相对该目录，因此只守护工作区的钩子必须检查 `Mount`。在带 FUSE 会话挂载的执行端上，写入 `outputs` 的文件在关闭时由挂载提交；SDK 自带的 `SessionFiles` 扫描只上传启动过进程的会话的 outputs（其 daemon 路由以进程为锚），所以收不到只有 ACP 回合的会话的产出。工具在另一台机器上执行的回合（`tools_mcp_*`）不会在本机得到这些挂载。

在这些检查接好之前想关掉某个工具，用下面的 `ACPAgentOptions.ToolPolicy`；它是上限，`deny` 严于 `ask` 严于 `allow`，平台的 `tool_policy` 和权限模式只能更严。

实现 `SessionFileTransferer` 的文件传输器改收 `TransferSessionFile(ctx, ProcessContext, transfer)`。

示例 [`examples/sandboxed-folder`](../../examples/sandboxed-folder)：metadata `owner` 校验（`session_rejected`）、`workspace_ref` → 本机目录（`workspace_unavailable`）、cwd 越界（`workspace_boundary`）、macOS `sandbox-exec` 包装（只允许写该目录与会话 outputs）、缺 `agent_session_id` 的进程返回 `session_rejected`（平台自发进程在设置了 `-platform-dir` 时运行于该独立目录，不进任何会话目录）、包安装返回 `local_confirmation_denied`。

### 宿主的工具权限上限：`ACPAgentOptions.ToolPolicy`

`ACPAgentOptions.ToolPolicy map[string]string`（工具名 → `allow`、`ask`、`deny`）是宿主对 `ACPAgent` 工具（六个工具和宿主工具）的权限上限。一个回合里某工具的权限，取这个值与权限模式、平台 `turn.start` 的 `metadata.tool_policy` 三者中较严的（`deny` > `ask` > `allow`），所以后两者只能更严、不能放松；没列出的工具不设上限。宿主还没接好某工具的检查时，先把它设为 `deny`（fail closed）。无论脑怎么做，都由执行端的 gate 强制（`permission_denied`；`ask` 没有用户批准时为 `approval_required`）。`NewACPAgent` 对其他取值和空工具名报错。

### 宿主决定电脑操作要不要审批：`ComputerUseApprover`

`computer_observe` 和 `computer_action` 默认是 `ask`：每次调用都在平台上审批。`CoreOptions.ComputerUse` 的后端如果同时实现 `ComputerUseApprover`（`NeedsObserveApproval`、`NeedsActionApproval`），就逐次由它决定：答 `false` 的调用上报为 `allow`（`agent.tool_use` 的 `evaluated_permission`），在账本里记为已允许，不发 `interaction.request` 直接执行；答 `true` 照旧询问。只有当 `ask` 来自工具的默认值时才问它：平台的 `metadata.tool_policy` 或 `ACPAgentOptions.ToolPolicy` 写了这个工具，就按写的值办，不再问它。每次调用只问一次。判断不了的调用（比如不知道点击会点到什么）应答 `true`。只有 Go 有，和电脑操作本身一样。

### 审批前先拒绝：`ComputerUsePreflighter`

后端如果同时实现 `ComputerUsePreflighter`（`PreflightObserve`、`PreflightAction`），可以在问人之前拒绝一次调用，比如点到别的应用、窗口在别的桌面空间。本机脑的调用顺序是：输入校验（含 `window_frame_required`）→ 预检 → 审批（`ComputerUseApprover` 或平台策略）→ 执行；被拒的调用上报为 `allow`，结果是带错误码的工具错误，不问任何人。平台脑的 `tool.call` 在到达前已在平台上审批，所以那条路径是输入校验 → 预检 → 执行。拒绝文本：带 data reason 的 `*RPCError`（`NewReasonError`）为 `<reason>: <message>`；文本本身以 `<code>: ` 开头时原样；否则为 `computer_use_refused: <文本>`。请求带 `SessionID`，动作还带 `Frame`（坐标所属的、这个会话最近一次观察）。

### 只能读文字的模型：`CoreOptions.ComputerVision`

`ComputerVision(ctx, ComputerVisionQuery{SessionID, Model})` 回答这个会话的模型能不能看图（本机脑回合里 `Model` 是这一轮的模型，`tool.call` 为空）。答 `ComputerVisionTextOnly` 时照常截图（动作按它的像素），但不发给模型，结果里写明原因；`ComputerVisionUnknown` 或没有回调时照常发图。

TypeScript / Python 没有 process 运行时，因此不提供 Launch 钩子：执行端自己的 `process.start` handler 就是等价物（读上下文、校验、再 spawn），其 params 里能看到 `package_install`，所以包安装与命令有同样的决策点。见 [`typescript/examples/session-context`](../../typescript/examples/session-context/main.mjs) 与 [`python/examples/session_context.py`](../../python/examples/session_context.py)：缺 `agent_session_id`（承载的 `initiator: "platform"` 除外）与 `package_install` 在读取会话上下文之前就被拒绝。

## 会话文件根

声明 `capabilities.session_files.root`（展开后的绝对路径）的执行端，平台在提示、工具说明和写入规则中使用 `<root>/<会话ID>/uploads`（只读）与 `<root>/<会话ID>/outputs`，代替 `/mnt/session`，同一台机器上的并发会话互不共享文件。不声明则保持 `/mnt/session`。

只有这样的执行端才会收到 `write_file` / `edit_file` 的相对路径：不含 `.`、`..` 段且不以 `~` 开头的 `file_path` 原样进入 `process.start` 的 `file_transfer.local_path`，由执行端在该会话的目录（与会话命令同一个）中解析。其他机器的相对路径在审批提示之前、进程创建之前就被平台拒绝。`uploads/` 下的相对路径与只读输入一样被拒；其余的限制由执行端负责（平台的检查不是沙箱）。绝对路径与 `~/` 路径不变。

Go：`files, _ := NewSessionFiles(SessionFilesOptions{Root: root})`，传 `ProcessOptions{SessionFiles: files}`（自动声明 root），并 `go files.Run(ctx)`。会话的每个进程启动前同步挂载文件到 uploads（sha256 校验、0444、其余条目删除）；outputs 稳定后每 2 秒上传，`process.read` 报告进程结束时立即上传。全部经 `os.Root`，不跟随符号链接，只上传普通文件。`files.Dirs(sessionID)` 返回两个目录（例如写进沙箱配置）。根目录不要放在执行端状态库与 IPC 目录下。TypeScript / Python 声明 `capabilities: { session_files: { root } }`，用同一组 daemon 路由自行同步。

Go SDK 最低版本升至 Go 1.25（`os.Root.MkdirAll` / `Rename`）。
