// session.event failures must never leave a hole in a session's sequence:
// a platform failure is resent, a gap answer renumbers from
// expected_sequence, and a permanent rejection ends the turn visibly at the
// rejected sequence (11-12: one rejected event left a DSH session stuck on
// "sequence gap: expected 3" for good).
import assert from 'node:assert/strict'
import { test } from 'node:test'

import { AgentRuntime, EventRejectedError, type AgentHandler, type AgentHost } from '../src/agent.js'
import { JsonRpcErrorCode, RpcError } from '../src/index.js'
import { waitFor } from './helpers.js'

interface Stored {
  sequence: number
  type: string
  event: Record<string, unknown>
}

/** The platform side of session.event, scripted per call. */
class FakeHost implements AgentHost {
  readonly stored: Stored[] = []
  readonly sent: number[] = []
  acked = 0
  /** Answers the n-th call (0-based) with an error instead of storing it. */
  failures = new Map<number, RpcError>()
  /** Types refused for good, every time. */
  refuse = new Set<string>()
  private calls = 0
  private readonly stop = new AbortController()
  readonly stopSignal = this.stop.signal

  async call(method: string, params: unknown): Promise<never> {
    if (method !== 'session.event') return { accepted: true } as never
    const p = params as { sequence: number; event: Record<string, unknown> }
    this.sent.push(p.sequence)
    const failure = this.failures.get(this.calls++)
    if (failure) throw failure
    if (this.refuse.has(String(p.event.type))) throw new RpcError(JsonRpcErrorCode.InvalidParams, 'bad event')
    if (p.sequence <= this.acked) return { accepted: true, acked_sequence: this.acked } as never
    if (p.sequence !== this.acked + 1) {
      throw new RpcError(JsonRpcErrorCode.Conflict, 'conflict', { retryable: false, expected_sequence: this.acked + 1 })
    }
    this.acked = p.sequence
    this.stored.push({ sequence: p.sequence, type: String(p.event.type), event: p.event })
    return { accepted: true, acked_sequence: this.acked } as never
  }
  notify(): void {}
  connected(): boolean {
    return true
  }
  nextRegistration(): Promise<void> {
    return new Promise(() => {})
  }
  addSession(): void {}
  removeSession(): void {}
  async sessionEnded(): Promise<never> {
    return { accepted: true } as never
  }
  log(): void {}
  end(): void {
    this.stop.abort()
  }
}

/** Emits three messages; "wait" then blocks until aborted and records the reason. */
function agent(reasons: unknown[], release: Promise<void>): AgentHandler {
  return {
    async openTurn(params) {
      return { accepted: true, turn_id: params.turn_id, executor_session_ref: 'ref', resumed: false, effective: {} }
    },
    async runTurn(turn, signal) {
      for (const text of ['one', 'tool', 'two']) {
        if (text === 'tool') turn.emit({ type: 'agent.tool_use', name: 'bash', input: {} })
        else turn.emit({ type: 'agent.message', content: [{ type: 'text', text }] })
      }
      await Promise.race([release, new Promise((resolve) => signal.addEventListener('abort', resolve, { once: true }))])
      reasons.push(signal.reason)
    },
    async closeSession() {},
  }
}

async function run(host: FakeHost, release: Promise<void> = Promise.resolve()) {
  const reasons: unknown[] = []
  const runtime = new AgentRuntime(host, agent(reasons, release), { enabled: true })
  await runtime.dispatch('turn.start', {
    session_id: 'ses_1',
    turn_id: 't_1',
    input_id: 'in_1',
    input: [{ type: 'text', text: 'go' }],
  })
  return reasons
}

test('a platform failure (-32603, not marked retryable) is resent in place, nothing skipped', async () => {
  const host = new FakeHost()
  host.failures.set(2, new RpcError(JsonRpcErrorCode.InternalError, 'internal error'))
  try {
    await run(host)
    await waitFor(() => host.stored.some((s) => s.type === 'session.status_idle'))
    assert.deepEqual(
      host.stored.map((s) => s.type),
      ['session.status_running', 'agent.message', 'agent.tool_use', 'agent.message', 'session.status_idle'],
    )
    assert.deepEqual(host.stored.map((s) => s.sequence), [1, 2, 3, 4, 5])
  } finally {
    host.end()
  }
})

test('a gap answer renumbers the unacknowledged events from expected_sequence', async () => {
  const host = new FakeHost()
  let release!: () => void
  const released = new Promise<void>((resolve) => (release = resolve))
  try {
    await run(host, released)
    await waitFor(() => host.stored.length === 4)
    // The platform starts a new epoch for the session: it expects 1 again.
    host.acked = 0
    host.stored.length = 0
    release()
    await waitFor(() => host.stored.some((s) => s.type === 'session.status_idle'))
    assert.deepEqual(host.stored.map((s) => s.sequence), [1], 'the next event is renumbered to the expected sequence')
    assert.equal(host.sent.at(-2), 5, 'it was first sent as 5 and refused as a gap')
  } finally {
    host.end()
  }
})

test('a permanent rejection ends the turn at that sequence: session.error + status_idle failed, no hole', async () => {
  const host = new FakeHost()
  host.refuse.add('agent.tool_use')
  let release!: () => void
  const released = new Promise<void>((resolve) => (release = resolve))
  try {
    const reasons = await run(host, released)
    await waitFor(() => host.stored.some((s) => s.type === 'session.status_idle'))
    assert.deepEqual(
      host.stored.map((s) => s.type),
      ['session.status_running', 'agent.message', 'session.error', 'session.status_idle'],
    )
    assert.deepEqual(host.stored.map((s) => s.sequence), [1, 2, 3, 4], 'the terminal events take the rejected sequence on')
    assert.match(String((host.stored[2]!.event.error as { message: string }).message), /^event_rejected: /)
    assert.deepEqual(host.stored[3]!.event.stop_reason, { type: 'retries_exhausted' })
    assert.equal(host.stored[3]!.event.turn_outcome, 'failed')
    await waitFor(() => reasons.length === 1)
    assert.ok(reasons[0] instanceof EventRejectedError, 'the turn is stopped')
    release()
    await new Promise((resolve) => setTimeout(resolve, 50))
    assert.equal(host.stored.length, 4, 'nothing of the ended turn follows')
  } finally {
    host.end()
  }
})
