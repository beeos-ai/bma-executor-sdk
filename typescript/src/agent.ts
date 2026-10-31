// Agent capability, executor side: a complete local agent served over the
// executor protocol. The SDK owns turn.start/input/cancel, session.close,
// event ids and sequences, ACK tracking and resend after reconnect,
// interactions, and the session.status_running / status_idle events that
// frame every turn. Mirrors go/executor agent.go.

import {
  JsonRpcErrorCode,
  type Ack,
  type AgentCapability,
  type AgentEvent,
  type ConnectionState,
  type EventDeltaType,
  type InteractionCancelParams,
  type InteractionCancelReason,
  type InteractionRequestParams,
  type InteractionRequestResult,
  type InteractionResponseParams,
  type InteractionResponseResult,
  type SessionCloseParams,
  type SessionCloseResult,
  type SessionEndedParams,
  type SessionEvent,
  type SessionEventDeltaParams,
  type SessionEventParams,
  type SessionEventResult,
  type TurnCancelParams,
  type TurnCancelResult,
  type TurnInputParams,
  type TurnStartParams,
  type TurnStartResult,
} from '../generated/executor.js'
import { AgentSessionClosedError, ClientStoppedError, deferred, RpcError, type Deferred } from './errors.js'
import type { LogLevel } from './rpc.js'

const RESEND_DELAY_MS = 200
const CLOSE_FLUSH_WAIT_MS = 30_000

export type { ConnectionState }

/** A complete local agent (model loop and tools). */
export interface AgentHandler {
  /**
   * Decides whether to accept a turn.start and reports the configuration the
   * agent will apply. It runs before turn.start is answered, so it must not
   * do the turn's work. A thrown RpcError keeps its code.
   */
  openTurn(params: TurnStartParams): Promise<TurnStartResult>
  /**
   * Does the turn's work. signal is aborted by turn.cancel, session.close and
   * deadline_ms. Resolving ends the turn as completed, an aborted signal as
   * canceled (failed for deadline_ms), a rejection as failed (the SDK emits
   * session.error first).
   */
  runTurn(turn: AgentTurn, signal: AbortSignal): Promise<void>
  /** Releases the local conversation after session.close. */
  closeSession(sessionId: string, reason: string): Promise<void>
}

/** What the agent runtime needs from the executor client. */
export interface AgentHost {
  call<T>(method: string, params: unknown): Promise<T>
  notify(method: string, params: unknown): void
  readonly stopSignal: AbortSignal
  connected(): boolean
  /** Resolves after the next successful executor.register. */
  nextRegistration(): Promise<void>
  addSession(id: string): void
  removeSession(id: string): void
  sessionEnded(params: SessionEndedParams): Promise<Ack>
  log(level: LogLevel, message: string, error?: unknown): void
}

class DeadlineExceeded extends Error {
  constructor() {
    super('turn deadline_ms exceeded')
  }
}

/** An unbounded async queue with one consumer. */
class InputQueue<T> implements AsyncIterable<T> {
  private readonly items: T[] = []
  private waiter?: Deferred<IteratorResult<T>>
  private ended = false

  push(item: T): void {
    if (this.waiter) {
      const waiter = this.waiter
      this.waiter = undefined
      waiter.resolve({ value: item, done: false })
    } else this.items.push(item)
  }

  end(): void {
    this.ended = true
    this.waiter?.resolve({ value: undefined, done: true })
    this.waiter = undefined
  }

  next(): Promise<IteratorResult<T>> {
    const item = this.items.shift()
    if (item !== undefined) return Promise.resolve({ value: item, done: false })
    if (this.ended) return Promise.resolve({ value: undefined, done: true })
    this.waiter = deferred<IteratorResult<T>>()
    return this.waiter.promise
  }

  [Symbol.asyncIterator](): AsyncIterator<T> {
    return { next: () => this.next() }
  }
}

