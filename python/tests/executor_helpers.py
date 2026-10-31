"""Shared fixtures for the executor tests: an in-memory AgentPlatform."""

from __future__ import annotations

import asyncio
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
