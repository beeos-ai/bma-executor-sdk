"""BMA executor protocol (bma.executor.v1.1): executor, harness and agent ends.

Install with ``pip install bma-executor-sdk``.
Protocol types are generated from the BMA contract into
:mod:`bma_executor_sdk.generated` (TypedDicts: plain JSON dicts on the wire).
"""

from . import generated
from ._rpc import ClientStoppedError, ExecutorError, NotConnectedError, RPCError
from .agent import AgentEventRecord, AgentHandler, AgentPlatform, AgentSessionClosedError, AgentTurn
from .client import (
    DEFAULT_BASE_URL,
    PROTOCOL_V1,
    PROTOCOL_V11,
    ExecutorClient,
    ExecutorTokenError,
    ExecutorTokenScopeError,
    LogEvent,
    MCPChannel,
    static_token,
)
from .generated import (
    BashInput,
    ComputerActionInput,
    ComputerObserveInput,
    EditInput,
    GlobInput,
    GrepInput,
    ListFilesInput,
    ReadInput,
    WriteInput,
)
from .harness import (
    AssignRejectedError,
    Executor,
    Harness,
    HarnessMCPChannel,
    HarnessTools,
    HarnessToolSlot,
    Principal,
    Session,
    SessionEndedError,
    ToolOutcomeUnknownError,
    Unauthorized,
)
from .tools import BashResult, ExecutorTools, ToolContext, ToolError, ToolHandler, ToolOutput, ToolResult, ToolSlot
from .transport import (
    TERMINAL_CLOSE_CODES,
    ChannelClosed,
    ConnectRequest,
    Connection,
    Dialer,
    dial,
)

__all__ = [
    "DEFAULT_BASE_URL",
    "PROTOCOL_V1",
    "PROTOCOL_V11",
    "TERMINAL_CLOSE_CODES",
    "AgentEventRecord",
    "AgentHandler",
    "AgentPlatform",
    "AgentSessionClosedError",
    "AgentTurn",
    "AssignRejectedError",
    "BashInput",
    "BashResult",
    "ChannelClosed",
    "ClientStoppedError",
    "ComputerActionInput",
    "ComputerObserveInput",
    "ConnectRequest",
    "Connection",
    "Dialer",
    "EditInput",
    "Executor",
    "ExecutorClient",
    "ExecutorError",
    "ExecutorTokenError",
    "ExecutorTokenScopeError",
    "ExecutorTools",
    "GlobInput",
    "GrepInput",
    "Harness",
    "HarnessMCPChannel",
    "HarnessToolSlot",
    "HarnessTools",
    "ListFilesInput",
    "LogEvent",
    "MCPChannel",
    "NotConnectedError",
    "Principal",
    "RPCError",
    "ReadInput",
    "Session",
    "SessionEndedError",
    "ToolContext",
    "ToolError",
    "ToolHandler",
    "ToolOutcomeUnknownError",
    "ToolOutput",
    "ToolResult",
    "ToolSlot",
    "Unauthorized",
    "WriteInput",
    "dial",
    "generated",
    "static_token",
]
