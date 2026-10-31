package bmaexecutor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ProtocolV11 is the bma.executor.v1.1 subprotocol and register value. The
// harness accepts it alongside ProtocolVersion so it can drive executors on
// either revision; the method shapes it relies on are identical in both.
const ProtocolV11 = "bma.executor.v1.1"

const (
	DefaultHarnessHeartbeatInterval = 15 * time.Second
	DefaultHarnessLeaseTTL          = 60 * time.Second
	maxRememberedEventIDs           = 1024
)

var (
	// ErrSessionEnded is returned for requests on a session that has ended.
	ErrSessionEnded = errors.New("bmaexecutor: session ended")
	// ErrToolOutcomeUnknown means the executor was lost (lease expiry,
	// executor.close, session ended, or a restarted executor that no longer
	// reports the session) before a tool.result arrived. The tool may or may
	// not have run; the caller decides whether a retry is safe.
	ErrToolOutcomeUnknown = errors.New("bmaexecutor: tool outcome unknown: executor lost before tool.result")
	// ErrAssignRejected is returned when session.assign answers accepted=false.
	ErrAssignRejected = errors.New("bmaexecutor: executor rejected session.assign")
)

// Principal is what HarnessOptions.Authenticate learns from an upgrade
// request. A non-empty ExecutorID pins the executor identity: register and
// MCP channels must belong to it.
type Principal struct {
	ExecutorID string
}

// HarnessOptions configures a Harness.
type HarnessOptions struct {
	// Authenticate verifies every WebSocket upgrade (control and MCP). nil
	// accepts every connection: use that only on a loopback listener.
	Authenticate func(r *http.Request) (Principal, error)
	// HeartbeatInterval and LeaseTTL are returned by executor.register. A
	// disconnected executor keeps its sessions and pending tool calls for
	// LeaseTTL; then they end and pending calls fail with
	// ErrToolOutcomeUnknown.
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	// RequestTimeout bounds harness-to-executor requests. Defaults to 30s.
	RequestTimeout time.Duration
	// Protocols are the accepted subprotocols in preference order. Defaults
	// to ProtocolV11 then ProtocolVersion.
	Protocols []string

	// OnExecutor runs after each successful executor.register.
	OnExecutor func(*Executor)
	// OnSessionEvent persists session.event. nil accepts with a generated id.
	// The harness answers a retried JSON-RPC id with the first result.
	OnSessionEvent  func(ctx context.Context, executor *Executor, params SessionEventParams) (SessionEventResult, error)
	OnSessionStatus func(executor *Executor, params SessionStatusParams)
	OnToolProgress  func(executor *Executor, params ToolProgressParams)
	OnLog           func(LogEvent)
}

// Harness is the harness ("brain") end of the executor protocol: an
// http.Handler that accepts executor control and MCP channels, plus the
// session.assign / tool.call / tool.cancel / session.stop API that drives
// them. It is the same protocol the BMA service speaks, so an executor built
// on this SDK cannot tell a local harness from the cloud.
type Harness struct {
	opts HarnessOptions

	mu        sync.Mutex
	executors map[string]*Executor
	sessions  map[string]*Session
	changed   chan struct{}
	nextConn  atomic.Uint64
}

// NewHarness returns a harness with defaults applied.
func NewHarness(opts HarnessOptions) *Harness {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHarnessHeartbeatInterval
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = DefaultHarnessLeaseTTL
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if len(opts.Protocols) == 0 {
		opts.Protocols = []string{ProtocolV11, ProtocolVersion}
	}
	return &Harness{
		opts:      opts,
		executors: map[string]*Executor{},
		sessions:  map[string]*Session{},
		changed:   make(chan struct{}),
	}
}

// Executors returns the executors currently registered (connected or within
// their reconnect lease).
func (h *Harness) Executors() []*Executor {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Executor, 0, len(h.executors))
	for _, executor := range h.executors {
		out = append(out, executor)
	}
	return out
}

