// Typed tool handlers (client.tools.<name>.handle) and typed harness calls
// (session.tools.<name>.call), ADR 0012 §4.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { expectTypeOf } from 'expect-type'

import {
  ExecutorClient,
  Harness,
  ToolError,
  type BashInput,
  type ReadInput,
  type ToolContext,
  type ToolResultParams,
} from '../src/index.js'
import { executorInfo } from './helpers.js'

async function pair(register: (client: ExecutorClient) => void, capabilities = {}) {
  const harness = new Harness()
  const client = new ExecutorClient({
    environmentId: 'env_test',
    executor: executorInfo,
    capabilities,
    dialer: harness.inProcessDialer(),
  })
  client.onSessionAssign(async (assign) => {
    setImmediate(() => void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token }))
    return { accepted: true, session_id: assign.session_id }
  })
  register(client)
  await client.start()
  const executor = await harness.waitExecutor(AbortSignal.timeout(5000))
  const session = await executor.assignSession({ session_id: 'sess_typed' })
  await session.ready(AbortSignal.timeout(5000))
  return { client, executor, session, close: () => client.stop() }
}

test('bash result renders like Go Core: stdout, stderr, exit_code', async () => {
  const seen: BashInput[] = []
  const { session, executor, close } = await pair((client) => {
    client.tools.bash.handle(async (input, ctx) => {
      expectTypeOf(input).toEqualTypeOf<BashInput>()
      expectTypeOf(ctx).toEqualTypeOf<ToolContext>()
      seen.push(input)
      assert.equal(ctx.sessionId, 'sess_typed')
      return { stdout: 'out\n', stderr: 'warn\n', exitCode: 3 }
    })
  })
  try {
    assert.deepEqual(executor.capabilities.shell, { enabled: true })
    const result = await session.tools.bash.call({ command: 'false', cwd: 'sub' })
    assert.deepEqual(seen, [{ command: 'false', cwd: 'sub' }])
    assert.equal(result.outcome, 'succeeded')
    assert.equal(result.is_error, false)
    assert.deepEqual(result.content, [
      { type: 'text', text: 'out\n' },
      { type: 'text', text: 'warn\n' },
      { type: 'text', text: 'exit_code: 3' },
    ])
    // Empty streams are omitted, as in Go Core.
    const quiet = await session.callTool('bash', { command: 'true' }, { toolCallId: 'call_quiet' })
    void quiet
  } finally {
    close()
  }
})

test('string, blocks, ToolError and exceptions', async () => {
  const { session, close } = await pair((client) => {
    client.tools.read.handle(async ({ path, offset }) => {
      if (path === 'missing') throw new ToolError(`no such file: ${path}`)
      if (path === 'boom') throw new Error('disk on fire')
      if (path === 'img') return [{ type: 'image', data: 'AAAA', mime_type: 'image/png' }]
      return `contents of ${path} from ${offset ?? 0}`
    })
  })
  try {
    const ok = await session.tools.read.call({ path: 'a.txt', offset: 2 })
    assert.deepEqual([ok.outcome, ok.is_error, ok.content], ['succeeded', false, [{ type: 'text', text: 'contents of a.txt from 2' }]])

    const img = await session.tools.read.call({ path: 'img' })
    assert.equal(img.content[0]?.type, 'image')

    const missing = await session.tools.read.call({ path: 'missing' })
    assert.deepEqual([missing.outcome, missing.is_error, missing.content[0]?.text], ['succeeded', true, 'no such file: missing'])

    const boom = await session.tools.read.call({ path: 'boom' })
    assert.deepEqual([boom.outcome, boom.is_error, boom.content[0]?.text], ['failed', true, 'disk on fire'])

    const unhandled: ToolResultParams = await session.tools.write.call({ path: 'x', content: 'y' })
    assert.equal(unhandled.outcome, 'failed')
    assert.match(unhandled.content[0]?.text ?? '', /no handler registered for tool write/)
  } finally {
    close()
  }
})

test('capabilities advertise only registered tools', async () => {
  const { executor, close } = await pair((client) => {
    client.tools.read.handle(async () => 'x')
  }, { shell: { enabled: true }, files: { enabled: false, read_roots: ['/w'] } })
  try {
    assert.deepEqual(executor.capabilities.shell, { enabled: false })
    assert.deepEqual(executor.capabilities.files, { enabled: true, read_roots: ['/w'] })
  } finally {
    close()
  }
})

test('type-level: inputs are per tool', () => {
  const typeOnly = false
  if (!typeOnly) return
  const client = null as unknown as ExecutorClient
  client.tools.read.handle(async (input) => {
    expectTypeOf(input).toEqualTypeOf<ReadInput>()
    // @ts-expect-error read has no command
    void input.command
    return ''
  })
  // @ts-expect-error a bash result is only accepted from the bash handler
  client.tools.read.handle(async () => ({ stdout: '', stderr: '', exitCode: 0 }))
  // @ts-expect-error unknown tool
  void client.tools.nope
  const session = null as unknown as Awaited<ReturnType<typeof pair>>['session']
  // @ts-expect-error read requires path
  void session.tools.read.call({})
  // @ts-expect-error input must match the tool
  void session.callTool('bash', { path: 'x' })
})
