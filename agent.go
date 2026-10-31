package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// v1.1 error codes the v1 generated.go does not carry yet.
const (
	codeCapabilityUnavailable JSONRPCErrorCode = -32012
	codeConflict              JSONRPCErrorCode = -32013
)

const (
	agentInputBuffer    = 16
	agentResendDelay    = 200 * time.Millisecond
	agentCloseFlushWait = 30 * time.Second
)

// ErrAgentSessionClosed is returned by AgentTurn methods after session.close.
var ErrAgentSessionClosed = errors.New("bmaexecutor: agent session closed")

// AgentHandler is a complete local agent (model loop and tools) served over
// the executor protocol. The SDK owns the wire: turn.start/input/cancel,
// session.close, event ids and sequences, ACK tracking and resend after
// reconnect, interactions, and the session.status_running / status_idle
// events that frame every turn.
type AgentHandler interface {
	// OpenTurn decides whether to accept a turn.start and reports the
	// configuration the agent will actually apply. It runs before turn.start
	// is answered, so it must not do the turn's work. An *RPCError keeps its
	// code (for example -32001 for a cwd outside the allowed roots).
	OpenTurn(ctx context.Context, params TurnStartParams) (TurnStartResult, error)
	// RunTurn does the turn's work and returns when the turn is over. ctx is
	// canceled by turn.cancel and session.close. A nil error ends the turn as
	// completed, a canceled ctx as canceled, any other error as failed (the
	// SDK emits session.error first).
	RunTurn(ctx context.Context, turn *AgentTurn) error
	// CloseSession releases the local conversation after session.close.
	CloseSession(ctx context.Context, sessionID, reason string) error
}

// ServeAgent advertises the agent capability and routes the agent methods to
// handler. Call it before Start.
func (c *Client) ServeAgent(handler AgentHandler, capability AgentCapability) {
	capability.Enabled = true
	runtime := &agentRuntime{client: c, handler: handler, capability: capability, sessions: map[string]*agentSession{}}
	c.mu.Lock()
	c.agent = runtime
	c.mu.Unlock()
	c.OnRequest(MethodTurnStart, decodeThen(runtime.turnStart))
	c.OnRequest(MethodTurnInput, decodeThen(runtime.turnInput))
	c.OnRequest(MethodTurnCancel, decodeThen(runtime.turnCancel))
	c.OnRequest(MethodSessionClose, decodeThen(runtime.sessionClose))
	c.OnRequest(MethodInteractionResponse, decodeThen(runtime.interactionResponse))
}

func decodeThen[P any, R any](handle func(context.Context, P) (R, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &RPCError{Code: JSONRPCErrorCodeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		return handle(ctx, params)
	}
}

type agentRuntime struct {
	client     *Client
	handler    AgentHandler
	capability AgentCapability

	mu       sync.Mutex
	sessions map[string]*agentSession
}

// agentSession is one platform session on this executor: its outbox of
// durable events and at most one running turn.
type agentSession struct {
	runtime *agentRuntime
	id      string

	mu       sync.Mutex
	nextSeq  int64
	nextID   uint64
	outbox   []agentOutboxEvent
	wake     chan struct{}
	drained  chan struct{} // closed while the outbox is empty
	closed   bool
	turn     *AgentTurn
	results  map[string]TurnStartResult // by turn_id, for idempotent turn.start
	pending  map[string]chan InteractionResponseParams
	answered map[string]bool
}

type agentOutboxEvent struct {
	params agentSessionEventParams
}

// agentSessionEventParams is session.event with the agent members; the v1
// SessionEventParams has neither them nor a raw event.
type agentSessionEventParams struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	EventID   string          `json:"event_id"`
	Sequence  int64           `json:"sequence"`
	Event     json.RawMessage `json:"event"`
}

type agentSessionEventResult struct {
	Accepted      bool   `json:"accepted"`
	EventID       string `json:"event_id"`
	AckedSequence int64  `json:"acked_sequence"`
}

