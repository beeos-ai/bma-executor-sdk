// Executor end of the BMA executor protocol (bma.executor.v1) for TypeScript.
//
// The protocol types mirror go/executor/generated.go (the same contract); they
// will be replaced by types generated from contracts/bma/executor.openrpc.yaml
// once the contract generator lands. The harness end exists in Go only
// (go/executor Harness); a TypeScript harness is a follow-up.

export const PROTOCOL_VERSION = "bma.executor.v1";
export const WS_SUBPROTOCOL = "bma.executor.v1";

export const ErrorCode = {
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  InternalError: -32603,
  NotFound: -32002,
  Timeout: -32004,
  Canceled: -32005,
  SessionNotActive: -32007,
  McpUnavailable: -32008,
  ExecutorBusy: -32009,
  UnsupportedProtocol: -32010,
} as const;

export interface ExecutorInfo { name: string; os: string; arch: string; version: string }
export interface ExecutorCapabilities {
  shell?: { enabled: boolean };
  files?: { enabled: boolean; read_roots: string[]; write_roots: string[] };
  mcp?: { stdio: boolean; http: boolean; max_servers: number };
  chrome?: boolean;
  computer_use?: boolean;
  display?: boolean;
}
export interface ExecutorTokenResponse {
  executor_id?: string;
  token: string;
  expires_in?: number;
  ws_url: string;
  channel_scope?: ("control" | "mcp")[];
}
export interface ExecutorRegisterResult {
  executor_id: string;
  connection_id: string;
  server_time: string;
  heartbeat_interval_ms: number;
  lease_ttl_ms: number;
  max_control_frame_bytes: number;
  max_mcp_frame_bytes: number;
  drain: boolean;
}
export interface MCPServerConfig {
  server_key: string;
  connection_origin: "service" | "environment";
  transport: {
    type: "stdio" | "http";
    command?: string;
    args?: string[];
    cwd?: string;
    env?: Record<string, string>;
    timeout_ms?: number;
    command_sha256?: string;
    url?: string;
  };
}
export interface SessionAssignParams {
  session_id: string;
  work_id: string;
  agent_id: string;
  environment_id: string;
  deadline_ms?: number;
  lease: { token: string; fencing_token: number };
  mcp_servers?: MCPServerConfig[];
  metadata?: Record<string, string>;
}
export interface SessionAssignResult { accepted: boolean; session_id: string }
export interface SessionStopParams { session_id: string; reason: string; deadline_ms?: number }
export interface SessionStopResult { stopped: boolean }
export interface ExecutorDrainParams { reason: string; deadline_ms?: number }
export type ToolName = "bash" | "read" | "write" | "edit" | "glob" | "grep" | "computer_observe" | "computer_action";
export interface ToolInput {
  command?: string;
  cwd?: string;
  timeout_ms?: number;
  path?: string;
  offset?: number;
  limit?: number;
  content?: string;
  old_string?: string;
  new_string?: string;
  pattern?: string;
  glob?: string;
  display?: string;
  action?: "click" | "type" | "key" | "scroll" | "drag";
}
export interface ToolCallParams {
  session_id: string;
  tool_call_id: string;
  tool: ToolName | string;
  input: ToolInput;
  deadline_ms?: number;
  permission?: { mode: "always_allow" | "always_ask" };
}
export interface ContentBlock {
  type: "text" | "image" | "resource" | "artifact";
  text?: string;
  data?: string;
  mime_type?: string;
  url?: string;
  artifact_id?: string;
}
export interface ToolResultParams {
  session_id: string;
  tool_call_id: string;
  outcome: "succeeded" | "failed" | "canceled";
  is_error: boolean;
  content: ContentBlock[];
  duration_ms?: number;
  artifacts?: { artifact_id: string; name?: string; mime_type?: string; size_bytes?: number }[];
}
export type ToolResult = Omit<ToolResultParams, "session_id" | "tool_call_id">;
export interface SessionStatusParams {
  session_id: string;
  status: "starting" | "running" | "idle" | "stopping" | "stopped" | "error";
  detail?: string;
}

