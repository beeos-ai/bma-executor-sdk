package bmaexecutor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The executor re-checks a local tool call itself (ADR-0005 gateway, local
// copy): the brain is not trusted to have asked. Every case below is a brain
// that calls _bma/tools/call regardless of the permission relay.

func gateFixture(t *testing.T) (*acpFixture, context.Context) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return startACPFixture(t, ctx, ACPAgentOptions{}), ctx
}

// reported is the answer a scripted brain saw ("text|isError|outcome|errorCode"),
// as the message it wrote into the turn.
func reported(t *testing.T, f *acpFixture, turn string) []string {
	t.Helper()
	assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeCompleted)
	var out []string
	for _, e := range eventsOf(f.platform.snapshot(), turn) {
		if e.Type == AgentEventTypeAgentMessage {
			out = append(out, eventJSON(t, e)["content"].([]any)[0].(map[string]any)["text"].(string))
		}
	}
	return out
}

func assertNotRun(t *testing.T, f *acpFixture, session, file string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.root, "acp", session, file)); err == nil {
		t.Fatalf("%s exists: the tool ran", file)
	}
}

func TestACPAgentRefusesAnAskToolTheBrainDidNotAskFor(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g1", "turn_1", "bypass") // no tool_policy: bash is ask
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasPrefix(got[0], "approval_required: ") || !strings.HasSuffix(got[0], "|true|not_forwarded|approval_required") {
		t.Fatalf("answer = %q", got)
	}
	assertNotRun(t, f, "sesn_g1", "pwned.txt")
	if len(f.platform.interactions) != 0 {
		t.Fatalf("interactions = %+v", f.platform.interactions)
	}
	// The refusal is what the model reads: a failed tool result in the session log.
	var result map[string]any
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if e.Type == AgentEventTypeAgentToolResult {
			result = eventJSON(t, e)
		}
	}
	if result == nil || result["is_error"] != true {
		t.Fatalf("tool_result = %v", result)
	}
}

func TestACPAgentRefusesADeniedTool(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g2", "turn_1", "bypass", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"deny"}`}
	})
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasPrefix(got[0], "permission_denied: ") || !strings.HasSuffix(got[0], "|true|not_forwarded|permission_denied") {
		t.Fatalf("answer = %q", got)
	}
	assertNotRun(t, f, "sesn_g2", "pwned.txt")
}

func TestACPAgentRefusesAnAskToolTheUserDenied(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g3", "turn_1", "ask-ignore")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_g3", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeDeny}); err != nil {
		t.Fatal(err)
	}
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "|true|not_forwarded|approval_invalid") {
		t.Fatalf("answer = %q", got)
	}
	assertNotRun(t, f, "sesn_g3", "pwned.txt")
}

func TestACPAgentRefusesAnApprovalForAnotherInput(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g4", "turn_1", "bypass-swap")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_g4", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "|true|not_forwarded|approval_invalid") {
		t.Fatalf("answer = %q", got)
	}
	assertNotRun(t, f, "sesn_g4", "pwned.txt")
}

func TestACPAgentRunsAnApprovedAskTool(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g5", "turn_1", "ask-ignore")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_g5", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "|false|executed|") {
		t.Fatalf("answer = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_g5", "pwned.txt")); err != nil || strings.TrimSpace(string(b)) != "pwned" {
		t.Fatalf("pwned.txt = %q, %v", b, err)
	}
}

func TestACPAgentRunsAnAllowedToolWithoutAsking(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g6", "turn_1", "bypass", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	got := reported(t, f, "turn_1")
	if len(got) != 1 || !strings.HasSuffix(got[0], "|false|executed|") {
		t.Fatalf("answer = %q", got)
	}
	if len(f.platform.interactions) != 0 {
		t.Fatalf("interactions = %+v", f.platform.interactions)
	}
}

