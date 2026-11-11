package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	agentInputBuffer    = 16
	agentResendDelay    = 200 * time.Millisecond
	agentCloseFlushWait = 30 * time.Second
	// agentSuspendWait bounds one SuspendSession; a turn.start waits for it.
	agentSuspendWait = 5 * time.Second
)

// AgentError is an error of RunTurn that names its cause with one of the
// contract's AgentErrorCode values (AgentErrorCodeAgentUnavailable,
// AgentErrorCodeAgentAuthRequired, ...): the turn ends failed with
// session.error of that type, so every agent says the same thing for the same
// cause. Any other error ends the turn with AgentErrorCodeAgentError.
type AgentError struct {
	// Code is an AgentErrorCode value.
	Code    string
	Message string
}

func (e *AgentError) Error() string { return e.Message }

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
	// is answered, and the platform answers turn.start with a timeout, so it
	// must be quick: refuse what can be refused for certain (unknown
	// harness, a cwd outside the allowed roots) and leave anything slow, such as
	// starting a process or opening the local conversation, to RunTurn, whose
	// failure ends the turn with session.error (AgentError picks its type) and
	// whose canceled ctx ends it canceled. An *RPCError keeps its
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

// AgentSessionSuspender is an optional extension of AgentHandler for a handler
// that holds local resources per session (a process, a conversation in memory,
// a Core session). The SDK counts two things apart:
//
//   - running turns, bounded by AgentCapability.MaxSessions: a turn.start that
//     would exceed it answers executor_busy, and the slot is free again when
//     the turn ends;
//   - resident sessions, bounded by MaxResidentSessions: sessions whose
//     resources are held, running or idle. A session with no running turn
//     costs no slot; it only keeps its resources until it is the least
//     recently used idle one when the limit is passed.
//
// Past the limit the SDK calls SuspendSession for the least recently used idle
// session before it opens the next turn. A suspended session is not ended: the
// platform still has it, its events keep their numbering, and its next
// turn.start goes to OpenTurn like any other, where the handler loads the
// conversation again (turn.start carries the executor_session_ref). Without
// this extension resident sessions are not bounded.
type AgentSessionSuspender interface {
	// MaxResidentSessions is the limit; 0 means unbounded.
	MaxResidentSessions() int
	// SuspendSession releases the resources of an idle session and keeps its
	// conversation so that a later OpenTurn can continue it. It must be safe
	// for a session the handler no longer has. An error keeps the session
	// resident; the SDK then suspends the next candidate.
	SuspendSession(ctx context.Context, sessionID string) error
}

// ServeAgent advertises the agent capability and routes the agent methods to
// handler. Call it before Start.
func (c *Client) ServeAgent(handler AgentHandler, capability AgentCapability) {
	capability.Enabled = true
	runtime := &agentRuntime{client: c, handler: handler, capability: capability, sessions: map[string]*agentSession{}}
	c.mu.Lock()
	c.agent = runtime
	c.mu.Unlock()
	c.onRequest(MethodTurnStart, runtime.turnStartRaw)
	c.onRequest(MethodTurnInput, decodeThen(runtime.turnInput))
	c.onRequest(MethodTurnCancel, decodeThen(runtime.turnCancel))
	c.onRequest(MethodSessionClose, decodeThen(runtime.sessionClose))
	c.onRequest(MethodInteractionResponse, decodeThen(runtime.interactionResponse))
	c.onRequest(MethodAgentCustomToolResult, decodeThen(runtime.customToolResult))
}

