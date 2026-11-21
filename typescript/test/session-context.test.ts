// Session context (GET /api/v1/daemon/processes/{id}/session and
// /api/v1/daemon/sessions/{id}) over the machine token, cached per session;
// RpcError.withReason and ErrorReason carry Go's names and values.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  Code,
  Harness,
  MetadataAgentSessionId,
  DaemonHttpError,
  ErrorReason,
  ExecutorClient,
  MachineTokenRequiredError,
  RpcError,
  SessionContextNotFoundError,
  type SessionContext,
} from '../src/index.js'
import { executorInfo } from './helpers.js'

const sessionA: SessionContext = {
  id: 'sesn_a',
  environment_id: 'mch_1',
  metadata: { owner: 'u_1', app_ref: 'ws_7' },
  created_at: '2026-11-07T00:00:00Z',
}

function fakeDaemon(processes: Record<string, string>, sessions: Record<string, SessionContext>) {
  const calls: string[] = []
  const fetch = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input))
    calls.push(url.pathname)
    const headers = new Headers(init?.headers)
    if (headers.get('authorization') !== 'Bearer mtok') return new Response('', { status: 401 })
    let sessionId: string | undefined
    const byProcess = /^\/api\/v1\/daemon\/processes\/([^/]+)\/session$/.exec(url.pathname)
    const bySession = /^\/api\/v1\/daemon\/sessions\/([^/]+)$/.exec(url.pathname)
    if (byProcess) sessionId = processes[decodeURIComponent(byProcess[1]!)]
    if (bySession) sessionId = decodeURIComponent(bySession[1]!)
    const session = sessionId === undefined ? undefined : sessions[sessionId]
    if (!session) return Response.json({ code: 'not_found', message: 'session not bound to this machine' }, { status: 404 })
    return Response.json(session)
  }) as typeof globalThis.fetch
  return { calls, fetch }
}

function client(fetch: typeof globalThis.fetch, overrides: Partial<ConstructorParameters<typeof ExecutorClient>[0]> = {}) {
  return new ExecutorClient({ environmentId: '', environmentKey: 'mtok', baseUrl: 'https://bma.test/api/v1', executor: executorInfo, fetch, ...overrides })
}

test('sessionContext by process and by id, cached per session', async () => {
  const daemon = fakeDaemon({ proc_1: 'sesn_a', proc_2: 'sesn_a' }, { sesn_a: sessionA })
  const executor = client(daemon.fetch)
  const got = await executor.sessionContext('proc_1')
  assert.deepEqual(got, sessionA)
  await executor.sessionContext('proc_1')
  await executor.sessionContextById('sesn_a')
  assert.deepEqual(daemon.calls, ['/api/v1/daemon/processes/proc_1/session'])
  await executor.sessionContext('proc_2')
  assert.equal(daemon.calls.length, 2)
  got.metadata.owner = 'u_2'
  assert.equal((await executor.sessionContextById('sesn_a')).metadata.owner, 'u_1')
})

test('sessionContext: 404 is SessionContextNotFoundError and is not cached', async () => {
  const sessions: Record<string, SessionContext> = {}
  const daemon = fakeDaemon({}, sessions)
  const executor = client(daemon.fetch)
  await assert.rejects(executor.sessionContext('proc_other'), SessionContextNotFoundError)
  await assert.rejects(executor.sessionContextById('sesn_b'), SessionContextNotFoundError)
  sessions.sesn_b = { ...sessionA, id: 'sesn_b' }
  assert.equal((await executor.sessionContextById('sesn_b')).id, 'sesn_b')
})

test('sessionContext needs a machine-scoped executor; server errors keep the status', async () => {
  const daemon = fakeDaemon({}, {})
  await assert.rejects(client(daemon.fetch, { environmentId: 'env_1' }).sessionContextById('sesn_a'), MachineTokenRequiredError)
  const failing = (async () => new Response('upstream down', { status: 502 })) as typeof globalThis.fetch
  await assert.rejects(client(failing).sessionContextById('sesn_a'), (error: unknown) => error instanceof DaemonHttpError && error.status === 502)
  await assert.rejects(client(daemon.fetch).sessionContextById('../processes/x'), /invalid session id/)
  assert.equal(daemon.calls.length, 0)
})

test('RpcError.withReason and ErrorReason match the Go SDK', () => {
  assert.deepEqual(
    { ...ErrorReason },
    {
      WorkspaceUnavailable: 'workspace_unavailable',
      WorkspaceBoundary: 'workspace_boundary',
      WorkspaceReadOnly: 'workspace_read_only',
      SessionRejected: 'session_rejected',
      LocalConfirmationDenied: 'local_confirmation_denied',
      LocalConfirmationExpired: 'local_confirmation_expired',
      SpawnFailed: 'spawn_failed',
      ProcessIdSpecMismatch: 'process_id_spec_mismatch',
    },
  )
  const error = RpcError.withReason(Code.Forbidden, ErrorReason.SessionRejected, 'not this machine')
  assert.deepEqual(error.toJSON(), { code: -32001, message: 'not this machine', data: { reason: 'session_rejected' } })
})

test('a process.start handler reads the session context and refuses with a reason', async () => {
  const daemon = fakeDaemon({ proc_mine: 'sesn_a', proc_theirs: 'sesn_b' }, { sesn_a: sessionA, sesn_b: { ...sessionA, id: 'sesn_b', metadata: { owner: 'u_9' } } })
  const harness = new Harness()
  const executor = client(daemon.fetch, {
    dialer: harness.inProcessDialer(),
    capabilities: { session_files: { root: '/Users/me/Library/Application Support/example/sessions' } },
  })
  executor.onRequest('process.start', async (params) => {
    const context = await executor.sessionContext(params.process_id)
    if (context.metadata.owner !== 'u_1') throw RpcError.withReason(Code.Forbidden, ErrorReason.SessionRejected, 'not this machine')
    assert.equal(params.metadata?.[MetadataAgentSessionId], context.id)
    return { accepted: true, process_id: params.process_id, state: 'running', started_at: new Date().toISOString(), cursors: { stdout: 0, stderr: 0 } } as never
  })
  await executor.start()
  try {
    const remote = await harness.waitExecutor(AbortSignal.timeout(5000))
    assert.equal(remote.capabilities.session_files?.root, '/Users/me/Library/Application Support/example/sessions')
    const started = await remote.call('process.start', { session_id: 's1', process_id: 'proc_mine', argv: ['true'], metadata: { agent_session_id: 'sesn_a' } })
    assert.equal(started.process_id, 'proc_mine')
    await assert.rejects(
      remote.call('process.start', { session_id: 's1', process_id: 'proc_theirs', argv: ['true'], metadata: { agent_session_id: 'sesn_b' } }),
      (error: unknown) => error instanceof RpcError && error.code === Code.Forbidden && error.data?.reason === 'session_rejected',
    )
  } finally {
    executor.stop()
  }
})
