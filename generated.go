// Code generated from the BMA executor contract (bma.executor.v1). DO NOT EDIT.

package bmaexecutor

import "context"

// Ack Generic acknowledgement returned by executor RPC methods.
type Ack struct {
	Accepted bool `json:"accepted"`
}

type ArtifactRef struct {
	ArtifactID string  `json:"artifact_id"`
	Name       *string `json:"name,omitempty"`
	MimeType   *string `json:"mime_type,omitempty"`
	SizeBytes  *int64  `json:"size_bytes,omitempty"`
}

// ChannelScope Channels a short-lived executor JWT may open.
type ChannelScope string

const (
	ChannelScopeControl ChannelScope = "control"
	ChannelScopeMcp     ChannelScope = "mcp"
)

// ComputerAction Input actions accepted by the computer_action tool.
type ComputerAction string

const (
	ComputerActionClick  ComputerAction = "click"
	ComputerActionType   ComputerAction = "type"
	ComputerActionKey    ComputerAction = "key"
	ComputerActionScroll ComputerAction = "scroll"
	ComputerActionDrag   ComputerAction = "drag"
)

// ConnectionState Executor control channel state machine.
type ConnectionState string

const (
	ConnectionStateDisconnected ConnectionState = "disconnected"
	ConnectionStateConnecting   ConnectionState = "connecting"
	ConnectionStateRegistered   ConnectionState = "registered"
	ConnectionStateReady        ConnectionState = "ready"
	ConnectionStateDraining     ConnectionState = "draining"
	ConnectionStateClosed       ConnectionState = "closed"
)

// ContentBlock A tool result content block. Large output is uploaded to artifact storage and referenced with an artifact block.
type ContentBlock struct {
	Type       ContentBlockType `json:"type"`
	Text       *string          `json:"text,omitempty"`
	Data       *string          `json:"data,omitempty"`
	MimeType   *string          `json:"mime_type,omitempty"`
	URL        *string          `json:"url,omitempty"`
	ArtifactID *string          `json:"artifact_id,omitempty"`
}

type ContentBlockType string

const (
	ContentBlockTypeText     ContentBlockType = "text"
	ContentBlockTypeImage    ContentBlockType = "image"
	ContentBlockTypeResource ContentBlockType = "resource"
	ContentBlockTypeArtifact ContentBlockType = "artifact"
)

// ExecutorCapabilities Capabilities advertised at registration. Every field is optional so the same shape can carry a partial executor.capabilities.update.
type ExecutorCapabilities struct {
	Shell       *ShellCapability `json:"shell,omitempty"`
	Files       *FilesCapability `json:"files,omitempty"`
	MCP         *MCPCapability   `json:"mcp,omitempty"`
	Chrome      *bool            `json:"chrome,omitempty"`
	ComputerUse *bool            `json:"computer_use,omitempty"`
	Display     *bool            `json:"display,omitempty"`
}

type ExecutorDrainResult struct {
	Accepted       bool     `json:"accepted"`
	ActiveSessions []string `json:"active_sessions"`
}

type ExecutorHeartbeatResult struct {
	ServerTime string `json:"server_time"`
	LeaseTTLMS int64  `json:"lease_ttl_ms"`
	Drain      bool   `json:"drain"`
}

// ExecutorInfo Identity of the executor process reported at registration.
type ExecutorInfo struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
}

// ExecutorLoad Coarse load reported in heartbeats.
type ExecutorLoad struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
}

type ExecutorRegisterResult struct {
	ExecutorID           string `json:"executor_id"`
	ConnectionID         string `json:"connection_id"`
	ServerTime           string `json:"server_time"`
	HeartbeatIntervalMS  int64  `json:"heartbeat_interval_ms"`
	LeaseTTLMS           int64  `json:"lease_ttl_ms"`
	MaxControlFrameBytes int64  `json:"max_control_frame_bytes"`
	MaxMCPFrameBytes     int64  `json:"max_mcp_frame_bytes"`
	Drain                bool   `json:"drain"`
}

// ExecutorResume Reconnect state. The harness may reattach these sessions after registration.
type ExecutorResume struct {
	ExecutorID       *string  `json:"executor_id,omitempty"`
	LastConnectionID *string  `json:"last_connection_id,omitempty"`
	ActiveSessions   []string `json:"active_sessions,omitempty"`
}

