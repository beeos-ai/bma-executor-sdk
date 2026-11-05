"""Static type assertions for typed tool handlers, checked by pyright
(strict): every line marked ``pyright: ignore[...]`` must be an error
(reportUnnecessaryTypeIgnoreComment)."""

from __future__ import annotations

from typing_extensions import assert_type

from bma_executor_sdk import BashInput, BashResult, ExecutorClient, ReadInput, Session, ToolContext
from bma_executor_sdk.generated import ToolResultParams


def register(client: ExecutorClient) -> None:
    @client.tools.bash.handler
    async def bash(input: BashInput, ctx: ToolContext) -> BashResult:  # pyright: ignore[reportUnusedFunction]
        assert_type(input["command"], str)
        return BashResult(stdout="", stderr="", exit_code=0)

    @client.tools.read.handler
    async def read(input: ReadInput, ctx: ToolContext) -> str:  # pyright: ignore[reportUnusedFunction]
        return input["path"]

    async def wrong(input: BashInput, ctx: ToolContext) -> str:
        return ""

    client.tools.read.handler(wrong)  # pyright: ignore[reportArgumentType]

    async def bash_result_from_read(input: ReadInput, ctx: ToolContext) -> BashResult:
        return BashResult(stdout="", stderr="", exit_code=0)

    client.tools.read.handler(bash_result_from_read)  # pyright: ignore[reportArgumentType]
    client.tools.nope  # pyright: ignore[reportAttributeAccessIssue, reportUnknownMemberType]


async def call(session: Session) -> None:
    result = await session.tools.read.call({"path": "a"})
    assert_type(result, ToolResultParams)
    await session.tools.read.call({})  # pyright: ignore[reportArgumentType]
    await session.tools.bash.call({"path": "x"})  # pyright: ignore[reportArgumentType]
