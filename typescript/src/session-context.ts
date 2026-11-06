import { DaemonHttpError, MachineTokenRequiredError, SessionContextNotFoundError } from './errors.js'

/**
 * What BMA tells this machine about an agent session it serves
 * (GET /api/v1/daemon/processes/{process_id}/session and
 * GET /api/v1/daemon/sessions/{session_id}): the session's environment and the
 * metadata its creator set, never agent configuration or credentials.
 */
export interface SessionContext {
  id: string
  environment_id: string
  metadata: Record<string, string>
  /** RFC 3339 date-time. */
  created_at: string
}

export interface DaemonClientOptions {
  baseUrl: string
  environmentId: string
  environmentKey?: string
  fetch?: typeof fetch
}

const MAX_CACHED = 1024

/** A Map that drops its oldest entry past MAX_CACHED. */
class BoundedMap<V> {
  private readonly items = new Map<string, V>()
  get(key: string): V | undefined {
    return this.items.get(key)
  }
  set(key: string, value: V): void {
    if (!this.items.has(key) && this.items.size >= MAX_CACHED) {
      const oldest = this.items.keys().next().value
      if (oldest !== undefined) this.items.delete(oldest)
    }
    this.items.set(key, value)
  }
}

function pathSegment(kind: string, id: string): string {
  if (!id || id.length > 256 || id === '.' || id === '..' || /[/\\\0]/.test(id)) throw new Error(`invalid ${kind} ${JSON.stringify(id)}`)
  return encodeURIComponent(id)
}

function clone(context: SessionContext): SessionContext {
  return { ...context, metadata: { ...(context.metadata ?? {}) } }
}

/** Session context reads over the machine token, cached per session (metadata is read once). */
export class SessionContexts {
  private readonly sessions = new BoundedMap<SessionContext>()
  private readonly processes = new BoundedMap<string>()

  constructor(private readonly options: DaemonClientOptions) {}

  async byProcess(processId: string): Promise<SessionContext> {
    const known = this.processes.get(processId)
    if (known !== undefined) return this.byId(known)
    const context = await this.fetch(`/processes/${pathSegment('process id', processId)}/session`)
    this.processes.set(processId, context.id)
    return this.remember(context)
  }

  async byId(sessionId: string): Promise<SessionContext> {
    const cached = this.sessions.get(sessionId)
    if (cached) return clone(cached)
    return this.remember(await this.fetch(`/sessions/${pathSegment('session id', sessionId)}`))
  }

  private remember(context: SessionContext): SessionContext {
    const cached = this.sessions.get(context.id)
    if (cached) return clone(cached)
    this.sessions.set(context.id, context)
    return clone(context)
  }

  private async fetch(path: string): Promise<SessionContext> {
    const { baseUrl, environmentId, environmentKey } = this.options
    if (environmentId !== '' || !environmentKey?.trim()) throw new MachineTokenRequiredError()
    const origin = baseUrl.replace(/\/+$/, '').replace(/\/api\/v1$/, '')
    const response = await (this.options.fetch ?? fetch)(`${origin}/api/v1/daemon${path}`, {
      headers: { Authorization: `Bearer ${environmentKey}`, Accept: 'application/json' },
      // A redirect would forward the machine token elsewhere.
      redirect: 'manual',
    })
    if (response.status === 404) throw new SessionContextNotFoundError()
    if (response.status < 200 || response.status >= 300) {
      const raw = (await response.text()).slice(0, 64 * 1024)
      let body: { code?: string; message?: string } = {}
      try {
        body = JSON.parse(raw) as typeof body
      } catch {
        // not BMA's error body
      }
      throw body.message ? new DaemonHttpError(response.status, body.message, body.code) : new DaemonHttpError(response.status, raw.trim())
    }
    const context = (await response.json()) as SessionContext
    if (!context?.id) throw new Error('session context response has no id')
    return { ...context, metadata: context.metadata ?? {} }
  }
}