// ExecutorTokenRequest Body of POST /v1/executors/token, authenticated with an environment key.
type ExecutorTokenRequest struct {
	EnvironmentID string       `json:"environment_id"`
	Executor      ExecutorInfo `json:"executor"`
}

// ExecutorTokenResponse Short-lived, channel-scoped executor JWT issued by POST /v1/executors/token.
type ExecutorTokenResponse struct {
	ExecutorID   string         `json:"executor_id"`
	Token        string         `json:"token"`
	ExpiresIn    int64          `json:"expires_in"`
	WSURL        string         `json:"ws_url"`
	ChannelScope []ChannelScope `json:"channel_scope"`
}

type FilesCapability struct {
	Enabled    bool     `json:"enabled"`
	ReadRoots  []string `json:"read_roots"`
	WriteRoots []string `json:"write_roots"`
}

type JSONRPCError struct {
	Code    JSONRPCErrorCode  `json:"code"`
	Message string            `json:"message"`
	Data    *JSONRPCErrorData `json:"data,omitempty"`
}

// JSONRPCErrorCode JSON-RPC and BMA executor error codes.
type JSONRPCErrorCode int64

const (
	// invalid JSON
	JSONRPCErrorCodeParseError JSONRPCErrorCode = -32700
	// not a JSON-RPC request
	JSONRPCErrorCodeInvalidRequest JSONRPCErrorCode = -32600
	// unknown method
	JSONRPCErrorCodeMethodNotFound JSONRPCErrorCode = -32601
	// schema/validation failure
	JSONRPCErrorCodeInvalidParams JSONRPCErrorCode = -32602
	// server error
	JSONRPCErrorCodeInternalError JSONRPCErrorCode = -32603
	// bad/expired token
	JSONRPCErrorCodeUnauthorized JSONRPCErrorCode = -32000
	// tenant/policy denial
	JSONRPCErrorCodeForbidden JSONRPCErrorCode = -32001
	// resource not found
	JSONRPCErrorCodeNotFound JSONRPCErrorCode = -32002
	// fencing/lease invalid
	JSONRPCErrorCodeLeaseExpired JSONRPCErrorCode = -32003
	// deadline exceeded
	JSONRPCErrorCodeTimeout JSONRPCErrorCode = -32004
	// canceled by peer
	JSONRPCErrorCodeCanceled JSONRPCErrorCode = -32005
	// frame limit exceeded
	JSONRPCErrorCodePayloadTooLarge JSONRPCErrorCode = -32006
	// session not assigned/ended
	JSONRPCErrorCodeSessionNotActive JSONRPCErrorCode = -32007
	// MCP server failed
	JSONRPCErrorCodeMcpUnavailable JSONRPCErrorCode = -32008
	// capacity exceeded
	JSONRPCErrorCodeExecutorBusy JSONRPCErrorCode = -32009
	// version mismatch
	JSONRPCErrorCodeUnsupportedProtocol JSONRPCErrorCode = -32010
)

// JSONRPCErrorData Optional structured JSON-RPC error data.
type JSONRPCErrorData struct {
	Retryable *bool   `json:"retryable,omitempty"`
	Detail    *string `json:"detail,omitempty"`
}

// JSONRPCErrorResponse JSON-RPC 2.0 error response.
type JSONRPCErrorResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      JSONRPCID    `json:"id"`
	Error   JSONRPCError `json:"error"`
}

// JSONRPCID JSON-RPC request id. Control-channel ids are unique per executor connection.
type JSONRPCID = string

