//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The local bash tool leaves a command that outlives its wait running (pid and
// output file), like the platform gateway. What ends such a command: its own
// exit, a canceled or timed-out turn, the end of the session, on_timeout kill.
// A turn that completed does not end it.

type bashRig struct {
	agent  *ACPAgent
	turn   *acpTurn
	cancel context.CancelFunc
}

func newBashRig(t *testing.T) *bashRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	workspace := filepath.Join(core.opts.WorkRoot, "acp", rigSession)
	noErr(t, os.MkdirAll(workspace, 0o755))
	noErr(t, core.openLocal(acpBrain, rigSession, workspace))
	agent := &ACPAgent{opts: ACPAgentOptions{Core: core, StateDir: t.TempDir()}}
	t.Cleanup(agent.background.killAll)
	ledger, err := agent.ledger(rigSession)
	noErr(t, err)
	_, err = ledger.begin(rigTurn)
	noErr(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	turn := &acpTurn{agent: agent, ctx: ctx, ledger: ledger, turnID: rigTurn, policy: acpEffectivePolicy("default", map[string]string{"bash": "allow"})}
	turn.toolRoute = localToolRoute{agent: agent, turn: turn}
	return &bashRig{agent: agent, turn: turn, cancel: cancel}
}

func (r *bashRig) background(t *testing.T, callID string) *RouteBackground {
	t.Helper()
	got := r.turn.toolRoute.call(context.Background(), routedCall{SessionID: rigSession, CallID: callID, Name: "bash",
		Input: map[string]any{"command": "sleep 30", "timeout": float64(200)}})
	wantExecuted(t, got)
	if got.Background == nil || !processAlive(got.Background.PID) {
		t.Fatalf("answer = %+v", got)
	}
	if _, err := os.Stat(got.Background.OutputFile); err != nil {
		t.Fatalf("output file: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-got.Background.PID, syscall.SIGKILL) })
	return got.Background
}

func waitDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the command is still running", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLocalBashBackgroundEndsWithACanceledTurn(t *testing.T) {
	rig := newBashRig(t)
	bg := rig.background(t, "c1")
	rig.cancel()
	waitDead(t, bg.PID, "turn canceled")
}

func TestLocalBashBackgroundSurvivesACompletedTurnAndEndsWithTheSession(t *testing.T) {
	rig := newBashRig(t)
	bg := rig.background(t, "c1")
	rig.turn.completed.Store(true)
	rig.cancel() // the SDK cancels the turn context when RunTurn returns
	time.Sleep(300 * time.Millisecond)
	if !processAlive(bg.PID) {
		t.Fatal("the command was killed when its turn completed")
	}
	noErr(t, rig.agent.CloseSession(context.Background(), rigSession, "test"))
	waitDead(t, bg.PID, "session closed")
	if _, err := os.Stat(bg.OutputFile); err == nil {
		t.Fatal("the output file outlived the session")
	}
}

func TestLocalBashOutputFileHoldsTheOutputAndTheCommandOutlivesTheWait(t *testing.T) {
	rig := newBashRig(t)
	got := rig.turn.toolRoute.call(context.Background(), routedCall{SessionID: rigSession, CallID: "c1", Name: "bash",
		Input: map[string]any{"command": "echo early; sleep 30", "timeout": float64(500)}})
	wantExecuted(t, got)
	if !strings.Contains(got.Text, "running in the background") || !strings.Contains(got.Text, got.Background.OutputFile) {
		t.Fatalf("text = %q", got.Text)
	}
	t.Cleanup(func() { _ = syscall.Kill(-got.Background.PID, syscall.SIGKILL) })
	raw, err := os.ReadFile(got.Background.OutputFile)
	if err != nil || strings.TrimSpace(string(raw)) != "early" {
		t.Fatalf("output file = %q, %v", raw, err)
	}
	// The output file is inside the session's workspace, where the read tool can reach it.
	read := rig.turn.toolRoute.call(context.Background(), routedCall{SessionID: rigSession, CallID: "c2", Name: "read",
		Input: map[string]any{"file_path": got.Background.OutputFile}})
	if read.IsError || !strings.Contains(read.Text, "early") {
		t.Fatalf("read of the output file = %+v", read)
	}
}

func TestLocalBashOnTimeoutKillStopsTheCommand(t *testing.T) {
	rig := newBashRig(t)
	got := rig.turn.toolRoute.call(context.Background(), routedCall{SessionID: rigSession, CallID: "c1", Name: "bash",
		Input: map[string]any{"command": "echo $$ > pid.txt; sleep 30", "timeout": float64(300), "on_timeout": "kill"}})
	if got.Outcome != RouteOutcomeExecuted || !got.IsError || got.Background != nil || !strings.Contains(got.Text, "timed out") {
		t.Fatalf("answer = %+v", got)
	}
	pid := waitForPID(t, filepath.Join(rig.agent.opts.Core.workspace(acpBrain, rigSession), "pid.txt"))
	waitDead(t, pid, "on_timeout kill")
}

func TestLocalBashRepeatedCallReplaysTheBackgroundAnswer(t *testing.T) {
	rig := newBashRig(t)
	bg := rig.background(t, "c1")
	again := rig.turn.toolRoute.call(context.Background(), routedCall{SessionID: rigSession, CallID: "c1", Name: "bash",
		Input: map[string]any{"command": "sleep 30", "timeout": float64(200)}})
	if !again.Replayed || !strings.Contains(again.Text, bg.OutputFile) {
		t.Fatalf("replay = %+v", again)
	}
}
