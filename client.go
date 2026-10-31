// Package bmaexecutor implements the BMA Executor Protocol (bma.executor.v1.1,
// generated from contracts/bma): the executor end (Client: token bootstrap,
// pluggable transport, JSON-RPC 2.0, registration, heartbeat, reconnect with
// resume, dispatch, tool cancellation, retried results, artifacts, MCP
// channel), the execution Core with registrable capabilities (process.*,
// skill.install, ...), and the harness end (Harness).
package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultBaseURL              = "https://api.bma.beeos.ai"
	DefaultRequestTimeout       = 30 * time.Second
	DefaultMaxReconnectAttempts = 10
	DefaultReconnectBaseDelay   = 500 * time.Millisecond
	// DefaultToolResultRetryAttempts is the total number of tool.result attempts.
	DefaultToolResultRetryAttempts = 5
	// DefaultToolResultRetryBaseDelay is the first retry backoff for tool.result.
	DefaultToolResultRetryBaseDelay = 250 * time.Millisecond
	defaultHeartbeatInterval        = 15 * time.Second
	defaultReconnectMaxDelay        = 30 * time.Second
	// tokenRefreshAt is the fraction of a token's lifetime after which the
	// client sends executor.token.refresh.
	tokenRefreshAt          = 0.7
	defaultLeaseTTL         = time.Minute
	maxTrackedToolResults   = 8192
	maxToolResultRetryDelay = 10 * time.Second
	maxFrameBytes           = 4 << 20

	// Artifact upload (v1.1 §6.5).
	artifactPath         = "/v1/executors/artifacts"
	headerSessionID      = "X-BMA-Session-ID"
	headerArtifactDigest = "X-BMA-Artifact-Digest"
	headerArtifactName   = "X-BMA-Artifact-Name"
)

var (
	// ErrNotConnected is returned when a call is attempted before the channel opens.
	ErrNotConnected = errors.New("bmaexecutor: executor channel is not connected")
	// ErrClientStopped is returned after Stop or a terminal close code.
	ErrClientStopped = errors.New("bmaexecutor: client stopped")
	// ErrReplaced is Err after the harness closed with 4004: a newer
	// connection for this executor is authoritative and this one must stop.
	ErrReplaced = errors.New("bmaexecutor: executor connection was replaced (4004)")
	// ErrReconnectExhausted is Err after MaxReconnectAttempts failed.
	ErrReconnectExhausted = errors.New("bmaexecutor: executor reconnect attempts exhausted")
)

// ConnectionState is the executor control channel state (ConnectionState* values).
type ConnectionState string

type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// LogEvent is emitted through Options.OnLog.
type LogEvent struct {
	Level   LogLevel
	Message string
	Err     error
}

// ToolCallHandler handles an inbound tool.call and returns the result to report.
type ToolCallHandler func(ctx context.Context, params ToolCallParams) (ToolResultParams, error)

// SessionAssignHandler handles an inbound session.assign.
type SessionAssignHandler func(ctx context.Context, params SessionAssignParams) (SessionAssignResult, error)

// SessionStopHandler handles an inbound session.stop.
type SessionStopHandler func(ctx context.Context, params SessionStopParams) (SessionStopResult, error)

// DrainHandler handles an inbound executor.drain.
type DrainHandler func(ctx context.Context, params ExecutorDrainParams) (ExecutorDrainResult, error)

// Options configures a Client.
type Options struct {
	// EnvironmentKey is the bearer of POST /v1/executors/token: an environment
	// key, or a machine daemon token for a machine-scoped executor (empty
	// EnvironmentID).
	EnvironmentKey string
	// EnvironmentID is the selfhost environment; empty registers a
	// machine-scoped executor.
	EnvironmentID string
	Executor      ExecutorInfo
	Capabilities  Capabilities
	// Protocol is the WebSocket subprotocol and executor.register protocol.
	// Defaults to ProtocolV11.
	Protocol string

	BaseURL string
	WSURL   string
	// TokenSource replaces the POST /v1/executors/token bootstrap, for example
	// StaticToken for a local harness. When set, EnvironmentKey and BaseURL
	// are not used.
	TokenSource func(ctx context.Context) (ExecutorTokenResponse, error)
	// Dial opens the control connection. Defaults to WebSocketDialer with
	// HTTPClient; see UnixSocketDialer, ListenUnixSocket and InProcessDialer.
	Dial Dialer
	// HTTPClient is used for the token request, artifact uploads and the
	// default WebSocket dial. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// RequestTimeout bounds one JSON-RPC request. Defaults to 30s.
	RequestTimeout time.Duration
	// HeartbeatInterval overrides the interval returned by executor.register.
	HeartbeatInterval time.Duration
	// HeartbeatLoad is sampled for each heartbeat.
	HeartbeatLoad func() *ExecutorLoad
	// MaxReconnectAttempts bounds consecutive failed reconnects (default 10);
	// a negative value reconnects until Stop.
	MaxReconnectAttempts int
	ReconnectBaseDelay   time.Duration
	// ReconnectMaxDelay caps the reconnect backoff before jitter. Defaults
	// to 30s.
	ReconnectMaxDelay time.Duration
	Random            func() float64
	// ToolResultRetryAttempts is the total number of attempts for tool.result,
	// session.ready and session.ended, including the first. It defaults to 5;
	// set it to 1 to disable retries.
	ToolResultRetryAttempts int
	// ToolResultRetryBaseDelay is the first backoff for retryable failures.
	// It defaults to 250ms and doubles per attempt up to 10s.
	ToolResultRetryBaseDelay time.Duration
	// OnToolResultLost is called by the dispatch path when a tool result could
	// not be accepted after all retries, or the error is not retryable. The
	// adapter must persist the result and replay it with the same
	// tool_call_id; the client never drops a result silently.
	OnToolResultLost func(params ToolResultParams, err error)
	OnLog            func(LogEvent)
}

