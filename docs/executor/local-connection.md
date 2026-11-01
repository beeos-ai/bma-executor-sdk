# Local connection and authentication

[中文](local-connection.zh-CN.md) · [Overview](README.md)

A local brain and a local hand exchange the same Executor Protocol messages as the cloud; only transport and authentication differ. Everything on this page ships in bma-executor-sdk 0.4.0 unless marked otherwise.

| Transport | Who listens | Go | TypeScript | Python |
| --- | --- | --- | --- | --- |
| WebSocket over loopback TCP | brain (`Harness`) | `http.Serve(ln, harness)` + `StaticToken("ws://…", token)` | `harness.listen()` | `await harness.serve(host, port)` |
| WebSocket over a Unix domain socket (`unix://`) | brain (`Harness`) | serve `Harness` on a `unix` listener + `StaticToken("unix:///path.sock", token)` | `harness.listenLocal(path)` | `await harness.serve_unix(path)` |
| WebSocket over a Unix domain socket | hand (executor) | `ListenUnixSocket(path, token)` + `Harness.DialExecutor(ctx, path, token)` | `listenExecutor({ path, token })` + `harness.dialExecutor(url, { token })` | not provided |
| Windows named pipe (`npipe:////./pipe/<name>`), brain listens | brain (`Harness`) | serve `Harness` on `NamedPipeListener(name)` + `StaticToken("npipe:////./pipe/<name>", token)` | `harness.listenLocal("\\\\.\\pipe\\<name>")` | `harness.serve_npipe(name)` |
| Windows named pipe, executor listens | hand (executor) | `ListenNamedPipe(name, token)` + `Harness.DialExecutor(ctx, "npipe:////./pipe/<name>", token)` | as the executor-listens row above, with a pipe path | not provided |
| In process | — | `InProcessDialer(harness, principal)` | `harness.inProcessDialer()` | `harness.inprocess_dialer()` |

---

## 1. The brain listens (executor dials)

### 1.1 Topology

```text
local brain process                         executor process (desktop app, or bmad later)
┌──────────────────────┐  ws://127.0.0.1:<port>  ┌──────────────────────────────┐
│ Harness (http.Handler)│  or unix:///path.sock   │ Client "local" ─┐             │
│  WaitExecutor/Assign/ │◀────────────────────────│                 ├─ Core
│  CallTool/MCP         │  bma.executor.v1.1 / v1 │ Client "cloud" ─┘             │
└──────────────────────┘                         └──────────┬───────────────────┘
                                                            │ wss://…/v1/executors/channel
                                                            ▼
                                                        BMA platform
```

- The brain serves `Harness` on a **loopback** listener (`127.0.0.1` / `::1`) or a Unix socket, never on a network interface.
- The executor attaches one `Client` per harness to the same `Core`: `core.Attach("cloud", cloudClient)`, `core.Attach("local", localClient)`. It is the WebSocket client on both.
- Local calls never round-trip through the cloud.

```go
// local brain
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

// executor
local, _ := bmaexecutor.NewClient(bmaexecutor.Options{
	EnvironmentID: "local", Executor: info, Capabilities: core.Capabilities(),
	TokenSource: bmaexecutor.StaticToken("ws://"+ln.Addr().String(), localToken),
})
_ = core.Attach("local", local)
```

The runnable version is [`executor/examples/local-brain`](../../executor/examples/local-brain/main.go) (`cd executor && go run ./examples/local-brain`). For a Unix socket, serve the same `Harness` on `net.Listen("unix", path)` and use `StaticToken("unix://"+path, localToken)`; the default dialer (like `UnixSocketDialer(path)`) speaks WebSocket over the socket. In Python, [`harness_loopback.py --unix PATH`](../../executor/python/examples/harness_loopback.py) does the same.

### 1.2 Authentication

| Item | Rule |
| --- | --- |
| Local token | random per brain start (≥32 bytes), handed to the executor through a channel only the current user can read (0600 file, inherited fd, or an in-process variable); never logged |
| Verification | `HarnessOptions.Authenticate` checks every upgrade (control and MCP). `Authenticate: nil` means no auth and is **only** acceptable on a loopback listener reachable by this user alone; the SDK does not check the listen address for you |
| Pinned identity | `Principal.ExecutorID` pins the executor: `resume.executor_id` must match, and only that executor may open MCP channels for its sessions |
| Separation | the local token is unrelated to platform credentials (machine token / environment key); platform credentials are never given to the brain |

### 1.3 Several harnesses on one hand (Go `Core`)