// JSONRPCNotification JSON-RPC 2.0 notification envelope (no id, no response).
type JSONRPCNotification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// JSONRPCRequest JSON-RPC 2.0 request envelope used on the control channel.
type JSONRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      JSONRPCID      `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// JSONRPCSuccessResponse JSON-RPC 2.0 success response.
type JSONRPCSuccessResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      JSONRPCID `json:"id"`
	Result  any       `json:"result"`
}

// Lease Fencing lease for a session assignment.
type Lease struct {
	Token        string `json:"token"`
	FencingToken int64  `json:"fencing_token"`
}

type MCPCapability struct {
	Stdio      bool  `json:"stdio"`
	HTTP       bool  `json:"http"`
	MaxServers int64 `json:"max_servers"`
}

// MCPConnectionOrigin Who connects the MCP server. environment means the executor starts/connects it; service means the harness connects.
type MCPConnectionOrigin string

const (
	MCPConnectionOriginService     MCPConnectionOrigin = "service"
	MCPConnectionOriginEnvironment MCPConnectionOrigin = "environment"
)

// MCPContentBlock MCP tool content block. Large binary content must go to artifact storage.
type MCPContentBlock struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	Data     *string `json:"data,omitempty"`
	MimeType *string `json:"mimeType,omitempty"`
	Resource any     `json:"resource,omitempty"`
}

type MCPImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type MCPInitializeParams struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ClientInfo      MCPImplementation `json:"clientInfo"`
}

type MCPInitializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      MCPImplementation `json:"serverInfo"`
}

type MCPJSONRPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type MCPJSONRPCErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      MCPJSONRPCID    `json:"id"`
	Error   MCPJSONRPCError `json:"error"`
}

// MCPJSONRPCID MCP JSON-RPC id.
type MCPJSONRPCID = any

type MCPJSONRPCNotification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type MCPJSONRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      MCPJSONRPCID   `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type MCPJSONRPCSuccessResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      MCPJSONRPCID `json:"id"`
	Result  any          `json:"result"`
}

type MCPProgressParams struct {
	ProgressToken any      `json:"progressToken"`
	Progress      float64  `json:"progress"`
	Total         *float64 `json:"total,omitempty"`
}

type MCPServerConfig struct {
	ServerKey        string              `json:"server_key"`
	ConnectionOrigin MCPConnectionOrigin `json:"connection_origin"`
	Transport        MCPServerTransport  `json:"transport"`
}

// MCPServerTransport Transport for one MCP server. stdio requires command; http requires url. Fields for the other transport are omitted.
type MCPServerTransport struct {
	Type          MCPTransportType  `json:"type"`
	Command       *string           `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Cwd           *string           `json:"cwd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	TimeoutMS     *int64            `json:"timeout_ms,omitempty"`
	CommandSHA256 *string           `json:"command_sha256,omitempty"`
	URL           *string           `json:"url,omitempty"`
}

