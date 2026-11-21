"""Reconnect policy (Go reconnect_test.go): by default the client reconnects
until stop() through any outage; a credential the token endpoint rejects
(401/403) and an exhausted positive max_reconnect_attempts end the client,
and wait_closed() raises the reason instead of the client hanging."""

from __future__ import annotations

import asyncio
from typing import Any

import httpx
import pytest

import bma_executor_sdk as bx
from bma_executor_sdk import client as bxc

EXECUTOR = {"name": "py-test", "os": "linux", "arch": "arm64", "version": "0.1.0"}
Ready = bx.generated.ConnectionState.Ready


class TokenEndpoint:
    """Answers the first token request, then fail_status for the next
    fail_for requests (forever when fail_for < 0)."""

    def __init__(self, fail_status: int, fail_for: int) -> None:
        self.requests = 0
        self.fail_status = fail_status
        self.fail_for = fail_for

    def __call__(self, request: httpx.Request) -> httpx.Response:
        self.requests += 1
        failed = self.requests - 1
        if failed > 0 and (self.fail_for < 0 or failed <= self.fail_for):
            return httpx.Response(self.fail_status, text="upstream unavailable")
        return httpx.Response(200, json={"executor_id": "", "token": "jwt", "expires_in": 300, "ws_url": "inprocess://harness"})


async def start_client(harness: bx.Harness, endpoint: TokenEndpoint, **options: Any) -> bx.ExecutorClient:
    client = bx.ExecutorClient(
        environment_id="env_local", executor=EXECUTOR, environment_key="env_key", base_url="http://bma.test",
        http_client=httpx.AsyncClient(transport=httpx.MockTransport(endpoint)), dialer=harness.inprocess_dialer(),
        request_timeout=2.0, reconnect_base_delay=0.001, reconnect_max_delay=0.002, **options,
    )
    await client.start()
    return client


async def drop(client: bx.ExecutorClient) -> None:
    await client._peer.conn.close(1012, "node replaced")  # noqa: SLF001


async def test_reconnects_after_a_long_outage_by_default() -> None:
    harness = bx.Harness(request_timeout=2.0)
    endpoint = TokenEndpoint(503, 25)
    client = await start_client(harness, endpoint)
    first = client.connection_id
    await drop(client)
    for _ in range(500):
        if client.state == Ready and client.connection_id != first:
            break
        await asyncio.sleep(0.01)
    assert client.state == Ready and client.connection_id != first
    assert endpoint.requests == 27  # the start, 25 failures, the reconnect
    await client.stop()
    await asyncio.wait_for(client.wait_closed(), 1)  # stop() ends it without an error


@pytest.mark.parametrize("status", [401, 403])
async def test_rejected_credential_ends_the_client(status: int) -> None:
    harness = bx.Harness(request_timeout=2.0)
    endpoint = TokenEndpoint(status, -1)
    client = await start_client(harness, endpoint)
    await drop(client)
    with pytest.raises(bx.CredentialRejectedError) as raised:
        await asyncio.wait_for(client.wait_closed(), 2)
    assert raised.value.status_code == status
    assert endpoint.requests == 2  # no retry after the rejection
    assert client.state == bx.generated.ConnectionState.Closed


async def test_exhausted_max_reconnect_attempts_ends_the_client() -> None:
    harness = bx.Harness(request_timeout=2.0)
    endpoint = TokenEndpoint(503, -1)
    client = await start_client(harness, endpoint, max_reconnect_attempts=3)
    await drop(client)
    with pytest.raises(bx.ReconnectExhaustedError) as raised:
        await asyncio.wait_for(client.wait_closed(), 2)
    assert isinstance(raised.value.__cause__, bx.ExecutorTokenError)
    assert endpoint.requests == 4  # the start and 3 reconnect attempts


async def test_terminal_close_code_ends_the_client() -> None:
    harness = bx.Harness(request_timeout=2.0)
    client = await start_client(harness, TokenEndpoint(503, 0))
    await client._peer.conn.close(4004, "replaced")  # noqa: SLF001
    with pytest.raises(bx.ChannelClosed) as raised:
        await asyncio.wait_for(client.wait_closed(), 2)
    assert raised.value.code == 4004


async def test_heartbeat_timeout_reconnects(monkeypatch: pytest.MonkeyPatch) -> None:
    """A heartbeat unanswered within request_timeout means a dead (half-open)
    connection: the client closes it and reconnects (gRPC keepalive)."""
    from bma_executor_sdk import harness as bxh

    silent = {"on": False}
    dispatch = bxh._HarnessConn._dispatch  # noqa: SLF001

    async def maybe_silent(self: Any, executor: Any, request_id: str, method: str, params: Any) -> Any:
        if method == bx.generated.METHOD_EXECUTOR_HEARTBEAT and silent["on"]:
            await asyncio.Event().wait()
        return await dispatch(self, executor, request_id, method, params)

    monkeypatch.setattr(bxh._HarnessConn, "_dispatch", maybe_silent)  # noqa: SLF001
    harness = bx.Harness(request_timeout=2.0, heartbeat_interval=0.02)
    client = await start_client(harness, TokenEndpoint(503, 0))
    client._request_timeout = 0.1  # noqa: SLF001
    first = client.connection_id
    silent["on"] = True
    for _ in range(300):
        if client.state == Ready and client.connection_id != first:
            break
        await asyncio.sleep(0.01)
    assert client.state == Ready and client.connection_id != first
    await client.stop()


def test_reconnect_defaults() -> None:
    assert bxc.DEFAULT_MAX_RECONNECT_ATTEMPTS == 0
    assert bxc.DEFAULT_RECONNECT_BASE_DELAY == 1.0


async def test_backoff_stays_capped_after_many_attempts() -> None:
    harness = bx.Harness(request_timeout=2.0)
    client = await start_client(harness, TokenEndpoint(503, 3))
    first = client.connection_id
    client._reconnect_attempts = 5000  # noqa: SLF001  hours into an outage
    await drop(client)
    for _ in range(300):
        if client.state == Ready and client.connection_id != first:
            break
        await asyncio.sleep(0.01)
    assert client.state == Ready and client.connection_id != first  # no OverflowError in the backoff
    await client.stop()
