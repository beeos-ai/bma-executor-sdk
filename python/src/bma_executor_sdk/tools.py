"""Typed tool handlers of the executor end (ADR 0012 §4.1).

One slot per built-in tool (``client.tools.bash``, ``client.tools.read`` ...)
with that tool's input type from the contract::

    @client.tools.bash.handler
    async def bash(input: BashInput, ctx: ToolContext) -> BashResult:
        proc = await asyncio.create_subprocess_shell(input["command"], cwd=input.get("cwd"), ...)
        out, err = await proc.communicate()
        return BashResult(stdout=out.decode(), stderr=err.decode(), exit_code=proc.returncode or 0)

A handler returns a string (one text block), content blocks, a
:class:`ToolResult`, or (bash) a :class:`BashResult` rendered as Go Core
renders it. ``raise ToolError(...)`` reports a tool-level error to the model
(outcome succeeded, is_error true); any other exception is outcome failed.
"""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any, Generic, Optional, TypeVar, Union

from .generated import (
    ArtifactRef,
    BashInput,
    ComputerActionInput,
    ComputerObserveInput,
    ContentBlock,
    EditInput,
    GlobInput,
    GrepInput,
    ListFilesInput,
    ReadInput,
    ToolPermission,
    WriteInput,
)

I = TypeVar("I")
O = TypeVar("O")

__all__ = [
    "BashResult",
    "ExecutorTools",
    "ToolContext",
    "ToolError",
    "ToolHandler",
    "ToolOutput",
    "ToolResult",
    "ToolSlot",
]


class ToolError(Exception):
    """Raised from a tool handler: a tool-level error reported to the model
    (outcome succeeded, is_error true), as Go Core reports a missing file.
    Same role as the Anthropic SDK ``ToolError``."""

    def __init__(self, content: Union[str, list[ContentBlock]]) -> None:
        super().__init__(content if isinstance(content, str) else "tool error")
        self.content: list[ContentBlock] = [{"type": "text", "text": content}] if isinstance(content, str) else content  # type: ignore[list-item]


@dataclass(frozen=True)
class ToolContext:
    session_id: str
    tool_call_id: str
    progress: Callable[[str], None]
    """Sends tool.progress for this call (best-effort)."""
    permission: Optional[ToolPermission] = None


@dataclass(frozen=True)
class ToolResult:
    """A complete result: content, a tool-level error flag and artifacts."""

    content: list[ContentBlock]
    is_error: bool = False
    artifacts: Optional[list[ArtifactRef]] = None


@dataclass(frozen=True)
class BashResult:
    """bash output, rendered as Go Core renders it: stdout, stderr, ``exit_code: N``."""

    stdout: str
    stderr: str
    exit_code: int


ToolOutput = Union[str, list[ContentBlock], ToolResult]
ToolHandler = Callable[[I, ToolContext], Awaitable[O]]


@dataclass
class ToolSlot(Generic[I, O]):
    """One built-in tool of the executor."""

    name: str
    _handlers: dict[str, Any] = field(repr=False)

    def handler(self, fn: ToolHandler[I, O]) -> ToolHandler[I, O]:
        """Serves tool.call for this tool (replacing an earlier handler);
        usable as a decorator."""
        self._handlers[self.name] = fn
        return fn


class ExecutorTools:
    """``client.tools.<name>``: one typed slot per built-in tool."""

    bash: ToolSlot[BashInput, Union[ToolOutput, BashResult]]
    read: ToolSlot[ReadInput, ToolOutput]
    write: ToolSlot[WriteInput, ToolOutput]
    edit: ToolSlot[EditInput, ToolOutput]
    glob: ToolSlot[GlobInput, ToolOutput]
    grep: ToolSlot[GrepInput, ToolOutput]
    list_files: ToolSlot[ListFilesInput, ToolOutput]
    computer_observe: ToolSlot[ComputerObserveInput, ToolOutput]
    computer_action: ToolSlot[ComputerActionInput, ToolOutput]

    def __init__(self) -> None:
        self._handlers: dict[str, Any] = {}
        for name in self.__class__.__annotations__:
            setattr(self, name, ToolSlot(name, self._handlers))

    def get(self, name: str) -> Optional[ToolHandler[Any, Any]]:
        return self._handlers.get(name)

    def registered(self, *names: str) -> bool:
        return any(n in self._handlers for n in names)


def render(output: Union[ToolOutput, BashResult]) -> dict[str, Any]:
    """A handler's output as tool.result fields (bash as Go Core's bashResult)."""
    if isinstance(output, str):
        return {"outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": output}]}
    if isinstance(output, BashResult):
        content: list[dict[str, Any]] = []
        if output.stdout:
            content.append({"type": "text", "text": output.stdout})
        if output.stderr:
            content.append({"type": "text", "text": output.stderr})
        content.append({"type": "text", "text": f"exit_code: {output.exit_code}"})
        return {"outcome": "succeeded", "is_error": False, "content": content}
    if isinstance(output, ToolResult):
        result: dict[str, Any] = {"outcome": "succeeded", "is_error": output.is_error, "content": list(output.content)}
        if output.artifacts is not None:
            result["artifacts"] = list(output.artifacts)
        return result
    return {"outcome": "succeeded", "is_error": False, "content": list(output)}
