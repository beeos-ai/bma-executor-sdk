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

def test_exact_receipt_bound_to_opening_input_and_revision():
    from bma_executor_sdk._input_skills import validate_input_skill_receipt
    from copy import deepcopy
    doc = {"skill_id":"skl_one","revision_id":"skr_one","body_sha256":"a"*64}
    params = {"input_id":"opening","config":{"input_skills":[doc]}}
    result = {"effective":{"input_skill_receipt":{"input_id":"opening","skills":[doc]}}}
    validate_input_skill_receipt(params,result)
    invalid = [{}, {"effective":{"input_skill_receipt":{"input_id":"other","skills":[doc]}}}, {"effective":{"input_skill_receipt":{"input_id":"opening","skills":[]}}}]
    changed = deepcopy(result);changed["effective"]["input_skill_receipt"]["skills"][0]["revision_id"] = "skr_other";invalid.append(changed)
    for receipt in invalid:
        with pytest.raises(RPCError):
            validate_input_skill_receipt(params,receipt)

async def test_receipt_refusal_unblocks_concurrent_close():
    import asyncio
    from types import SimpleNamespace
    started, release = asyncio.Event(), asyncio.Event()
    ran = []
    async def opened(params):
        started.set();await release.wait()
        return {"accepted":True,"turn_id":"t","effective":{}}
    async def run(turn):
        ran.append(True)
    async def close(sid,reason):
        pass
    async def call(*args):
        return {"accepted":True}
    tasks = []
    def spawn(work):
        task=asyncio.create_task(work);tasks.append(task);return task
    client=SimpleNamespace(_sessions=set(),_spawn=spawn,_registered=asyncio.Event(),_call=call,_log=lambda *args:None)
    runtime=_AgentRuntime(client,SimpleNamespace(open_turn=opened,run_turn=run,close_session=close),{"enabled":True,"input_skills":True})
    params={"session_id":"s","turn_id":"t","input_id":"i","input":[{"type":"text","text":"x"}],"config":{"input_skills":[{"skill_id":"skl_one","revision_id":"skr_one","name":"One","body":"skill","body_sha256":"a"*64}]}}
    opening=asyncio.create_task(runtime.turn_start(params))
    await started.wait()
    closing=asyncio.create_task(runtime.session_close({"session_id":"s","reason":"test"}))
    # Wait for session.close's cancellation, proving it captured this turn.
    async def captured():
        while not runtime.sessions["s"].turn._canceled:
            await asyncio.sleep(0)
    await asyncio.wait_for(captured(),1)
    release.set()
    with pytest.raises(RPCError):
        await opening
    assert await asyncio.wait_for(closing,1)=={"closed":True}
    assert not ran
    await asyncio.gather(*tasks)
