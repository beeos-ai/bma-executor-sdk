// A machine-scoped executor whose processes run in a local folder chosen per
// session. The caller puts opaque references in the session metadata, for
// example {"workspace_ref": "ws_7", "owner": "u_42"}; before spawning, the
// process.start handler reads the session context, refuses another owner's
// session (session_rejected) or an unknown reference (workspace_unavailable),
// and runs the command in this machine's folder for the reference. BMA never
// stores the path.
//
// A process.start without metadata.agent_session_id has no session to check
// against: it is refused with session_rejected, never run in a default folder
// (the harness does not send one; a person's console terminal is
// terminal.attach, not process.start). The exception is a process the platform
// starts on its own (metadata initiator "platform", for example the machine
// desktop agent): with EXAMPLE_PLATFORM_DIR set it runs in that directory by
// this machine's policy, never in a session folder; without it, it is refused
// too. A package install runs a package manager that writes outside the
// folder: this example has no local confirmation, so it refuses with
// local_confirmation_denied.
//
// TypeScript has no built-in process runtime (Go's ProcessOptions.Launch):
// the executor serves process.* itself. This example keeps output in memory
// and serves process.start, process.read and process.list only.
//
//   BMA_BASE_URL=https://api.<env>.bma.beeos.ai BMA_MACHINE_TOKEN=... \
//   EXAMPLE_OWNER=u_42 EXAMPLE_FOLDERS='{"ws_7":"/Users/me/projects/site"}' \
//   EXAMPLE_PLATFORM_DIR=/Users/me/bma-platform \
//   node session-context/main.mjs
import { spawn } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import path from 'node:path'

import { Code, ErrorReason, ExecutorClient, RpcError } from '@beeos-ai/bma-executor-sdk'

const owner = process.env.EXAMPLE_OWNER
const folders = JSON.parse(process.env.EXAMPLE_FOLDERS ?? '{}')
// Where platform-initiated processes run; empty means this machine hosts none.
const platformDir = process.env.EXAMPLE_PLATFORM_DIR ?? ''
if (platformDir) mkdirSync(platformDir, { recursive: true })

const client = new ExecutorClient({
  baseUrl: process.env.BMA_BASE_URL,
  // A machine daemon token and no environment id: a machine-scoped executor.
  environmentKey: process.env.BMA_MACHINE_TOKEN,
  environmentId: '',
  executor: { name: 'session-context-example', os: process.platform, arch: process.arch, version: '0.1.0' },
  capabilities: { process: { enabled: true, max_processes: 8, buffer_bytes: 1 << 20 } },
  onLog: ({ level, message }) => console.error(`[${level}] ${message}`),
})

const processes = new Map()

// Resolve cwd (relative to folder) and refuse one outside it.
function confine(folder, cwd) {
  const resolved = path.resolve(folder, cwd ?? '.')
  const rel = path.relative(folder, resolved)
  if (rel === '..' || rel.startsWith(`..${path.sep}`) || path.isAbsolute(rel)) {
    throw RpcError.withReason(Code.Forbidden, ErrorReason.WorkspaceBoundary, 'cwd is outside the allowed folder')
  }
  return resolved
}

const installRefusal = () =>
  RpcError.withReason(Code.Forbidden, ErrorReason.LocalConfirmationDenied, 'package installs need a local confirmation this executor does not offer')

// No execution Core in TypeScript: accept the session and report it ready
// (nothing to prepare here), or the platform never uses this machine.
client.onSessionAssign(async (assign) => {
  setImmediate(() => void client.sessionReady({ session_id: assign.session_id, work_id: assign.work_id, lease_token: assign.lease.token }))
  return { accepted: true, session_id: assign.session_id }
})

client.onRequest('process.start', async (params) => {
  const existing = processes.get(params.process_id)
  if (existing) return state(existing) // a replayed start
  let cwd
  if (params.metadata?.agent_session_id) {
    if (params.package_install) throw installRefusal()
    const session = await client.sessionContext(params.process_id)
    if (session.metadata.owner !== owner) {
      throw RpcError.withReason(Code.Forbidden, ErrorReason.SessionRejected, 'this machine does not serve the session owner')
    }
    const folder = folders[session.metadata.workspace_ref]
    if (!folder) throw RpcError.withReason(Code.Forbidden, ErrorReason.WorkspaceUnavailable, 'unknown workspace on this machine')
    cwd = confine(folder, params.cwd)
  } else if (platformDir && params.metadata?.initiator === 'platform') {
    // Started by the platform on its own: no session, its own directory.
    if (params.package_install) throw installRefusal()
    cwd = confine(platformDir, params.cwd)
  } else {
    throw RpcError.withReason(Code.Forbidden, ErrorReason.SessionRejected, 'the process names no agent session')
  }
  const child = spawn(params.argv[0], params.argv.slice(1), { cwd, env: { ...process.env, ...params.env }, stdio: ['ignore', 'pipe', 'pipe'] })
  // stderr "merge" (the harness asks for it): one stream, one cursor, reported in the state.
  const merge = params.stderr === 'merge'
  const proc = { id: params.process_id, startedAt: new Date().toISOString(), stdout: [], stderr: [], exitCode: null, state: 'running', merge }
  child.stdout.on('data', (chunk) => proc.stdout.push(chunk))
  child.stderr.on('data', (chunk) => (merge ? proc.stdout : proc.stderr).push(chunk))
  child.on('close', (code) => Object.assign(proc, { exitCode: code ?? -1, state: 'exited', endedAt: new Date().toISOString() }))
  processes.set(params.process_id, proc)
  return state(proc)
})

// The platform lists processes when it reconciles a session; a machine-scoped
// executor that declares process must answer it.
client.onRequest('process.list', async () => ({ processes: [...processes.values()].map(state) }))

client.onRequest('process.read', async ({ process_id, stream, cursor }) => {
  const proc = processes.get(process_id)
  if (!proc) throw new RpcError(Code.NotFound, 'process not found')
  const all = Buffer.concat(proc[stream])
  const data = all.subarray(cursor)
  return {
    process_id, stream, data: data.toString('base64'), from_cursor: cursor, next_cursor: all.length, earliest_cursor: 0,
    gap: false, truncated: false, eof: proc.state !== 'running', state: proc.state, ended_at: proc.endedAt,
    exit_code: proc.exitCode, exit_signal: null, ...(proc.merge ? { stderr: 'merge' } : {}),
  }
})

function state(proc) {
  return {
    accepted: true, process_id: proc.id, state: proc.state, started_at: proc.startedAt, ended_at: proc.endedAt,
    cursors: { stdout: Buffer.concat(proc.stdout).length, stderr: Buffer.concat(proc.stderr).length },
    exit_code: proc.exitCode, exit_signal: null, ...(proc.merge ? { stderr: 'merge' } : {}),
  }
}

const registered = await client.start()
console.log(`registered as ${registered.executor_id}`)
process.on('SIGINT', () => {
  client.stop('interrupted')
  process.exit(0)
})
