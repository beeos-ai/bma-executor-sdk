// Local connections carry the same WebSocket protocol as the cloud (same
// subprotocol negotiation, bearer token and close codes) over a Unix domain
// socket or a Windows named pipe instead of TCP:
//   unix:///run/user/1000/bma-executor/executor.sock
//   npipe:////./pipe/bma-executor-<user>          (\\.\pipe\bma-executor-<user>)
// The channel query (?channel=mcp&session_id=&server_key=) is the same as on
// ws:// URLs. Who dials is independent of the protocol role: locally the
// executor may listen (listenExecutor) and the harness dial it
// (Harness.dialExecutor); the executor still sends executor.register first.
// The token is the per-start local token (0600 file), never an environment key.

import { createHash, randomBytes, timingSafeEqual } from 'node:crypto'
import { chmod, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { createServer, type IncomingMessage, type Server } from 'node:http'
import { homedir, tmpdir, userInfo } from 'node:os'
import { dirname, join } from 'node:path'
import type { Duplex } from 'node:stream'

import { WebSocketServer } from 'ws'

import { ChannelScope, ProtocolVersion } from '../generated/executor.js'
import type { ChannelTarget, Dialer } from './client.js'
import { ClientStoppedError } from './errors.js'
import { MAX_FRAME_BYTES, webSocketConn, type Conn } from './transport.js'

export const DEFAULT_PROTOCOLS = [ProtocolVersion.BmaExecutorV11, ProtocolVersion.BmaExecutorV1]

const PIPE_PREFIX = '\\\\.\\pipe\\'

/** The unix:// or npipe:// URL of a local socket path. */
export function localUrl(path: string): string {
  if (path.startsWith(PIPE_PREFIX)) return `npipe:////./pipe/${path.slice(PIPE_PREFIX.length)}`
  return `unix://${path}`
}

/** The socket path of a unix:// or npipe:// URL; undefined for any other scheme. */
export function localSocketPath(url: string): string | undefined {
  const parsed = new URL(url)
  if (parsed.protocol === 'unix:') return decodeURIComponent(parsed.pathname)
  if (parsed.protocol === 'npipe:') {
    // WHATWG URL parsing drops the "." segment of //./pipe/<name>.
    const name = decodeURIComponent(parsed.pathname).replace(/^\/\/(\.\/)?pipe\//, '')
    return `${PIPE_PREFIX}${name}`
  }
  return undefined
}

/** The default local endpoint and token file for the current user. */
export function defaultLocalEndpoint(): { path: string; tokenFile: string } {
  const user = userInfo().username.replace(/[^A-Za-z0-9_.-]/g, '_')
  if (process.platform === 'win32') {
    const base = process.env.LOCALAPPDATA ?? join(homedir(), 'AppData', 'Local')
    return { path: `${PIPE_PREFIX}bma-executor-${user}`, tokenFile: join(base, 'bma', 'executor.token') }
  }
  const dir = join(process.env.XDG_RUNTIME_DIR ?? tmpdir(), `bma-executor-${user}`)
  return { path: join(dir, 'executor.sock'), tokenFile: join(dir, 'executor.token') }
}

/** Writes a fresh local token (0600, in a 0700 directory) and returns it. */
export async function createLocalToken(tokenFile: string): Promise<string> {
  const token = randomBytes(32).toString('hex')
  await mkdir(dirname(tokenFile), { recursive: true, mode: 0o700 })
  await writeFile(tokenFile, token, { mode: 0o600 })
  return token
}

export async function readLocalToken(tokenFile: string): Promise<string> {
  return (await readFile(tokenFile, 'utf8')).trim()
}

function bearerMatches(token: string, request: IncomingMessage): boolean {
  const header = request.headers.authorization ?? ''
  const offered = header.startsWith('Bearer ') ? header.slice('Bearer '.length) : ''
  const digest = (value: string) => createHash('sha256').update(value).digest()
  return offered !== '' && timingSafeEqual(digest(token), digest(offered))
}

/** A HarnessOptions.authenticate that accepts exactly `Authorization: Bearer <token>`. */
export function bearerAuthenticator(token: string): (request: IncomingMessage) => Record<string, never> {
  return (request) => {
    if (!bearerMatches(token, request)) throw new Error('unauthorized')
    return {}
  }
}

/**
 * Serves an HTTP server on a local socket path. On Unix the socket lives in a
 * 0700 directory and is chmod 0600; a stale socket file is replaced.
 * @internal
 */
export async function listenHttpLocal(server: Server, path: string): Promise<() => Promise<void>> {
  const unix = !path.startsWith(PIPE_PREFIX)
  if (unix) {
    await mkdir(dirname(path), { recursive: true, mode: 0o700 })
    await rm(path, { force: true })
  }
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(path, () => {
      server.off('error', reject)
      resolve()
    })
  })
  if (unix) await chmod(path, 0o600)
  return () =>
    new Promise((resolve) => {
      server.closeAllConnections()
      server.close(() => resolve())
      if (unix) void rm(path, { force: true })
    })
}

