"""Unsupported input controls fail before any session or handler is touched."""
import pytest
from bma_executor_sdk.agent import _AgentRuntime
from bma_executor_sdk.generated import JsonRpcErrorCode
from bma_executor_sdk._rpc import RPCError

async def test_unsupported_selected_skills_refused_before_handler():
    runtime = _AgentRuntime(None, None, {"enabled": True})
    params = {"session_id":"sesn_test","turn_id":"turn_test","input_id":"in_test","input":[{"type":"text","text":"test"}],"config":{"input_skills":[{"skill_id":"skl_test","revision_id":"skr_v1","name":"Review","body":"skill","body_sha256":"a"*64}]}}
    with pytest.raises(RPCError) as failure:
        await runtime.turn_start(params)
    assert failure.value.code == JsonRpcErrorCode.CapabilityUnavailable
    params["config"] = {"input_skill_receipt":{"input_id":"in_test","skills":[]}}
    with pytest.raises(RPCError) as failure:
        await runtime.turn_start(params)
    assert failure.value.code == JsonRpcErrorCode.InvalidParams


@pytest.mark.parametrize("value", [{}, [], None, False, [None], [{"skill_id":"skl_only"}]])
async def test_malformed_selected_skills_refused_before_handler(value):
    runtime = _AgentRuntime(None, None, {"enabled": True})
    with pytest.raises(RPCError) as failure:
        await runtime.turn_start({"config":{"input_skills":value}})
    assert failure.value.code == JsonRpcErrorCode.InvalidParams

async def test_null_receipt_is_still_effective_only():
    runtime = _AgentRuntime(None, None, {"enabled": True})
    with pytest.raises(RPCError) as failure:
        await runtime.turn_start({"config":{"input_skill_receipt":None}})
    assert failure.value.code == JsonRpcErrorCode.InvalidParams

async def test_selected_skill_turn_refuses_another_input():
    from bma_executor_sdk.agent import AgentTurn, _AgentSession
    runtime = _AgentRuntime(None, None, {"enabled": True, "input_skills":True})
    session = _AgentSession(runtime,"s")
    session.turn = AgentTurn(session,{"turn_id":"t","input_id":"opening","config":{"input_skills":[{}]}})
    runtime.sessions["s"] = session
    with pytest.raises(RPCError) as failure:
        await runtime.turn_input({"session_id":"s","turn_id":"t","input_id":"other"})
    assert failure.value.code == JsonRpcErrorCode.Conflict
    assert await runtime.turn_input({"session_id":"s","turn_id":"t","input_id":"opening"}) == {"accepted":True}
