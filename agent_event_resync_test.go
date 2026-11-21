package bmaexecutor

// session.event failures must never leave a hole in a session's sequence: a
// platform failure is resent, a gap answer renumbers from expected_sequence,
// and a permanent rejection ends the turn visibly at the rejected sequence
// (11-12: one rejected event left a session stuck on "sequence gap" for good).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type resyncStored struct {
	sequence int64
	kind     string
	event    map[string]any
}

// resyncPlatform is the platform side of session.event, scripted per call.
type resyncPlatform struct {
	mu     sync.Mutex
	stored []resyncStored
	sent   []int64
	acked  int64
	calls  int
	// failures answers the n-th session.event (0-based) with an error instead of storing it.
	failures map[int]JSONRPCError
	// refuse lists event types refused for good, every time.
	refuse  map[string]bool
	changed chan struct{}
}

func newResyncPlatform() *resyncPlatform {
	return &resyncPlatform{failures: map[int]JSONRPCError{}, refuse: map[string]bool{}, changed: make(chan struct{}, 64)}
}

func (p *resyncPlatform) answer(params agentSessionEventParams) (any, *JSONRPCError) {
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		select {
		case p.changed <- struct{}{}:
		default:
		}
	}()
	var event map[string]any
	_ = json.Unmarshal(params.Event, &event)
	kind, _ := event["type"].(string)
	p.sent = append(p.sent, params.Sequence)
	call := p.calls
	p.calls++
	if failure, ok := p.failures[call]; ok {
		return nil, &failure
	}
	if p.refuse[kind] {
		return nil, &JSONRPCError{Code: CodeInvalidParams, Message: "bad event"}
	}
	if params.Sequence <= p.acked {
		return agentSessionEventResult{Accepted: true, EventID: params.EventID, AckedSequence: p.acked}, nil
	}
	if params.Sequence != p.acked+1 {
		return nil, &JSONRPCError{Code: CodeConflict, Message: "conflict", Data: &JSONRPCErrorData{ExpectedSequence: p.acked + 1}}
	}
	p.acked = params.Sequence
	p.stored = append(p.stored, resyncStored{sequence: params.Sequence, kind: kind, event: event})
	return agentSessionEventResult{Accepted: true, EventID: params.EventID, AckedSequence: p.acked}, nil
}

func (p *resyncPlatform) waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		p.mu.Lock()
		ok := done()
		p.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-p.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			p.mu.Lock()
			defer p.mu.Unlock()
			t.Fatalf("timed out waiting for %s; stored=%v sent=%v", what, p.kinds(), p.sent)
		}
	}
}

func (p *resyncPlatform) kinds() []string {
	out := make([]string, len(p.stored))
	for i, s := range p.stored {
		out[i] = s.kind
	}
	return out
}

func (p *resyncPlatform) sequences() []int64 {
	out := make([]int64, len(p.stored))
	for i, s := range p.stored {
		out[i] = s.sequence
	}
	return out
}

func (p *resyncPlatform) hasIdle() bool {
	for _, s := range p.stored {
		if s.kind == AgentEventTypeSessionStatusIdle {
			return true
		}
	}
	return false
}

// resyncAgent emits three events, then waits for release or the turn's end
// and records context.Cause of the turn.
type resyncAgent struct {
	release chan struct{}
	causes  chan error
}

func (a *resyncAgent) OpenTurn(_ context.Context, params TurnStartParams) (TurnStartResult, error) {
	return TurnStartResult{Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "ref"}, nil
}

func (a *resyncAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
	_, _ = turn.Emit(AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent("one")})
	_, _ = turn.Emit(AgentToolUseEvent{Type: AgentEventTypeAgentToolUse, Name: "bash", Input: map[string]any{}})
	_, _ = turn.Emit(AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent("two")})
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	a.causes <- context.Cause(ctx)
	return nil
}

func (a *resyncAgent) CloseSession(context.Context, string, string) error { return nil }

