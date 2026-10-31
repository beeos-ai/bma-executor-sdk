import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { ExecutorClient, type ExecutorClientOptions, type ToolCallParams } from '../src/index.js'

export const executorInfo = { name: 'ts-test', os: process.platform, arch: process.arch, version: '0' }

export function waitFor(predicate: () => boolean, timeoutMs = 5000): Promise<void> {
  const started = Date.now()
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (predicate()) return resolve()
      if (Date.now() - started > timeoutMs) return reject(new Error('waitFor timed out'))
      setTimeout(tick, 10)
    }
    tick()
  })
}

export async function socketPath(name: string): Promise<string> {
  if (process.platform === 'win32') return `\\\\.\\pipe\\bma-test-${name}-${process.pid}-${Date.now()}`
  // Unix socket paths are limited to ~104 bytes; keep them short.
  const dir = await mkdtemp(join(tmpdir(), 'bma-'))
  return join(dir, `${name}.sock`)
}

/** An executor that answers bash with "ts:<command>", blocks "block" until canceled and echoes MCP frames. */
export function testExecutor(options: Partial<ExecutorClientOptions> & Pick<ExecutorClientOptions, 'dialer'>) {
  const client = new ExecutorClient({
    environmentId: 'env_test',
    executor: executorInfo,
    capabilities: { shell: { enabled: true } },
    reconnectBaseDelayMs: 10,
    ...options,
  })
  const runs: string[] = []
  client.onSessionAssign(async (assign) => {
    setImmediate(() => void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token }))
    return { accepted: true, session_id: assign.session_id }
  })
  client.handleTool('bash', async (call: ToolCallParams, { progress }) => {
    runs.push(call.tool_call_id)
    progress('running')
    return { outcome: 'succeeded', is_error: false, content: [{ type: 'text', text: `ts:${call.input.command}` }] }
  })
  client.onToolCall(async (call, { signal }) => {
    runs.push(call.tool_call_id)
    await new Promise((resolve) => signal.addEventListener('abort', resolve, { once: true }))
    return { outcome: 'succeeded', is_error: false, content: [] }
  })
  return { client, runs }
}
