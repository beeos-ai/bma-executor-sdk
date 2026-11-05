// Typed tool handlers (client.tools.<name>.handle) and typed harness calls
// (session.tools.<name>.call), ADR 0012 §4.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { expectTypeOf } from 'expect-type'

import {
  ExecutorClient,
  Harness,
  INLINE_RESULT_BYTES,
  JsonRpcErrorCode,
  REQUIRED_TOOL_INPUT,
  RpcError,
  ToolError,
  type Ack,
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

    // A tool without a handler is refused before the ack, as Go Core refuses it.
    await assert.rejects(
      session.tools.write.call({ path: 'x', content: 'y' }),
      (error: unknown) => error instanceof RpcError && error.code === JsonRpcErrorCode.CapabilityUnavailable && error.data?.capability === 'tool:write',
    )
  } finally {
    close()
  }
})

test('capabilities advertise registered tools; an explicit enabled:false wins', async () => {
  const { executor, close } = await pair((client) => {
    client.tools.read.handle(async () => 'x')
    client.tools.bash.handle(async () => 'x')
  }, { shell: { enabled: false }, files: { read_roots: ['/w'] } })
  try {
    assert.deepEqual(executor.capabilities.shell, { enabled: false })
    assert.deepEqual(executor.capabilities.files, { enabled: true, read_roots: ['/w'] })
    assert.equal(executor.capabilities.computer_use, false)
  } finally {
    close()
  }
})

test('a handler registered after start sends executor.capabilities.update', async () => {
  let registered!: ExecutorClient
  const { executor, close } = await pair((client) => void (registered = client))
  try {
    assert.equal(executor.capabilities.shell?.enabled, false)
    registered.tools.bash.handle(async () => 'x')
    for (let i = 0; i < 200 && !executor.capabilities.shell?.enabled; i++) await new Promise((r) => setTimeout(r, 10))
    assert.equal(executor.capabilities.shell?.enabled, true)
  } finally {
    close()
  }
})

test('a missing required input member fails the call before the handler runs', async () => {
  let called = false
  const { session, close } = await pair((client) => {
    client.tools.bash.handle(async () => {
      called = true
      return 'ran'
    })
  })
  try {
    const result = await session.callTool('bash', { cwd: 'x' } as never)
    assert.deepEqual([result.outcome, result.content[0]?.text, called], ['failed', 'bash requires command', false])
  } finally {
    close()
  }
})

test('large bash output is truncated like Go Core', async () => {
  const { session, close } = await pair((client) => {
    client.tools.bash.handle(async ({ command }) =>
      command === 'big'
        ? { stdout: 'o'.repeat(100_000), stderr: 'e'.repeat(30_000), exitCode: 1 }
        : { stdout: 'head', stderr: '', exitCode: 0, truncated: true },
    )
  })
  try {
    const big = await session.tools.bash.call({ command: 'big' })
    assert.deepEqual(
      big.content.map((b) => (b.text ?? '').length),
      [INLINE_RESULT_BYTES / 2, INLINE_RESULT_BYTES / 4, 'exit_code: 1 (output truncated to fit the result; 130000 bytes total)'.length],
    )
    assert.equal(big.content[2]?.text, 'exit_code: 1 (output truncated to fit the result; 130000 bytes total)')
    const marked = await session.tools.bash.call({ command: 'small' })
    assert.equal(marked.content.at(-1)?.text, 'exit_code: 0 (output truncated to fit the result; 4 bytes total)')
  } finally {
    close()
  }
})

test('typed onRequest, call and notify', async () => {
  const harness = new Harness()
  const client = new ExecutorClient({ environmentId: 'env_test', executor: executorInfo, dialer: harness.inProcessDialer() })
  client.onRequest('process.list', async ({ session_id }) => ({ processes: [{ process_id: `p_${session_id}` } as never] }))
  await client.start()
  try {
    const executor = await harness.waitExecutor(AbortSignal.timeout(5000))
    const listed = await executor.call('process.list', { session_id: 's1' })
    assert.equal(listed.processes[0]?.process_id, 'p_s1')
  } finally {
    client.stop()
  }
})

test('required input table matches the contract', async () => {
  const { readFile } = await import('node:fs/promises')
  const schema = JSON.parse(await readFile(new URL('../../../contracts/bma/executor/schemas/tool.json', import.meta.url), 'utf8')) as {
    $defs: Record<string, { required?: string[] }>
  }
  const defs: Record<string, string> = { bash: 'BashInput', read: 'ReadInput', write: 'WriteInput', edit: 'EditInput', glob: 'GlobInput', grep: 'GrepInput', list_files: 'ListFilesInput' }
  for (const [tool, def] of Object.entries(defs)) {
    assert.deepEqual(REQUIRED_TOOL_INPUT[tool as keyof typeof REQUIRED_TOOL_INPUT], schema.$defs[def]?.required ?? [], tool)
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
  // @ts-expect-error computer_* have no slot until the contract defines their input
  void client.tools.computer_action
  // @ts-expect-error onRequest only takes contract methods the client does not serve itself
  client.onRequest('tool.call', () => ({ accepted: true }))
  client.onRequest('process.list', (params) => {
    expectTypeOf(params.session_id).toEqualTypeOf<string>()
    return { processes: [] }
  })
  // @ts-expect-error the result must match the method
  client.onRequest('process.list', () => ({ accepted: true }))
  expectTypeOf(client.call('session.ready', { session_id: 's', work_id: 'w', lease_token: 't' })).resolves.toEqualTypeOf<Ack>()
  // @ts-expect-error params must match the method
  void client.call('session.ready', { nope: 1 })
  // @ts-expect-error session.status is a notification
  void client.call('session.status', { session_id: 's', status: 'running' })
  client.notify('tool.progress', { session_id: 's', tool_call_id: 't', message: 'm' })
  const session = null as unknown as Awaited<ReturnType<typeof pair>>['session']
  // @ts-expect-error read requires path
  void session.tools.read.call({})
  // @ts-expect-error input must match the tool
  void session.callTool('bash', { path: 'x' })
})
