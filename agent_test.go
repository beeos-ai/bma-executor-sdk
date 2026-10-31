package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recordingPlatform is the platform side of the test: it stores every durable
// agent event in arrival order and answers interactions from a channel.
type recordingPlatform struct {
	mu           sync.Mutex
	events       []AgentEventRecord
	deltas       []SessionEventDeltaParams
	interactions []InteractionRequestParams
	canceled     []InteractionCancelParams
	persisted    map[string]int64
	changed      chan struct{}
	// afterPersist runs after an event is durable and before it is acked.
	afterPersist func(AgentEventRecord)
}

func newRecordingPlatform() *recordingPlatform {
	return &recordingPlatform{persisted: map[string]int64{}, changed: make(chan struct{}, 64)}
}

func (p *recordingPlatform) notify() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *recordingPlatform) AckedSequence(_ context.Context, sessionID string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persisted[sessionID], nil
}

func (p *recordingPlatform) AgentEvent(_ context.Context, _ *Executor, event AgentEventRecord) error {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.persisted[event.SessionID] = event.Sequence
	after := p.afterPersist
	p.mu.Unlock()
	if after != nil {
		after(event)
	}
	p.notify()
	return nil
}

func (p *recordingPlatform) AgentDelta(_ *Executor, delta SessionEventDeltaParams) {
	p.mu.Lock()
	p.deltas = append(p.deltas, delta)
	p.mu.Unlock()
}

func (p *recordingPlatform) InteractionRequested(_ context.Context, _ *Executor, request InteractionRequestParams) (string, error) {
	p.mu.Lock()
	p.interactions = append(p.interactions, request)
	p.mu.Unlock()
	p.notify()
	return "aint_" + request.InteractionID, nil
}

func (p *recordingPlatform) InteractionCanceled(_ context.Context, _ *Executor, cancel InteractionCancelParams) error {
	p.mu.Lock()
	p.canceled = append(p.canceled, cancel)
	p.mu.Unlock()
	p.notify()
	return nil
}

func (p *recordingPlatform) snapshot() []AgentEventRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AgentEventRecord(nil), p.events...)
}

func (p *recordingPlatform) waitFor(t *testing.T, what string, done func() bool) {
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
			t.Fatalf("timed out waiting for %s; events=%v", what, eventTypes(p.snapshot()))
		}
	}
}

func (p *recordingPlatform) waitIdle(t *testing.T, turnID string) AgentEventRecord {
	t.Helper()
	var idle AgentEventRecord
	p.waitFor(t, "idle of "+turnID, func() bool {
		for _, event := range p.events {
			if event.TurnID == turnID && event.Type == AgentEventTypeSessionStatusIdle {
				idle = event
				return true
			}
		}
		return false
	})
	return idle
}

func eventTypes(events []AgentEventRecord) []string {
	out := make([]string, len(events))
	for i, event := range events {
		out[i] = event.Type
	}
	return out
}

// scriptedAgent is a deterministic local agent: it answers each turn with a
// streamed message, asks permission for one bash call, runs it, and waits on
// "hold" turns until canceled.
type scriptedAgent struct {
	mu      sync.Mutex
	closed  []string
	holding chan struct{}
}

func (a *scriptedAgent) OpenTurn(_ context.Context, params TurnStartParams) (TurnStartResult, error) {
	effective := AgentTurnConfig{Model: "local-model", ThinkingLevel: "medium", PermissionMode: "default"}
	if params.Config != nil && params.Config.Model != "" {
		effective.Model = params.Config.Model
	}
	ref := params.ExecutorSessionRef
	return TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "local_" + params.SessionID,
		Resumed: ref != "", Effective: effective,
	}, nil
}

