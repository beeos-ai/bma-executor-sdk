import { JsonRpcErrorCode, type JsonRpcError, type JsonRpcErrorData } from '../generated/executor.js'

/**
 * A JSON-RPC error from the peer, or one a handler throws to choose the wire
 * code (for example executor_busy for a full executor). Any other thrown
 * error is answered as internal_error.
 */
export class RpcError extends Error {
  constructor(
    readonly code: number,
    message: string,
    readonly data?: JsonRpcErrorData,
  ) {
    super(message)
    this.name = 'RpcError'
  }

  toJSON(): JsonRpcError {
    return { code: this.code as JsonRpcError['code'], message: this.message, ...(this.data ? { data: this.data } : {}) }
  }
}

export function rpcError(code: number, message: string, retryable: boolean): RpcError {
  return new RpcError(code, message, { retryable })
}

/** A call was attempted while no channel is open, or the channel closed before the answer. */
export class NotConnectedError extends Error {
  constructor(message = 'executor channel is not connected') {
    super(message)
    this.name = 'NotConnectedError'
  }
}

/** Returned after stop(), executor.close, or a terminal close code. */
export class ClientStoppedError extends Error {
  constructor() {
    super('executor client stopped')
    this.name = 'ClientStoppedError'
  }
}

/** A request did not get an answer within requestTimeoutMs. */
export class RequestTimeoutError extends Error {
  constructor(method: string) {
    super(`${method} timed out`)
    this.name = 'RequestTimeoutError'
  }
}

/** A request on a session that has ended. */
export class SessionEndedError extends Error {
  constructor() {
    super('session ended')
    this.name = 'SessionEndedError'
  }
}

/**
 * The executor was lost (lease expiry, executor.close, session ended, or a
 * restarted executor that no longer reports the session) before a
 * tool.result arrived. The tool may or may not have run.
 */
export class ToolOutcomeUnknownError extends Error {
  constructor() {
    super('tool outcome unknown: executor lost before tool.result')
    this.name = 'ToolOutcomeUnknownError'
  }
}

/** session.assign answered accepted=false. */
export class AssignRejectedError extends Error {
  constructor() {
    super('executor rejected session.assign')
    this.name = 'AssignRejectedError'
  }
}

/** AgentTurn methods after session.close. */
export class AgentSessionClosedError extends Error {
  constructor() {
    super('agent session closed')
    this.name = 'AgentSessionClosedError'
  }
}

export function abortError(signal: AbortSignal): unknown {
  return signal.reason ?? new DOMException('aborted', 'AbortError')
}

export const Code = JsonRpcErrorCode

export interface Deferred<T = void> {
  promise: Promise<T>
  resolve(value: T): void
}

export function deferred<T = void>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => (resolve = done))
  return { promise, resolve }
}
