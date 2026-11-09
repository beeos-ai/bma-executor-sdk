//go:build !windows

package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// The ids a Rig's route must be bound to. A Factory derives the approval id of
// a call from them (bmaexecutor.ApprovalID).
const (
	SessionID = "sesn_t1"
	TurnID    = "turn_1"
)

// Rig is one route under test, with the levers the platform and the machine
// have: the user's answer to a permission, an executor restart, a newer brain
// epoch, a turn cancel, hands that are offline.
type Rig struct {
	// Workspace is the directory the tools run in (the hands' working
	// directory) on this machine: the cases look at the files the tools leave
	// there, and at its parent for escapes.
	Workspace string
	// Route is the route of the current turn (SessionID, TurnID). The cases ask
	// for it before every call, so Restart and Supersede may replace it.
	Route func() bmaexecutor.ToolRoute
	// Approve records the user's answer ("allow" or "deny") to the permission
	// interaction of a call, asked under bmaexecutor.ApprovalID(SessionID,
	// TurnID, callID) for tool and input, and makes the route present it.
	Approve func(callID, tool string, input map[string]any, decision string)
	// Restart is an executor (and brain process) restart in the middle of the
	// turn: nothing in memory survives, what the route keeps on disk or the
	// platform keeps does. Route then returns a route of the same turn.
	Restart func()
	// MarkStarted: the call was submitted to the hands and the process died
	// before the result was saved.
	MarkStarted func(callID string)
	// Supersede starts a newer epoch of the turn; routes handed out before are
	// stale and Route returns the newer one.
	Supersede func()
	// CancelTurn is the user's cancel of the turn: calls in flight end and
	// their process groups are killed.
	CancelTurn func()
	// HandsOffline makes the hands unreachable (and reachable again). Nil when
	// the hands cannot be somewhere else (they are in the same process): T11 is
	// then not run. There is no other exemption.
	HandsOffline func(offline bool)
}

// Factory builds a Rig for one case. toolPolicy overrides the default
// permission of tools (allow, ask or deny): without an override bash, write and
// edit are ask and read, glob and grep are allow. The rig's route must enforce
// the result, and everything it creates must be removed with t.
type Factory func(t *testing.T, toolPolicy map[string]string) *Rig

func (r *Rig) call(callID, name string, input map[string]any) *bmaexecutor.RouteResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.Route().Call(ctx, bmaexecutor.RouteCall{ID: callID, Name: name, Input: wireInput(input)})
}