// WaitExecutor returns a connected executor, waiting for one to register.
func (h *Harness) WaitExecutor(ctx context.Context) (*Executor, error) {
	for {
		h.mu.Lock()
		for _, executor := range h.executors {
			if executor.connected() {
				h.mu.Unlock()
				return executor, nil
			}
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (h *Harness) signalLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// ServeHTTP accepts /v1/executors/channel style upgrades: the control channel
// by default, the MCP channel with ?channel=mcp&session_id=...&server_key=...
func (h *Harness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var principal Principal
	if h.opts.Authenticate != nil {
		var err error
		if principal, err = h.opts.Authenticate(r); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	switch r.URL.Query().Get("channel") {
	case "", string(ChannelScopeControl):
		h.serveControl(w, r, principal)
	case string(ChannelScopeMcp):
		h.serveMCP(w, r, principal)
	default:
		http.Error(w, "unknown channel", http.StatusBadRequest)
	}
}

func (h *Harness) accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	offered := strings.Join(r.Header.Values("Sec-WebSocket-Protocol"), ",")
	supported := false
	for _, protocol := range h.opts.Protocols {
		for _, value := range strings.Split(offered, ",") {
			supported = supported || strings.TrimSpace(value) == protocol
		}
	}
	if !supported {
		http.Error(w, "unsupported executor protocol", http.StatusBadRequest)
		return nil, false
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: h.opts.Protocols})
	if err != nil {
		h.log(LogWarn, "executor upgrade failed", err)
		return nil, false
	}
	ws.SetReadLimit(maxFrameBytes)
	return ws, true
}

func (h *Harness) serveControl(w http.ResponseWriter, r *http.Request, principal Principal) {
	ws, ok := h.accept(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &harnessConn{
		h: h, ws: ws, principal: principal, ctx: ctx, cancel: cancel,
		pending: map[string]chan rpcOutcome{},
		id:      fmt.Sprintf("conn_%d_%s", h.nextConn.Add(1), randomID(4)),
	}
	conn.readLoop()
}

func (h *Harness) log(level LogLevel, message string, err error) {
	if h.opts.OnLog != nil {
		h.opts.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// Executor is one registered executor. It outlives a single connection: a
// reconnect with resume.executor_id within the lease attaches to the same
// Executor and keeps its sessions and pending tool calls.
type Executor struct {
	h  *Harness
	id string

	mu            sync.Mutex
	conn          *harnessConn
	info          ExecutorInfo
	capabilities  json.RawMessage
	environmentID string
	protocol      string
	sessions      map[string]*Session
	leaseTimer    *time.Timer
	draining      bool
	gone          bool
	eventIDs      map[string]SessionEventResult
}

func (e *Executor) ID() string { return e.id }

func (e *Executor) Info() ExecutorInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.info
}

// Protocol is the negotiated subprotocol of the current connection.
func (e *Executor) Protocol() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.protocol
}

// Capabilities decodes the advertised capabilities into out (for example
// *ExecutorCapabilities, or a v1.1 capability struct).
func (e *Executor) Capabilities(out any) error {
	e.mu.Lock()
	raw := e.capabilities
	e.mu.Unlock()
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, out)
}

// Sessions returns the sessions bound to this executor.
func (e *Executor) Sessions() []*Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Session, 0, len(e.sessions))
	for _, session := range e.sessions {
		out = append(out, session)
	}
	return out
}

func (e *Executor) connected() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn != nil
}

func (e *Executor) call(ctx context.Context, method string, params, out any) error {
	e.mu.Lock()
	conn := e.conn
	e.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return conn.call(ctx, method, params, out)
}

// Drain asks the executor to stop accepting sessions; existing sessions run
// to completion. Heartbeats report drain=true from now on.
func (e *Executor) Drain(ctx context.Context, reason string) (ExecutorDrainResult, error) {
	e.mu.Lock()
	e.draining = true
	e.mu.Unlock()
	var result ExecutorDrainResult
	err := e.call(ctx, MethodExecutorDrain, ExecutorDrainParams{Reason: reason}, &result)
	return result, err
}

