// Cross-language interop with the Python SDK over WebSocket on a Unix domain
// socket: TS executor ↔ Python harness (Harness.serve_unix) and Python
// executor ↔ TS harness (unix:// URL). The Python package comes from
// BMA_PYTHON_SDK (a checkout's python/ directory with a .venv), defaulting to
// ../python; the suite is skipped when it has no bma_executor_sdk.
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { createInterface } from 'node:readline'
import { describe, test } from 'node:test'

import { bearerAuthenticator, Harness, localUrl, staticToken, webSocketDialer } from '../src/index.js'
import { executorInfo, socketPath, testExecutor, waitFor } from './helpers.js'

const pythonSdk = resolve(process.env.BMA_PYTHON_SDK ?? join(process.cwd(), '../python'))
const python = process.env.BMA_PYTHON ?? join(pythonSdk, '.venv/bin/python')
const scripts = resolve(process.cwd(), 'test/python')
const available = existsSync(join(pythonSdk, 'src/bma_executor_sdk/harness.py')) && existsSync(python)

function startPython(script: string, args: string[]) {
  const child = spawn(python, [join(scripts, script), ...args], {
    env: { ...process.env, PYTHONPATH: join(pythonSdk, 'src') },
    stdio: ['ignore', 'pipe', 'inherit'],
  })
  const lines: string[] = []
  createInterface({ input: child.stdout! }).on('line', (line) => lines.push(line))
  const exited = new Promise<number | null>((done) => child.on('exit', done))
  return { child, lines, exited }
}

describe('cross-language interop with the Python SDK (WebSocket over UDS)', { skip: !available && `no Python SDK at ${pythonSdk}`, timeout: 60_000 }, () => {
  test('TypeScript executor ↔ Python harness', async () => {
    const path = await socketPath('py')
    const peer = startPython('py_harness.py', [path, 'tok'])
    try {
      await waitFor(() => peer.lines.some((line) => line.startsWith('LISTEN ')), 15_000)
      const url = peer.lines.find((line) => line.startsWith('LISTEN '))!.slice('LISTEN '.length)
      assert.equal(url, localUrl(path))
      const { client, runs } = testExecutor({
        dialer: webSocketDialer({ environmentId: 'env_interop', executor: executorInfo, tokenSource: staticToken(url, 'tok') }),
        reconnect: false,
      })
      try {
        await client.start()
        assert.equal(client.protocol, 'bma.executor.v1.1')
        await waitFor(() => client.activeSessions.includes('sess_py'))
        const channel = await client.openMcpChannel('sess_py', 'echo')
        channel.write((await channel.read()).replace('"ping"', '"pong"'))
        assert.equal(await peer.exited, 0, peer.lines.join('\n'))
        assert.ok(peer.lines.includes('PASS'))
        assert.deepEqual(runs, ['call_1'], 'the repeated tool_call_id must not run twice')
      } finally {
        client.stop()
      }
    } finally {
      peer.child.kill()
    }
  })

  test('Python executor ↔ TypeScript harness', async () => {
    const harness = new Harness({ authenticate: bearerAuthenticator('tok') })
    const server = await harness.listenLocal(await socketPath('ts'))
    const peer = startPython('py_executor.py', [server.url, 'tok'])
    try {
      const executor = await harness.waitExecutor(AbortSignal.timeout(15_000))
      assert.equal(executor.protocol, 'bma.executor.v1.1')
      assert.equal(executor.info.name, 'py-interop')
      const session = await executor.assignSession({ session_id: 'sess_ts' })
      await session.ready(AbortSignal.timeout(5000))
      for (let i = 0; i < 2; i++) {
        const result = await session.callTool({ tool_call_id: 'call_1', tool: 'bash', input: { command: 'hello' } })
        assert.equal(result.content[0]?.text, 'py:hello')
      }
      const channel = await session.mcp('echo', AbortSignal.timeout(5000))
      channel.write('{"jsonrpc":"2.0","id":1,"method":"ping"}')
      assert.equal(await channel.read(AbortSignal.timeout(5000)), '{"jsonrpc":"2.0","id":1,"method":"pong"}')
      await session.stop('done')
      assert.equal(await peer.exited, 0, peer.lines.join('\n'))
      assert.ok(peer.lines.includes('PASS'))
    } finally {
      peer.child.kill()
      await server.close()
    }
  })
})
