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

INLINE_RESULT_BYTES = 48 * 1024
"""Largest bash output returned inline, as Go Core's DefaultCoreInlineResultBytes."""

REQUIRED_TOOL_INPUT: dict[str, tuple[str, ...]] = {
    "bash": ("command",),
    "read": ("path",),
    "write": ("path", "content"),
    "edit": ("path", "old_string", "new_string"),
    "glob": ("pattern",),
    "grep": ("pattern",),
    "list_files": (),
}
"""Required members of each tool input (schemas/tool.json <Tool>Input.required)."""


def missing_required(tool: str, input: Any) -> Optional[str]:
    """The first required member that is absent or null (or an empty bash
    command, as Go Core rejects), else None."""
    members: dict[str, Any] = input if isinstance(input, dict) else {}
    for name in REQUIRED_TOOL_INPUT.get(tool, ()):
        value = members.get(name)
        if value is None or (name == "command" and value == ""):
            return name
    return None

__all__ = [
    "INLINE_RESULT_BYTES",
    "REQUIRED_TOOL_INPUT",
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
    """bash output, rendered as Go Core renders it: stdout, stderr, ``exit_code: N``.
    Output beyond INLINE_RESULT_BYTES, or marked ``truncated`` by the handler,
    is cut to Core's preview (half the limit of stdout, a quarter of stderr)."""

    stdout: str
    stderr: str
    exit_code: int
    truncated: bool = False
    """The handler already cut the output short."""


ToolOutput = Union[str, list[ContentBlock], ToolResult]
ToolHandler = Callable[[I, ToolContext], Awaitable[O]]


@dataclass
class ToolSlot(Generic[I, O]):
    """One built-in tool of the executor."""

    name: str
    _tools: "ExecutorTools" = field(repr=False)

    def handler(self, fn: ToolHandler[I, O]) -> ToolHandler[I, O]:
        """Serves tool.call for this tool (replacing an earlier handler);
        usable as a decorator. After ``start()`` the changed capabilities are
        sent with executor.capabilities.update."""
        self._tools._handlers[self.name] = fn
        self._tools._changed()
        return fn


class ExecutorTools:
    """``client.tools.<name>``: one typed slot per built-in tool.
    computer_observe and computer_action get one once the contract defines
    their input members."""

    bash: ToolSlot[BashInput, Union[ToolOutput, BashResult]]
    read: ToolSlot[ReadInput, ToolOutput]
    write: ToolSlot[WriteInput, ToolOutput]
    edit: ToolSlot[EditInput, ToolOutput]
    glob: ToolSlot[GlobInput, ToolOutput]
    grep: ToolSlot[GrepInput, ToolOutput]
    list_files: ToolSlot[ListFilesInput, ToolOutput]

    def __init__(self, on_change: Callable[[], None] = lambda: None) -> None:
        self._handlers: dict[str, Any] = {}
        self._changed = on_change
        for name in ExecutorTools.__annotations__:
            setattr(self, name, ToolSlot(name, self))

    def get(self, name: str) -> Optional[ToolHandler[Any, Any]]:
        return self._handlers.get(name)

    def registered(self, *names: str) -> bool:
        return any(n in self._handlers for n in names)


def render(output: Union[ToolOutput, BashResult]) -> dict[str, Any]:
    """A handler's output as tool.result fields (bash as Go Core's bashResult)."""
    if isinstance(output, str):
        return {"outcome": "succeeded", "is_error": False, "content": [{"type": "text", "text": output}]}
    if isinstance(output, BashResult):
        exit = f"exit_code: {output.exit_code}"
        out, err = output.stdout.encode(), output.stderr.encode()
        if not output.truncated and len(out) + len(err) + len(exit) <= INLINE_RESULT_BYTES:
            content: list[dict[str, Any]] = []
            if output.stdout:
                content.append({"type": "text", "text": output.stdout})
            if output.stderr:
                content.append({"type": "text", "text": output.stderr})
            content.append({"type": "text", "text": exit})
            return {"outcome": "succeeded", "is_error": False, "content": content}
        return {"outcome": "succeeded", "is_error": False, "content": [
            {"type": "text", "text": out[: INLINE_RESULT_BYTES // 2].decode(errors="ignore")},
            {"type": "text", "text": err[: INLINE_RESULT_BYTES // 4].decode(errors="ignore")},
            {"type": "text", "text": f"{exit} (output truncated to fit the result; {len(out) + len(err)} bytes total)"},
        ]}
    if isinstance(output, ToolResult):
        result: dict[str, Any] = {"outcome": "succeeded", "is_error": output.is_error, "content": list(output.content)}
        if output.artifacts is not None:
            result["artifacts"] = list(output.artifacts)
        return result
    return {"outcome": "succeeded", "is_error": False, "content": list(output)}
