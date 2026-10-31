// Harness ("brain") end of the BMA executor protocol: accepts executor
// control and MCP channels on any transport and drives them with
// session.assign / tool.call / tool.cancel / session.stop, plus the agent
// capability. It speaks the same protocol as the BMA service, so an executor
// cannot tell a local harness from the cloud. Mirrors go/executor Harness.

import { randomBytes } from 'node:crypto'
import { createServer, type IncomingMessage, type Server } from 'node:http'
import type { Duplex } from 'node:stream'
import { WebSocketServer } from 'ws'

import {
  ChannelScope,
  JsonRpcErrorCode,
  ProtocolVersion,
  type Ack,
  type AgentCapability,
  type Capabilities,
  type ExecutorDrainResult,
  type ExecutorHeartbeatResult,
  type ExecutorInfo,
  type ExecutorRegisterParams,
  type ExecutorRegisterResult,
  type InteractionCancelParams,
  type InteractionRequestParams,
  type InteractionRequestResult,
  type InteractionResponseParams,
  type InteractionResponseResult,
  type SessionAssignParams,
  type SessionAssignResult,
  type SessionCloseParams,
  type SessionCloseResult,
  type SessionEndedParams,
  type SessionEventDeltaParams,
  type SessionEventParams,
  type SessionEventResult,
  type SessionReadyParams,
  type SessionStatusParams,
  type SessionStopResult,
  type ToolCallParams,
  type ToolCancelResult,
  type ToolProgressParams,
  type ToolResultParams,
  type TurnCancelParams,
  type TurnCancelResult,
  type TurnInputParams,
  type TurnStartParams,
  type TurnStartResult,
} from '../generated/executor.js'
import type { ChannelTarget, Dialer } from './client.js'
import {
  AssignRejectedError,
  deferred,
  NotConnectedError,
  rpcError,
  RpcError,
  SessionEndedError,
  ToolOutcomeUnknownError,
  type Deferred,
} from './errors.js'
import { DEFAULT_PROTOCOLS, dialLocal, type DialLocalOptions } from './local.js'
import { RpcPeer, type LogLevel, type Logger } from './rpc.js'
import { FrameChannel, inProcessPair, MAX_FRAME_BYTES, webSocketConn, type Conn } from './transport.js'

export const DEFAULT_HARNESS_HEARTBEAT_INTERVAL_MS = 15_000
export const DEFAULT_HARNESS_LEASE_TTL_MS = 60_000
const DEFAULT_REQUEST_TIMEOUT_MS = 30_000
const MAX_REMEMBERED_EVENT_IDS = 1024

/** What authenticate learns from a connection. A non-empty executorId pins the executor identity. */
export interface Principal {
  executorId?: string
}

/** One durable agent event, validated. */
export interface AgentEventRecord {
  sessionId: string
  turnId: string
  eventId: string
  sequence: number
  type: string
  /** The event object as sent, for example an agent.message event. */
  event: Record<string, unknown>
}

/** The platform half of the agent capability. */
export interface AgentPlatform {
  /** Highest contiguous sequence already persisted for the session (0 when none). Asked once per session. */
  ackedSequence(sessionId: string): Promise<number>
  /** Persists one durable event; resolves once durable. Called once per event, in sequence order per session. */
  agentEvent(executor: HarnessExecutor, event: AgentEventRecord): Promise<void>
  /** Relays a transient preview. Never persisted. */
  agentDelta(executor: HarnessExecutor, delta: SessionEventDeltaParams): void
  /** Opens (or for a repeated interaction_id returns) the platform interaction; returns its id. */
  interactionRequested(executor: HarnessExecutor, request: InteractionRequestParams): Promise<string>
  /** Closes the platform interaction without an answer. */
  interactionCanceled(executor: HarnessExecutor, cancel: InteractionCancelParams): Promise<void>
}

