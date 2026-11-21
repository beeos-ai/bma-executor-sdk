// Reconnect policy (Go reconnect_test.go): by default the client reconnects
// until stop() through any outage; a credential the token endpoint rejects
// (401/403) and an exhausted positive maxReconnectAttempts end the client,
// and waitClosed() rejects with the reason instead of the client hanging.
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'
import { WebSocketServer, type WebSocket } from 'ws'

import {
  ChannelClosedError,
  CredentialRejectedError,
  DEFAULT_MAX_RECONNECT_ATTEMPTS,
  DEFAULT_RECONNECT_BASE_DELAY_MS,
  ExecutorClient,
  ReconnectExhaustedError,
  type ExecutorClientOptions,
} from '../src/index.js'
import { executorInfo, waitFor } from './helpers.js'

/** A harness that registers every connection and, until healthy, closes it with closeCode. */
async function outageHarness(closeCode = 1012, heartbeatMs = 60000) {
  const server = new WebSocketServer({ port: 0, host: '127.0.0.1', handleProtocols: () => 'bma.executor.v1.1' })
  await new Promise<void>((resolve) => server.once('listening', resolve))
  const state = { healthy: false, registrations: 0 }
  server.on('connection', (ws: WebSocket) => {
    ws.on('message', (raw) => {
      const frame = JSON.parse(raw.toString()) as { id?: string; method?: string }
      if (!frame.id || frame.method !== 'executor.register') return
      state.registrations++
      const n = state.registrations
      ws.send(JSON.stringify({ jsonrpc: '2.0', id: frame.id, result: {
        protocol: 'bma.executor.v1.1', executor_id: 'exec_test', connection_id: `conn_${n}`, server_time: new Date().toISOString(),
        heartbeat_interval_ms: heartbeatMs, lease_ttl_ms: 60000, max_control_frame_bytes: 65536, max_mcp_frame_bytes: 1 << 20,
      } }))
      if (!state.healthy) setTimeout(() => ws.close(closeCode, 'node replaced'), 5)
    })
  })
  const url = `ws://127.0.0.1:${(server.address() as { port: number }).port}`
  return { url, state, close: () => new Promise((r) => server.close(r)) }
}

/** The token endpoint: the first request succeeds, the next failFor (forever when < 0) answer failStatus. */
function tokenEndpoint(wsUrl: string, failStatus: number, failFor: number) {
  const counter = { requests: 0 }
  const fetch = (async () => {
    counter.requests++
    const failed = counter.requests - 1
    if (failed > 0 && (failFor < 0 || failed <= failFor)) return new Response('upstream unavailable', { status: failStatus })
    return Response.json({ executor_id: '', token: 'jwt', expires_in: 300, ws_url: wsUrl })
  }) as typeof globalThis.fetch
  return { counter, fetch }
}

function client(fetch: typeof globalThis.fetch, options: Partial<ExecutorClientOptions> = {}) {
  return new ExecutorClient({
    environmentId: 'env_test', environmentKey: 'env_key', baseUrl: 'http://bma.test', executor: executorInfo, fetch,
    reconnectBaseDelayMs: 1, reconnectMaxDelayMs: 2, ...options,
  })
}

describe('reconnect policy', () => {
  test('reconnects after a long outage by default', async () => {
    const harness = await outageHarness()
    const token = tokenEndpoint(harness.url, 503, 25)
    const executor = client(token.fetch)
    try {
      await executor.start()
      harness.state.healthy = true
      await waitFor(() => harness.state.registrations >= 2 && executor.state === 'ready', 5000)
      assert.equal(token.counter.requests, 27) // the start, 25 failures, the reconnect
      executor.stop()
      await executor.waitClosed() // stop() ends it without an error
    } finally {
      executor.stop()
      await harness.close()
    }
  })

  for (const status of [401, 403]) {
    test(`a credential rejected with ${status} ends the client`, async () => {
      const harness = await outageHarness()
      const token = tokenEndpoint(harness.url, status, -1)
      const executor = client(token.fetch)
      try {
        await executor.start()
        await assert.rejects(executor.waitClosed(), (error: unknown) => error instanceof CredentialRejectedError && error.status === status)
        assert.equal(token.counter.requests, 2) // no retry after the rejection
        assert.equal(executor.state, 'closed')
      } finally {
        executor.stop()
        await harness.close()
      }
    })
  }

  test('an exhausted maxReconnectAttempts ends the client', async () => {
    const harness = await outageHarness()
    const token = tokenEndpoint(harness.url, 503, -1)
    const executor = client(token.fetch, { maxReconnectAttempts: 3 })
    try {
      await executor.start()
      await assert.rejects(executor.waitClosed(), (error: unknown) => error instanceof ReconnectExhaustedError && error.cause instanceof Error)
      assert.equal(token.counter.requests, 4) // the start and 3 reconnect attempts
    } finally {
      executor.stop()
      await harness.close()
    }
  })

  test('a terminal close code ends the client', async () => {
    const harness = await outageHarness(4004)
    const token = tokenEndpoint(harness.url, 503, 0)
    const executor = client(token.fetch)
    try {
      await executor.start()
      await assert.rejects(executor.waitClosed(), (error: unknown) => error instanceof ChannelClosedError && error.code === 4004)
    } finally {
      executor.stop()
      await harness.close()
    }
  })

  test('a heartbeat that times out closes the connection and reconnects', async () => {
    // The harness never answers executor.heartbeat: a dead (half-open) connection
    // after a network cut. gRPC keepalive closes a transport whose ping times out.
    const harness = await outageHarness(1012, 20)
    harness.state.healthy = true
    const token = tokenEndpoint(harness.url, 503, 0)
    const executor = client(token.fetch, { requestTimeoutMs: 100 })
    try {
      await executor.start()
      await waitFor(() => harness.state.registrations >= 2 && executor.state === 'ready', 5000)
    } finally {
      executor.stop()
      await harness.close()
    }
  })

  test('defaults: unlimited attempts from 1 s', () => {
    assert.equal(DEFAULT_MAX_RECONNECT_ATTEMPTS, 0)
    assert.equal(DEFAULT_RECONNECT_BASE_DELAY_MS, 1000)
  })
})
