"""executor.token.refresh: one control connection across token lifetimes,
and the 4001 reconnect on a harness without the method."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable
from typing import Any

from websockets.asyncio.server import ServerConnection, serve

import bma_executor_sdk as bx

EXECUTOR = {"name": "py-test", "os": "linux", "arch": "arm64", "version": "0.1.0"}


async def fake_harness(answer_refresh: Callable[[dict[str, Any]], dict[str, Any]]):
    refreshes: list[str] = []
    dials: list[int] = []

    async def handler(ws: ServerConnection) -> None:
        dials.append(1)
        async for raw in ws:
            frame = json.loads(raw)
            if "id" not in frame or "method" not in frame:
                continue
            if frame["method"] == "executor.register":
                reply: dict[str, Any] = {
                    "result": {
                        "protocol": "bma.executor.v1.1", "executor_id": "exec_test", "connection_id": "conn_test",
                        "server_time": "2026-11-01T00:00:00Z", "heartbeat_interval_ms": 60000, "lease_ttl_ms": 60000,
                        "max_control_frame_bytes": 65536, "max_mcp_frame_bytes": 1 << 20,
                    }
                }
            elif frame["method"] == "executor.token.refresh":
                refreshes.append(frame["params"]["token"])
                reply = answer_refresh(frame)
            else:
                reply = {"result": {"accepted": True}}
            await ws.send(json.dumps({"jsonrpc": "2.0", "id": frame["id"], **reply}))

    server = await serve(handler, "127.0.0.1", 0, subprotocols=["bma.executor.v1.1"])
    port = server.sockets[0].getsockname()[1]
    return server, f"ws://127.0.0.1:{port}", refreshes, dials


def client(url: str) -> bx.ExecutorClient:
    minted = 0

    async def source() -> Any:
        nonlocal minted
        minted += 1
        return {"executor_id": "exec_test", "token": f"tok-{minted}", "expires_in": 1, "ws_url": url, "channel_scope": []}

    return bx.ExecutorClient(environment_id="", executor=EXECUTOR, token_source=source)  # type: ignore[arg-type]


async def test_refreshes_inside_the_connection() -> None:
    server, url, refreshes, dials = await fake_harness(lambda _: {"result": {"expires_at": "2026-11-01T00:00:01Z"}})
    executor = client(url)
    try:
        await executor.start()
        for _ in range(400):
            if len(refreshes) >= 3:
                break
            await asyncio.sleep(0.01)
        assert refreshes[:3] == ["tok-2", "tok-3", "tok-4"]
        assert len(dials) == 1
    finally:
        await executor.stop()
        server.close()
        await server.wait_closed()


async def test_stops_refreshing_on_method_not_found() -> None:
    server, url, refreshes, _ = await fake_harness(
        lambda _: {"error": {"code": -32601, "message": "unknown method executor.token.refresh"}}
    )
    executor = client(url)
    try:
        await executor.start()
        await asyncio.sleep(2.5)
        assert refreshes == ["tok-2"]
    finally:
        await executor.stop()
        server.close()
        await server.wait_closed()
