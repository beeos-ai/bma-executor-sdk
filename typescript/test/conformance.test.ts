// The contract's conformance examples (contracts/bma/executor/examples)
// against the TypeScript runtime: every example names a generated method of
// the right kind, every request is accepted by the TS end that receives it,
// and every answer the TS runtime produces carries the members of the
// example result.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { test } from 'node:test'

import {
  ExecutorClient,
  Harness,
  inProcessPair,
  JsonRpcErrorCode,
  methods,
  RpcError,
  type AgentPlatform,
  type MethodName,
  type TurnStartResult,
} from '../src/index.js'
import { RpcPeer } from '../src/rpc.js'
import { executorInfo, waitFor } from './helpers.js'

interface Example {
  name: string
  method: MethodName
  kind: 'request' | 'notification' | 'result'
  message?: { jsonrpc: string; id?: string; method: string; params: Record<string, unknown> }
  result?: Record<string, unknown>
}

const fixtures = JSON.parse(
  readFileSync(resolve(process.cwd(), '../../contracts/bma/executor/examples/conformance.json'), 'utf8'),
) as { protocol: string; messages: Example[] }

function request(name: string): Example['message'] & {} {
  const example = fixtures.messages.find((candidate) => candidate.name === name)
  assert.ok(example?.message, `missing example ${name}`)
  return example.message
}

function resultOf(method: string, name = `${method} result`): Record<string, unknown> | undefined {
  return fixtures.messages.find((candidate) => candidate.kind === 'result' && candidate.name === name)?.result
}

// Optional result members the TS harness does not set (limits are server policy).
const optionalMembers: Record<string, string[]> = { 'executor.register': ['limits'] }

/** Our answer must carry every member of the example result, with the same JSON type. */
function assertShape(method: string, actual: unknown, name?: string): void {
  const expected = resultOf(method, name)
  assert.ok(expected, `no result example for ${method}`)
  assert.equal(typeof actual, 'object', `${method}: answer is not an object`)
  for (const [key, value] of Object.entries(expected)) {
    if (optionalMembers[method]?.includes(key)) continue
    const got = (actual as Record<string, unknown>)[key]
    assert.ok(got !== undefined, `${method}: answer lacks ${key}`)
    assert.equal(Array.isArray(got), Array.isArray(value), `${method}.${key}: array mismatch`)
    assert.equal(typeof got, typeof value, `${method}.${key}: type mismatch`)
  }
}

/** A raw peer that answers every request with its example result. */
function examplePeer(conn: ReturnType<typeof inProcessPair>[number], idPrefix: string) {
  const received: string[] = []
  const peer = new RpcPeer(conn, {
    idPrefix,
    requestTimeoutMs: 5000,
    onRequest: (method) => {
      received.push(method)
      return resultOf(method) ?? { accepted: true }
    },
    onNotification: (method) => received.push(method),
    onClose: () => {},
  })
  return { peer, received }
}

test('every example names a generated method of the same kind', () => {
  assert.equal(fixtures.protocol, 'bma.executor.v1.1')
  for (const example of fixtures.messages) {
    const spec = methods[example.method]
    assert.ok(spec, `${example.name}: unknown method`)
    if (example.kind === 'result') assert.equal(spec.kind, 'request', example.name)
    else {
      assert.equal(spec.kind, example.kind, example.name)
      assert.equal(example.message?.id === undefined, example.kind === 'notification', example.name)
    }
  }
})

