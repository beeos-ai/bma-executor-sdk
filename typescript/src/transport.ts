// Transports carry protocol frames unchanged: one JSON-RPC message (control
// channel) or one raw MCP JSON-RPC message (MCP channel) per frame. Two
// transports exist: WebSocket, over TCP (ws://, wss://) or a local socket
// (unix://, npipe://, see local.ts), and in-process pairs.

import { connect } from 'node:net'
import WebSocket from 'ws'

import { NotConnectedError } from './errors.js'
import { localSocketPath } from './local.js'

/** Largest frame accepted on any transport (1 MiB), as in the Go SDK. */
export const MAX_FRAME_BYTES = 1 << 20

export interface ConnHandlers {
  message(frame: string): void
  close(code: number, reason: string): void
}

/**
 * Conn is one open, message-oriented channel. Frames received before
 * setHandlers are queued, so a caller may attach handlers after an await.
 */
export interface Conn {
  /** The negotiated subprotocol, for example bma.executor.v1.1. */
  readonly protocol: string
  readonly closed: boolean
  send(frame: string): void
  /** Closes with a WebSocket close code (1000 when omitted). */
  close(code?: number, reason?: string): void
  setHandlers(handlers: ConnHandlers): void
}

abstract class BaseConn implements Conn {
  closed = false
  private handlers?: ConnHandlers
  private queue: string[] = []
  private closeInfo?: [number, string]

  constructor(readonly protocol: string) {}

  abstract send(frame: string): void
  abstract close(code?: number, reason?: string): void

  setHandlers(handlers: ConnHandlers): void {
    this.handlers = handlers
    const queued = this.queue
    this.queue = []
    for (const frame of queued) handlers.message(frame)
    if (this.closeInfo) handlers.close(...this.closeInfo)
  }

  protected deliver(frame: string): void {
    if (this.closeInfo) return
    if (this.handlers) this.handlers.message(frame)
    else this.queue.push(frame)
  }

  protected closedWith(code: number, reason: string): void {
    if (this.closeInfo) return
    this.closed = true
    this.closeInfo = [code, reason]
    this.handlers?.close(code, reason)
  }
}

class InProcessConn extends BaseConn {
  peer!: InProcessConn

  send(frame: string): void {
    if (this.closed) throw new NotConnectedError()
    const peer = this.peer
    queueMicrotask(() => peer.deliver(frame))
  }

  close(code = 1000, reason = ''): void {
    if (this.closed) return
    this.closedWith(code, reason)
    const peer = this.peer
    queueMicrotask(() => peer.closedWith(code, reason))
  }
}

/** Two connected in-process ends, for a brain and a hand in one process. */
export function inProcessPair(protocol: string): [Conn, Conn] {
  const a = new InProcessConn(protocol)
  const b = new InProcessConn(protocol)
  a.peer = b
  b.peer = a
  return [a, b]
}

class WebSocketConn extends BaseConn {
  constructor(private readonly ws: WebSocket) {
    super(ws.protocol)
    ws.on('message', (data: WebSocket.RawData) => this.deliver(data.toString()))
    ws.on('close', (code: number, reason: Buffer) => this.closedWith(code, reason.toString()))
    // An error is always followed by close; the listener keeps it from throwing.
    ws.on('error', () => {})
  }

  send(frame: string): void {
    if (this.ws.readyState !== WebSocket.OPEN) throw new NotConnectedError()
    this.ws.send(frame)
  }

  close(code = 1000, reason = ''): void {
    if (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING) this.ws.close(code, reason)
  }
}

/** Wraps an open `ws` WebSocket. */
export function webSocketConn(ws: WebSocket): Conn {
  return new WebSocketConn(ws)
}

/**
 * Dials a WebSocket and resolves once it is open. unix:// and npipe:// URLs
 * run the same handshake over the local socket; their query is the request's.
 */
export function dialWebSocket(
  url: string,
  options: { protocols: string[]; token?: string; maxPayload?: number; signal?: AbortSignal },
): Promise<Conn> {
  return new Promise((resolve, reject) => {
    const socketPath = localSocketPath(url)
    const target = socketPath === undefined ? url : `ws://localhost/v1/executors/channel${new URL(url).search}`
    const ws = new WebSocket(target, options.protocols, {
      headers: options.token ? { Authorization: `Bearer ${options.token}` } : {},
      maxPayload: options.maxPayload ?? MAX_FRAME_BYTES,
      ...(socketPath === undefined ? {} : { createConnection: () => connect(socketPath) }),
    })
    const onAbort = () => ws.terminate()
    options.signal?.addEventListener('abort', onAbort, { once: true })
    ws.once('open', () => {
      options.signal?.removeEventListener('abort', onAbort)
      resolve(webSocketConn(ws))
    })
    ws.once('unexpected-response', (_request, response) => {
      reject(new Error(`websocket dial ${url}: HTTP ${response.statusCode}`))
      ws.terminate()
    })
    ws.once('error', (error) => reject(new Error(`websocket dial ${url}: ${error.message}`)))
  })
}

/**
 * FrameChannel reads frames from a Conn one at a time: the raw MCP channel
 * on both ends. read() rejects with NotConnectedError once the channel closed
 * and every queued frame was read.
 */
export class FrameChannel {
  private readonly frames: string[] = []
  private waiters: Array<{ resolve: (frame: string) => void; reject: (error: unknown) => void }> = []
  private closeError?: Error
  private readonly closedPromise: Promise<void>
  private markClosed!: () => void

  constructor(readonly conn: Conn) {
    this.closedPromise = new Promise((resolve) => (this.markClosed = resolve))
    conn.setHandlers({
      message: (frame) => {
        const waiter = this.waiters.shift()
        if (waiter) waiter.resolve(frame)
        else this.frames.push(frame)
      },
      close: (code, reason) => {
        this.closeError = new NotConnectedError(`channel closed (${code}${reason ? `: ${reason}` : ''})`)
        for (const waiter of this.waiters) waiter.reject(this.closeError)
        this.waiters = []
        this.markClosed()
      },
    })
  }

  /** Resolves when the channel has closed. */
  get done(): Promise<void> {
    return this.closedPromise
  }

  read(signal?: AbortSignal): Promise<string> {
    const frame = this.frames.shift()
    if (frame !== undefined) return Promise.resolve(frame)
    if (this.closeError) return Promise.reject(this.closeError)
    return new Promise((resolve, reject) => {
      const waiter = { resolve, reject }
      this.waiters.push(waiter)
      signal?.addEventListener(
        'abort',
        () => {
          this.waiters = this.waiters.filter((candidate) => candidate !== waiter)
          reject(signal.reason)
        },
        { once: true },
      )
    })
  }

  write(frame: string): void {
    this.conn.send(frame)
  }

  close(): void {
    this.conn.close(1000, 'mcp channel closed')
  }
}
