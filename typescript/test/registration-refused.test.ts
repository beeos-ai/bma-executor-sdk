// A machine-scoped executor that declares neither the process nor the agent
// capability is closed with 4002 "runtime registration refused" right after
// executor.register succeeds. Reconnecting cannot fix it: after 3 such
// refusals in a row the client ends (waitClosed() rejects with
// RegistrationRefusedError), and waitReady() reports ready only for a
// registration that stayed open for one heartbeat interval (Go registration_refused_test.go).
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'
import { WebSocketServer, type WebSocket } from 'ws'

import { ExecutorClient, RegistrationRefusedError, RpcError, type ExecutorClientOptions } from '../src/index.js'
import { executorInfo, waitFor } from './helpers.js'

const REFUSED = 'runtime registration refused'

interface Behavior {
  /** Runs after the n-th registration (1-based) was answered. */
  after?: (n: number, ws: WebSocket) => void
  /** JSON-RPC error code answering every registration after the first; 0 answers success. */
  registerError?: number
  heartbeatMs?: number
}

async function harness(behavior: Behavior) {
  const server = new WebSocketServer({ port: 0, host: '127.0.0.1', handleProtocols: () => 'bma.executor.v1.1' })
  await new Promise<void>((resolve) => server.once('listening', resolve))
  const state = { registrations: 0 }
  server.on('connection', (ws: WebSocket) => {
    ws.on('message', (raw) => {
      const frame = JSON.parse(raw.toString()) as { id?: string; method?: string }
      if (!frame.id || frame.method !== 'executor.register') return
      const n = ++state.registrations
      if (behavior.registerError && n > 1) {
        ws.send(JSON.stringify({ jsonrpc: '2.0', id: frame.id, error: { code: behavior.registerError, message: 'capability not available' } }))
        return
      }
      ws.send(JSON.stringify({ jsonrpc: '2.0', id: frame.id, result: {
        protocol: 'bma.executor.v1.1', executor_id: 'exec_test', connection_id: `conn_${n}`, server_time: new Date().toISOString(),
        heartbeat_interval_ms: behavior.heartbeatMs ?? 60000, lease_ttl_ms: 60000, max_control_frame_bytes: 65536, max_mcp_frame_bytes: 1 << 20,
      } }))
      behavior.after?.(n, ws)
    })
  })
  const url = `ws://127.0.0.1:${(server.address() as { port: number }).port}`
  const fetch = (async () => Response.json({ executor_id: '', token: 'jwt', expires_in: 300, ws_url: url })) as typeof globalThis.fetch
  return { state, fetch, close: () => new Promise((r) => server.close(r)) }
}

function client(fetch: typeof globalThis.fetch, options: Partial<ExecutorClientOptions> = {}) {
  return new ExecutorClient({
    environmentId: '', environmentKey: 'env_key', baseUrl: 'http://bma.test', executor: executorInfo, fetch,
    reconnectBaseDelayMs: 1, reconnectMaxDelayMs: 2, ...options,
  })
}

describe('registration refused', () => {
  test('3 refusals in a row end the client', async () => {
    const h = await harness({ after: (_n, ws) => setTimeout(() => ws.close(4002, REFUSED), 5) })
    const executor = client(h.fetch)
    try {
      await executor.start()
      await assert.rejects(executor.waitClosed(), (error: unknown) =>
        error instanceof RegistrationRefusedError && error.cause instanceof Error && /4002/.test(error.cause.message))
      assert.equal(h.state.registrations, 3)
      assert.equal(executor.state, 'closed')
    } finally {
      executor.stop()
      await h.close()
    }
  })

  test('4002 "daemon runtime ended" is not a configuration error', async () => {
    const h = await harness({ after: (_n, ws) => setTimeout(() => ws.close(4002, 'daemon runtime ended'), 5) })
    const executor = client(h.fetch)
    try {
      await executor.start()
      await waitFor(() => h.state.registrations >= 8)
      assert.notEqual(executor.state, 'closed')
    } finally {
      executor.stop()
      await h.close()
    }
  })

  test('a refusal of a connection that was stable is not counted', async () => {
    const h = await harness({ heartbeatMs: 20, after: (_n, ws) => setTimeout(() => ws.close(4002, REFUSED), 120) })
    const executor = client(h.fetch)
    try {
      await executor.start()
      await waitFor(() => h.state.registrations >= 6, 8000)
      assert.notEqual(executor.state, 'closed')
    } finally {
      executor.stop()
      await h.close()
    }
  })

  for (const code of [-32012, -32602]) {
    test(`an executor.register error ${code} counts as a refusal`, async () => {
      const h = await harness({ registerError: code, after: (_n, ws) => setTimeout(() => ws.close(1012, 'node replaced'), 5) })
      const executor = client(h.fetch)
      try {
        await executor.start()
        await assert.rejects(executor.waitClosed(), (error: unknown) =>
          error instanceof RegistrationRefusedError && error.cause instanceof RpcError && error.cause.code === code)
      } finally {
        executor.stop()
        await h.close()
      }
    })
  }

  test('waitReady resolves only after one heartbeat interval', async () => {
    const h = await harness({ heartbeatMs: 300 })
    const executor = client(h.fetch)
    try {
      const started = Date.now()
      await executor.start()
      await executor.waitReady(5000)
      assert.ok(Date.now() - started >= 250, `waitReady returned after ${Date.now() - started}ms`)
      assert.equal(executor.state, 'ready')
    } finally {
      executor.stop()
      await h.close()
    }
  })

  test('waitReady rejects with the configuration error for refused registrations', async () => {
    const h = await harness({ after: (_n, ws) => setTimeout(() => ws.close(4002, REFUSED), 5) })
    const executor = client(h.fetch)
    try {
      await executor.start()
      await assert.rejects(executor.waitReady(5000), (error: unknown) => error instanceof RegistrationRefusedError)
    } finally {
      executor.stop()
      await h.close()
    }
  })

  test('waitReady times out while the registration is not stable', async () => {
    const h = await harness({})
    const executor = client(h.fetch)
    try {
      await executor.start()
      await assert.rejects(executor.waitReady(50), /timed out/)
    } finally {
      executor.stop()
      await h.close()
    }
  })
})