/** A JSON-RPC error from the peer, or one a handler throws to set the wire code. */
export class ExecutorRpcError extends Error {
  constructor(readonly code: number, message: string, readonly data?: { retryable?: boolean; detail?: string }) {
    super(message);
  }
}

export interface ExecutorClientOptions {
  environmentId: string;
  executor: ExecutorInfo;
  capabilities?: ExecutorCapabilities;
  /** POST {baseUrl}/v1/executors/token with environmentKey, unless tokenSource is set. */
  baseUrl?: string;
  environmentKey?: string;
  /** For a harness reached directly, e.g. a local harness on ws://127.0.0.1. */
  tokenSource?: () => Promise<ExecutorTokenResponse>;
  requestTimeoutMs?: number;
  toolResultRetryAttempts?: number;
  /** Called when a tool result could not be delivered; persist and replay it. */
  onToolResultLost?: (result: ToolResultParams, error: unknown) => void;
  /** WebSocket constructor; defaults to globalThis.WebSocket (Node >= 22). */
  WebSocket?: typeof WebSocket;
  fetch?: typeof fetch;
}

export type ToolCallHandler = (call: ToolCallParams, signal: AbortSignal) => Promise<ToolResult>;

interface Pending { resolve: (value: unknown) => void; reject: (error: unknown) => void; timer: ReturnType<typeof setTimeout> }

/**
 * Outbound executor client: token bootstrap, WebSocket registration,
 * heartbeat, session.assign/stop, tool.call dispatch (deduplicated by
 * tool_call_id, aborted by tool.cancel) and retried tool.result.
 * Reconnect and MCP process supervision are not implemented yet.
 */
export class ExecutorClient {
  private ws?: WebSocket;
  private nextId = 0;
  private readonly pending = new Map<string, Pending>();
  private readonly sessions = new Set<string>();
  private readonly seenCalls = new Set<string>();
  private readonly running = new Map<string, { abort: AbortController; reason?: string }>();
  private heartbeat?: ReturnType<typeof setInterval>;
  private registered?: ExecutorRegisterResult;
  private token?: ExecutorTokenResponse;

  onToolCall?: ToolCallHandler;
  onSessionAssign?: (params: SessionAssignParams) => Promise<SessionAssignResult>;
  onSessionStop?: (params: SessionStopParams) => Promise<SessionStopResult>;

  constructor(private readonly options: ExecutorClientOptions) {
    if (!options.tokenSource && !options.environmentKey) throw new Error("environmentKey or tokenSource is required");
  }

  get executorId(): string | undefined { return this.registered?.executor_id; }
  get activeSessions(): string[] { return [...this.sessions]; }

  /** Connects, registers and starts heartbeats. */
  async start(): Promise<ExecutorRegisterResult> {
    this.token = await this.fetchToken();
    const Ctor = this.options.WebSocket ?? globalThis.WebSocket;
    // Browsers cannot set headers; Node's WebSocket (undici) accepts them.
    const init = this.token.token
      ? ({ protocols: [WS_SUBPROTOCOL], headers: { Authorization: `Bearer ${this.token.token}` } } as unknown as string[])
      : [WS_SUBPROTOCOL];
    const ws = new Ctor(this.token.ws_url, init);
    this.ws = ws;
    await new Promise<void>((resolve, reject) => {
      ws.addEventListener("open", () => resolve(), { once: true });
      ws.addEventListener("error", () => reject(new Error("executor websocket failed to open")), { once: true });
    });
    ws.addEventListener("message", (event) => this.onFrame(String(event.data)));
    ws.addEventListener("close", () => this.failPending(new Error("executor channel closed")));
    this.registered = (await this.call("executor.register", {
      protocol: PROTOCOL_VERSION,
      environment_id: this.options.environmentId,
      executor: this.options.executor,
      capabilities: this.options.capabilities ?? {},
    })) as ExecutorRegisterResult;
    const interval = this.registered.heartbeat_interval_ms > 0 ? this.registered.heartbeat_interval_ms : 15_000;
    this.heartbeat = setInterval(() => {
      this.call("executor.heartbeat", { connection_id: this.registered?.connection_id, active_sessions: this.activeSessions }).catch(() => {});
    }, interval);
    return this.registered;
  }

