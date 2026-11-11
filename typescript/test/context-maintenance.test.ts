import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { ExecutorClient, Harness, JsonRpcErrorCode, type AgentHandler, type SessionCompactParams, type SessionCompactResult, type SessionCompactCancelParams, type SessionCompactCancelResult } from '../src/index.js'
import { executorInfo, waitFor } from './helpers.js'

const params: SessionCompactParams = { session_id: 'session_compact', operation_id: 'context_once', expected_context_version: 0, expected_last_input_id: 'input_source', model: 'model-1', deadline_ms: 10000, metadata: {} }
const result = (p: SessionCompactParams): SessionCompactResult => ({ session_id: p.session_id, operation_id: p.operation_id, previous_context_version: p.expected_context_version, context_version: p.expected_context_version + 1, summary: 'Prior decision and file references retained.', summary_sha256: createHash('sha256').update('Prior decision and file references retained.').digest('hex'), source_event_count: 12 })
const cancelArgs=(operation_id=params.operation_id):SessionCompactCancelParams=>({session_id:params.session_id,operation_id,expected_context_version:params.expected_context_version,expected_last_input_id:params.expected_last_input_id,model:params.model})
const canceled=(operation_id=params.operation_id,settled=true):SessionCompactCancelResult=>({session_id:params.session_id,operation_id,canceled:true,settled})
const code = (expected: number) => (error: unknown) => !!error && typeof error === 'object' && 'code' in error && error.code === expected

async function setup(t: import('node:test').TestContext, compact?: AgentHandler['compactSession'], cancel?: AgentHandler['cancelCompactSession']) {
  const events: unknown[] = []
  let ordinary = 0
  const harness = new Harness({ agent: {
    async ackedSequence() { return events.length },
    async agentEvent(_executor, event) { events.push(event) },
    agentDelta() {},
    async interactionRequested() { throw new Error('unexpected interaction') },
    async interactionCanceled() {},
  } })
  const client = new ExecutorClient({ environmentId: 'env_test', executor: executorInfo, dialer: harness.inProcessDialer() })
  client.serveAgent({
    async openTurn(p) { ordinary++; return { accepted: true, turn_id: p.turn_id, executor_session_ref:p.session_id, resumed:true, effective: {} } },
    async runTurn() {}, async closeSession() {},
    ...(compact ? { compactSession: compact, cancelCompactSession: cancel ?? (async p=>canceled(p.operation_id)) } : {}),
  }, { ...(compact ? { context_maintenance: true } : {}) })
  await client.start()
  t.after(() => client.stop())
  const executor = await harness.waitExecutor()
  return { executor, events, ordinary: () => ordinary }
}

test('explicit context RPC returns a verified receipt without an ordinary input or turn', async t => {
  let calls = 0
  const env = await setup(t, async p => { calls++; return result(p) })
  assert.equal(env.executor.agentCapability?.context_maintenance, true)
  assert.deepEqual(await env.executor.call('session.compact', params), result(params))
  assert.equal(calls, 1)
  assert.equal(env.ordinary(), 0)
  assert.deepEqual(env.events, [])
  for (const change of [{ expected_context_version: -1 }, { deadline_ms: 300001 }, { expected_last_input_id: '' }, { operation_id: 'bad/path' }]) {
    await assert.rejects(env.executor.call('session.compact', { ...params, ...change }), code(JsonRpcErrorCode.InvalidParams))
  }
  assert.equal(calls, 1)
})

test('maintenance reserves before waiting, shares an exact pending retry and rejects competing work', async t => {
  let calls = 0
  let release!: () => void
  const held = new Promise<void>(resolve => { release = resolve })
  const env = await setup(t, async p => { calls++; await held; return result(p) })
  const first = env.executor.call('session.compact', params)
  await waitFor(() => calls === 1)
  const retry = env.executor.call('session.compact', { ...params, metadata: { model_token: 'rotated-synthetic' } })
  await assert.rejects(env.executor.call('session.compact', { ...params, expected_last_input_id: 'different_source' }), code(JsonRpcErrorCode.Conflict))
  await assert.rejects(env.executor.startTurn({ session_id: params.session_id, turn_id: 'competing_turn', input_id: 'competing_input', input: [{ type: 'text', text: 'must not run' }] }), code(JsonRpcErrorCode.Conflict))
  release()
  assert.deepEqual(await first, await retry)
  assert.equal(calls, 1)
  assert.equal(env.ordinary(), 0)
  assert.deepEqual(env.events, [])
})

