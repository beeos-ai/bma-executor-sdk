package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func callsOf(t *testing.T, f *acpFixture, verb string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.agent.opts.StateDir, "fake-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if v, id, ok := strings.Cut(line, " "); ok && v == verb {
			out = append(out, id)
		}
	}
	return out
}

func messageTexts(t *testing.T, events []AgentEventRecord, turnID string) string {
	t.Helper()
	var out []string
	for _, e := range eventsOf(events, turnID) {
		m := eventJSON(t, e)
		if m["type"] != AgentEventTypeAgentMessage {
			continue
		}
		for _, b := range m["content"].([]any) {
			out = append(out, b.(map[string]any)["text"].(string))
		}
	}
	return strings.Join(out, "")
}

// Regression for the ninth session that never started: ten and more sessions
// run one after another through one ACPAgent and bma-dsh-acp process. Idle
// sessions hold no slot, and past MaxResidentSessions the least recently used
// is closed in the agent (session/close) and loaded again by its next turn
// with the whole conversation.
func TestACPAgentManySessionsOneAfterAnother(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{MaxSessions: 2, MaxResidentSessions: 3, Env: []string{"BMA_ACP_FAKE_STRICT=1"}})
	if got := f.agent.Capability().MaxSessions; got != 2 {
		t.Fatalf("capability max_sessions = %d", got)
	}
	for i := 1; i <= 12; i++ {
		turn := fmt.Sprintf("turn_%d", i)
		f.start(t, ctx, fmt.Sprintf("sesn_%d", i), turn, "hello")
		assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	closed := callsOf(t, f, "close")
	want := []string{}
	for i := 1; i <= 9; i++ { // 12 sessions, 3 resident
		want = append(want, fmt.Sprintf("sesn_%d", i))
	}
	if strings.Join(closed, ",") != strings.Join(want, ",") {
		t.Fatalf("session/close calls = %v, want %v (least recently used first)", closed, want)
	}
	if n := len(f.agent.opts.Core.sessions); n > 3 {
		t.Fatalf("Core holds %d sessions, want at most the 3 resident", n)
	}

	// The oldest session comes back with its transcript: loaded, not created.
	result := f.start(t, ctx, "sesn_1", "turn_back", "history", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_1" })
	if !result.Resumed {
		t.Fatalf("turn.start of a suspended session: %+v, want resumed", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_back"), TurnOutcomeCompleted)
	if got := messageTexts(t, f.platform.snapshot(), "turn_back"); got != "hello" {
		t.Fatalf("transcript after suspension = %q, want the earlier prompt", got)
	}
	if loads := callsOf(t, f, "load"); len(loads) != 1 || loads[0] != "sesn_1" {
		t.Fatalf("session/load calls = %v, want only sesn_1", loads)
	}
	// And it works without the platform's ref: the agent remembers the id it suspended.
	f.start(t, ctx, "sesn_2", "turn_back2", "history")
	assertIdle(t, f.platform.waitIdle(t, "turn_back2"), TurnOutcomeCompleted)
	if got := messageTexts(t, f.platform.snapshot(), "turn_back2"); got != "hello" {
		t.Fatalf("transcript of sesn_2 = %q", got)
	}
	if loads := callsOf(t, f, "load"); strings.Join(loads, ",") != "sesn_1,sesn_2" {
		t.Fatalf("session/load calls = %v, want sesn_1,sesn_2 (loaded, not created again)", loads)
	}
	// Archiving a suspended session still cleans up.
	if _, err := f.executor.CloseAgentSession(ctx, SessionCloseParams{SessionID: "sesn_3", Reason: "archived"}); err != nil {
		t.Fatalf("session.close of a suspended session: %v", err)
	}
	if _, err := os.Stat(ledgerPath(f.agent.opts.StateDir, "sesn_3")); !os.IsNotExist(err) {
		t.Fatalf("the ledger of an archived session stays: %v", err)
	}
}

func TestACPAgentRunningTurnLimitWithManySessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{MaxSessions: 2, MaxResidentSessions: 4})
	for i := 1; i <= 6; i++ {
		turn := fmt.Sprintf("turn_%d", i)
		f.start(t, ctx, fmt.Sprintf("sesn_%d", i), turn, "hello")
		assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	f.start(t, ctx, "sesn_a", "turn_a", "slow")
	f.start(t, ctx, "sesn_b", "turn_b", "slow")
	_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_c", TurnID: "turn_c", InputID: "in_c",
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}, Config: &AgentTurnConfig{System: "Be brief."}})
	wantBusy(t, err, "a third running turn at MaxSessions 2")
	assertIdle(t, f.platform.waitIdle(t, "turn_a"), TurnOutcomeCompleted)
	f.start(t, ctx, "sesn_c", "turn_c", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_c"), TurnOutcomeCompleted)
	assertIdle(t, f.platform.waitIdle(t, "turn_b"), TurnOutcomeCompleted)
}

func TestACPAgentResidentLimitDefaultsFromTheCore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	if got := f.agent.MaxResidentSessions(); got != DefaultCoreMaxSessions*DefaultACPResidentSessions/100 {
		t.Fatalf("default resident limit = %d", got)
	}
}

// TestACPAgentManySessionsWithBmaDshAcp is TestACPAgentManySessionsOneAfterAnother
// against the real bma-dsh-acp (BMA_DSH_ACP_BIN, as in TestACPAgentWithBmaDshAcp):
// twelve sessions through one process with three resident; a session that was
// closed in the agent comes back with its earlier user messages in the model's
// context (DSH's session log survives session/close).
func TestACPAgentManySessionsWithBmaDshAcp(t *testing.T) {
	bin := os.Getenv("BMA_DSH_ACP_BIN")
	if bin == "" {
		t.Skip("BMA_DSH_ACP_BIN is not set")
	}
	command := []string{bin}
	if strings.HasSuffix(bin, ".mjs") {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not found for the .mjs bundle")
		}
		command = []string{node, bin}
	}
	var mu sync.Mutex
	var lastUsers []string // the user messages of the latest model request
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var users []string
		for _, m := range body.Messages {
			if m.Role == "user" {
				users = append(users, fmt.Sprint(m.Content))
			}
		}
		mu.Lock()
		lastUsers = users
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": nil}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		b, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	}))
	defer model.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, MaxSessions: 2, MaxResidentSessions: 3,
		Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "scripted"}})
	for i := 1; i <= 12; i++ {
		turn := fmt.Sprintf("turn_%d", i)
		f.start(t, ctx, fmt.Sprintf("sesn_%d", i), turn, fmt.Sprintf("question %d", i))
		assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeCompleted)
	}
	if n := len(f.agent.opts.Core.sessions); n > 3 {
		t.Fatalf("Core holds %d sessions, want at most 3", n)
	}
	result := f.start(t, ctx, "sesn_1", "turn_back", "and again", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_1" })
	if !result.Resumed {
		t.Fatalf("turn.start of a suspended session: %+v", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_back"), TurnOutcomeCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(lastUsers) < 2 || !strings.Contains(lastUsers[0], "question 1") || !strings.Contains(lastUsers[len(lastUsers)-1], "and again") {
		t.Fatalf("model context after the session came back = %q, want the earlier question first", lastUsers)
	}
}
