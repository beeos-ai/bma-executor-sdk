"""WebSocket over a Windows named pipe (``npipe:////./pipe/<name>``).

``websockets`` has no named pipe transport, so this drives its sans-I/O
protocol from one ``asyncio.Protocol`` that works on any asyncio transport.
On Windows the transport comes from the ProactorEventLoop (the default),
``create_pipe_connection`` and ``start_serving_pipe``; the protocol class
itself is transport-agnostic and is tested over a Unix socket on other
platforms. No pywin32.
"""

from __future__ import annotations

import asyncio
import re
from collections.abc import Awaitable, Callable, Mapping, Sequence
from http import HTTPStatus
from urllib.parse import urlsplit

from websockets.client import ClientProtocol
from websockets.frames import Frame, Opcode
from websockets.http11 import Request, Response
from websockets.server import ServerProtocol
from websockets.uri import parse_uri

from .transport import CLOSE_NORMAL, ChannelClosed, Connection

PIPE_DIR = "\\\\.\\pipe\\"
_CLOSE_TIMEOUT = 5.0
_OPEN_TIMEOUT = 10.0
_PIPE_PATH = re.compile(r"^/{1,2}(?:\./)?pipe/([^/\\]+)$")


def pipe_path(url_or_name: str) -> str:
    """The ``\\\\.\\pipe\\<name>`` path of an ``npipe:////./pipe/<name>`` URL, a
    ``\\\\.\\pipe\\<name>`` path or a bare pipe name."""
    name = url_or_name
    if url_or_name.startswith("npipe:"):
        found = _PIPE_PATH.match(urlsplit(url_or_name).path)
        name = found.group(1) if found else ""
    elif url_or_name.startswith(PIPE_DIR):
        name = url_or_name[len(PIPE_DIR):]
    if not name or "/" in name or "\\" in name:
        raise ValueError(f"bma_executor_sdk: invalid named pipe {url_or_name!r}")
    return PIPE_DIR + name


def pipe_url(path_or_name: str) -> str:
    return "npipe:////./pipe/" + pipe_path(path_or_name)[len(PIPE_DIR):]


# What the server side does with an upgrade request (subprotocols offered,
# request target, headers): admit it, returning the handler that serves the
# connection, or refuse it with a status and a message.
Handler = Callable[[Connection], Awaitable[None]]
Admission = Handler | tuple[HTTPStatus, str]
Admit = Callable[[str, str, Mapping[str, str]], Awaitable[Admission]]