// AssignSession binds a session to this executor and sends session.assign.
// WorkID and the lease token are generated when empty. The session is bound
// before the request is sent, so session.ready and MCP channels that race the
// assign response are accepted.
func (e *Executor) AssignSession(ctx context.Context, params SessionAssignParams) (*Session, error) {
	if strings.TrimSpace(params.SessionID) == "" {
		return nil, errors.New("bmaexecutor: SessionID is required")
	}
	if params.WorkID == "" {
		params.WorkID = "work_" + randomID(8)
	}
	if params.Lease.Token == "" {
		params.Lease.Token = "lease_" + randomID(16)
	}
	e.mu.Lock()
	if params.EnvironmentID == "" {
		params.EnvironmentID = e.environmentID
	}
	e.mu.Unlock()
	session := &Session{
		executor: e, id: params.SessionID, workID: params.WorkID, leaseToken: params.Lease.Token,
		ready: make(chan struct{}), done: make(chan struct{}),
		calls: map[string]*toolCall{}, mcp: map[string]*HarnessMCPChannel{}, mcpChanged: make(chan struct{}),
	}
	if err := e.h.bind(e, session); err != nil {
		return nil, err
	}
	var result SessionAssignResult
	err := e.call(ctx, MethodSessionAssign, params, &result)
	if err == nil && !result.Accepted {
		err = ErrAssignRejected
	}
	if err != nil {
		session.end("assign_failed", ErrSessionEnded)
		return nil, err
	}
	return session, nil
}

func (h *Harness) bind(executor *Executor, session *Session) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.sessions[session.id]; exists {
		return fmt.Errorf("bmaexecutor: session %s is already bound", session.id)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.gone {
		return ErrNotConnected
	}
	h.sessions[session.id] = session
	executor.sessions[session.id] = session
	return nil
}

func (h *Harness) unbind(session *Session) {
	h.mu.Lock()
	if h.sessions[session.id] == session {
		delete(h.sessions, session.id)
	}
	h.mu.Unlock()
	executor := session.executor
	executor.mu.Lock()
	if executor.sessions[session.id] == session {
		delete(executor.sessions, session.id)
	}
	executor.mu.Unlock()
}

func (e *Executor) session(id string) (*Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.sessions[id]
	return session, ok
}

// endAll ends every session; pending tool calls fail with ErrToolOutcomeUnknown.
func (e *Executor) endAll(reason string) {
	for _, session := range e.Sessions() {
		session.end(reason, ErrToolOutcomeUnknown)
	}
}

func (e *Executor) forget(reason string) {
	e.mu.Lock()
	e.gone = true
	e.mu.Unlock()
	e.endAll(reason)
	e.h.mu.Lock()
	if e.h.executors[e.id] == e {
		delete(e.h.executors, e.id)
	}
	e.h.signalLocked()
	e.h.mu.Unlock()
}

// Session is one session bound to an executor.
type Session struct {
	executor   *Executor
	id         string
	workID     string
	leaseToken string

	readyOnce sync.Once
	ready     chan struct{}

	mu         sync.Mutex
	done       chan struct{}
	endReason  string
	calls      map[string]*toolCall
	mcp        map[string]*HarnessMCPChannel
	mcpChanged chan struct{}
}

type toolCall struct {
	done   chan struct{}
	result ToolResultParams
	err    error
}

func (s *Session) ID() string            { return s.id }
func (s *Session) WorkID() string        { return s.workID }
func (s *Session) Executor() *Executor   { return s.executor }
func (s *Session) Done() <-chan struct{} { return s.done }

// EndReason is set once Done is closed.
func (s *Session) EndReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endReason
}

