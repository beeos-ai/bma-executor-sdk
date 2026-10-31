// A cloud executor: token bootstrap with an environment key, outbound
// WebSocket, reconnect with resume, and one tool (bash).
//
//   BMA_ENVIRONMENT_KEY=... BMA_ENVIRONMENT_ID=env_... node executor-minimal/main.mjs
//   (BMA_BASE_URL defaults to https://api.bma.beeos.ai)
import { exec } from 'node:child_process'
import os from 'node:os'

import { ExecutorClient } from '@beeos-ai/bma-sdk/executor'

const client = new ExecutorClient({
  baseUrl: process.env.BMA_BASE_URL,
  environmentKey: process.env.BMA_ENVIRONMENT_KEY,
  environmentId: process.env.BMA_ENVIRONMENT_ID ?? '',
  executor: { name: 'executor-minimal', os: process.platform, arch: process.arch, version: '0.1.0' },
  capabilities: { shell: { enabled: true } },
  onLog: ({ level, message }) => console.error(`[${level}] ${message}`),
})

client.onSessionAssign(async (assign) => {
  // Prepare the workspace here, then report ready.
  setImmediate(() => void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token }))
  return { accepted: true, session_id: assign.session_id }
})

client.handleTool('bash', (call, { signal }) =>
  new Promise((resolve) => {
    exec(call.input.command ?? '', { cwd: call.input.cwd ?? os.tmpdir(), signal }, (error, stdout, stderr) => {
      resolve({
        outcome: error ? 'failed' : 'succeeded',
        is_error: Boolean(error),
        content: [{ type: 'text', text: stdout + stderr }],
      })
    })
  }),
)

const registered = await client.start()
console.log(`registered as ${registered.executor_id} (${client.protocol})`)
process.on('SIGINT', () => {
  client.stop('interrupted')
  process.exit(0)
})
