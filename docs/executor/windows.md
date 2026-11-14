# The Go executor on Windows

What the Go execution core (`NewCore`, `NewProcessCapability`, `NewSessionFiles`)
does on Windows, and how to run it on a machine. The installable build is
`examples/windows-hand` with `packaging/windows` (CI artifact `bma-windows-hand`).

## Behavior

| Area | Windows behavior |
|---|---|
| Environment | Bash tools and processes inherit `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR` plus `SystemRoot`, `SystemDrive`, `windir`, `ComSpec`, `PATHEXT`, `TEMP`/`TMP`, `USERNAME`, `USERDOMAIN`, `USERPROFILE`, `HOMEDRIVE`, `HOMEPATH`, `APPDATA`, `LOCALAPPDATA`, `ProgramData`, `ALLUSERSPROFILE`, `PUBLIC`, `ProgramFiles*`, `CommonProgramFiles*`, `PROCESSOR_ARCHITECTURE`, `NUMBER_OF_PROCESSORS`, `OS`, `COMPUTERNAME`, `PSModulePath`. Names merge case-insensitively (`Path` overrides `PATH`). `DenyEnv` still applies; stdio MCP servers keep the narrower MCP default list. |
| Command lines | `cmd.exe /d /s /c <command>` (the bash tool, and process.start argv `cmd.exe … /c <command>`) is started with the command line verbatim (`SysProcAttr.CmdLine`, `/s` added when missing), so `"` and `$` reach PowerShell unchanged. Every other program keeps Go's argv quoting. |
| Process trees | Every command, process and MCP server runs in a Job Object (`KILL_ON_JOB_CLOSE`): created suspended, assigned, resumed. Cancel, timeout, `process.terminate` and session end end the whole tree, including children left behind by a process that already exited. |
| Interrupt | `process.interrupt` sends console `CTRL_BREAK` to the command's process group (the executor creates a hidden console if it has none). `process.terminate` asks the same way, then kills the tree after `grace_ms` (at once when it cannot be delivered). `exit_signal` is `SIGINT`, `SIGTERM` or `SIGKILL`. |
| PTY | `io_mode: pty` uses ConPTY (Windows 10 1809+; `process.pty` is advertised only where it exists). Input is the terminal's input, interrupt writes `^C`, output is one merged stream. The terminal is 24×80. The core registers no `terminal.*` handlers; a host builds terminal sessions on `io_mode: pty`. |
| cwd | A relative cwd is under the session workspace. An absolute cwd equal to the work root (a machine's configured cwd, for example `C:\bma\work`) means the session workspace. Other absolute directories must be inside the workspace. Paths compare as real paths (symlinks, 8.3 names, case). |
| Session files | `<Root>\<agent session id>\uploads` (mounts, read-only attribute) and `outputs`. A changed mount replaces its read-only predecessor, an unmounted one is removed. The read-only attribute is a convention for the same user, not an ACL. Mount paths that Windows cannot create (`:`, reserved names) fail the process start with the OS error. |
| File transfer | `NewFileTransferer()` serves `process.start file_transfer` on a machine-scoped executor (`/api/v1/daemon/processes/{id}/file`). Download replaces the destination atomically and keeps its read-only attribute; `~` is `USERPROFILE`. `edit_file` downloads are not supported. |
| Computer use | `CoreOptions.ComputerUse` (`NewCuaDriver`) serves `computer_observe` / `computer_action` through Cua Driver's daemon; see below. Off unless a driver works. |

## Session model

The executor and the Cua daemon must run in the interactive session of the
auto-logged-on user (Session 1): a scheduled task at that user's logon, not a
service (a service lives in Session 0 and sees no desktop). Autologon is the
machine image's job (a per-machine random password, never shared in an image).

## Computer use (Cua Driver)

Only the MIT-licensed Cua Driver (pin v0.44.0, verify its checksum and
signature) is used, never the AGPL perception extension or the FSL space
daemon. Telemetry is off (`CUA_TELEMETRY=0`). The executor runs
`cua-driver call <tool> <json>`, which talks to the daemon over
`\\.\pipe\cua-driver`.

- `computer_observe` with no input returns a screenshot (image block) and the
  on-screen windows (`pid`, `window_id`, title, bounds). With `pid` and
  `window_id` it returns that window's screenshot and UIA tree.
- `computer_action`: `click` (`coordinate`, `button`, `click_count`), `type`
  (`text`), `key` (`text`: `Return`, `ctrl+s`), `scroll` (`coordinate`,
  `scroll_direction`, `scroll_amount`), `drag` (`start_coordinate`,
  `coordinate`). Without `pid`/`window_id` coordinates are screen pixels of the
  last full observation and input goes to the focused app; with them input is
  delivered to that window in the background and coordinates are window-local.
  A click, scroll or drag without `pid`/`window_id` right after a window
  observation is refused (`window_frame_required`).
  A local brain's `computer_action` also has `open_app` (`app`: name or
  bundle id), which runs the driver's `launch_app` (macOS drivers only) and
  `bring_to_front` and lists only windows on the current desktop, and `menu`
  (`menu_path` with `pid` and `window_id`: the driver's `invoke_menu`). Both
  tools take a `description` of the step for clients to show; a call without
  it still runs. `type` ends with `typed: verified`, `typed: mismatch` or
  `typed: unverified` (see the CHANGELOG, 1.6.9).
- Driver advice that names tools the model cannot call is dropped or
  rewritten into `computer_*` actions.
- Degradation: no driver or daemon at start leaves `computer_use` off.

## Install

See `packaging/windows/README.txt` (included in the zip). `install.ps1`
registers `BMA Executor` (and `BMA Cua Driver` when the driver is supplied)
at logon of `-User`, writes `executor.env` readable only by SYSTEM,
Administrators and that user, and never prints the token.

## Not covered

- A Session 0 service plus a session-1 executor over a named pipe (the UiPath
  shape): the logon-task form is the supported one.
- Rejecting input actions during human takeover: takeover state is held by the
  host or platform, not by this core.
- Video (use the machine's own display stream); TypeScript and Python
  executors on Windows are unchanged.
