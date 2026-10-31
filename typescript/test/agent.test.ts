// Agent capability: TypeScript agent executor against the TypeScript harness
// with an in-memory platform.
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
  ExecutorClient,
  Harness,
  JsonRpcErrorCode,
  RpcError,
  staticToken,
  webSocketDialer,
  type AgentEventRecord,
  type AgentHandler,
  type AgentPlatform,
  type Dialer,
  type HarnessExecutor,
  type InteractionRequestParams,
  type SessionEventDeltaParams,
} from '../src/index.js'
import { executorInfo, waitFor } from './helpers.js'

class MemoryPlatform implements AgentPlatform {
  events: AgentEventRecord[] = []
  deltas: SessionEventDeltaParams[] = []
  interactions: InteractionRequestParams[] = []
  canceled: string[] = []
  failNext = 0
  executor?: HarnessExecutor

  async ackedSequence(sessionId: string): Promise<number> {
    return this.events.filter((event) => event.sessionId === sessionId).length
  }
  async agentEvent(_executor: HarnessExecutor, event: AgentEventRecord): Promise<void> {
    if (this.failNext > 0) {
      this.failNext--
      throw new Error('store unavailable')
    }
    this.events.push(event)
  }
  agentDelta(_executor: HarnessExecutor, delta: SessionEventDeltaParams): void {
    this.deltas.push(delta)
  }
  async interactionRequested(executor: HarnessExecutor, request: InteractionRequestParams): Promise<string> {
    this.interactions.push(request)
    // The user allows the tool shortly after the platform opened the interaction.
    setTimeout(() => {
      void executor.respondInteraction({ session_id: request.session_id, interaction_id: request.interaction_id, outcome: 'allow' })
    }, 5)
    return `pint_${this.interactions.length}`
  }
  async interactionCanceled(_executor: HarnessExecutor, cancel: { interaction_id: string }): Promise<void> {
    this.canceled.push(cancel.interaction_id)
  }
  types(sessionId: string): string[] {
    return this.events.filter((event) => event.sessionId === sessionId).map((event) => event.type)
  }
}

/** Echo agent: asks permission for a tool, waits for one turn.input, echoes it. "block" waits for cancel. */
function echoAgent(closed: string[]): AgentHandler {
  return {
    async openTurn(params) {
      return { accepted: true, turn_id: params.turn_id, executor_session_ref: `ref_${params.session_id}`, resumed: false, effective: {} }
    },
    async runTurn(turn, signal) {
      const text = turn.params.input[0]?.text ?? ''
      if (text === 'block') {
        await new Promise((resolve) => signal.addEventListener('abort', resolve, { once: true }))
        return
      }
      if (text === 'fail') throw new Error('model unavailable')
      const messageId = turn.newEventId()
      turn.delta(messageId, 'agent.message', 0, 'thinking…')
      turn.emit({ type: 'agent.tool_use', name: 'bash', input: { command: 'ls' } })
      const answer = await turn.requestInteraction({ kind: 'permission', tool_use_id: 'tu_1', tool_name: 'bash' }, signal)
      turn.emit({ type: 'agent.tool_result', tool_use_id: 'tu_1', content: [{ type: 'text', text: answer.outcome }] })
      const input = await turn.inputs.next()
      turn.emitWithId(messageId, { type: 'agent.message', content: [{ type: 'text', text: `echo:${input.value?.input[0]?.text}` }] })
    },
    async closeSession(sessionId) {
      closed.push(sessionId)
    },
  }
}

async function setup(platform: MemoryPlatform, dialer: (harness: Harness) => Promise<{ dialer: Dialer; close(): Promise<void> }>) {
  const ended: string[] = []
  const harness = new Harness({ agent: platform })
  const transport = await dialer(harness)
  const closed: string[] = []
  const client = new ExecutorClient({ environmentId: 'env_test', executor: executorInfo, dialer: transport.dialer, reconnectBaseDelayMs: 10 })
  client.serveAgent(echoAgent(closed), { models: [{ id: 'echo-1' }], interaction_kinds: ['permission'] })
  await client.start()
  const executor = await harness.waitExecutor()
  return {
    harness,
    client,
    executor,
    closed,
    ended,
    async close() {
      client.stop()
      await transport.close()
    },
  }
}

const inProcess = async (harness: Harness) => ({ dialer: harness.inProcessDialer(), close: async () => {} })
const webSocket = async (harness: Harness) => {
  const server = await harness.listen()
  return {
    dialer: webSocketDialer({ environmentId: 'env_test', executor: executorInfo, tokenSource: staticToken(server.url) }),
    close: server.close,
  }
}

const turnStart = (sessionId: string, turnId: string, text: string) => ({
  session_id: sessionId,
  turn_id: turnId,
  input_id: `in_${turnId}`,
  input: [{ type: 'text' as const, text }],
})