// runResync serves one control channel: it registers the executor, starts one
// turn and answers every session.event from platform.
func runResync(t *testing.T, platform *resyncPlatform, release chan struct{}) *resyncAgent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		session := &testSession{ws: ws, log: newFrameLog()}
		go session.readLoop(ctx)
		if registerSession(ctx, session, 60000) != nil {
			return
		}
		_ = session.write(rpcRequestEnvelope{JSONRPC: "2.0", ID: "start_1", Method: MethodTurnStart, Params: TurnStartParams{
			SessionID: "ses_1", TurnID: "t_1", InputID: "in_1", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "go"}},
		}})
		for {
			frame, err := session.awaitRequest(ctx, MethodSessionEvent)
			if err != nil {
				return
			}
			var params agentSessionEventParams
			_ = json.Unmarshal(frame.Params, &params)
			result, rpcError := platform.answer(params)
			if rpcError != nil {
				_ = session.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: *frame.ID, Error: *rpcError})
			} else {
				_ = session.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *frame.ID, Result: result})
			}
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Options{
		TokenSource:    StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), "jwt"),
		Executor:       ExecutorInfo{Name: "desk", OS: "darwin", Arch: "arm64", Version: "0.1.0"},
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := &resyncAgent{release: release, causes: make(chan error, 1)}
	client.ServeAgent(agent, AgentCapability{Enabled: true, MaxSessions: 1})
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	return agent
}

func TestAgentEventPlatformFailureResentInPlace(t *testing.T) {
	platform := newResyncPlatform()
	// -32603 not marked retryable: the platform's own failure, the event is idempotent.
	platform.failures[2] = JSONRPCError{Code: CodeInternalError, Message: "internal error"}
	release := make(chan struct{})
	close(release)
	runResync(t, platform, release)
	platform.waitFor(t, "idle", platform.hasIdle)
	platform.mu.Lock()
	defer platform.mu.Unlock()
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentMessage, AgentEventTypeAgentToolUse, AgentEventTypeAgentMessage, AgentEventTypeSessionStatusIdle}
	if got := platform.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stored = %v, want %v", got, want)
	}
	if got := platform.sequences(); !equalInt64s(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("sequences = %v", got)
	}
}

func TestAgentEventGapRenumbersFromExpectedSequence(t *testing.T) {
	platform := newResyncPlatform()
	release := make(chan struct{})
	runResync(t, platform, release)
	platform.waitFor(t, "four events", func() bool { return len(platform.stored) == 4 })
	// The platform starts a new epoch for the session: it expects 1 again.
	platform.mu.Lock()
	platform.acked = 0
	platform.stored = nil
	platform.mu.Unlock()
	close(release)
	platform.waitFor(t, "idle", platform.hasIdle)
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if got := platform.sequences(); !equalInt64s(got, []int64{1}) {
		t.Fatalf("sequences = %v, want the next event renumbered to 1", got)
	}
	if got := platform.sent[len(platform.sent)-2]; got != 5 {
		t.Fatalf("first send = %d, want 5 refused as a gap (sent %v)", got, platform.sent)
	}
}

func TestAgentEventPermanentRejectionEndsTurn(t *testing.T) {
	platform := newResyncPlatform()
	platform.refuse[AgentEventTypeAgentToolUse] = true
	release := make(chan struct{})
	agent := runResync(t, platform, release)
	platform.waitFor(t, "idle", platform.hasIdle)
	platform.mu.Lock()
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentMessage, AgentEventTypeSessionError, AgentEventTypeSessionStatusIdle}
	if got := platform.kinds(); strings.Join(got, ",") != strings.Join(want, ",") {
		platform.mu.Unlock()
		t.Fatalf("stored = %v, want %v", got, want)
	}
	if got := platform.sequences(); !equalInt64s(got, []int64{1, 2, 3, 4}) {
		platform.mu.Unlock()
		t.Fatalf("sequences = %v, want the terminal events on the rejected sequence", got)
	}
	errorInfo, _ := platform.stored[2].event["error"].(map[string]any)
	idle := platform.stored[3].event
	platform.mu.Unlock()
	if errorInfo["type"] != AgentErrorCodeAgentError || !strings.HasPrefix(errorInfo["message"].(string), "event_rejected: ") {
		t.Fatalf("session.error = %v", errorInfo)
	}
	if stop, _ := idle["stop_reason"].(map[string]any); stop["type"] != StopReasonTypeRetriesExhausted || idle["turn_outcome"] != TurnOutcomeFailed {
		t.Fatalf("status_idle = %v", idle)
	}
	select {
	case cause := <-agent.causes:
		var rejected *EventRejectedError
		if !errors.As(cause, &rejected) {
			t.Fatalf("turn cause = %v, want *EventRejectedError", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn was not stopped")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	platform.mu.Lock()
	defer platform.mu.Unlock()
	if len(platform.stored) != 4 {
		t.Fatalf("stored = %v, want nothing of the ended turn after it", platform.kinds())
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