test('cancel targets only its operation and retains exclusion until the handler settles', async t => {
  let started = false, observedAbort = false
  let settle!: () => void
  const held = new Promise<void>(resolve => { settle = resolve })
  const env = await setup(t, async (_p, signal) => {
    started = true
    await new Promise<void>(resolve => signal.addEventListener('abort', () => { observedAbort = true; resolve() }, { once: true }))
    await held
    throw signal.reason
  })
  const pending = env.executor.call('session.compact', params)
  // Attach rejection before requesting cancellation, avoiding an unhandled RPC.
  const rejected = assert.rejects(pending)
  await waitFor(() => started)
  assert.deepEqual(await env.executor.call('session.compact_cancel', cancelArgs('another')), canceled('another'))
  assert.equal(observedAbort, false)
  assert.deepEqual(await env.executor.call('session.compact_cancel', cancelArgs()), canceled(params.operation_id,false))
  await waitFor(() => observedAbort)
  await assert.rejects(env.executor.call('session.compact', { ...params, operation_id: 'new_while_aborting' }), code(JsonRpcErrorCode.Conflict))
  settle()
  await rejected
  assert.deepEqual(await env.executor.call('session.compact_cancel', cancelArgs()), canceled())
  assert.deepEqual(env.events, [])
})

test('unsupported agents explicitly reject maintenance and malformed receipts never claim success', async t => {
  const old = await setup(t)
  await assert.rejects(old.executor.call('session.compact', params), code(JsonRpcErrorCode.CapabilityUnavailable))
  await assert.rejects(old.executor.call('session.compact_cancel', cancelArgs()), code(JsonRpcErrorCode.CapabilityUnavailable))
  const bad = await setup(t, async p => ({ ...result(p), summary_sha256: 'a'.repeat(64) }))
  await assert.rejects(bad.executor.call('session.compact', params), code(JsonRpcErrorCode.InvalidParams))
  assert.deepEqual(bad.events, [])
})


test('cancel reconciles a durable applied receipt after SDK restart without an ordinary turn',async t=>{
 const env=await setup(t,async p=>result(p),async p=>({session_id:p.session_id,operation_id:p.operation_id,canceled:false,settled:false,receipt:result(params)}))
 assert.deepEqual(await env.executor.call('session.compact_cancel',cancelArgs()),{session_id:params.session_id,operation_id:params.operation_id,canceled:false,settled:true,receipt:result(params)})
 assert.equal(env.ordinary(),0);assert.deepEqual(env.events,[])
 for(const change of [{expected_context_version:-1},{expected_last_input_id:''},{model:''}])await assert.rejects(env.executor.call('session.compact_cancel',{...cancelArgs(),...change}),code(JsonRpcErrorCode.InvalidParams))
})

test('cancel cannot claim rollback without a fence, or claim application with a corrupt receipt',async t=>{
 const env=await setup(t,async p=>result(p),async p=>({session_id:p.session_id,operation_id:p.operation_id,canceled:false,settled:true}))
 await assert.rejects(env.executor.call('session.compact_cancel',cancelArgs()),code(JsonRpcErrorCode.InvalidParams))
 const bad=await setup(t,async p=>result(p),async p=>({session_id:p.session_id,operation_id:p.operation_id,canceled:false,settled:true,receipt:{...result(params),summary_sha256:'a'.repeat(64)}}))
 await assert.rejects(bad.executor.call('session.compact_cancel',cancelArgs()),code(JsonRpcErrorCode.InvalidParams))
 const nullReceipt=await setup(t,async p=>result(p),async()=>({...canceled(),receipt:null} as unknown as SessionCompactCancelResult))
 await assert.rejects(nullReceipt.executor.call('session.compact_cancel',cancelArgs()),code(JsonRpcErrorCode.InvalidParams))
})

test('the public capability cannot be advertised without durable cancellation',()=>{
 const client=new ExecutorClient({environmentId:'env_test',executor:executorInfo,dialer:new Harness().inProcessDialer()})
 assert.throws(()=>client.serveAgent({async openTurn(p){return {accepted:true,turn_id:p.turn_id,executor_session_ref:p.session_id,resumed:true,effective:{}}},async runTurn(){},async closeSession(){},async compactSession(p){return result(p)}},{context_maintenance:true}),/cancelCompactSession/)
})