export interface HarnessOptions {
  /**
   * Verifies every WebSocket upgrade (control and MCP). Throwing answers 401.
   * Omitted accepts every connection: use that only on loopback.
   */
  authenticate?: (request: IncomingMessage) => Principal | Promise<Principal>
  /** Returned by executor.register. A disconnected executor keeps its sessions and calls for leaseTtlMs. */
  heartbeatIntervalMs?: number
  leaseTtlMs?: number
  requestTimeoutMs?: number
  /** Accepted subprotocols in preference order; defaults to v1.1 then v1. */
  protocols?: string[]
  onExecutor?: (executor: HarnessExecutor) => void
  /** Persists a tool executor's session.event; omitted accepts with a generated id. */
  onSessionEvent?: (executor: HarnessExecutor, params: SessionEventParams) => Promise<SessionEventResult>
  onSessionStatus?: (executor: HarnessExecutor, params: SessionStatusParams) => void
  onToolProgress?: (executor: HarnessExecutor, params: ToolProgressParams) => void
  onLog?: Logger
  /** Receives agent-capability traffic; omitted rejects it as unknown methods. */
  agent?: AgentPlatform
}

function randomId(bytes: number): string {
  return randomBytes(bytes).toString('hex')
}

/** Where a connection came from: its principal and, for MCP, its session and server key. */
export interface AcceptOptions {
  principal?: Principal
  channel?: ChannelScope
  sessionId?: string
  serverKey?: string
}

export class Harness {
  readonly options: Required<Pick<HarnessOptions, 'heartbeatIntervalMs' | 'leaseTtlMs' | 'requestTimeoutMs' | 'protocols'>> &
    HarnessOptions
  private readonly executorsById = new Map<string, HarnessExecutor>()
  /** @internal */
  readonly sessionsById = new Map<string, HarnessSession>()
  private changed = deferred()
  private nextConn = 0
  private readonly wss: WebSocketServer
  /** @internal */
  readonly ledger = new AgentLedger()

  constructor(options: HarnessOptions = {}) {
    this.options = {
      ...options,
      heartbeatIntervalMs: options.heartbeatIntervalMs ?? DEFAULT_HARNESS_HEARTBEAT_INTERVAL_MS,
      leaseTtlMs: options.leaseTtlMs ?? DEFAULT_HARNESS_LEASE_TTL_MS,
      requestTimeoutMs: options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
      protocols: options.protocols ?? DEFAULT_PROTOCOLS,
    }
    this.wss = new WebSocketServer({
      noServer: true,
      maxPayload: MAX_FRAME_BYTES,
      handleProtocols: (offered) => this.options.protocols.find((protocol) => offered.has(protocol)) ?? false,
    })
  }

  /** Executors currently registered (connected or within their lease). */
  executors(): HarnessExecutor[] {
    return [...this.executorsById.values()]
  }

  /** Resolves with a connected executor, waiting for one to register. */
  async waitExecutor(signal?: AbortSignal): Promise<HarnessExecutor> {
    for (;;) {
      const executor = this.executors().find((candidate) => candidate.connected)
      if (executor) return executor
      const changed = this.changed.promise
      await (signal
        ? Promise.race([
            changed,
            new Promise((_resolve, reject) => {
              if (signal.aborted) reject(signal.reason)
              signal.addEventListener('abort', () => reject(signal.reason), { once: true })
            }),
          ])
        : changed)
    }
  }

  /** @internal */
  signal(): void {
    const changed = this.changed
    this.changed = deferred()
    changed.resolve()
  }

  /**
   * Handles an HTTP upgrade for /v1/executors/channel style URLs: the control
   * channel by default, an MCP channel with ?channel=mcp&session_id=&server_key=.
   */
  async handleUpgrade(request: IncomingMessage, socket: Duplex, head: Buffer): Promise<void> {
    const refuse = (status: number, message: string) => {
      socket.end(`HTTP/1.1 ${status} ${message}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`)
    }
    let principal: Principal = {}
    if (this.options.authenticate) {
      try {
        principal = await this.options.authenticate(request)
      } catch {
        refuse(401, 'Unauthorized')
        return
      }
    }
    const query = new URL(request.url ?? '/', 'http://localhost').searchParams
    const channel = query.get('channel') || ChannelScope.Control
    if (channel !== ChannelScope.Control && channel !== ChannelScope.Mcp) {
      refuse(400, 'Bad Request')
      return
    }
    const offered = (request.headers['sec-websocket-protocol'] ?? '').split(',').map((value) => value.trim())
    if (!this.options.protocols.some((protocol) => offered.includes(protocol))) {
      refuse(400, 'Bad Request')
      return
    }
    const target: AcceptOptions = {
      principal,
      channel,
      sessionId: query.get('session_id') ?? undefined,
      serverKey: query.get('server_key') ?? undefined,
    }
    if (channel === ChannelScope.Mcp && !this.mcpAllowed(target)) {
      refuse(403, 'Forbidden')
      return
    }
    this.wss.handleUpgrade(request, socket, head, (ws) => this.accept(webSocketConn(ws), target))
  }

