// Local connections: Unix domain sockets, or Windows named pipes (a path such
// as \\.\pipe\bma-executor), through node:net. Who dials is independent of
// the protocol role: locally the executor usually listens and a brain dials,
// yet the executor still sends executor.register first.
//
// Handshake (one line each, then newline-delimited frames, see transport.ts):
//   dialer   -> {"bma_hello":{"protocols":["bma.executor.v1.1"],"token":"...","channel":"control"}}
//               (channel "mcp" adds session_id and server_key)
//   listener -> {"bma_hello":{"protocol":"bma.executor.v1.1"}}
//            or {"bma_close":{"code":4003,"reason":"..."}} and closes
// The token is the per-start local token (0600 file), never an environment key.

import { createHash, randomBytes, timingSafeEqual } from 'node:crypto'
import { chmod, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { createConnection, createServer, type Server, type Socket } from 'node:net'
import { homedir, tmpdir, userInfo } from 'node:os'
import { dirname, join } from 'node:path'

import { ProtocolVersion } from '../generated/executor.js'
import { encodeClose, LineSplitter, streamConn, type Conn } from './transport.js'

/** What the dialer asks for in its hello line. */
export interface LocalHello {
  protocols: string[]
  token?: string
  channel: 'control' | 'mcp'
  session_id?: string
  server_key?: string
}

/** Close codes used during the handshake, mirroring HTTP 401/400 on a WebSocket upgrade. */
export const LocalCloseCode = { Unauthorized: 4401, UnsupportedProtocol: 4003, BadHello: 4400 } as const

export class LocalHandshakeError extends Error {
  constructor(
    readonly code: number,
    reason: string,
  ) {
    super(`local handshake rejected (${code}): ${reason}`)
    this.name = 'LocalHandshakeError'
  }
}

const HANDSHAKE_TIMEOUT_MS = 10_000
export const DEFAULT_PROTOCOLS = [ProtocolVersion.BmaExecutorV11, ProtocolVersion.BmaExecutorV1]

/** The default local endpoint and token file for the current user. */
export function defaultLocalEndpoint(): { path: string; tokenFile: string } {
  const user = userInfo().username.replace(/[^A-Za-z0-9_.-]/g, '_')
  if (process.platform === 'win32') {
    const base = process.env.LOCALAPPDATA ?? join(homedir(), 'AppData', 'Local')
    return { path: `\\\\.\\pipe\\bma-executor-${user}`, tokenFile: join(base, 'bma', 'executor.token') }
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

function sameToken(expected: string, offered: string | undefined): boolean {
  const digest = (value: string) => createHash('sha256').update(value).digest()
  return offered !== undefined && timingSafeEqual(digest(expected), digest(offered))
}

/** Reads exactly one handshake line; returns it with any bytes read past it. */
function readLine(socket: Socket): Promise<{ line: string; rest: string }> {
  return new Promise((resolve, reject) => {
    const splitter = new LineSplitter()
    const timer = setTimeout(() => fail(new Error('local handshake timed out')), HANDSHAKE_TIMEOUT_MS)
    const cleanup = () => {
      // Paused until the stream conn takes over, so no data is lost in between.
      socket.pause()
      clearTimeout(timer)
      socket.off('data', onData)
      socket.off('close', onClose)
      socket.off('error', fail)
    }
    function fail(error: Error) {
      cleanup()
      reject(error)
    }
    const onClose = () => fail(new Error('local socket closed during handshake'))
    const onData = (chunk: string) => {
      let lines: string[]
      try {
        lines = splitter.push(chunk)
      } catch (error) {
        fail(error as Error)
        return
      }
      const [line, ...more] = lines
      if (line === undefined) return
      cleanup()
      resolve({ line, rest: [...more.map((frame) => `${frame}\n`), splitter.rest()].join('') })
    }
    socket.setEncoding('utf8')
    socket.on('data', onData)
    socket.once('close', onClose)
    socket.once('error', fail)
  })
}

export interface DialLocalOptions {
  path: string
  token?: string
  protocols?: string[]
  channel?: 'control' | 'mcp'
  sessionId?: string
  serverKey?: string
}

/** Dials a local listener and completes the handshake. */
export async function dialLocal(options: DialLocalOptions): Promise<Conn> {
  const socket = createConnection(options.path)
  await new Promise<void>((resolve, reject) => {
    socket.once('connect', resolve)
    socket.once('error', reject)
  })
  const hello: LocalHello = {
    protocols: options.protocols ?? DEFAULT_PROTOCOLS,
    channel: options.channel ?? 'control',
    ...(options.token ? { token: options.token } : {}),
    ...(options.sessionId ? { session_id: options.sessionId } : {}),
    ...(options.serverKey ? { server_key: options.serverKey } : {}),
  }
  socket.write(`${JSON.stringify({ bma_hello: hello })}\n`)
  const { line, rest } = await readLine(socket).catch((error) => {
    socket.destroy()
    throw error
  })
  const reply = JSON.parse(line) as { bma_hello?: { protocol: string }; bma_close?: { code: number; reason?: string } }
  if (!reply.bma_hello) {
    socket.destroy()
    throw new LocalHandshakeError(reply.bma_close?.code ?? LocalCloseCode.BadHello, reply.bma_close?.reason ?? 'no hello')
  }
  return streamConn(socket, reply.bma_hello.protocol, rest)
}

export interface ListenLocalOptions {
  path: string
  /** Required on every hello. Omit only in tests. */
  token?: string
  /** Accepted subprotocols in preference order. */
  protocols?: string[]
}

export interface LocalListener {
  readonly path: string
  close(): Promise<void>
}

/**
 * Listens on a local socket. On Unix the socket lives in a 0700 directory
 * and is chmod 0600; a stale socket file is replaced. Each accepted
 * connection is handed over after a successful handshake.
 */
export async function listenLocal(
  options: ListenLocalOptions,
  onConnection: (conn: Conn, hello: LocalHello) => void,
): Promise<LocalListener> {
  const protocols = options.protocols ?? DEFAULT_PROTOCOLS
  const unix = process.platform !== 'win32'
  if (unix) {
    await mkdir(dirname(options.path), { recursive: true, mode: 0o700 })
    await rm(options.path, { force: true })
  }
  const server: Server = createServer((socket) => {
    socket.on('error', () => {})
    void readLine(socket).then(
      ({ line, rest }) => {
        const reject = (code: number, reason: string) => socket.end(encodeClose(code, reason))
        let hello: LocalHello
        try {
          hello = (JSON.parse(line) as { bma_hello: LocalHello }).bma_hello
          if (!hello || !Array.isArray(hello.protocols)) throw new Error('missing bma_hello')
        } catch {
          reject(LocalCloseCode.BadHello, 'invalid hello')
          return
        }
        if (options.token !== undefined && !sameToken(options.token, hello.token)) {
          reject(LocalCloseCode.Unauthorized, 'unauthorized')
          return
        }
        const protocol = protocols.find((candidate) => hello.protocols.includes(candidate))
        if (!protocol) {
          reject(LocalCloseCode.UnsupportedProtocol, 'unsupported executor protocol')
          return
        }
        socket.write(`${JSON.stringify({ bma_hello: { protocol } })}\n`)
        onConnection(streamConn(socket, protocol, rest), hello)
      },
      () => socket.destroy(),
    )
  })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(options.path, () => resolve())
  })
  if (unix) await chmod(options.path, 0o600)
  return {
    path: options.path,
    close: () =>
      new Promise((resolve) => {
        server.close(() => resolve())
        if (unix) void rm(options.path, { force: true })
      }),
  }
}