class PipeWebSocket(asyncio.Protocol):
    """One WebSocket end (client or server) on an asyncio transport; the
    Connection interface of the executor protocol."""

    def __init__(self, protocol: ClientProtocol | ServerProtocol, *, token: str = "", admit: Admit | None = None) -> None:
        self._protocol = protocol
        self._token = token
        self._admit = admit
        self._transport: asyncio.Transport | None = None
        self._messages: asyncio.Queue[str | ChannelClosed] = asyncio.Queue()
        self._fragments: list[bytes] = []
        self._closed: ChannelClosed | None = None
        loop = asyncio.get_running_loop()
        self._opened: asyncio.Future[None] = loop.create_future()
        self._opened.add_done_callback(lambda done: done.cancelled() or done.exception())  # a server end never awaits it
        self._lost = asyncio.Event()
        self._eof = False
        self.on_lost: Callable[[PipeWebSocket], None] | None = None
        self.tasks: set[asyncio.Task[None]] = set()

    # --- Connection ---

    @property
    def subprotocol(self) -> str | None:
        return self._protocol.subprotocol

    async def send(self, message: str) -> None:
        if self._closed is not None or self._transport is None:
            raise self._closed or ChannelClosed(1006, "abnormal closure")
        try:
            self._protocol.send_text(message.encode())
        except Exception as error:
            raise ChannelClosed(self._protocol.close_code or 1006, "connection closed") from error
        self._flush()

    async def recv(self) -> str:
        if self._closed is not None and self._messages.empty():
            raise self._closed
        item = await self._messages.get()
        if isinstance(item, ChannelClosed):
            self._messages.put_nowait(item)  # every later recv fails the same way
            raise item
        return item

    async def close(self, code: int = CLOSE_NORMAL, reason: str = "") -> None:
        if self._transport is None:
            return
        try:
            self._protocol.send_close(code, reason)
        except Exception:
            pass  # already closing
        self._flush()
        try:
            await asyncio.wait_for(self._lost.wait(), _CLOSE_TIMEOUT)
        except asyncio.TimeoutError:
            self._transport.abort()

    def abort(self) -> None:
        if self._transport is not None:
            self._transport.abort()

    async def handshake(self) -> None:
        """Client: wait for the upgrade response."""
        await asyncio.wait_for(asyncio.shield(self._opened), _OPEN_TIMEOUT)

    # --- asyncio.Protocol ---

    def connection_made(self, transport: asyncio.BaseTransport) -> None:
        self._transport = transport  # type: ignore[assignment]
        if isinstance(self._protocol, ClientProtocol):
            request = self._protocol.connect()
            if self._token:
                request.headers["Authorization"] = f"Bearer {self._token}"
            self._protocol.send_request(request)
            self._flush()

    def data_received(self, data: bytes) -> None:
        self._protocol.receive_data(data)
        self._pump()

    def eof_received(self) -> bool:
        self._receive_eof()
        self._pump()
        return False  # close the transport

    def _receive_eof(self) -> None:
        if not self._eof:
            self._eof = True
            self._protocol.receive_eof()

    def connection_lost(self, exc: Exception | None) -> None:
        self._receive_eof()
        self._pump(lost=True)
        close = self._protocol.close_rcvd
        self._closed = ChannelClosed(close.code, close.reason) if close is not None else ChannelClosed(1006, "abnormal closure")
        self._messages.put_nowait(self._closed)
        if not self._opened.done():
            self._opened.set_exception(ConnectionError(f"named pipe closed during the WebSocket handshake: {exc or 'EOF'}"))
        self._lost.set()
        if self.on_lost is not None:
            self.on_lost(self)

    # --- driving the sans-I/O protocol ---

    def _flush(self) -> None:
        transport = self._transport
        if transport is None:
            return
        for data in self._protocol.data_to_send():
            if data == b"":
                transport.close()  # the protocol is done: end the stream
            elif not transport.is_closing():
                transport.write(data)

    def _pump(self, lost: bool = False) -> None:
        for event in self._protocol.events_received():
            if isinstance(event, Response):
                self._on_response()
            elif isinstance(event, Request):
                self._on_request(event)
            elif isinstance(event, Frame):
                self._on_frame(event)
        if not lost:
            self._flush()

    def _on_response(self) -> None:
        error = self._protocol.handshake_exc
        if self._opened.done():
            return
        if error is None:
            self._opened.set_result(None)
        else:
            self._opened.set_exception(error)

    def _on_request(self, request: Request) -> None:
        task = asyncio.ensure_future(self._serve(request))
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)

    async def _serve(self, request: Request) -> None:
        protocol = self._protocol
        assert isinstance(protocol, ServerProtocol) and self._admit is not None
        offered = ",".join(request.headers.get_all("Sec-WebSocket-Protocol"))
        admission = await self._admit(offered, request.path, dict(request.headers.raw_items()))
        if isinstance(admission, tuple):
            protocol.send_response(protocol.reject(admission[0], admission[1] + "\n"))
            self._flush()
            return
        handler = admission
        response = protocol.accept(request)
        protocol.send_response(response)
        self._flush()
        if protocol.handshake_exc is not None:
            return
        try:
            await handler(self)
        finally:
            await self.close()

    def _on_frame(self, frame: Frame) -> None:
        if frame.opcode in (Opcode.TEXT, Opcode.BINARY):
            self._fragments = [frame.data]
        elif frame.opcode is Opcode.CONT:
            self._fragments.append(frame.data)
        else:
            return  # ping/pong are answered by the protocol; close ends the stream
        if frame.fin:
            self._messages.put_nowait(b"".join(self._fragments).decode())
            self._fragments = []


async def dial(url: str, token: str, subprotocols: Sequence[str], *, max_size: int) -> PipeWebSocket:
    """Open a channel on the named pipe of an ``npipe://`` URL (ProactorEventLoop)."""
    loop = asyncio.get_running_loop()
    if not hasattr(loop, "create_pipe_connection"):
        raise NotImplementedError("bma_executor_sdk: npipe:// needs the Windows ProactorEventLoop")
    path = pipe_path(url)
    query = urlsplit(url).query
    uri = parse_uri("ws://localhost/v1/executors/channel" + (f"?{query}" if query else ""))
    ws = PipeWebSocket(ClientProtocol(uri, subprotocols=list(subprotocols), max_size=max_size), token=token)  # type: ignore[arg-type]
    await loop.create_pipe_connection(lambda: ws, path)  # type: ignore[attr-defined]
    try:
        await ws.handshake()
    except BaseException:
        ws.abort()
        raise
    return ws


class NamedPipeServer:
    """The listening named pipe of Harness.serve_npipe."""

    def __init__(self, servers: list[object], connections: set[PipeWebSocket]) -> None:
        self._servers = servers
        self._connections = connections

    def close(self) -> None:
        for server in self._servers:
            server.close()  # type: ignore[attr-defined]
        for connection in list(self._connections):
            connection.abort()

    async def wait_closed(self) -> None:
        while self._connections:
            await asyncio.sleep(0.01)


async def serve(path_or_name: str, admit: Admit, subprotocols: Sequence[str], *, max_size: int) -> NamedPipeServer:
    """Listen on a named pipe for the current user (the first instance of the
    pipe name; Windows ProactorEventLoop)."""
    loop = asyncio.get_running_loop()
    if not hasattr(loop, "start_serving_pipe"):
        raise NotImplementedError("bma_executor_sdk: npipe:// needs the Windows ProactorEventLoop")
    connections: set[PipeWebSocket] = set()

    def factory() -> PipeWebSocket:
        ws = PipeWebSocket(ServerProtocol(subprotocols=list(subprotocols), max_size=max_size), admit=admit)  # type: ignore[arg-type]
        connections.add(ws)
        ws.on_lost = connections.discard
        return ws

    servers = await loop.start_serving_pipe(factory, pipe_path(path_or_name))  # type: ignore[attr-defined]
    return NamedPipeServer(servers, connections)
