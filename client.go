// Package bmaexecutor implements the BMA executor protocol transport for
// bma.executor.v1: token bootstrap, outbound WebSocket, JSON-RPC 2.0,
// registration, heartbeat, reconnect, dispatch, session lifecycle, tool
// cancellation, retried tool results, artifact references, and the MCP channel.
package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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
	maxTrackedToolResults           = 8192
	maxToolResultRetryDelay         = 10 * time.Second
	maxFrameBytes                   = 1 << 20
)

var (
	// ErrNotConnected is returned when a call is attempted before the channel opens.
	ErrNotConnected = errors.New("bmaexecutor: executor channel is not connected")
	// ErrClientStopped is returned after Stop or a terminal close code.
	ErrClientStopped = errors.New("bmaexecutor: client stopped")
)

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
	EnvironmentKey string
	EnvironmentID  string
	Executor       ExecutorInfo
	Capabilities   ExecutorCapabilities

	BaseURL string
	WSURL   string
	// TokenSource replaces the POST /v1/executors/token bootstrap, for example
	// StaticToken for a local harness on a loopback listener. When set,
	// EnvironmentKey and BaseURL are not used.
	TokenSource func(ctx context.Context) (ExecutorTokenResponse, error)
	// HTTPClient is used for the token request. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// RequestTimeout bounds one JSON-RPC request. Defaults to 30s.
	RequestTimeout time.Duration
	// HeartbeatInterval overrides the interval returned by executor.register.
	HeartbeatInterval time.Duration
	// HeartbeatLoad is sampled for each heartbeat.
	HeartbeatLoad        func() *ExecutorLoad
	MaxReconnectAttempts int
	ReconnectBaseDelay   time.Duration
	Random               func() float64
	// ToolResultRetryAttempts is the total number of tool.result attempts,
	// including the first. It defaults to 5; set it to 1 to disable retries.
	ToolResultRetryAttempts int
	// ToolResultRetryBaseDelay is the first backoff for retryable tool.result
	// failures. It defaults to 250ms and doubles per attempt.
	ToolResultRetryBaseDelay time.Duration
	// OnToolResultLost is called by the dispatch path when a tool result could
	// not be accepted after all retries, or the error is not retryable. The
	// adapter must persist the result and replay it with the same
	// tool_call_id; the client never drops a result silently.
	OnToolResultLost func(params ToolResultParams, err error)
	OnLog            func(LogEvent)
}

// RPCError is a JSON-RPC error returned by the harness.
type RPCError struct {
	Code    JSONRPCErrorCode
	Message string
	Data    *JSONRPCErrorData
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("bmaexecutor: json-rpc error %d: %s", e.Code, e.Message)
}

type connection struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

func (c *connection) write(ctx context.Context, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, payload)
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

// Client is an outbound executor client.
type Client struct {
	options    Options
	httpClient *http.Client

	rootCtx context.Context
	cancel  context.CancelFunc

	mu                   sync.Mutex
	conn                 *connection
	state                ConnectionState
	stopped              bool
	registerResult       *ExecutorRegisterResult
	connectionID         string
	executorID           string
	heartbeatInterval    time.Duration
	sessions             map[string]struct{}
	pending              map[string]chan rpcOutcome
	reconnectAttempts    int
	completedToolResults map[string]struct{}
	seenToolCalls        map[string]struct{}
	runningToolCalls     map[string]*runningToolCall
	toolCallHandler      ToolCallHandler
	sessionAssignHandler SessionAssignHandler
	sessionStopHandler   SessionStopHandler
	drainHandler         DrainHandler
	requestHandlers      map[string]func(context.Context, json.RawMessage) (any, error)
	notificationHandlers map[string]func(json.RawMessage)
	nextID               uint64
}