  /** Sends executor.close and closes the channel. */
  stop(reason = "stopped"): void {
    clearInterval(this.heartbeat);
    for (const call of this.running.values()) call.abort.abort();
    if (this.ws && this.ws.readyState === this.ws.OPEN) {
      this.send({ jsonrpc: "2.0", method: "executor.close", params: { reason } });
      this.ws.close(1000, reason);
    }
  }

  sessionReady(params: { session_id: string; work_id: string; lease_token: string }): Promise<unknown> {
    return this.call("session.ready", params);
  }

  async sessionEnded(params: { session_id: string; reason: string }): Promise<unknown> {
    const result = await this.call("session.ended", params);
    this.sessions.delete(params.session_id);
    return result;
  }

  sessionStatus(params: SessionStatusParams): void {
    this.send({ jsonrpc: "2.0", method: "session.status", params });
  }

  toolProgress(params: { session_id: string; tool_call_id: string; message: string }): void {
    this.send({ jsonrpc: "2.0", method: "tool.progress", params });
  }

  /** tool.result with retries on executor_busy, timeout or data.retryable. */
  async reportToolResult(result: ToolResultParams): Promise<void> {
    const attempts = this.options.toolResultRetryAttempts ?? 5;
    let delay = 250;
    for (let attempt = 1; ; attempt++) {
      try {
        await this.call("tool.result", result);
        return;
      } catch (error) {
        const retryable = error instanceof ExecutorRpcError
          ? error.data?.retryable ?? (error.code === ErrorCode.ExecutorBusy || error.code === ErrorCode.Timeout)
          : false;
        if (!retryable || attempt >= attempts) throw error;
        await new Promise((r) => setTimeout(r, delay * (0.5 + Math.random() * 0.5)));
        delay = Math.min(delay * 2, 10_000);
      }
    }
  }

  private async fetchToken(): Promise<ExecutorTokenResponse> {
    if (this.options.tokenSource) return this.options.tokenSource();
    const base = (this.options.baseUrl ?? "https://api.bma.beeos.ai").replace(/\/$/, "");
    const response = await (this.options.fetch ?? fetch)(`${base}/v1/executors/token`, {
      method: "POST",
      headers: { Authorization: `Bearer ${this.options.environmentKey}`, "Content-Type": "application/json" },
      body: JSON.stringify({ environment_id: this.options.environmentId, executor: this.options.executor }),
    });
    if (!response.ok) throw new Error(`executor token request failed: ${response.status}`);
    const token = (await response.json()) as ExecutorTokenResponse;
    if (!token.token || !token.ws_url) throw new Error("executor token response is missing token or ws_url");
    return token;
  }

  private send(message: unknown): void {
    if (!this.ws || this.ws.readyState !== this.ws.OPEN) throw new Error("executor channel is not connected");
    this.ws.send(JSON.stringify(message));
  }