  /** Serves executor channels on an HTTP server's upgrade event. */
  attach(server: Server): void {
    server.on('upgrade', (request, socket, head) => void this.handleUpgrade(request, socket, head))
  }

  /** Listens on host:port (defaults 127.0.0.1, an ephemeral port) and returns the ws:// URL. */
  async listen(port = 0, host = '127.0.0.1'): Promise<{ url: string; close(): Promise<void> }> {
    const server = createServer((_request, response) => response.writeHead(426).end())
    this.attach(server)
    await new Promise<void>((resolve) => server.listen(port, host, resolve))
    const address = server.address()
    if (!address || typeof address === 'string') throw new Error('unexpected listen address')
    return {
      url: `ws://${host}:${address.port}/v1/executors/channel`,
      close: () =>
        new Promise((resolve) => {
          for (const client of this.wss.clients) client.terminate()
          server.closeAllConnections()
          server.close(() => resolve())
        }),
    }
  }

  /** Dials an executor that listens on a local socket (the local "who dials" inversion). */
  async dialLocal(options: Omit<DialLocalOptions, 'channel'>): Promise<void> {
    this.accept(await dialLocal({ protocols: this.options.protocols, ...options, channel: 'control' }), {})
  }

  /** A Dialer for an ExecutorClient in the same process. */
  inProcessDialer(principal: Principal = {}): Dialer {
    return async (target: ChannelTarget) => {
      const options: AcceptOptions = { principal, channel: target.channel, sessionId: target.sessionId, serverKey: target.serverKey }
      if (target.channel === ChannelScope.Mcp && !this.mcpAllowed(options)) throw new Error('session is not active for this executor')
      const [executorEnd, harnessEnd] = inProcessPair(this.options.protocols[0] ?? ProtocolVersion.BmaExecutorV11)
      this.accept(harnessEnd, options)
      return executorEnd
    }
  }

  /** Takes over an established connection (any transport). */
  accept(conn: Conn, options: AcceptOptions = {}): void {
    if (options.channel === ChannelScope.Mcp) {
      const session = options.sessionId ? this.sessionsById.get(options.sessionId) : undefined
      if (!session || !options.serverKey || !this.mcpAllowed(options)) {
        conn.close(1008, 'session is not active for this executor')
        return
      }
      session.attachMcp(options.serverKey, new HarnessMcpChannel(conn))
      return
    }
    new HarnessConn(this, conn, options.principal ?? {}, `conn_${++this.nextConn}_${randomId(4)}`)
  }

  /** Only the executor holding the session may serve its MCP servers. */
  private mcpAllowed(options: AcceptOptions): boolean {
    const session = options.sessionId ? this.sessionsById.get(options.sessionId) : undefined
    if (!session || !options.serverKey) return false
    const pinned = options.principal?.executorId
    return !pinned || pinned === session.executor.id
  }

  /** @internal */
  executorFor(id: string): HarnessExecutor {
    let executor = this.executorsById.get(id)
    if (!executor) {
      executor = new HarnessExecutor(this, id)
      this.executorsById.set(id, executor)
    }
    return executor
  }

  /** @internal */
  forgetExecutor(executor: HarnessExecutor): void {
    if (this.executorsById.get(executor.id) === executor) this.executorsById.delete(executor.id)
    this.signal()
  }

  /** @internal */
  log(level: LogLevel, message: string, error?: unknown): void {
    this.options.onLog?.({ level, message, error })
  }
}

/** One registered executor; it outlives a connection within its lease. */
export class HarnessExecutor {
  /** @internal */ conn?: HarnessConn
  info: ExecutorInfo = { name: '', os: '', arch: '', version: '' }
  /** The negotiated protocol of the current connection. */
  protocol = ''
  environmentId = ''
  capabilities: Capabilities = {}
  /** @internal */ readonly sessionsById = new Map<string, HarnessSession>()
  /** @internal */ leaseTimer?: ReturnType<typeof setTimeout>
  /** @internal */ draining = false
  /** @internal */ gone = false
  /** @internal */ readonly eventIds = new Map<string, SessionEventResult>()