// AgentTurn is the handle RunTurn uses to talk to the platform.
type AgentTurn struct {
	session *agentSession
	params  TurnStartParams
	inputs  chan TurnInputParams
	seen    map[string]bool
	cancel  context.CancelFunc
	done    chan struct{}
	ended   atomic.Bool
}

func (t *AgentTurn) SessionID() string       { return t.session.id }
func (t *AgentTurn) TurnID() string          { return t.params.TurnID }
func (t *AgentTurn) Params() TurnStartParams { return t.params }

// Inputs delivers turn.input messages in arrival order.
func (t *AgentTurn) Inputs() <-chan TurnInputParams { return t.inputs }

// NewEventID returns a fresh event id, for a message that is streamed with
// Delta before it is emitted with EmitWithID.
func (t *AgentTurn) NewEventID() string {
	s := t.session
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return fmt.Sprintf("evt_%s_%d", s.id, s.nextID)
}

// Emit queues one durable event (one of the Agent*Event / Session*Event
// structs) and returns its event id. Delivery is asynchronous, ordered and
// at-least-once; the platform deduplicates by event id.
func (t *AgentTurn) Emit(event any) (string, error) {
	return t.EmitWithID(t.NewEventID(), event)
}

// EmitWithID is Emit with a caller-chosen event id (from NewEventID).
func (t *AgentTurn) EmitWithID(eventID string, event any) (string, error) {
	if t.ended.Load() {
		return "", errors.New("bmaexecutor: turn already ended")
	}
	return eventID, t.session.enqueue(t.params.TurnID, eventID, event)
}

// Delta streams a transient preview of the durable event eventID. It is
// best-effort: dropped while disconnected and never resent.
func (t *AgentTurn) Delta(eventID, eventType string, index int64, text string) {
	c := t.session.runtime.client
	params := SessionEventDeltaParams{SessionID: t.session.id, TurnID: t.params.TurnID, EventID: eventID, EventType: eventType, Index: index, Text: text}
	if err := SendSessionEventDelta(c.rootCtx, c, params); err != nil {
		c.log(LogDebug, "session.event_delta dropped", err)
	}
}

// Flush waits until every event emitted so far is acknowledged.
func (t *AgentTurn) Flush(ctx context.Context) error { return t.session.flush(ctx) }

// RequestInteraction asks the platform for an approval or an answer and
// waits for interaction.response. It flushes first so the platform already
// has the agent.tool_use the request references. When ctx ends or
// ExpiresAt passes, it withdraws the interaction with interaction.cancel; an
// expiry returns outcome expired, which the agent must treat as a deny. The
// answer is advice: the local agent still makes the final decision.
func (t *AgentTurn) RequestInteraction(ctx context.Context, params InteractionRequestParams) (InteractionResponseParams, error) {
	s := t.session
	params.SessionID, params.TurnID = s.id, t.params.TurnID
	if params.InteractionID == "" {
		params.InteractionID = "int_" + t.NewEventID()[4:]
	}
	answer := make(chan InteractionResponseParams, 1)
	s.mu.Lock()
	s.pending[params.InteractionID] = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, params.InteractionID)
		s.mu.Unlock()
	}()
	if err := t.Flush(ctx); err != nil {
		return InteractionResponseParams{}, err
	}
	if err := s.callUntilConnected(ctx, func(ctx context.Context) error {
		_, err := SendInteractionRequest(ctx, s.runtime.client, params)
		return err
	}); err != nil {
		return InteractionResponseParams{}, err
	}
	var expiry <-chan time.Time
	if params.ExpiresAt != "" {
		at, err := time.Parse(time.RFC3339, params.ExpiresAt)
		if err != nil {
			return InteractionResponseParams{}, fmt.Errorf("bmaexecutor: expires_at: %w", err)
		}
		timer := time.NewTimer(time.Until(at))
		defer timer.Stop()
		expiry = timer.C
	}
	select {
	case response := <-answer:
		return response, nil
	case <-expiry:
		s.withdraw(params.InteractionID, InteractionCancelReasonExpired)
		return InteractionResponseParams{SessionID: s.id, InteractionID: params.InteractionID, Outcome: InteractionOutcomeExpired}, nil
	case <-ctx.Done():
		s.withdraw(params.InteractionID, InteractionCancelReasonTurnCanceled)
		return InteractionResponseParams{}, ctx.Err()
	}
}

