// baseUrl is the deployment origin (like BMA_BASE_URL in bma-sdk): the token
// bootstrap posts to {origin}/v1/environments/{environment_id}/token or
// {origin}/v1/machines/{machine_id}/token, and a trailing /api/v1 is dropped.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { ChannelScope, DEFAULT_BASE_URL, ExecutorClient, executorOrigin, MACHINE_ID_ENV, webSocketDialer } from '../src/index.js'
import { executorInfo } from './helpers.js'

test('baseUrl is the origin; a trailing /api/v1 is not repeated', async () => {
  assert.equal(DEFAULT_BASE_URL, 'https://executor.beeos.ai')
  for (const baseUrl of ['https://bma.test', 'https://bma.test/', 'https://bma.test/api/v1', 'https://bma.test/api/v1/']) {
    const urls: string[] = []
    const fetch = (async (url: string) => {
      urls.push(String(url))
      return new Response('nope', { status: 500 })
    }) as typeof globalThis.fetch
    const dial = webSocketDialer({ environmentId: 'env', executor: executorInfo, baseUrl, environmentKey: 'k', fetch })
    await assert.rejects(dial({ channel: ChannelScope.Control }))
    assert.deepEqual(urls, ['https://bma.test/v1/environments/env/token'], baseUrl)
  }
})

test('a base URL on a host retired for executors moves to the executor host', async () => {
  assert.equal(executorOrigin('https://api.bma.beeos.ai/api/v1/'), 'https://executor.beeos.ai')
  assert.equal(executorOrigin('https://API.staging.bma.beeos.ai'), 'https://executor.staging.beeos.ai')
  assert.equal(executorOrigin('https://executor.beeos.ai/'), 'https://executor.beeos.ai')
  assert.equal(executorOrigin('https://api.beeos.ai'), 'https://api.beeos.ai')
  assert.equal(executorOrigin(), 'https://executor.beeos.ai')
  const urls: string[] = []
  const fetch = (async (url: string) => {
    urls.push(String(url))
    return new Response('nope', { status: 500 })
  }) as typeof globalThis.fetch
  const dial = webSocketDialer({ environmentId: '', machineId: 'mch_1', executor: executorInfo, baseUrl: 'https://api.bma.beeos.ai', environmentKey: 'k', fetch })
  await assert.rejects(dial({ channel: ChannelScope.Control }))
  assert.deepEqual(urls, ['https://executor.beeos.ai/v1/machines/mch_1/token'])
})

test('a machine token with neither id keeps the legacy alias /api/v1/executors/token', async () => {
  const urls: string[] = []
  const fetch = (async (url: string) => {
    urls.push(String(url))
    return new Response('nope', { status: 500 })
  }) as typeof globalThis.fetch
  const dial = webSocketDialer({ environmentId: '', executor: executorInfo, baseUrl: 'https://bma.test', environmentKey: 'mtok', fetch })
  await assert.rejects(dial({ channel: ChannelScope.Control }))
  assert.deepEqual(urls, ['https://bma.test/api/v1/executors/token'])
})

// Go and Python bound the token request by the request timeout; fetch has none of its own.
test('a token request that gets no answer is aborted after requestTimeoutMs', { timeout: 5000 }, async () => {
  let aborted = false
  const fetch = ((_url: string, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => {
        aborted = true
        reject(init.signal?.reason)
      })
    })) as typeof globalThis.fetch
  const dial = webSocketDialer({ environmentId: 'env', executor: executorInfo, baseUrl: 'https://bma.test', environmentKey: 'k', fetch, requestTimeoutMs: 50 })
  // AbortSignal.timeout does not keep the loop alive; a real fetch holds a socket, this one holds nothing.
  const keepAlive = setTimeout(() => {}, 5000)
  try {
    await assert.rejects(dial({ channel: ChannelScope.Control }))
  } finally {
    clearTimeout(keepAlive)
  }
  assert.ok(aborted, 'the fetch signal fired')
})

async function tokenUrl(options: { environmentId: string; machineId?: string }): Promise<string> {
  const urls: string[] = []
  const fetch = (async (url: string) => {
    urls.push(String(url))
    return new Response('nope', { status: 500 })
  }) as typeof globalThis.fetch
  const dial = webSocketDialer({ ...options, executor: executorInfo, baseUrl: 'https://bma.test', environmentKey: 'mtok', fetch })
  await assert.rejects(dial({ channel: ChannelScope.Control }))
  assert.equal(urls.length, 1)
  return urls[0] ?? ''
}

test('a machine id exchanges the machine token at /v1/machines/{machine_id}/token', async () => {
  assert.equal(await tokenUrl({ environmentId: '', machineId: 'mach_1' }), 'https://bma.test/v1/machines/mach_1/token')
  assert.equal(await tokenUrl({ environmentId: '', machineId: 'a/b' }), 'https://bma.test/v1/machines/a%2Fb/token')
})

test('instanceId asks for a token of that brain fleet instance; without it the body has no instance_id', async () => {
  const bodies: unknown[] = []
  const fetch = (async (_url: string, init?: RequestInit) => {
    bodies.push(JSON.parse(String(init?.body)))
    return new Response('nope', { status: 500 })
  }) as typeof globalThis.fetch
  for (const instanceId of ['brain-host-7d9f-x2', undefined]) {
    const dial = webSocketDialer({ environmentId: '', machineId: 'mch_1', instanceId, executor: executorInfo, baseUrl: 'https://bma.test', environmentKey: 'mtok', fetch })
    await assert.rejects(dial({ channel: ChannelScope.Control }))
  }
  assert.deepEqual(bodies, [
    { environment_id: '', executor: executorInfo, instance_id: 'brain-host-7d9f-x2' },
    { environment_id: '', executor: executorInfo },
  ])
})

test('BMA_MACHINE_ID supplies the machine id unless an environmentId is set', async () => {
  assert.equal(MACHINE_ID_ENV, 'BMA_MACHINE_ID')
  const previous = process.env[MACHINE_ID_ENV]
  process.env[MACHINE_ID_ENV] = 'mach_env'
  try {
    assert.equal(await tokenUrl({ environmentId: '' }), 'https://bma.test/v1/machines/mach_env/token')
    assert.equal(await tokenUrl({ environmentId: 'env' }), 'https://bma.test/v1/environments/env/token')
  } finally {
    if (previous === undefined) delete process.env[MACHINE_ID_ENV]
    else process.env[MACHINE_ID_ENV] = previous
  }
})

test('environmentId and machineId are alternatives', () => {
  assert.throws(
    () => new ExecutorClient({ environmentId: 'env', machineId: 'mach_1', executor: executorInfo, environmentKey: 'k' }),
    /environmentId and machineId/,
  )
})