// RPCError is a JSON-RPC error returned by the peer, or returned by a handler
// to answer with a specific code and data.
type RPCError struct {
	Code    int
	Message string
	Data    *JSONRPCErrorData
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("bmaexecutor: json-rpc error %d: %s", e.Code, e.Message)
}

type connection struct {
	conn    Conn
	writeMu sync.Mutex
}

func (c *connection) write(ctx context.Context, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, payload)
}

type rpcOutcome struct {
	result json.RawMessage
	err    error
}

type rpcRequestEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcNotificationEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcSuccessEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Result  any    `json:"result"`
}

type rpcErrorEnvelope struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      string       `json:"id"`
	Error   JSONRPCError `json:"error"`
}

type rpcInbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *JSONRPCError   `json:"error"`
}

// runningToolCall tracks one in-flight tool.call so tool.cancel can abort it.
type runningToolCall struct {
	cancel   context.CancelFunc
	reason   string
	canceled bool
}

type requestIDKey struct{}

// RequestID returns the JSON-RPC id of the inbound request whose handler
// received ctx (the idempotency key of retried requests), or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Client is an outbound executor client.
type Client struct {
	options    Options
	httpClient *http.Client

	rootCtx context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	mu                   sync.Mutex
	conn                 *connection
	state                ConnectionState
	stopped              bool
	err                  error
	registerResult       *ExecutorRegisterResult
	token                ExecutorTokenResponse
	connectionID         string
	executorID           string
	heartbeatInterval    time.Duration
	leaseTTL             time.Duration
	disconnectedAt       time.Time
	sessions             map[string]struct{}
	pending              map[string]chan rpcOutcome
	reconnectAttempts    int
	completedToolResults map[string]struct{}
	seenToolCalls        map[string]struct{}
	runningToolCalls     map[string]*runningToolCall
	toolCallHandler      ToolCallHandler
	toolCallAccept       func(ToolCallParams) error
	afterToolResults     []func(ToolCallParams)
	sessionsLost         []func([]string)
	sessionAssignHandler SessionAssignHandler
	sessionStopHandler   SessionStopHandler
	drainHandler         DrainHandler
	requestHandlers      map[string]func(context.Context, json.RawMessage) (any, error)
	notificationHandlers map[string]func(json.RawMessage)
	nextID               uint64
	// agent is set by ServeAgent; registered is closed and replaced after
	// every successful executor.register so agent outboxes resend.
	agent      *agentRuntime
	registered chan struct{}
}

// NewClient validates options and returns a disconnected client.
func NewClient(options Options) (*Client, error) {
	if options.TokenSource == nil && strings.TrimSpace(options.EnvironmentKey) == "" {
		return nil, errors.New("bmaexecutor: EnvironmentKey is required")
	}
	if options.Executor.Name == "" {
		return nil, errors.New("bmaexecutor: Executor.Name is required")
	}
	if options.Protocol == "" {
		options.Protocol = ProtocolV11
	}
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	options.BaseURL = strings.TrimRight(options.BaseURL, "/")
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Dial == nil {
		options.Dial = WebSocketDialer(options.HTTPClient)
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultHeartbeatInterval
	}
	if options.MaxReconnectAttempts == 0 {
		options.MaxReconnectAttempts = DefaultMaxReconnectAttempts
	}
	if options.ReconnectBaseDelay <= 0 {
		options.ReconnectBaseDelay = DefaultReconnectBaseDelay
	}
	if options.ReconnectMaxDelay <= 0 {
		options.ReconnectMaxDelay = defaultReconnectMaxDelay
	}
	if options.Random == nil {
		options.Random = rand.Float64
	}
	if options.ToolResultRetryAttempts <= 0 {
		options.ToolResultRetryAttempts = DefaultToolResultRetryAttempts
	}
	if options.ToolResultRetryBaseDelay <= 0 {
		options.ToolResultRetryBaseDelay = DefaultToolResultRetryBaseDelay
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		options:              options,
		httpClient:           options.HTTPClient,
		rootCtx:              ctx,
		cancel:               cancel,
		done:                 make(chan struct{}),
		state:                ConnectionStateDisconnected,
		heartbeatInterval:    options.HeartbeatInterval,
		leaseTTL:             defaultLeaseTTL,
		sessions:             make(map[string]struct{}),
		pending:              make(map[string]chan rpcOutcome),
		completedToolResults: make(map[string]struct{}),
		seenToolCalls:        make(map[string]struct{}),
		runningToolCalls:     make(map[string]*runningToolCall),
		requestHandlers:      make(map[string]func(context.Context, json.RawMessage) (any, error)),
		notificationHandlers: make(map[string]func(json.RawMessage)),
		registered:           make(chan struct{}),
	}, nil
}

// Start fetches an executor token, connects, registers, and starts heartbeats.
// After a failed Start the client may be started again.
func (c *Client) Start(ctx context.Context) (ExecutorRegisterResult, error) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return ExecutorRegisterResult{}, ErrClientStopped
	}
	if c.conn != nil {
		c.mu.Unlock()
		return ExecutorRegisterResult{}, errors.New("bmaexecutor: client already started")
	}
	c.state = ConnectionStateConnecting
	c.mu.Unlock()

	token, err := c.fetchToken(ctx)
	if err != nil {
		c.setState(ConnectionStateDisconnected)
		return ExecutorRegisterResult{}, err
	}
	if err := c.connect(ctx, token); err != nil {
		c.setState(ConnectionStateDisconnected)
		return ExecutorRegisterResult{}, err
	}
	go c.heartbeatLoop()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registerResult == nil {
		return ExecutorRegisterResult{}, errors.New("bmaexecutor: registration did not complete")
	}
	return *c.registerResult, nil
}

