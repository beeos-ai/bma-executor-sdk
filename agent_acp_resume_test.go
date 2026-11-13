package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fake brain: a durable transcript, like bma-dsh-acp's session log ---

type fakeTranscript struct {
	Inputs  []string          `json:"inputs"`
	Results map[string]string `json:"results"`
}

func fakeTranscriptPath(sid string) string {
	return filepath.Join(os.Getenv("BMA_DSH_ACP_HOME"), "fake-"+sid+".json")
}

func (tr *fakeTranscript) load(sid string) {
	tr.Results = map[string]string{}
	if raw, err := os.ReadFile(fakeTranscriptPath(sid)); err == nil {
		_ = json.Unmarshal(raw, tr)
	}
}

func (tr *fakeTranscript) save(sid string) {
	raw, _ := json.Marshal(tr)
	_ = os.WriteFile(fakeTranscriptPath(sid), raw, 0o600)
}

// fakeResume is one turn of a brain that survives restarts: it records its
// inputs and tool results, and when the same input comes again it continues:
// the tool call whose result it does not hold is issued again (same call id).
// amnesia makes it forget tool results, so it asks for completed calls again.
func fakeResume(c *fakeConn, sid, inputID string, allow, amnesia bool) (any, map[string]any) {
	endTurn := map[string]any{"stopReason": "end_turn"}
	var tr fakeTranscript
	tr.load(sid)
	tr.Inputs = append(tr.Inputs, inputID)
	tr.save(sid)
	const callID = "call_r1"
	if _, known := tr.Results[callID]; !known {
		input := map[string]any{"command": "echo x >> count.txt && cat count.txt"}
		permission := "ask"
		if allow {
			permission = "allow"
		}
		c.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": callID, "title": "bash", "kind": "execute", "status": "pending", "rawInput": input,
			"_meta": map[string]any{"bma": map[string]any{"name": "bash", "permission": permission}}})
		if !allow {
			raw := c.request("session/request_permission", map[string]any{"sessionId": sid,
				"toolCall": map[string]any{"toolCallId": callID, "title": "bash", "rawInput": input},
				"options":  []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}})
			var r struct {
				Result struct {
					Outcome struct {
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				} `json:"result"`
			}
			_ = json.Unmarshal(raw, &r)
			if r.Result.Outcome.OptionID != "allow" {
				return endTurn, nil
			}
		}
		out, isErr := c.tool(sid, callID, "bash", input)
		if !amnesia {
			tr.Results[callID] = out
			tr.save(sid)
		}
		status := "completed"
		if isErr {
			status = "failed"
		}
		c.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": callID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": out}}}})
	}
	c.message(sid, "m-final", "message", "done")
	return endTurn, nil
}

// beginEpoch is what the platform does for a session the restarted executor
// does not have: its events are numbered from 1 again.
func (p *recordingPlatform) beginEpoch(sessionID string) {
	p.mu.Lock()
	p.persisted[sessionID] = 0
	p.mu.Unlock()
}

// crashPoint blocks the executor at a named point until its context ends,
// which is what a killed executor looks like from there.
type crashPoint struct {
	at      string
	reached chan struct{}
	once    sync.Once
}

func (c *crashPoint) hook(ctx context.Context, point string) {
	if point != c.at {
		return
	}
	hit := false
	c.once.Do(func() { hit = true })
	if !hit {
		return
	}
	close(c.reached)
	<-ctx.Done()
}