type MCPTool struct {
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type MCPToolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type MCPToolsCallResult struct {
	Content           []MCPContentBlock `json:"content"`
	IsError           *bool             `json:"isError,omitempty"`
	StructuredContent any               `json:"structuredContent,omitempty"`
}

type MCPToolsListParams struct {
	Cursor *string `json:"cursor,omitempty"`
}

type MCPToolsListResult struct {
	Tools      []MCPTool `json:"tools"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

type MCPTransportType string

const (
	MCPTransportTypeStdio MCPTransportType = "stdio"
	MCPTransportTypeHttp  MCPTransportType = "http"
)

// PermissionMode Permission decision attached by the harness before dispatch.
type PermissionMode string

const (
	PermissionModeAlwaysAllow PermissionMode = "always_allow"
	PermissionModeAlwaysAsk   PermissionMode = "always_ask"
)

type SessionAssignResult struct {
	Accepted  bool   `json:"accepted"`
	SessionID string `json:"session_id"`
}

type SessionEndedResult struct {
	Accepted bool `json:"accepted"`
}

type SessionEvent struct {
	Type      SessionEventType `json:"type"`
	ToolUseID string           `json:"tool_use_id"`
	Content   []ContentBlock   `json:"content"`
	IsError   bool             `json:"is_error"`
}

type SessionEventResult struct {
	Accepted bool   `json:"accepted"`
	EventID  string `json:"event_id"`
}

// SessionEventType Durable event types an executor may emit. Event names follow Anthropic Managed Agents; the executor must not invent new names.
type SessionEventType string

const (
	SessionEventTypeAgentToolResult SessionEventType = "agent.tool_result"
)

// SessionMemoryResource A memory store mounted into the session workspace.
type SessionMemoryResource struct {
	Type          string `json:"type"`
	MemoryStoreID string `json:"memory_store_id"`
	MountPath     string `json:"mount_path"`
}

// SessionStatus Lifecycle status reported by the executor.
type SessionStatus string

const (
	SessionStatusStarting SessionStatus = "starting"
	SessionStatusRunning  SessionStatus = "running"
	SessionStatusIdle     SessionStatus = "idle"
	SessionStatusStopping SessionStatus = "stopping"
	SessionStatusStopped  SessionStatus = "stopped"
	SessionStatusError    SessionStatus = "error"
)

type SessionStopResult struct {
	Stopped bool `json:"stopped"`
}

type ShellCapability struct {
	Enabled bool `json:"enabled"`
}

type ToolCancelResult struct {
	Canceled bool `json:"canceled"`
}

// ToolInput Flat union of the built-in tool inputs. Only the fields for the selected tool are meaningful.
type ToolInput struct {
	Command   *string         `json:"command,omitempty"`
	Cwd       *string         `json:"cwd,omitempty"`
	TimeoutMS *int64          `json:"timeout_ms,omitempty"`
	Path      *string         `json:"path,omitempty"`
	Offset    *int64          `json:"offset,omitempty"`
	Limit     *int64          `json:"limit,omitempty"`
	Content   *string         `json:"content,omitempty"`
	OldString *string         `json:"old_string,omitempty"`
	NewString *string         `json:"new_string,omitempty"`
	Pattern   *string         `json:"pattern,omitempty"`
	Glob      *string         `json:"glob,omitempty"`
	Display   *string         `json:"display,omitempty"`
	Action    *ComputerAction `json:"action,omitempty"`
}

// ToolName Built-in executor tool names.
type ToolName string

const (
	ToolNameBash            ToolName = "bash"
	ToolNameRead            ToolName = "read"
	ToolNameWrite           ToolName = "write"
	ToolNameEdit            ToolName = "edit"
	ToolNameGlob            ToolName = "glob"
	ToolNameGrep            ToolName = "grep"
	ToolNameComputerObserve ToolName = "computer_observe"
	ToolNameComputerAction  ToolName = "computer_action"
)

type ToolOutcome string

const (
	ToolOutcomeSucceeded ToolOutcome = "succeeded"
	ToolOutcomeFailed    ToolOutcome = "failed"
	ToolOutcomeCanceled  ToolOutcome = "canceled"
)

type ToolPermission struct {
	Mode PermissionMode `json:"mode"`
}

const ProtocolVersion = "bma.executor.v1"

const WSSubprotocol = "bma.executor.v1"

// MethodDirection is the direction a JSON-RPC method travels.
type MethodDirection string

const (
	DirectionExecutorToHarness MethodDirection = "executor-to-harness"
	DirectionHarnessToExecutor MethodDirection = "harness-to-executor"
	DirectionBidirectional     MethodDirection = "bidirectional"
)

// MethodKind distinguishes requests from notifications.
type MethodKind string

const (
	KindRequest      MethodKind = "request"
	KindNotification MethodKind = "notification"
)

// MethodSpec describes one bma.executor.v1 method.
type MethodSpec struct {
	Name         string
	Direction    MethodDirection
	Kind         MethodKind
	Channel      string
	IdempotentBy string
}

// Methods is the complete bma.executor.v1 method catalog.
var Methods = map[string]MethodSpec{
	"executor.register":            {Name: "executor.register", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"executor.heartbeat":           {Name: "executor.heartbeat", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"executor.capabilities.update": {Name: "executor.capabilities.update", Direction: DirectionExecutorToHarness, Kind: KindNotification, Channel: "control", IdempotentBy: ""},
	"executor.drain":               {Name: "executor.drain", Direction: DirectionHarnessToExecutor, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"executor.close":               {Name: "executor.close", Direction: DirectionBidirectional, Kind: KindNotification, Channel: "control", IdempotentBy: ""},
	"session.assign":               {Name: "session.assign", Direction: DirectionHarnessToExecutor, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"session.ready":                {Name: "session.ready", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"session.event":                {Name: "session.event", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: "jsonrpc_id"},
	"session.status":               {Name: "session.status", Direction: DirectionExecutorToHarness, Kind: KindNotification, Channel: "control", IdempotentBy: ""},
	"session.stop":                 {Name: "session.stop", Direction: DirectionHarnessToExecutor, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"session.ended":                {Name: "session.ended", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"tool.call":                    {Name: "tool.call", Direction: DirectionHarnessToExecutor, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"tool.result":                  {Name: "tool.result", Direction: DirectionExecutorToHarness, Kind: KindRequest, Channel: "control", IdempotentBy: "tool_call_id"},
	"tool.cancel":                  {Name: "tool.cancel", Direction: DirectionHarnessToExecutor, Kind: KindRequest, Channel: "control", IdempotentBy: ""},
	"tool.progress":                {Name: "tool.progress", Direction: DirectionExecutorToHarness, Kind: KindNotification, Channel: "control", IdempotentBy: ""},
}

const (
	MethodExecutorRegister           = "executor.register"
	MethodExecutorHeartbeat          = "executor.heartbeat"
	MethodExecutorCapabilitiesUpdate = "executor.capabilities.update"
	MethodExecutorDrain              = "executor.drain"
	MethodExecutorClose              = "executor.close"
	MethodSessionAssign              = "session.assign"
	MethodSessionReady               = "session.ready"
	MethodSessionEvent               = "session.event"
	MethodSessionStatus              = "session.status"
	MethodSessionStop                = "session.stop"
	MethodSessionEnded               = "session.ended"
	MethodToolCall                   = "tool.call"
	MethodToolResult                 = "tool.result"
	MethodToolCancel                 = "tool.cancel"
	MethodToolProgress               = "tool.progress"
)

var ExecutorToHarnessRequests = []string{"executor.register", "executor.heartbeat", "session.ready", "session.event", "session.ended", "tool.result"}
var HarnessToExecutorRequests = []string{"executor.drain", "session.assign", "session.stop", "tool.call", "tool.cancel"}
var ExecutorToHarnessNotifications = []string{"executor.capabilities.update", "executor.close", "session.status", "tool.progress"}
var HarnessToExecutorNotifications = []string{"executor.close"}
var ExecutorToHarnessMethods = []string{"executor.register", "executor.heartbeat", "executor.capabilities.update", "executor.close", "session.ready", "session.event", "session.status", "session.ended", "tool.result", "tool.progress"}
var HarnessToExecutorMethods = []string{"executor.drain", "executor.close", "session.assign", "session.stop", "tool.call", "tool.cancel"}

// ExecutorRegisterParams holds the JSON-RPC params for executor.register.
type ExecutorRegisterParams struct {
	Protocol      string               `json:"protocol"`
	EnvironmentID string               `json:"environment_id"`
	Executor      ExecutorInfo         `json:"executor"`
	Capabilities  ExecutorCapabilities `json:"capabilities"`
	Resume        *ExecutorResume      `json:"resume,omitempty"`
}

// ExecutorHeartbeatParams holds the JSON-RPC params for executor.heartbeat.
type ExecutorHeartbeatParams struct {
	ConnectionID   string        `json:"connection_id"`
	ActiveSessions []string      `json:"active_sessions"`
	Load           *ExecutorLoad `json:"load,omitempty"`
}

// ExecutorCapabilitiesUpdateParams holds the JSON-RPC params for executor.capabilities.update.
type ExecutorCapabilitiesUpdateParams struct {
	Capabilities ExecutorCapabilities `json:"capabilities"`
}

// ExecutorDrainParams holds the JSON-RPC params for executor.drain.
type ExecutorDrainParams struct {
	Reason     string `json:"reason"`
	DeadlineMS *int64 `json:"deadline_ms,omitempty"`
}

// ExecutorCloseParams holds the JSON-RPC params for executor.close.
type ExecutorCloseParams struct {
	Reason string `json:"reason"`
}

// SessionAssignParams holds the JSON-RPC params for session.assign.
type SessionAssignParams struct {
	SessionID     string                  `json:"session_id"`
	WorkID        string                  `json:"work_id"`
	AgentID       string                  `json:"agent_id"`
	EnvironmentID string                  `json:"environment_id"`
	DeadlineMS    *int64                  `json:"deadline_ms,omitempty"`
	Lease         Lease                   `json:"lease"`
	Resources     []SessionMemoryResource `json:"resources,omitempty"`
	MCPServers    []MCPServerConfig       `json:"mcp_servers,omitempty"`
	Metadata      map[string]string       `json:"metadata,omitempty"`
}

// SessionReadyParams holds the JSON-RPC params for session.ready.
type SessionReadyParams struct {
	SessionID  string `json:"session_id"`
	WorkID     string `json:"work_id"`
	LeaseToken string `json:"lease_token"`
}

// SessionEventParams holds the JSON-RPC params for session.event.
type SessionEventParams struct {
	SessionID string       `json:"session_id"`
	Event     SessionEvent `json:"event"`
}

// SessionStatusParams holds the JSON-RPC params for session.status.
type SessionStatusParams struct {
	SessionID string        `json:"session_id"`
	Status    SessionStatus `json:"status"`
	Detail    *string       `json:"detail,omitempty"`
}

// SessionStopParams holds the JSON-RPC params for session.stop.
type SessionStopParams struct {
	SessionID  string `json:"session_id"`
	Reason     string `json:"reason"`
	DeadlineMS *int64 `json:"deadline_ms,omitempty"`
}

// SessionEndedParams holds the JSON-RPC params for session.ended.
type SessionEndedParams struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// ToolCallParams holds the JSON-RPC params for tool.call.
type ToolCallParams struct {
	SessionID  string          `json:"session_id"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       ToolName        `json:"tool"`
	Input      ToolInput       `json:"input"`
	DeadlineMS *int64          `json:"deadline_ms,omitempty"`
	Permission *ToolPermission `json:"permission,omitempty"`
}

// ToolResultParams holds the JSON-RPC params for tool.result.
type ToolResultParams struct {
	SessionID  string         `json:"session_id"`
	ToolCallID string         `json:"tool_call_id"`
	Outcome    ToolOutcome    `json:"outcome"`
	IsError    bool           `json:"is_error"`
	Content    []ContentBlock `json:"content"`
	DurationMS *int64         `json:"duration_ms,omitempty"`
	Artifacts  []ArtifactRef  `json:"artifacts,omitempty"`
}

// ToolCancelParams holds the JSON-RPC params for tool.cancel.
type ToolCancelParams struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Reason     string `json:"reason"`
}