// Stop sends executor.close, closes the connection, and fails pending calls.
func (c *Client) Stop(ctx context.Context, reason string) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	conn := c.conn
	c.mu.Unlock()

	if conn != nil {
		payload, err := json.Marshal(rpcNotificationEnvelope{
			JSONRPC: "2.0",
			Method:  MethodExecutorClose,
			Params:  ExecutorCloseParams{Reason: reason},
		})
		if err == nil {
			writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = conn.write(writeCtx, payload)
			cancel()
		}
	}
	c.finish(nil, 1000, reason)
	return nil
}

// finish stops the client for good with err (nil for Stop).
func (c *Client) finish(err error, code int, reason string) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.err = err
	conn := c.conn
	c.conn = nil
	c.state = ConnectionStateClosed
	c.mu.Unlock()
	if conn != nil {
		_ = conn.conn.Close(code, reason)
	}
	c.cancel()
	c.failPending(ErrClientStopped)
	close(c.done)
}

// Done is closed when the client stops for good: Stop, executor.close, a
// terminal close code (4004 replaced, 4003 unsupported protocol, ...) or
// exhausted reconnects.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err explains Done: nil after Stop or executor.close, ErrReplaced after
// 4004, ErrReconnectExhausted, or the terminal close error.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// State returns the current connection state.
func (c *Client) State() ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// ExecutorID returns the id assigned by executor.register.
func (c *Client) ExecutorID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executorID
}

// ConnectionID returns the current connection id.
func (c *Client) ConnectionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectionID
}

// ActiveSessions returns the sessions currently assigned to this executor.
func (c *Client) ActiveSessions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionIDsLocked()
}

// OnToolCall registers the tool.call handler.
func (c *Client) OnToolCall(handler ToolCallHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolCallHandler = handler
}

// OnToolCallAccept registers a check run before a tool.call is acknowledged.
// An error answers the request instead (a *RPCError keeps its code, for
// example session_not_active or capability_unavailable) and the tool does
// not run.
func (c *Client) OnToolCallAccept(accept func(ToolCallParams) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolCallAccept = accept
}

// afterToolResult registers a hook run after a tool call's result was
// delivered (or given up on).
func (c *Client) afterToolResult(hook func(ToolCallParams)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.afterToolResults = append(c.afterToolResults, hook)
}