test('executor-to-harness examples against the TypeScript harness', async () => {
  const events: string[] = []
  const platform: AgentPlatform = {
    ackedSequence: async () => 3,
    agentEvent: async (_executor, event) => void events.push(event.eventId),
    agentDelta: (_executor, delta) => void events.push(`delta:${delta.event_id}`),
    interactionRequested: async () => 'aint_01JAGENT',
    interactionCanceled: async () => {},
  }
  const harness = new Harness({ agent: platform })

  // v1 executors register with protocol bma.executor.v1 on a v1 channel.
  {
    const [executorEnd, harnessEnd] = inProcessPair('bma.executor.v1')
    harness.accept(harnessEnd)
    const { peer } = examplePeer(executorEnd, 'v1_')
    assertShape('executor.register', await peer.call('executor.register', request('executor.register v1 request').params))
    peer.close()
  }

  const [executorEnd, harnessEnd] = inProcessPair('bma.executor.v1.1')
  harness.accept(harnessEnd)
  const { peer, received } = examplePeer(executorEnd, 'x_')
  const call = (name: string) => {
    const message = request(name)
    return peer.call(message.method, message.params)
  }
  const notify = (name: string) => {
    const message = request(name)
    peer.notify(message.method, message.params)
  }

  assertShape('executor.register', await call('executor.register request'))
  assertShape('executor.heartbeat', await call('executor.heartbeat request'))
  notify('executor.capabilities.update notification')
  const executor = await harness.waitExecutor()
  await waitFor(() => executor.capabilities.process?.enabled === true)

  // Bind the session and dispatch the call the executor examples refer to.
  const session = await executor.assignSession(request('session.assign request').params as never)
  assert.ok(received.includes('session.assign'))
  assertShape('session.ready', await call('session.ready request'))
  assertShape('session.event', await call('session.event request'))
  notify('session.status notification')
  const toolCall = request('tool.call request').params
  const pending = session.callTool(toolCall as never)
  await waitFor(() => received.includes('tool.call'))
  notify('tool.progress notification')
  assertShape('tool.result', await call('tool.result request'))
  assert.equal((await pending).outcome, 'succeeded')

  // Agent capability traffic.
  assertShape('session.event', await call('session.event agent request'), 'session.event agent result')
  await assert.rejects(call('session.event agent idle request'), (error: unknown) => {
    // Sequence 9 after acked 4 is a gap: conflict with the expected sequence.
    return error instanceof RpcError && error.code === JsonRpcErrorCode.Conflict && error.data?.expected_sequence === 5
  })
  notify('session.event_delta notification')
  assertShape('interaction.request', await call('interaction.request request'))
  assertShape('interaction.cancel', await call('interaction.cancel request'))
  await waitFor(() => events.includes('delta:evt_0003'))
  assert.ok(events.includes('evt_0004'))

  assertShape('session.ended', await call('session.ended request'))
  assert.equal(session.endReason, 'stopped')
  notify('executor.close notification')
  await waitFor(() => !harness.executors().includes(executor))
})

test('harness-to-executor examples against the TypeScript executor', async () => {
  const [executorEnd, harnessEnd] = inProcessPair('bma.executor.v1.1')
  const { peer, received } = examplePeer(harnessEnd, 'h_')
  const client = new ExecutorClient({ environmentId: '', executor: executorInfo, dialer: async () => executorEnd, reconnect: false })
  client.handleTool('bash', async () => ({ outcome: 'succeeded', is_error: false, content: [{ type: 'text', text: 'hi\n' }] }))
  client.serveAgent({
    openTurn: async () => resultOf('turn.start') as unknown as TurnStartResult,
    runTurn: (_turn, signal) => new Promise((resolve) => signal.addEventListener('abort', () => resolve(), { once: true })),
    closeSession: async () => {},
  })
  // Process, terminal and skill methods are not wrapped; adapters serve them with onRequest.
  const answers = new Map<string, unknown>()
  for (const example of fixtures.messages) {
    if (/^(process|terminal|skill)\./.test(example.method) && example.kind === 'request' && example.message?.id) {
      const answer = resultOf(example.method, `${example.name.replace(/ request$/, '')} result`) ?? resultOf(example.method)
      answers.set(JSON.stringify(example.message.params), answer)
      client.onRequest(example.method, (params) => answers.get(JSON.stringify(params)))
    }
  }
  await client.start()
  assert.ok(received.includes('executor.register'))
  const call = (name: string) => {
    const message = request(name)
    return peer.call(message.method, message.params)
  }
  try {
    assertShape('session.assign', await call('session.assign request'))
    assertShape('tool.call', await call('tool.call request'))
    await waitFor(() => received.includes('tool.result'))
    assertShape('tool.cancel', await call('tool.cancel request'))

    assertShape('turn.start', await call('turn.start request'))
    await waitFor(() => received.includes('session.event'))
    assertShape('turn.input', await call('turn.input request'))
    assertShape('interaction.response', await call('interaction.response request'))
    assertShape('turn.cancel', await call('turn.cancel request'))
    assertShape('session.close', await call('session.close request'))
    await waitFor(() => received.includes('session.ended'))

    for (const example of fixtures.messages) {
      if (!/^(process|terminal|skill)\./.test(example.method) || example.kind !== 'request' || !example.message?.id) continue
      if (methods[example.method].direction !== 'harness-to-executor') continue
      const answer = await call(example.name)
      const named = `${example.name.replace(/ request$/, '')} result`
      assertShape(example.method, answer, resultOf(example.method, named) ? named : undefined)
    }

    assertShape('session.stop', await call('session.stop request'))
    assertShape('executor.drain', await call('executor.drain request'))
  } finally {
    client.stop()
  }
})
