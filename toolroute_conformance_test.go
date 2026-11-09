//go:build !windows

package bmaexecutor

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
)

// The tool route conformance suite (ADR-0029 §5, T1–T11): one set of cases,
// run against every toolRoute. A brain must not be able to tell which route
// its calls took: the same refusals with the same codes, the same three
// outcomes, the same texts, at most once per call id, the same cancel and
// epoch behavior. The only case a route does not run is T11 on hands in the
// same process (they cannot be offline); there is no other exemption.

const (
	rigSession = "sesn_t1"
	rigTurn    = "turn_1"
)

// routeRig is one route under test, with the levers the platform and the
// machine have: the user's answer to a permission, an executor restart, a
// newer brain epoch, a turn cancel, a hand that is offline.
type routeRig struct {
	workspace string // where the tools run (the hands' working directory)
	// route is the route of the current turn.
	route func() toolRoute
	// approve records the user's answer ("allow" or "deny") to the permission
	// interaction of a call, as given for tool and input.
	approve func(callID, tool string, input map[string]any, decision string)
	// restart is an executor (and brain process) restart in the middle of a turn.
	restart func()
	// markStarted: the call was submitted to the hands and the process died
	// before the result was saved.
	markStarted func(callID string)
	// supersede starts a newer epoch of the turn; routes handed out before are stale.
	supersede func()
	// cancelTurn is the user's cancel of the turn.
	cancelTurn func()
	// handsOffline is nil when the hands cannot be somewhere else.
	handsOffline func(offline bool)
}

func (r *routeRig) call(callID, name string, input map[string]any) *acpToolResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.route().call(ctx, routedCall{SessionID: rigSession, CallID: callID, Name: name, Input: wireInput(input)})
}