// NewClient validates options and returns a disconnected client.
func NewClient(options Options) (*Client, error) {
	if options.TokenSource == nil && strings.TrimSpace(options.EnvironmentKey) == "" {
		return nil, errors.New("bmaexecutor: EnvironmentKey is required")
	}
	if strings.TrimSpace(options.EnvironmentID) == "" {
		return nil, errors.New("bmaexecutor: EnvironmentID is required")
	}
	if options.Executor.Name == "" {
		return nil, errors.New("bmaexecutor: Executor.Name is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	options.BaseURL = strings.TrimRight(options.BaseURL, "/")
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultHeartbeatInterval
	}
	if options.MaxReconnectAttempts <= 0 {
		options.MaxReconnectAttempts = DefaultMaxReconnectAttempts
	}
	if options.ReconnectBaseDelay <= 0 {
		options.ReconnectBaseDelay = DefaultReconnectBaseDelay
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
		state:                ConnectionStateDisconnected,
		heartbeatInterval:    options.HeartbeatInterval,
		sessions:             make(map[string]struct{}),
		pending:              make(map[string]chan rpcOutcome),
		completedToolResults: make(map[string]struct{}),
		seenToolCalls:        make(map[string]struct{}),
		runningToolCalls:     make(map[string]*runningToolCall),
		requestHandlers:      make(map[string]func(context.Context, json.RawMessage) (any, error)),
		notificationHandlers: make(map[string]func(json.RawMessage)),
	}, nil
}

// Start fetches an executor token, connects, registers, and starts heartbeats.
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

// Stop sends executor.close, closes the WebSocket, and fails pending calls.
func (c *Client) Stop(ctx context.Context, reason string) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	conn := c.conn
	c.conn = nil
	c.state = ConnectionStateClosed
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
		_ = conn.ws.Close(websocket.StatusNormalClosure, reason)
	}
	c.cancel()
	c.failPending(ErrClientStopped)
	return nil
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
// failures with exponential backoff. A duplicate call for an accepted
// tool_call_id returns accepted without sending another frame.
func (c *Client) ReportToolResult(ctx context.Context, params ToolResultParams) (Ack, error) {
	c.mu.Lock()
	if _, ok := c.completedToolResults[params.ToolCallID]; ok {
		c.mu.Unlock()
		return Ack{Accepted: true}, nil
	}
	c.rememberToolCallLocked(c.completedToolResults, params.ToolCallID)
	c.mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= c.options.ToolResultRetryAttempts; attempt++ {
		var ack Ack
		err := c.Call(ctx, MethodToolResult, params, &ack)
		if err == nil {
			return ack, nil
		}
		lastErr = err
		if attempt == c.options.ToolResultRetryAttempts || !retryableToolResultError(err) {
			break
		}
		c.log(LogWarn, fmt.Sprintf("tool.result for %s failed on attempt %d, retrying", params.ToolCallID, attempt), err)
		if err := c.waitToolResultRetry(ctx, attempt); err != nil {
			lastErr = err
			break
		}
	}

	c.mu.Lock()
	delete(c.completedToolResults, params.ToolCallID)
	c.mu.Unlock()
	return Ack{}, lastErr
}

// SessionReady reports that the executor accepted session.assign and has
// prepared the session. Call it after workspace and MCP setup complete.
func (c *Client) SessionReady(ctx context.Context, params SessionReadyParams) (Ack, error) {
	var ack Ack
	err := c.Call(ctx, MethodSessionReady, params, &ack)
	return ack, err
}

// SessionEnded reports that every process and MCP server for the session has
// stopped. The session leaves ActiveSessions after the harness accepts.
func (c *Client) SessionEnded(ctx context.Context, params SessionEndedParams) (SessionEndedResult, error) {
	var result SessionEndedResult
	if err := c.Call(ctx, MethodSessionEnded, params, &result); err != nil {
		return SessionEndedResult{}, err
	}
	c.mu.Lock()
	delete(c.sessions, params.SessionID)
	c.mu.Unlock()
	return result, nil
}

func retryableToolResultError(err error) bool {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Data != nil && rpcErr.Data.Retryable != nil {
			return *rpcErr.Data.Retryable
		}
		switch rpcErr.Code {
		case JSONRPCErrorCodeExecutorBusy, JSONRPCErrorCodeTimeout:
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

// StaticToken is a TokenSource for a harness reached directly, typically a
// local harness on ws://127.0.0.1. token may be empty when the harness has no
// Authenticate hook.
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
	wsURL := c.options.WSURL
	if wsURL == "" {
		wsURL = token.WSURL
	}
	c.setState(ConnectionStateConnecting)
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader:   authHeader(token.Token),
		Subprotocols: []string{WSSubprotocol},
	})
	if err != nil {
		return fmt.Errorf("bmaexecutor: websocket dial: %w", err)
	}
	ws.SetReadLimit(maxFrameBytes)
	conn := &connection{ws: ws}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		_ = ws.Close(websocket.StatusNormalClosure, "stopped")
		return ErrClientStopped
	}
	c.conn = conn
	c.mu.Unlock()
	go c.readLoop(conn)

	params := ExecutorRegisterParams{
		Protocol:      ProtocolVersion,
		EnvironmentID: c.options.EnvironmentID,
		Executor:      c.options.Executor,
		Capabilities:  c.options.Capabilities,
		Resume:        c.resumeState(),
	}
	var result ExecutorRegisterResult
	if err := c.Call(ctx, MethodExecutorRegister, params, &result); err != nil {
		c.dropConnection(conn)
		return err
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
	c.state = ConnectionStateReady
	c.reconnectAttempts = 0
	c.mu.Unlock()
	c.log(LogInfo, "executor registered as "+result.ExecutorID, nil)
	return nil
}