// onSessionsLost registers a hook run when the client gives up its sessions
// because it stayed disconnected past the harness lease.
func (c *Client) onSessionsLost(hook func([]string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionsLost = append(c.sessionsLost, hook)
}

// OnSessionAssign registers the session.assign handler.
func (c *Client) OnSessionAssign(handler SessionAssignHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionAssignHandler = handler
}

// OnSessionStop registers the session.stop handler.
func (c *Client) OnSessionStop(handler SessionStopHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionStopHandler = handler
}

// OnDrain registers the executor.drain handler.
func (c *Client) OnDrain(handler DrainHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainHandler = handler
}

// OnRequest registers a handler for a harness-to-executor request method.
// RequestID(ctx) is the request's JSON-RPC id.
func (c *Client) OnRequest(method string, handler func(context.Context, json.RawMessage) (any, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestHandlers[method] = handler
}

// OnNotification registers a handler for a control-channel notification.
func (c *Client) OnNotification(method string, handler func(json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notificationHandlers[method] = handler
}

// Call implements generated RPC: it sends a JSON-RPC request and decodes the result.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if len(raw) == 0 {
		raw = []byte("null")
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("bmaexecutor: decode %s result: %w", method, err)
	}
	return nil
}

// Notify implements generated RPC: it sends a JSON-RPC notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	conn := c.currentConn()
	if conn == nil {
		return ErrNotConnected
	}
	payload, err := json.Marshal(rpcNotificationEnvelope{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	if err := conn.write(writeCtx, payload); err != nil {
		return fmt.Errorf("bmaexecutor: write %s: %w", method, err)
	}
	return nil
}

// ReportToolResult sends tool.result once per tool_call_id, retrying retryable
// failures (executor_busy, timeout, a disconnect) with exponential backoff
// across reconnects. A duplicate call for an accepted tool_call_id returns
// accepted without sending another frame.
func (c *Client) ReportToolResult(ctx context.Context, params ToolResultParams) (Ack, error) {
	c.mu.Lock()
	if _, ok := c.completedToolResults[params.ToolCallID]; ok {
		c.mu.Unlock()
		return Ack{Accepted: true}, nil
	}
	c.rememberToolCallLocked(c.completedToolResults, params.ToolCallID)
	c.mu.Unlock()

	var ack Ack
	err := c.callRetrying(ctx, MethodToolResult, params, &ack)
	if err != nil {
		c.mu.Lock()
		delete(c.completedToolResults, params.ToolCallID)
		c.mu.Unlock()
		return Ack{}, err
	}
	return ack, nil
}

// SessionReady reports that the executor accepted session.assign and has
// prepared the session. Call it after workspace and MCP setup complete.
func (c *Client) SessionReady(ctx context.Context, params SessionReadyParams) (Ack, error) {
	var ack Ack
	err := c.callRetrying(ctx, MethodSessionReady, params, &ack)
	return ack, err
}

// SessionEnded reports that every process and MCP server for the session has
// stopped. The session leaves ActiveSessions after the harness accepts.
func (c *Client) SessionEnded(ctx context.Context, params SessionEndedParams) (Ack, error) {
	var result Ack
	if err := c.callRetrying(ctx, MethodSessionEnded, params, &result); err != nil {
		return Ack{}, err
	}
	c.mu.Lock()
	delete(c.sessions, params.SessionID)
	c.mu.Unlock()
	return result, nil
}

// callRetrying sends a request whose delivery is mandatory (v1.1 §8.4),
// retrying retryable failures with backoff.
func (c *Client) callRetrying(ctx context.Context, method string, params, out any) error {
	var lastErr error
	for attempt := 1; attempt <= c.options.ToolResultRetryAttempts; attempt++ {
		err := c.Call(ctx, method, params, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == c.options.ToolResultRetryAttempts || !retryableToolResultError(err) {
			break
		}
		c.log(LogWarn, fmt.Sprintf("%s failed on attempt %d, retrying", method, attempt), err)
		if err := c.waitToolResultRetry(ctx, attempt); err != nil {
			lastErr = err
			break
		}
	}
	return lastErr
}

func retryableToolResultError(err error) bool {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Data != nil && rpcErr.Data.Retryable {
			return true
		}
		switch rpcErr.Code {
		case CodeExecutorBusy, CodeTimeout:
			return true
		default:
			return false
		}
	}
	return errors.Is(err, ErrNotConnected) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) waitToolResultRetry(ctx context.Context, attempt int) error {
	delay := c.options.ToolResultRetryBaseDelay
	for i := 1; i < attempt; i++ {
		if delay >= maxToolResultRetryDelay {
			delay = maxToolResultRetryDelay
			break
		}
		delay *= 2
	}
	if delay > maxToolResultRetryDelay {
		delay = maxToolResultRetryDelay
	}
	delay = time.Duration(float64(delay) * (0.5 + c.options.Random()*0.5))

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.rootCtx.Done():
		return ErrClientStopped
	case <-timer.C:
		return nil
	}
}

func (c *Client) rememberToolCallLocked(set map[string]struct{}, id string) {
	if len(set) >= maxTrackedToolResults {
		for key := range set {
			delete(set, key)
			break
		}
	}
	set[id] = struct{}{}
}

// UploadArtifact implements POST /v1/executors/artifacts (v1.1 §6.5) with
// the current executor token. It must finish before a tool.result references
// the artifact; the upload is idempotent by (session, digest).
func (c *Client) UploadArtifact(ctx context.Context, sessionID, name, mimeType string, content []byte) (ArtifactRef, error) {
	if len(content) > DefaultMaxArtifactBytes {
		return ArtifactRef{}, fmt.Errorf("bmaexecutor: artifact is %d bytes, limit is %d", len(content), DefaultMaxArtifactBytes)
	}
	c.mu.Lock()
	token := c.token.Token
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.options.BaseURL+artifactPath, bytes.NewReader(content))
	if err != nil {
		return ArtifactRef{}, err
	}
	sum := sha256.Sum256(content)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", mimeType)
	request.Header.Set(headerSessionID, sessionID)
	request.Header.Set(headerArtifactDigest, "sha256:"+hex.EncodeToString(sum[:]))
	request.Header.Set(headerArtifactName, name)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return ArtifactRef{}, fmt.Errorf("bmaexecutor: artifact upload returned %d", response.StatusCode)
	}
	var ref ArtifactRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return ArtifactRef{}, fmt.Errorf("bmaexecutor: decode artifact upload response: %w", err)
	}
	return ref, nil
}

// StaticToken is a TokenSource for a harness reached directly, typically a
// local harness. token may be empty when the harness has no Authenticate hook.
func StaticToken(wsURL, token string) func(context.Context) (ExecutorTokenResponse, error) {
	return func(context.Context) (ExecutorTokenResponse, error) {
		return ExecutorTokenResponse{Token: token, WSURL: wsURL}, nil
	}
}

