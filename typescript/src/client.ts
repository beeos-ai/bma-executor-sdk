// Executor end of the BMA executor protocol: registration, heartbeat,
// reconnect with resume, session lifecycle, tool dispatch and cancellation,
// retried tool results and the raw MCP channel. Mirrors go/executor Client.

import {
  ChannelScope,
  ConnectionState,
  JsonRpcErrorCode,
  ProtocolVersion,
  type Ack,
  type Capabilities,
  type ExecutorCapabilitiesUpdateParams,
  type ExecutorDrainParams,
  type ExecutorDrainResult,
  type ExecutorHeartbeatParams,
  type ExecutorHeartbeatResult,
  type ExecutorInfo,
  type ExecutorLoad,
  type ExecutorRegisterParams,
  type ExecutorRegisterResult,
  type ExecutorResume,
  type ExecutorTokenResponse,
  type SessionAssignParams,
  type SessionAssignResult,
  type SessionEndedParams,
  type SessionEventParams,
  type SessionEventResult,
  type SessionReadyParams,
  type SessionStatusParams,
  type SessionStopParams,
  type SessionStopResult,
  type ToolCallParams,
  type ToolCancelParams,
  type ToolCancelResult,
  type ToolProgressParams,
  type ToolResultParams,
  type AgentCapability,
} from '../generated/executor.js'
import { AgentRuntime, type AgentHandler, type AgentHost } from './agent.js'
import { ClientStoppedError, deferred, NotConnectedError, RequestTimeoutError, RpcError } from './errors.js'
import { DEFAULT_PROTOCOLS } from './local.js'
import { RpcPeer, type LogLevel, type Logger } from './rpc.js'
import { dialWebSocket, FrameChannel, type Conn } from './transport.js'

export const DEFAULT_BASE_URL = 'https://api.bma.beeos.ai'
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000
export const DEFAULT_MAX_RECONNECT_ATTEMPTS = 10
export const DEFAULT_RECONNECT_BASE_DELAY_MS = 500
export const DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS = 5
export const DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY_MS = 250
const DEFAULT_HEARTBEAT_INTERVAL_MS = 15_000
const MAX_TRACKED_TOOL_CALLS = 8192
const MAX_TOOL_RESULT_RETRY_DELAY_MS = 10_000
const MAX_RECONNECT_DELAY_MS = 30_000

/** Close codes after which the executor must not reconnect (1008 policy, 4002 revoked, 4003 protocol, 4004 replaced, 4005 drained). */
export function isTerminalCloseCode(code: number): boolean {
  return code === 1008 || code === 4002 || code === 4003 || code === 4004 || code === 4005
}

/** Which channel to open: the control channel, or one MCP channel of a session. */
export interface ChannelTarget {
  channel: ChannelScope
  sessionId?: string
  serverKey?: string
}

/** Opens a channel. Called again for every reconnect and every MCP channel. */
export type Dialer = (target: ChannelTarget) => Promise<Conn>

export type TokenSource = () => Promise<Pick<ExecutorTokenResponse, 'token' | 'ws_url'> & Partial<ExecutorTokenResponse>>

/** A TokenSource for a harness reached directly: ws://127.0.0.1, unix:///path.sock or npipe:////./pipe/<name>. */
export function staticToken(wsUrl: string, token = ''): TokenSource {
  return async () => ({ token, ws_url: wsUrl })
}

export interface WebSocketDialerOptions {
  environmentId: string
  executor: ExecutorInfo
  /** POST {baseUrl}/v1/executors/token with environmentKey, unless tokenSource is set. */
  baseUrl?: string
  environmentKey?: string
  tokenSource?: TokenSource
  /** Overrides the ws_url of the token response. */
  wsUrl?: string
  protocols?: string[]
  fetch?: typeof fetch
}