  private call(method: string, params: unknown): Promise<unknown> {
    const id = `req_${++this.nextId}`;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new ExecutorRpcError(ErrorCode.Timeout, `${method} timed out`));
      }, this.options.requestTimeoutMs ?? 30_000);
      this.pending.set(id, { resolve, reject, timer });
      try {
        this.send({ jsonrpc: "2.0", id, method, params });
      } catch (error) {
        clearTimeout(timer);
        this.pending.delete(id);
        reject(error);
      }
    });
  }

  private failPending(error: Error): void {
    clearInterval(this.heartbeat);
    for (const [id, pending] of this.pending) {
      clearTimeout(pending.timer);
      pending.reject(error);
      this.pending.delete(id);
    }
  }

  private onFrame(raw: string): void {
    let message: { id?: string; method?: string; params?: unknown; result?: unknown; error?: { code: number; message: string; data?: { retryable?: boolean } } };
    try {
      message = JSON.parse(raw);
    } catch {
      return;
    }
    if (message.method && message.id !== undefined) {
      void this.dispatch(message.id, message.method, message.params);
    } else if (message.method === "executor.close") {
      this.stop("executor.close");
    } else if (message.id !== undefined) {
      const pending = this.pending.get(message.id);
      if (!pending) return;
      this.pending.delete(message.id);
      clearTimeout(pending.timer);
      if (message.error) pending.reject(new ExecutorRpcError(message.error.code, message.error.message, message.error.data));
      else pending.resolve(message.result);
    }
  }

  private async dispatch(id: string, method: string, params: unknown): Promise<void> {
    const respond = (result: unknown) => this.send({ jsonrpc: "2.0", id, result });
    const fail = (error: unknown) => {
      const rpc = error instanceof ExecutorRpcError ? error : new ExecutorRpcError(ErrorCode.InternalError, String(error));
      this.send({ jsonrpc: "2.0", id, error: { code: rpc.code, message: rpc.message, ...(rpc.data ? { data: rpc.data } : {}) } });
    };
    try {
      switch (method) {
        case "tool.call": {
          const call = params as ToolCallParams;
          respond({ accepted: true });
          if (this.seenCalls.has(call.tool_call_id)) return; // a resent call runs once
          this.seenCalls.add(call.tool_call_id);
          await this.runToolCall(call);
          return;
        }
        case "tool.cancel": {
          const { tool_call_id, reason } = params as { tool_call_id: string; reason: string };
          const running = this.running.get(tool_call_id);
          if (running) {
            running.reason = reason;
            running.abort.abort();
          }
          respond({ canceled: Boolean(running) });
          return;
        }
        case "session.assign": {
          const assign = params as SessionAssignParams;
          const result = this.onSessionAssign
            ? await this.onSessionAssign(assign)
            : { accepted: true, session_id: assign.session_id };
          if (result.accepted) this.sessions.add(assign.session_id);
          respond(result);
          return;
        }
        case "session.stop": {
          const stop = params as SessionStopParams;
          const result = this.onSessionStop ? await this.onSessionStop(stop) : { stopped: true };
          this.sessions.delete(stop.session_id);
          respond(result);
          return;
        }
        case "executor.drain":
          respond({ accepted: true, active_sessions: this.activeSessions });
          return;
        default:
          fail(new ExecutorRpcError(ErrorCode.MethodNotFound, `unknown method ${method}`));
      }
    } catch (error) {
      fail(error);
    }
  }

  private async runToolCall(call: ToolCallParams): Promise<void> {
    const abort = new AbortController();
    const entry: { abort: AbortController; reason?: string } = { abort };
    this.running.set(call.tool_call_id, entry);
    const timer = call.deadline_ms ? setTimeout(() => abort.abort(), call.deadline_ms) : undefined;
    let result: ToolResult;
    try {
      if (!this.onToolCall) throw new Error("no tool handler registered");
      result = await this.onToolCall(call, abort.signal);
    } catch (error) {
      result = { outcome: "failed", is_error: true, content: [{ type: "text", text: String(error) }] };
    } finally {
      clearTimeout(timer);
      this.running.delete(call.tool_call_id);
    }
    if (abort.signal.aborted && entry.reason !== undefined) {
      result = { outcome: "canceled", is_error: false, content: [{ type: "text", text: `canceled: ${entry.reason}` }] };
    }
    const params: ToolResultParams = { ...result, session_id: call.session_id, tool_call_id: call.tool_call_id };
    try {
      await this.reportToolResult(params);
    } catch (error) {
      this.options.onToolResultLost?.(params, error);
    }
  }
}
