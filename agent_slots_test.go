package bmaexecutor

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Slots: AgentCapability.MaxSessions bounds the turns running at once (the
// "active" slots). A session whose turn is over keeps its conversation but not
// a slot, so a long-lived executor serves any number of sessions one after
// another. The sessions whose local resources are held at once (resident) are
// bounded by the handler's own MaxResidentSessions: past it the least recently
// used idle session is suspended and loads again with its next turn.

// slotAgent is a handler whose turn runs until its input names it: "hold"
// turns wait for release(turnID) or cancel, any other text ends at once. With
// resident > 0 it also implements AgentSessionSuspender.
type slotAgent struct {
	mu        sync.Mutex
	resident  int
	released  map[string]chan struct{}
	started   []string
	suspended []string // in order
	closed    []string
	opened    map[string]int // OpenTurn per session
	// suspendErr is returned (once per session) by SuspendSession when set.
	suspendErr map[string]error
	// live is the set of sessions with resources (opened and not suspended).
	live map[string]bool
}

func newSlotAgent(resident int) *slotAgent {
	return &slotAgent{resident: resident, released: map[string]chan struct{}{}, opened: map[string]int{}, live: map[string]bool{}, suspendErr: map[string]error{}}
}

func (a *slotAgent) gate(turnID string) chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released[turnID] == nil {
		a.released[turnID] = make(chan struct{})
	}
	return a.released[turnID]
}

func (a *slotAgent) release(turnID string) { close(a.gate(turnID)) }

func (a *slotAgent) OpenTurn(_ context.Context, params TurnStartParams) (TurnStartResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opened[params.SessionID]++
	resumed := a.live[params.SessionID] || params.ExecutorSessionRef != ""
	a.live[params.SessionID] = true
	return TurnStartResult{Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "ref_" + params.SessionID, Resumed: resumed}, nil
}

func (a *slotAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
	a.mu.Lock()
	a.started = append(a.started, turn.TurnID())
	a.mu.Unlock()
	if strings.Contains(turn.Params().Input[0].Text, "hold") {
		select {
		case <-a.gate(turn.TurnID()):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := turn.Emit(AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: textContent("done " + turn.TurnID())})
	return err
}

func (a *slotAgent) CloseSession(_ context.Context, sessionID, _ string) error {
	a.mu.Lock()
	a.closed = append(a.closed, sessionID)
	delete(a.live, sessionID)
	a.mu.Unlock()
	return nil
}

// suspendingSlotAgent adds the optional AgentSessionSuspender.
type suspendingSlotAgent struct{ *slotAgent }

func (a suspendingSlotAgent) MaxResidentSessions() int { return a.resident }

func (a suspendingSlotAgent) SuspendSession(_ context.Context, sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.suspendErr[sessionID]; err != nil {
		delete(a.suspendErr, sessionID)
		return err
	}
	a.suspended = append(a.suspended, sessionID)
	delete(a.live, sessionID)
	return nil
}

func (a *slotAgent) suspendedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.suspended...)
}

func startSlotPair(t *testing.T, ctx context.Context, agent AgentHandler, capability AgentCapability) (*Executor, *recordingPlatform) {
	t.Helper()
	platform := newRecordingPlatform()
	harness := NewHarness(HarnessOptions{Agent: platform, RequestTimeout: 2 * time.Second})
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)
	client, err := NewClient(Options{
		EnvironmentID: "env_local", Executor: ExecutorInfo{Name: "desktop", OS: "darwin", Arch: "arm64", Version: "0.1.0"},
		TokenSource:    StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), "local"),
		RequestTimeout: 2 * time.Second, ReconnectBaseDelay: 20 * time.Millisecond, Random: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	capability.Enabled = true
	client.ServeAgent(agent, capability)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return executor, platform
}

func slotTurn(session, turn, text string) TurnStartParams {
	return TurnStartParams{SessionID: session, TurnID: turn, InputID: "in_" + turn, Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: text}}}
}

func wantBusy(t *testing.T, err error, what string) {
	t.Helper()
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeExecutorBusy {
		t.Fatalf("%s: err = %v, want executor_busy", what, err)
	}
}

