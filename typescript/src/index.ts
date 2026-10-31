// @beeos-ai/bma-sdk/executor: the BMA executor protocol (bma.executor.v1.1
// and v1) for Node.js. Protocol types are generated from contracts/bma
// (src/generated/executor.ts); the executor end, harness end, agent
// capability and transports are handwritten to match go/executor.

export * from '../generated/executor.js'
export * from './errors.js'
export {
  ExecutorClient,
  channelUrl,
  isTerminalCloseCode,
  localDialer,
  retryableToolResultError,
  staticToken,
  webSocketDialer,
  DEFAULT_BASE_URL,
  DEFAULT_MAX_RECONNECT_ATTEMPTS,
  DEFAULT_RECONNECT_BASE_DELAY_MS,
  DEFAULT_REQUEST_TIMEOUT_MS,
  DEFAULT_TOOL_RESULT_RETRY_ATTEMPTS,
  DEFAULT_TOOL_RESULT_RETRY_BASE_DELAY_MS,
  type ChannelTarget,
  type Dialer,
  type DrainHandler,
  type ExecutorClientOptions,
  type RequestHandler,
  type SessionAssignHandler,
  type SessionStopHandler,
  type TokenSource,
  type ToolCallHandler,
  type ToolContext,
  type ToolResult,
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
  type HarnessOptions,
  type Principal,
} from './harness.js'
export {
  createLocalToken,
  defaultLocalEndpoint,
  dialLocal,
  listenLocal,
  readLocalToken,
  DEFAULT_PROTOCOLS,
  LocalCloseCode,
  LocalHandshakeError,
  type DialLocalOptions,
  type ListenLocalOptions,
  type LocalHello,
  type LocalListener,
} from './local.js'
export { type LogEvent, type LogLevel, type Logger } from './rpc.js'
export {
  dialWebSocket,
  inProcessPair,
  streamConn,
  webSocketConn,
  FrameChannel,
  MAX_FRAME_BYTES,
  type Conn,
  type ConnHandlers,
} from './transport.js'