// ToolProgressParams holds the JSON-RPC params for tool.progress.
type ToolProgressParams struct {
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
	Message    string `json:"message"`
}

// RPC is the transport contract implemented by the handwritten client.
type RPC interface {
	Call(ctx context.Context, method string, params any, result any) error
	Notify(ctx context.Context, method string, params any) error
}

// SendExecutorRegister calls executor.register.
func SendExecutorRegister(ctx context.Context, rpc RPC, params ExecutorRegisterParams) (ExecutorRegisterResult, error) {
	var result ExecutorRegisterResult
	err := rpc.Call(ctx, MethodExecutorRegister, params, &result)
	return result, err
}

// SendExecutorHeartbeat calls executor.heartbeat.
func SendExecutorHeartbeat(ctx context.Context, rpc RPC, params ExecutorHeartbeatParams) (ExecutorHeartbeatResult, error) {
	var result ExecutorHeartbeatResult
	err := rpc.Call(ctx, MethodExecutorHeartbeat, params, &result)
	return result, err
}

// SendExecutorCapabilitiesUpdate sends executor.capabilities.update. Notifications have no result.
func SendExecutorCapabilitiesUpdate(ctx context.Context, rpc RPC, params ExecutorCapabilitiesUpdateParams) error {
	return rpc.Notify(ctx, MethodExecutorCapabilitiesUpdate, params)
}