/** The handle runTurn uses to talk to the platform. */
export class AgentTurn {
  ended = false
  readonly abort = new AbortController()
  readonly done = deferred()
  readonly seen = new Set<string>()
  private readonly queue = new InputQueue<TurnInputParams>()

  constructor(
    private readonly session: AgentSession,
    readonly params: TurnStartParams,
  ) {
    this.seen.add(params.input_id)
  }

  get sessionId(): string {
    return this.session.id
  }
  get turnId(): string {
    return this.params.turn_id
  }

  /** turn.input messages in arrival order; the iteration ends with the turn. */
  get inputs(): AsyncIterable<TurnInputParams> & { next(): Promise<IteratorResult<TurnInputParams>> } {
    return this.queue
  }

  /** @internal */
  pushInput(input: TurnInputParams): void {
    this.queue.push(input)
  }

  /** @internal */
  finish(): void {
    this.ended = true
    this.queue.end()
  }

  /** A fresh event id, for a message streamed with delta before emitWithId. */
  newEventId(): string {
    return this.session.newEventId()
  }

  /**
   * Queues one durable event and returns its id. Delivery is asynchronous,
   * ordered and at-least-once; the platform deduplicates by event id.
   */
  emit(event: AgentEvent): string {
    return this.emitWithId(this.newEventId(), event)
  }

  emitWithId(eventId: string, event: AgentEvent): string {
    if (this.ended) throw new Error('turn already ended')
    this.session.enqueue(this.params.turn_id, eventId, event)
    return eventId
  }

  /** Streams a transient preview of the durable event eventId; dropped while disconnected, never resent. */
  delta(eventId: string, eventType: EventDeltaType, index: number, text: string): void {
    const params: SessionEventDeltaParams = {
      session_id: this.session.id,
      turn_id: this.params.turn_id,
      event_id: eventId,
      event_type: eventType,
      text,
      ...(index ? { index } : {}),
    }
    try {
      this.session.host.notify('session.event_delta', params)
    } catch (error) {
      this.session.host.log('debug', 'session.event_delta dropped', error)
    }
  }

  /** Resolves once every event emitted so far is acknowledged. */
  flush(signal?: AbortSignal): Promise<void> {
    return this.session.flush(signal)
  }

  /**
   * Asks the platform for an approval or an answer and waits for
   * interaction.response. It flushes first so the platform already has the
   * agent.tool_use the request references. When signal aborts or expires_at
   * passes, it withdraws the interaction with interaction.cancel; an expiry
   * resolves with outcome expired, which the agent must treat as a deny.
   */
  async requestInteraction(
    request: Omit<InteractionRequestParams, 'session_id' | 'turn_id' | 'interaction_id'> & { interaction_id?: string },
    signal?: AbortSignal,
  ): Promise<InteractionResponseParams> {
    const session = this.session
    const params: InteractionRequestParams = {
      ...request,
      session_id: session.id,
      turn_id: this.params.turn_id,
      interaction_id: request.interaction_id || `int_${this.newEventId().slice(4)}`,
    }
    const answer = deferred<InteractionResponseParams>()
    session.pending.set(params.interaction_id, answer)
    try {
      await this.flush(signal)
      await session.callUntilConnected(() => session.host.call<InteractionRequestResult>('interaction.request', params), signal)
      const waits: Array<Promise<InteractionResponseParams>> = [answer.promise]
      let timer: ReturnType<typeof setTimeout> | undefined
      if (params.expires_at) {
        const at = Date.parse(params.expires_at)
        if (Number.isNaN(at)) throw new Error(`invalid expires_at ${params.expires_at}`)
        waits.push(
          new Promise((resolve) => {
            timer = setTimeout(() => {
              void session.withdraw(params.interaction_id, 'expired')
              resolve({ session_id: session.id, interaction_id: params.interaction_id, outcome: 'expired' })
            }, at - Date.now())
          }),
        )
      }
      if (signal) {
        waits.push(
          new Promise((_resolve, reject) => {
            const onAbort = () => {
              void session.withdraw(params.interaction_id, 'turn_canceled')
              reject(signal.reason)
            }
            if (signal.aborted) onAbort()
            else signal.addEventListener('abort', onAbort, { once: true })
          }),
        )
      }
      try {
        return await Promise.race(waits)
      } finally {
        clearTimeout(timer)
      }
    } finally {
      session.pending.delete(params.interaction_id)
    }
  }

