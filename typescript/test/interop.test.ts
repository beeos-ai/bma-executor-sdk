// Cross-language interop with the Go SDK over real WebSockets and processes:
// TS executor ↔ Go harness, TS agent executor ↔ Go agent platform, and
// TS harness ↔ Go executor (tools, MCP channel, agent).
import assert from 'node:assert/strict'
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createInterface } from 'node:readline'
import { before, describe, test } from 'node:test'

import {
  ExecutorClient,
  Harness,
  staticToken,
  webSocketDialer,
  type AgentEventRecord,
  type AgentPlatform,
} from '../src/index.js'
import { executorInfo, testExecutor, waitFor } from './helpers.js'

const goDir = resolve(process.cwd(), '..')
const binDir = mkdtempSync(join(tmpdir(), 'bma-interop-'))
const bin = (name: string) => join(binDir, name)

before(() => {
  for (const name of ['harness', 'agentharness', 'executor']) {
    execFileSync('go', ['build', '-o', bin(name), `./internal/conformance/${name}`], { cwd: goDir, stdio: 'inherit' })
  }
})

interface Peer {
  process: ChildProcess
  output: string[]
  exited: Promise<number | null>
  /** Resolves with the rest of the first line starting with prefix. */
  line(prefix: string): Promise<string>
}

function startPeer(name: string, args: string[] = []): Peer {
  const child = spawn(bin(name), args, { stdio: ['ignore', 'pipe', 'inherit'] })
  const output: string[] = []
  const waiters: Array<{ prefix: string; resolve: (rest: string) => void }> = []
  createInterface({ input: child.stdout! }).on('line', (line) => {
    output.push(line)
    for (const waiter of waiters.filter((candidate) => line.startsWith(candidate.prefix))) waiter.resolve(line.slice(waiter.prefix.length))
  })
  const exited = new Promise<number | null>((done) => child.on('exit', done))
  return {
    process: child,
    output,
    exited,
    line: (prefix) =>
      new Promise((resolveLine, reject) => {
        const seen = output.find((line) => line.startsWith(prefix))
        if (seen !== undefined) return resolveLine(seen.slice(prefix.length))
        waiters.push({ prefix, resolve: resolveLine })
        void exited.then(() => reject(new Error(`${name} exited early: ${output.join('\n')}`)))
      }),
  }
}

describe('cross-language interop with the Go SDK', { timeout: 120_000 }, () => {
  test('TypeScript executor ↔ Go harness', async () => {
    const harness = startPeer('harness')
    const url = (await harness.line('LISTEN ')).trim()
    const { client, runs } = testExecutor({
      dialer: webSocketDialer({ environmentId: 'local', executor: executorInfo, tokenSource: staticToken(url, 'conformance') }),
    })
    try {
      const registered = await client.start()
      assert.match(registered.executor_id, /^exe_/)
      assert.equal(client.protocol, 'bma.executor.v1.1')
      const code = await harness.exited
      assert.equal(code, 0, harness.output.join('\n'))
      assert.ok(harness.output.includes('PASS'))
      assert.deepEqual(runs, ['call_1', 'call_block'], 'the repeated tool_call_id must not run twice')
    } finally {
      client.stop()
      harness.process.kill()
    }
  })

  test('TypeScript agent executor ↔ Go agent platform', async () => {
    const harness = startPeer('agentharness')
    const url = (await harness.line('LISTEN ')).trim()
    const client = new ExecutorClient({
      environmentId: 'local',
      executor: executorInfo,
      dialer: webSocketDialer({ environmentId: 'local', executor: executorInfo, tokenSource: staticToken(url) }),
    })
    const closed: string[] = []
    client.serveAgent({
      openTurn: async (params) => ({ accepted: true, turn_id: params.turn_id, executor_session_ref: 'ts_ref', resumed: false, effective: {} }),
      runTurn: async (turn, signal) => {
        const answer = await turn.requestInteraction({ kind: 'permission', tool_use_id: 'tu_ts', tool_name: 'bash' }, signal)
        turn.emit({ type: 'agent.message', content: [{ type: 'text', text: `ts:${turn.params.input[0]?.text}:${answer.outcome}` }] })
      },
      closeSession: async (sessionId) => void closed.push(sessionId),
    })
    try {
      await client.start()
      const code = await harness.exited
      assert.equal(code, 0, harness.output.join('\n'))
      assert.deepEqual(closed, ['as_go'])
    } finally {
      client.stop()
      harness.process.kill()
    }
  })

  test('TypeScript harness ↔ Go executor', async () => {
    const events: AgentEventRecord[] = []
    const platform: AgentPlatform = {
      ackedSequence: async () => 0,
      agentEvent: async (_executor, event) => void events.push(event),
      agentDelta: () => {},
      interactionRequested: async (executor, request) => {
        setImmediate(() => {
          void executor.respondInteraction({ session_id: request.session_id, interaction_id: request.interaction_id, outcome: 'allow' })
        })
        return 'pint_ts'
      },
      interactionCanceled: async () => {},
    }
    const harness = new Harness({
      agent: platform,
      authenticate: (request) => {
        if (request.headers.authorization !== 'Bearer conformance') throw new Error('unauthorized')
        return {}
      },
    })
    const server = await harness.listen()
    const executorProcess = startPeer('executor', [server.url])
    try {
      await executorProcess.line('READY')
      const executor = await harness.waitExecutor(AbortSignal.timeout(10_000))
      assert.equal(executor.info.name, 'go-conformance')
      assert.equal(executor.protocol, 'bma.executor.v1.1')

      const session = await executor.assignSession({ session_id: 'sess_go' })
      await session.ready(AbortSignal.timeout(10_000))
      for (let i = 0; i < 2; i++) {
        const result = await session.callTool({ tool_call_id: 'call_go', tool: 'bash', input: { command: 'hello' } })
        assert.equal(result.content[0]?.text, 'go:hello')
      }
      const blocked = session.callTool({ tool_call_id: 'call_block', tool: 'block' as 'bash', input: {} })
      let canceled = false
      for (let i = 0; i < 100 && !canceled; i++) {
        canceled = await session.cancelTool('call_block', 'interop')
        if (!canceled) await new Promise((r) => setTimeout(r, 20))
      }
      assert.ok(canceled)
      assert.equal((await blocked).outcome, 'canceled')

      const channel = await session.mcp('echo', AbortSignal.timeout(10_000))
      channel.write('{"jsonrpc":"2.0","id":1,"method":"ping"}')
      assert.equal(await channel.read(AbortSignal.timeout(10_000)), '{"jsonrpc":"2.0","id":1,"method":"pong"}')

      assert.equal(executor.agentCapability?.enabled, true)
      const started = await executor.startTurn({
        session_id: 'as_ts',
        turn_id: 'turn_ts',
        input_id: 'in_ts',
        input: [{ type: 'text', text: 'hello' }],
      })
      assert.equal(started.executor_session_ref, 'go_as_ts')
      await waitFor(() => events.some((event) => event.type === 'session.status_idle'), 10_000)
      assert.deepEqual(
        events.map((event) => [event.sequence, event.type]),
        [
          [1, 'session.status_running'],
          [2, 'agent.message'],
          [3, 'session.status_idle'],
        ],
      )
      assert.match(JSON.stringify(events[1]?.event), /go:hello:allow/)
      assert.deepEqual(await executor.closeAgentSession({ session_id: 'as_ts', reason: 'done' }), { closed: true })

      await session.stop('done')
      assert.deepEqual(await executor.drain('done'), { accepted: true, active_sessions: [] })
    } finally {
      executorProcess.process.kill()
      await server.close()
    }
  })
})