export function channelUrl(rawUrl: string, target: ChannelTarget): string {
  const url = new URL(rawUrl)
  if (!['ws:', 'wss:', 'unix:', 'npipe:'].includes(url.protocol)) {
    throw new Error('executor channel URL must use ws, wss, unix or npipe')
  }
  if (target.channel === ChannelScope.Mcp) {
    url.searchParams.set('channel', 'mcp')
    url.searchParams.set('session_id', target.sessionId ?? '')
    url.searchParams.set('server_key', target.serverKey ?? '')
  }
  return url.toString()
}

/** The cloud dialer: token bootstrap, then an outbound WebSocket with the bearer token. */
export function webSocketDialer(options: WebSocketDialerOptions): Dialer {
  if (!options.tokenSource && !options.environmentKey?.trim()) throw new Error('environmentKey or tokenSource is required')
  const fetchToken: TokenSource =
    options.tokenSource ??
    (async () => {
      const base = (options.baseUrl ?? DEFAULT_BASE_URL).replace(/\/+$/, '')
      const response = await (options.fetch ?? fetch)(`${base}/v1/executors/token`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${options.environmentKey}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ environment_id: options.environmentId, executor: options.executor }),
      })
      if (response.status !== 200) {
        const detail = (await response.text()).slice(0, 4096).trim()
        throw new Error(`executor token request failed: ${response.status}: ${detail}`)
      }
      const token = (await response.json()) as ExecutorTokenResponse
      if (!token.token || !token.ws_url) throw new Error('executor token response is missing token or ws_url')
      return token
    })
  return async (target) => {
    const token = await fetchToken()
    const wsUrl = options.wsUrl || token.ws_url
    if (!wsUrl) throw new Error('token source returned no ws_url')
    if (target.channel === ChannelScope.Mcp && token.channel_scope?.length && !token.channel_scope.includes(ChannelScope.Mcp)) {
      throw new Error('executor token does not allow the mcp channel')
    }
    return dialWebSocket(channelUrl(wsUrl, target), { protocols: options.protocols ?? DEFAULT_PROTOCOLS, token: token.token })
  }
}

/** What a tool handler returns; the client fills in session_id and tool_call_id. */
export type ToolResult = Omit<ToolResultParams, 'session_id' | 'tool_call_id'>

export interface ToolContext {
  /** Aborted by tool.cancel, deadline_ms, or stop(). */
  signal: AbortSignal
  /** Sends tool.progress for this call (best-effort). */
  progress(message: string): void
}

export type ToolCallHandler = (call: ToolCallParams, context: ToolContext) => Promise<ToolResult>
export type SessionAssignHandler = (params: SessionAssignParams) => Promise<SessionAssignResult>
export type SessionStopHandler = (params: SessionStopParams) => Promise<SessionStopResult>
export type DrainHandler = (params: ExecutorDrainParams) => Promise<ExecutorDrainResult>
export type RequestHandler = (params: unknown) => unknown

export interface ExecutorClientOptions extends Omit<WebSocketDialerOptions, 'environmentKey' | 'tokenSource'> {
  environmentKey?: string
  tokenSource?: TokenSource
  capabilities?: Capabilities
  /** Replaces the WebSocket dialer, for example listenExecutor().dialer or Harness.inProcessDialer. */
  dialer?: Dialer
  /** false for a connection that cannot be redialed (an accepted local connection). */
  reconnect?: boolean
  requestTimeoutMs?: number
  /** Overrides the interval returned by executor.register. */
  heartbeatIntervalMs?: number
  heartbeatLoad?: () => ExecutorLoad | undefined
  maxReconnectAttempts?: number
  reconnectBaseDelayMs?: number
  random?: () => number
  /** Total tool.result attempts including the first; 1 disables retries. */
  toolResultRetryAttempts?: number
  toolResultRetryBaseDelayMs?: number
  /**
   * Called when a tool result was not accepted after all retries or the error
   * is not retryable. Persist it and replay it with the same tool_call_id.
   */
  onToolResultLost?: (result: ToolResultParams, error: unknown) => void
  onLog?: Logger
}

