# Changelog

## Unreleased

- **Go: `CoreOptions.Launch`, the Launch hook for bash tools.** It takes the
  same `LaunchFunc` as `ProcessOptions.Launch` and runs once per bash tool
  call before its command starts: a `tool.call` bash and the bash of a
  `LocalToolRoute`, so `ACPAgent`'s own tools too. Before, those commands
  started with `sh -lc` and skipped the host's admission check and sandbox
  that `process.*` commands get; pass the same function to both options to
  close that gap. The hook may change dir, argv and env, or refuse, as for a
  process. `LaunchRequest` gains `Tool` (`"bash"`; `""` for `process.start`)
  and `ToolCallID`; `ProcessID` is empty for a tool, and a `LocalToolRoute`
  has no `Metadata` or `Client` (`AgentSessionID` is its session). A refusal
  starts nothing: a `tool.call` fails with `<reason>: <message>`; a
  `LocalToolRoute` call is `not_forwarded` with the reason as `Code`
  (`spawn_failed` for a plain error) and is recorded like a result, so the
  same call id gets the same answer, as behind the platform gateway. File
  tools do not call the hook. Without the option nothing changes.

## 1.4.0

- **Contract snapshot synced to beeos-managed-agents f872a540b (contract
  1.4.0):** `FsCapability.skills_per_session` (Go `SkillsPerSession`, TS/Python
  generated types): an executor that mounts each session's skills view at the
  path of its `fs.mount` sets it, and the harness then mounts skills at
  `<root>/<agent_session_id>/skills` instead of the fixed `/mnt/skills`. The
  SDK itself does not mount views, so it never sets it; field and doc changes
  only. `FsMountParams.service_url` no longer documents the old `/v1/fs/<op>`
  alias (executors already use the given prefix as is).
- **Go: `LocalToolRoute`, the local counterpart of `PlatformToolRoute`.**
  `NewLocalToolRoute(LocalToolRouteOptions{Core, StateDir, SessionID, TurnID,
  Workspace, PermissionMode, ToolPolicy, Context, OnLog})` is a `ToolRoute`
  that runs the six tools on this machine, in a `Core`, behind the executor's
  own gate (the one `ACPAgent` has had since the local tool re-check):
  `stale_turn` for a call after its turn ended or from an older turn,
  `permission_denied` for `deny`, and for `ask` the user's `allow` for this very
  call (`RecordApproval(callID, request, answer)` after
  `AgentTurn.RequestInteraction`; none is `approval_required`, another tool,
  input or approval id `approval_invalid`). A call id runs at most once, also
  across executor restarts (turn state under `StateDir`; a started call whose
  result was lost answers `outcome_unknown`). `Complete()` lets a bash command
  left in the background outlive the turn; `CloseSession()` ends the session.
  `ACPAgent` now runs its local turns through this same route: the gate, the
  ledger and the `bash` background logic are one implementation, not a copy
  (file names changed accordingly: `agent_acp_tools.go`/`agent_acp_bash.go` are
  `toolroute_local*.go`). No behavior change for `ACPAgent`; the turn state
  ledger is now kept per `Core` (one in-memory copy per file) instead of per
  `ACPAgent`, and `Core.Close` also ends bash commands left in the background.
  New exported function `ApprovalID(sessionID, turnID, callID)` (what
  `AgentTurn.ApprovalID` returns) for code that has no `AgentTurn`.
- **Go: the tool route conformance suite is a public package for tests,
  `github.com/beeos-ai/bma-executor-sdk/conformance` (cases T1–T11).**
  `RunToolRouteConformance(t, factory)` runs the cases against the route a
  `Factory` builds (a `Rig`: the route and the levers - approval, restart,
  newer epoch, cancel, offline hands), so a third-party brain or hands can hold
  its own route to the same cases the SDK holds `LocalToolRoute` and
  `PlatformToolRoute` to. `PlatformFactory(hands)` builds the platform route
  against `conformance/gatewaystub`, a standard-library-only contract double of
  the platform's tool gateway, with hands of your choosing (`CoreHands`,
  `DefaultHands`). `examples/third-party-brain` and `examples/third-party-hand`
  run the suite in their tests; the SDK runs it against both of its routes.
  The packages import package `testing`: production code must not import them
  (the new `arch_test.go` fails if any non-test file of the module does, or if
  a conformance package imports an `internal/` package) and they are not built
  on Windows. Test-only; nothing changes for programs that do not import them.
- **Go `ACPAgent`, Claude Code profile: prewarm.** `NewACPAgent` starts the
  agent process as before and now also opens one throwaway session in the
  background (scratch directory `StateDir/prewarm`, closed unused, never
  prompted, so no model credit is spent), which takes the cold start of Claude
  Code off the first real session. It never delays or fails registration; a
  failure is logged (`agent prewarm failed`, LogWarn) and returned by the new
  `ACPAgent.WaitPrewarm(ctx)`. `ACPAgentOptions.DisablePrewarm` turns it off.
  Measured with the pinned claude-agent-acp on one machine: `turn.start` is
  answered in ~1 ms (before: the whole session/new, 0.5 s to 4.6 s); the first
  content event of the first turn comes ~0.2-0.26 s after `turn.start` (before:
  0.57-4.64 s; without the prewarm, 0.35-0.7 s). The DSH profile has no
  prewarm (its process is resident and its session/new is cheap).