/** @internal Answers a refused WebSocket upgrade with a bare HTTP status. */
export function refuseUpgrade(socket: Duplex, status: number, message: string): void {
  socket.end(`HTTP/1.1 ${status} ${message}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`)
}

export interface ListenExecutorOptions {
  /** Unix socket path or \\.\pipe\<name>. */
  path: string
  /** Required as the harness's bearer token. Omit only in tests. */
  token?: string
  /** Accepted subprotocols in preference order. */
  protocols?: string[]
}

export interface ExecutorListener {
  /** unix:// or npipe:// URL for Harness.dialExecutor. */
  readonly url: string
  /**
   * The ExecutorClient dialer: every call waits for the harness to open the
   * matching channel (control, or the MCP channel of a session and server key).
   */
  readonly dialer: Dialer
  close(): Promise<void>
}

/**
 * The executor end of the "harness dials" direction: a WebSocket listener on
 * a local socket whose incoming channels feed an ExecutorClient dialer.
 */
export async function listenExecutor(options: ListenExecutorOptions): Promise<ExecutorListener> {
  const protocols = options.protocols ?? DEFAULT_PROTOCOLS
  const slotOf = (target: ChannelTarget) =>
    target.channel === ChannelScope.Mcp ? `mcp\0${target.sessionId ?? ''}\0${target.serverKey ?? ''}` : 'control'
  const ready = new Map<string, Conn[]>()
  const waiting = new Map<string, Array<{ resolve(conn: Conn): void; reject(error: unknown): void }>>()
  let closed = false

  const offer = (slot: string, conn: Conn) => {
    const waiter = waiting.get(slot)?.shift()
    if (waiter) waiter.resolve(conn)
    else ready.set(slot, [...(ready.get(slot) ?? []), conn])
  }

  const wss = new WebSocketServer({
    noServer: true,
    maxPayload: MAX_FRAME_BYTES,
    handleProtocols: (offered) => protocols.find((protocol) => offered.has(protocol)) ?? false,
  })
  const server = createServer((_request, response) => response.writeHead(426).end())
  server.on('upgrade', (request: IncomingMessage, socket: Duplex, head: Buffer) => {
    if (options.token !== undefined && !bearerMatches(options.token, request)) return refuseUpgrade(socket, 401, 'Unauthorized')
    const query = new URL(request.url ?? '/', 'http://localhost').searchParams
    const channel = query.get('channel') || ChannelScope.Control
    if (channel !== ChannelScope.Control && channel !== ChannelScope.Mcp) return refuseUpgrade(socket, 400, 'Bad Request')
    const offered = (request.headers['sec-websocket-protocol'] ?? '').split(',').map((value) => value.trim())
    if (!protocols.some((protocol) => offered.includes(protocol))) return refuseUpgrade(socket, 400, 'Bad Request')
    const target: ChannelTarget = {
      channel,
      sessionId: query.get('session_id') ?? undefined,
      serverKey: query.get('server_key') ?? undefined,
    }
    wss.handleUpgrade(request, socket, head, (ws) => offer(slotOf(target), webSocketConn(ws)))
  })
  const closeServer = await listenHttpLocal(server, options.path)

  return {
    url: localUrl(options.path),
    dialer: (target) => {
      if (closed) return Promise.reject(new ClientStoppedError())
      const slot = slotOf(target)
      const queued = ready.get(slot)?.shift()
      if (queued) return Promise.resolve(queued)
      return new Promise((resolve, reject) => waiting.set(slot, [...(waiting.get(slot) ?? []), { resolve, reject }]))
    },
    close: async () => {
      closed = true
      for (const waiters of waiting.values()) for (const waiter of waiters) waiter.reject(new ClientStoppedError())
      waiting.clear()
      for (const client of wss.clients) client.terminate()
      await closeServer()
    },
  }
}