  /** Withdraws an open interaction, for example one the user answered locally (resolved_locally). */
  cancelInteraction(interactionId: string, reason: InteractionCancelReason): Promise<void> {
    return this.session.withdraw(interactionId, reason)
  }
}

interface OutboxEvent {
  params: SessionEventParams
}

/** One platform session on this executor: its outbox of durable events and at most one running turn. */
class AgentSession {
  private nextSequence = 1
  private nextId = 0
  private readonly outbox: OutboxEvent[] = []
  private wake = deferred()
  private drained = deferred()
  closed = false
  turn?: AgentTurn
  readonly results = new Map<string, TurnStartResult>()
  readonly pending = new Map<string, Deferred<InteractionResponseParams>>()
  readonly answered = new Set<string>()

  constructor(
    readonly host: AgentHost,
    readonly id: string,
  ) {
    this.drained.resolve()
    void this.sendLoop()
  }

  newEventId(): string {
    return `evt_${this.id}_${++this.nextId}`
  }

  enqueue(turnId: string, eventId: string, event: AgentEvent): void {
    if (this.closed) throw new AgentSessionClosedError()
    const params: SessionEventParams = {
      session_id: this.id,
      turn_id: turnId,
      event_id: eventId,
      sequence: this.nextSequence++,
      event: event as unknown as SessionEvent,
    }
    if (this.outbox.length === 0) this.drained = deferred()
    this.outbox.push({ params })
    this.signal()
  }

  signal(): void {
    this.wake.resolve()
  }

  async flush(signal?: AbortSignal): Promise<void> {
    if (!signal) return this.drained.promise
    await Promise.race([
      this.drained.promise,
      new Promise<never>((_resolve, reject) => {
        if (signal.aborted) reject(signal.reason)
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
      }),
    ])
  }

  /**
   * Delivers the outbox head first, one event at a time, so the platform sees
   * every session in sequence order. A transport failure keeps the event and
   * waits for the next registration: the resend after reconnect.
   */
  private async sendLoop(): Promise<void> {
    for (;;) {
      const head = this.outbox[0]
      if (!head) {
        if (this.closed || this.host.stopSignal.aborted) return
        this.wake = deferred()
        await this.wake.promise
        continue
      }
      try {
        await this.callUntilConnected(() => this.host.call<SessionEventResult>('session.event', head.params))
      } catch (error) {
        if (this.host.stopSignal.aborted) return
        // A permanent rejection cannot be fixed by resending; surface it instead of blocking the session.
        this.host.log('error', `session.event ${head.params.event_id} rejected`, error)
      }
      this.outbox.shift()
      if (this.outbox.length === 0) this.drained.resolve()
    }
  }

  /** Retries call across disconnects; only transport failures, executor_busy and retryable errors are retried. */
  async callUntilConnected<T>(call: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    for (;;) {
      const registered = this.host.nextRegistration()
      try {
        return await call()
      } catch (error) {
        if (error instanceof RpcError && error.data?.retryable !== true && error.code !== JsonRpcErrorCode.ExecutorBusy) throw error
        if (error instanceof ClientStoppedError || this.host.stopSignal.aborted) throw error
      }
      const waits: Array<Promise<unknown>> = [registered]
      if (this.host.connected()) waits.push(new Promise((resolve) => setTimeout(resolve, RESEND_DELAY_MS)))
      const stop = signal ?? this.host.stopSignal
      waits.push(
        new Promise((_resolve, reject) => {
          if (stop.aborted) reject(stop.reason)
          stop.addEventListener('abort', () => reject(stop.reason), { once: true })
        }),
      )
      if (signal) {
        const stopped = this.host.stopSignal
        waits.push(new Promise((_resolve, reject) => stopped.addEventListener('abort', () => reject(stopped.reason), { once: true })))
      }
      await Promise.race(waits)
    }
  }