/** Remembers up to MAX_TRACKED_TOOL_CALLS ids, evicting the oldest. */
class BoundedSet {
  private readonly items = new Set<string>()
  has(id: string): boolean {
    return this.items.has(id)
  }
  add(id: string): void {
    if (this.items.size >= MAX_TRACKED_TOOL_CALLS) {
      const oldest = this.items.values().next().value
      if (oldest !== undefined) this.items.delete(oldest)
    }
    this.items.add(id)
  }
  delete(id: string): void {
    this.items.delete(id)
  }
}

interface RunningToolCall {
  abort: AbortController
  canceled: boolean
  reason: string
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(new ClientStoppedError())
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = () => {
      clearTimeout(timer)
      reject(new ClientStoppedError())
    }
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

/** An executor client. One client is one logical executor connection to one harness. */
export class ExecutorClient {
  private readonly options: ExecutorClientOptions
  private readonly dialer: Dialer
  private peer?: RpcPeer
  private currentState: ConnectionState = ConnectionState.Disconnected
  private stopped = false
  private readonly stopController = new AbortController()
  private registerResult?: ExecutorRegisterResult
  private currentConnectionId = ''
  private currentExecutorId = ''
  private heartbeatIntervalMs: number
  private heartbeatTimer?: ReturnType<typeof setTimeout>
  private reconnectAttempts = 0
  private readonly sessions = new Set<string>()
  private readonly completedToolResults = new BoundedSet()
  private readonly seenToolCalls = new BoundedSet()
  private readonly running = new Map<string, RunningToolCall>()
  private readonly tools = new Map<string, ToolCallHandler>()
  private toolCallHandler?: ToolCallHandler
  private sessionAssignHandler?: SessionAssignHandler
  private sessionStopHandler?: SessionStopHandler
  private drainHandler?: DrainHandler
  private readonly requestHandlers = new Map<string, RequestHandler>()
  private readonly notificationHandlers = new Map<string, (params: unknown) => void>()
  private registration = deferred()
  private agent?: AgentRuntime

  constructor(options: ExecutorClientOptions) {
    if (options.environmentId === undefined) throw new Error('environmentId is required')
    if (!options.executor?.name) throw new Error('executor.name is required')
    this.options = options
    this.dialer = options.dialer ?? webSocketDialer(options)
    this.heartbeatIntervalMs = options.heartbeatIntervalMs ?? DEFAULT_HEARTBEAT_INTERVAL_MS
  }

  get state(): ConnectionState {
    return this.currentState
  }
  get executorId(): string {
    return this.currentExecutorId
  }
  get connectionId(): string {
    return this.currentConnectionId
  }
  /** The negotiated protocol of the current connection. */
  get protocol(): string | undefined {
    return this.peer?.conn.protocol
  }
  get activeSessions(): string[] {
    return [...this.sessions]
  }

  /** Registers the handler for every tool without a handleTool handler. */
  onToolCall(handler: ToolCallHandler): void {
    this.toolCallHandler = handler
  }
  /** Registers the handler for one tool name (bash, read, ..., or a custom name). */
  handleTool(name: string, handler: ToolCallHandler): void {
    this.tools.set(name, handler)
  }
  onSessionAssign(handler: SessionAssignHandler): void {
    this.sessionAssignHandler = handler
  }
  onSessionStop(handler: SessionStopHandler): void {
    this.sessionStopHandler = handler
  }
  onDrain(handler: DrainHandler): void {
    this.drainHandler = handler
  }
  /** Registers a handler for another harness-to-executor request (process.*, terminal.*, skill.install). */
  onRequest(method: string, handler: RequestHandler): void {
    this.requestHandlers.set(method, handler)
  }
  onNotification(method: string, handler: (params: unknown) => void): void {
    this.notificationHandlers.set(method, handler)
  }

  /** Advertises the agent capability and serves turn.*, session.close and interaction.response. Call before start. */
  serveAgent(handler: AgentHandler, capability: Omit<AgentCapability, 'enabled'> = {}): void {
    this.agent = new AgentRuntime(this.agentHost(), handler, { ...capability, enabled: true })
  }