describe('agent capability', () => {
  test('turn with interaction, input, ordered acknowledged events and session.close', async () => {
    const platform = new MemoryPlatform()
    const env = await setup(platform, inProcess)
    try {
      assert.equal(env.executor.agentCapability?.enabled, true)
      assert.equal(env.executor.agentCapability?.models?.[0]?.id, 'echo-1')

      const started = await env.executor.startTurn(turnStart('as_1', 't_1', 'hi'))
      assert.deepEqual(started, { accepted: true, turn_id: 't_1', executor_session_ref: 'ref_as_1', resumed: false, effective: {} })
      // A repeated turn.start returns the recorded result; a second turn conflicts.
      assert.deepEqual(await env.executor.startTurn(turnStart('as_1', 't_1', 'hi')), started)
      await assert.rejects(
        env.executor.startTurn(turnStart('as_1', 't_2', 'again')),
        (error: unknown) => error instanceof RpcError && error.code === JsonRpcErrorCode.Conflict,
      )

      await waitFor(() => platform.events.some((event) => event.type === 'agent.tool_result'))
      assert.equal(platform.interactions[0]?.tool_use_id, 'tu_1')
      assert.equal(platform.deltas[0]?.text, 'thinking…')
      const input = { session_id: 'as_1', turn_id: 't_1', input_id: 'in_2', input: [{ type: 'text' as const, text: 'more' }] }
      assert.deepEqual(await env.executor.sendTurnInput(input), { accepted: true })
      await waitFor(() => platform.types('as_1').includes('session.status_idle'))

      assert.deepEqual(platform.types('as_1'), [
        'session.status_running',
        'agent.tool_use',
        'agent.tool_result',
        'agent.message',
        'session.status_idle',
      ])
      assert.deepEqual(
        platform.events.map((event) => event.sequence),
        [1, 2, 3, 4, 5],
      )
      assert.equal((platform.events[4]?.event as { turn_outcome: string }).turn_outcome, 'completed')
      assert.match(JSON.stringify(platform.events[3]?.event), /echo:more/)
      await assert.rejects(
        env.executor.sendTurnInput({ ...input, input_id: 'in_3' }),
        (error: unknown) => error instanceof RpcError && error.code === JsonRpcErrorCode.Conflict,
      )

      assert.deepEqual(await env.executor.closeAgentSession({ session_id: 'as_1', reason: 'done' }), { closed: true })
      assert.deepEqual(env.closed, ['as_1'])
      assert.deepEqual(env.client.activeSessions, [])
    } finally {
      await env.close()
    }
  })

  test('turn.cancel ends the turn as canceled; a failing agent ends it as failed', async () => {
    const platform = new MemoryPlatform()
    const env = await setup(platform, inProcess)
    try {
      await env.executor.startTurn(turnStart('as_c', 't_c', 'block'))
      await waitFor(() => platform.types('as_c').includes('session.status_running'))
      assert.deepEqual(await env.executor.cancelTurn({ session_id: 'as_c', turn_id: 't_c' }), { canceled: true })
      await waitFor(() => platform.types('as_c').includes('session.status_idle'))
      assert.equal((platform.events.at(-1)?.event as { turn_outcome: string }).turn_outcome, 'canceled')
      assert.deepEqual(await env.executor.cancelTurn({ session_id: 'as_c', turn_id: 't_c' }), { canceled: false })

      await env.executor.startTurn(turnStart('as_c', 't_f', 'fail'))
      await waitFor(() => platform.types('as_c').filter((type) => type === 'session.status_idle').length === 2)
      assert.deepEqual(platform.types('as_c').slice(-3), ['session.status_running', 'session.error', 'session.status_idle'])
      assert.equal((platform.events.at(-1)?.event as { turn_outcome: string }).turn_outcome, 'failed')
    } finally {
      await env.close()
    }
  })

  test('events are resent after a retryable failure and a dropped connection, without gaps or duplicates', async () => {
    const platform = new MemoryPlatform()
    platform.failNext = 2
    const env = await setup(platform, webSocket)
    try {
      await env.executor.startTurn(turnStart('as_r', 't_r', 'block'))
      await waitFor(() => platform.types('as_r').includes('session.status_running'), 10_000)
      const conn = (env.executor as unknown as { conn: { conn: { close(code: number): void } } }).conn.conn
      conn.close(1001)
      const before = env.client.connectionId
      await waitFor(() => env.executor.connected && env.client.state === 'ready' && env.client.connectionId !== before)
      assert.deepEqual(await env.executor.cancelTurn({ session_id: 'as_r', turn_id: 't_r' }), { canceled: true })
      await waitFor(() => platform.types('as_r').includes('session.status_idle'))
      assert.deepEqual(platform.types('as_r'), ['session.status_running', 'session.status_idle'])
      assert.deepEqual(
        platform.events.map((event) => event.sequence),
        [1, 2],
      )
    } finally {
      await env.close()
    }
  })

  test('the harness rejects a sequence gap with expected_sequence and turn.start without the capability', async () => {
    const platform = new MemoryPlatform()
    const harness = new Harness({ agent: platform })
    const client = new ExecutorClient({ environmentId: 'env_test', executor: executorInfo, dialer: harness.inProcessDialer() })
    client.onSessionAssign(async (assign) => ({ accepted: true, session_id: assign.session_id }))
    await client.start()
    try {
      const executor = await harness.waitExecutor()
      await assert.rejects(
        executor.startTurn(turnStart('as_x', 't_x', 'hi')),
        (error: unknown) => error instanceof RpcError && error.code === JsonRpcErrorCode.CapabilityUnavailable,
      )
      await assert.rejects(
        client.call('session.event', {
          session_id: 'as_x',
          turn_id: 't_x',
          event_id: 'evt_9',
          sequence: 3,
          event: { type: 'agent.message', content: [] },
        }),
        (error: unknown) =>
          error instanceof RpcError && error.code === JsonRpcErrorCode.Conflict && error.data?.expected_sequence === 1,
      )
    } finally {
      client.stop()
    }
  })
})
