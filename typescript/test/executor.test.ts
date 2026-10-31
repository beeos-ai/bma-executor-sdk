// TypeScript executor end against the TypeScript harness end, over every
// transport: in-process, WebSocket, and local sockets in both dial directions.
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
  Harness,
  JsonRpcErrorCode,
  listenLocal,
  localDialer,
  RpcError,
  staticToken,
  ToolOutcomeUnknownError,
  webSocketDialer,
  type Dialer,
  type HarnessExecutor,
  type ToolProgressParams,
} from '../src/executor/index.js'
import { executorInfo, socketPath, testExecutor, waitFor } from './helpers.js'

type TestExecutor = ReturnType<typeof testExecutor>

async function scenario(harness: Harness, start: () => Promise<TestExecutor>, mcp: boolean) {
  const progress: ToolProgressParams[] = []
  harness.options.onToolProgress = (_executor, params) => progress.push(params)
  const { client, runs } = await start()
  try {
    assert.equal(client.protocol, 'bma.executor.v1.1')
    const executor: HarnessExecutor = await harness.waitExecutor(AbortSignal.timeout(5000))
    assert.match(executor.id, /^exe_/)
    assert.equal(executor.info.name, executorInfo.name)
    assert.deepEqual(executor.capabilities.shell, { enabled: true })

    const session = await executor.assignSession({ session_id: 'sess_1' })
    await session.ready(AbortSignal.timeout(5000))
    assert.deepEqual(client.activeSessions, ['sess_1'])

    // tool.call is idempotent by tool_call_id on both ends.
    for (let i = 0; i < 2; i++) {
      const result = await session.callTool({ tool_call_id: 'call_1', tool: 'bash', input: { command: 'hello' } })
      assert.equal(result.outcome, 'succeeded')
      assert.equal(result.content[0]?.text, 'ts:hello')
    }
    assert.deepEqual(runs, ['call_1'])
    await waitFor(() => progress.length === 1)
    assert.equal(progress[0]?.tool_call_id, 'call_1')

    // tool.cancel aborts the handler and the result reports canceled.
    const blocked = session.callTool({ tool_call_id: 'call_block', tool: 'block' as 'bash', input: {} })
    await waitFor(() => runs.includes('call_block'))
    assert.equal(await session.cancelTool('call_block', 'test'), true)
    const canceled = await blocked
    assert.equal(canceled.outcome, 'canceled')
    assert.equal(canceled.content[0]?.text, 'canceled: test')
    assert.equal(await session.cancelTool('call_unknown', 'test'), false)

    // A result for a call the harness never dispatched is not_found and not retried.
    await assert.rejects(
      client.reportToolResult({ session_id: 'sess_1', tool_call_id: 'call_ghost', outcome: 'succeeded', is_error: false, content: [] }),
      (error: unknown) => error instanceof RpcError && error.code === JsonRpcErrorCode.NotFound,
    )

    client.updateCapabilities({ shell: { enabled: true }, chrome: true })
    await waitFor(() => executor.capabilities.chrome === true)

    if (mcp) {
      const served = (async () => {
        const channel = await client.openMcpChannel('sess_1', 'echo')
        const frame = await channel.read()
        channel.write(frame.replace('"ping"', '"pong"'))
        await channel.done
      })()
      const channel = await session.mcp('echo', AbortSignal.timeout(5000))
      channel.write('{"jsonrpc":"2.0","id":1,"method":"ping"}')
      assert.equal(await channel.read(AbortSignal.timeout(5000)), '{"jsonrpc":"2.0","id":1,"method":"pong"}')
      channel.close()
      await served
    }

    const drained = await executor.drain('maintenance')
    assert.deepEqual(drained, { accepted: true, active_sessions: ['sess_1'] })
    assert.equal(client.state, 'draining')

    await session.stop('done')
    assert.equal(session.endReason, 'done')
    assert.deepEqual(client.activeSessions, [])
  } finally {
    client.stop('test done')
  }
}

async function started(made: TestExecutor): Promise<TestExecutor> {
  await made.client.start()
  return made
}