- **Behavior change, Go `ACPAgent`: `turn.start` is answered at once.** As the
  contract says ("answers as soon as the turn is accepted"), `OpenTurn` only
  validates (harness, permission mode, cwd, session capacity: still
  `turn.start` errors) and the agent's session is opened first thing in
  `RunTurn`. A failed open (agent process unavailable, a conversation to
  resume that does not exist, the agent's refusal, a failed mode gate) is no
  longer a `turn.start` error but ends the turn with `session.error` and
  `session.status_idle` failed, typed with the contract's `AgentErrorCode`:
  `agent_unavailable`, `agent_auth_required` (ACP -32000),
  `agent_protocol_error` (-32602 / -32601, a tool catalog violation) or
  `agent_error`. `turn.cancel` or the deadline during the open ends the turn
  canceled immediately; the open still finishes and leaves its session for the
  next turn. New `AgentError{Code, Message}`: a `RunTurn` error of that type
  picks the `session.error` type (any other error stays `agent_error`).
  `TurnStartResult.executor_session_ref` of the first turn of a Claude Code
  session is empty (Claude Code names its session only after `session/new`,
  and the id cannot be chosen); from the second turn on it is the real id.
- **Go: the brain-side tool API is public (ADR-0029 S3):** `ToolRoute` (one
  method, `Call(ctx, RouteCall) *RouteResult`), `RouteCall` (`ID`, `ToolUseID`,
  `Name`, `Input`, `ApprovalID`), `RouteResult` with the `RouteOutcome*`
  constants and `RouteBackground`, `PlatformToolRoute` with
  `NewPlatformToolRoute`, and on `AgentTurn`: `Tools()` (the platform route of
  a turn that carries `tools_mcp_*`, a refusing route for a half credential,
  nil when the hands are this machine) and `ApprovalID(callID)` (the
  deterministic `appr_` interaction id). A third-party brain needs only the SDK
  and the contract; two runnable skeletons with tests:
  `examples/third-party-brain` and `examples/third-party-hand`. Previously unexported
  (`acpToolResult`, `toolBackground`, `platformToolRoute`): no earlier release
  exposed them.
- **Behavior change, Go `ACPAgent` local `bash` (ADR-0029 T6):** `timeout` is
  now how long the call waits, not how long the command may live, as on the
  platform's tool gateway. A command that outlives the wait (at most 55 s, or
  `timeout` when shorter; default `timeout` 120 s, at most 600 s) is no longer
  killed: the call answers `running in the background (pid N), output in
  <file>` (the file is under `.bma-bash/` in the working directory) and the
  command keeps running until it exits, 1 h, a canceled or timed-out turn, the
  end of the session or `ACPAgent.Close`; a turn that completes does not stop
  it. `on_timeout: "kill"` keeps the old behavior of stopping it at the end of
  the wait. Output of a command that finishes within the wait is unchanged
  (stdout and stderr now share one stream, in write order). `Core`'s own bash
  (the `tool.call` path of a hand) is unchanged. The tool route conformance
  suite no longer exempts the local route from T6.
- **Go: platform tool route wired into `ACPAgent` (contract 1.4.0, ADR-0029
  S3):** a turn whose `turn.start` metadata carries `tools_mcp_url` and
  `tools_mcp_token` sends every tool call to the platform's tool gateway (the
  hands are on another machine) and never runs one here; any other turn runs
  its tools here as before. `ACPAgentOptions.PlatformTools` declares
  `platform_credentials: [brain_tools]`. The token is kept in memory only and
  replaced with the new `Client.RefreshToolsToken` / `AgentTurn.RefreshToolsToken`
  (`tools_token.refresh`) before it expires; a failed refresh is logged and
  retried with backoff (5 s to 1 min). 401/403 from the gateway is
  `not_forwarded` / `stale_turn`. `AgentTurn.EnvironmentInfo()` returns the
  hands machine's description; for a remote-hands turn `config.cwd` is not
  checked against this machine's roots and the agent works in a scratch
  workspace.
- **Go: `Options.Labels` and `Options.RemoteBrain`:** report
  `ExecutorInfo.labels` (checked against the contract's limits) and
  `Capabilities.remote_brain` (off unless set; the platform cannot enable it).
- **Go: Claude Code profile of `ACPAgent`
  (`ACPAgentOptions.Profile: ACPProfileClaudeCode`):** hosts the official
  claude-agent-acp (pinned release, unmodified) with the node owner's own
  credentials only; Claude Code's tools are replaced by the six platform
  tools served as an MCP server on loopback. `session/set_mode default` after
  `session/new` with an asserted default mode, a checked tool catalog (known
  placeholder tools of the pinned release are listed explicitly), foreign
  tools rejected, no `harness_model`, no `turn_resume`. `examples/local-dsh`
  gains `-profile`, `-labels`, `-remote-brain`, `-platform-tools`.

## 1.3.0

- **Go tool route seam (ADR-0029 S1, internal, no API or protocol change):**
  `ACPAgent` now sends every local tool call through an unexported `toolRoute`
  (`localToolRoute`: the executor's re-check, the turn ledger and `Core`, same
  behavior as before). `platformToolRoute` is a second implementation that
  calls the platform's execute-only tool gateway (MCP Streamable HTTP, ADR-0005
  `_meta` keys, three-state outcome, idempotency key, in-memory credential); it
  is not wired into any configuration. A conformance suite (T1–T11) runs the
  same cases against both routes. Known gap: the local `Core` kills a bash
  command at its timeout instead of leaving it running with `pid` and
  `output_file`.

- **Go `ACPAgent` re-checks every local tool call itself (ADR-0005 gateway
  semantics, security fix):** a brain that calls `_bma/tools/call` without
  `session/request_permission` no longer gets the tool run. Before a tool runs
  the executor checks that the call belongs to the session's running turn
  (`stale_turn` otherwise), applies the turn's effective policy (mode defaults
  plus `turn.start` `metadata.tool_policy`; an unlisted tool is `ask`): `deny`
  is refused (`permission_denied`); `ask` needs the platform's `allow` for this
  call (`interaction.response` for the deterministic `appr_` id, same tool and
  input; `approval_required` / `approval_invalid` otherwise); `allow` runs. The
  answer to `_bma/tools/call` now carries `outcome` (`not_forwarded` /
  `executed` / `unknown`) and `errorCode` next to `text` / `isError`; a refused
  call is `isError`, so the brain reports `tool_call_update` failed and the
  platform sees `agent.tool_result` `is_error` with the text `"<code>: ..."`.
  `metadata.tool_policy` values other than `allow|ask|deny` now fail the turn.
- **Go `ACPAgent` in a sandbox (BMA S6):** `ACPAgentOptions.PlatformModel`
  declares `platform_credentials: [harness_model]` and makes `Upstream`
  optional (a turn without the platform credential then fails with
  `session.error`, never goes elsewhere); the platform's `model_token` is
  refreshed with `model_token.refresh` before a forwarded request when at most
  10 minutes of `model_token_expires_at` are left (a failure keeps the current
  token and backs off 5 s to 1 min); the agent process runs as
  `CoreOptions.RunAs` (HOME/USER of the workload, `StateDir` owned by it).
  New `AgentTurn.RefreshModelToken`; the reference `Harness` answers
  `model_token.refresh` when its `AgentPlatform` implements `AgentModelTokens`.
- **Go `Client.RefreshModelToken(ctx, sessionID, turnID)`:** calls the contract's
  `model_token.refresh` and returns the turn's fresh `model_token`,
  `model_base_url` and `expires_at`; one attempt, the caller owns backoff.
- **Contract 1.3.0 (additive, `make contract-sdk` from BMA feat/executor-agent-harnesses):**
  `AgentCapability.harnesses[]` (every hosted agent; the single `harness` is
  deprecated) and `AgentTurnConfig.harness` (the agent a turn runs on).
