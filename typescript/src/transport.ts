// Transports carry protocol frames unchanged: one JSON-RPC message (control
// channel) or one raw MCP JSON-RPC message (MCP channel) per frame. Three
// transports exist: WebSocket (cloud), local stream sockets (Unix domain
// socket / Windows named pipe, see local.ts) and in-process pairs.

import type { Socket } from 'node:net'
import WebSocket from 'ws'

import { NotConnectedError } from './errors.js'

/** Largest frame accepted on any transport (1 MiB), as in the Go SDK. */
export const MAX_FRAME_BYTES = 1 << 20

/** WebSocket close code a peer reports when the transport dropped without a close frame. */
export const CLOSE_ABNORMAL = 1006

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

/** Dials a WebSocket and resolves once it is open. */
export function dialWebSocket(
  url: string,
  options: { protocols: string[]; token?: string; maxPayload?: number; signal?: AbortSignal },
): Promise<Conn> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url, options.protocols, {
      headers: options.token ? { Authorization: `Bearer ${options.token}` } : {},
      maxPayload: options.maxPayload ?? MAX_FRAME_BYTES,
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

// --- local stream sockets ---------------------------------------------------
//
// A stream carries newline-delimited JSON: one frame per line. JSON.stringify
// never emits a raw newline, and MCP frames are single-line by the MCP stdio
// rules. Two control lines exist besides frames; neither is a valid JSON-RPC
// message, so they cannot collide with one:
//   {"bma_hello":{...}}                    the handshake (local.ts)
//   {"bma_close":{"code":4004,"reason":""}} a WebSocket-equivalent close code

/** Splits a byte stream into lines, enforcing MAX_FRAME_BYTES per line. */
export class LineSplitter {
  private buffer = ''

  push(chunk: string): string[] {
    this.buffer += chunk
    const lines = this.buffer.split('\n')
    this.buffer = lines.pop() ?? ''
    if (Buffer.byteLength(this.buffer) > MAX_FRAME_BYTES) throw new Error('local frame exceeds 1 MiB')
    return lines.filter((line) => line.length > 0)
  }

  rest(): string {
    const rest = this.buffer
    this.buffer = ''
    return rest
  }
}

interface CloseLine {
  bma_close: { code: number; reason?: string }
}

function closeLine(line: string): CloseLine['bma_close'] | undefined {
  if (!line.startsWith('{"bma_close"')) return undefined
  try {
    return (JSON.parse(line) as CloseLine).bma_close
  } catch {
    return undefined
  }
}

export function encodeClose(code: number, reason: string): string {
  return `${JSON.stringify({ bma_close: { code, reason } })}\n`
}

class StreamConn extends BaseConn {
  private readonly splitter = new LineSplitter()

  constructor(
    private readonly socket: Socket,
    protocol: string,
    initial: string,
  ) {
    super(protocol)
    socket.setEncoding('utf8')
    socket.on('data', (chunk: string) => this.consume(chunk))
    socket.on('close', () => this.closedWith(CLOSE_ABNORMAL, 'local socket closed'))
    socket.on('error', () => {})
    if (initial) this.consume(initial)
    socket.resume()
  }

  private consume(chunk: string): void {
    let lines: string[]
    try {
      lines = this.splitter.push(chunk)
    } catch (error) {
      this.close(1009, (error as Error).message)
      return
    }
    for (const line of lines) {
      const close = closeLine(line)
      if (close) {
        this.closedWith(close.code, close.reason ?? '')
        this.socket.end()
        return
      }
      this.deliver(line)
    }
  }

  send(frame: string): void {
    if (this.closed || this.socket.destroyed) throw new NotConnectedError()
    this.socket.write(`${frame}\n`)
  }

  close(code = 1000, reason = ''): void {
    if (this.closed) return
    this.closedWith(code, reason)
    if (!this.socket.destroyed) this.socket.end(encodeClose(code, reason))
  }
}

/** Wraps a connected local socket after the handshake; initial is data read past the hello line. */
export function streamConn(socket: Socket, protocol: string, initial = ''): Conn {
  return new StreamConn(socket, protocol, initial)
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