func decodeThen[P any, R any](handle func(context.Context, P) (R, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var params P
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
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
	// active counts the turns holding a slot (AgentCapability.MaxSessions):
	// reserved by turn.start, released when the turn ends or is refused.
	active atomic.Int64
}

// agentSession is one platform session on this executor: its outbox of
// durable events and at most one running turn.
type agentSession struct {
	runtime *agentRuntime
	id      string

	mu      sync.Mutex
	nextSeq int64
	nextID  uint64
	outbox  []agentOutboxEvent
	wake    chan struct{}
	drained chan struct{} // closed while the outbox is empty
	closed  bool
	turn    *AgentTurn
	// lastUsed orders idle sessions for suspension (least recent first).
	lastUsed time.Time
	// suspended: the handler released the session's resources (SuspendSession);
	// the next turn.start loads them again. suspending is non-nil, and closed
	// when done, while SuspendSession runs.
	suspended  bool
	suspending chan struct{}
	results    map[string]TurnStartResult // by turn_id, for idempotent turn.start
	pending    map[string]chan InteractionResponseParams
	answered   map[string]bool
	// customTools: waiters by tool_use_id; customDone: ids no longer waited on.
	customTools map[string]chan AgentCustomToolResultParams
	customDone  map[string]bool
}

type agentOutboxEvent struct {
	params agentSessionEventParams
	// onAck runs once the platform acknowledged the event.
	onAck func()
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

	toolsOnce sync.Once
	tools     ToolRoute
}

func (t *AgentTurn) SessionID() string       { return t.session.id }
func (t *AgentTurn) TurnID() string          { return t.params.TurnID }
func (t *AgentTurn) Params() TurnStartParams { return t.params }

// RefreshModelToken asks the platform for a fresh model credential for this
// turn (Client.RefreshModelToken).
func (t *AgentTurn) RefreshModelToken(ctx context.Context) (ModelTokenRefreshResult, error) {
	return t.session.runtime.client.RefreshModelToken(ctx, t.session.id, t.params.TurnID)
}

// RefreshToolsToken asks the platform for a fresh credential for this turn's
// platform tool calls (Client.RefreshToolsToken).
func (t *AgentTurn) RefreshToolsToken(ctx context.Context) (ToolsTokenRefreshResult, error) {
	return t.session.runtime.client.RefreshToolsToken(ctx, t.session.id, t.params.TurnID)
}

// EnvironmentInfo describes the hands machine when brain and hands are on
// different machines (turn.start config.environment_info, contract 1.4.0);
// nil when the tools run on this machine.
func (t *AgentTurn) EnvironmentInfo() *EnvironmentInfo {
	if t.params.Config == nil {
		return nil
	}
	return t.params.Config.EnvironmentInfo
}

// Tools returns the route this turn's tool calls go through when the hands are
// on another machine (ADR-0029), or nil when they are this machine.
//
// A turn started with the platform's tools endpoint (turn.start metadata
// tools_mcp_url and tools_mcp_token, contract 1.4.0; the executor declared
// AgentCapability.platform_credentials brain_tools) gets a PlatformToolRoute:
// every call goes to the platform's tool gateway and none may run on this
// machine. The token stays in memory and is replaced through
// tools_token.refresh before it expires. One of the two metadata keys without
// the other gives a route that refuses every call (tools_unavailable) instead
// of nil, so a half credential never turns into a local run.
//
// nil means the turn carries no tools endpoint: the brain's tools run here, on
// the brain's own account (ACPAgent does it through Core with its own
// re-checks; a brain of your own runs them itself). The route is the same
// value for every call of the turn.
func (t *AgentTurn) Tools() ToolRoute {
	t.toolsOnce.Do(func() {
		md := t.params.Metadata
		if !handsAreRemote(md) {
			return
		}
		endpoint, token := md[metaToolsURL], md[metaToolsToken]
		if endpoint == "" || token == "" {
			t.tools = refusingRoute{detail: "turn.start metadata tools_mcp_url and tools_mcp_token must come together; the turn's tools are not run on this machine"}
			return
		}
		// An unparsable or missing expiry is never refreshed (the platform did not say).
		expiresAt, _ := time.Parse(time.RFC3339, md[metaToolsExpiresAt])
		route, err := NewPlatformToolRoute(PlatformToolRouteOptions{
			Endpoint: endpoint, Token: token, ExpiresAt: expiresAt, SessionID: t.session.id, TurnID: t.params.TurnID,
			Refresh: t.RefreshToolsToken, OnLog: t.session.runtime.client.log,
		})
		if err != nil {
			t.tools = refusingRoute{detail: "turn.start metadata tools_mcp_url is not usable: " + err.Error()}
			return
		}
		t.tools = route
	})
	return t.tools
}

// ApprovalID is the interaction id to ask a tool call's approval under
// (InteractionRequestParams.InteractionID) and to present with the call
// (RouteCall.ApprovalID): "appr_" and 24 hex characters of sha256 over session,
// turn and call id (ADR-0027). It is the same after a reconnect or an executor
// restart, so the platform keeps one interaction per call and an approval
// already answered is not asked again.
func (t *AgentTurn) ApprovalID(callID string) string {
	return ApprovalID(t.session.id, t.params.TurnID, callID)
}

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

// emitAcked is Emit that calls onAck when the platform acknowledged the event.
func (t *AgentTurn) emitAcked(event any, onAck func()) error {
	if t.ended.Load() {
		return errors.New("bmaexecutor: turn already ended")
	}
	return t.session.enqueueAck(t.params.TurnID, t.NewEventID(), event, onAck)
}

// Delta streams a transient preview of the durable event eventID. It is
// best-effort: dropped while disconnected and never resent.
func (t *AgentTurn) Delta(eventID, eventType string, index int64, text string) {
	c := t.session.runtime.client
	params := SessionEventDeltaParams{SessionID: t.session.id, TurnID: t.params.TurnID, EventID: eventID, EventType: eventType, Index: index, Text: text}
	if err := Notifications.SessionEventDelta.Notify(c.rootCtx, c, params); err != nil {
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
		_, err := Calls.InteractionRequest.Call(ctx, s.runtime.client, params)
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

// RequestCustomTool emits agent.custom_tool_use for a tool the user's
// application runs (capability CustomTools) and waits for the platform's
// agent.custom_tool_result. It returns the call's event id with the result so
// the agent can reference it. The turn stays open while it waits: do not end
// it, and do not emit session.status_idle. When ctx ends first it returns
// ctx.Err() and a later result answers accepted=false; to give up on a call
// the agent emits a failed AgentToolResultEvent naming the returned id
// (empty when the event could not be emitted). Use ctx with a timeout for an
// executor-side limit.
func (t *AgentTurn) RequestCustomTool(ctx context.Context, name string, input map[string]any) (string, AgentCustomToolResultParams, error) {
	return t.requestCustomTool(ctx, t.NewEventID(), name, input)
}

// requestCustomTool is RequestCustomTool under a given event id. An id
// derived from the session, the turn and the agent's call id is the same
// after an executor restart, so the platform hands back a result given while
// the executor was away (it keys the call by that id).
func (t *AgentTurn) requestCustomTool(ctx context.Context, id, name string, input map[string]any) (string, AgentCustomToolResultParams, error) {
	s := t.session
	answer := make(chan AgentCustomToolResultParams, 1)
	// Register before emitting: the platform may answer as soon as the
	// event is acknowledged.
	s.mu.Lock()
	s.customTools[id] = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.customTools, id)
		s.customDone[id] = true
		s.mu.Unlock()
	}()
	if _, err := t.EmitWithID(id, AgentCustomToolUseEvent{Type: AgentEventTypeAgentCustomToolUse, Name: name, Input: input}); err != nil {
		return "", AgentCustomToolResultParams{}, err
	}
	select {
	case result := <-answer:
		return id, result, nil
	case <-ctx.Done():
		return id, AgentCustomToolResultParams{}, ctx.Err()
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
		_, err := Calls.InteractionCancel.Call(ctx, c, InteractionCancelParams{SessionID: s.id, InteractionID: interactionID, Reason: reason})
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
	drained := make(chan struct{})
	close(drained)
	s := &agentSession{
		runtime: r, id: id, nextSeq: 1, lastUsed: time.Now(), wake: make(chan struct{}, 1), drained: drained,
		results: map[string]TurnStartResult{}, pending: map[string]chan InteractionResponseParams{}, answered: map[string]bool{},
		customTools: map[string]chan AgentCustomToolResultParams{}, customDone: map[string]bool{},
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
	if params.Config != nil {
		if params.Config.InputSkillReceipt != nil {
			return TurnStartResult{}, &RPCError{Code: CodeInvalidParams, Message: "input_skill_receipt is effective-only"}
		}
		if err := validateInputSkillDocuments(params.Config.InputSkills); err != nil {
			return TurnStartResult{}, err
		}
		if len(params.Config.InputSkills) > 0 && !r.capability.InputSkills {
			return TurnStartResult{}, &RPCError{Code: CodeCapabilityUnavailable, Message: "input_skills is not supported"}
		}
	}
	if params.SessionID == "" || params.TurnID == "" || len(params.Input) == 0 {
		return TurnStartResult{}, &RPCError{Code: CodeInvalidParams, Message: "session_id, turn_id and input are required"}
	}
	// A turn.start of a turn the session already knows is answered as before,
	// slot or no slot.
	if existing, _ := r.session(params.SessionID, false); existing != nil {
		existing.mu.Lock()
		result, known := existing.results[params.TurnID]
		running := existing.turn != nil
		existing.mu.Unlock()
		if known {
			return result, nil
		}
		if running {
			return TurnStartResult{}, &RPCError{Code: CodeConflict, Message: "a turn is already running; use turn.input"}
		}
	}
	// One slot per running turn (MaxSessions); an idle session holds none.
	if !r.acquireSlot() {
		return TurnStartResult{}, &RPCError{Code: CodeExecutorBusy, Message: "executor_busy"}
	}
	slot := true
	defer func() {
		if slot {
			r.active.Add(-1)
		}
	}()
	s, err := r.session(params.SessionID, true)
	if err != nil {
		return TurnStartResult{}, err
	}
	turnCtx, cancel := context.WithCancel(r.client.rootCtx)
	if params.DeadlineMS > 0 {
		turnCtx, cancel = context.WithTimeout(r.client.rootCtx, time.Duration(params.DeadlineMS)*time.Millisecond)
	}
	turn := &AgentTurn{session: s, params: params, inputs: make(chan TurnInputParams, agentInputBuffer), seen: map[string]bool{params.InputID: true}, cancel: cancel, done: make(chan struct{})}
	for {
		s.mu.Lock()
		if done := s.suspending; done != nil {
			// The session is being suspended: open it only after that ended.
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				cancel()
				return TurnStartResult{}, ctx.Err()
			}
		}
		break
	}
	if result, ok := s.results[params.TurnID]; ok {
		s.mu.Unlock()
		cancel()
		return result, nil
	}
	if s.turn != nil {
		s.mu.Unlock()
		cancel()
		return TurnStartResult{}, &RPCError{Code: CodeConflict, Message: "a turn is already running; use turn.input"}
	}
	s.turn = turn // reserve the session before OpenTurn
	s.suspended = false
	s.lastUsed = time.Now()
	s.mu.Unlock()
	slot = false // the turn owns it now, see finishTurn
	r.suspendSurplus(ctx, s)

	result, err := r.handler.OpenTurn(ctx, params)
	if err == nil && !result.Accepted {
		err = &RPCError{Code: CodeExecutorBusy, Message: "turn not accepted"}
	}
	s.mu.Lock()
	if err != nil {
		s.mu.Unlock()
		s.finishTurn(turn)
		cancel()
		return TurnStartResult{}, err
	}
	result.TurnID = params.TurnID
	s.results[params.TurnID] = result
	s.mu.Unlock()
	go r.runTurn(turnCtx, turn)
	return result, nil
}

// acquireSlot reserves one running-turn slot; false when MaxSessions turns run.
func (r *agentRuntime) acquireSlot() bool {
	max := int64(r.capability.MaxSessions)
	for {
		current := r.active.Load()
		if max > 0 && current >= max {
			return false
		}
		if r.active.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// finishTurn ends the session's running turn (if turn still is it): the
// session is idle, most recently used now, and the turn's slot is free.
func (s *agentSession) finishTurn(turn *AgentTurn) {
	s.mu.Lock()
	owned := s.turn == turn
	if owned {
		s.turn = nil
		s.lastUsed = time.Now()
	}
	s.mu.Unlock()
	if owned {
		s.runtime.active.Add(-1)
	}
}

// suspendSurplus suspends the least recently used idle sessions until at most
// MaxResidentSessions are resident, never keep (the session about to run) and
// never one with a running turn; a candidate's unacknowledged events are
// settled first.
// A session whose suspension fails stays resident, so the limit is soft: it
// can be passed when nothing else is idle.
func (r *agentRuntime) suspendSurplus(ctx context.Context, keep *agentSession) {
	suspender, ok := r.handler.(AgentSessionSuspender)
	if !ok {
		return
	}
	limit := suspender.MaxResidentSessions()
	if limit <= 0 {
		return
	}
	failed := map[*agentSession]bool{}
	for {
		victim := r.claimVictim(limit, keep, failed)
		if victim == nil {
			return
		}
		suspendCtx, cancel := context.WithTimeout(ctx, agentSuspendWait)
		// Its last events may still be unacknowledged: their callbacks use the
		// handler's state, so they are settled before that is released.
		err := victim.flush(suspendCtx)
		if err == nil {
			err = suspender.SuspendSession(suspendCtx, victim.id)
		}
		cancel()
		victim.mu.Lock()
		done := victim.suspending
		victim.suspending = nil
		victim.suspended = err == nil
		victim.mu.Unlock()
		close(done)
		if err != nil {
			failed[victim] = true
			r.client.log(LogWarn, "suspending idle session "+victim.id+" failed; it stays resident", err)
		}
	}
}

// claimVictim marks the least recently used idle resident session as being
// suspended and returns it, or nil when the resident sessions are within limit
// or none can be suspended. Sessions being suspended already do not count.
func (r *agentRuntime) claimVictim(limit int, keep *agentSession, failed map[*agentSession]bool) *agentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	resident := 0
	var candidates []*agentSession
	for _, s := range r.sessions {
		s.mu.Lock()
		if !s.suspended && s.suspending == nil && !s.closed {
			resident++
			if s != keep && s.turn == nil && !failed[s] {
				candidates = append(candidates, s)
			}
		}
		s.mu.Unlock()
	}
	if resident <= limit {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		candidates[i].mu.Lock()
		a := candidates[i].lastUsed
		candidates[i].mu.Unlock()
		candidates[j].mu.Lock()
		b := candidates[j].lastUsed
		candidates[j].mu.Unlock()
		return a.Before(b)
	})
	for _, s := range candidates {
		s.mu.Lock()
		// Check again: a turn.start may have taken it since the scan.
		if !s.suspended && s.suspending == nil && !s.closed && s.turn == nil {
			s.suspending = make(chan struct{})
			s.mu.Unlock()
			return s
		}
		s.mu.Unlock()
	}
	return nil
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
		code := AgentErrorCodeAgentError
		if agentErr := (*AgentError)(nil); errors.As(err, &agentErr) && agentErr.Code != "" {
			code = agentErr.Code
		}
		_ = s.enqueue(turnID, turn.NewEventID(), SessionErrorEvent{Type: AgentEventTypeSessionError, Error: AgentErrorInfo{Type: code, Message: err.Error()}})
	}
	_ = s.enqueue(turnID, turn.NewEventID(), SessionStatusIdleEvent{
		Type: AgentEventTypeSessionStatusIdle, StopReason: StopReason{Type: StopReasonTypeEndTurn}, TurnOutcome: outcome,
	})
	s.finishTurn(turn)
}

func (r *agentRuntime) turnInput(_ context.Context, params TurnInputParams) (Ack, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return Ack{}, &RPCError{Code: CodeSessionNotActive, Message: "session_not_active"}
	}
	s.mu.Lock()
	turn := s.turn
	if turn == nil || turn.params.TurnID != params.TurnID || turn.ended.Load() {
		s.mu.Unlock()
		return Ack{}, &RPCError{Code: CodeConflict, Message: "turn is not running; start a new turn"}
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
		return Ack{}, &RPCError{Code: CodeConflict, Message: "turn ended; start a new turn"}
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
	turn, suspending := s.turn, s.suspending
	s.mu.Unlock()
	if suspending != nil {
		select {
		case <-suspending:
		case <-ctx.Done():
			return SessionCloseResult{}, ctx.Err()
		}
	}
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

func (r *agentRuntime) customToolResult(_ context.Context, params AgentCustomToolResultParams) (CustomToolResultResult, error) {
	s, _ := r.session(params.SessionID, false)
	if s == nil {
		return CustomToolResultResult{Accepted: false}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	waiter, open := s.customTools[params.ToolUseID]
	if !open || s.customDone[params.ToolUseID] {
		return CustomToolResultResult{Accepted: false}, nil
	}
	s.customDone[params.ToolUseID] = true
	waiter <- params
	return CustomToolResultResult{Accepted: true}, nil
}

// --- outbox ---

func (s *agentSession) enqueue(turnID, eventID string, event any) error {
	return s.enqueueAck(turnID, eventID, event, nil)
}

// enqueueAck is enqueue with a callback for the platform's acknowledgement.
func (s *agentSession) enqueueAck(turnID, eventID string, event any, onAck func()) error {
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
	s.outbox = append(s.outbox, agentOutboxEvent{params: params, onAck: onAck})
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
			return c.callInto(ctx, MethodSessionEvent, head.params, &result)
		})
		if err != nil && c.rootCtx.Err() != nil {
			return
		}
		if err != nil {
			// A permanent rejection (invalid event, unknown session) cannot be
			// fixed by resending; it is surfaced instead of blocking the session.
			c.log(LogError, fmt.Sprintf("session.event %s rejected", head.params.EventID), err)
		} else if head.onAck != nil {
			head.onAck()
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
			retryable := rpcErr.Data != nil && rpcErr.Data.Retryable
			if !retryable && rpcErr.Code != CodeExecutorBusy {
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
