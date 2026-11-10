# Session context, launch hook, session files and refusal reasons (1.1)

[中文](session-context.zh-CN.md)

A machine-scoped executor (machine daemon token in `EnvironmentKey` / `environmentKey` / `environment_key`, empty environment id) can decide per session where and how a process runs, and refuse with a machine-readable reason. The platform never stores a local path: the session creator puts opaque references in the session `metadata` (as Claude Managed Agents passes file references in session metadata), and the executor resolves them with its own local table.

## Session context

| | Go | TypeScript | Python |
| --- | --- | --- | --- |
| by process (`process.start` of an agent session) | `client.SessionContext(ctx, processID)` | `client.sessionContext(processId)` | `await client.session_context(process_id)` |
| by session (agent capability, no process) | `client.SessionContextByID(ctx, sessionID)` | `client.sessionContextById(sessionId)` | `await client.session_context_by_id(session_id)` |
| not on this machine | `ErrSessionContextNotFound` | `SessionContextNotFoundError` | `SessionContextNotFoundError` |
| not a machine-scoped executor | `ErrMachineTokenRequired` | `MachineTokenRequiredError` | `MachineTokenRequiredError` |

The result is `{id, environment_id, metadata, created_at}` from `GET /api/v1/daemon/processes/{process_id}/session` or `GET /api/v1/daemon/sessions/{session_id}`; it never contains agent configuration, credentials or other sessions. Results are cached per session, so the metadata is the first read's (see [Cached metadata](#cached-metadata)); failures are not cached. `process.start` metadata `agent_session_id` (`MetadataAgentSessionID`) names the session.

## Cached metadata

The result is the session as of the first read: the executor caches it per session (Go, TypeScript and Python alike) and every later process of that session sees the same `metadata`. A later `metadata` patch on the session (the API allows it, as Claude Managed Agents does) is not guaranteed to reach the executor. Callers must not rely on a mid-session change to move a session to another location or owner: set `workspace_ref`, `owner` and the like when creating the session, and create a new session to change them. An executor should itself treat the first value as final and check it, not re-read it to follow changes. Failures (`not found`, transient errors) are not cached, so the next process asks again.