// Regression: the ninth session did not start. Every session, finished or
// idle, kept one of MaxSessions slots until session.close, which the platform
// sends only when a session is archived.
func TestAgentIdleSessionsDoNotHoldSlots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := newSlotAgent(0)
	executor, platform := startSlotPair(t, ctx, agent, AgentCapability{MaxSessions: 8})
	for i := 1; i <= 24; i++ {
		session, turn := "sesn_"+strconv.Itoa(i), "turn_"+strconv.Itoa(i)
		if _, err := executor.StartTurn(ctx, slotTurn(session, turn, "hello")); err != nil {
			t.Fatalf("session %d: turn.start: %v", i, err)
		}
		assertIdle(t, platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	// The first session continues with a second turn; nothing was closed.
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_1", "turn_again", "hello")); err != nil {
		t.Fatalf("second turn of the first session: %v", err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_again"), TurnOutcomeCompleted)
	if len(agent.closed) != 0 {
		t.Fatalf("sessions closed without session.close: %v", agent.closed)
	}
}

func TestAgentRunningTurnLimitStillApplies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := newSlotAgent(0)
	executor, platform := startSlotPair(t, ctx, agent, AgentCapability{MaxSessions: 2})
	// An idle session first: it must not count, and its next turn is subject to the limit.
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_idle", "turn_idle", "hello")); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_idle"), TurnOutcomeCompleted)

	for _, n := range []string{"1", "2"} {
		if _, err := executor.StartTurn(ctx, slotTurn("sesn_"+n, "turn_"+n, "hold")); err != nil {
			t.Fatalf("turn %s: %v", n, err)
		}
	}
	_, err := executor.StartTurn(ctx, slotTurn("sesn_3", "turn_3", "hello"))
	wantBusy(t, err, "a new session past the running-turn limit")
	_, err = executor.StartTurn(ctx, slotTurn("sesn_idle", "turn_idle2", "hello"))
	wantBusy(t, err, "a new turn of an idle session past the running-turn limit")
	// A repeated turn.start of a running turn is the same turn, not a new one.
	if again, err := executor.StartTurn(ctx, slotTurn("sesn_1", "turn_1", "hold")); err != nil || !again.Accepted {
		t.Fatalf("idempotent turn.start at the limit: %+v, %v", again, err)
	}
	// A refused turn.start leaves nothing behind: the slot comes back when a turn ends.
	agent.release("turn_1")
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_3", "turn_3", "hello")); err != nil {
		t.Fatalf("turn.start after a slot was freed: %v", err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_3"), TurnOutcomeCompleted)
	agent.release("turn_2")
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

func TestAgentCanceledAndFailedTurnsFreeTheirSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := newSlotAgent(0)
	executor, platform := startSlotPair(t, ctx, agent, AgentCapability{MaxSessions: 1})
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_1", "turn_1", "hold")); err != nil {
		t.Fatal(err)
	}
	_, err := executor.StartTurn(ctx, slotTurn("sesn_2", "turn_2", "hello"))
	wantBusy(t, err, "second turn at limit 1")
	if result, err := executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_1", TurnID: "turn_1"}); err != nil || !result.Canceled {
		t.Fatalf("turn.cancel: %+v, %v", result, err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_2", "turn_2", "hello")); err != nil {
		t.Fatalf("turn.start after cancel: %v", err)
	}
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