// wireInput is the input as the brain's JSON decoder hands it over (numbers are float64).
func wireInput(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

type routeFactory func(t *testing.T, toolPolicy map[string]string) *routeRig

func bashInput(command string) map[string]any { return map[string]any{"command": command} }

func exists(rig *routeRig, name string) bool {
	_, err := os.Stat(filepath.Join(rig.workspace, name))
	return err == nil
}

func lineCount(t *testing.T, rig *routeRig, name string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(rig.workspace, name))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func wantRefused(t *testing.T, got *acpToolResult, code string) {
	t.Helper()
	if got.Outcome != acpToolNotForwarded || got.Code != code || !got.IsError || !strings.HasPrefix(got.Text, code+": ") {
		t.Fatalf("answer = %+v, want not_forwarded %s", got, code)
	}
}

func wantExecuted(t *testing.T, got *acpToolResult) {
	t.Helper()
	if got.Outcome != acpToolExecuted || got.Code != "" || got.IsError {
		t.Fatalf("answer = %+v, want executed without error", got)
	}
}

type conformanceCase struct {
	id   string
	name string
	run  func(t *testing.T, factory routeFactory)
}

// registerToolRouteConformance runs T1–T11 against the route factory.
func registerToolRouteConformance(t *testing.T, name string, factory routeFactory, hasRemoteHands bool) {
	t.Run(name, func(t *testing.T) {
		for _, c := range conformanceCases {
			if c.id == "T11" && !hasRemoteHands {
				continue // the hands are in the same process: they cannot be offline
			}
			t.Run(c.id+"_"+c.name, func(t *testing.T) { c.run(t, factory) })
		}
	})
}

var conformanceCases = []conformanceCase{
	{"T1", "allowed_tool_executes", func(t *testing.T, factory routeFactory) {
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
		if failed.Outcome != acpToolExecuted || !failed.IsError || failed.Text == "" {
			t.Fatalf("failing read answer = %+v", failed)
		}
	}},
	{"T2", "policy_deny_is_not_run", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "deny"})
		wantRefused(t, rig.call("c1", "bash", bashInput("touch pwned.txt")), "permission_denied")
		if exists(rig, "pwned.txt") {
			t.Fatal("the denied tool ran")
		}
	}},
	{"T3", "ask_needs_a_matching_approval", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "ask"})
		run := bashInput("touch pwned.txt")
		wantRefused(t, rig.call("none", "bash", run), "approval_required")
		rig.approve("other-input", "bash", bashInput("true"), "allow")
		wantRefused(t, rig.call("other-input", "bash", run), "approval_invalid")
		rig.approve("other-tool", "write", run, "allow")
		wantRefused(t, rig.call("other-tool", "bash", run), "approval_invalid")
		rig.approve("denied", "bash", run, "deny")
		wantRefused(t, rig.call("denied", "bash", run), "approval_invalid")
		if exists(rig, "pwned.txt") {
			t.Fatal("a call without a matching approval ran")
		}
		rig.approve("ok", "bash", run, "allow")
		wantExecuted(t, rig.call("ok", "bash", run))
		if !exists(rig, "pwned.txt") {
			t.Fatal("the approved call did not run")
		}
	}},
	{"T4", "approval_is_one_call_and_a_retry_runs_once", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "ask"})
		run := bashInput("echo x >> count.txt")
		rig.approve("c1", "bash", run, "allow")
		first := rig.call("c1", "bash", run)
		wantExecuted(t, first)
		retry := rig.call("c1", "bash", run)
		if retry.Outcome != acpToolExecuted || retry.IsError || retry.Text != first.Text || !retry.Replayed {
			t.Fatalf("retry answer = %+v, first %+v", retry, first)
		}
		if n := lineCount(t, rig, "count.txt"); n != 1 {
			t.Fatalf("the command ran %d times", n)
		}
		// The approval was for that call: the same command under a new call id needs its own.
		wantRefused(t, rig.call("c2", "bash", run), "approval_required")
		if n := lineCount(t, rig, "count.txt"); n != 1 {
			t.Fatalf("the unapproved call ran (%d)", n)
		}
	}},
	{"T5", "restart_keeps_results_and_never_reruns", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		run := bashInput("echo x >> count.txt")
		first := rig.call("c1", "bash", run)
		wantExecuted(t, first)
		rig.restart()
		again := rig.call("c1", "bash", run)
		if again.Outcome != acpToolExecuted || again.Text != first.Text || !again.Replayed {
			t.Fatalf("after restart = %+v, first %+v", again, first)
		}
		// Submitted, but the result was lost with the process: unknown, never run again.
		rig.markStarted("c2")
		rig.restart()
		lost := rig.call("c2", "bash", bashInput("touch rerun.txt"))
		if lost.Outcome != acpToolUnknown || lost.Code != "outcome_unknown" || !lost.IsError || !strings.HasPrefix(lost.Text, "outcome_unknown: ") {
			t.Fatalf("lost call answer = %+v", lost)
		}
		if exists(rig, "rerun.txt") || lineCount(t, rig, "count.txt") != 1 {
			t.Fatal("a call was run again")
		}
	}},
	{"T6", "bash_past_its_wait", func(t *testing.T, factory routeFactory) {
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
	{"T7", "cancel_ends_the_call_and_kills_the_process_group", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		answer := make(chan *acpToolResult, 1)
		go func() { answer <- rig.call("c1", "bash", bashInput("sleep 60 & echo $! > child.pid; wait")) }()
		pid := waitForPID(t, filepath.Join(rig.workspace, "child.pid"))
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		rig.cancelTurn()
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
	{"T8", "hands_refusal_is_passed_through", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"write": "allow"})
		got := rig.call("c1", "write", map[string]any{"file_path": "../escape.txt", "content": "x"})
		if got.Outcome != acpToolExecuted || !got.IsError || got.Text == "" {
			t.Fatalf("answer = %+v", got)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(rig.workspace), "escape.txt")); err == nil {
			t.Fatal("the hands wrote outside their workspace")
		}
	}},
	{"T9", "large_write_roundtrips", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"write": "allow"})
		content := strings.Repeat("0123456789abcdef", 128*1024) // 2 MiB
		wantExecuted(t, rig.call("c1", "write", map[string]any{"file_path": "big.txt", "content": content}))
		got, err := os.ReadFile(filepath.Join(rig.workspace, "big.txt"))
		if err != nil || string(got) != content {
			t.Fatalf("big.txt: %d bytes, %v", len(got), err)
		}
	}},
	{"T10", "an_older_epoch_is_refused", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		old := rig.route()
		rig.supersede()
		got := old.call(context.Background(), routedCall{SessionID: rigSession, CallID: "c1", Name: "bash", Input: wireInput(bashInput("touch stale.txt"))})
		wantRefused(t, got, "stale_turn")
		if exists(rig, "stale.txt") {
			t.Fatal("a call of an older epoch ran")
		}
		wantExecuted(t, rig.call("c2", "bash", bashInput("true")))
	}},
	{"T11", "offline_hands_are_not_forwarded", func(t *testing.T, factory routeFactory) {
		rig := factory(t, map[string]string{"bash": "allow"})
		rig.handsOffline(true)
		wantRefused(t, rig.call("c1", "bash", bashInput("touch late.txt")), "environment_unavailable")
		if exists(rig, "late.txt") {
			t.Fatal("the call ran on offline hands")
		}
		// Not forwarded is safe to ask again: it was not recorded.
		rig.handsOffline(false)
		wantExecuted(t, rig.call("c1", "bash", bashInput("touch late.txt")))
		if !exists(rig, "late.txt") {
			t.Fatal("the retry did not run")
		}
	}},
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

func TestToolRouteConformance(t *testing.T) {
	registerToolRouteConformance(t, "local", newLocalRig, false)
	registerToolRouteConformance(t, "platform", newPlatformRig, true)
}

// The refusal text of the hands (T8) is the same on both routes.
func TestToolRouteHandsRefusalTextIsTheSame(t *testing.T) {
	var texts []string
	for _, f := range []routeFactory{newLocalRig, newPlatformRig} {
		rig := f(t, map[string]string{"write": "allow"})
		texts = append(texts, rig.call("c1", "write", map[string]any{"file_path": "../escape.txt", "content": "x"}).Text)
	}
	if texts[0] != texts[1] {
		t.Fatalf("local %q, platform %q", texts[0], texts[1])
	}
}
