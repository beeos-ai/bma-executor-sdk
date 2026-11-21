"""A machine-scoped executor that declares neither the process nor the agent
capability is closed with 4002 "runtime registration refused" right after
executor.register succeeds. Reconnecting cannot fix it: after 3 such refusals
in a row the client ends (wait_closed() raises RegistrationRefusedError), and
wait_ready() returns only for a registration that stayed open for one
heartbeat interval (Go registration_refused_test.go)."""

from __future__ import annotations

import asyncio
import json
import time
from collections.abc import Awaitable, Callable, Sequence
from typing import Any

import httpx
import pytest

import bma_executor_sdk as bx
from bma_executor_sdk.transport import Connection, pipe

EXECUTOR = {"name": "py-test", "os": "linux", "arch": "arm64", "version": "0.1.0"}
REFUSED = "runtime registration refused"


class FakeHarness:
    """Answers executor.register on every dialed channel, then runs after(n, harness_end)."""

    def __init__(
        self,
        after: Callable[[int, Connection], Awaitable[None]] | None = None,
        *,
        register_error: int = 0,
        heartbeat_ms: int = 60000,
    ) -> None:
        self.registrations = 0
        self._after = after
        self._register_error = register_error
        self._heartbeat_ms = heartbeat_ms
        self._tasks: set[asyncio.Task[None]] = set()

    async def dial(self, url: str, token: str, subprotocols: Sequence[str]) -> Connection:
        executor_end, harness_end = pipe(bx.PROTOCOL_V11)
        task = asyncio.ensure_future(self._serve(harness_end))
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
        return executor_end

    async def _serve(self, conn: Connection) -> None:
        try:
            while True:
                frame = json.loads(await conn.recv())
                if frame.get("method") != "executor.register":
                    continue
                self.registrations += 1
                n = self.registrations
                if self._register_error and n > 1:
                    reply: dict[str, Any] = {"jsonrpc": "2.0", "id": frame["id"], "error": {"code": self._register_error, "message": "capability not available"}}
                    await conn.send(json.dumps(reply))
                    continue
                result = {
                    "protocol": bx.PROTOCOL_V11, "executor_id": "exec_test", "connection_id": f"conn_{n}",
                    "server_time": "2026-11-07T00:00:00Z", "heartbeat_interval_ms": self._heartbeat_ms,
                    "lease_ttl_ms": 60000, "max_control_frame_bytes": 65536, "max_mcp_frame_bytes": 1 << 20,
                }
                await conn.send(json.dumps({"jsonrpc": "2.0", "id": frame["id"], "result": result}))
                if self._after is not None:
                    await self._after(n, conn)
        except bx.ChannelClosed:
            return

    def cancel(self) -> None:
        for task in list(self._tasks):
            task.cancel()


def token_endpoint(request: httpx.Request) -> httpx.Response:
    return httpx.Response(200, json={"executor_id": "", "token": "jwt", "expires_in": 300, "ws_url": "inprocess://harness"})


async def start_client(harness: FakeHarness) -> bx.ExecutorClient:
    client = bx.ExecutorClient(
        environment_id="", executor=EXECUTOR, environment_key="env_key", base_url="http://bma.test",
        http_client=httpx.AsyncClient(transport=httpx.MockTransport(token_endpoint)), dialer=harness.dial,
        request_timeout=2.0, reconnect_base_delay=0.001, reconnect_max_delay=0.002,
    )
    await client.start()
    return client


def close_after(delay: float, code: int, reason: str) -> Callable[[int, Connection], Awaitable[None]]:
    async def after(n: int, conn: Connection) -> None:
        await asyncio.sleep(delay)
        await conn.close(code, reason)

    return after


async def until(predicate: Callable[[], bool], timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while not predicate():
        assert time.monotonic() < deadline, "timed out"
        await asyncio.sleep(0.01)


async def test_three_refusals_in_a_row_end_the_client() -> None:
    harness = FakeHarness(close_after(0.005, 4002, REFUSED))
    client = await start_client(harness)
    with pytest.raises(bx.RegistrationRefusedError) as raised:
        await asyncio.wait_for(client.wait_closed(), 5)
    assert isinstance(raised.value.__cause__, bx.ChannelClosed) and raised.value.__cause__.code == 4002
    assert harness.registrations == 3
    assert client.state == bx.generated.ConnectionState.Closed
    harness.cancel()


async def test_runtime_ended_4002_is_not_a_configuration_error() -> None:
    harness = FakeHarness(close_after(0.005, 4002, "daemon runtime ended"))
    client = await start_client(harness)
    await until(lambda: harness.registrations >= 8)
    assert client.state != bx.generated.ConnectionState.Closed
    await client.stop()
    harness.cancel()


async def test_a_refusal_of_a_stable_connection_is_not_counted() -> None:
    harness = FakeHarness(close_after(0.12, 4002, REFUSED), heartbeat_ms=20)
    client = await start_client(harness)
    await until(lambda: harness.registrations >= 6, 8)
    assert client.state != bx.generated.ConnectionState.Closed
    await client.stop()
    harness.cancel()


@pytest.mark.parametrize("code", [-32012, -32602])
async def test_register_error_counts_as_a_refusal(code: int) -> None:
    harness = FakeHarness(close_after(0.005, 1012, "node replaced"), register_error=code)
    client = await start_client(harness)
    with pytest.raises(bx.RegistrationRefusedError) as raised:
        await asyncio.wait_for(client.wait_closed(), 5)
    assert isinstance(raised.value.__cause__, bx.RPCError) and raised.value.__cause__.code == code
    harness.cancel()


async def test_wait_ready_needs_one_heartbeat_interval() -> None:
    harness = FakeHarness(heartbeat_ms=300)
    started = time.monotonic()
    client = await start_client(harness)
    await client.wait_ready(5)
    assert time.monotonic() - started >= 0.25
    assert client.state == bx.generated.ConnectionState.Ready
    await client.stop()
    harness.cancel()


async def test_wait_ready_raises_the_configuration_error_for_refusals() -> None:
    harness = FakeHarness(close_after(0.005, 4002, REFUSED))
    client = await start_client(harness)
    with pytest.raises(bx.RegistrationRefusedError):
        await client.wait_ready(5)
    harness.cancel()


async def test_wait_ready_times_out_while_the_registration_is_not_stable() -> None:
    harness = FakeHarness()
    client = await start_client(harness)
    with pytest.raises(TimeoutError):
        await client.wait_ready(0.05)
    await client.stop()
    harness.cancel()