func TestAgentSuspendsTheLeastRecentlyUsedIdleSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := newSlotAgent(3)
	agent := suspendingSlotAgent{base}
	executor, platform := startSlotPair(t, ctx, agent, AgentCapability{MaxSessions: 2})
	run := func(session, turn string) {
		t.Helper()
		if _, err := executor.StartTurn(ctx, slotTurn(session, turn, "hello")); err != nil {
			t.Fatalf("%s: %v", turn, err)
		}
		assertIdle(t, platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	run("sesn_1", "t1")
	run("sesn_2", "t2")
	run("sesn_3", "t3")
	if got := base.suspendedIDs(); len(got) != 0 {
		t.Fatalf("suspended within the resident limit: %v", got)
	}
	run("sesn_4", "t4") // 4 resident > 3: the oldest, sesn_1
	run("sesn_5", "t5") // then sesn_2
	if got := strings.Join(base.suspendedIDs(), ","); got != "sesn_1,sesn_2" {
		t.Fatalf("suspended = %s, want sesn_1,sesn_2 (least recently used first)", got)
	}
	// A turn of a suspended session resumes it (OpenTurn again) and suspends
	// the least recently used of the others; its events keep their numbering.
	run("sesn_1", "t6")
	if got := strings.Join(base.suspendedIDs(), ","); got != "sesn_1,sesn_2,sesn_3" {
		t.Fatalf("suspended = %s, want sesn_3 after sesn_1 came back", got)
	}
	if base.opened["sesn_1"] != 2 {
		t.Fatalf("OpenTurn of sesn_1 = %d, want 2 (it came back from suspension)", base.opened["sesn_1"])
	}
	assertSequences(t, eventsForSession(platform.snapshot(), "sesn_1"))
	// Using a session refreshes its place: sesn_4 was used before sesn_5, so
	// after touching sesn_4 the next victim is sesn_5.
	run("sesn_4", "t7")
	run("sesn_2", "t8")
	got := base.suspendedIDs()
	if last := got[len(got)-1]; last != "sesn_5" {
		t.Fatalf("suspended = %v, want sesn_5 last", got)
	}
}

func eventsForSession(events []AgentEventRecord, sessionID string) []AgentEventRecord {
	var out []AgentEventRecord
	for _, e := range events {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out
}

func TestAgentNeverSuspendsARunningSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := newSlotAgent(2)
	executor, platform := startSlotPair(t, ctx, suspendingSlotAgent{base}, AgentCapability{MaxSessions: 4})
	if _, err := executor.StartTurn(ctx, slotTurn("sesn_run", "t_run", "hold")); err != nil { // the oldest, but running
		t.Fatal(err)
	}
	for i, id := range []string{"sesn_a", "sesn_b", "sesn_c"} {
		turn := "t_" + strconv.Itoa(i)
		if _, err := executor.StartTurn(ctx, slotTurn(id, turn, "hello")); err != nil {
			t.Fatal(err)
		}
		assertIdle(t, platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	for _, id := range base.suspendedIDs() {
		if id == "sesn_run" {
			t.Fatalf("a session with a running turn was suspended: %v", base.suspendedIDs())
		}
	}
	if got := strings.Join(base.suspendedIDs(), ","); got != "sesn_a,sesn_b" {
		t.Fatalf("suspended = %s, want sesn_a,sesn_b", got)
	}
	base.release("t_run")
	assertIdle(t, platform.waitIdle(t, "t_run"), TurnOutcomeCompleted)
}

func TestAgentFailedSuspendKeepsTheSessionAndTriesTheNext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := newSlotAgent(2)
	base.suspendErr["sesn_1"] = errors.New("agent refused")
	executor, platform := startSlotPair(t, ctx, suspendingSlotAgent{base}, AgentCapability{MaxSessions: 4})
	for i, id := range []string{"sesn_1", "sesn_2", "sesn_3"} {
		turn := "t_" + strconv.Itoa(i)
		if _, err := executor.StartTurn(ctx, slotTurn(id, turn, "hello")); err != nil {
			t.Fatalf("turn.start %s: %v", id, err)
		}
		assertIdle(t, platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	if got := strings.Join(base.suspendedIDs(), ","); got != "sesn_2" {
		t.Fatalf("suspended = %s, want sesn_2 (sesn_1 refused)", got)
	}
}

func TestAgentSessionCloseAfterSuspend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := newSlotAgent(1)
	executor, platform := startSlotPair(t, ctx, suspendingSlotAgent{base}, AgentCapability{MaxSessions: 2})
	for _, n := range []string{"1", "2"} {
		if _, err := executor.StartTurn(ctx, slotTurn("sesn_"+n, "t"+n, "hello")); err != nil {
			t.Fatal(err)
		}
		assertIdle(t, platform.waitIdle(t, "t"+n), TurnOutcomeCompleted)
	}
	if got := strings.Join(base.suspendedIDs(), ","); got != "sesn_1" {
		t.Fatalf("suspended = %s", got)
	}
	result, err := executor.CloseAgentSession(ctx, SessionCloseParams{SessionID: "sesn_1", Reason: "archived"})
	if err != nil || !result.Closed {
		t.Fatalf("session.close of a suspended session: %+v, %v", result, err)
	}
	if strings.Join(base.closed, ",") != "sesn_1" {
		t.Fatalf("CloseSession calls = %v, want sesn_1", base.closed)
	}
}
