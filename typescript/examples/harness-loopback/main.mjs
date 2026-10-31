// A local brain and a local hand in two roles of one machine: the executor
// listens on a local socket (Unix domain socket / Windows named pipe) with a
// per-start token, and the harness dials it. Then the harness assigns a
// session and runs bash on the executor. Run: node harness-loopback/main.mjs
import { exec } from 'node:child_process'

import {
  createLocalToken,
  defaultLocalEndpoint,
  ExecutorClient,
  Harness,
  listenLocal,
  readLocalToken,
} from '@beeos-ai/bma-sdk/executor'

const endpoint = defaultLocalEndpoint()

// --- the hand: listens locally, one ExecutorClient per accepted brain ---
const token = await createLocalToken(endpoint.tokenFile)
const clients = []
const listener = await listenLocal({ path: endpoint.path, token }, (conn) => {
  const client = new ExecutorClient({
    environmentId: '',
    executor: { name: 'loopback-hand', os: process.platform, arch: process.arch, version: '0.1.0' },
    capabilities: { shell: { enabled: true } },
    dialer: async () => conn,
    reconnect: false,
  })
  client.onSessionAssign(async (assign) => {
    setImmediate(() => void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token }))
    return { accepted: true, session_id: assign.session_id }
  })
  client.handleTool('bash', (call, { signal }) =>
    new Promise((resolve) => {
      exec(call.input.command ?? '', { signal }, (error, stdout, stderr) =>
        resolve({ outcome: error ? 'failed' : 'succeeded', is_error: Boolean(error), content: [{ type: 'text', text: stdout + stderr }] }),
      )
    }),
  )
  clients.push(client)
  void client.start()
})

// --- the brain: reads the local token and dials the hand ---
const harness = new Harness()
await harness.dialLocal({ path: endpoint.path, token: await readLocalToken(endpoint.tokenFile) })
const executor = await harness.waitExecutor(AbortSignal.timeout(5000))
const session = await executor.assignSession({ session_id: 'sess_loopback' })
await session.ready()
const result = await session.callTool({ tool: 'bash', input: { command: 'echo hello from the local hand' } })
console.log(`${executor.info.name} (${executor.protocol}) -> ${result.outcome}: ${result.content[0]?.text?.trim()}`)

await session.stop('done')
for (const client of clients) client.stop('done')
await listener.close()
