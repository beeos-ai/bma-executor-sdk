"""BMA executor protocol (bma.executor.v1.1): executor, harness and agent ends.

Install with ``pip install 'bma-sdk[executor]'`` (adds ``websockets``).
Protocol types are generated from the BMA contract into
:mod:`bma_sdk.executor.generated` (TypedDicts: plain JSON dicts on the wire).
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
from .harness import (
    AssignRejectedError,
    Executor,
    Harness,
    HarnessMCPChannel,
    Principal,
    Session,
    SessionEndedError,
    ToolOutcomeUnknownError,
    Unauthorized,
)
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
    "ChannelClosed",
    "ClientStoppedError",
    "ConnectRequest",
    "Connection",
    "Dialer",
    "Executor",
    "ExecutorClient",
    "ExecutorError",
    "ExecutorTokenError",
    "ExecutorTokenScopeError",
    "Harness",
    "HarnessMCPChannel",
    "LogEvent",
    "MCPChannel",
    "NotConnectedError",
    "Principal",
    "RPCError",
    "Session",
    "SessionEndedError",
    "ToolOutcomeUnknownError",
    "Unauthorized",
    "dial",
    "generated",
    "static_token",
]