// Ready waits for session.ready.
func (s *Session) Ready(ctx context.Context) error {
	select {
	case <-s.ready:
		return nil
	case <-s.done:
		return ErrSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) markReady() { s.readyOnce.Do(func() { close(s.ready) }) }

// CallTool sends tool.call and waits for its tool.result. It is idempotent by
// ToolCallID (generated when empty): repeating an id joins the in-flight call
// or returns the recorded result without re-dispatching. Canceling ctx stops
// waiting only; use CancelTool to abort the tool. The result is recorded even
// if nobody is waiting, so a later CallTool with the same id returns it.
func (s *Session) CallTool(ctx context.Context, params ToolCallParams) (ToolResultParams, error) {
	params.SessionID = s.id
	if params.ToolCallID == "" {
		params.ToolCallID = "call_" + randomID(8)
	}
	s.mu.Lock()
	call, exists := s.calls[params.ToolCallID]
	if !exists {
		if s.endReason != "" {
			s.mu.Unlock()
			return ToolResultParams{}, ErrSessionEnded
		}
		call = &toolCall{done: make(chan struct{})}
		s.calls[params.ToolCallID] = call
	}
	s.mu.Unlock()

	if !exists {
		var ack Ack
		err := s.executor.call(ctx, MethodToolCall, params, &ack)
		if err == nil && !ack.Accepted {
			err = errors.New("bmaexecutor: executor did not accept tool.call")
		}
		if err != nil {
			// Not dispatched: forget the call so the same id can be retried.
			s.mu.Lock()
			if s.calls[params.ToolCallID] == call {
				delete(s.calls, params.ToolCallID)
			}
			s.mu.Unlock()
			return ToolResultParams{}, err
		}
	}
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return ToolResultParams{}, ctx.Err()
	}
}

// CancelTool sends tool.cancel. The waiting CallTool still receives the
// executor's tool.result (outcome canceled when the abort won the race).
func (s *Session) CancelTool(ctx context.Context, toolCallID, reason string) (bool, error) {
	var result ToolCancelResult
	err := s.executor.call(ctx, MethodToolCancel, ToolCancelParams{SessionID: s.id, ToolCallID: toolCallID, Reason: reason}, &result)
	return result.Canceled, err
}

// Stop sends session.stop; the executor terminates the session's processes
// and MCP servers before answering. The session ends locally on success.
func (s *Session) Stop(ctx context.Context, reason string) error {
	var result SessionStopResult
	if err := s.executor.call(ctx, MethodSessionStop, SessionStopParams{SessionID: s.id, Reason: reason}, &result); err != nil {
		return err
	}
	s.end(reason, ErrToolOutcomeUnknown)
	return nil
}

// complete records a tool.result. It reports whether the call was dispatched
// by this harness; duplicates of a recorded result are accepted.
func (s *Session) complete(result ToolResultParams) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.calls[result.ToolCallID]
	if !ok {
		return false
	}
	select {
	case <-call.done:
	default:
		call.result = result
		close(call.done)
	}
	return true
}

// end unbinds the session, fails calls still waiting with pendingErr and
// closes its MCP channels. It is idempotent.
func (s *Session) end(reason string, pendingErr error) {
	s.mu.Lock()
	if s.endReason != "" {
		s.mu.Unlock()
		return
	}
	s.endReason = reason
	for _, call := range s.calls {
		select {
		case <-call.done:
		default:
			call.err = pendingErr
			close(call.done)
		}
	}
	channels := s.mcp
	s.mcp = map[string]*HarnessMCPChannel{}
	close(s.done)
	close(s.mcpChanged)
	s.mcpChanged = make(chan struct{})
	s.mu.Unlock()
	for _, channel := range channels {
		_ = channel.Close()
	}
	s.executor.h.unbind(s)
}

// --- connection ---

type harnessConn struct {
	h         *Harness
	ws        *websocket.Conn
	principal Principal
	id        string
	ctx       context.Context
	cancel    context.CancelFunc

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan rpcOutcome
	nextID  uint64
	// executor is set by executor.register.
	executor *Executor
}

