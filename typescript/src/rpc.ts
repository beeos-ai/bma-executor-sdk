// JSON-RPC 2.0 over one Conn, shared by the executor and the harness ends.

import type { JsonRpcError } from '../generated/executor.js'
import { Code, NotConnectedError, RequestTimeoutError, RpcError } from './errors.js'
import type { Conn } from './transport.js'

export type LogLevel = 'debug' | 'info' | 'warn' | 'error'
export interface LogEvent {
  level: LogLevel
  message: string
  error?: unknown
}
export type Logger = (event: LogEvent) => void

interface Inbound {
  jsonrpc?: string
  id?: string | number | null
  method?: string
  params?: unknown
  result?: unknown
  error?: JsonRpcError
}

interface Pending {
  resolve(value: unknown): void
  reject(error: unknown): void
  timer: ReturnType<typeof setTimeout>
}

export interface RpcPeerOptions {
  idPrefix: string
  requestTimeoutMs: number
  /** Answers a request; a thrown RpcError keeps its code and data. */
  onRequest(method: string, params: unknown, id: string): unknown
  onNotification(method: string, params: unknown): void
  onClose(code: number, reason: string): void
  log?: Logger
}

/** Encodes a handler failure as a JSON-RPC error object. */
export function toWireError(error: unknown): JsonRpcError {
  if (error instanceof RpcError) return error.toJSON()
  return { code: Code.InternalError, message: error instanceof Error ? error.message : String(error) }
}

export class RpcPeer {
  private nextId = 0
  private readonly pending = new Map<string, Pending>()

  constructor(
    readonly conn: Conn,
    private readonly options: RpcPeerOptions,
  ) {
    conn.setHandlers({
      message: (frame) => this.onFrame(frame),
      close: (code, reason) => {
        this.failPending(new NotConnectedError(`channel closed (${code})`))
        options.onClose(code, reason)
      },
    })
  }

  get closed(): boolean {
    return this.conn.closed
  }

  /** Sends a request and resolves with its result, or rejects with RpcError. */
  call<T = unknown>(method: string, params: unknown, timeoutMs = this.options.requestTimeoutMs): Promise<T> {
    const id = `${this.options.idPrefix}${++this.nextId}`
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new RequestTimeoutError(method))
      }, timeoutMs)
      this.pending.set(id, { resolve: resolve as (value: unknown) => void, reject, timer })
      try {
        this.conn.send(JSON.stringify({ jsonrpc: '2.0', id, method, params }))
      } catch (error) {
        clearTimeout(timer)
        this.pending.delete(id)
        reject(error)
      }
    })
  }

  notify(method: string, params: unknown): void {
    this.conn.send(JSON.stringify({ jsonrpc: '2.0', method, params }))
  }

  close(code = 1000, reason = ''): void {
    this.conn.close(code, reason)
  }

  private log(level: LogLevel, message: string, error?: unknown): void {
    this.options.log?.({ level, message, error })
  }

  private onFrame(frame: string): void {
    let message: Inbound
    try {
      message = JSON.parse(frame) as Inbound
    } catch (error) {
      this.log('warn', 'invalid JSON-RPC frame', error)
      return
    }
    const hasId = message.id !== undefined && message.id !== null
    if (message.method !== undefined && hasId) {
      this.dispatch(String(message.id), message.method, message.params)
    } else if (message.method !== undefined) {
      try {
        this.options.onNotification(message.method, message.params)
      } catch (error) {
        this.log('error', `notification ${message.method} handler failed`, error)
      }
    } else if (hasId) {
      this.settle(String(message.id), message)
    } else {
      this.log('warn', 'JSON-RPC frame is neither request nor response')
    }
  }

  private dispatch(id: string, method: string, params: unknown): void {
    let outcome: unknown
    try {
      outcome = this.options.onRequest(method, params, id)
    } catch (error) {
      this.respond(id, undefined, toWireError(error))
      return
    }
    void Promise.resolve(outcome).then(
      (result) => this.respond(id, result, undefined),
      (error: unknown) => this.respond(id, undefined, toWireError(error)),
    )
  }

  private respond(id: string, result: unknown, error: JsonRpcError | undefined): void {
    const envelope = error ? { jsonrpc: '2.0', id, error } : { jsonrpc: '2.0', id, result: result ?? null }
    try {
      this.conn.send(JSON.stringify(envelope))
    } catch (sendError) {
      this.log('debug', 'write JSON-RPC response', sendError)
    }
  }

  private settle(id: string, message: Inbound): void {
    const pending = this.pending.get(id)
    if (!pending) return
    this.pending.delete(id)
    clearTimeout(pending.timer)
    if (message.error) pending.reject(new RpcError(message.error.code, message.error.message, message.error.data))
    else pending.resolve(message.result)
  }

  private failPending(error: Error): void {
    for (const [id, pending] of this.pending) {
      clearTimeout(pending.timer)
      pending.reject(error)
      this.pending.delete(id)
    }
  }
}