  async withdraw(interactionId: string, reason: InteractionCancelReason): Promise<void> {
    this.answered.add(interactionId)
    const params: InteractionCancelParams = { session_id: this.id, interaction_id: interactionId, reason }
    try {
      await this.callUntilConnected(
        () => this.host.call<Ack>('interaction.cancel', params),
        AbortSignal.timeout(CLOSE_FLUSH_WAIT_MS),
      )
    } catch (error) {
      this.host.log('warn', `interaction.cancel for ${interactionId} failed`, error)
    }
  }
}

/** @internal Routes agent methods for one ExecutorClient. */
export class AgentRuntime {
  private readonly sessions = new Map<string, AgentSession>()

  constructor(
    private readonly host: AgentHost,
    private readonly handler: AgentHandler,
    readonly capability: AgentCapability,
  ) {}

  /** Handles an agent method; undefined for every other method. */
  dispatch(method: string, params: unknown): Promise<unknown> | undefined {
    switch (method) {
      case 'turn.start':
        return this.turnStart(params as TurnStartParams)
      case 'turn.input':
        return this.turnInput(params as TurnInputParams)
      case 'turn.cancel':
        return Promise.resolve(this.turnCancel(params as TurnCancelParams))
      case 'session.close':
        return this.sessionClose(params as SessionCloseParams)
      case 'interaction.response':
        return Promise.resolve(this.interactionResponse(params as InteractionResponseParams))
      default:
        return undefined
    }
  }

  private session(id: string, create: boolean): AgentSession | undefined {
    const existing = this.sessions.get(id)
    if (existing || !create) return existing
    const max = this.capability.max_sessions ?? 0
    if (max > 0 && this.sessions.size >= max) throw new RpcError(JsonRpcErrorCode.ExecutorBusy, 'executor_busy')
    const session = new AgentSession(this.host, id)
    this.sessions.set(id, session)
    this.host.addSession(id)
    return session
  }

  private async turnStart(params: TurnStartParams): Promise<TurnStartResult> {
    if (!params?.session_id || !params.turn_id || !params.input?.length) {
      throw new RpcError(JsonRpcErrorCode.InvalidParams, 'session_id, turn_id and input are required')
    }
    const session = this.session(params.session_id, true)!
    const recorded = session.results.get(params.turn_id)
    if (recorded) return recorded
    if (session.turn) throw new RpcError(JsonRpcErrorCode.Conflict, 'a turn is already running; use turn.input')
    const turn = new AgentTurn(session, params)
    session.turn = turn // reserve the session before openTurn
    let result: TurnStartResult
    try {
      result = await this.handler.openTurn(params)
      if (!result.accepted) throw new RpcError(JsonRpcErrorCode.ExecutorBusy, 'turn not accepted')
    } catch (error) {
      session.turn = undefined
      throw error
    }
    result = { ...result, turn_id: params.turn_id }
    session.results.set(params.turn_id, result)
    setImmediate(() => void this.runTurn(session, turn))
    return result
  }