func TestACPAgentRefusesAToolCallAfterItsTurnEnded(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_g7", "turn_1", "late", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	home := os.Getenv("BMA_DSH_ACP_HOME")
	if home == "" {
		t.Skip("BMA_DSH_ACP_HOME is not set")
	}
	if err := os.WriteFile(filepath.Join(home, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var answer fakeToolResult
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(filepath.Join(home, "late.json")); err == nil && json.Unmarshal(raw, &answer) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late tool call was never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if answer.Outcome != "not_forwarded" || answer.ErrorCode != "stale_turn" || !answer.IsError {
		t.Fatalf("answer = %+v", answer)
	}
	assertNotRun(t, f, "sesn_g7", "late.txt")
}

// The decision is made on the turn's own state; these cases are about what
// that state says (table of gate decisions and their wire/event mapping).
func TestACPAgentToolGate(t *testing.T) {
	f, ctx := gateFixture(t)
	const turn = "turn_gt"
	approved := func(l *acpLedger, sid, call, tool string, input map[string]any) {
		noErr(t, l.update(call, func(c *acpCallState) {
			c.Decision, c.InteractionID, c.ToolName, c.InputHash = acpDecisionAllow, ApprovalID(sid, turn, call), tool, acpInputHash(input)
		}))
	}
	bashInput := map[string]any{"command": "true"}
	cases := []struct {
		name   string
		policy map[string]string
		setup  func(l *acpLedger, sid string)
		ledger string // turn the ledger belongs to
		ended  bool
		// presented is the approval id the brain presents with the call.
		presented string
		call      string
		tool      string
		input     map[string]any
		want      string // "" = may run, otherwise error code
	}{
		{name: "allow", policy: map[string]string{"bash": "allow"}, call: "c", tool: "bash", input: bashInput},
		{name: "deny", policy: map[string]string{"bash": "deny"}, call: "c", tool: "bash", input: bashInput, want: "permission_denied"},
		{name: "unlisted tools are ask", policy: map[string]string{}, call: "c", tool: "bash", input: bashInput, want: "approval_required"},
		{name: "ask without a decision", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput, want: "approval_required"},
		{name: "ask approved", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			setup: func(l *acpLedger, sid string) { approved(l, sid, "c", "bash", bashInput) }},
		{name: "approved for another call", policy: map[string]string{"bash": "ask"}, call: "c2", tool: "bash", input: bashInput,
			setup: func(l *acpLedger, sid string) { approved(l, sid, "c", "bash", bashInput) }, want: "approval_required"},
		{name: "approved for another tool", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			setup: func(l *acpLedger, sid string) { approved(l, sid, "c", "write", bashInput) }, want: "approval_invalid"},
		{name: "approved for another input", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: map[string]any{"command": "rm -rf /"},
			setup: func(l *acpLedger, sid string) { approved(l, sid, "c", "bash", bashInput) }, want: "approval_invalid"},
		{name: "approved under another interaction id", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			setup: func(l *acpLedger, sid string) {
				approved(l, sid, "c", "bash", bashInput)
				noErr(t, l.update("c", func(c *acpCallState) { c.InteractionID = ApprovalID(sid, "turn_other", "c") }))
			}, want: "approval_invalid"},
		{name: "an approval presented with no answer recorded", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			presented: "appr_nothing", want: "approval_invalid"},
		{name: "another approval presented than the one answered", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			presented: "appr_other", setup: func(l *acpLedger, sid string) { approved(l, sid, "c", "bash", bashInput) }, want: "approval_invalid"},
		{name: "denied by the user", policy: map[string]string{"bash": "ask"}, call: "c", tool: "bash", input: bashInput,
			setup: func(l *acpLedger, sid string) {
				noErr(t, l.update("c", func(c *acpCallState) { c.Decision = acpDecisionDeny }))
			}, want: "approval_invalid"},
		{name: "a tool that was denied by policy needs no approval record", policy: map[string]string{"read": "deny"}, call: "c", tool: "read", input: map[string]any{}, want: "permission_denied"},
		{name: "ledger of another turn", policy: map[string]string{"bash": "allow"}, ledger: "turn_newer", call: "c", tool: "bash", input: bashInput, want: "stale_turn"},
		{name: "turn context ended", policy: map[string]string{"bash": "allow"}, ended: true, call: "c", tool: "bash", input: bashInput, want: "stale_turn"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := "sesn_gt" + string(rune('a'+i))
			ledger, err := f.agent.ledger(sid)
			noErr(t, err)
			_, err = ledger.begin(turn)
			noErr(t, err)
			if tc.setup != nil {
				tc.setup(ledger, sid)
			}
			if tc.ledger != "" {
				_, err = ledger.begin(tc.ledger)
				noErr(t, err)
			}
			turnCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if tc.ended {
				cancel()
			}
			route := newLocalToolRoute(localRouteConfig{core: f.agent.opts.Core, sessionID: sid, turnID: turn, ledger: ledger, policy: tc.policy, ctx: turnCtx})
			got := route.gate(RouteCall{ID: tc.call, Name: tc.tool, Input: tc.input, ApprovalID: tc.presented})
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("refused: %+v", got)
			case tc.want != "" && (got == nil || got.Code != tc.want || got.Outcome != RouteOutcomeNotForwarded || !got.IsError || !strings.HasPrefix(got.Text, tc.want+": ")):
				t.Fatalf("gate = %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestACPAgentEffectivePolicy(t *testing.T) {
	cases := []struct {
		mode      string
		overrides map[string]string
		want      map[string]string
	}{
		{"default", nil, map[string]string{"bash": "ask", "write": "ask", "edit": "ask", "read": "allow", "glob": "allow", "grep": "allow"}},
		{"accept_edits", nil, map[string]string{"bash": "ask", "write": "allow", "edit": "allow", "read": "allow", "glob": "allow", "grep": "allow"}},
		{"default", map[string]string{"bash": "allow", "read": "deny"}, map[string]string{"bash": "allow", "write": "ask", "edit": "ask", "read": "deny", "glob": "allow", "grep": "allow"}},
	}
	for _, tc := range cases {
		got := acpEffectivePolicy(tc.mode, tc.overrides)
		for tool, want := range tc.want {
			if got[tool] != want {
				t.Fatalf("%s %v: %s = %q, want %q", tc.mode, tc.overrides, tool, got[tool], want)
			}
		}
	}
}

func TestACPAgentNoTurnOfThatProcessIsStale(t *testing.T) {
	f, ctx := gateFixture(t)
	f.start(t, ctx, "sesn_old", "turn_1", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	// A call that comes from a process that is not the session's (an earlier
	// brain process): the turn is not found.
	raw, _ := json.Marshal(map[string]any{"sessionId": "sesn_old", "toolCallId": "c", "name": "bash", "input": map[string]any{"command": "true"}})
	got, rpcErr := f.agent.onRequest(ctx, &acpProcess{}, "_bma/tools/call", raw)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	m := got.(map[string]any)
	if m["outcome"] != RouteOutcomeNotForwarded || m["errorCode"] != "stale_turn" || m["isError"] != true {
		t.Fatalf("answer = %v", m)
	}
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
