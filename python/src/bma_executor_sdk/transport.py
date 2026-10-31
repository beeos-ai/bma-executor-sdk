"""Transports for the executor protocol.

Every transport carries the same thing: WebSocket-style text messages, one
JSON-RPC (control) or raw MCP (mcp) frame per message, with WebSocket close
codes. Three are provided:

- ``ws://`` / ``wss://``: an outbound WebSocket (the BMA service, or a harness
  on a TCP listener).
- ``unix:///path/to.sock``: the same WebSocket protocol over a Unix domain
  socket, for a harness on the same machine. The query string selects the
  channel exactly as on ``ws://``. Windows named pipes are not implemented yet.
- in-process: :meth:`Harness.inprocess_dialer` connects an executor to a
  harness in the same event loop without a socket.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Protocol
from urllib.parse import parse_qsl, urlencode, urlsplit

# WebSocket close codes (executor.openrpc.yaml): 4001 token expired and 4005
# drained reconnect with a fresh token; 4003 unsupported protocol and 4004
# replaced by a newer connection stop the executor, as do 1008 policy
# violation and 4002 forbidden.
CLOSE_NORMAL = 1000
CLOSE_POLICY_VIOLATION = 1008
CLOSE_INTERNAL_ERROR = 1011
CLOSE_TOKEN_EXPIRED = 4001
CLOSE_FORBIDDEN = 4002
CLOSE_UNSUPPORTED_PROTOCOL = 4003
CLOSE_REPLACED = 4004
CLOSE_DRAINED = 4005
TERMINAL_CLOSE_CODES = frozenset({CLOSE_POLICY_VIOLATION, CLOSE_FORBIDDEN, CLOSE_UNSUPPORTED_PROTOCOL, CLOSE_REPLACED})

MAX_FRAME_BYTES = 1 << 20


class ChannelClosed(Exception):
    """The peer or the transport closed the channel."""

    def __init__(self, code: int, reason: str = "") -> None:
        super().__init__(f"channel closed: {code} {reason}".rstrip())
        self.code = code
        self.reason = reason


class Connection(Protocol):
    """One open channel. recv raises ChannelClosed when the channel ends."""

    @property
    def subprotocol(self) -> str | None: ...

    async def send(self, message: str) -> None: ...

    async def recv(self) -> str: ...

    async def close(self, code: int = CLOSE_NORMAL, reason: str = "") -> None: ...


# Dialer opens one channel: (url, bearer token or "", offered subprotocols).
Dialer = Callable[[str, str, Sequence[str]], Awaitable[Connection]]


@dataclass(frozen=True)
class ConnectRequest:
    """What the harness sees of a channel upgrade: path, query and headers."""

    path: str
    query: Mapping[str, str]
    headers: Mapping[str, str] = field(default_factory=dict)

    @property
    def bearer_token(self) -> str:
        value = next((v for k, v in self.headers.items() if k.lower() == "authorization"), "")
        return value[7:] if value[:7].lower() == "bearer " else ""


class WebSocketConnection:
    """Connection over a ``websockets`` asyncio connection (client or server)."""

    def __init__(self, ws: object) -> None:
        self._ws = ws

    @property
    def subprotocol(self) -> str | None:
        return self._ws.subprotocol  # type: ignore[attr-defined]

    async def send(self, message: str) -> None:
        from websockets.exceptions import ConnectionClosed

        try:
            await self._ws.send(message)  # type: ignore[attr-defined]
        except ConnectionClosed as error:
            raise _closed(error) from error

    async def recv(self) -> str:
        from websockets.exceptions import ConnectionClosed

        try:
            message = await self._ws.recv()  # type: ignore[attr-defined]
        except ConnectionClosed as error:
            raise _closed(error) from error
        return message if isinstance(message, str) else message.decode()

    async def close(self, code: int = CLOSE_NORMAL, reason: str = "") -> None:
        await self._ws.close(code, reason)  # type: ignore[attr-defined]


def _closed(error: Exception) -> ChannelClosed:
    received = getattr(error, "rcvd", None)
    if received is None:
        return ChannelClosed(1006, "abnormal closure")
    return ChannelClosed(received.code, received.reason)


async def dial(url: str, token: str, subprotocols: Sequence[str], *, max_size: int = MAX_FRAME_BYTES) -> Connection:
    """Default dialer: ``ws://``, ``wss://`` and ``unix:///path?query``."""
    from websockets.asyncio.client import connect, unix_connect

    headers = {"Authorization": f"Bearer {token}"} if token else None
    parts = urlsplit(url)
    offered = list(subprotocols)
    if parts.scheme == "unix":
        uri = "ws://localhost/v1/executors/channel" + (f"?{parts.query}" if parts.query else "")
        ws = await unix_connect(parts.path, uri, additional_headers=headers, subprotocols=offered, max_size=max_size)  # type: ignore[arg-type]
    elif parts.scheme in ("ws", "wss"):
        ws = await connect(url, additional_headers=headers, subprotocols=offered, max_size=max_size)  # type: ignore[arg-type]
    else:
        raise ValueError(f"bma_executor_sdk: unsupported channel URL scheme {parts.scheme!r}")
    return WebSocketConnection(ws)


def channel_url(base: str, **query: str) -> str:
    """Return base with the given query members set (used for the MCP channel)."""
    parts = urlsplit(base)
    if parts.scheme not in ("ws", "wss", "unix", "inprocess"):
        raise ValueError("bma_executor_sdk: channel URL must use ws, wss or unix")
    merged = dict(parse_qsl(parts.query))
    merged.update(query)
    return parts._replace(query=urlencode(merged)).geturl()


# --- in-process ---

_EOF = object()


class _PipeEnd:
    def __init__(self, inbox: asyncio.Queue[object], outbox: asyncio.Queue[object], subprotocol: str | None) -> None:
        self._inbox = inbox
        self._outbox = outbox
        self._subprotocol = subprotocol
        self._closed: ChannelClosed | None = None
        self.peer: _PipeEnd | None = None

    @property
    def subprotocol(self) -> str | None:
        return self._subprotocol

    async def send(self, message: str) -> None:
        if self._closed is not None:
            raise self._closed
        await self._outbox.put(message)

    async def recv(self) -> str:
        if self._closed is not None:
            raise self._closed
        item = await self._inbox.get()
        if isinstance(item, ChannelClosed):
            self._closed = item
            raise item
        return item  # type: ignore[return-value]

    async def close(self, code: int = CLOSE_NORMAL, reason: str = "") -> None:
        if self._closed is not None:
            return
        self._closed = ChannelClosed(code, reason)
        await self._outbox.put(ChannelClosed(code, reason))
        await self._inbox.put(ChannelClosed(code, reason))


def pipe(subprotocol: str | None) -> tuple[Connection, Connection]:
    """Return two connected in-process channel ends."""
    a: asyncio.Queue[object] = asyncio.Queue()
    b: asyncio.Queue[object] = asyncio.Queue()
    return _PipeEnd(a, b, subprotocol), _PipeEnd(b, a, subprotocol)