  /** Dials, registers and starts heartbeats. */
  async start(): Promise<ExecutorRegisterResult> {
    if (this.stopped) throw new ClientStoppedError()
    if (this.peer) throw new Error('executor client already started')
    try {
      await this.connect()
    } catch (error) {
      this.currentState = ConnectionState.Disconnected
      throw error
    }
    this.scheduleHeartbeat()
    return this.registerResult!
  }

  /** Sends executor.close, closes the channel and stops reconnecting. */
  stop(reason = 'stopped'): void {
    if (this.stopped) return
    this.stopped = true
    this.currentState = ConnectionState.Closed
    clearTimeout(this.heartbeatTimer)
    const peer = this.peer
    this.peer = undefined
    if (peer && !peer.closed) {
      try {
        peer.notify('executor.close', { reason })
      } catch {
        // already gone
      }
      peer.close(1000, reason)
    }
    this.stopController.abort(new ClientStoppedError())
  }

  /** Sends a request on the control channel. */
  call<T = unknown>(method: string, params: unknown): Promise<T> {
    const peer = this.peer
    if (!peer) return Promise.reject(this.stopped ? new ClientStoppedError() : new NotConnectedError())
    return peer.call<T>(method, params, this.options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS)
  }

  /** Sends a notification on the control channel. */
  notify(method: string, params: unknown): void {
    const peer = this.peer
    if (!peer) throw new NotConnectedError()
    peer.notify(method, params)
  }

  /** Reports that the session is prepared; call after workspace and MCP setup. */
  sessionReady(params: SessionReadyParams): Promise<Ack> {
    return this.call<Ack>('session.ready', params)
  }

  /** Reports that every process and MCP server of the session stopped. */
  async sessionEnded(params: SessionEndedParams): Promise<Ack> {
    const result = await this.call<Ack>('session.ended', params)
    this.sessions.delete(params.session_id)
    return result
  }

  /** session.event for a tool executor (v1 shape, without agent fields). */
  sessionEvent(params: SessionEventParams): Promise<SessionEventResult> {
    return this.call<SessionEventResult>('session.event', params)
  }

  sessionStatus(params: SessionStatusParams): void {
    this.notify('session.status', params)
  }

  toolProgress(params: ToolProgressParams): void {
    this.notify('tool.progress', params)
  }

  /** Replaces the advertised capabilities; they are also sent on every re-register. */
  updateCapabilities(capabilities: Capabilities): void {
    this.options.capabilities = capabilities
    const params: ExecutorCapabilitiesUpdateParams = { capabilities: this.registeredCapabilities() }
    this.notify('executor.capabilities.update', params)
  }

  /**
   * Sends tool.result once per tool_call_id, retrying retryable failures with
   * exponential backoff. A repeat for an accepted id resolves without a frame.
   */
  async reportToolResult(params: ToolResultParams): Promise<Ack> {
    if (this.completedToolResults.has(params.tool_call_id)) return { accepted: true }
    this.completedToolResults.add(params.tool_call_id)
    const attempts = this.options.toolResultRetryAttempts ?? DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS
    let lastError: unknown
    for (let attempt = 1; attempt <= attempts; attempt++) {
      try {
        return await this.call<Ack>('tool.result', params)
      } catch (error) {
        lastError = error
        if (attempt === attempts || !retryableToolResultError(error)) break
        this.log('warn', `tool.result for ${params.tool_call_id} failed on attempt ${attempt}, retrying`, error)
        try {
          await sleep(this.toolResultRetryDelay(attempt), this.stopController.signal)
        } catch (stopped) {
          lastError = stopped
          break
        }
      }
    }
    this.completedToolResults.delete(params.tool_call_id)
    throw lastError
  }

  /** Opens the raw MCP channel for one session.assign mcp_servers entry. */
  async openMcpChannel(sessionId: string, serverKey: string): Promise<FrameChannel> {
    if (!sessionId.trim()) throw new Error('sessionId is required')
    if (!serverKey.trim()) throw new Error('serverKey is required')
    if (this.stopped) throw new ClientStoppedError()
    return new FrameChannel(await this.dialer({ channel: ChannelScope.Mcp, sessionId, serverKey }))
  }

  // --- connection -----------------------------------------------------------