  constructor(
    private readonly harness: Harness,
    readonly id: string,
  ) {}

  get connected(): boolean {
    return this.conn !== undefined
  }

  sessions(): HarnessSession[] {
    return [...this.sessionsById.values()]
  }

  /** @internal */
  call<T>(method: string, params: unknown): Promise<T> {
    if (!this.conn) return Promise.reject(new NotConnectedError())
    return this.conn.peer.call<T>(method, params)
  }

  /** Asks the executor to stop accepting sessions; heartbeats report drain=true from now on. */
  drain(reason: string): Promise<ExecutorDrainResult> {
    this.draining = true
    return this.call<ExecutorDrainResult>('executor.drain', { reason })
  }

  /**
   * Binds a session and sends session.assign. work_id and the lease token are
   * generated when empty. The session is bound before the request is sent, so
   * session.ready and MCP channels racing the answer are accepted.
   */
  async assignSession(
    params: Omit<SessionAssignParams, 'work_id' | 'environment_id' | 'lease'> & Partial<SessionAssignParams>,
  ): Promise<HarnessSession> {
    if (!params.session_id?.trim()) throw new Error('session_id is required')
    const full: SessionAssignParams = {
      ...params,
      work_id: params.work_id || `work_${randomId(8)}`,
      environment_id: params.environment_id || this.environmentId,
      lease: { token: params.lease?.token || `lease_${randomId(16)}`, fencing_token: params.lease?.fencing_token ?? 0 },
    }
    if (this.harness.sessionsById.has(full.session_id)) throw new Error(`session ${full.session_id} is already bound`)
    if (this.gone) throw new NotConnectedError()
    const session = new HarnessSession(this.harness, this, full.session_id, full.work_id, full.lease.token)
    this.harness.sessionsById.set(session.id, session)
    this.sessionsById.set(session.id, session)
    try {
      const result = await this.call<SessionAssignResult>('session.assign', full)
      if (!result.accepted) throw new AssignRejectedError()
    } catch (error) {
      session.end('assign_failed', () => new SessionEndedError())
      throw error
    }
    return session
  }

  /** @internal Ends every session; pending tool calls fail with ToolOutcomeUnknownError. */
  forget(reason: string): void {
    this.gone = true
    for (const session of this.sessions()) session.end(reason, () => new ToolOutcomeUnknownError())
    this.harness.forgetExecutor(this)
  }

  // --- agent capability ---

  /** The advertised agent capability, undefined when absent. */
  get agentCapability(): AgentCapability | undefined {
    return this.capabilities.agent
  }

  private requireAgent(): void {
    if (!this.agentCapability?.enabled) {
      throw new RpcError(JsonRpcErrorCode.CapabilityUnavailable, 'executor does not advertise the agent capability')
    }
  }

  /** turn.start; safe to repeat with the same turn_id after a timeout or reconnect. */
  async startTurn(params: TurnStartParams): Promise<TurnStartResult> {
    this.requireAgent()
    return this.call<TurnStartResult>('turn.start', params)
  }

  /** turn.input; a conflict means the turn is over and the input belongs in a new turn.start. */
  sendTurnInput(params: TurnInputParams): Promise<Ack> {
    return this.call<Ack>('turn.input', params)
  }

  /** turn.cancel; the turn's end is still reported by its session.status_idle event. */
  cancelTurn(params: TurnCancelParams): Promise<TurnCancelResult> {
    return this.call<TurnCancelResult>('turn.cancel', params)
  }

  closeAgentSession(params: SessionCloseParams): Promise<SessionCloseResult> {
    return this.call<SessionCloseResult>('session.close', params)
  }

  /** Delivers the platform's answer; accepted=false means the executor no longer waits for it. */
  respondInteraction(params: InteractionResponseParams): Promise<InteractionResponseResult> {
    return this.call<InteractionResponseResult>('interaction.response', params)
  }
}

interface ToolCallRecord {
  done: Deferred<void>
  settled: boolean
  result?: ToolResultParams
  error?: unknown
}

/** One session bound to an executor. */
export class HarnessSession {
  private readonly readyDeferred = deferred()
  private readonly doneDeferred = deferred()
  private isReady = false
  /** Set once the session ended. */
  endReason = ''
  private readonly calls = new Map<string, ToolCallRecord>()
  private readonly mcpChannels = new Map<string, HarnessMcpChannel>()
  private mcpChanged = deferred()