func (a *scriptedAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
	text := turn.Params().Input[0].Text
	if strings.Contains(text, "hold") {
		close(a.holding)
		<-ctx.Done()
		return ctx.Err()
	}
	messageID := turn.NewEventID()
	turn.Delta(messageID, EventDeltaTypeAgentMessage, 0, "Working on: ")
	turn.Delta(messageID, EventDeltaTypeAgentMessage, 0, text)
	if _, err := turn.EmitWithID(messageID, AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent("Working on: " + text)}); err != nil {
		return err
	}
	if strings.Contains(text, "run") {
		toolUseID, err := turn.Emit(AgentToolUseEvent{
			Type: AgentEventTypeAgentToolUse, Name: "bash", Input: map[string]any{"command": "go test ./..."},
			EvaluatedPermission: &EvaluatedPermission{Type: "ask"},
		})
		if err != nil {
			return err
		}
		answer, err := turn.RequestInteraction(ctx, InteractionRequestParams{
			Kind: InteractionKindPermission, ToolUseID: toolUseID, ToolName: "bash",
			ToolInput: map[string]any{"command": "go test ./..."}, Scopes: []string{InteractionScopeOnce, InteractionScopeSession},
		})
		if err != nil {
			return err
		}
		result := AgentToolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: toolUseID}
		if answer.Outcome == InteractionOutcomeAllow {
			result.Content = textContent("ok")
		} else {
			result.Content, result.IsError = textContent("denied"), true
		}
		if _, err := turn.Emit(result); err != nil {
			return err
		}
	}
	for {
		select {
		case input := <-turn.Inputs():
			if _, err := turn.Emit(AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent("also: " + input.Input[0].Text)}); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (a *scriptedAgent) CloseSession(_ context.Context, sessionID, _ string) error {
	a.mu.Lock()
	a.closed = append(a.closed, sessionID)
	a.mu.Unlock()
	return nil
}

func textContent(text string) []ContentBlock {
	return []ContentBlock{{Type: ContentBlockTypeText, Text: text}}
}

func startAgentPair(t *testing.T, ctx context.Context, agent AgentHandler) (*Harness, *Executor, *Client, *recordingPlatform) {
	t.Helper()
	platform := newRecordingPlatform()
	harness := NewHarness(HarnessOptions{Agent: platform, RequestTimeout: 2 * time.Second})
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, err := NewClient(Options{
		EnvironmentID:  "env_local",
		Executor:       ExecutorInfo{Name: "desktop", OS: "darwin", Arch: "arm64", Version: "0.1.0"},
		TokenSource:    StaticToken(wsURL, "local"),
		RequestTimeout: 2 * time.Second, ReconnectBaseDelay: 20 * time.Millisecond, Random: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ServeAgent(agent, AgentCapability{
		Enabled: true, Models: []AgentModel{{ID: "local-model", ThinkingLevels: []string{"low", "medium", "high"}}},
		PermissionModes: []string{"default", "accept_edits"}, InteractionKinds: []string{InteractionKindPermission, InteractionKindQuestion},
	})
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return harness, executor, client, platform
}

func TestAgentTurnLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	agent := &scriptedAgent{holding: make(chan struct{})}
	_, executor, client, platform := startAgentPair(t, ctx, agent)

	capability, err := executor.AgentCapability()
	if err != nil || capability == nil || !capability.Enabled || capability.Models[0].ID != "local-model" {
		t.Fatalf("agent capability = %+v, %v", capability, err)
	}

	// 1. Task: stream, approval, result.
	start := TurnStartParams{
		SessionID: "sesn_1", TurnID: "turn_1", InputID: "ain_1",
		Input:  []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "run the tests"}},
		Config: &AgentTurnConfig{Model: "local-model", Cwd: "/tmp"},
	}
	started, err := executor.StartTurn(ctx, start)
	if err != nil {
		t.Fatalf("turn.start: %v", err)
	}
	if !started.Accepted || started.Effective.Model != "local-model" || started.Resumed || started.ExecutorSessionRef != "local_sesn_1" {
		t.Fatalf("turn.start result = %+v", started)
	}
	again, err := executor.StartTurn(ctx, start)
	if err != nil || again.ExecutorSessionRef != started.ExecutorSessionRef {
		t.Fatalf("repeated turn.start must be idempotent: %+v %v", again, err)
	}
	platform.waitFor(t, "permission request", func() bool { return len(platform.interactions) == 1 })
	request := platform.interactions[0]
	if request.SessionID != "sesn_1" || request.TurnID != "turn_1" || request.Kind != InteractionKindPermission || request.ToolUseID == "" {
		t.Fatalf("interaction.request = %+v", request)
	}
	// The tool_use event is durable before the interaction references it.
	if !hasEvent(platform.snapshot(), request.ToolUseID) {
		t.Fatal("interaction.request arrived before its agent.tool_use was acknowledged")
	}
	answer := InteractionResponseParams{SessionID: "sesn_1", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow, Scope: InteractionScopeOnce}
	if result, err := executor.RespondInteraction(ctx, answer); err != nil || !result.Accepted {
		t.Fatalf("interaction.response = %+v, %v", result, err)
	}
	if result, err := executor.RespondInteraction(ctx, answer); err != nil || result.Accepted {
		t.Fatalf("duplicate interaction.response must answer accepted=false: %+v, %v", result, err)
	}
	idle := platform.waitIdle(t, "turn_1")
	assertIdle(t, idle, TurnOutcomeCompleted)
	got := eventTypes(platform.snapshot())
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentMessage, AgentEventTypeAgentToolUse, AgentEventTypeAgentToolResult, AgentEventTypeSessionStatusIdle}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	assertSequences(t, platform.snapshot())
	platform.mu.Lock()
	deltas := len(platform.deltas)
	platform.mu.Unlock()
	if deltas != 2 {
		t.Fatalf("deltas = %d, want 2", deltas)
	}

	// 2. Follow-up turn on the same session resumes the local conversation.
	followUp, err := executor.StartTurn(ctx, TurnStartParams{
		SessionID: "sesn_1", TurnID: "turn_2", InputID: "ain_2", ExecutorSessionRef: started.ExecutorSessionRef,
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "summarize"}},
	})
	if err != nil || !followUp.Resumed {
		t.Fatalf("follow-up turn.start = %+v, %v", followUp, err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)

	// 3. Cancel a running turn; the session stays open.
	if _, err := executor.StartTurn(ctx, TurnStartParams{
		SessionID: "sesn_1", TurnID: "turn_3", InputID: "ain_3", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hold"}},
	}); err != nil {
		t.Fatal(err)
	}
	<-agent.holding
	var conflict *RPCError
	_, err = executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_1", TurnID: "turn_x", InputID: "ain_x", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "x"}}})
	if !errors.As(err, &conflict) || conflict.Code != CodeConflict {
		t.Fatalf("turn.start during a running turn must conflict, got %v", err)
	}
	if canceled, err := executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_1", TurnID: "turn_3", Reason: "user_interrupt"}); err != nil || !canceled.Canceled {
		t.Fatalf("turn.cancel = %+v, %v", canceled, err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_3"), TurnOutcomeCanceled)

	// 4. Close the session.
	closed, err := executor.CloseAgentSession(ctx, SessionCloseParams{SessionID: "sesn_1", Reason: "archived"})
	if err != nil || !closed.Closed {
		t.Fatalf("session.close = %+v, %v", closed, err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.closed) != 1 || agent.closed[0] != "sesn_1" {
		t.Fatalf("CloseSession calls = %v", agent.closed)
	}
	if contains(client.ActiveSessions(), "sesn_1") {
		t.Fatal("closed agent session is still reported as active")
	}
}

// blockingAgent emits events only when told to, so the test can cut the
// connection between them.
type blockingAgent struct {
	step chan string
}

func (a *blockingAgent) OpenTurn(_ context.Context, params TurnStartParams) (TurnStartResult, error) {
	return TurnStartResult{Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "ref", Effective: AgentTurnConfig{}}, nil
}

func (a *blockingAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
	for text := range a.step {
		if text == "" {
			return nil
		}
		if _, err := turn.Emit(AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent(text)}); err != nil {
			return err
		}
	}
	return nil
}

func (a *blockingAgent) CloseSession(context.Context, string, string) error { return nil }

func TestAgentEventsResentAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	agent := &blockingAgent{step: make(chan string)}
	_, executor, client, platform := startAgentPair(t, ctx, agent)
	if _, err := executor.StartTurn(ctx, TurnStartParams{
		SessionID: "sesn_r", TurnID: "turn_r", InputID: "ain_r", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "go"}},
	}); err != nil {
		t.Fatal(err)
	}
	agent.step <- "one"
	platform.waitFor(t, "first message", func() bool { return len(platform.events) == 2 })

	drop := func() {
		executor.mu.Lock()
		conn := executor.conn
		executor.mu.Unlock()
		if conn != nil {
			_ = conn.ws.Close(int(websocket.StatusGoingAway), "network drop")
		}
	}
	// Cut the connection, keep producing events while disconnected.
	drop()
	agent.step <- "two"
	// Lose the ACK of "three": it is persisted, the connection drops before
	// the response, and the executor resends it after reconnect.
	var once sync.Once
	platform.mu.Lock()
	platform.afterPersist = func(event AgentEventRecord) {
		if strings.Contains(string(event.Event), "three") {
			once.Do(drop)
		}
	}
	platform.mu.Unlock()
	agent.step <- "three"
	agent.step <- ""

	idle := platform.waitIdle(t, "turn_r")
	assertIdle(t, idle, TurnOutcomeCompleted)
	events := platform.snapshot()
	assertSequences(t, events)
	var texts []string
	for _, event := range events {
		if event.Type == AgentEventTypeAgentMessage {
			var message AgentMessageEvent
			if err := json.Unmarshal(event.Event, &message); err != nil {
				t.Fatal(err)
			}
			texts = append(texts, message.Content[0].Text)
		}
	}
	if strings.Join(texts, ",") != "one,two,three" {
		t.Fatalf("messages after reconnect = %v", texts)
	}
	if client.State() != ConnectionStateReady {
		t.Fatalf("client state = %s", client.State())
	}
}