| Rule | Behavior |
| --- | --- |
| Sessions | keyed by `(brain, session_id)`; two harnesses may reuse a `session_id` |
| Calls | `tool.call` reaches only sessions its own harness assigned; otherwise a failed result `session_not_active` |
| Workspaces | per session `WorkRoot/<brain>/<session>` (override with `CoreOptions.Workspace`); file tools confined to it |
| MCP | channels and stdio servers per (brain, session, server_key) |
| Capacity | `MaxSessions` (default 16) shared; `MaxSessionsPerBrain` (default `MaxSessions`) so the cloud cannot starve the local brain; full → `session.assign` `-32009` (`retryable:true`) |
| Cancel/cleanup | `tool.cancel`, deadlines, `timeout_ms`, `session.stop` kill only that session's process groups; a harness disconnect affects only its sessions |
| Trust | bash is not a sandbox: every harness runs as the executor's OS user; attach only brains you trust with this machine |

### 1.4 One executor per computer still holds

Several harnesses share the hand **locally**; the platform still sees one outbound connection and one `exec_<machine_id>`. Do not start a second platform executor for the local brain (they would replace each other with 4004, see [protocol reference §2.4](protocol-reference.md#24-websocket-close-codes)).

---

## 2. The hand listens (harness dials)

Lets a local brain attach to a hand that is already running (option A) without a TCP port, protected by file-system permissions.

| Item | Behavior |
| --- | --- |
| Direction | the executor listens, the brain connects; roles stay harness/executor (the executor still sends `executor.register` first) |
| Location | a per-user runtime directory; the SDK creates the directory `0700` and the socket `0600`. Windows: a named pipe `\\.\pipe\<name>` (URL `npipe:////./pipe/<name>`) that refuses remote clients; Go restricts its ACL to the current user, TypeScript and Python use the platform's default pipe ACL, so the local token is the authentication |
| Local token | generated per start (TypeScript `createLocalToken` writes a `0600` file, `readLocalToken` reads it); required as `Authorization: Bearer` on every connect |
| Network | no listener on any network interface |
| MCP | the dialing harness also dials the session MCP channels, again after a server restart |
| Several harnesses | as §1.3 |

```go
// executor (hand)
dial, closeListener, _ := bmaexecutor.ListenUnixSocket(sockPath, localToken)
defer closeListener()
client, _ := bmaexecutor.NewClient(bmaexecutor.Options{
	Executor: info, Capabilities: core.Capabilities(),
	TokenSource: bmaexecutor.StaticToken("ws://local/", ""), Dial: dial,
})
_ = core.Attach("local", client)

// local brain
harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{})
go harness.DialExecutor(ctx, sockPath, localToken) // serves the connection until it closes
executor, _ := harness.WaitExecutor(ctx)
```

TypeScript: [`executor/typescript/examples/harness-loopback`](../../executor/typescript/examples/harness-loopback/main.mjs) (`listenExecutor` + `dialExecutor`, Unix socket or named pipe).

Windows named pipe (Go): the same calls with `ListenNamedPipe(name, token)` instead of `ListenUnixSocket`, and `harness.DialExecutor(ctx, "npipe:////./pipe/"+name, localToken)` (a `\\.\pipe\<name>` path works too). A brain serves `Harness` on `NamedPipeListener(name)` and the executor dials `npipe:////./pipe/<name>` with the default dialer or `NamedPipeDialer(name)`. On other platforms these return `ErrNamedPipeUnsupported`. Python needs the Windows `ProactorEventLoop` (the asyncio default on Windows): `harness.serve_npipe(name)` on the brain, `static_token("npipe:////./pipe/<name>", token)` on the executor.

The named pipe transport of all three SDKs is tested on a Windows runner in CI (job `windows-named-pipe`): Go loopback in both directions, TypeScript local-socket tests, and Go ↔ TypeScript ↔ Python interop over a pipe.

bmad does not listen on a local socket yet [Coming]. Once it does, a brain attaches with the same `DialExecutor` call; when the brain registers as an agent provider, bmad advertises `capabilities.agent` on its platform connection and forwards `turn.*`/events.

---

## 3. In process

When brain and hand share a process (typical for option B), connect them with the in-process transport: `InProcessDialer(harness, principal)` in Go (the harness side is `Harness.ServeConn`), `harness.inProcessDialer()` in TypeScript, `harness.inprocess_dialer()` in Python. Semantics are identical to the remote case, so the same tests apply. Examples: [`agent-minimal`](../../executor/typescript/examples/agent-minimal/main.mjs), [`harness_loopback.py`](../../executor/python/examples/harness_loopback.py).
