// executor.token.refresh: the client keeps one control connection across
// token lifetimes, and falls back to the 4001 reconnect on an older harness.
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'
import { WebSocketServer, type WebSocket } from 'ws'

import { ExecutorClient } from '../src/index.js'
import { executorInfo, waitFor } from './helpers.js'

interface Frame {
  id?: string
  method?: string
  params?: { token?: string }
}

async function fakeHarness(answerRefresh: (frame: Frame) => object) {
  const server = new WebSocketServer({ port: 0, host: '127.0.0.1', handleProtocols: () => 'bma.executor.v1.1' })
  await new Promise<void>((resolve) => server.once('listening', resolve))
  const port = (server.address() as { port: number }).port
  const refreshes: string[] = []
  let dials = 0
  server.on('connection', (ws: WebSocket) => {
    dials++
    ws.on('message', (raw) => {
      const frame = JSON.parse(raw.toString()) as Frame
      if (!frame.id || !frame.method) return
      let result: object
      if (frame.method === 'executor.register') {
        result = {
          result: {
            protocol: 'bma.executor.v1.1', executor_id: 'exec_test', connection_id: 'conn_test', server_time: new Date().toISOString(),
            heartbeat_interval_ms: 60000, lease_ttl_ms: 60000, max_control_frame_bytes: 65536, max_mcp_frame_bytes: 1 << 20,
          },
        }
      } else if (frame.method === 'executor.token.refresh') {
        refreshes.push(frame.params?.token ?? '')
        result = answerRefresh(frame)
      } else {
        result = { result: { accepted: true } }
      }
      ws.send(JSON.stringify({ jsonrpc: '2.0', id: frame.id, ...result }))
    })
  })
  return { url: `ws://127.0.0.1:${port}`, refreshes, dials: () => dials, close: () => new Promise((r) => server.close(r)) }
}

function client(url: string) {
  let minted = 0
  return new ExecutorClient({
    environmentId: '',
    executor: executorInfo,
    tokenSource: async () => ({ token: `tok-${++minted}`, ws_url: url, expires_in: 1 }),
  })
}

describe('executor.token.refresh', () => {
  test('refreshes inside the connection at about 70% of the token lifetime', async () => {
    const harness = await fakeHarness(() => ({ result: { expires_at: new Date(Date.now() + 1000).toISOString() } }))
    const executor = client(harness.url)
    try {
      await executor.start()
      await waitFor(() => harness.refreshes.length >= 3, 4000)
      assert.deepEqual(harness.refreshes.slice(0, 3), ['tok-2', 'tok-3', 'tok-4'])
      assert.equal(harness.dials(), 1)
    } finally {
      executor.stop()
      await harness.close()
    }
  })

  test('stops refreshing when the harness answers -32601', async () => {
    const harness = await fakeHarness(() => ({ error: { code: -32601, message: 'unknown method executor.token.refresh' } }))
    const executor = client(harness.url)
    try {
      await executor.start()
      await waitFor(() => harness.refreshes.length >= 1, 3000)
      await new Promise((resolve) => setTimeout(resolve, 1500))
      assert.equal(harness.refreshes.length, 1)
    } finally {
      executor.stop()
      await harness.close()
    }
  })
})