  constructor(
    private readonly harness: Harness,
    readonly executor: HarnessExecutor,
    readonly id: string,
    readonly workId: string,
    /** @internal */ readonly leaseToken: string,
  ) {}

  /** Resolves when the session ended. */
  get done(): Promise<void> {
    return this.doneDeferred.promise
  }

  /** Waits for session.ready. */
  async ready(signal?: AbortSignal): Promise<void> {
    if (this.isReady) return
    if (this.endReason) throw new SessionEndedError()
    await this.race(
      Promise.race([this.readyDeferred.promise, this.done.then(() => Promise.reject(new SessionEndedError()))]),
      signal,
    )
  }

  /** @internal */
  markReady(): void {
    this.isReady = true
    this.readyDeferred.resolve()
  }

  /**
   * Sends tool.call and waits for its tool.result. Idempotent by tool_call_id
   * (generated when empty): a repeat joins the in-flight call or returns the
   * recorded result without re-dispatching. Aborting signal stops waiting
   * only; use cancelTool to abort the tool.
   */
  async callTool(
    params: Omit<ToolCallParams, 'session_id' | 'tool_call_id'> & { tool_call_id?: string },
    signal?: AbortSignal,
  ): Promise<ToolResultParams> {
    const call: ToolCallParams = { ...params, session_id: this.id, tool_call_id: params.tool_call_id || `call_${randomId(8)}` }
    let record = this.calls.get(call.tool_call_id)
    if (!record) {
      if (this.endReason) throw new SessionEndedError()
      const created: ToolCallRecord = { done: deferred(), settled: false }
      record = created
      this.calls.set(call.tool_call_id, created)
      try {
        const ack = await this.executor.call<Ack>('tool.call', call)
        if (!ack.accepted) throw new Error('executor did not accept tool.call')
      } catch (error) {
        // Not dispatched: forget the call so the same id can be retried.
        if (this.calls.get(call.tool_call_id) === created) this.calls.delete(call.tool_call_id)
        throw error
      }
    }
    const settled = record
    await this.race(settled.done.promise, signal)
    if (settled.error !== undefined) throw settled.error
    return settled.result!
  }

  /** Sends tool.cancel; the waiting callTool still receives the executor's tool.result. */
  async cancelTool(toolCallId: string, reason: string): Promise<boolean> {
    const result = await this.executor.call<ToolCancelResult>('tool.cancel', {
      session_id: this.id,
      tool_call_id: toolCallId,
      reason,
    })
    return result.canceled
  }

  /** Sends session.stop; the session ends locally on success. */
  async stop(reason: string): Promise<void> {
    await this.executor.call<SessionStopResult>('session.stop', { session_id: this.id, reason })
    this.end(reason, () => new ToolOutcomeUnknownError())
  }

  /**
   * Waits for the executor to open the MCP channel for serverKey (one of the
   * session.assign mcp_servers). A newer channel replaces the old one.
   */
  async mcp(serverKey: string, signal?: AbortSignal): Promise<HarnessMcpChannel> {
    for (;;) {
      if (this.endReason) throw new SessionEndedError()
      const channel = this.mcpChannels.get(serverKey)
      if (channel) return channel
      await this.race(this.mcpChanged.promise, signal)
    }
  }

  /** @internal */
  attachMcp(serverKey: string, channel: HarnessMcpChannel): void {
    if (this.endReason) {
      channel.close()
      return
    }
    const previous = this.mcpChannels.get(serverKey)
    this.mcpChannels.set(serverKey, channel)
    void channel.done.then(() => {
      if (this.mcpChannels.get(serverKey) === channel) this.mcpChannels.delete(serverKey)
    })
    this.notifyMcp()
    previous?.close()
  }

  private notifyMcp(): void {
    const changed = this.mcpChanged
    this.mcpChanged = deferred()
    changed.resolve()
  }

  /** @internal Records a tool.result; false when this harness never dispatched the call. */
  complete(result: ToolResultParams): boolean {
    const record = this.calls.get(result.tool_call_id)
    if (!record) return false
    if (!record.settled) {
      record.settled = true
      record.result = result
      record.done.resolve()
    }
    return true
  }