- **Go `ACPAgent`:** an `AgentHandler` that hosts a local brain over the Agent
  Client Protocol (bma-dsh-acp, ADR-0027): one agent process for all sessions,
  token deltas and durable messages, `agent.tool_use` / `agent.tool_result`,
  permission requests as platform interactions with the deterministic
  `appr_` id (sha256 of session, turn and call), tools run in `Core` in the turn's working directory, a loopback
  model proxy (placeholder key, running-turn and tool-catalog gate), cancel
  with a kill after `CancelGrace`, `max_tokens` as `output_limit_reached`,
  `session/load` from `executor_session_ref`. Example: `examples/local-dsh`.
- **Go `ACPAgent` for a supervising app:** `ACPAgentOptions.OnInteraction` tells
  the app when an approval is asked of the platform and when it ends (answered
  anywhere, expired, withdrawn), with the tool input, under the same `appr_` id;
  `examples/local-dsh` writes these as JSON lines (registered, interaction
  requested/ended) and takes `-stdin-lifeline` (exit when stdin closes, so the
  app's exit or crash stops the executor).
- **Go `ACPAgent` declares `turn_resume`:** after an executor restart the
  platform starts the in-flight turn again (same `turn_id`, `executor_session_ref`);
  ACPAgent loads the conversation (`session/load`) and the agent continues the
  recorded input (same `inputId`). A per-session ledger under
  `StateDir/executor-turns` makes the restart safe: a tool whose result is
  recorded is not run again (the recorded result is returned), one that was
  started without a result has an unknown outcome and is answered with an error
  instead of running again, an approval the platform already answered is not
  asked again and a pending one is asked again under the same `appr_` id, events
  the platform already acknowledged (a reported `agent.tool_result`, a finished
  input) are not emitted again. Tests kill the executor before the tool ran,
  during a pending approval and after the tool ran, then restart it.
- **Contract sync + agent.custom_tool_result (Go/TS/Python):** `make contract-sdk` is synced to
  beeos-managed-agents main (lock `source_commit` is a clean commit). New: capability
  `agent.custom_tools`, event `agent.custom_tool_use`, request `agent.custom_tool_result`.
  Executor side: `AgentTurn.RequestCustomTool` (Go) / `requestCustomTool` (TS) /
  `request_custom_tool` (Python) emits the event and waits for the result; a result for an
  unknown, answered, timed-out or canceled call answers `accepted=false`. Harness side:
  `Executor.RespondCustomToolResult` / `respondCustomToolResult` / `respond_custom_tool_result`;
  `agent.custom_tool_use` is accepted as an agent event. Additive.
- **Note:** main's contract does not carry the `AgentErrorCode` stop code table yet
  (beeos-managed-agents#643 is unmerged), so the generated `AgentErrorCode` constants added by #25
  are absent after this sync; nothing in the SDK uses them. Run `make contract-sdk` again after
  #643 merges to restore them.

Windows executor gaps (additive; no contract change, no change to the minimum
supported runtimes).

- **Go, Windows (G1):** bash tools and processes inherit the variables Windows
  programs need (`SystemRoot`, `windir`, `ComSpec`, `PATHEXT`, `TEMP`/`TMP`,
  `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `ProgramFiles*`, ...) in addition
  to `PATH`/`HOME`/`USER`/`LANG`/`TMPDIR`. `DenyEnv` still applies, a request's
  env overrides an inherited name case-insensitively, and stdio MCP servers
  keep the narrower `mcpBaseEnv`.
- **Go, Windows (G2):** a `cmd.exe /c <command>` (the bash tool and a
  process.start argv of `cmd.exe /d /s /c <command>`) is started with the
  command line verbatim (`SysProcAttr.CmdLine`, with `/s`), so quotes and `$`
  in a PowerShell `-Command` survive.
- **Go, Windows (G4):** every bash tool, process and stdio MCP server runs in
  its own Job Object (`KILL_ON_JOB_CLOSE`; created suspended, assigned, then
  resumed). Cancellation, a timeout, `process.terminate` and the end of the
  session end the whole process tree, including children a process left
  behind after it exited.
- **Go, Windows (G5):** `process.interrupt` sends a console `CTRL_BREAK` to the
  command's process group (the executor creates a hidden console when it has
  none); `process.terminate` asks the same way and kills the tree after
  `grace_ms`. Exits are reported as `exit_signal` `SIGINT`/`SIGTERM`/`SIGKILL`.
- **Go, Windows (G3):** `process.start` `io_mode` `pty` runs on a ConPTY
  pseudo console (Windows 10 1809+; `process.pty` is advertised only where it
  exists), inside the process's Job Object. `process.write` is the terminal's
  input, `process.interrupt` writes `^C`, output is one merged stream that
  ends with EOF when the process does.
- **Go, Windows (G6):** session files work on Windows: a changed mounted file
  replaces its read-only (0444 = read-only attribute) predecessor, stale
  read-only mounts are removed, outputs are collected as before. New
  `NewFileTransferer()` (`ProcessOptions.FileTransfer`) serves `process.start`
  `file_transfer` on a machine-scoped executor like bmad does (upload and
  download via `/api/v1/daemon/processes/{id}/file`, atomic replace that keeps
  the destination's permissions, `~` expands to the home directory).
  `ProcessContext.Client` (additive) is the connection a process came
  through.
- **Go (G7):** an absolute cwd equal to `CoreOptions.WorkRoot` (a machine's
  configured cwd such as `C:\bma\work`) now means the session's workspace
  instead of being refused with `cwd must be under the executor work root`;
  other directories stay confined. Paths compare as real paths, so symlinks,
  8.3 names and drive-letter case do not matter on Windows.
- **Go (G8):** optional computer use. `CoreOptions.ComputerUse`
  (`ComputerUseBackend`) serves `computer_observe` / `computer_action` and
  advertises `computer_use`; without a backend the capability is off and the
  tools are refused as `capability_unavailable`. `NewCuaDriver` is the backend
  on Cua Driver (MIT driver only; the AGPL perception extension and the FSL
  space daemon are not used; telemetry is forced off): screenshots, the
  window list and a window's accessibility tree (UIA on Windows) for
  `computer_observe`, and click / type / key / scroll / drag for
  `computer_action`, delivered to a named window in the background or to the
  focused app on the desktop scope. It runs `cua-driver call` against the
  daemon of the interactive session, so the executor itself may run in
  session 0. A missing driver or daemon makes `NewCuaDriver` fail with
  `ErrComputerUseUnavailable` (the capability stays off); a driver that fails
  later is a failed tool result. Inputs follow Anthropic's computer-use tool
  (`coordinate`, `start_coordinate`, `text`, `scroll_direction`,
  `scroll_amount`) plus `click_count`, `button`, `pid`, `window_id`. Go only;
  there is still no public `computer_*` tool slot in any language.
- **Windows package:** `examples/windows-hand` is the machine executor for a
  Windows machine (cmd.exe tool, processes, ConPTY, session files, file
  transfers, optional Cua Driver). `packaging/windows` builds
  `bma-windows-hand-<sha>-windows-amd64.zip` (`build.sh`, `install.ps1`
  registers logon tasks, `uninstall.ps1`, checksums); CI keeps it as the
  `bma-windows-hand` artifact and installs it on a runner. See
  [docs/executor/windows.md](docs/executor/windows.md).
- **CI:** the Go suite runs on `windows-latest`.

## 1.2.0

Additive; no breaking change ([MIGRATION](MIGRATION.md#120)). Contract 1.2.0
(`make contract-sdk`, BMA #527).

- **All languages:** new `machine_id` option — Go `Options.MachineID`,
  TypeScript `machineId`, Python `machine_id` — defaulting to `BMA_MACHINE_ID`
  (`MachineIDEnv` / `MACHINE_ID_ENV`) when no environment id is set. Go's
  `EnvironmentID` and `MachineID` are alternatives; setting both is an error.
- **Token exchange and renewal** use the executor's own resource:
  `POST /v1/machines/{machine_id}/token` and
  `POST /v1/environments/{environment_id}/token`. A machine token without a
  machine id keeps the legacy `POST /api/v1/executors/token` alias, which the
  server serves until contract 2.0. The channel URL is the token response's
  `ws_url` (`/v1/machines/{machine_id}/channel`,
  `/v1/environments/{environment_id}/channel`), so there is no client change
  for it. Renewal (`executor.token.refresh`) repeats the same exchange.
- **Go:** `UploadArtifact` posts to
  `/v1/environments/{environment_id}/artifacts` and refuses a machine-scoped
  client.
- **Contract:** `Capabilities.fs` (`FsCapability{fuse, views}`),
  `AgentCapability.platform_credentials`, and the `fs.mount` / `fs.unmount`
  methods (`service_url` is the complete prefix
  `<origin>/v1/machines/{machine_id}/fs`; append `/<op>` only).
- Not changed: the `/api/v1/daemon/*` session-context routes and the
  `daemon` symbol names (a later release).

### Toolchain (no API change)

- **Development only:** TypeScript 5.9.3 → 7.0.2 (native compiler, major upgrade of the build tool); the published API is unchanged, emitted `.d.ts` may differ in formatting.

Toolchain and dependency upgrade; no API change and no change to the minimum
supported runtimes (Go 1.25, Node.js 20, Python 3.10).

- **Go:** `golang.org/x/sys` v0.10.0 → v0.47.0, the newest release that still
  builds with Go 1.25. `go-winio` stays at v0.6.2: v0.6.3 and later need
  Go 1.26.
- **CI:** matrix of the oldest supported runtimes (Go 1.25, Node 20,
  Python 3.10), Go 1.26 / Node 24 / Python 3.13, and Go 1.27 / Node 26 /
  Python 3.14; the release job publishes npm with Node 26. GitHub Actions are
  pinned by commit SHA at their latest stable release.
- **Development only:** `@types/node` 22 → 26, pyright 1.1.407 → 1.1.414,
  pytest >= 9, pytest-asyncio >= 1.4.

## 1.1.0

Additive; no breaking change ([MIGRATION](MIGRATION.md#110)). Guide:
[docs/executor/session-context.md](docs/executor/session-context.md).

- **TypeScript / Python examples:** the `session-context` examples now accept
  `session.assign` and report `session.ready`, answer `process.list`, and honor
  `stderr: "merge"`; without these a machine-scoped executor never went online,
  was refused after registering, or failed `process.start` with `-32012`
  against the platform (found in the staging run).
- **Contract:** `Capabilities.session_files` (`SessionFilesCapability{root}`,
  `make contract-sdk`): an executor-declared session files root; the
  harness derives `<root>/<session_id>/{uploads,outputs}`.
- **All languages:** session context over the machine token —
  Go `Client.SessionContext(ctx, processID)` / `SessionContextByID`,
  TypeScript `sessionContext` / `sessionContextById`, Python
  `session_context` / `session_context_by_id` (cached per session;
  not-found and machine-token-required errors). Recommended refusal reasons
  with the same names in every language (Go `Reason*`, TypeScript and Python
  `ErrorReason`) and constructors for a JSON-RPC error with `data.reason`
  (Go `NewReasonError`, TypeScript `RpcError.withReason`, Python
  `RPCError.with_reason`); `MetadataAgentSessionID` /
  `MetadataAgentSessionId` / `METADATA_AGENT_SESSION_ID`.
- **Go:** `ProcessOptions.Launch` — a per-process start hook that may set
  the directory, wrap argv (sandbox-exec, bwrap), change the environment or
  refuse with a reason; `SessionFileTransferer` gives file transfers the
  session; `ProcessOptions.SessionFiles` / `NewSessionFiles` — the
  per-session session files reference implementation (bmad's `os.Root` and
  no-follow guards, concurrent sessions) that advertises
  `session_files.root`. Example `examples/sandboxed-folder`.
- **Go:** `Launch` also runs for a package install (`process.start`
  `package_install`): `LaunchRequest.PackageInstall` is set and `Argv` /
  `Cwd` / `Env` are the installer's fixed command, which the hook may wrap,
  re-environ, move or refuse with a `data.reason`, so an executor cannot be
  bypassed through `install_packages`. An empty `LaunchSpec` keeps the
  installer's command; without `Launch` nothing changes. TypeScript and
  Python see `package_install` in their own `process.start` handler
  (examples `typescript/examples/session-context`,
  `python/examples/session_context.py`).
- **Examples (Go `sandboxed-folder`, TypeScript `session-context`, Python
  `session_context.py`):** a process without `agent_session_id` is refused with
  `session_rejected` (no fallback to a default folder) and a package install
  with `local_confirmation_denied`. A platform-initiated process (`initiator:
  "platform"`, no `agent_session_id`, for example the machine desktop agent)
  is refused too unless `-platform-dir` / `EXAMPLE_PLATFORM_DIR` is set, in
  which case it runs in that directory, never in a session folder.
- **Contract:** snapshot refreshed from BMA #470: `process.start` metadata says
  who started the process (`agent_session_id` + `tool_call_id` for agent
  tools, `initiator: "platform"` for the platform's own, human terminals are
  `terminal.attach`); `session_files.root` must be clean. Relative
  `write_file` / `edit_file` paths reach only executors that declared
  `session_files`.
- **Docs:** [session-context](docs/executor/session-context.md) states that
  the session `metadata` is the first read's (cached; a mid-session change
  is not guaranteed to reach the executor and must not be used to move a
  session or change its owner, as a Claude Managed Agents worker reads
  metadata once when it claims work) and which processes carry
  `agent_session_id` (every agent tool process; a platform-initiated process
  carries `initiator: "platform"` instead, and a person's console terminal is
  `terminal.attach`, not `process.start`).
- **Go:** the module needs Go 1.25 (`os.Root.MkdirAll` / `Rename`).
- TypeScript and Python have no process runtime and get no Launch hook: the
  executor's `process.start` handler is the equivalent (example
  `typescript/examples/session-context`).
- Platform dependencies (beeos-managed-agents #411): the session context
  routes (F1), `session_files` handling (F2) and `data.reason` propagation
  (F3). Without them the calls answer 404 and the declaration is ignored.

## 1.0.5

### Fixed

- **Python:** `wait_ready(timeout)` raised `asyncio.TimeoutError`, which is not
  the builtin `TimeoutError` before Python 3.11; it now raises `TimeoutError`
  on every supported version. This is the only change from 1.0.4.


## 1.0.4

The `v1.0.4` tag exists (Go module) but npm and PyPI were never published at 1.0.4: the release CI
failed on Python 3.10 (see 1.0.5). Use 1.0.5 in every language.

### Fixed

- **All languages:** a machine-scoped executor that advertises neither the
  `process` nor the `agent` capability is refused by the platform: it answers
  `executor.register`, then closes with `4002` "runtime registration
  refused". 1.0.3 reconnected forever on that and reported `ready` between
  the register answer and the close. After 3 registrations in a row refused
  right after `executor.register` (close `4002` "runtime registration
  refused" before the connection stayed registered for one heartbeat
  interval, or an `executor.register` error `capability_unavailable` /
  `invalid_params`) the client now ends with a configuration error: Go `Done`
  closes with `Err()` wrapping `ErrRegistrationRefused`, TypeScript
  `waitClosed()` rejects with `RegistrationRefusedError`, Python
  `wait_closed()` raises `RegistrationRefusedError`. Other `4002` closes
  (`daemon runtime ended`) and every other close still reconnect, and a
  refusal of a connection that had been stable does not count.

### Added

- **All languages:** `WaitReady(ctx)` / `waitReady(timeoutMs?)` /
  `wait_ready(timeout=None)` return once the executor is registered and the
  registration stayed open for one heartbeat interval, or with the error of a
  client that ended. `State()` still turns `ready` when `executor.register`
  returns; use the wait to tell a usable executor from a registration that
  is about to be refused.

### Documentation

- READMEs and `docs/executor`: a machine-scoped executor must declare
  `process` (or `agent`); minimal examples for Go, TypeScript and Python.

## 1.0.3

### Fixed

- **All languages:** `MaxReconnectAttempts` / `max_reconnect_attempts` /
  `maxReconnectAttempts` = N allows exactly N reconnects. When a connection
  was lost while `executor.register` was still in flight, the registration
  that returned afterwards reset the reconnect counter and so erased the
  attempt the loss had already counted (N+1 attempts in practice, #10). Go
  now resets the budget only when the connection that registered is still
  the current one.
- **TypeScript:** the token request is bounded by `requestTimeoutMs` (default
  30s, `AbortSignal.timeout`), as Go and Python bound it by their request
  timeout. `fetch` has no timeout of its own, so a token endpoint that
  accepted the connection and never answered stalled the reconnect forever.
- **Python:** a call whose channel ends while the request is still being
  written (for example the harness closing with 4002) no longer leaves a
  failed Future that nobody retrieves; asyncio logged "Future exception was
  never retrieved" for it.

## 1.0.2

### Fixed

- **All languages:** an executor no longer gives up reconnecting during an
  outage. 1.0.1 stopped after 10 failed reconnects (about 2 minutes of `503`
  from a restarting deployment); Go then closed `Done` with
  `ErrReconnectExhausted`, while Python and TypeScript only set the state to
  `closed` with no completion signal, so the process kept running offline.
  By default the client now reconnects until stopped, backing off from 1s
  doubling to 60s with jitter (gRPC connection backoff; GitHub Actions runner
  and Buildkite agent also retry until reconnected). `MaxReconnectAttempts` /
  `max_reconnect_attempts` / `maxReconnectAttempts` = 0 (the new default) or
  negative means unlimited; a positive value still bounds the attempts.
- **All languages:** a heartbeat with no answer within the request timeout
  closes the connection and reconnects, as gRPC keepalive closes a transport
  whose ping times out. 1.0.1 only logged the failure, so after a network cut
  the Go and TypeScript clients stayed `ready` on a dead (half-open)
  connection until the kernel gave up on TCP (about 15 minutes); Python
  relied on the `websockets` keepalive ping alone.
- **Go:** each reconnect attempt drops the HTTP client's idle connections
  and bounds the token request by `RequestTimeout` (as Python does). After a
  network cut the pooled HTTP/2 connection was dead; with
  `http.DefaultClient` (no timeout) the reconnect hung on it, and a timeout
  alone left it pooled, so every later attempt failed although the network
  was back.
- **All languages:** a `401` or `403` from the token endpoint on reconnect
  (revoked or invalid environment key or machine token) is not retried; the
  client ends with `ErrCredentialRejected` (Go) / `CredentialRejectedError`
  (Python, TypeScript).
- **Python, TypeScript:** `wait_closed()` / `waitClosed()` report the end of
  the client, as Go `Done`/`Err`: they return after `stop()` or
  `executor.close` and raise / reject with `ChannelClosed` /
  `ChannelClosedError` (4003, 4004), the credential error, or
  `ReconnectExhaustedError` (wrapping the last failure, as Go's
  `ErrReconnectExhausted` now does), so an executor can exit and be
  restarted by its supervisor instead of hanging.

## 1.0.1

### Fixed

- **All languages:** a machine-scoped executor (machine daemon token, empty
  environment id) exchanges its token at bma-api
  `{BaseURL}/api/v1/executors/token` (protocol reference §2.1). 1.0.0 posted
  it to the managed-agents `/v1/executors/token`, which answers a machine
  token `404`, so the machine path could not connect without a custom
  `TokenSource`. An environment key keeps `/v1/executors/token`.

## 1.0.0

- **Breaking (ADR 0012 §4.3), all languages:** protocol methods are typed by
  the contract: TypeScript `onRequest` / `call` / `notify` and harness `call`
  use `MethodParamsMap` / `MethodResultMap`; Python `on_request` / `call` /
  `notify` take descriptors from `bma_executor_sdk.methods`; Go
  `Requests.<M>.Handle`, `Calls.<M>.Call`, `Notifications.<M>.Notify`
  (generated by `tools/gen_methods.py`) replace `Client.OnRequest` /
  `OnNotification` / `Call` / `Notify`. `onNotification` / `on_notification`
  are removed (no harness-to-executor notification is left to handle).
- **Breaking, all languages:** an explicit `enabled: false` for `shell` /
  `files` wins over registered handlers; a handler registered after start
  sends `executor.capabilities.update` (as MCP `registerTool` sends
  `tools/list_changed`); a `tool.call` for an unregistered tool is refused
  with `-32012` before the ack (as Go Core); input missing a required member
  fails before the handler; bash output is truncated like Core
  (`BashResult.truncated` added); the `computer_*` slots are hidden and
  `computer_use` is off until the contract defines their input.
- `AgentCapability.turn_resume` from the BMA contract (`make contract-sdk`).
- All languages: the base URL is the deployment origin, the same value as
  `BMA_BASE_URL` in bma-sdk 1.0 (like `ANTHROPIC_BASE_URL`); a trailing
  `/api/v1` is dropped instead of producing `/api/v1/v1/executors/token`.

### Known limitations and roadmap

- Go harness: `Executor.Call(ctx, method string, params, out any)` stays the
  low-level, string-named call for harness-to-executor requests in 1.0
  (TypeScript and Python harness calls are typed). Typed Go harness wrappers
  will be added in 1.1 alongside it, without removing `Executor.Call`.

- **Breaking (ADR 0012), TypeScript:** typed tool handlers
  `client.tools.<name>.handle((input: <Tool>Input, ctx) => output)` replace
  `handleTool` / `onToolCall`; outputs are a string, content blocks,
  `{ content, isError, artifacts }`, or for bash `{ stdout, stderr, exitCode }`
  rendered as Go Core does; `ToolError` reports `is_error: true`. The `shell`,
  `files` and `computer_use` capabilities follow the registered handlers.
  Harness: `session.tools.<name>.call(input, options)` and
  `callTool(tool, input, { toolCallId, deadlineMs, permission, signal })`
  (fixes the `callTool` params type, which no longer compiled against the
  per-tool `ToolCallParams` union). Upgrade guide: [MIGRATION.md](MIGRATION.md).
- **Breaking (ADR 0012), Python:** `@client.tools.<name>.handler` with the
  contract's `<Tool>Input` TypedDicts replaces `on_tool_call`; handlers return
  `str`, content blocks, `ToolResult` or (bash) `BashResult`; `ToolError`
  reports `is_error`. Harness: `session.tools.<name>.call(input, ...)` and
  `call_tool(tool, input, ...)` replace `call_tool(params)`.
- **Breaking (ADR 0012), Go:** `Client.Tools.<Name>.Handle(func(ctx, <Name>Input, ToolContext) (ToolOutput, error))`
  replaces `OnToolCall` / `ToolCallHandler`; `BashResult.Output()` renders like
  `Core`, `NewToolError` reports `is_error`; capabilities follow the handlers.
  `Core.Attach` registers each tool it implements. Harness:
  `session.Tools.<Name>.Call(ctx, input, CallToolOptions{...})`.
- Conformance peers block on the bash command `block` instead of a tool named
  `block`, which is not a protocol tool name.
- TypeScript: `@types/ws` is now a runtime dependency (was a dev dependency).
  The published declarations import `ws` types (`webSocketConn(ws: WebSocket)`),
  so consumers type-checking with `strict` (`noImplicitAny`) failed with TS7016
  unless they installed `@types/ws` themselves.
- Docs: the BMA platform side of the agent capability is in preview
  (`PUT …/agents/{agentID}/executor-binding`, text input only); the READMEs
  require an explicit base URL while the hosted service is in early access.

## bma-executor-sdk v0.4.9

Repository migration; the Go import path changed. The code is identical to
bma-executor-sdk 0.4.8.

- The SDK moved from `beeos-ai/bma-sdks` (archived) to
  [`beeos-ai/bma-executor-sdk`](https://github.com/beeos-ai/bma-executor-sdk),
  with its git history.
- **Go import path:** `github.com/beeos-ai/bma-sdks/executor` →
  `github.com/beeos-ai/bma-executor-sdk` (package `bmaexecutor`, now at the
  repository root). Update the import and run
  `go get github.com/beeos-ai/bma-executor-sdk@v0.4.9`. The old module is
  marked deprecated.
- npm `@beeos-ai/bma-executor-sdk` and PyPI `bma-executor-sdk` keep their
  names. TypeScript is in `typescript/`, Python in `python/`.
- One tag `v0.4.9` now releases all three languages (was `executor/v*`,
  `executor-ts/v*`, `executor-python/v*`).

## bma-executor-sdk v0.4.8

Released 2026-11-01 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.8) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.8) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.8).

Go tag `executor/v0.4.8`, npm `@beeos-ai/bma-executor-sdk@0.4.8`, PyPI
`bma-executor-sdk` 0.4.8 (tag `executor-python/v0.4.8`).

### Features

- Contract (additive): `process.start package_install {manager: apt|pip|npm,
  packages[]}` and `capabilities.process.package_install`. Generated types in
  Go, TypeScript and Python.
- Go `Core`: `ProcessOptions.PackageInstaller` serves it. The core validates
  the request (manager enum; 1-32 plain package names matching
  `^[A-Za-z0-9][A-Za-z0-9.+-]{0,127}$`, so no option, version, path, URL or
  shell syntax; excludes argv, io_mode, file_transfer and git_credentials) and
  runs the installer's fixed command as a process with stdin closed: output,
  read, timeout and terminate work as for any process. Only a command the
  installer marks `Privileged` keeps the executor's identity; everything else
  runs as `CoreOptions.RunAs`. Without an installer the field is answered
  with `capability_unavailable` (`process.package_install`).
- **stdio MCP `secret_env`.** The core advertises `capabilities.mcp.secret_env`
  and accepts `session.assign.mcp_servers[].transport.secret_env`: values the
  server resolved from `secret:` references. They are added to that server's
  process environment only (not argv, not bash tools or processes, not logs)
  and live in memory until the session ends. A `secret_env` name must not be
  denied, a loader variable or also set in `env`, and `secret_env` is stdio
  only; violations fail `session.assign` with `invalid_params` naming the
  variable, never its value. A `secret:` reference left in `env` is still
  refused.
- Windows named pipe (`npipe:////./pipe/<name>`) transport in Go, TypeScript
  and Python, tested on a Windows runner.

### Fixes (Go `Core`)

- A stdio MCP server's bare `command` (for example `npx`) is now resolved on
  the `PATH` the server is started with (`transport.env`, else the executor's
  base env), not the executor's own `PATH`, which `exec.LookPath` used. A
  relative path with a separator is refused; an absolute path is unchanged.

## bma-executor-sdk v0.4.7

Released 2026-11-01 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.7) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.7) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.7).

Go tag `executor/v0.4.7`, npm `@beeos-ai/bma-executor-sdk@0.4.7`, PyPI
`bma-executor-sdk` 0.4.7 (tag `executor-python/v0.4.7`).

### Fixes (Go `Core`)

- The stdio MCP supervisor no longer gives up after 5 consecutive failures.
  It retries until the session ends, with exponential backoff from 200ms
  capped at 30s and jitter, and resets the backoff once a channel stays open
  for 1s. A channel the harness rejected for a while (for example a `404`
  from a replica during a rollout) now recovers within the session.
  `mcp_unavailable: <key>` is still reported once per outage after 5
  failures, and each failure is logged with its cause. TypeScript and Python
  have no MCP supervisor and are unchanged apart from the version.

## bma-executor-sdk v0.4.6

Released 2026-11-01 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.6) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.6) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.6).

Go tag `executor/v0.4.6`, npm `@beeos-ai/bma-executor-sdk@0.4.6`, PyPI
`bma-executor-sdk` 0.4.6 (tag `executor-python/v0.4.6`).

### Fixes (Go, TypeScript, Python clients)

- Only the contract's stop codes end the client: `4003` (unsupported
  protocol) and `4004` (replaced). `4002` (machine runtime refused or ended),
  `1008` and, in TypeScript, `4005` used to stop it too, so an executor
  exited when a restarting harness closed its connection with `4002`. They
  now reconnect with backoff like any other close.

## bma-executor-sdk v0.4.5

Released 2026-11-01 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.5) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.5) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.5).

Go tag `executor/v0.4.5`, npm `@beeos-ai/bma-executor-sdk@0.4.5`, PyPI
`bma-executor-sdk` 0.4.5 (tag `executor-python/v0.4.5`).

### Features (Go, TypeScript, Python clients)

- Contract: new executor → harness request `executor.token.refresh{token}` →
  `{expires_at}` (additive, `bma.executor.v1.1`).
- The client no longer drops its control connection every token lifetime. At
  70% of the token's `expires_in` it fetches a new token with its original
  credential and sends `executor.token.refresh`; the harness moves the
  connection's 4001 deadline. A harness without the method answers -32601 and
  the client keeps the previous behaviour (reconnect on 4001). Tokens without
  `expires_in` (static tokens) are never refreshed.
- Reconnect backoff cap is configurable: Go `Options.ReconnectMaxDelay`,
  TypeScript `reconnectMaxDelayMs`, Python `reconnect_max_delay` (default 30 s).

## bma-executor-sdk v0.4.4

Released 2026-11-01 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.4) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.4) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.4).

Go tag `executor/v0.4.4`, npm `@beeos-ai/bma-executor-sdk@0.4.4`, PyPI
`bma-executor-sdk` 0.4.4 (tag `executor-python/v0.4.4`).

### Features (Go `Core`)

- `CoreOptions.RunAs` (Unix): a privileged executor runs every bash tool,
  `process.start` child (pipe and PTY) and stdio MCP server as a separate
  workload uid/gid with its supplementary groups; bash and process `HOME`/`USER`
  and stdio MCP `USER`/`LOGNAME` are the workload's, and a session workspace the core creates is owned by it.
  In-process file tools (read/write/edit/glob/grep) are refused while `RunAs`
  is set, because they would run as the executor. bmad uses this so machine
  workloads cannot read the executor's credentials.

The TypeScript and Python packages start no processes; they are released at
0.4.4 to keep the family in lockstep.

## bma-executor-sdk v0.4.3

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.3) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.3) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.3).

Go tag `executor/v0.4.3`, npm `@beeos-ai/bma-executor-sdk@0.4.3`, PyPI
`bma-executor-sdk` 0.4.3 (tag `executor-python/v0.4.3`).

### Fixes (Go `Core`)

- **stdio MCP servers get the MCP SDKs' safe default environment.** 0.4.2
  started servers with `transport.env` only, so `npx`/`uvx`-style servers had
  no `PATH`. The server now inherits `PATH`, `SHELL`, `TERM`, `USER`,
  `LOGNAME` (the official MCP SDK `getDefaultEnvironment` list, `DenyEnv`
  filtered; the Windows list likewise) and `transport.env` overlays them.
  `HOME` stays the host's and is not inherited, nor is any `BMA_*` or other
  host variable; `secret:` refusal is unchanged.

The TypeScript and Python packages start no processes; they are released at
0.4.3 to keep the family in lockstep.

## bma-executor-sdk v0.4.2

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.2) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.2) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.2).

Go tag `executor/v0.4.2`, npm `@beeos-ai/bma-executor-sdk@0.4.2`, PyPI
`bma-executor-sdk` 0.4.2 (tag `executor-python/v0.4.2`).

### Security fixes (Go `Core`)

- **stdio MCP servers get only their configured env.** An
  environment-origin stdio MCP server now starts with exactly
  `transport.env` (protocol v1 §8.1); 0.4.0–0.4.1 passed the executor's whole
  environment minus `DenyEnv`, so host variables such as `HOME`, `BMA_HOME`
  or unrelated credentials reached the server. Bash tools, processes and
  terminals are unchanged (base env + `CoreOptions.Env` /
  `ProcessOptions.Env`, `DenyEnv` filtered).
- **`secret:` env references are refused.** `session.assign` fails with
  `-32602 invalid_params` naming the server and variable (never the value)
  when an environment-origin server's env holds a `secret:` reference, which
  the executor cannot resolve, instead of handing the literal reference to
  the server. A loader variable (`LD_PRELOAD`, …) or a `DenyEnv` name in that
  env is refused the same way, at assign time rather than on every restart.

The TypeScript and Python packages start no processes; they are released at
0.4.2 to keep the family in lockstep.

## bma-executor-sdk v0.4.1

Released 2026-10-31 (UTC). GitHub Releases: [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.1) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.1) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.1).

Go tag `executor/v0.4.1`, npm `@beeos-ai/bma-executor-sdk@0.4.1`, PyPI
`bma-executor-sdk` 0.4.1 (tag `executor-python/v0.4.1`).

### Fixes

- **Contract compatibility: unregistered capability methods answer
  `capability_unavailable`.** A harness → executor contract method behind a
  capability the executor did not register (`terminal.*`, `skill.install`,
  `workspace.backup`, the agent methods, …) now answers `-32012
  capability_unavailable` with `data.capability` set to the contract's
  `x-bma-capability` (for example `terminal.enabled`), as the contract
  requires and pre-SDK executors did. 0.4.0 answered `-32601
  method_not_found`. Methods outside the contract still answer
  `method_not_found`. Go, TypeScript and Python executors behave the same.

## v0.4.0

| Family | Released | Tags | GitHub Releases |
| --- | --- | --- | --- |
| bma-executor-sdk 0.4.0 | 2026-10-31 (UTC) | `executor/v0.4.0`, `executor-ts/v0.4.0`, `executor-python/v0.4.0` | [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/executor%2Fv0.4.0) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-ts%2Fv0.4.0) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/executor-python%2Fv0.4.0) |
| bma-sdk 0.4.0 | 2026-10-31 (UTC) | `go/v0.4.0`, `ts/v0.4.0`, `python/v0.4.0` | [Go](https://github.com/beeos-ai/bma-sdks/releases/tag/go%2Fv0.4.0) · [TypeScript](https://github.com/beeos-ai/bma-sdks/releases/tag/ts%2Fv0.4.0) · [Python](https://github.com/beeos-ai/bma-sdks/releases/tag/python%2Fv0.4.0) |

bma-executor-sdk 0.4.0 was released first (Go tag `executor/v0.4.0`, npm
`@beeos-ai/bma-executor-sdk@0.4.0`, PyPI `bma-executor-sdk` via
`executor-python/v0.4.0`). bma-sdk 0.4.0 (`go/v0.4.0`, `ts/v0.4.0`,
`python/v0.4.0`) follows with the REST snapshot regenerated from BMA main
(beeos-ai/beeos-managed-agents@ceee7dcf).

Two SDK families now ship from this repository, generated from the same BMA
contract and versioned independently:

| Family | Go | npm | PyPI |
| --- | --- | --- | --- |
| bma-sdk (REST + façade extensions) | `github.com/beeos-ai/bma-sdks/go` (tag `go/v0.4.0`) | `@beeos-ai/bma-sdk@0.4.0` (tag `ts/v0.4.0`) | `bma-sdk` 0.4.0 (tag `python/v0.4.0`) |
| bma-executor-sdk (executor protocol) | `github.com/beeos-ai/bma-sdks/executor` (tag `executor/v0.4.0`) | `@beeos-ai/bma-executor-sdk@0.4.0` (tag `executor-ts/v0.4.0`) | `bma-executor-sdk` 0.4.0 (tag `executor-python/v0.4.0`) |

Executor contract: `bma.executor.v1.1` (contract 1.1.0), snapshot and types
generated by BMA `make contract-sdk`; `contracts/bma/executor/lock.json`
records the BMA commit (beeos-ai/beeos-managed-agents@69c05b65, #278).

### Breaking changes

- **Package split.** Executor code moved out of bma-sdk:
  - Go: `github.com/beeos-ai/bma-sdks/go/executor` → `github.com/beeos-ai/bma-sdks/executor`
    (its own module; `go get github.com/beeos-ai/bma-sdks/executor@v0.4.0`).
  - TypeScript: `@beeos-ai/bma-sdk/executor` → `@beeos-ai/bma-executor-sdk`;
    `@beeos-ai/bma-sdk` no longer depends on `ws`.
  - Python: `bma_sdk.executor` / `bma-sdk[executor]` → `bma_executor_sdk` /
    `bma-executor-sdk`.
- **Go generated types use value fields.** Optional scalars, strings and
  structs are values with `omitempty` instead of pointers
  (e.g. `ContentBlock.Text string`, `JSONRPCErrorData.Retryable bool`);
  drop `&x` / `*x` at call sites.
- **Go error codes are `Code*` untyped constants** (`CodeInvalidParams`,
  `CodeExecutorBusy`, `CodeConflict`, …) on `RPCError.Code int`; the
  `JSONRPCErrorCode*` names are gone.
- **Go protocol constant `ProtocolV11`** (`"bma.executor.v1.1"`) replaces
  `ProtocolVersionV11`. Go clients negotiate v1.1 by default
  (`Options.Protocol`); a harness still accepts v1.
- **Go `ToolCallParams.Input` is `json.RawMessage`.** Decode the built-in
  shape with `params.DecodeInput()`; build it with
  `NewToolInput(ToolInput{...})`.
- **Go capabilities type is `Capabilities`** (was `ExecutorCapabilities`),
  including `Agent`, `Process`, `Terminal`, `Skills`, `Workspace`.
- **TypeScript JSON-RPC error codes use semantic names**
  (`JsonRpcErrorCode.ExecutorBusy`, `.CapabilityUnavailable`, …) generated
  from the contract's `x-bma-enum-names`; numeric `Value32009`-style names are
  gone. Python `JsonRpcErrorCode` uses the same names.
- `session.event_delta.index` is required on the wire (an index of 0 was
  previously dropped).

### New

- **Harness end** in Go, TypeScript and Python: accept executors, assign
  sessions, call/cancel tools, MCP channels (`Harness`).
- **Agent capability** (`bma.executor.v1.1`, capability `agent`): executor
  `ServeAgent`/`serve_agent` with turns, inputs, cancel, durable ordered
  session events with resend after reconnect, transient deltas,
  interactions; harness-side `AgentPlatform`. Machine-scoped executors
  (machine token via `TokenSource`, no environment id).
- **Go execution `Core`** (bmad runs on it): bash, file tools
  (read/write/edit/glob/grep), stdio MCP supervisor, cleanup, multi-harness
  isolation, registrable capabilities (`process.*` with PTY / file transfer /
  Git credential hooks, `skill.install`, custom). `DenyEnv` keeps
  executor-private variables (e.g. machine tokens) out of bash, processes,
  MCP servers and terminals.
- **Local transports**: WebSocket over a Unix domain socket (`unix://`) or
  Windows named pipe (`npipe://`, TypeScript), executor-listens local socket,
  in-process pipe; MCP channels on every transport.
- Cross-language conformance and interop suites (Go ↔ TypeScript,
  Go ↔ Python, TypeScript ↔ Python over `ws://` and `unix://`).

### Behavior notes for bmad / BMA

- `tool.cancel` results follow the SDK shape.
- `BMA_HOME` stays visible to bash and `process.*` (skills paths depend on
  it); `BMA_MACHINE_TOKEN` and other internal `BMA_*` variables are hidden
  from bash, processes, stdio MCP and PTY terminals.