func (c *Client) fetchToken(ctx context.Context) (ExecutorTokenResponse, error) {
	if c.options.TokenSource != nil {
		token, err := c.options.TokenSource(ctx)
		if err == nil && token.WSURL == "" {
			err = errors.New("bmaexecutor: token source returned no ws_url")
		}
		return token, err
	}
	body, err := json.Marshal(ExecutorTokenRequest{
		EnvironmentID: c.options.EnvironmentID,
		Executor:      c.options.Executor,
	})
	if err != nil {
		return ExecutorTokenResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.options.BaseURL+"/v1/executors/token", bytes.NewReader(body))
	if err != nil {
		return ExecutorTokenResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.options.EnvironmentKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return ExecutorTokenResponse{}, fmt.Errorf("bmaexecutor: token request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return ExecutorTokenResponse{}, fmt.Errorf("bmaexecutor: token request failed: %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	var token ExecutorTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil {
		return ExecutorTokenResponse{}, fmt.Errorf("bmaexecutor: decode token response: %w", err)
	}
	if token.Token == "" || token.WSURL == "" {
		return ExecutorTokenResponse{}, errors.New("bmaexecutor: token response is missing token or ws_url")
	}
	return token, nil
}

func (c *Client) connect(ctx context.Context, token ExecutorTokenResponse) error {
	if c.options.WSURL != "" {
		token.WSURL = c.options.WSURL
	}
	c.setState(ConnectionStateConnecting)
	transport, err := c.options.Dial(ctx, token, c.options.Protocol, ChannelTarget{Channel: ChannelControl})
	if err != nil {
		return err
	}
	conn := &connection{conn: transport}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		_ = transport.Close(1000, "stopped")
		return ErrClientStopped
	}
	c.conn = conn
	c.token = token
	c.mu.Unlock()
	go c.readLoop(conn)

	params := ExecutorRegisterParams{
		Protocol:      c.options.Protocol,
		EnvironmentID: c.options.EnvironmentID,
		Executor:      c.options.Executor,
		Capabilities:  c.options.Capabilities,
		Resume:        c.resumeState(),
	}
	if c.agent != nil {
		params.Capabilities.Agent = &c.agent.capability
	}
	var result ExecutorRegisterResult
	if err := c.Call(ctx, MethodExecutorRegister, params, &result); err != nil {
		c.dropConnection(conn)
		return err
	}
	if c.options.Protocol == ProtocolV11 && result.Protocol != ProtocolV11 {
		c.dropConnection(conn)
		return fmt.Errorf("bmaexecutor: register result protocol %q, need %s", result.Protocol, ProtocolV11)
	}
	c.mu.Lock()
	interval := time.Duration(result.HeartbeatIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = c.options.HeartbeatInterval
	}
	c.registerResult = &result
	c.connectionID = result.ConnectionID
	c.executorID = result.ExecutorID
	c.heartbeatInterval = interval
	if result.LeaseTTLMS > 0 {
		c.leaseTTL = time.Duration(result.LeaseTTLMS) * time.Millisecond
	}
	c.disconnectedAt = time.Time{}
	c.state = ConnectionStateReady
	c.reconnectAttempts = 0
	close(c.registered)
	c.registered = make(chan struct{})
	c.mu.Unlock()
	c.log(LogInfo, "executor registered as "+result.ExecutorID, nil)
	go c.refreshTokenLoop(conn, token)
	return nil
}

// refreshTokenLoop keeps conn open across token lifetimes: at 70% of the
// current token's lifetime it fetches a new token and sends it with
// executor.token.refresh. It ends with the connection, or when the harness
// does not serve the method; the harness then closes with 4001 at expiry and
// the client reconnects as before. A token without expires_in (StaticToken)
// is never refreshed.
func (c *Client) refreshTokenLoop(conn *connection, token ExecutorTokenResponse) {
	if token.ExpiresIn <= 0 {
		return
	}
	expiry := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	wait := time.Duration(float64(time.Duration(token.ExpiresIn)*time.Second) * tokenRefreshAt)
	for {
		timer := time.NewTimer(wait)
		select {
		case <-c.rootCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if c.currentConn() != conn {
			return
		}
		fresh, err := c.fetchToken(c.rootCtx)
		if err == nil {
			var result ExecutorTokenRefreshResult
			err = c.Call(c.rootCtx, MethodExecutorTokenRefresh, ExecutorTokenRefreshParams{Token: fresh.Token}, &result)
			if err == nil {
				c.mu.Lock()
				if c.conn == conn {
					c.token.Token = fresh.Token
					c.token.ExpiresIn = fresh.ExpiresIn
				}
				c.mu.Unlock()
				lifetime := time.Duration(fresh.ExpiresIn) * time.Second
				if fresh.ExpiresIn <= 0 {
					return
				}
				expiry = time.Now().Add(lifetime)
				wait = time.Duration(float64(lifetime) * tokenRefreshAt)
				continue
			}
			var rpcErr *RPCError
			if errors.As(err, &rpcErr) && (rpcErr.Code == -32601 || rpcErr.Code == -32012) {
				c.log(LogInfo, "harness does not refresh executor tokens; reconnecting at expiry", nil)
				return
			}
		}
		// Retry halfway to the expiry; give up when too little is left and let
		// the 4001 reconnect take over.
		remaining := time.Until(expiry)
		c.log(LogWarn, "executor token refresh failed", err)
		if remaining < 2*time.Second {
			return
		}
		wait = remaining / 2
	}
}

func (c *Client) resumeState() *ExecutorResume {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.executorID == "" && c.connectionID == "" && len(c.sessions) == 0 {
		return nil
	}
	return &ExecutorResume{
		ExecutorID: c.executorID, LastConnectionID: c.connectionID, ActiveSessions: c.sessionIDsLocked(),
	}
}

func (c *Client) sessionIDsLocked() []string {
	ids := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (c *Client) currentConn() *connection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	id := fmt.Sprintf("req_%d", atomic.AddUint64(&c.nextID, 1))
	pending := make(chan rpcOutcome, 1)
	c.pending[id] = pending
	c.mu.Unlock()

	payload, err := json.Marshal(rpcRequestEnvelope{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		c.removePending(id)
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	if err := conn.write(callCtx, payload); err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("bmaexecutor: write %s: %w: %w", method, ErrNotConnected, err)
	}
	select {
	case outcome := <-pending:
		return outcome.result, outcome.err
	case <-callCtx.Done():
		c.removePending(id)
		return nil, fmt.Errorf("bmaexecutor: %s: %w", method, callCtx.Err())
	case <-c.rootCtx.Done():
		c.removePending(id)
		return nil, ErrClientStopped
	}
}

func (c *Client) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) readLoop(conn *connection) {
	for {
		data, err := conn.conn.Read(c.rootCtx)
		if err != nil {
			c.disconnected(conn, err)
			return
		}
		c.handleFrame(conn, data)
	}
}

func (c *Client) handleFrame(conn *connection, data []byte) {
	var message rpcInbound
	if err := json.Unmarshal(data, &message); err != nil {
		c.log(LogError, "invalid JSON-RPC frame", err)
		return
	}
	switch {
	case message.Method != "" && message.ID != nil:
		go c.dispatchRequest(conn, *message.ID, message.Method, message.Params)
	case message.Method != "":
		c.dispatchNotification(message.Method, message.Params)
	case message.ID != nil:
		c.settle(*message.ID, message)
	default:
		c.log(LogError, "JSON-RPC frame is neither request nor response", nil)
	}
}

func (c *Client) settle(id string, message rpcInbound) {
	c.mu.Lock()
	pending, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if message.Error != nil {
		pending <- rpcOutcome{err: &RPCError{Code: message.Error.Code, Message: message.Error.Message, Data: message.Error.Data}}
		return
	}
	pending <- rpcOutcome{result: message.Result}
}

func (c *Client) dispatchRequest(conn *connection, id string, method string, params json.RawMessage) {
	switch method {
	case MethodToolCall:
		var call ToolCallParams
		if err := json.Unmarshal(params, &call); err != nil {
			c.respondError(conn, id, CodeInvalidParams, "invalid tool.call params")
			return
		}
		c.mu.Lock()
		accept := c.toolCallAccept
		c.mu.Unlock()
		if accept != nil {
			if err := accept(call); err != nil {
				c.respondHandlerError(conn, id, err)
				return
			}
		}
		callCtx, started := c.beginToolCall(call)
		c.respondResult(conn, id, Ack{Accepted: true})
		if started {
			go c.runToolCall(callCtx, call)
			return
		}
		c.log(LogDebug, "duplicate tool.call for "+call.ToolCallID+" ignored", nil)
	case MethodToolCancel:
		var cancel ToolCancelParams
		if err := json.Unmarshal(params, &cancel); err != nil {
			c.respondError(conn, id, CodeInvalidParams, "invalid tool.cancel params")
			return
		}
		c.respondResult(conn, id, c.cancelToolCall(cancel))
	case MethodSessionAssign:
		var assign SessionAssignParams
		if err := json.Unmarshal(params, &assign); err != nil {
			c.respondError(conn, id, CodeInvalidParams, "invalid session.assign params")
			return
		}
		result, err := c.assignSession(assign)
		if err != nil {
			c.respondHandlerError(conn, id, err)
			return
		}
		c.respondResult(conn, id, result)
	case MethodSessionStop:
		var stop SessionStopParams
		if err := json.Unmarshal(params, &stop); err != nil {
			c.respondError(conn, id, CodeInvalidParams, "invalid session.stop params")
			return
		}
		result, err := c.stopSession(stop)
		if err != nil {
			c.respondHandlerError(conn, id, err)
			return
		}
		c.respondResult(conn, id, result)
	case MethodExecutorDrain:
		var drain ExecutorDrainParams
		if err := json.Unmarshal(params, &drain); err != nil {
			c.respondError(conn, id, CodeInvalidParams, "invalid executor.drain params")
			return
		}
		result, err := c.drain(drain)
		if err != nil {
			c.respondError(conn, id, CodeInternalError, err.Error())
			return
		}
		c.respondResult(conn, id, result)
	default:
		c.mu.Lock()
		handler, ok := c.requestHandlers[method]
		c.mu.Unlock()
		if !ok {
			c.respondHandlerError(conn, id, unregisteredMethodError(method))
			return
		}
		result, err := handler(context.WithValue(c.rootCtx, requestIDKey{}, id), params)
		if err != nil {
			c.respondHandlerError(conn, id, err)
			return
		}
		c.respondResult(conn, id, result)
	}
}

// unregisteredMethodError answers a request no handler serves: a contract
// method behind a capability this executor did not register is
// capability_unavailable naming that capability; anything else is
// method_not_found.
func unregisteredMethodError(method string) *RPCError {
	spec, ok := Methods[method]
	if !ok || spec.Capability == "" || spec.Direction == DirectionExecutorToHarness {
		return &RPCError{Code: CodeMethodNotFound, Message: "unknown method " + method}
	}
	return &RPCError{Code: CodeCapabilityUnavailable, Message: "capability_unavailable",
		Data: &JSONRPCErrorData{Capability: spec.Capability}}
}

func (c *Client) dispatchNotification(method string, params json.RawMessage) {
	if method == MethodExecutorClose {
		c.finish(nil, 1000, "executor.close")
		return
	}
	c.mu.Lock()
	handler, ok := c.notificationHandlers[method]
	c.mu.Unlock()
	if ok {
		handler(params)
	}
}

func (c *Client) assignSession(params SessionAssignParams) (SessionAssignResult, error) {
	c.mu.Lock()
	draining := c.state == ConnectionStateDraining
	handler := c.sessionAssignHandler
	c.mu.Unlock()
	if draining {
		return SessionAssignResult{Accepted: false, SessionID: params.SessionID}, nil
	}
	result := SessionAssignResult{Accepted: true, SessionID: params.SessionID}
	if handler != nil {
		var err error
		result, err = handler(c.rootCtx, params)
		if err != nil {
			return SessionAssignResult{}, err
		}
	}
	if result.Accepted {
		c.mu.Lock()
		c.sessions[params.SessionID] = struct{}{}
		c.mu.Unlock()
	}
	return result, nil
}

func (c *Client) stopSession(params SessionStopParams) (SessionStopResult, error) {
	c.mu.Lock()
	handler := c.sessionStopHandler
	c.mu.Unlock()
	result := SessionStopResult{Stopped: true}
	if handler != nil {
		var err error
		result, err = handler(c.rootCtx, params)
		if err != nil {
			return SessionStopResult{}, err
		}
	}
	c.mu.Lock()
	delete(c.sessions, params.SessionID)
	c.mu.Unlock()
	return result, nil
}

func (c *Client) drain(params ExecutorDrainParams) (ExecutorDrainResult, error) {
	c.mu.Lock()
	c.state = ConnectionStateDraining
	handler := c.drainHandler
	c.mu.Unlock()
	if handler != nil {
		return handler(c.rootCtx, params)
	}
	return ExecutorDrainResult{Accepted: true, ActiveSessions: c.ActiveSessions()}, nil
}

// beginToolCall registers an inbound tool.call exactly once. It returns false
// for a duplicate tool_call_id, which the harness may resend after reconnect.
func (c *Client) beginToolCall(params ToolCallParams) (context.Context, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.seenToolCalls[params.ToolCallID]; ok {
		return nil, false
	}
	if _, ok := c.runningToolCalls[params.ToolCallID]; ok {
		return nil, false
	}
	c.rememberToolCallLocked(c.seenToolCalls, params.ToolCallID)

	var (
		callCtx context.Context
		cancel  context.CancelFunc
	)
	if params.DeadlineMS > 0 {
		callCtx, cancel = context.WithTimeout(c.rootCtx, time.Duration(params.DeadlineMS)*time.Millisecond)
	} else {
		callCtx, cancel = context.WithCancel(c.rootCtx)
	}
	c.runningToolCalls[params.ToolCallID] = &runningToolCall{cancel: cancel}
	return callCtx, true
}

// cancelToolCall aborts a running tool.call. The handler observes cancellation
// through its context and the client reports outcome=canceled.
func (c *Client) cancelToolCall(params ToolCancelParams) ToolCancelResult {
	c.mu.Lock()
	running, ok := c.runningToolCalls[params.ToolCallID]
	if ok {
		running.canceled = true
		running.reason = params.Reason
	}
	c.mu.Unlock()
	if !ok {
		return ToolCancelResult{Canceled: false}
	}
	running.cancel()
	c.log(LogInfo, "canceled tool.call "+params.ToolCallID, nil)
	return ToolCancelResult{Canceled: true}
}

func (c *Client) runToolCall(ctx context.Context, params ToolCallParams) {
	c.mu.Lock()
	handler := c.toolCallHandler
	c.mu.Unlock()

	var result ToolResultParams
	if handler == nil {
		result = failedToolResult(params, "no tool handler registered")
	} else {
		var err error
		result, err = handler(ctx, params)
		if err != nil {
			result = failedToolResult(params, err.Error())
		}
	}

	c.mu.Lock()
	running := c.runningToolCalls[params.ToolCallID]
	canceled := running != nil && running.canceled
	reason := ""
	if running != nil {
		reason = running.reason
	}
	delete(c.runningToolCalls, params.ToolCallID)
	hooks := slices.Clone(c.afterToolResults)
	c.mu.Unlock()

	if canceled {
		result = canceledToolResult(params, reason)
	}
	// The result must identify the call the harness dispatched, whatever the
	// handler put in the struct.
	result.ToolCallID = params.ToolCallID
	result.SessionID = params.SessionID

	if _, err := c.ReportToolResult(c.rootCtx, result); err != nil {
		c.log(LogError, "failed to report tool result for "+params.ToolCallID, err)
		if c.options.OnToolResultLost != nil {
			c.options.OnToolResultLost(result, err)
		}
	}
	for _, hook := range hooks {
		hook(params)
	}
}

func failedToolResult(params ToolCallParams, message string) ToolResultParams {
	return ToolResultParams{
		SessionID:  params.SessionID,
		ToolCallID: params.ToolCallID,
		Outcome:    ToolOutcomeFailed,
		IsError:    true,
		Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: message}},
	}
}

