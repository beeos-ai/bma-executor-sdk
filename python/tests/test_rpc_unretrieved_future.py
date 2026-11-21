"""A channel that closes (4002 runtime ended) while a call is still being
written must not leave a failed Future nobody retrieves: asyncio logs
"Future exception was never retrieved" when such a Future is collected."""

from __future__ import annotations

import asyncio
import gc
from typing import Any

import pytest

from bma_executor_sdk._rpc import NotConnectedError, Peer
from bma_executor_sdk.transport import CLOSE_FORBIDDEN, ChannelClosed


class ClosingConnection:
    """send blocks until the channel is closed, then fails as a closed channel does."""

    subprotocol = None

    def __init__(self) -> None:
        self.closed = asyncio.Event()

    async def send(self, message: str) -> None:
        await self.closed.wait()
        raise ChannelClosed(CLOSE_FORBIDDEN, "runtime ended")

    async def recv(self) -> str:
        await self.closed.wait()
        raise ChannelClosed(CLOSE_FORBIDDEN, "runtime ended")

    async def close(self, code: int = 1000, reason: str = "") -> None:
        self.closed.set()


async def test_close_during_send_leaves_no_unretrieved_future() -> None:
    loop = asyncio.get_running_loop()
    reported: list[dict[str, Any]] = []
    loop.set_exception_handler(lambda _loop, context: reported.append(context))

    conn = ClosingConnection()

    async def no_request(*_: Any) -> Any:
        return None

    peer = Peer(conn, id_prefix="t", on_request=no_request, on_notification=lambda *_: None, on_log=lambda *_: None)
    reader = asyncio.create_task(peer.run())
    call = asyncio.create_task(peer.call("tool.call", {}, 5.0))
    await asyncio.sleep(0)  # the call is registered and blocked in send
    conn.closed.set()
    with pytest.raises(NotConnectedError):
        await call
    await reader
    del call, reader, peer
    for _ in range(3):
        gc.collect()
        await asyncio.sleep(0)

    assert [c.get("message") for c in reported] == []