func (c *harnessConn) readLoop() {
	defer c.cancel()
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.disconnected()
			return
		}
		var message rpcInbound
		if err := json.Unmarshal(data, &message); err != nil {
			c.h.log(LogWarn, "executor sent invalid JSON", err)
			continue
		}
		switch {
		case message.Method != "" && message.ID != nil:
			if message.Method == MethodExecutorRegister {
				// Serialized: nothing else is meaningful before register.
				c.handleRegister(*message.ID, message.Params)
				continue
			}
			go c.handleRequest(*message.ID, message.Method, message.Params)
		case message.Method != "":
			c.handleNotification(message.Method, message.Params)
		case message.ID != nil:
			c.settle(*message.ID, message)
		}
	}
}

func (c *harnessConn) currentExecutor() *Executor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executor
}

func (c *harnessConn) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.h.opts.RequestTimeout)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, payload)
}

func (c *harnessConn) respond(id string, result any, rpcErr *JSONRPCError) {
	var err error
	if rpcErr != nil {
		err = c.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: id, Error: *rpcErr})
	} else {
		err = c.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: id, Result: result})
	}
	if err != nil {
		c.h.log(LogDebug, "write response to executor", err)
	}
}

func rpcErr(code JSONRPCErrorCode, message string, retryable bool) *JSONRPCError {
	return &JSONRPCError{Code: code, Message: message, Data: &JSONRPCErrorData{Retryable: &retryable}}
}

