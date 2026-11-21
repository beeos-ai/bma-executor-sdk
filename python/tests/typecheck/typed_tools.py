"""Static type assertions for typed tool handlers, checked by pyright
(strict): every line marked ``pyright: ignore[...]`` must be an error
(reportUnnecessaryTypeIgnoreComment)."""

from __future__ import annotations

from typing_extensions import assert_type

from bma_executor_sdk import BashInput, BashResult, ExecutorClient, ReadInput, Session, ToolContext
from bma_executor_sdk.generated import ToolResultParams


def register(client: ExecutorClient) -> None:
    @client.tools.bash.handler
    async def bash(input: BashInput, ctx: ToolContext) -> BashResult:
        assert_type(input["command"], str)
        return BashResult(stdout="", stderr="", exit_code=0)

    @client.tools.read.handler
    async def read(input: ReadInput, ctx: ToolContext) -> str:
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


async def typed_methods(client: ExecutorClient) -> None:
    from bma_executor_sdk import methods
    from bma_executor_sdk.generated import Ack, ProcessListParams, ProcessListResult

    async def process_list(params: ProcessListParams) -> ProcessListResult:
        return {"processes": []}

    client.on_request(methods.PROCESS_LIST, process_list)

    async def wrong_result(params: ProcessListParams) -> Ack:
        return {"accepted": True}

    client.on_request(methods.PROCESS_LIST, wrong_result)  # pyright: ignore[reportArgumentType]
    client.on_request("process.list", process_list)  # pyright: ignore[reportArgumentType]
    ack = await client.call(methods.SESSION_READY, {"session_id": "s", "work_id": "w", "lease_token": "t"})
    assert_type(ack, Ack)
    await client.call(methods.SESSION_READY, {"nope": 1})  # pyright: ignore[reportArgumentType]
    await client.notify(methods.TOOL_PROGRESS, {"session_id": "s", "tool_call_id": "t", "message": "m"})
    client.tools.computer_action  # pyright: ignore[reportAttributeAccessIssue, reportUnknownMemberType]