func canceledToolResult(params ToolCallParams, reason string) ToolResultParams {
	text := "canceled"
	if reason != "" {
		text += ": " + reason
	}
	return ToolResultParams{
		SessionID:  params.SessionID,
		ToolCallID: params.ToolCallID,
		Outcome:    ToolOutcomeCanceled,
		Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: text}},
	}
}

func (c *Client) respondResult(conn *connection, id string, result any) {
	payload, err := json.Marshal(rpcSuccessEnvelope{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		c.log(LogError, "marshal JSON-RPC result", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.rootCtx, 5*time.Second)
	defer cancel()
	if err := conn.write(ctx, payload); err != nil {
		c.log(LogWarn, "write JSON-RPC result", err)
	}
}

// respondHandlerError answers with the handler's *RPCError code and data (for
// example executor_busy for a full executor), or internal_error otherwise.
func (c *Client) respondHandlerError(conn *connection, id string, err error) {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		c.respondError(conn, id, CodeInternalError, err.Error())
		return
	}
	payload, marshalErr := json.Marshal(rpcErrorEnvelope{
		JSONRPC: "2.0", ID: id, Error: JSONRPCError{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data},
	})
	if marshalErr != nil {
		c.log(LogError, "marshal JSON-RPC error", marshalErr)
		return
	}
	ctx, cancel := context.WithTimeout(c.rootCtx, 5*time.Second)
	defer cancel()
	if err := conn.write(ctx, payload); err != nil {
		c.log(LogWarn, "write JSON-RPC error", err)
	}
}

func (c *Client) respondError(conn *connection, id string, code int, message string) {
	payload, err := json.Marshal(rpcErrorEnvelope{
		JSONRPC: "2.0",
		ID:      id,
		Error:   JSONRPCError{Code: code, Message: message},
	})
	if err != nil {
		c.log(LogError, "marshal JSON-RPC error", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.rootCtx, 5*time.Second)
	defer cancel()
	if err := conn.write(ctx, payload); err != nil {
		c.log(LogWarn, "write JSON-RPC error", err)
	}
}

func (c *Client) heartbeatLoop() {
	for {
		c.mu.Lock()
		interval := c.heartbeatInterval
		c.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-c.rootCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		c.heartbeat()
	}
}

func (c *Client) heartbeat() {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if state != ConnectionStateReady && state != ConnectionStateDraining {
		return
	}
	params := ExecutorHeartbeatParams{ConnectionID: c.ConnectionID(), ActiveSessions: c.ActiveSessions()}
	if c.options.HeartbeatLoad != nil {
		params.Load = c.options.HeartbeatLoad()
	}
	var result ExecutorHeartbeatResult
	if err := c.Call(c.rootCtx, MethodExecutorHeartbeat, params, &result); err != nil {
		c.log(LogWarn, "executor heartbeat failed", err)
		return
	}
	if result.Drain {
		c.mu.Lock()
		if c.state == ConnectionStateReady {
			c.state = ConnectionStateDraining
		}
		c.mu.Unlock()
	}
}

func (c *Client) disconnected(conn *connection, err error) {
	c.mu.Lock()
	if c.conn != conn {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	pending := c.pending
	c.pending = make(map[string]chan rpcOutcome)
	stopped := c.stopped
	if c.disconnectedAt.IsZero() {
		c.disconnectedAt = time.Now()
	}
	c.mu.Unlock()
	for _, channel := range pending {
		channel <- rpcOutcome{err: ErrNotConnected}
	}
	if stopped {
		return
	}
	c.setState(ConnectionStateDisconnected)
	if c.finishOnTerminalClose(err) {
		return
	}
	c.log(LogWarn, "executor connection lost", err)
	c.scheduleReconnect()
}

// finishOnTerminalClose stops the client for good when err carries a
// terminal close code, whether it ended a registered connection or a
// re-registration.
func (c *Client) finishOnTerminalClose(err error) bool {
	switch code := CloseCode(err); {
	case code == 4004:
		c.finish(ErrReplaced, 1000, "replaced")
	case terminalCloseCode(code):
		c.finish(err, 1000, "terminal close")
	default:
		return false
	}
	return true
}

func (c *Client) scheduleReconnect() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if c.options.MaxReconnectAttempts > 0 && c.reconnectAttempts >= c.options.MaxReconnectAttempts {
		c.mu.Unlock()
		c.log(LogError, "executor reconnect attempts exhausted", nil)
		c.finish(ErrReconnectExhausted, 1000, "reconnect exhausted")
		return
	}
	attempt := c.reconnectAttempts
	c.reconnectAttempts++
	var lost []string
	var hooks []func([]string)
	// v1.1 §3.9: once the harness lease has certainly expired the sessions
	// and their processes are gone on the harness side; stop them here too.
	if !c.disconnectedAt.IsZero() && time.Since(c.disconnectedAt) > c.leaseTTL && len(c.sessions) > 0 {
		lost = c.sessionIDsLocked()
		c.sessions = make(map[string]struct{})
		hooks = append(hooks, c.sessionsLost...)
	}
	c.mu.Unlock()
	if len(lost) > 0 {
		for _, hook := range hooks {
			hook(lost)
		}
	}

	delay := c.reconnectDelay(attempt)
	c.log(LogInfo, fmt.Sprintf("reconnecting executor channel in %s", delay), nil)
	time.AfterFunc(delay, func() { c.reconnect() })
}

// reconnectDelay is the exponential backoff for attempt, capped at
// ReconnectMaxDelay, with 50–100% jitter.
func (c *Client) reconnectDelay(attempt int) time.Duration {
	shift := attempt
	if shift > 6 {
		shift = 6
	}
	delay := float64(c.options.ReconnectBaseDelay) * float64(int(1)<<shift)
	if delay > float64(c.options.ReconnectMaxDelay) {
		delay = float64(c.options.ReconnectMaxDelay)
	}
	return time.Duration(delay * (0.5 + c.options.Random()*0.5))
}

func (c *Client) reconnect() {
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return
	}
	token, err := c.fetchToken(c.rootCtx)
	if err != nil {
		c.log(LogWarn, "executor reconnect token request failed", err)
		c.scheduleReconnect()
		return
	}
	if err := c.connect(c.rootCtx, token); err != nil {
		if c.finishOnTerminalClose(err) {
			return
		}
		c.log(LogWarn, "executor reconnect failed", err)
		c.scheduleReconnect()
	}
}

func (c *Client) dropConnection(conn *connection) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = conn.conn.Close(1011, "registration failed")
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan rpcOutcome)
	c.mu.Unlock()
	for _, channel := range pending {
		channel <- rpcOutcome{err: err}
	}
}

func (c *Client) setState(state ConnectionState) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

func (c *Client) log(level LogLevel, message string, err error) {
	if c.options.OnLog != nil {
		c.options.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// terminalCloseCode reports close codes after which the executor must not
// reconnect. 4001 (token expired) and 4005 (drained) reconnect.
func terminalCloseCode(code int) bool {
	switch code {
	case 1008, 4002, 4003, 4004:
		return true
	default:
		return false
	}
}

func authHeader(token string) http.Header {
	if token == "" {
		return nil
	}
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

// registeredSignal is closed after the next successful executor.register.
func (c *Client) registeredSignal() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registered
}

func stringPtr(value string) *string {
	return &value
}