// CancelInteraction withdraws an open interaction, for example after the
// user answered it in the local UI (InteractionCancelReasonResolvedLocally).
// A later interaction.response for it answers accepted=false.
func (t *AgentTurn) CancelInteraction(interactionID, reason string) {
	t.session.withdraw(interactionID, reason)
}

func (s *agentSession) withdraw(interactionID, reason string) {
	s.mu.Lock()
	s.answered[interactionID] = true
	s.mu.Unlock()
	c := s.runtime.client
	ctx, cancel := context.WithTimeout(c.rootCtx, agentCloseFlushWait)
	defer cancel()
	err := s.callUntilConnected(ctx, func(ctx context.Context) error {
		_, err := SendInteractionCancel(ctx, c, InteractionCancelParams{SessionID: s.id, InteractionID: interactionID, Reason: reason})
		return err
	})
	if err != nil {
		c.log(LogWarn, "interaction.cancel for "+interactionID+" failed", err)
	}
}

// --- request handlers ---

func (r *agentRuntime) session(id string, create bool) (*agentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		return s, nil
	}
	if !create {
		return nil, nil
	}
	if max := r.capability.MaxSessions; max > 0 && int64(len(r.sessions)) >= max {
		return nil, &RPCError{Code: JSONRPCErrorCodeExecutorBusy, Message: "executor_busy"}
	}
	drained := make(chan struct{})
	close(drained)
	s := &agentSession{
		runtime: r, id: id, nextSeq: 1, wake: make(chan struct{}, 1), drained: drained,
		results: map[string]TurnStartResult{}, pending: map[string]chan InteractionResponseParams{}, answered: map[string]bool{},
	}
	r.sessions[id] = s
	c := r.client
	c.mu.Lock()
	c.sessions[id] = struct{}{}
	c.mu.Unlock()
	go s.sendLoop()
	return s, nil
}

func (r *agentRuntime) turnStart(ctx context.Context, params TurnStartParams) (TurnStartResult, error) {
	if params.SessionID == "" || params.TurnID == "" || len(params.Input) == 0 {
		return TurnStartResult{}, &RPCError{Code: JSONRPCErrorCodeInvalidParams, Message: "session_id, turn_id and input are required"}
	}
	s, err := r.session(params.SessionID, true)
	if err != nil {
		return TurnStartResult{}, err
	}
	s.mu.Lock()
	if result, ok := s.results[params.TurnID]; ok {
		s.mu.Unlock()
		return result, nil
	}
	if s.turn != nil {
		s.mu.Unlock()
		return TurnStartResult{}, &RPCError{Code: codeConflict, Message: "a turn is already running; use turn.input"}
	}
	turnCtx, cancel := context.WithCancel(r.client.rootCtx)
	if params.DeadlineMS > 0 {
		turnCtx, cancel = context.WithTimeout(r.client.rootCtx, time.Duration(params.DeadlineMS)*time.Millisecond)
	}
	turn := &AgentTurn{session: s, params: params, inputs: make(chan TurnInputParams, agentInputBuffer), seen: map[string]bool{params.InputID: true}, cancel: cancel, done: make(chan struct{})}
	s.turn = turn // reserve the session before OpenTurn
	s.mu.Unlock()

	result, err := r.handler.OpenTurn(ctx, params)
	if err == nil && !result.Accepted {
		err = &RPCError{Code: JSONRPCErrorCodeExecutorBusy, Message: "turn not accepted"}
	}
	s.mu.Lock()
	if err != nil {
		s.turn = nil
		s.mu.Unlock()
		cancel()
		return TurnStartResult{}, err
	}
	result.TurnID = params.TurnID
	s.results[params.TurnID] = result
	s.mu.Unlock()
	go r.runTurn(turnCtx, turn)
	return result, nil
}