func TestHarnessAgentSequenceRules(t *testing.T) {
	platform := newRecordingPlatform()
	harness := NewHarness(HarnessOptions{Agent: platform})
	executor := &Executor{h: harness, id: "exe_seq"}
	send := func(seq int64, eventID string) (any, *JSONRPCError) {
		raw, _ := json.Marshal(map[string]any{
			"session_id": "sesn_s", "turn_id": "turn_s", "event_id": eventID, "sequence": seq,
			"event": map[string]any{"type": "agent.message", "content": []any{map[string]any{"type": "text", "text": "x"}}},
		})
		result, rpcError, handled := harness.dispatchAgent(context.Background(), executor, MethodSessionEvent, raw)
		if !handled {
			t.Fatal("agent session.event was not handled")
		}
		return result, rpcError
	}
	if _, rpcError := send(1, "evt_1"); rpcError != nil {
		t.Fatalf("seq 1: %+v", rpcError)
	}
	result, rpcError := send(1, "evt_1")
	if rpcError != nil || result.(agentSessionEventResult).AckedSequence != 1 {
		t.Fatalf("duplicate seq 1: %+v %+v", result, rpcError)
	}
	if _, rpcError := send(3, "evt_3"); rpcError == nil || rpcError.Code != CodeConflict {
		t.Fatalf("gap must conflict, got %+v", rpcError)
	}
	if _, rpcError := send(2, "evt_2"); rpcError != nil {
		t.Fatalf("seq 2: %+v", rpcError)
	}
	if got := len(platform.snapshot()); got != 2 {
		t.Fatalf("persisted %d events, want 2", got)
	}
	// A malformed agent event never reaches the platform.
	raw, _ := json.Marshal(map[string]any{"session_id": "sesn_s", "turn_id": "turn_s", "event_id": "evt_bad", "sequence": 3, "event": map[string]any{"type": "user.message"}})
	if _, rpcError, _ := harness.dispatchAgent(context.Background(), executor, MethodSessionEvent, raw); rpcError == nil || rpcError.Code != CodeInvalidParams {
		t.Fatalf("user.message from an executor must be rejected, got %+v", rpcError)
	}
}

func hasEvent(events []AgentEventRecord, eventID string) bool {
	for _, event := range events {
		if event.EventID == eventID {
			return true
		}
	}
	return false
}

func assertIdle(t *testing.T, record AgentEventRecord, outcome string) {
	t.Helper()
	var idle SessionStatusIdleEvent
	if err := json.Unmarshal(record.Event, &idle); err != nil {
		t.Fatal(err)
	}
	if idle.TurnOutcome != outcome || idle.StopReason.Type != StopReasonTypeEndTurn {
		t.Fatalf("idle = %+v, want outcome %s", idle, outcome)
	}
}

func assertSequences(t *testing.T, events []AgentEventRecord) {
	t.Helper()
	next := map[string]int64{}
	for _, event := range events {
		next[event.SessionID]++
		if event.Sequence != next[event.SessionID] {
			t.Fatalf("event %s has sequence %d, want %d", event.EventID, event.Sequence, next[event.SessionID])
		}
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