describe('executor end ↔ harness end', () => {
  test('in-process', async () => {
    const harness = new Harness()
    await scenario(harness, () => started(testExecutor({ dialer: harness.inProcessDialer() })), true)
  })

  test('WebSocket with bearer token', async () => {
    const harness = new Harness({
      authenticate: (request) => {
        if (request.headers.authorization !== 'Bearer secret') throw new Error('unauthorized')
        return {}
      },
    })
    const server = await harness.listen()
    const dialer = (token: string): Dialer =>
      webSocketDialer({ environmentId: 'env_test', executor: executorInfo, tokenSource: staticToken(server.url, token) })
    try {
      await assert.rejects(testExecutor({ dialer: dialer('wrong') }).client.start(), /HTTP 401/)
      await scenario(harness, () => started(testExecutor({ dialer: dialer('secret') })), true)
    } finally {
      await server.close()
    }
  })

  test('local socket, executor dials the harness', async () => {
    const harness = new Harness()
    const path = await socketPath('h')
    const listener = await listenLocal({ path, token: 'tok' }, (conn, hello) =>
      harness.accept(conn, { channel: hello.channel, sessionId: hello.session_id, serverKey: hello.server_key }),
    )
    try {
      await assert.rejects(testExecutor({ dialer: localDialer({ path, token: 'bad' }) }).client.start(), /4401/)
      await scenario(harness, () => started(testExecutor({ dialer: localDialer({ path, token: 'tok' }) })), true)
    } finally {
      await listener.close()
    }
  })

  test('local socket, harness dials the executor', async () => {
    const harness = new Harness()
    const path = await socketPath('e')
    let resolveClient!: (made: TestExecutor) => void
    const accepted = new Promise<TestExecutor>((resolve) => (resolveClient = resolve))
    const listener = await listenLocal({ path, token: 'tok' }, (conn) => {
      const made = testExecutor({ dialer: async () => conn, reconnect: false })
      void made.client.start().then(() => resolveClient(made))
    })
    try {
      await harness.dialLocal({ path, token: 'tok' })
      await scenario(harness, () => accepted, false)
    } finally {
      await listener.close()
    }
  })
})

function dropConnection(executor: HarnessExecutor, code: number): void {
  ;(executor as unknown as { conn: { conn: { close(code: number): void } } }).conn.conn.close(code)
}

describe('reconnect and lease', () => {
  test('resume keeps sessions and pending calls across a dropped connection', async () => {
    const harness = new Harness()
    const server = await harness.listen()
    const { client, runs } = testExecutor({
      dialer: webSocketDialer({ environmentId: 'env_test', executor: executorInfo, tokenSource: staticToken(server.url) }),
    })
    try {
      const first = await client.start()
      const executor = await harness.waitExecutor()
      const session = await executor.assignSession({ session_id: 'sess_r' })
      await session.ready()
      const blocked = session.callTool({ tool_call_id: 'call_r', tool: 'block' as 'bash', input: {} })
      await waitFor(() => runs.includes('call_r'))

      dropConnection(executor, 1001)
      await waitFor(() => executor.connected && client.state === 'ready' && client.connectionId !== first.connection_id)
      assert.equal(client.executorId, first.executor_id)
      assert.equal(session.endReason, '')

      assert.equal(await session.cancelTool('call_r', 'after reconnect'), true)
      assert.equal((await blocked).outcome, 'canceled')
    } finally {
      client.stop()
      await server.close()
    }
  })

  test('4004 is terminal; lease expiry fails pending calls with ToolOutcomeUnknownError', async () => {
    const harness = new Harness({ leaseTtlMs: 50 })
    const made = testExecutor({ dialer: harness.inProcessDialer() })
    try {
      await made.client.start()
      const executor = await harness.waitExecutor()
      const session = await executor.assignSession({ session_id: 'sess_l' })
      await session.ready()
      const blocked = session.callTool({ tool_call_id: 'call_l', tool: 'block' as 'bash', input: {} })
      await waitFor(() => made.runs.includes('call_l'))
      dropConnection(executor, 4004)
      const outcome = blocked.then(() => undefined, (error: unknown) => error)
      await waitFor(() => harness.executors().length === 0)
      assert.ok((await outcome) instanceof ToolOutcomeUnknownError)
      assert.equal(made.client.state, 'closed')
      assert.equal(harness.executors().length, 0)
    } finally {
      made.client.stop()
    }
  })
})
