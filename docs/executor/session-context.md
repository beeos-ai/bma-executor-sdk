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

The result is `{id, environment_id, metadata, created_at}` from `GET /api/v1/daemon/processes/{process_id}/session` or `GET /api/v1/daemon/sessions/{session_id}`; it never contains agent configuration, credentials or other sessions. Results are cached per session (metadata is read once, as of the session's start); failures are not cached. `process.start` metadata `agent_session_id` (`MetadataAgentSessionID`) names the session.

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

`Launch(ctx, LaunchRequest) (LaunchSpec, error)` runs once per new command process (not for replays, file transfers or package installs), after the session's files are synced and before spawning. `LaunchRequest` carries the executor session id, process id, `AgentSessionID`, `Metadata`, the requested `Argv`, the unresolved `Cwd`, the session workspace `DefaultCwd` and the computed `Env`. `LaunchSpec{Dir, Argv, Env}` replaces them; an empty field keeps the request's value (no `Dir` confines the requested cwd to the workspace as without a hook; a `Dir` must be absolute and is not confined). An `*RPCError` refuses the start as is; any other error is `spawn_failed`.

A file transferer that implements `SessionFileTransferer` gets `TransferSessionFile(ctx, ProcessContext, transfer)` instead of `TransferFile`, so it can check a path against the session's folder.

See [`examples/sandboxed-folder`](../../examples/sandboxed-folder): metadata `owner` check (`session_rejected`), `workspace_ref` → local folder (`workspace_unavailable`), cwd outside the folder (`workspace_boundary`), command wrapped in macOS `sandbox-exec` that only allows writes to the folder and the session's outputs.

TypeScript and Python have no process runtime, so there is no Launch hook: the executor's own `process.start` handler is the equivalent (read the context, check, then spawn). See [`typescript/examples/session-context`](../../typescript/examples/session-context/main.mjs).

## Session files root

An executor that declares `capabilities.session_files.root` (an absolute, expanded path) gets `<root>/<session id>/uploads` (read-only) and `<root>/<session id>/outputs` instead of `/mnt/session` in prompts, tool descriptions and write rules, so concurrent sessions on one machine do not share files. Without the declaration the platform keeps `/mnt/session`.

Go: `files, _ := NewSessionFiles(SessionFilesOptions{Root: root})`, pass `ProcessOptions{SessionFiles: files}` (the root is advertised for you) and run `go files.Run(ctx)`. Before each process of an agent session the session's mounts are synced into its uploads (sha256-checked, 0444, everything else removed); outputs are uploaded every 2 s once stable and when a `process.read` reports a process ended. All file operations go through `os.Root`; symlinks are never followed and only regular files are uploaded. `files.Dirs(sessionID)` gives the two directories, for example for a sandbox profile. Keep the root apart from the executor's state and IPC directories. TypeScript and Python declare `capabilities: { session_files: { root } }` and sync with the same daemon routes themselves.

The Go SDK now needs Go 1.25 (`os.Root.MkdirAll` / `Rename`).
