// baseUrl is the deployment origin (like BMA_BASE_URL in bma-sdk): the token
// bootstrap posts to {origin}/v1/executors/token, and a trailing /api/v1 is dropped.
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { ChannelScope, DEFAULT_BASE_URL, webSocketDialer } from '../src/index.js'
import { executorInfo } from './helpers.js'

test('baseUrl is the origin; a trailing /api/v1 is not repeated', async () => {
  assert.equal(DEFAULT_BASE_URL, 'https://api.bma.beeos.ai')
  for (const baseUrl of ['https://bma.test', 'https://bma.test/', 'https://bma.test/api/v1', 'https://bma.test/api/v1/']) {
    const urls: string[] = []
    const fetch = (async (url: string) => {
      urls.push(String(url))
      return new Response('nope', { status: 500 })
    }) as typeof globalThis.fetch
    const dial = webSocketDialer({ environmentId: 'env', executor: executorInfo, baseUrl, environmentKey: 'k', fetch })
    await assert.rejects(dial({ channel: ChannelScope.Control }))
    assert.deepEqual(urls, ['https://bma.test/v1/executors/token'], baseUrl)
  }
})

test('a machine token (no environmentId) is exchanged by bma-api at /api/v1/executors/token', async () => {
  const urls: string[] = []
  const fetch = (async (url: string) => {
    urls.push(String(url))
    return new Response('nope', { status: 500 })
  }) as typeof globalThis.fetch
  const dial = webSocketDialer({ environmentId: '', executor: executorInfo, baseUrl: 'https://bma.test', environmentKey: 'mtok', fetch })
  await assert.rejects(dial({ channel: ChannelScope.Control }))
  assert.deepEqual(urls, ['https://bma.test/api/v1/executors/token'])
})
