"""Shared fixtures for the executor tests: an in-memory AgentPlatform."""

from __future__ import annotations

import asyncio
import os
import tempfile
from typing import Any

from bma_executor_sdk import AgentEventRecord


class RecordingPlatform:
    """AgentPlatform that keeps everything in memory (Go: newRecordingPlatform)."""

    def __init__(self) -> None:
        self.events: list[AgentEventRecord] = []
        self.deltas: list[dict[str, Any]] = []
        self.interactions: dict[str, dict[str, Any]] = {}
        self.canceled: list[dict[str, Any]] = []
        self.changed = asyncio.Event()
        self.fail_next_event = False

    def _signal(self) -> None:
        changed, self.changed = self.changed, asyncio.Event()
        changed.set()

    async def acked_sequence(self, session_id: str) -> int:
        return max((e.sequence for e in self.events if e.session_id == session_id), default=0)

    async def agent_event(self, executor: Any, record: AgentEventRecord) -> None:
        if self.fail_next_event:
            self.fail_next_event = False
            raise RuntimeError("store unavailable")
        self.events.append(record)
        self._signal()

    def agent_delta(self, executor: Any, delta: dict[str, Any]) -> None:
        self.deltas.append(delta)
        self._signal()

    async def interaction_requested(self, executor: Any, request: dict[str, Any]) -> str:
        self.interactions[request["interaction_id"]] = request
        self._signal()
        return "pint_" + request["interaction_id"]

    async def interaction_canceled(self, executor: Any, cancel: dict[str, Any]) -> None:
        self.canceled.append(cancel)
        self._signal()

    def types(self, session_id: str) -> list[str]:
        return [e.type for e in self.events if e.session_id == session_id]

    async def wait_for(self, predicate: Any, timeout: float = 5.0) -> None:
        async def loop() -> None:
            while not predicate():
                await self.changed.wait()

        await asyncio.wait_for(loop(), timeout)


def fake_named_pipes(loop: asyncio.AbstractEventLoop) -> None:
    """Back create_pipe_connection and start_serving_pipe (Windows
    ProactorEventLoop methods) with a Unix socket on other platforms, so the
    named pipe transport is exercised everywhere. Windows uses the real pipe."""
    directory = tempfile.mkdtemp(prefix="bmapipe")

    def path(address: str) -> str:
        return os.path.join(directory, address.rsplit("\\", 1)[-1])

    class Listener:
        def __init__(self, address: str, factory: Any) -> None:
            self.task = asyncio.ensure_future(loop.create_unix_server(factory, path(address)))

        def close(self) -> None:
            if self.task.done():
                self.task.result().close()
            else:
                self.task.add_done_callback(lambda task: task.result().close())

    loop.create_pipe_connection = lambda factory, address: loop.create_unix_connection(factory, path(address))  # type: ignore[attr-defined]
    async def start_serving_pipe(factory: Any, address: str) -> list[Listener]:
        return [Listener(address, factory)]

    loop.start_serving_pipe = start_serving_pipe  # type: ignore[attr-defined]