func (c *harnessConn) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("h_%d", c.nextID)
	waiter := make(chan rpcOutcome, 1)
	c.pending[id] = waiter
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	callCtx, cancel := context.WithTimeout(ctx, c.h.opts.RequestTimeout)
	defer cancel()
	if err := c.write(rpcRequestEnvelope{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return fmt.Errorf("bmaexecutor: write %s: %w", method, err)
	}
	select {
	case outcome := <-waiter:
		if outcome.err != nil {
			return outcome.err
		}
		if out != nil && len(outcome.result) > 0 {
			if err := json.Unmarshal(outcome.result, out); err != nil {
				return fmt.Errorf("bmaexecutor: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-callCtx.Done():
		return fmt.Errorf("bmaexecutor: %s: %w", method, callCtx.Err())
	case <-c.ctx.Done():
		return ErrNotConnected
	}
}

func (c *harnessConn) settle(id string, message rpcInbound) {
	c.mu.Lock()
	waiter := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if waiter == nil {
		return
	}
	if message.Error != nil {
		waiter <- rpcOutcome{err: &RPCError{Code: message.Error.Code, Message: message.Error.Message, Data: message.Error.Data}}
		return
	}
	waiter <- rpcOutcome{result: message.Result}
}

type harnessRegisterParams struct {
	Protocol      string          `json:"protocol"`
	EnvironmentID string          `json:"environment_id"`
	Executor      ExecutorInfo    `json:"executor"`
	Capabilities  json.RawMessage `json:"capabilities"`
	Resume        *ExecutorResume `json:"resume,omitempty"`
}

type harnessRegisterResult struct {
	ExecutorRegisterResult
	Protocol string `json:"protocol"`
}

func (c *harnessConn) handleRegister(id string, raw json.RawMessage) {
	if c.currentExecutor() != nil {
		c.respond(id, nil, rpcErr(JSONRPCErrorCodeInvalidRequest, "already registered", false))
		return
	}
	var params harnessRegisterParams
	if err := json.Unmarshal(raw, &params); err != nil {
		c.respond(id, nil, rpcErr(JSONRPCErrorCodeInvalidParams, "invalid executor.register params", false))
		return
	}
	if params.Protocol != c.ws.Subprotocol() {
		c.respond(id, nil, rpcErr(JSONRPCErrorCodeUnsupportedProtocol, "protocol must match the negotiated subprotocol", false))
		return
	}
	resumeID := ""
	if params.Resume != nil && params.Resume.ExecutorID != nil {
		resumeID = *params.Resume.ExecutorID
	}
	executorID := c.principal.ExecutorID
	switch {
	case executorID != "" && resumeID != "" && resumeID != executorID:
		c.respond(id, nil, rpcErr(JSONRPCErrorCodeForbidden, "resume.executor_id does not match the authenticated executor", false))
		return
	case executorID == "" && resumeID != "":
		executorID = resumeID
	case executorID == "":
		executorID = "exe_" + randomID(8)
	}

	h := c.h
	h.mu.Lock()
	executor := h.executors[executorID]
	if executor == nil {
		executor = &Executor{h: h, id: executorID, sessions: map[string]*Session{}, eventIDs: map[string]SessionEventResult{}}
		h.executors[executorID] = executor
	}
	h.mu.Unlock()

	executor.mu.Lock()
	previous := executor.conn
	executor.conn = c
	executor.info = params.Executor
	executor.capabilities = params.Capabilities
	executor.environmentID = params.EnvironmentID
	executor.protocol = params.Protocol
	if executor.leaseTimer != nil {
		executor.leaseTimer.Stop()
		executor.leaseTimer = nil
	}
	draining := executor.draining
	executor.mu.Unlock()
	c.mu.Lock()
	c.executor = executor
	c.mu.Unlock()
	if previous != nil {
		// The new connection is authoritative (v1 §7: 4004 replaced).
		_ = previous.ws.Close(4004, "replaced by a newer connection")
	}

	// Sessions the executor no longer reports were lost with its old process.
	var keep []string
	if params.Resume != nil {
		keep = params.Resume.ActiveSessions
	}
	for _, session := range executor.Sessions() {
		if !slices.Contains(keep, session.id) {
			session.end("executor_lost", ErrToolOutcomeUnknown)
		}
	}

	c.respond(id, harnessRegisterResult{
		ExecutorRegisterResult: ExecutorRegisterResult{
			ExecutorID: executorID, ConnectionID: c.id, ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
			HeartbeatIntervalMS: h.opts.HeartbeatInterval.Milliseconds(), LeaseTTLMS: h.opts.LeaseTTL.Milliseconds(),
			MaxControlFrameBytes: maxFrameBytes, MaxMCPFrameBytes: maxFrameBytes, Drain: draining,
		},
		Protocol: params.Protocol,
	}, nil)
	h.mu.Lock()
	h.signalLocked()
	h.mu.Unlock()
	if h.opts.OnExecutor != nil {
		go h.opts.OnExecutor(executor)
	}
}

func (c *harnessConn) disconnected() {
	c.cancel()
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan rpcOutcome{}
	executor := c.executor
	c.mu.Unlock()
	for _, waiter := range pending {
		waiter <- rpcOutcome{err: ErrNotConnected}
	}
	if executor == nil {
		return
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.conn != c || executor.gone {
		return
	}
	executor.conn = nil
	executor.leaseTimer = time.AfterFunc(c.h.opts.LeaseTTL, func() {
		executor.mu.Lock()
		expired := executor.conn == nil && !executor.gone
		executor.mu.Unlock()
		if expired {
			executor.forget("lease_expired")
		}
	})
}

func (c *harnessConn) handleRequest(id, method string, raw json.RawMessage) {
	executor := c.currentExecutor()
	if executor == nil {
		c.respond(id, nil, rpcErr(JSONRPCErrorCodeInvalidRequest, "executor.register must come first", false))
		return
	}
	result, rpcError := c.dispatch(executor, id, method, raw)
	c.respond(id, result, rpcError)
}

func (c *harnessConn) dispatch(executor *Executor, id, method string, raw json.RawMessage) (any, *JSONRPCError) {
	switch method {
	case MethodExecutorHeartbeat:
		executor.mu.Lock()
		draining := executor.draining
		executor.mu.Unlock()
		return ExecutorHeartbeatResult{
			ServerTime: time.Now().UTC().Format(time.RFC3339Nano), LeaseTTLMS: c.h.opts.LeaseTTL.Milliseconds(), Drain: draining,
		}, nil
	case MethodSessionReady:
		var params SessionReadyParams
		if json.Unmarshal(raw, &params) != nil {
			return nil, rpcErr(JSONRPCErrorCodeInvalidParams, "invalid session.ready params", false)
		}
		session, ok := executor.session(params.SessionID)
		if !ok {
			return nil, rpcErr(JSONRPCErrorCodeSessionNotActive, "session is not active on this executor", false)
		}
		if params.LeaseToken != "" && params.LeaseToken != session.leaseToken {
			return nil, rpcErr(JSONRPCErrorCodeLeaseExpired, "lease token does not match", false)
		}
		session.markReady()
		return Ack{Accepted: true}, nil
	case MethodSessionEnded:
		var params SessionEndedParams
		if json.Unmarshal(raw, &params) != nil {
			return nil, rpcErr(JSONRPCErrorCodeInvalidParams, "invalid session.ended params", false)
		}
		if session, ok := executor.session(params.SessionID); ok {
			session.end(params.Reason, ErrToolOutcomeUnknown)
		}
		return SessionEndedResult{Accepted: true}, nil
	case MethodToolResult:
		var params ToolResultParams
		if json.Unmarshal(raw, &params) != nil {
			return nil, rpcErr(JSONRPCErrorCodeInvalidParams, "invalid tool.result params", false)
		}
		session, ok := executor.session(params.SessionID)
		if !ok {
			return nil, rpcErr(JSONRPCErrorCodeSessionNotActive, "session is not active on this executor", false)
		}
		if !session.complete(params) {
			return nil, rpcErr(JSONRPCErrorCodeNotFound, "tool_call_id was not dispatched by this harness", false)
		}
		return Ack{Accepted: true}, nil
	case MethodSessionEvent:
		var params SessionEventParams
		if json.Unmarshal(raw, &params) != nil {
			return nil, rpcErr(JSONRPCErrorCodeInvalidParams, "invalid session.event params", false)
		}
		if _, ok := executor.session(params.SessionID); !ok {
			return nil, rpcErr(JSONRPCErrorCodeSessionNotActive, "session is not active on this executor", false)
		}
		executor.mu.Lock()
		previous, replay := executor.eventIDs[id]
		executor.mu.Unlock()
		if replay {
			return previous, nil
		}
		result := SessionEventResult{Accepted: true, EventID: "evt_" + randomID(8)}
		if c.h.opts.OnSessionEvent != nil {
			var err error
			if result, err = c.h.opts.OnSessionEvent(c.ctx, executor, params); err != nil {
				return nil, rpcErr(JSONRPCErrorCodeInternalError, err.Error(), true)
			}
		}
		executor.mu.Lock()
		if len(executor.eventIDs) >= maxRememberedEventIDs {
			clear(executor.eventIDs)
		}
		executor.eventIDs[id] = result
		executor.mu.Unlock()
		return result, nil
	default:
		return nil, rpcErr(JSONRPCErrorCodeMethodNotFound, "unknown method "+method, false)
	}
}

func (c *harnessConn) handleNotification(method string, raw json.RawMessage) {
	executor := c.currentExecutor()
	if executor == nil {
		return
	}
	switch method {
	case MethodSessionStatus:
		var params SessionStatusParams
		if json.Unmarshal(raw, &params) == nil && c.h.opts.OnSessionStatus != nil {
			c.h.opts.OnSessionStatus(executor, params)
		}
	case MethodToolProgress:
		var params ToolProgressParams
		if json.Unmarshal(raw, &params) == nil && c.h.opts.OnToolProgress != nil {
			c.h.opts.OnToolProgress(executor, params)
		}
	case MethodExecutorCapabilitiesUpdate:
		var params struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		if json.Unmarshal(raw, &params) == nil && len(params.Capabilities) > 0 {
			executor.mu.Lock()
			executor.capabilities = params.Capabilities
			executor.mu.Unlock()
		}
	case MethodExecutorClose:
		// A deliberate shutdown: no reconnect grace.
		executor.forget("executor_closed")
		_ = c.ws.Close(websocket.StatusNormalClosure, "executor.close")
	}
}

func randomID(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