// SendExecutorClose sends executor.close. Notifications have no result.
func SendExecutorClose(ctx context.Context, rpc RPC, params ExecutorCloseParams) error {
	return rpc.Notify(ctx, MethodExecutorClose, params)
}

// SendSessionReady calls session.ready.
func SendSessionReady(ctx context.Context, rpc RPC, params SessionReadyParams) (Ack, error) {
	var result Ack
	err := rpc.Call(ctx, MethodSessionReady, params, &result)
	return result, err
}

// SendSessionEvent calls session.event.
func SendSessionEvent(ctx context.Context, rpc RPC, params SessionEventParams) (SessionEventResult, error) {
	var result SessionEventResult
	err := rpc.Call(ctx, MethodSessionEvent, params, &result)
	return result, err
}

// SendSessionStatus sends session.status. Notifications have no result.
func SendSessionStatus(ctx context.Context, rpc RPC, params SessionStatusParams) error {
	return rpc.Notify(ctx, MethodSessionStatus, params)
}

// SendSessionEnded calls session.ended.
func SendSessionEnded(ctx context.Context, rpc RPC, params SessionEndedParams) (SessionEndedResult, error) {
	var result SessionEndedResult
	err := rpc.Call(ctx, MethodSessionEnded, params, &result)
	return result, err
}

// SendToolResult calls tool.result.
func SendToolResult(ctx context.Context, rpc RPC, params ToolResultParams) (Ack, error) {
	var result Ack
	err := rpc.Call(ctx, MethodToolResult, params, &result)
	return result, err
}

// SendToolProgress sends tool.progress. Notifications have no result.
func SendToolProgress(ctx context.Context, rpc RPC, params ToolProgressParams) error {
	return rpc.Notify(ctx, MethodToolProgress, params)
}