  private async runTurn(session: AgentSession, turn: AgentTurn): Promise<void> {
    const turnId = turn.params.turn_id
    const onStop = () => turn.abort.abort(new ClientStoppedError())
    this.host.stopSignal.addEventListener('abort', onStop, { once: true })
    const deadline =
      turn.params.deadline_ms && turn.params.deadline_ms > 0
        ? setTimeout(() => turn.abort.abort(new DeadlineExceeded()), turn.params.deadline_ms)
        : undefined
    try {
      session.enqueue(turnId, session.newEventId(), { type: 'session.status_running' })
      let failure: unknown
      try {
        await this.handler.runTurn(turn, turn.abort.signal)
      } catch (error) {
        failure = error
      }
      turn.finish()
      let outcome: 'completed' | 'canceled' | 'failed' = 'completed'
      if (turn.abort.signal.reason instanceof DeadlineExceeded) {
        outcome = 'failed'
        session.enqueue(turnId, session.newEventId(), {
          type: 'session.error',
          error: { type: 'timeout', message: 'turn deadline_ms exceeded' },
        })
      } else if (turn.abort.signal.aborted) {
        outcome = 'canceled'
      } else if (failure !== undefined) {
        outcome = 'failed'
        session.enqueue(turnId, session.newEventId(), {
          type: 'session.error',
          error: { type: 'agent_error', message: failure instanceof Error ? failure.message : String(failure) },
        })
      }
      session.enqueue(turnId, session.newEventId(), {
        type: 'session.status_idle',
        stop_reason: { type: 'end_turn' },
        turn_outcome: outcome,
      })
    } catch (error) {
      this.host.log('warn', `turn ${turnId} ended after session.close`, error)
    } finally {
      clearTimeout(deadline)
      this.host.stopSignal.removeEventListener('abort', onStop)
      if (session.turn === turn) session.turn = undefined
      turn.done.resolve()
    }
  }

  private async turnInput(params: TurnInputParams): Promise<Ack> {
    const session = this.session(params.session_id, false)
    if (!session) throw new RpcError(JsonRpcErrorCode.SessionNotActive, 'session_not_active')
    const turn = session.turn
    if (!turn || turn.params.turn_id !== params.turn_id || turn.ended) {
      throw new RpcError(JsonRpcErrorCode.Conflict, 'turn is not running; start a new turn')
    }
    if (turn.seen.has(params.input_id)) return { accepted: true }
    turn.seen.add(params.input_id)
    turn.pushInput(params)
    return { accepted: true }
  }

  private turnCancel(params: TurnCancelParams): TurnCancelResult {
    const turn = this.session(params.session_id, false)?.turn
    if (!turn || turn.params.turn_id !== params.turn_id || turn.ended) return { canceled: false }
    turn.abort.abort(new Error('turn.cancel'))
    return { canceled: true }
  }

  private async sessionClose(params: SessionCloseParams): Promise<SessionCloseResult> {
    const session = this.session(params.session_id, false)
    if (!session) return { closed: false }
    const turn = session.turn
    if (turn) {
      turn.abort.abort(new Error('session.close'))
      await turn.done.promise
    }
    try {
      await session.flush(AbortSignal.timeout(CLOSE_FLUSH_WAIT_MS))
    } catch (error) {
      this.host.log('warn', `session.close: events of ${session.id} still unacknowledged`, error)
    }
    await this.handler.closeSession(session.id, params.reason)
    session.closed = true
    session.signal()
    this.sessions.delete(session.id)
    this.host.removeSession(session.id)
    void session
      .callUntilConnected(
        () => this.host.sessionEnded({ session_id: session.id, reason: params.reason }),
        AbortSignal.timeout(CLOSE_FLUSH_WAIT_MS),
      )
      .catch((error) => this.host.log('warn', `session.ended for ${session.id} failed`, error))
    return { closed: true }
  }

  private interactionResponse(params: InteractionResponseParams): InteractionResponseResult {
    const session = this.session(params.session_id, false)
    if (!session) return { accepted: false }
    const waiter = session.pending.get(params.interaction_id)
    if (!waiter || session.answered.has(params.interaction_id)) return { accepted: false }
    session.answered.add(params.interaction_id)
    waiter.resolve(params)
    return { accepted: true }
  }
}
