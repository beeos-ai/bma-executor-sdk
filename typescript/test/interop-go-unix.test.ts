// Cross-language interop with the Go SDK over WebSocket on a Unix domain
// socket: TS executor ↔ Go harness (mcpharness -unix) and Go executor ↔ TS
// harness (listenLocal, unix:// URL), each with a tool call and an MCP echo.
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { createInterface } from 'node:readline'
import { before, describe, test } from 'node:test'

import { bearerAuthenticator, Harness, staticToken, webSocketDialer } from '../src/index.js'
import { executorInfo, socketPath, testExecutor, waitFor } from './helpers.js'

const goDir = resolve(process.cwd(), '..')
const binDir = mkdtempSync(join(tmpdir(), 'bma-interop-unix-'))
const bin = (name: string) => join(binDir, name)

before(() => {
  for (const name of ['mcpharness', 'mcpexecutor']) {
    execFileSync('go', ['build', '-o', bin(name), `./internal/interop/${name}`], { cwd: goDir, stdio: 'inherit' })
  }
})

function startGo(name: string, args: string[]) {
  const child = spawn(bin(name), args, { stdio: ['pipe', 'pipe', 'inherit'] })
  const lines: string[] = []
  createInterface({ input: child.stdout! }).on('line', (line) => lines.push(line))
  const exited = new Promise<number | null>((done) => child.on('exit', done))
  return { child, lines, exited }
}

describe('cross-language interop with the Go SDK (WebSocket over UDS)', { timeout: 120_000 }, () => {
  test('TypeScript executor ↔ Go harness', async () => {
    const path = await socketPath('go')
    const peer = startGo('mcpharness', ['-unix', path])
    try {
      await waitFor(() => peer.lines.some((line) => line.startsWith('LISTEN ')), 15_000)
      const url = peer.lines.find((line) => line.startsWith('LISTEN '))!.slice('LISTEN '.length)
      assert.equal(url, `unix://${path}`)
      const { client } = testExecutor({
        dialer: webSocketDialer({ environmentId: 'env_interop', executor: executorInfo, tokenSource: staticToken(url, '') }),
        reconnect: false,
      })
      try {
        await client.start()
        assert.equal(client.protocol, 'bma.executor.v1.1')
        await waitFor(() => client.activeSessions.includes('ses_go'))
        const channel = await client.openMcpChannel('ses_go', 'echo')
        channel.write((await channel.read()).replace('"ping"', '"pong"'))
        assert.equal(await peer.exited, 0, peer.lines.join('\n'))
        assert.ok(peer.lines.includes('PASS'))
      } finally {
        client.stop()
      }
    } finally {
      peer.child.kill()
    }
  })

  test('Go executor ↔ TypeScript harness', async () => {
    const harness = new Harness({ authenticate: bearerAuthenticator('tok') })
    const server = await harness.listenLocal(await socketPath('ts'))
    const peer = startGo('mcpexecutor', ['-url', server.url, '-token', 'tok'])
    try {
      const executor = await harness.waitExecutor(AbortSignal.timeout(15_000))
      assert.equal(executor.protocol, 'bma.executor.v1.1')
      assert.equal(executor.info.name, 'go-interop')
      const session = await executor.assignSession({ session_id: 'sess_ts' })
      await session.ready(AbortSignal.timeout(5000))
      const result = await session.callTool({ tool_call_id: 'call_1', tool: 'bash', input: { command: 'hello' } })
      assert.equal(result.content[0]?.text, 'go:hello')
      const channel = await session.mcp('echo', AbortSignal.timeout(5000))
      channel.write('{"jsonrpc":"2.0","id":1,"method":"ping"}')
      assert.equal(await channel.read(AbortSignal.timeout(5000)), '{"jsonrpc":"2.0","id":1,"method":"pong"}')
      await session.stop('done')
      peer.child.stdin!.end()
      assert.equal(await peer.exited, 0, peer.lines.join('\n'))
    } finally {
      peer.child.kill()
      await server.close()
    }
  })
})