This matches Claude Managed Agents: the work item a worker claims carries the session id but not the metadata, so the worker reads the session once when it takes the work and uses that value for the run ([Stage files for a session](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-workers#stage-files-for-a-session), the metadata read is stated there; "read once, not re-read" is inferred from its spawn script, not stated).

## Which processes carry `agent_session_id`

Who started a `process.start` is told by its metadata, as defined by the platform ([executor-session-files](https://github.com/beeos-ai/beeos-managed-agents/blob/main/docs/architecture/executor-session-files.md), contract [`executor.openrpc.yaml`](../../contracts/bma/executor/executor.openrpc.yaml)):

| Started by | Message | Metadata |
| --- | --- | --- |
| An agent tool: commands (`run_command`, shell tools), file transfers (`write_file`, `edit_file`, storage upload and download), package installs (`install_packages`), brain-hosted tool commands | `process.start` | `agent_session_id` and `tool_call_id`; no `initiator` |
| The platform on its own, such as the machine desktop agent | `process.start` | `initiator: "platform"`; no `agent_session_id` |
| A person in the console: a terminal | `terminal.attach`, not `process.start` | none; a terminal is always human-opened, no agent tool starts one |

A `process.start` with neither key is not one the harness sends. An executor that decides by session (`Launch`, session files, owner check) must handle it explicitly: refuse with `session_rejected` instead of falling back to a default folder. `initiator` is an open set; `platform` is the only value defined, and an unknown value without `agent_session_id` is refused like a missing key.

A platform process belongs to no session, so it must not land in a session directory. An executor that hosts it decides by its own policy (a directory and confinement of its own, as `-platform-dir` in the example); one that does not refuses it with `session_rejected`. A person's terminal arrives as `terminal.attach` and is served by the executor's own terminal capability, with its own authentication and policy, not by the session path. Go delivers the keys as `ProcessContext.AgentSessionID` (empty when absent) and `ProcessContext.Metadata["initiator"]` (also in `LaunchRequest`); in TypeScript and Python they are `params.metadata?.agent_session_id` / `params.metadata?.initiator` (`params["metadata"].get(...)`), absent or empty when not sent.

## Refusing with a reason

Answer `process.start` (or refuse a `tool.call`) with a JSON-RPC error whose `data.reason` is machine-readable; BMA passes it unchanged to the tool result's `error_code` and the turn's `session.error.code`.

| Reason | Go | TypeScript / Python `ErrorReason.` |
| --- | --- | --- |
| `workspace_unavailable` | `ReasonWorkspaceUnavailable` | `WorkspaceUnavailable` |
| `workspace_boundary` | `ReasonWorkspaceBoundary` | `WorkspaceBoundary` |
| `workspace_read_only` | `ReasonWorkspaceReadOnly` | `WorkspaceReadOnly` |
| `session_rejected` | `ReasonSessionRejected` | `SessionRejected` |
| `local_confirmation_denied` | `ReasonLocalConfirmationDenied` | `LocalConfirmationDenied` |
| `local_confirmation_expired` | `ReasonLocalConfirmationExpired` | `LocalConfirmationExpired` |
| `spawn_failed` | `ReasonSpawnFailed` | `SpawnFailed` |
| `process_id_spec_mismatch` | `ReasonProcessIDSpecMismatch` | `ProcessIdSpecMismatch` |

The set is open: any other value is passed through. Construct the error with Go `NewReasonError(CodeForbidden, ReasonSessionRejected, msg)`, TypeScript `RpcError.withReason(Code.Forbidden, ErrorReason.SessionRejected, msg)` or Python `RPCError.with_reason(JsonRpcErrorCode.Forbidden, ErrorReason.SessionRejected, msg)`. Only a refusal at start gets a structured reason; a command denied by the OS sandbox while it runs reports its exit code and output.

## Go: `ProcessOptions.Launch`

`Launch(ctx, LaunchRequest) (LaunchSpec, error)` runs once per new command process or package install (not for replays or file transfers), after the session's files are synced and before spawning. `LaunchRequest` carries the executor session id, process id, `AgentSessionID`, `Metadata`, the requested `Argv`, the unresolved `Cwd`, the session workspace `DefaultCwd` and the computed `Env`. For a package install `PackageInstall` holds the manager and packages, and `Argv`, `Cwd` and `Env` are the installer's fixed command (its `Dir` as `Cwd`); `PackageInstall == nil` is a command. `LaunchSpec{Dir, Argv, Env}` replaces them; an empty field keeps the request's value (no `Dir` confines the requested cwd to the workspace as without a hook; a `Dir` must be absolute and is not confined). An `*RPCError` refuses the start as is; any other error is `spawn_failed`.

A package install calls a package manager, may run install scripts and writes system or global directories, so a hook that confines or refuses commands should handle `req.PackageInstall != nil` explicitly: wrap it like a command, change its environment, or refuse it with a reason (for example `local_confirmation_denied` when the user must confirm locally). An empty `LaunchSpec` keeps the installer's own command, directory and environment. Without a `Launch` hook installs run as before.

### Bash tools: `CoreOptions.Launch`

`CoreOptions.Launch` takes the same `LaunchFunc` and runs once per bash tool call, before its command starts: a `tool.call` bash and the bash of a `LocalToolRoute`, which is what `ACPAgent` runs its own tools through. Pass the same function to both options and the brain's own commands get the same admission check and sandbox as `process.*`. The request has `Tool: "bash"` and `ToolCallID`; `ProcessID` is empty; `Argv` is the shell command (`bash -lc <command>`, `sh` when bash is missing; `cmd.exe /d /s /c <command>` on Windows); `Cwd` is the call's cwd (empty for a `LocalToolRoute`). For a `tool.call`, `AgentSessionID`, `Metadata` and `Client` come from the session's `session.assign`; for a `LocalToolRoute`, `SessionID` and `AgentSessionID` are the route's session and there is no `Metadata` or `Client` (read the session context with your own client if you need it). The spec is applied as for a process. A refusal starts nothing: a `tool.call` fails with the text `<reason>: <message>`, a `LocalToolRoute` call is `not_forwarded` with the reason as `Code` (`spawn_failed` for a plain error) and the same call id is answered with that again. File tools (read/write/edit/glob/grep) run inside the executor and do not call this hook; see the next section. Without `CoreOptions.Launch` bash tools run as before.

A `LocalToolRoute` bash writes its output to a file (the background answer names it). By default that file is under `.bma-bash/` in the turn's working directory, written by the executor, outside any sandbox. A host that sandboxes commands sets `CoreOptions.ToolOutputDir` to a directory outside every folder a session may write (and that no session's sandbox opens): the files then go to `ToolOutputDir/<short hash of the session>/` (0700, owned by `RunAs` when set), nothing is written into the user's folders, the session's `read` tool may still read its own output files there, and `CloseSession` removes that directory. `tool.call` bash keeps its output in memory and writes no file.

### File tools: the workspace boundary and `CoreOptions.FileAccess`

`read`, `write`, `edit`, `glob` and `grep` (a `tool.call` and a `LocalToolRoute`, so `ACPAgent` too) run in the executor process, past any sandbox `Launch` sets up. They are confined to the session workspace by the system, whatever the host does: every file is reached through an `os.Root` opened on the workspace, so `..`, an absolute path outside it and a symbolic link that leads out of it are refused, also when a directory is swapped for a link between a check and the use. Links that stay inside the workspace work. `glob` and `grep` do not cross a link out of the workspace. The session's own `ToolOutputDir` output files stay readable with `read`.

`CoreOptions.FileAccess` (`func(ctx, FileAccessRequest) error`) lets the host decide per call, before the tool touches anything. `FileAccessRequest` has `SessionID`, `AgentSessionID` (the same id for a `LocalToolRoute`; the `session.assign` metadata `agent_session_id` for a `tool.call`), `Tool`, `ToolCallID`, `Operation` (`FileOperationRead` or `FileOperationWrite`) and `Path` (relative to the workspace, cleaned, slash-separated, `.` for the workspace). `read`, `glob` and `grep` read (the last two are asked about their search directory, not each file); `write` writes; `edit` reads and writes, so the hook is called twice, read first. A path outside the workspace fails by itself without asking. Return an error to refuse: an `*RPCError` (`NewReasonError`) gives its `data.reason` as the code, such as `workspace_read_only` for a read-only session or `workspace_boundary` for a protected path like `.git/hooks`; any other error is `permission_denied`. Nothing is read or written then. A `tool.call` fails with `<code>: <message>`; a `LocalToolRoute` call is `not_forwarded` with the code as `Code`, recorded like a result, so the same call id gets the same answer again without asking the hook. A host also uses it to refuse once a grant is revoked or the turn stopped. Without the option file tools behave as before (confined to the workspace).

To switch a tool off before such checks exist, set `ACPAgentOptions.ToolPolicy` (below); it is a ceiling, `deny` over `ask` over `allow`, that the platform's `tool_policy` and the permission mode can only tighten.

A file transferer that implements `SessionFileTransferer` gets `TransferSessionFile(ctx, ProcessContext, transfer)` instead of `TransferFile`, so it can check a path against the session's folder.

See [`examples/sandboxed-folder`](../../examples/sandboxed-folder): metadata `owner` check (`session_rejected`), `workspace_ref` → local folder (`workspace_unavailable`), cwd outside the folder (`workspace_boundary`), command wrapped in macOS `sandbox-exec` that only allows writes to the folder and the session's outputs, a process without `agent_session_id` refused with `session_rejected` (a platform-initiated one runs in its own `-platform-dir`, outside every session folder, when that is set), and a package install refused with `local_confirmation_denied`.

### Host ceiling on tool permissions: `ACPAgentOptions.ToolPolicy`

`ACPAgentOptions.ToolPolicy map[string]string` (tool name to `allow`, `ask` or `deny`) is the host's ceiling for the tools of an `ACPAgent`, the six and the host tools. In a turn a tool's permission is the stricter of this value and what the permission mode and the platform's `turn.start` `metadata.tool_policy` give (`deny` over `ask` over `allow`), so neither can loosen it; a tool it does not name is not capped. A host that has not wired the checks for a tool yet sets it to `deny` (fail closed). The executor's gate enforces it whatever the agent does (`permission_denied`, or `approval_required` for `ask` without the user's approval). `NewACPAgent` rejects other values and empty tool names.

TypeScript and Python have no process runtime, so there is no Launch hook: the executor's own `process.start` handler is the equivalent (read the context, check, then spawn), and it sees `package_install` in its params, so installs get the same decision point as commands. See [`typescript/examples/session-context`](../../typescript/examples/session-context/main.mjs) and [`python/examples/session_context.py`](../../python/examples/session_context.py): missing `agent_session_id` (except a hosted `initiator: "platform"`) and `package_install` are refused before the session context is read.

## Session files root

An executor that declares `capabilities.session_files.root` (an absolute, expanded path) gets `<root>/<session id>/uploads` (read-only) and `<root>/<session id>/outputs` instead of `/mnt/session` in prompts, tool descriptions and write rules, so concurrent sessions on one machine do not share files. Without the declaration the platform keeps `/mnt/session`.

Only such an executor receives relative `write_file` / `edit_file` paths: a `file_path` without `.` or `..` segments and not starting with `~` goes unchanged into `process.start` `file_transfer.local_path`, and the executor resolves it in the session's directory (the one it picked for the session's commands). Any other machine gets a relative path refused by the platform before the approval prompt and before a process exists. A relative path under `uploads/` is refused like the read-only inputs; confining everything else is the executor's job (the platform check is not a sandbox). Absolute and `~/` paths are unchanged.

Go: `files, _ := NewSessionFiles(SessionFilesOptions{Root: root})`, pass `ProcessOptions{SessionFiles: files}` (the root is advertised for you) and run `go files.Run(ctx)`. Before each process of an agent session the session's mounts are synced into its uploads (sha256-checked, 0444, everything else removed); outputs are uploaded every 2 s once stable and when a `process.read` reports a process ended. All file operations go through `os.Root`; symlinks are never followed and only regular files are uploaded. `files.Dirs(sessionID)` gives the two directories, for example for a sandbox profile. Keep the root apart from the executor's state and IPC directories. TypeScript and Python declare `capabilities: { session_files: { root } }` and sync with the same daemon routes themselves.

The Go SDK now needs Go 1.25 (`os.Root.MkdirAll` / `Rename`).