func (r *agentRuntime) runTurn(ctx context.Context, turn *AgentTurn) {
	defer close(turn.done)
	defer turn.cancel()
	s := turn.session
	turnID := turn.params.TurnID
	_ = s.enqueue(turnID, turn.NewEventID(), SessionStatusRunningEvent{Type: AgentEventTypeSessionStatusRunning})
	err := r.handler.RunTurn(ctx, turn)
	turn.ended.Store(true)
	outcome := TurnOutcomeCompleted
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome = TurnOutcomeFailed
		_ = s.enqueue(turnID, turn.NewEventID(), SessionErrorEvent{Type: AgentEventTypeSessionError, Error: AgentErrorInfo{Type: "timeout", Message: "turn deadline_ms exceeded"}})
	case ctx.Err() != nil:
		outcome = TurnOutcomeCanceled
	case err != nil:
		outcome = TurnOutcomeFailed
		_ = s.enqueue(turnID, turn.NewEventID(), SessionErrorEvent{Type: AgentEventTypeSessionError, Error: AgentErrorInfo{Type: "agent_error", Message: err.Error()}})
	}
	_ = s.enqueue(turnID, turn.NewEventID(), SessionStatusIdleEvent{
		Type: AgentEventTypeSessionStatusIdle, StopReason: StopReason{Type: StopReasonTypeEndTurn}, TurnOutcome: outcome,
	})
	s.mu.Lock()
	if s.turn == turn {
		s.turn = nil
	}
	s.mu.Unlock()
}

func (r *agentRuntime) turnInput(_ context.Context, params TurnInputParams) (Ack, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return Ack{}, &RPCError{Code: JSONRPCErrorCodeSessionNotActive, Message: "session_not_active"}
	}
	s.mu.Lock()
	turn := s.turn
	if turn == nil || turn.params.TurnID != params.TurnID || turn.ended.Load() {
		s.mu.Unlock()
		return Ack{}, &RPCError{Code: codeConflict, Message: "turn is not running; start a new turn"}
	}
	if turn.seen[params.InputID] {
		s.mu.Unlock()
		return Ack{Accepted: true}, nil
	}
	turn.seen[params.InputID] = true
	s.mu.Unlock()
	select {
	case turn.inputs <- params:
		return Ack{Accepted: true}, nil
	case <-turn.done:
		return Ack{}, &RPCError{Code: codeConflict, Message: "turn ended; start a new turn"}
	}
}

func (r *agentRuntime) turnCancel(_ context.Context, params TurnCancelParams) (TurnCancelResult, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return TurnCancelResult{Canceled: false}, nil
	}
	s.mu.Lock()
	turn := s.turn
	s.mu.Unlock()
	if turn == nil || turn.params.TurnID != params.TurnID || turn.ended.Load() {
		return TurnCancelResult{Canceled: false}, nil
	}
	turn.cancel()
	return TurnCancelResult{Canceled: true}, nil
}

