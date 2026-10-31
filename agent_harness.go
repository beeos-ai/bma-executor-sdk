package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// AgentPlatform is the platform half of the agent capability: where durable
// agent events, previews and interactions go. BMA implements it on top of its
// session event log and agent interactions; tests and local tools can keep
// everything in memory.
type AgentPlatform interface {
	// AckedSequence is the highest contiguous sequence already persisted for
	// the session (0 when none). The harness asks once per session per process
	// so a restarted platform keeps deduplicating resent events.
	AckedSequence(ctx context.Context, sessionID string) (int64, error)
	// AgentEvent persists one durable event and returns once it is durable;
	// only then is it acknowledged to the executor and may be published to
	// clients. It is called once per event, in sequence order per session.
	AgentEvent(ctx context.Context, executor *Executor, event AgentEventRecord) error
	// AgentDelta relays a transient preview (Anthropic event_start /
	// event_delta). Never persisted.
	AgentDelta(executor *Executor, delta SessionEventDeltaParams)
	// InteractionRequested opens (or, for a repeated interaction_id, returns)
	// the platform interaction and returns its id. Answer it later with
	// Executor.RespondInteraction.
	InteractionRequested(ctx context.Context, executor *Executor, request InteractionRequestParams) (string, error)
	// InteractionCanceled closes the platform interaction without an answer.
	InteractionCanceled(ctx context.Context, executor *Executor, cancel InteractionCancelParams) error
}

// AgentEventRecord is one validated durable agent event.
type AgentEventRecord struct {
	SessionID string
	TurnID    string
	EventID   string
	Sequence  int64
	Type      string
	// Event is the event object as sent, for example an AgentMessageEvent.
	Event json.RawMessage
}

// agentLedger tracks the acknowledged sequence per session so resends are
// answered without reaching the platform twice.
type agentLedger struct {
	mu    sync.Mutex
	acked map[string]int64
	// session locks serialize AgentEvent per session.
	locks map[string]*sync.Mutex
}

func (l *agentLedger) lock(sessionID string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks, l.acked = map[string]*sync.Mutex{}, map[string]int64{}
	}
	m, ok := l.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		l.locks[sessionID] = m
	}
	return m
}

var agentEventTypes = map[string]bool{
	AgentEventTypeAgentMessage: true, AgentEventTypeAgentThinking: true, AgentEventTypeAgentToolUse: true,
	AgentEventTypeAgentToolResult: true, AgentEventTypeAgentMcpToolUse: true, AgentEventTypeAgentMcpToolResult: true,
	AgentEventTypeSessionStatusRunning: true, AgentEventTypeSessionStatusIdle: true, AgentEventTypeSessionError: true,
}

// dispatchAgent handles the agent methods an executor sends. handled is
// false for everything else, including session.event without event_id (a
// tool executor's event).
func (h *Harness) dispatchAgent(ctx context.Context, executor *Executor, method string, raw json.RawMessage) (any, *JSONRPCError, bool) {
	platform := h.opts.Agent
	if platform == nil {
		return nil, nil, false
	}
	switch method {
	case MethodSessionEvent:
		var params agentSessionEventParams
		if json.Unmarshal(raw, &params) != nil || params.EventID == "" {
			return nil, nil, false
		}
		result, rpcError := h.agentEvent(ctx, platform, executor, params)
		return result, rpcError, true
	case MethodInteractionRequest:
		var params InteractionRequestParams
		if json.Unmarshal(raw, &params) != nil || params.SessionID == "" || params.InteractionID == "" ||
			(params.Kind == InteractionKindPermission && params.ToolUseID == "") ||
			(params.Kind == InteractionKindQuestion && params.Question == nil) ||
			(params.Kind != InteractionKindPermission && params.Kind != InteractionKindQuestion) {
			return nil, rpcErr(CodeInvalidParams, "invalid interaction.request params", false), true
		}
		id, err := platform.InteractionRequested(ctx, executor, params)
		if err != nil {
			return nil, rpcErr(CodeInternalError, err.Error(), true), true
		}
		return InteractionRequestResult{Accepted: true, PlatformInteractionID: id}, nil, true
	case MethodInteractionCancel:
		var params InteractionCancelParams
		if json.Unmarshal(raw, &params) != nil || params.SessionID == "" || params.InteractionID == "" {
			return nil, rpcErr(CodeInvalidParams, "invalid interaction.cancel params", false), true
		}
		if err := platform.InteractionCanceled(ctx, executor, params); err != nil {
			return nil, rpcErr(CodeInternalError, err.Error(), true), true
		}
		return Ack{Accepted: true}, nil, true
	case MethodSessionEnded:
		// Agent sessions end with session.close; the hand-session path
		// answers session.ended for any session id, so nothing to add here.
		return nil, nil, false
	}
	return nil, nil, false
}