  /** @internal Unbinds the session, fails waiting calls and closes MCP channels. Idempotent. */
  end(reason: string, pendingError: () => Error): void {
    if (this.endReason) return
    this.endReason = reason || 'ended'
    for (const record of this.calls.values()) {
      if (record.settled) continue
      record.settled = true
      record.error = pendingError()
      record.done.resolve()
    }
    const channels = [...this.mcpChannels.values()]
    this.mcpChannels.clear()
    this.doneDeferred.resolve()
    this.notifyMcp()
    for (const channel of channels) channel.close()
    if (this.harness.sessionsById.get(this.id) === this) this.harness.sessionsById.delete(this.id)
    if (this.executor.sessionsById.get(this.id) === this) this.executor.sessionsById.delete(this.id)
  }

  private race<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
    if (!signal) return promise
    return Promise.race([
      promise,
      new Promise<never>((_resolve, reject) => {
        if (signal.aborted) reject(signal.reason)
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
      }),
    ])
  }
}

/**
 * The harness end of one MCP channel, isolated by (session_id, server_key).
 * Frames are raw MCP JSON-RPC: the harness is the MCP client. After a
 * transport error, call session.mcp again and re-initialize.
 */
export class HarnessMcpChannel extends FrameChannel {}

/** Serializes agent events per session and remembers the acknowledged sequence. */
class AgentLedger {
  readonly acked = new Map<string, number>()
  private readonly tails = new Map<string, Promise<unknown>>()

  /** Runs fn after every earlier fn of the same session. */
  serialize<T>(sessionId: string, fn: () => Promise<T>): Promise<T> {
    const previous = this.tails.get(sessionId) ?? Promise.resolve()
    const next = previous.then(fn, fn)
    this.tails.set(
      sessionId,
      next.catch(() => {}),
    )
    return next
  }
}

const AGENT_EVENT_TYPES = new Set([
  'agent.message',
  'agent.thinking',
  'agent.tool_use',
  'agent.tool_result',
  'agent.mcp_tool_use',
  'agent.mcp_tool_result',
  'session.status_running',
  'session.status_idle',
  'session.error',
])

/** The harness side of one connection. */
class HarnessConn {
  readonly peer: RpcPeer
  private executor?: HarnessExecutor

  constructor(
    private readonly harness: Harness,
    readonly conn: Conn,
    private readonly principal: Principal,
    private readonly id: string,
  ) {
    this.peer = new RpcPeer(conn, {
      idPrefix: 'h_',
      requestTimeoutMs: harness.options.requestTimeoutMs,
      onRequest: (method, params, requestId) => this.onRequest(method, params, requestId),
      onNotification: (method, params) => this.onNotification(method, params),
      onClose: () => this.disconnected(),
      log: harness.options.onLog,
    })
  }

  private onRequest(method: string, params: unknown, requestId: string): unknown {
    // Synchronous: nothing else is meaningful before register.
    if (method === 'executor.register') return this.register(params as ExecutorRegisterParams)
    const executor = this.executor
    if (!executor) throw rpcError(JsonRpcErrorCode.InvalidRequest, 'executor.register must come first', false)
    return this.dispatch(executor, method, params, requestId)
  }

  private register(params: ExecutorRegisterParams): ExecutorRegisterResult & { protocol: string } {
    if (this.executor) throw rpcError(JsonRpcErrorCode.InvalidRequest, 'already registered', false)
    if (!params || typeof params !== 'object' || typeof params.protocol !== 'string') {
      throw rpcError(JsonRpcErrorCode.InvalidParams, 'invalid executor.register params', false)
    }
    if (params.protocol !== this.conn.protocol) {
      setImmediate(() => this.conn.close(4003, 'unsupported protocol'))
      throw rpcError(JsonRpcErrorCode.UnsupportedProtocol, 'protocol must match the negotiated subprotocol', false)
    }
    const resumeId = params.resume?.executor_id ?? ''
    let executorId = this.principal.executorId ?? ''
    if (executorId && resumeId && resumeId !== executorId) {
      throw rpcError(JsonRpcErrorCode.Forbidden, 'resume.executor_id does not match the authenticated executor', false)
    }
    if (!executorId) executorId = resumeId || `exe_${randomId(8)}`

    const harness = this.harness
    const executor = harness.executorFor(executorId)
    const previous = executor.conn
    executor.conn = this
    executor.info = params.executor
    executor.capabilities = params.capabilities ?? {}
    executor.environmentId = params.environment_id
    executor.protocol = params.protocol
    clearTimeout(executor.leaseTimer)
    executor.leaseTimer = undefined
    this.executor = executor
    // The new connection is authoritative (4004 replaced).
    previous?.conn.close(4004, 'replaced by a newer connection')

    // Sessions the executor no longer reports were lost with its old process.
    const keep = new Set(params.resume?.active_sessions ?? [])
    for (const session of executor.sessions()) {
      if (!keep.has(session.id)) session.end('executor_lost', () => new ToolOutcomeUnknownError())
    }
    setImmediate(() => {
      harness.signal()
      harness.options.onExecutor?.(executor)
    })
    return {
      executor_id: executorId,
      connection_id: this.id,
      server_time: new Date().toISOString(),
      heartbeat_interval_ms: harness.options.heartbeatIntervalMs,
      lease_ttl_ms: harness.options.leaseTtlMs,
      max_control_frame_bytes: MAX_FRAME_BYTES,
      max_mcp_frame_bytes: MAX_FRAME_BYTES,
      drain: executor.draining,
      protocol: params.protocol,
    }
  }