func (c *Client) resumeState() *ExecutorResume {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.executorID == "" && c.connectionID == "" && len(c.sessions) == 0 {
		return nil
	}
	resume := &ExecutorResume{ActiveSessions: c.sessionIDsLocked()}
	if c.executorID != "" {
		resume.ExecutorID = stringPtr(c.executorID)
	}
	if c.connectionID != "" {
		resume.LastConnectionID = stringPtr(c.connectionID)
	}
	return resume
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
		return nil, fmt.Errorf("bmaexecutor: write %s: %w", method, err)
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
		_, data, err := conn.ws.Read(c.rootCtx)
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
			c.respondError(conn, id, JSONRPCErrorCodeInvalidParams, "invalid tool.call params")
			return
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
			c.respondError(conn, id, JSONRPCErrorCodeInvalidParams, "invalid tool.cancel params")
			return
		}
		c.respondResult(conn, id, c.cancelToolCall(cancel))
	case MethodSessionAssign:
		var assign SessionAssignParams
		if err := json.Unmarshal(params, &assign); err != nil {
			c.respondError(conn, id, JSONRPCErrorCodeInvalidParams, "invalid session.assign params")
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
			c.respondError(conn, id, JSONRPCErrorCodeInvalidParams, "invalid session.stop params")
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
			c.respondError(conn, id, JSONRPCErrorCodeInvalidParams, "invalid executor.drain params")
			return
		}
		result, err := c.drain(drain)
		if err != nil {
			c.respondError(conn, id, JSONRPCErrorCodeInternalError, err.Error())
			return
		}
		c.respondResult(conn, id, result)
	default:
		c.mu.Lock()
		handler, ok := c.requestHandlers[method]
		c.mu.Unlock()
		if !ok {
			c.respondError(conn, id, JSONRPCErrorCodeMethodNotFound, "unknown method "+method)
			return
		}
		result, err := handler(c.rootCtx, params)
		if err != nil {
			c.respondHandlerError(conn, id, err)
			return
		}
		c.respondResult(conn, id, result)
	}
}

func (c *Client) dispatchNotification(method string, params json.RawMessage) {
	if method == MethodExecutorClose {
		c.mu.Lock()
		c.stopped = true
		conn := c.conn
		c.conn = nil
		c.state = ConnectionStateClosed
		c.mu.Unlock()
		if conn != nil {
			_ = conn.ws.Close(websocket.StatusNormalClosure, "executor.close")
		}
		c.failPending(ErrClientStopped)
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
	if params.DeadlineMS != nil && *params.DeadlineMS > 0 {
		callCtx, cancel = context.WithTimeout(c.rootCtx, time.Duration(*params.DeadlineMS)*time.Millisecond)
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
}

func failedToolResult(params ToolCallParams, message string) ToolResultParams {
	text := message
	return ToolResultParams{
		SessionID:  params.SessionID,
		ToolCallID: params.ToolCallID,
		Outcome:    ToolOutcomeFailed,
		IsError:    true,
		Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: &text}},
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
		Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: &text}},
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
		c.respondError(conn, id, JSONRPCErrorCodeInternalError, err.Error())
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

func (c *Client) respondError(conn *connection, id string, code JSONRPCErrorCode, message string) {
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
	c.mu.Unlock()
	for _, channel := range pending {
		channel <- rpcOutcome{err: ErrNotConnected}
	}
	if stopped {
		c.setState(ConnectionStateClosed)
		return
	}
	c.setState(ConnectionStateDisconnected)
	code := websocket.CloseStatus(err)
	if terminalCloseCode(int(code)) {
		c.mu.Lock()
		c.state = ConnectionStateClosed
		c.mu.Unlock()
		return
	}
	c.scheduleReconnect()
}

func (c *Client) scheduleReconnect() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if c.reconnectAttempts >= c.options.MaxReconnectAttempts {
		c.state = ConnectionStateClosed
		c.mu.Unlock()
		c.log(LogError, "executor reconnect attempts exhausted", nil)
		return
	}
	attempt := c.reconnectAttempts
	c.reconnectAttempts++
	c.mu.Unlock()

	delay := float64(c.options.ReconnectBaseDelay) * float64(int(1)<<attempt)
	if delay > float64(30*time.Second) {
		delay = float64(30 * time.Second)
	}
	delay *= 0.5 + c.options.Random()*0.5
	c.log(LogInfo, fmt.Sprintf("reconnecting executor channel in %s", time.Duration(delay)), nil)
	time.AfterFunc(time.Duration(delay), func() { c.reconnect() })
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
	_ = conn.ws.Close(websocket.StatusInternalError, "registration failed")
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

func terminalCloseCode(code int) bool {
	switch code {
	case 1008, 4002, 4003, 4004, 4005:
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

func stringPtr(value string) *string {
	return &value
}