func countType(events []AgentEventRecord, eventType string) int {
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// Killing the executor process (and so its agent) at a moment of a turn and
// starting it again: the platform starts the same turn (same turn_id and
// input, executor_session_ref, then the epoch's sequence from 1). The host
// settles the interrupted call under its own event and approval id and hands
// the brain a recovery note: the turn completes, the tool ran once, and
// nothing is reported twice.
func TestACPAgentTurnResumeAfterExecutorRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	cases := []struct {
		name, prompt, crashAt string
		allow                 bool
		maxToolUses           int // one per epoch that got to emit it; a reported call is not emitted again
	}{
		{name: "before the tool ran", prompt: "resume", crashAt: "tool:before", allow: true, maxToolUses: 2},
		{name: "approval pending", prompt: "resume", crashAt: "approval", maxToolUses: 2},
		{name: "after the tool ran, before the agent saw the result", prompt: "resume-amnesia", crashAt: "tool:after", allow: true, maxToolUses: 2},
		{name: "after the result was reported, brain kept it", prompt: "resume", crashAt: "reported", allow: true, maxToolUses: 1},
		{name: "after the result was reported, brain forgot it", prompt: "resume-amnesia", crashAt: "reported", allow: true, maxToolUses: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			root, platform := t.TempDir(), newRecordingPlatform()
			crash := &crashPoint{at: tc.crashAt, reached: make(chan struct{})}
			metadata := map[string]string{}
			if tc.allow {
				metadata["tool_policy"] = `{"bash":"allow"}`
			}
			withMetadata := func(p *TurnStartParams) { p.Metadata = metadata }
			const session, turn = "sesn_r", "turn_r"

			first := startACPFixtureAt(t, ctx, ACPAgentOptions{}, root, platform, crash.hook)
			first.start(t, ctx, session, turn, tc.prompt, withMetadata)
			var request InteractionRequestParams
			if tc.crashAt == "approval" {
				platform.waitFor(t, "approval", func() bool { return len(platform.interactions) == 1 })
				request = platform.interactions[0]
			} else {
				select {
				case <-crash.reached:
				case <-ctx.Done():
					t.Fatalf("the executor never reached %s; events=%v", tc.crashAt, eventTypes(platform.snapshot()))
				}
			}
			// The kill: the agent process dies, the connection drops, nothing is flushed or closed properly.
			// The connection goes first so the turn's end that follows reaches nobody.
			stopCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			_ = first.client.Stop(stopCtx, "killed")
			stop()
			_ = first.agent.Close()
			beforeRestart := len(platform.snapshot())
			platform.beginEpoch(session)

			second := startACPFixtureAt(t, ctx, ACPAgentOptions{}, root, platform, nil)
			second.start(t, ctx, session, turn, tc.prompt, withMetadata, func(p *TurnStartParams) { p.ExecutorSessionRef = session })
			if tc.crashAt == "approval" {
				platform.waitFor(t, "the approval asked again", func() bool { return len(platform.interactions) == 2 })
				if again := platform.interactions[1]; again.InteractionID != request.InteractionID {
					t.Fatalf("approval id after restart = %s, before = %s", again.InteractionID, request.InteractionID)
				}
				if _, err := second.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: session, InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
					t.Fatal(err)
				}
			}
			idle := platform.waitIdle(t, turn)
			assertIdle(t, idle, TurnOutcomeCompleted)

			events := platform.snapshot()
			for _, e := range events {
				if e.Type == AgentEventTypeSessionError {
					t.Fatalf("session.error: %s", e.Event)
				}
			}
			for _, check := range []struct {
				eventType string
				want      int
			}{{AgentEventTypeAgentToolResult, 1}, {AgentEventTypeAgentMessage, 1}, {AgentEventTypeSessionStatusIdle, 1}} {
				if n := countType(events, check.eventType); n != check.want {
					t.Fatalf("%s events = %d, want %d; events=%v", check.eventType, n, check.want, eventTypes(events))
				}
			}
			if n := countType(events, AgentEventTypeAgentToolUse); n < 1 || n > tc.maxToolUses {
				t.Fatalf("agent.tool_use events = %d, want 1..%d; events=%v", n, tc.maxToolUses, eventTypes(events))
			}
			// Event ids are unique within an epoch (the platform keys them by epoch), and the
			// result names a tool_use of its own epoch.
			for _, segment := range [][]AgentEventRecord{events[:beforeRestart], events[beforeRestart:]} {
				seen := map[string]bool{}
				for _, e := range segment {
					if seen[e.EventID] {
						t.Fatalf("event id %s appears twice in one epoch", e.EventID)
					}
					seen[e.EventID] = true
				}
				for _, e := range segment {
					if e.Type == AgentEventTypeAgentToolResult && !hasEvent(segment, eventJSON(t, e)["tool_use_id"].(string)) {
						t.Fatalf("tool_result names a tool_use of another epoch: %v", eventJSON(t, e))
					}
				}
			}
			// The tool ran once.
			raw, err := os.ReadFile(filepath.Join(root, "work", "acp", session, "count.txt"))
			if err != nil || strings.TrimSpace(string(raw)) != "x" {
				t.Fatalf("count.txt = %q, %v: the tool did not run exactly once", raw, err)
			}
			if tc.crashAt == "approval" && len(platform.interactions) != 2 {
				t.Fatalf("interaction.request count = %d", len(platform.interactions))
			}
		})
	}
}

// A tool call whose start was recorded without a result (the executor died
// while the tool ran) has an unknown outcome: it is not run again, the agent
// gets an error it can read.
func TestACPAgentToolOfUnknownOutcomeIsNotRunAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	ledger, err := f.agent.ledger("sesn_u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.begin("turn_u"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.update("call_r1", func(c *acpCallState) { c.Phase = acpPhaseStarted }); err != nil {
		t.Fatal(err)
	}
	f.start(t, ctx, "sesn_u", "turn_u", "resume", func(p *TurnStartParams) { p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`} })
	assertIdle(t, f.platform.waitIdle(t, "turn_u"), TurnOutcomeCompleted)
	events := eventsOf(f.platform.snapshot(), "turn_u")
	var result map[string]any
	for _, e := range events {
		if e.Type == AgentEventTypeAgentToolResult {
			result = eventJSON(t, e)
		}
	}
	if result == nil || result["is_error"] != true || !strings.Contains(fmt.Sprint(result["content"]), "not run again") {
		t.Fatalf("tool_result = %v", result)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_u", "count.txt")); err == nil {
		t.Fatal("the tool ran although its outcome was unknown")
	}
}

// A turn that finished in an earlier process (its input done and acknowledged)
// is not prompted again when the platform starts it once more; a new turn
// replaces the ledger.
func TestACPAgentLedgerFollowsTheTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	ledger, err := f.agent.ledger("sesn_l")
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := ledger.begin("turn_1"); err != nil || kept {
		t.Fatalf("first begin kept=%v err=%v", kept, err)
	}
	if err := ledger.markInputDone("in_turn_1"); err != nil {
		t.Fatal(err)
	}
	f.start(t, ctx, "sesn_l", "turn_1", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if n := countType(eventsOf(f.platform.snapshot(), "turn_1"), AgentEventTypeAgentMessage); n != 0 {
		t.Fatalf("a finished input was prompted again: %d messages", n)
	}
	f.start(t, ctx, "sesn_l", "turn_2", "hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_l" })
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if n := countType(eventsOf(f.platform.snapshot(), "turn_2"), AgentEventTypeAgentMessage); n != 1 {
		t.Fatalf("messages of the next turn = %d", n)
	}
	if ledger.inputDone("in_turn_1") || !ledger.inputDone("in_turn_2") {
		t.Fatal("the ledger did not follow the turn")
	}
}
