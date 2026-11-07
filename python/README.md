# bma-executor-sdk (Python)

[![PyPI](https://img.shields.io/pypi/v/bma-executor-sdk.svg)](https://pypi.org/project/bma-executor-sdk/)

Connect your machine (hands) or your own agent (brain) to BMA — the executor, harness and agent ends of the `bma.executor` protocol.
[`bma-sdk`](https://github.com/beeos-ai/bma-sdk) drives and uses agents (REST / façade client); `bma-executor-sdk` connects hands or brains to BMA.

asyncio executor (`ExecutorClient`), harness (`Harness`) and agent ends of the BMA Executor Protocol `bma.executor.v1.1`, the counterpart of the [Go `executor` module](../README.md). Protocol types are TypedDicts generated into `bma_executor_sdk/generated.py` from the contract in [`contracts/bma/executor`](../contracts/bma/executor), so messages are plain JSON dicts.

| | |
| --- | --- |
| Version | 1.0.0 (tag `v1.0.0`), see [CHANGELOG](../CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (contract 1.1.0, [`lock.json`](../contracts/bma/executor/lock.json)) |
| Python | 3.10 or newer; depends on `httpx` and `websockets` |
| Integration guide | [docs/executor](../docs/executor/README.md) ([中文](../docs/executor/README.zh-CN.md)) |

```sh
pip install bma-executor-sdk==1.0.2
```

The REST / façade client is the separate package [`bma-sdk`](https://github.com/beeos-ai/bma-sdk/tree/main/python).

## Executor quick start

```python
import asyncio, os
from bma_executor_sdk import BashInput, BashResult, ExecutorClient, ToolContext

async def main() -> None:
    client = ExecutorClient(
        base_url=os.environ["BMA_BASE_URL"],  # required: see below
        environment_key=os.environ["BMA_ENVIRONMENT_KEY"],
        environment_id=os.environ["BMA_ENVIRONMENT_ID"],
        executor={"name": "my-executor", "os": "linux", "arch": "arm64", "version": "0.1.0"},
    )

    @client.tools.bash.handler  # one typed slot per built-in tool; only registered tools are advertised
    async def bash(input: BashInput, ctx: ToolContext) -> BashResult:
        proc = await asyncio.create_subprocess_shell(
            input["command"], stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE)
        out, err = await proc.communicate()
        return BashResult(stdout=out.decode(), stderr=err.decode(), exit_code=proc.returncode or 0)

    await client.start()
    await client.wait_closed()  # raises when the client ends for good

asyncio.run(main())
```

Set `base_url` to your BMA deployment: the hosted service is in early access and the default `DEFAULT_BASE_URL` (`https://api.bma.beeos.ai`) does not resolve yet. It is the deployment origin, the same value as `BMA_BASE_URL` in bma-sdk (for example `https://api.staging.bma.beeos.ai`); the client posts the environment key to `{base_url}/v1/environments/{environment_id}/token` (a machine daemon token to `{base_url}/v1/machines/{machine_id}/token`) and drops a trailing `/api/v1`. `machine_id` and `environment_id` are alternatives; an empty `machine_id` reads `BMA_MACHINE_ID` (`MACHINE_ID_ENV`), and without any id the legacy `/api/v1/executors/token` alias is used. The SDK does not read `BMA_BASE_URL` itself. A `token_source` carries its own channel URL.

### A machine-scoped executor must declare `process` or `agent`

With an empty `environment_id` (a machine token) the platform binds the connection as the machine's runtime and **refuses the registration of an executor that advertises neither the `process` nor the `agent` capability**: it answers `executor.register`, then closes with `4002` "runtime registration refused". Shell and files alone are such an executor. Declare `process` and serve `process.*` (or `serve_agent`), with `executor["version"]` as `MAJOR.MINOR.PATCH`:

```python
client = ExecutorClient(
    base_url=os.environ["BMA_BASE_URL"],
    environment_key=os.environ["BMA_MACHINE_TOKEN"],  # the machine token
    environment_id="",                                # machine scope
    executor={"name": "my-machine", "os": "linux", "arch": "arm64", "version": "1.0.0"},
    capabilities={"process": {"enabled": True}},
)
client.on_request(methods.PROCESS_START, handle_process_start)  # and process.read / write / interrupt / terminate / list
await client.start()
await client.wait_ready(30)  # registered and still open one heartbeat later
```

The SDK treats a refusal as a configuration error: after 3 registrations in a row refused right after `executor.register` (close `4002` "runtime registration refused", or an `executor.register` error `capability_unavailable` / `invalid_params`), `wait_closed()` raises `RegistrationRefusedError`; other `4002` closes (for example "daemon runtime ended") reconnect as before. `state` is Ready as soon as `executor.register` returns, which a refused registration also does; `wait_ready(timeout=None)` returns only after the registration stayed open for one heartbeat interval, and raises the error of a client that ended (`TimeoutError` after `timeout`).

A complete version that runs bash and stops the process on cancellation is [`examples/executor_minimal.py`](examples/executor_minimal.py); set `BMA_HARNESS_URL` (`ws://…` or `unix:///path.sock`) to connect it to a local harness instead of BMA.

Behavior matches the Go SDK:

- A handler returns a string, content blocks, `ToolResult(content, is_error, artifacts)` or (bash) `BashResult`, rendered like the Go `Core`; `raise ToolError(text)` reports a tool-level error (`is_error: true`), any other exception is `outcome: "failed"`. The harness calls tools as `await session.tools.bash.call({"command": "ls"})`.
- `tool.cancel` cancels the handler's task and reports `outcome: "canceled"`; duplicate `tool.call` ids run once.
- `tool.result` retries `-32009` / `-32004` / `retryable` errors and hands a lost result to `on_tool_result_lost`.
- Reconnect resumes with `resume.executor_id` / `active_sessions`, until `stop()` by default (backoff 1 s doubling to 60 s, jitter); only close codes 4003 and 4004 stop the client, and so does `RegistrationRefusedError` (3 refused registrations in a row). A positive `max_reconnect_attempts` bounds the attempts. `await client.wait_closed()` returns after `stop()` / `executor.close` and raises `ChannelClosed` (4003/4004), `RegistrationRefusedError`, `CredentialRejectedError` (the token endpoint answered 401/403 on reconnect: revoked or invalid credential, not retried) or `ReconnectExhaustedError`; let it end the process so a supervisor can restart it.
- Call `session_ready` after setup and `session_ended` after cleanup. `client.on_request(methods.PROCESS_START, handler)` serves `process.*`, `terminal.*`, `skill.install` or `workspace.backup` yourself with the contract's TypedDicts; `await client.call(methods.SESSION_EVENT, params)` / `await client.notify(methods.TOOL_PROGRESS, params)` send typed methods (`bma_executor_sdk.methods`). There is no execution `Core` in Python. In a `process.start` handler, `await client.session_context(params["process_id"])` reads the session's metadata and `RPCError.with_reason(code, ErrorReason.X, msg)` refuses with a `data.reason` ([session context](../docs/executor/session-context.md)).
- Input missing a required member fails the call before the handler runs; a `tool.call` for a tool without a handler is refused with `-32012` before the ack; `shell` / `files` are enabled while a handler of the group is registered unless set `enabled: False`, and a handler registered after `start()` sends `executor.capabilities.update`. `BashResult` beyond `INLINE_RESULT_BYTES` (or `truncated=True`) is cut to Go Core's preview. The `computer_*` tools get slots once the contract defines their input.

## Harness quick start

```python
from bma_executor_sdk import Harness

harness = Harness(authenticate=authenticate)  # authenticate(ConnectRequest) -> Principal; omit only in-process or on a private socket
server = await harness.serve("127.0.0.1", 0)   # or await harness.serve_unix(path)

executor = await harness.wait_executor()
session = await executor.assign_session({"session_id": "ses_1"})
await session.ready()
result = await session.call_tool({"tool": "bash", "input": {"command": "ls"}})
mcp = await session.mcp("echo")  # raw MCP JSON-RPC
await session.stop("done")
```

Semantics match the Go `Harness`: `call_tool` is idempotent by `tool_call_id`; sessions and pending calls survive a reconnect within the lease; lease expiry fails pending calls with `ToolOutcomeUnknownError`.

## Transports

Chosen by the channel URL of the token source:

| Transport | Executor | Harness |
| --- | --- | --- |
| WebSocket (`ws://`, `wss://`) | default | `await harness.serve(host, port)` |
| WebSocket over a Unix domain socket (`unix:///path.sock`) | `token_source=static_token("unix:///path.sock", token)` | `await harness.serve_unix(path)` |
| Windows named pipe (`npipe:////./pipe/<name>`, Windows ProactorEventLoop) | `token_source=static_token("npipe:////./pipe/<name>", token)` | `await harness.serve_npipe(name)` |
| In process | `dialer=harness.inprocess_dialer()` | — |

The executor-listens local socket is not implemented in Python (use the Go or TypeScript SDK); named pipes need the Windows `ProactorEventLoop` (the asyncio default there) and are tested on a Windows runner in CI. See [local connection](../docs/executor/local-connection.md).

## Agent executors

`client.serve_agent(handler, capability)` serves the `agent` capability; `handler` implements `open_turn`, `run_turn` and `close_session` (async; cancellation is task cancellation), and `run_turn` uses `turn.emit`, `await turn.delta(...)`, `turn.inputs` (an `asyncio.Queue`) and `turn.request_interaction`. The harness side is `Harness(agent=platform)` with an `AgentPlatform`. See [agent capability](../docs/executor/agent-capability.md).

## Go ↔ Python

| Go (`bmaexecutor`) | Python (`bma_executor_sdk`) |
| --- | --- |
| `NewClient(Options{...})` | `ExecutorClient(environment_id=..., executor=..., ...)` |
| `Options.TokenSource`, `StaticToken(url, token)` | `token_source=`, `static_token(url, token)` |
| `Client.Start` / `Stop` / `State` / `ExecutorID` / `ConnectionID` / `ActiveSessions` | `start()` / `stop(reason)` / `state` / `executor_id` / `connection_id` / `active_sessions` |
| `Tools.Bash.Handle(fn)` | `@client.tools.bash.handler` |
| `OnSessionAssign` / `OnSessionStop` / `OnDrain` | `on_session_assign` / `on_session_stop` / `on_drain` |
| `Requests.X.Handle(client, fn)` / `Calls.X.Call(ctx, client, p)` / `Notifications.X.Notify(ctx, client, p)` | `on_request(methods.X, fn)` / `await call(methods.X, p)` / `await notify(methods.X, p)` |
| `ReportToolResult` / `SessionReady` / `SessionEnded` / `OpenMCPChannel` | `report_tool_result` / `session_ready` / `session_ended` / `open_mcp_channel` |
| `ServeAgent(handler, AgentCapability)` | `serve_agent(handler, capability)` |
| `AgentHandler.OpenTurn` / `RunTurn` / `CloseSession` | `open_turn` / `run_turn` / `close_session` |
| `AgentTurn.Emit` / `EmitWithID` / `NewEventID` / `Delta` / `Flush` / `Inputs()` / `RequestInteraction` / `CancelInteraction` | `emit` / `emit_with_id` / `new_event_id` / `await delta` / `await flush` / `inputs` / `request_interaction` / `cancel_interaction` |
| `NewHarness(HarnessOptions{...})`, `http.Handler` | `Harness(...)`, `await serve(host, port)` / `serve_unix(path)` / `inprocess_dialer()` |
| `HarnessOptions.Authenticate(*http.Request) (Principal, error)` | `authenticate(ConnectRequest) -> Principal` (raise to refuse) |
| `Harness.Executors` / `WaitExecutor` | `executors()` / `wait_executor()` |
| `Executor.AssignSession` / `Drain` / `Capabilities` / `Protocol` | `assign_session` / `drain` / `capabilities` / `protocol` |
| `Executor.StartTurn` / `SendTurnInput` / `CancelTurn` / `CloseAgentSession` / `RespondInteraction` / `AgentCapability` | `start_turn` / `send_turn_input` / `cancel_turn` / `close_agent_session` / `respond_interaction` / `agent_capability()` |
| `Session.Ready` / `CallTool` / `CancelTool` / `Stop` / `MCP` / `Done` / `EndReason` | `ready()` / `call_tool` / `cancel_tool` / `stop` / `mcp` / `wait_ended()`, `ended` / `end_reason` |
| `AgentPlatform` | `AgentPlatform` (Protocol: `acked_sequence`, `agent_event`, `agent_delta`, `interaction_requested`, `interaction_canceled`) |
| `ErrNotConnected` / `ErrClientStopped` / `ErrSessionEnded` / `ErrToolOutcomeUnknown` / `ErrAssignRejected` / `*RPCError` | `NotConnectedError` / `ClientStoppedError` / `SessionEndedError` / `ToolOutcomeUnknownError` / `AssignRejectedError` / `RPCError` |
| `Core`, `NewProcessCapability`, `ListenUnixSocket`, `Harness.DialExecutor` | not provided |

## Examples

Run from [`examples`](examples) after `uv sync` (or `pip install bma-executor-sdk`):

| Example | What it shows |
| --- | --- |
| [`executor_minimal.py`](examples/executor_minimal.py) | executor answering bash calls, against BMA or a local harness |
| [`harness_loopback.py`](examples/harness_loopback.py) | brain and hand in one process, in-process or `--unix PATH` |
| [`agent_minimal.py`](examples/agent_minimal.py) | agent capability with an in-memory platform |

## Tests

```sh
uv sync && uv run pytest -q
```

[`tests/test_executor_interop.py`](tests/test_executor_interop.py) runs the Python executor against the Go harness (WebSocket and Unix socket) and the Python harness against the Go executor; it needs Go.