  private disconnected(): void {
    const executor = this.executor
    if (!executor || executor.conn !== this || executor.gone) return
    executor.conn = undefined
    executor.leaseTimer = setTimeout(() => {
      if (!executor.conn && !executor.gone) executor.forget('lease_expired')
    }, this.harness.options.leaseTtlMs)
    // A lease alone does not keep the process alive; the listener does.
    executor.leaseTimer.unref?.()
    this.harness.signal()
  }

  private dispatch(executor: HarnessExecutor, method: string, params: unknown, requestId: string): unknown {
    const agentResult = this.dispatchAgent(executor, method, params)
    if (agentResult !== undefined) return agentResult
    switch (method) {
      case 'executor.heartbeat': {
        const result: ExecutorHeartbeatResult = {
          server_time: new Date().toISOString(),
          lease_ttl_ms: this.harness.options.leaseTtlMs,
          drain: executor.draining,
        }
        return result
      }
      case 'session.ready': {
        const ready = params as SessionReadyParams
        const session = executor.sessionsById.get(ready?.session_id)
        if (!session) throw rpcError(JsonRpcErrorCode.SessionNotActive, 'session is not active on this executor', false)
        if (ready.lease_token && ready.lease_token !== session.leaseToken) {
          throw rpcError(JsonRpcErrorCode.LeaseExpired, 'lease token does not match', false)
        }
        session.markReady()
        return { accepted: true }
      }
      case 'session.ended': {
        const ended = params as SessionEndedParams
        executor.sessionsById.get(ended?.session_id)?.end(ended.reason, () => new ToolOutcomeUnknownError())
        return { accepted: true }
      }
      case 'tool.result': {
        const result = params as ToolResultParams
        const session = executor.sessionsById.get(result?.session_id)
        if (!session) throw rpcError(JsonRpcErrorCode.SessionNotActive, 'session is not active on this executor', false)
        if (!session.complete(result)) {
          throw rpcError(JsonRpcErrorCode.NotFound, 'tool_call_id was not dispatched by this harness', false)
        }
        return { accepted: true }
      }
      case 'session.event':
        return this.sessionEvent(executor, params as SessionEventParams, requestId)
      default:
        throw rpcError(JsonRpcErrorCode.MethodNotFound, `unknown method ${method}`, false)
    }
  }

  private async sessionEvent(executor: HarnessExecutor, params: SessionEventParams, requestId: string): Promise<SessionEventResult> {
    if (!executor.sessionsById.has(params?.session_id)) {
      throw rpcError(JsonRpcErrorCode.SessionNotActive, 'session is not active on this executor', false)
    }
    // A retried JSON-RPC id is answered with the first result.
    const previous = executor.eventIds.get(requestId)
    if (previous) return previous
    let result: SessionEventResult = { accepted: true, event_id: `evt_${randomId(8)}` }
    if (this.harness.options.onSessionEvent) {
      try {
        result = await this.harness.options.onSessionEvent(executor, params)
      } catch (error) {
        throw rpcError(JsonRpcErrorCode.InternalError, error instanceof Error ? error.message : String(error), true)
      }
    }
    if (executor.eventIds.size >= MAX_REMEMBERED_EVENT_IDS) executor.eventIds.clear()
    executor.eventIds.set(requestId, result)
    return result
  }

