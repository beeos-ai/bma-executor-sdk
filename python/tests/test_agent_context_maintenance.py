"""The Python runtime must not advertise or silently ignore unsupported maintenance."""
import pytest
from bma_executor_sdk.agent import _AgentRuntime
from bma_executor_sdk.client import ExecutorClient
from bma_executor_sdk.generated import JsonRpcErrorCode, METHOD_SESSION_COMPACT, METHOD_SESSION_COMPACT_CANCEL
from bma_executor_sdk._rpc import RPCError

@pytest.mark.parametrize("method", [METHOD_SESSION_COMPACT, METHOD_SESSION_COMPACT_CANCEL])
async def test_context_methods_refuse_before_any_session_or_handler(method):
    runtime = _AgentRuntime(None, None, {"enabled": True})
    with pytest.raises(RPCError) as failure:
        await runtime.methods[method]({"session_id": "s", "operation_id": "o"})
    assert failure.value.code == JsonRpcErrorCode.CapabilityUnavailable
    assert runtime.sessions == {}

def test_cannot_advertise_unsupported_context_capability():
    # Call through the public method; no transport or handler is touched.
    with pytest.raises(ValueError, match="context maintenance"):
        ExecutorClient.serve_agent(None, None, {"enabled": True, "context_maintenance": True})