// wireInput is the input as the brain's JSON decoder hands it over (numbers are float64).
func wireInput(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func bashInput(command string) map[string]any { return map[string]any{"command": command} }

func exists(rig *Rig, name string) bool {
	_, err := os.Stat(filepath.Join(rig.Workspace, name))
	return err == nil
}

func lineCount(rig *Rig, name string) int {
	raw, err := os.ReadFile(filepath.Join(rig.Workspace, name))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func wantRefused(t *testing.T, got *bmaexecutor.RouteResult, code string) {
	t.Helper()
	if got.Outcome != bmaexecutor.RouteOutcomeNotForwarded || got.Code != code || !got.IsError || !strings.HasPrefix(got.Text, code+": ") {
		t.Fatalf("answer = %+v, want not_forwarded %s", got, code)
	}
}

func wantExecuted(t *testing.T, got *bmaexecutor.RouteResult) {
	t.Helper()
	if got.Outcome != bmaexecutor.RouteOutcomeExecuted || got.Code != "" || got.IsError {
		t.Fatalf("answer = %+v, want executed without error", got)
	}
}

type conformanceCase struct {
	id   string
	name string
	run  func(t *testing.T, factory Factory)
}

// RunToolRouteConformance runs T1–T11 against the routes factory builds, one
// subtest per case. Call it from a Test function, once per route (wrap it in
// t.Run to name the route).
func RunToolRouteConformance(t *testing.T, factory Factory) {
	t.Helper()
	for _, c := range conformanceCases {
		t.Run(c.id+"_"+c.name, func(t *testing.T) { c.run(t, factory) })
	}
}

var conformanceCases = []conformanceCase{
	{"T1", "allowed_tool_executes", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow", "write": "allow"})
		got := rig.call("c1", "bash", bashInput("printf hi"))
		wantExecuted(t, got)
		if got.Text != "hi\nexit_code: 0" || got.Replayed || got.Background != nil {
			t.Fatalf("bash answer = %+v", got)
		}
		wantExecuted(t, rig.call("c2", "write", map[string]any{"file_path": "a.txt", "content": "hello"}))
		read := rig.call("c3", "read", map[string]any{"file_path": "a.txt"})
		wantExecuted(t, read)
		if !strings.Contains(read.Text, "hello") {
			t.Fatalf("read answer = %+v", read)
		}
		// A tool that fails is still an executed call; IsError says so.
		failed := rig.call("c4", "read", map[string]any{"file_path": "missing.txt"})
		if failed.Outcome != bmaexecutor.RouteOutcomeExecuted || !failed.IsError || failed.Text == "" {
			t.Fatalf("failing read answer = %+v", failed)
		}
	}},
	{"T2", "policy_deny_is_not_run", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "deny"})
		wantRefused(t, rig.call("c1", "bash", bashInput("touch pwned.txt")), "permission_denied")
		if exists(rig, "pwned.txt") {
			t.Fatal("the denied tool ran")
		}
	}},
	{"T3", "ask_needs_a_matching_approval", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "ask"})
		run := bashInput("touch pwned.txt")
		wantRefused(t, rig.call("none", "bash", run), "approval_required")
		rig.Approve("other-input", "bash", bashInput("true"), "allow")
		wantRefused(t, rig.call("other-input", "bash", run), "approval_invalid")
		rig.Approve("other-tool", "write", run, "allow")
		wantRefused(t, rig.call("other-tool", "bash", run), "approval_invalid")
		rig.Approve("denied", "bash", run, "deny")
		wantRefused(t, rig.call("denied", "bash", run), "approval_invalid")
		if exists(rig, "pwned.txt") {
			t.Fatal("a call without a matching approval ran")
		}
		rig.Approve("ok", "bash", run, "allow")
		wantExecuted(t, rig.call("ok", "bash", run))
		if !exists(rig, "pwned.txt") {
			t.Fatal("the approved call did not run")
		}
	}},
	{"T4", "approval_is_one_call_and_a_retry_runs_once", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "ask"})
		run := bashInput("echo x >> count.txt")
		rig.Approve("c1", "bash", run, "allow")
		first := rig.call("c1", "bash", run)
		wantExecuted(t, first)
		retry := rig.call("c1", "bash", run)
		if retry.Outcome != bmaexecutor.RouteOutcomeExecuted || retry.IsError || retry.Text != first.Text || !retry.Replayed {
			t.Fatalf("retry answer = %+v, first %+v", retry, first)
		}
		if n := lineCount(rig, "count.txt"); n != 1 {
			t.Fatalf("the command ran %d times", n)
		}
		// The approval was for that call: the same command under a new call id needs its own.
		wantRefused(t, rig.call("c2", "bash", run), "approval_required")
		if n := lineCount(rig, "count.txt"); n != 1 {
			t.Fatalf("the unapproved call ran (%d)", n)
		}
	}},
	{"T5", "restart_keeps_results_and_never_reruns", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		run := bashInput("echo x >> count.txt")
		first := rig.call("c1", "bash", run)
		wantExecuted(t, first)
		rig.Restart()
		again := rig.call("c1", "bash", run)
		if again.Outcome != bmaexecutor.RouteOutcomeExecuted || again.Text != first.Text || !again.Replayed {
			t.Fatalf("after restart = %+v, first %+v", again, first)
		}
		// Submitted, but the result was lost with the process: unknown, never run again.
		rig.MarkStarted("c2")
		rig.Restart()
		lost := rig.call("c2", "bash", bashInput("touch rerun.txt"))
		if lost.Outcome != bmaexecutor.RouteOutcomeUnknown || lost.Code != "outcome_unknown" || !lost.IsError || !strings.HasPrefix(lost.Text, "outcome_unknown: ") {
			t.Fatalf("lost call answer = %+v", lost)
		}
		if exists(rig, "rerun.txt") || lineCount(rig, "count.txt") != 1 {
			t.Fatal("a call was run again")
		}
	}},
	{"T6", "bash_past_its_wait", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		got := rig.call("c1", "bash", map[string]any{"command": "sleep 30", "timeout": 300})
		wantExecuted(t, got)
		if got.Background == nil || got.Background.PID <= 0 || got.Background.OutputFile == "" {
			t.Fatalf("answer = %+v, want a background pid and output file", got)
		}
		t.Cleanup(func() { _ = syscall.Kill(-got.Background.PID, syscall.SIGKILL) })
		if !processAlive(got.Background.PID) {
			t.Fatal("the command was killed at the end of the wait")
		}
	}},
	{"T7", "cancel_ends_the_call_and_kills_the_process_group", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		answer := make(chan *bmaexecutor.RouteResult, 1)
		go func() { answer <- rig.call("c1", "bash", bashInput("sleep 60 & echo $! > child.pid; wait")) }()
		pid := waitForPID(t, filepath.Join(rig.Workspace, "child.pid"))
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		rig.CancelTurn()
		select {
		case got := <-answer:
			if !got.IsError {
				t.Fatalf("canceled call answer = %+v", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the canceled call did not end within the budget")
		}
		deadline := time.Now().Add(5 * time.Second)
		for processAlive(pid) {
			if time.Now().After(deadline) {
				t.Fatal("the process group survived the cancel")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}},
	{"T8", "hands_refusal_is_passed_through", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"write": "allow"})
		got := rig.call("c1", "write", map[string]any{"file_path": "../escape.txt", "content": "x"})
		if got.Outcome != bmaexecutor.RouteOutcomeExecuted || !got.IsError || got.Text == "" {
			t.Fatalf("answer = %+v", got)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(rig.Workspace), "escape.txt")); err == nil {
			t.Fatal("the hands wrote outside their workspace")
		}
	}},
	{"T9", "large_write_roundtrips", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"write": "allow"})
		content := strings.Repeat("0123456789abcdef", 128*1024) // 2 MiB
		wantExecuted(t, rig.call("c1", "write", map[string]any{"file_path": "big.txt", "content": content}))
		got, err := os.ReadFile(filepath.Join(rig.Workspace, "big.txt"))
		if err != nil || string(got) != content {
			t.Fatalf("big.txt: %d bytes, %v", len(got), err)
		}
	}},
	{"T10", "an_older_epoch_is_refused", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		old := rig.Route()
		rig.Supersede()
		got := old.Call(context.Background(), bmaexecutor.RouteCall{ID: "c1", Name: "bash", Input: wireInput(bashInput("touch stale.txt"))})
		wantRefused(t, got, "stale_turn")
		if exists(rig, "stale.txt") {
			t.Fatal("a call of an older epoch ran")
		}
		wantExecuted(t, rig.call("c2", "bash", bashInput("true")))
	}},
	{"T11", "offline_hands_are_not_forwarded", func(t *testing.T, factory Factory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		if rig.HandsOffline == nil {
			t.Skip("the hands are in the same process: they cannot be offline")
		}
		rig.HandsOffline(true)
		wantRefused(t, rig.call("c1", "bash", bashInput("touch late.txt")), "environment_unavailable")
		if exists(rig, "late.txt") {
			t.Fatal("the call ran on offline hands")
		}
		// Not forwarded is safe to ask again: it was not recorded.
		rig.HandsOffline(false)
		wantExecuted(t, rig.call("c1", "bash", bashInput("touch late.txt")))
		if !exists(rig, "late.txt") {
			t.Fatal("the retry did not run")
		}
	}},
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