func (r *agentRuntime) sessionClose(ctx context.Context, params SessionCloseParams) (SessionCloseResult, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return SessionCloseResult{Closed: false}, nil
	}
	s.mu.Lock()
	turn := s.turn
	s.mu.Unlock()
	if turn != nil {
		turn.cancel()
		<-turn.done
	}
	flushCtx, cancel := context.WithTimeout(ctx, agentCloseFlushWait)
	defer cancel()
	if err := s.flush(flushCtx); err != nil {
		r.client.log(LogWarn, "session.close: events of "+s.id+" still unacknowledged", err)
	}
	if err := r.handler.CloseSession(ctx, s.id, params.Reason); err != nil {
		return SessionCloseResult{}, err
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.signal()
	r.mu.Lock()
	delete(r.sessions, s.id)
	r.mu.Unlock()
	c := r.client
	c.mu.Lock()
	delete(c.sessions, s.id)
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.rootCtx, agentCloseFlushWait)
		defer cancel()
		if err := s.callUntilConnected(ctx, func(ctx context.Context) error {
			_, err := c.SessionEnded(ctx, SessionEndedParams{SessionID: s.id, Reason: params.Reason})
			return err
		}); err != nil {
			c.log(LogWarn, "session.ended for "+s.id+" failed", err)
		}
	}()
	return SessionCloseResult{Closed: true}, nil
}

func (r *agentRuntime) interactionResponse(_ context.Context, params InteractionResponseParams) (InteractionResponseResult, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return InteractionResponseResult{Accepted: false}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	waiter, open := s.pending[params.InteractionID]
	if !open || s.answered[params.InteractionID] {
		return InteractionResponseResult{Accepted: false}, nil
	}
	s.answered[params.InteractionID] = true
	waiter <- params
	return InteractionResponseResult{Accepted: true}, nil
}

// --- outbox ---

func (s *agentSession) enqueue(turnID, eventID string, event any) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrAgentSessionClosed
	}
	params := agentSessionEventParams{SessionID: s.id, TurnID: turnID, EventID: eventID, Sequence: s.nextSeq, Event: raw}
	s.nextSeq++
	if len(s.outbox) == 0 {
		s.drained = make(chan struct{})
	}
	s.outbox = append(s.outbox, agentOutboxEvent{params: params})
	s.mu.Unlock()
	s.signal()
	return nil
}

func (s *agentSession) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *agentSession) flush(ctx context.Context) error {
	s.mu.Lock()
	drained := s.drained
	s.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendLoop delivers the outbox head first, one event at a time, so the
// platform sees every session in sequence order. A transport failure keeps
// the event and waits for the next registration; that is the resend after
// reconnect.
func (s *agentSession) sendLoop() {
	c := s.runtime.client
	for {
		s.mu.Lock()
		if len(s.outbox) == 0 {
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-s.wake:
				continue
			case <-c.rootCtx.Done():
				return
			}
		}
		head := s.outbox[0]
		s.mu.Unlock()

		err := s.callUntilConnected(c.rootCtx, func(ctx context.Context) error {
			var result agentSessionEventResult
			return c.Call(ctx, MethodSessionEvent, head.params, &result)
		})
		if err != nil && c.rootCtx.Err() != nil {
			return
		}
		if err != nil {
			// A permanent rejection (invalid event, unknown session) cannot be
			// fixed by resending; it is surfaced instead of blocking the session.
			c.log(LogError, fmt.Sprintf("session.event %s rejected", head.params.EventID), err)
		}
		s.mu.Lock()
		s.outbox = s.outbox[1:]
		if len(s.outbox) == 0 {
			close(s.drained)
		}
		s.mu.Unlock()
	}
}

// callUntilConnected retries call across disconnects. Only transport
// failures, executor_busy and retryable JSON-RPC errors are retried.
func (s *agentSession) callUntilConnected(ctx context.Context, call func(context.Context) error) error {
	c := s.runtime.client
	for {
		registered := c.registeredSignal()
		err := call(ctx)
		if err == nil {
			return nil
		}
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			retryable := rpcErr.Data != nil && rpcErr.Data.Retryable != nil && *rpcErr.Data.Retryable
			if !retryable && rpcErr.Code != JSONRPCErrorCodeExecutorBusy {
				return err
			}
		}
		if errors.Is(err, ErrClientStopped) {
			return err
		}
		connected := c.State() == ConnectionStateReady || c.State() == ConnectionStateDraining
		var wait <-chan time.Time
		if connected {
			wait = time.After(agentResendDelay)
		}
		select {
		case <-registered:
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
