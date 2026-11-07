// @beeos-ai/bma-executor-sdk: the BMA executor protocol (bma.executor.v1.1
// and v1) for Node.js. Protocol types are generated from contracts/bma
// (src/generated/executor.ts); the executor end, harness end, agent
// capability and transports are handwritten to match go/executor.

export * from './generated/executor.js'
export * from './errors.js'
export type { SessionContext } from './session-context.js'
export {
  ExecutorClient,
  channelUrl,
  isTerminalCloseCode,
  retryableToolResultError,
  staticToken,
  webSocketDialer,
  DEFAULT_BASE_URL,
  MACHINE_ID_ENV,
  DEFAULT_MAX_RECONNECT_ATTEMPTS,
  DEFAULT_RECONNECT_BASE_DELAY_MS,
  DEFAULT_REQUEST_TIMEOUT_MS,
  DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS,
  DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY_MS,
  INLINE_RESULT_BYTES,
  REQUIRED_TOOL_INPUT,
  type ChannelTarget,
  type Dialer,
  type DrainHandler,
  type ExecutorClientOptions,
  type ExecutorRequestMethod,
  type RequestHandler,
  type SlotToolName,
  type SessionAssignHandler,
  type SessionStopHandler,
  type TokenSource,
  type BashResult,
  type ExecutorTools,
  type ToolContext,
  type ToolHandler,
  type ToolOutput,
  type ToolOutputOf,
  type ToolResult,
  type ToolSlot,
  type WebSocketDialerOptions,
} from './client.js'
export { AgentTurn, type AgentHandler } from './agent.js'
export {
  Harness,
  HarnessExecutor,
  HarnessMcpChannel,
  HarnessSession,
  DEFAULT_HARNESS_HEARTBEAT_INTERVAL_MS,
  DEFAULT_HARNESS_LEASE_TTL_MS,
  type AcceptOptions,
  type AgentEventRecord,
  type AgentPlatform,
  type CallToolOptions,
  type HarnessOptions,
  type HarnessTools,
  type Principal,
} from './harness.js'
export {
  bearerAuthenticator,
  createLocalToken,
  defaultLocalEndpoint,
  listenExecutor,
  localSocketPath,
  localUrl,
  readLocalToken,
  DEFAULT_PROTOCOLS,
  type ExecutorListener,
  type ListenExecutorOptions,
} from './local.js'
export { type LogEvent, type LogLevel, type Logger } from './rpc.js'
export {
  dialWebSocket,
  inProcessPair,
  webSocketConn,
  FrameChannel,
  MAX_FRAME_BYTES,
  type Conn,
  type ConnHandlers,
} from './transport.js'
