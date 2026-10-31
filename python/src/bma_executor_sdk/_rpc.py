"""JSON-RPC 2.0 over a Connection, shared by the executor and harness ends."""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from typing import Any

from .generated import JsonRpcErrorCode
from .transport import ChannelClosed, Connection


class ExecutorError(Exception):
    """Base class of bma_sdk.executor errors."""


class NotConnectedError(ExecutorError):
    """The channel is not connected (yet, or any more)."""

    def __init__(self, message: str = "executor channel is not connected") -> None:
        super().__init__(message)


class ClientStoppedError(ExecutorError):
    """The client was stopped, or closed with a terminal close code."""

    def __init__(self, message: str = "client stopped") -> None:
        super().__init__(message)


class RPCError(ExecutorError):
    """A JSON-RPC error. A handler that raises it keeps its code on the wire."""

    def __init__(self, code: int, message: str, data: dict[str, Any] | None = None) -> None:
        super().__init__(f"json-rpc error {code}: {message}")
        self.code = code
        self.message = message
        self.data = data

    @property
    def retryable(self) -> bool | None:
        return None if self.data is None else self.data.get("retryable")

    def to_wire(self) -> dict[str, Any]:
        error: dict[str, Any] = {"code": int(self.code), "message": self.message}
        if self.data is not None:
            error["data"] = self.data
        return error


def rpc_error(code: JsonRpcErrorCode, message: str, retryable: bool) -> RPCError:
    return RPCError(code, message, {"retryable": retryable})


RequestHandler = Callable[[str, str, Any], Awaitable[Any]]
NotificationHandler = Callable[[str, Any], None]


class Peer:
    """One JSON-RPC endpoint on a Connection.

    Inbound requests run as tasks (except methods in ``inline``, which are
    handled in read order); notifications are handled in read order.
    """

    def __init__(
        self,
        conn: Connection,
        *,
        id_prefix: str,
        on_request: RequestHandler,
        on_notification: NotificationHandler,
        on_log: Callable[[str, str, BaseException | None], None],
        inline: frozenset[str] = frozenset(),
    ) -> None:
        self.conn = conn
        self._id_prefix = id_prefix
        self._on_request = on_request
        self._on_notification = on_notification
        self._log = on_log
        self._inline = inline
        self._pending: dict[str, asyncio.Future[Any]] = {}
        self._next_id = 0
        self._tasks: set[asyncio.Task[Any]] = set()
        self.closed: ChannelClosed | None = None

    async def call(self, method: str, params: Any, timeout: float) -> Any:
        if self.closed is not None:
            raise NotConnectedError()
        self._next_id += 1
        request_id = f"{self._id_prefix}_{self._next_id}"
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        try:
            await self._send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
            return await asyncio.wait_for(future, timeout)
        except asyncio.TimeoutError as error:
            raise TimeoutError(f"{method}: no response within {timeout}s") from error
        finally:
            self._pending.pop(request_id, None)

    async def notify(self, method: str, params: Any) -> None:
        if self.closed is not None:
            raise NotConnectedError()
        await self._send({"jsonrpc": "2.0", "method": method, "params": params})

    async def _send(self, message: dict[str, Any]) -> None:
        try:
            await self.conn.send(json.dumps(message, separators=(",", ":")))
        except ChannelClosed as error:
            raise NotConnectedError(f"write {message.get('method', 'response')}: {error}") from error

    async def run(self) -> ChannelClosed:
        """Read until the channel closes; then fail pending calls."""
        try:
            while True:
                raw = await self.conn.recv()
                try:
                    message = json.loads(raw)
                except ValueError as error:
                    self._log("warn", "invalid JSON-RPC frame", error)
                    continue
                if isinstance(message, dict) and message.get("method") in self._inline and message.get("id") is not None:
                    await self._handle_request(str(message["id"]), message["method"], message.get("params"))
                    continue
                self._dispatch(message)
        except ChannelClosed as closed:
            self.closed = closed
        except asyncio.CancelledError:
            self.closed = ChannelClosed(1006, "reader canceled")
            raise
        finally:
            if self.closed is None:
                self.closed = ChannelClosed(1006, "reader failed")
            self.fail_pending(NotConnectedError())
        return self.closed

    def fail_pending(self, error: Exception) -> None:
        pending, self._pending = self._pending, {}
        for future in pending.values():
            if not future.done():
                future.set_exception(error)

    def _dispatch(self, message: Any) -> None:
        if not isinstance(message, dict):
            self._log("warn", "JSON-RPC frame is not an object", None)
            return
        method, request_id = message.get("method"), message.get("id")
        if method and request_id is not None:
            self._spawn(self._handle_request(str(request_id), method, message.get("params")))
        elif method:
            try:
                self._on_notification(method, message.get("params"))
            except Exception as error:  # a handler bug must not stop the channel
                self._log("error", f"{method} notification handler failed", error)
        elif request_id is not None:
            future = self._pending.pop(str(request_id), None)
            if future is None or future.done():
                return
            error = message.get("error")
            if error is not None:
                future.set_exception(RPCError(error.get("code", 0), error.get("message", ""), error.get("data")))
            else:
                future.set_result(message.get("result"))
        else:
            self._log("warn", "JSON-RPC frame is neither request nor response", None)

    def _spawn(self, coro: Awaitable[None]) -> None:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)

    async def _handle_request(self, request_id: str, method: str, params: Any) -> None:
        try:
            result = await self._on_request(request_id, method, params)
            response: dict[str, Any] = {"jsonrpc": "2.0", "id": request_id, "result": result}
        except RPCError as error:
            response = {"jsonrpc": "2.0", "id": request_id, "error": error.to_wire()}
        except asyncio.CancelledError:
            raise
        except Exception as error:
            self._log("warn", f"{method} handler failed", error)
            response = {"jsonrpc": "2.0", "id": request_id, "error": {"code": int(JsonRpcErrorCode.InternalError), "message": str(error)}}
        try:
            await self._send(response)
        except NotConnectedError as error:
            self._log("debug", f"write {method} response", error)

    def cancel_tasks(self) -> None:
        for task in list(self._tasks):
            task.cancel()
