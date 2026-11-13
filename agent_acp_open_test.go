//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// turn.start is answered as soon as the turn is accepted (contract): opening
// the agent's session runs afterwards, and what goes wrong there is reported
// with the contract's turn failure (session.error with an AgentErrorCode,
// session.status_idle failed), not as a failed or late turn.start.

func slowOpen(ms int) ACPAgentOptions {
	return ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_NEW_DELAY_MS=" + strconv.Itoa(ms)}}
}

func TestACPAgentAnswersTurnStartBeforeTheSessionIsOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(1500))
	began := time.Now()
	result := f.start(t, ctx, "sesn_slow", "turn_1", "hello")
	if took := time.Since(began); took > 700*time.Millisecond {
		t.Fatalf("turn.start took %v while session/new takes 1.5 s", took)
	}
	if !result.Accepted || result.TurnID != "turn_1" || result.ExecutorSessionRef != "sesn_slow" || result.Effective.Harness != "dsh" {
		t.Fatalf("result = %+v", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if errs := sessionErrors(f.platform, "turn_1"); len(errs) != 0 {
		t.Fatalf("session errors = %+v", errs)
	}
}

// A cancel that arrives while the session is opening ends the turn at once;
// the open runs on and the next turn uses the session it left.
func TestACPAgentCancelDuringOpenEndsTheTurnAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(2500))
	f.start(t, ctx, "sesn_c", "turn_1", "hello")
	began := time.Now()
	canceled, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_c", TurnID: "turn_1", Reason: "user_interrupt"})
	if err != nil || !canceled.Canceled {
		t.Fatalf("turn.cancel = %+v %v", canceled, err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the canceled turn ended after %v, the open takes 2.5 s", took)
	}
	f.start(t, ctx, "sesn_c", "turn_2", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

// The cause of a failed open is the contract's code, in session.error.
func TestACPAgentFailedOpenIsATypedTurnFailure(t *testing.T) {
	for _, tc := range []struct {
		acpCode int
		want    string
	}{
		{-32000, AgentErrorCodeAgentAuthRequired},
		{-32602, AgentErrorCodeAgentProtocolError},
		{-32603, AgentErrorCodeAgentError},
	} {
		t.Run(tc.want, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := startACPFixture(t, ctx, ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_NEW_ERROR=" + strconv.Itoa(tc.acpCode)}})
			f.start(t, ctx, "sesn_f", "turn_1", "hello") // accepted
			assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
			errs := sessionErrors(f.platform, "turn_1")
			if len(errs) != 1 || errs[0].Type != tc.want {
				t.Fatalf("session errors = %+v, want type %s", errs, tc.want)
			}
		})
	}
}

// A refusal that is certain and cheap stays a turn.start error.
func TestACPAgentStillRefusesBadParamsInTurnStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, slowOpen(1500))
	began := time.Now()
	_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_m", TurnID: "turn_1", InputID: "in_1", Config: &AgentTurnConfig{PermissionMode: "nonsense"},
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}})
	var rpc *RPCError
	if !asRPC(err, &rpc) || rpc.Code != CodeInvalidParams || time.Since(began) > 700*time.Millisecond {
		t.Fatalf("err = %v", err)
	}
}

// --- prewarm of the Claude Code profile ---

func startFakeClaudeLogged(t *testing.T, ctx context.Context, mode string, log func(LogEvent)) *ACPAgent {
	t.Helper()
	exe, err := os.Executable()
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	agent, err := NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCode, Command: []string{exe}, Env: []string{"BMA_ACP_FAKE_CLAUDE=" + mode},
		StateDir: t.TempDir(), Core: core, CancelGrace: time.Second, OnLog: log})
	noErr(t, err)
	t.Cleanup(func() { _ = agent.Close() })
	return agent
}

// A prewarm that fails is visible (log and WaitPrewarm) and changes nothing
// else: the agent is constructed, serves its capability and opens the first
// real session itself.
func TestClaudePrewarmFailureIsVisibleAndHarmless(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var warned []string
	agent := startFakeClaudeLogged(t, ctx, "warmfail", func(e LogEvent) {
		if e.Level == LogWarn {
			mu.Lock()
			warned = append(warned, e.Message)
			mu.Unlock()
		}
	})
	if err := agent.WaitPrewarm(ctx); err == nil || !strings.Contains(err.Error(), "scripted prewarm failure") {
		t.Fatalf("WaitPrewarm = %v", err)
	}
	mu.Lock()
	if len(warned) != 1 || !strings.Contains(warned[0], "prewarm failed") {
		t.Fatalf("warnings = %v", warned)
	}
	mu.Unlock()
	if c := agent.Capability(); !c.Enabled || len(c.Harnesses) != 1 {
		t.Fatalf("capability = %+v", c)
	}
	_, err := agent.OpenTurn(ctx, TurnStartParams{SessionID: "sesn_w", TurnID: "turn_1", InputID: "in_1", Config: &AgentTurnConfig{}})
	noErr(t, err)
	noErr(t, agent.openSession(ctx, agent.session("sesn_w")))
}

// The throwaway session is opened and closed, never prompted, and is not the
// session a turn uses.
func TestClaudePrewarmOpensAndClosesAThrowawaySession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := startFakeClaudeLogged(t, ctx, "ok", nil)
	noErr(t, agent.WaitPrewarm(ctx))
	agent.mu.Lock()
	n := len(agent.sessions)
	agent.mu.Unlock()
	if n != 0 {
		t.Fatalf("the prewarm left %d sessions in the agent", n)
	}
	if _, err := os.Stat(filepath.Join(agent.opts.StateDir, "prewarm")); err != nil {
		t.Fatalf("scratch directory: %v", err)
	}
}

func TestPrewarmCanBeDisabledAndDSHHasNone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	if _, ok := f.agent.adapter.(interface{ WaitPrewarm(context.Context) error }); ok || f.agent.WaitPrewarm(ctx) != nil {
		t.Fatal("the DSH profile has a prewarm")
	}
	exe, err := os.Executable()
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	agent, err := NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCode, DisablePrewarm: true, Command: []string{exe},
		Env: []string{"BMA_ACP_FAKE_CLAUDE=ok"}, StateDir: t.TempDir(), Core: core})
	noErr(t, err)
	t.Cleanup(func() { _ = agent.Close() })
	if agent.adapter.(*claudeACPAdapter).warm != nil {
		t.Fatal("DisablePrewarm did not disable it")
	}
}

// With the real, pinned claude-agent-acp: after the prewarm the first turn of a
// session is answered at once and its first content event comes without the
// cold start of Claude Code.
func TestClaudeCodePrewarmThenFirstTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "true"})
	f := startClaudeFixture(t, ctx, model, ACPAgentOptions{}, "")
	noErr(t, f.agent.WaitPrewarm(ctx))
	before := len(f.platform.snapshot())
	began := time.Now()
	f.start(t, ctx, "sesn_p", "turn_1", "run it")
	ack := time.Since(began)
	f.platform.waitFor(t, "first content event", func() bool { return len(f.platform.events) > before+1 })
	t.Logf("turn.start answered in %v, first content event after %v (prewarmed)", ack, time.Since(began))
	if ack > 250*time.Millisecond {
		t.Fatalf("turn.start took %v", ack)
	}
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	_, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_p", TurnID: "turn_1", Reason: "user_interrupt"})
	noErr(t, err)
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
}