  /** Agent methods; undefined for everything else, including session.event without event_id. */
  private dispatchAgent(executor: HarnessExecutor, method: string, params: unknown): Promise<unknown> | undefined {
    const platform = this.harness.options.agent
    if (!platform) return undefined
    switch (method) {
      case 'session.event': {
        const event = params as SessionEventParams
        if (!event?.event_id) return undefined
        return this.agentEvent(platform, executor, event)
      }
      case 'interaction.request': {
        const request = params as InteractionRequestParams
        const valid =
          request?.session_id &&
          request.interaction_id &&
          ((request.kind === 'permission' && request.tool_use_id) || (request.kind === 'question' && request.question))
        if (!valid) return Promise.reject(rpcError(JsonRpcErrorCode.InvalidParams, 'invalid interaction.request params', false))
        return platform.interactionRequested(executor, request).then(
          (id): InteractionRequestResult => ({ accepted: true, platform_interaction_id: id }),
          (error: unknown) => Promise.reject(rpcError(JsonRpcErrorCode.InternalError, String(error), true)),
        )
      }
      case 'interaction.cancel': {
        const cancel = params as InteractionCancelParams
        if (!cancel?.session_id || !cancel.interaction_id) {
          return Promise.reject(rpcError(JsonRpcErrorCode.InvalidParams, 'invalid interaction.cancel params', false))
        }
        return platform.interactionCanceled(executor, cancel).then(
          (): Ack => ({ accepted: true }),
          (error: unknown) => Promise.reject(rpcError(JsonRpcErrorCode.InternalError, String(error), true)),
        )
      }
      default:
        return undefined
    }
  }

  private agentEvent(platform: AgentPlatform, executor: HarnessExecutor, params: SessionEventParams): Promise<SessionEventResult> {
    const event = params.event as unknown as Record<string, unknown> | undefined
    const type = typeof event?.type === 'string' ? event.type : ''
    const sequence = params.sequence ?? 0
    if (!params.session_id || !params.turn_id || sequence < 1 || !AGENT_EVENT_TYPES.has(type)) {
      return Promise.reject(rpcError(JsonRpcErrorCode.InvalidParams, 'invalid agent session.event', false))
    }
    const ledger = this.harness.ledger
    return ledger.serialize(params.session_id, async () => {
      let acked = ledger.acked.get(params.session_id)
      if (acked === undefined) {
        try {
          acked = await platform.ackedSequence(params.session_id)
        } catch (error) {
          throw rpcError(JsonRpcErrorCode.InternalError, String(error), true)
        }
        ledger.acked.set(params.session_id, acked)
      }
      const result: SessionEventResult = { accepted: true, event_id: params.event_id!, acked_sequence: acked }
      if (sequence <= acked) return result
      if (sequence !== acked + 1) {
        this.harness.log('warn', `agent session.event sequence gap in ${params.session_id}`)
        throw new RpcError(JsonRpcErrorCode.Conflict, 'conflict', { retryable: false, expected_sequence: acked + 1 })
      }
      try {
        await platform.agentEvent(executor, {
          sessionId: params.session_id,
          turnId: params.turn_id!,
          eventId: params.event_id!,
          sequence,
          type,
          event: event!,
        })
      } catch (error) {
        throw rpcError(JsonRpcErrorCode.InternalError, String(error), true)
      }
      ledger.acked.set(params.session_id, sequence)
      return { ...result, acked_sequence: sequence }
    })
  }

  private onNotification(method: string, params: unknown): void {
    const executor = this.executor
    if (!executor) return
    const options = this.harness.options
    switch (method) {
      case 'session.event_delta': {
        const delta = params as SessionEventDeltaParams
        if (options.agent && delta?.session_id && delta.event_id) options.agent.agentDelta(executor, delta)
        return
      }
      case 'session.status':
        options.onSessionStatus?.(executor, params as SessionStatusParams)
        return
      case 'tool.progress':
        options.onToolProgress?.(executor, params as ToolProgressParams)
        return
      case 'executor.capabilities.update': {
        const capabilities = (params as { capabilities?: Capabilities })?.capabilities
        if (capabilities) executor.capabilities = capabilities
        return
      }
      case 'executor.close':
        // A deliberate shutdown: no reconnect grace.
        executor.forget('executor_closed')
        this.conn.close(1000, 'executor.close')
        return
    }
  }
}