  private registeredCapabilities(): Capabilities {
    const capabilities = { ...(this.options.capabilities ?? {}) }
    if (this.agent) capabilities.agent = this.agent.capability
    return capabilities
  }

  private resumeState(): ExecutorResume | undefined {
    if (!this.currentExecutorId && !this.currentConnectionId && this.sessions.size === 0) return undefined
    return {
      active_sessions: this.activeSessions,
      ...(this.currentExecutorId ? { executor_id: this.currentExecutorId } : {}),
      ...(this.currentConnectionId ? { last_connection_id: this.currentConnectionId } : {}),
    }
  }

  private async connect(): Promise<void> {
    this.currentState = ConnectionState.Connecting
    const conn = await this.dialer({ channel: ChannelScope.Control })
    if (this.stopped) {
      conn.close(1000, 'stopped')
      throw new ClientStoppedError()
    }
    const peer: RpcPeer = new RpcPeer(conn, {
      idPrefix: 'req_',
      requestTimeoutMs: this.options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
      onRequest: (method, params) => this.dispatchRequest(method, params),
      onNotification: (method, params) => this.dispatchNotification(peer, method, params),
      onClose: (code) => this.disconnected(peer, code),
      log: this.options.onLog,
    })
    this.peer = peer
    const protocol = (conn.protocol || ProtocolVersion.BmaExecutorV11) as ExecutorRegisterParams['protocol']
    const resume = this.resumeState()
    const params: ExecutorRegisterParams = {
      protocol,
      environment_id: this.options.environmentId,
      executor: this.options.executor,
      capabilities: this.registeredCapabilities(),
      ...(resume ? { resume } : {}),
    }
    let result: ExecutorRegisterResult
    try {
      result = await this.call<ExecutorRegisterResult>('executor.register', params)
    } catch (error) {
      if (this.peer === peer) this.peer = undefined
      peer.close(1011, 'registration failed')
      throw error
    }
    this.registerResult = result
    this.currentConnectionId = result.connection_id
    this.currentExecutorId = result.executor_id
    this.heartbeatIntervalMs =
      result.heartbeat_interval_ms > 0 ? result.heartbeat_interval_ms : (this.options.heartbeatIntervalMs ?? DEFAULT_HEARTBEAT_INTERVAL_MS)
    this.currentState = result.drain ? ConnectionState.Draining : ConnectionState.Ready
    this.reconnectAttempts = 0
    const registered = this.registration
    this.registration = deferred()
    registered.resolve()
    this.log('info', `executor registered as ${result.executor_id}`)
  }

  private disconnected(peer: RpcPeer, code: number): void {
    if (this.peer !== peer) return
    this.peer = undefined
    if (this.stopped) {
      this.currentState = ConnectionState.Closed
      return
    }
    this.currentState = ConnectionState.Disconnected
    if (isTerminalCloseCode(code) || this.options.reconnect === false) {
      this.currentState = ConnectionState.Closed
      this.log('warn', `executor channel closed with ${code}; not reconnecting`)
      return
    }
    this.scheduleReconnect()
  }

  private scheduleReconnect(): void {
    if (this.stopped) return
    const max = this.options.maxReconnectAttempts ?? DEFAULT_MAX_RECONNECT_ATTEMPTS
    if (this.reconnectAttempts >= max) {
      this.currentState = ConnectionState.Closed
      this.log('error', 'executor reconnect attempts exhausted')
      return
    }
    const attempt = this.reconnectAttempts++
    const base = this.options.reconnectBaseDelayMs ?? DEFAULT_RECONNECT_BASE_DELAY_MS
    const delay = Math.min(base * 2 ** attempt, MAX_RECONNECT_DELAY_MS) * (0.5 + this.random() * 0.5)
    this.log('info', `reconnecting executor channel in ${Math.round(delay)}ms`)
    setTimeout(() => void this.reconnect(), delay)
  }

  private async reconnect(): Promise<void> {
    if (this.stopped) return
    try {
      await this.connect()
    } catch (error) {
      this.log('warn', 'executor reconnect failed', error)
      this.currentState = ConnectionState.Disconnected
      this.scheduleReconnect()
    }
  }