func (h *Harness) agentEvent(ctx context.Context, platform AgentPlatform, executor *Executor, params agentSessionEventParams) (any, *JSONRPCError) {
	var head struct {
		Type string `json:"type"`
	}
	if params.SessionID == "" || params.TurnID == "" || params.Sequence < 1 ||
		json.Unmarshal(params.Event, &head) != nil || !agentEventTypes[head.Type] {
		return nil, rpcErr(CodeInvalidParams, "invalid agent session.event", false)
	}
	lock := h.agentLedger.lock(params.SessionID)
	lock.Lock()
	defer lock.Unlock()

	h.agentLedger.mu.Lock()
	acked, known := h.agentLedger.acked[params.SessionID]
	h.agentLedger.mu.Unlock()
	if !known {
		var err error
		if acked, err = platform.AckedSequence(ctx, params.SessionID); err != nil {
			return nil, rpcErr(CodeInternalError, err.Error(), true)
		}
	}
	result := agentSessionEventResult{Accepted: true, EventID: params.EventID, AckedSequence: acked}
	switch {
	case params.Sequence <= acked:
		return result, nil
	case params.Sequence != acked+1:
		h.log(LogWarn, "agent session.event sequence gap in "+params.SessionID, fmt.Errorf("expected_sequence=%d", acked+1))
		return nil, &JSONRPCError{Code: CodeConflict, Message: "conflict", Data: &JSONRPCErrorData{ExpectedSequence: acked + 1}}
	}
	record := AgentEventRecord{
		SessionID: params.SessionID, TurnID: params.TurnID, EventID: params.EventID,
		Sequence: params.Sequence, Type: head.Type, Event: params.Event,
	}
	if err := platform.AgentEvent(ctx, executor, record); err != nil {
		return nil, rpcErr(CodeInternalError, err.Error(), true)
	}
	h.agentLedger.mu.Lock()
	h.agentLedger.acked[params.SessionID] = params.Sequence
	h.agentLedger.mu.Unlock()
	result.AckedSequence = params.Sequence
	return result, nil
}

func (h *Harness) handleAgentNotification(executor *Executor, method string, raw json.RawMessage) bool {
	if h.opts.Agent == nil || method != MethodSessionEventDelta {
		return false
	}
	var params SessionEventDeltaParams
	if json.Unmarshal(raw, &params) == nil && params.SessionID != "" && params.EventID != "" {
		h.opts.Agent.AgentDelta(executor, params)
	}
	return true
}

// AgentCapability returns the advertised agent capability, nil when absent.
func (e *Executor) AgentCapability() (*AgentCapability, error) {
	var capabilities struct {
		Agent *AgentCapability `json:"agent"`
	}
	if err := e.Capabilities(&capabilities); err != nil {
		return nil, err
	}
	return capabilities.Agent, nil
}

func (e *Executor) requireAgent() error {
	capability, err := e.AgentCapability()
	if err != nil {
		return err
	}
	if capability == nil || !capability.Enabled {
		return &RPCError{Code: CodeCapabilityUnavailable, Message: "executor does not advertise the agent capability"}
	}
	return nil
}

// StartTurn sends turn.start. It is safe to repeat with the same turn_id
// after a timeout or reconnect.
func (e *Executor) StartTurn(ctx context.Context, params TurnStartParams) (TurnStartResult, error) {
	var result TurnStartResult
	if err := e.requireAgent(); err != nil {
		return result, err
	}
	err := e.call(ctx, MethodTurnStart, params, &result)
	return result, err
}

// SendTurnInput sends turn.input; a conflict means the turn is over and the
// input belongs in a new turn.start.
func (e *Executor) SendTurnInput(ctx context.Context, params TurnInputParams) (Ack, error) {
	var result Ack
	err := e.call(ctx, MethodTurnInput, params, &result)
	return result, err
}

// CancelTurn sends turn.cancel. The turn's end is still reported by its
// session.status_idle event.
func (e *Executor) CancelTurn(ctx context.Context, params TurnCancelParams) (TurnCancelResult, error) {
	var result TurnCancelResult
	err := e.call(ctx, MethodTurnCancel, params, &result)
	return result, err
}

// CloseAgentSession sends session.close.
func (e *Executor) CloseAgentSession(ctx context.Context, params SessionCloseParams) (SessionCloseResult, error) {
	var result SessionCloseResult
	err := e.call(ctx, MethodSessionClose, params, &result)
	return result, err
}

// RespondInteraction delivers the platform's answer. accepted=false means
// the executor no longer waits for it (answered already, withdrawn, or
// unknown); mark the platform interaction stale.
func (e *Executor) RespondInteraction(ctx context.Context, params InteractionResponseParams) (InteractionResponseResult, error) {
	var result InteractionResponseResult
	err := e.call(ctx, MethodInteractionResponse, params, &result)
	return result, err
}
