# bma-executor-sdk (Python)

[![PyPI](https://img.shields.io/pypi/v/bma-executor-sdk.svg)](https://pypi.org/project/bma-executor-sdk/)

asyncio executor (`ExecutorClient`), harness (`Harness`) and agent ends of the BMA Executor Protocol `bma.executor.v1.1`, the counterpart of the [Go `executor` module](../README.md). Protocol types are TypedDicts generated into `bma_executor_sdk/generated.py` from the contract in [`contracts/bma/executor`](../../contracts/bma/executor), so messages are plain JSON dicts.

| | |
| --- | --- |
| Version | 0.4.0 (tag `executor-python/v0.4.0`), see [CHANGELOG](../../CHANGELOG.md) |
| Protocol | `bma.executor.v1.1` (contract 1.1.0, [`lock.json`](../../contracts/bma/executor/lock.json)) |
| Python | 3.10 or newer; depends on `httpx` and `websockets` |
| Integration guide | [docs/executor](../../docs/executor/README.md) ([中文](../../docs/executor/README.zh-CN.md)) |

```sh
pip install bma-executor-sdk==0.4.0
```

The REST / façade client is the separate package [`bma-sdk`](../../python).

## Executor quick start

```python
import asyncio, os
from bma_executor_sdk import ExecutorClient

async def main() -> None:
    client = ExecutorClient(
        environment_key=os.environ["BMA_ENVIRONMENT_KEY"],
        environment_id=os.environ["BMA_ENVIRONMENT_ID"],
        executor={"name": "my-executor", "os": "linux", "arch": "arm64", "version": "0.1.0"},
    )

    async def on_tool_call(call):
        return {"session_id": call["session_id"], "tool_call_id": call["tool_call_id"],
                "outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": "ok"}]}

    client.on_tool_call(on_tool_call)
    await client.start()
    await asyncio.Event().wait()

asyncio.run(main())
```

A complete version that runs bash and stops the process on cancellation is [`examples/executor_minimal.py`](examples/executor_minimal.py); set `BMA_HARNESS_URL` (`ws://…` or `unix:///path.sock`) to connect it to a local harness instead of BMA.

Behavior matches the Go SDK:

- `tool.cancel` cancels the handler's task and reports `outcome: "canceled"`; duplicate `tool.call` ids run once.
- `tool.result` retries `-32009` / `-32004` / `retryable` errors and hands a lost result to `on_tool_result_lost`.
- Reconnect resumes with `resume.executor_id` / `active_sessions`; close codes 1008, 4002, 4003 and 4004 stop the client (4001 and 4005 reconnect).
- Call `session_ready` after setup and `session_ended` after cleanup. `on_request(method, handler)` serves `process.*`, `terminal.*` or `skill.install` yourself; there is no execution `Core` in Python.

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
| In process | `dialer=harness.inprocess_dialer()` | — |

The executor-listens local socket and Windows named pipes are not implemented in Python (use the Go or TypeScript SDK). See [local connection](../../docs/executor/local-connection.md).

## Agent executors

`client.serve_agent(handler, capability)` serves the `agent` capability; `handler` implements `open_turn`, `run_turn` and `close_session` (async; cancellation is task cancellation), and `run_turn` uses `turn.emit`, `await turn.delta(...)`, `turn.inputs` (an `asyncio.Queue`) and `turn.request_interaction`. The harness side is `Harness(agent=platform)` with an `AgentPlatform`. See [agent capability](../../docs/executor/agent-capability.md).

## Go ↔ Python

| Go (`bmaexecutor`) | Python (`bma_executor_sdk`) |
| --- | --- |
| `NewClient(Options{...})` | `ExecutorClient(environment_id=..., executor=..., ...)` |
| `Options.TokenSource`, `StaticToken(url, token)` | `token_source=`, `static_token(url, token)` |
| `Client.Start` / `Stop` / `State` / `ExecutorID` / `ConnectionID` / `ActiveSessions` | `start()` / `stop(reason)` / `state` / `executor_id` / `connection_id` / `active_sessions` |
| `OnToolCall` / `OnSessionAssign` / `OnSessionStop` / `OnDrain` / `OnRequest` / `OnNotification` | `on_tool_call` / `on_session_assign` / `on_session_stop` / `on_drain` / `on_request` / `on_notification` |
| `Call` / `Notify` / `ReportToolResult` / `SessionReady` / `SessionEnded` / `OpenMCPChannel` | `call` / `notify` / `report_tool_result` / `session_ready` / `session_ended` / `open_mcp_channel` |
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