  private scheduleHeartbeat(): void {
    if (this.stopped) return
    this.heartbeatTimer = setTimeout(() => {
      void this.heartbeat().finally(() => this.scheduleHeartbeat())
    }, this.heartbeatIntervalMs)
    this.heartbeatTimer.unref?.()
  }

  private async heartbeat(): Promise<void> {
    if (this.currentState !== ConnectionState.Ready && this.currentState !== ConnectionState.Draining) return
    const load = this.options.heartbeatLoad?.()
    const params: ExecutorHeartbeatParams = {
      connection_id: this.currentConnectionId,
      active_sessions: this.activeSessions,
      ...(load ? { load } : {}),
    }
    try {
      const result = await this.call<ExecutorHeartbeatResult>('executor.heartbeat', params)
      if (result.drain && this.currentState === ConnectionState.Ready) this.currentState = ConnectionState.Draining
    } catch (error) {
      this.log('warn', 'executor heartbeat failed', error)
    }
  }

  // --- inbound --------------------------------------------------------------

  private dispatchRequest(method: string, params: unknown): unknown {
    switch (method) {
      case 'tool.call':
        return this.acceptToolCall(params as ToolCallParams)
      case 'tool.cancel':
        return this.cancelToolCall(params as ToolCancelParams)
      case 'session.assign':
        return this.assignSession(params as SessionAssignParams)
      case 'session.stop':
        return this.stopSession(params as SessionStopParams)
      case 'executor.drain':
        return this.drain(params as ExecutorDrainParams)
    }
    const agentResult = this.agent?.dispatch(method, params)
    if (agentResult !== undefined) return agentResult
    const handler = this.requestHandlers.get(method)
    if (!handler) throw new RpcError(JsonRpcErrorCode.MethodNotFound, `unknown method ${method}`)
    return handler(params)
  }

  private dispatchNotification(peer: RpcPeer, method: string, params: unknown): void {
    if (method === 'executor.close') {
      this.stopped = true
      this.currentState = ConnectionState.Closed
      clearTimeout(this.heartbeatTimer)
      if (this.peer === peer) this.peer = undefined
      peer.close(1000, 'executor.close')
      this.stopController.abort(new ClientStoppedError())
      return
    }
    this.notificationHandlers.get(method)?.(params)
  }

  private async assignSession(params: SessionAssignParams): Promise<SessionAssignResult> {
    if (this.currentState === ConnectionState.Draining) return { accepted: false, session_id: params.session_id }
    const result = this.sessionAssignHandler
      ? await this.sessionAssignHandler(params)
      : { accepted: true, session_id: params.session_id }
    if (result.accepted) this.sessions.add(params.session_id)
    return result
  }

  private async stopSession(params: SessionStopParams): Promise<SessionStopResult> {
    const result = this.sessionStopHandler ? await this.sessionStopHandler(params) : { stopped: true }
    this.sessions.delete(params.session_id)
    return result
  }

  private async drain(params: ExecutorDrainParams): Promise<ExecutorDrainResult> {
    this.currentState = ConnectionState.Draining
    if (this.drainHandler) return this.drainHandler(params)
    return { accepted: true, active_sessions: this.activeSessions }
  }

  /** Accepts a tool.call at most once per tool_call_id; a resent call is acknowledged and ignored. */
  private acceptToolCall(call: ToolCallParams): Ack {
    if (!call || typeof call.tool_call_id !== 'string' || typeof call.session_id !== 'string') {
      throw new RpcError(JsonRpcErrorCode.InvalidParams, 'invalid tool.call params')
    }
    if (this.seenToolCalls.has(call.tool_call_id) || this.running.has(call.tool_call_id)) {
      this.log('debug', `duplicate tool.call for ${call.tool_call_id} ignored`)
      return { accepted: true }
    }
    this.seenToolCalls.add(call.tool_call_id)
    const running: RunningToolCall = { abort: new AbortController(), canceled: false, reason: '' }
    this.running.set(call.tool_call_id, running)
    // Starts after the acknowledgement is written.
    setImmediate(() => void this.runToolCall(call, running))
    return { accepted: true }
  }

  private cancelToolCall(params: ToolCancelParams): ToolCancelResult {
    const running = this.running.get(params.tool_call_id)
    if (!running) return { canceled: false }
    running.canceled = true
    running.reason = params.reason ?? ''
    running.abort.abort(new Error(`canceled: ${params.reason}`))
    this.log('info', `canceled tool.call ${params.tool_call_id}`)
    return { canceled: true }
  }

  private async runToolCall(call: ToolCallParams, running: RunningToolCall): Promise<void> {
    const handler = this.tools.get(call.tool) ?? this.toolCallHandler
    const onStop = () => running.abort.abort(new ClientStoppedError())
    this.stopController.signal.addEventListener('abort', onStop, { once: true })
    const deadline =
      call.deadline_ms && call.deadline_ms > 0
        ? setTimeout(() => running.abort.abort(new RequestTimeoutError('tool.call')), call.deadline_ms)
        : undefined
    let result: ToolResult
    try {
      if (!handler) throw new Error(`no tool handler registered for ${call.tool}`)
      result = await handler(call, {
        signal: running.abort.signal,
        progress: (message) => {
          try {
            this.toolProgress({ session_id: call.session_id, tool_call_id: call.tool_call_id, message })
          } catch (error) {
            this.log('debug', 'tool.progress dropped', error)
          }
        },
      })
    } catch (error) {
      result = failedToolResult(error instanceof Error ? error.message : String(error))
    } finally {
      clearTimeout(deadline)
      this.stopController.signal.removeEventListener('abort', onStop)
      this.running.delete(call.tool_call_id)
    }
    if (running.canceled) result = canceledToolResult(running.reason)
    // The result identifies the dispatched call whatever the handler returned.
    const params: ToolResultParams = { ...result, session_id: call.session_id, tool_call_id: call.tool_call_id }
    try {
      await this.reportToolResult(params)
    } catch (error) {
      this.log('error', `failed to report tool result for ${call.tool_call_id}`, error)
      this.options.onToolResultLost?.(params, error)
    }
  }

  private toolResultRetryDelay(attempt: number): number {
    const base = this.options.toolResultRetryBaseDelayMs ?? DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY_MS
    const delay = Math.min(base * 2 ** (attempt - 1), MAX_TOOL_RESULT_RETRY_DELAY_MS)
    return delay * (0.5 + this.random() * 0.5)
  }

  private random(): number {
    return (this.options.random ?? Math.random)()
  }

  private log(level: LogLevel, message: string, error?: unknown): void {
    this.options.onLog?.({ level, message, error })
  }

  private agentHost(): AgentHost {
    return {
      call: (method, params) => this.call(method, params),
      notify: (method, params) => this.notify(method, params),
      stopSignal: this.stopController.signal,
      connected: () => this.currentState === ConnectionState.Ready || this.currentState === ConnectionState.Draining,
      nextRegistration: () => this.registration.promise,
      addSession: (id) => this.sessions.add(id),
      removeSession: (id) => this.sessions.delete(id),
      sessionEnded: (params) => this.sessionEnded(params),
      log: (level, message, error) => this.log(level, message, error),
    }
  }
}

export function retryableToolResultError(error: unknown): boolean {
  if (error instanceof RpcError) {
    if (error.data?.retryable !== undefined) return error.data.retryable
    return error.code === JsonRpcErrorCode.ExecutorBusy || error.code === JsonRpcErrorCode.Timeout
  }
  return error instanceof NotConnectedError || error instanceof RequestTimeoutError
}

function failedToolResult(message: string): ToolResult {
  return { outcome: 'failed', is_error: true, content: [{ type: 'text', text: message }] }
}

function canceledToolResult(reason: string): ToolResult {
  return { outcome: 'canceled', is_error: false, content: [{ type: 'text', text: reason ? `canceled: ${reason}` : 'canceled' }] }
}
