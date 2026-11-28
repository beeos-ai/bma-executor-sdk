package bmaexecutor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Only a session with work in flight holds a slot (CoreOptions.MaxSessions):
// a full core releases its least recently used idle session without
// session.ended, keeps its workspace, and the harness's next session.assign
// opens it again. A running process or a tool call in flight keeps the slot;
// when every session is busy, session.assign is executor_busy.
func TestCoreReleasesIdleSessionsWhenFull(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{MaxSessions: 1})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{})); err != nil {
		t.Fatal(err)
	}
	b, _ := attachInProcess(t, core, "cloud", nil)
	first := readySession(t, b, SessionAssignParams{SessionID: "sess_first", Lease: Lease{Token: "lease"}})
	if result, err := first.CallTool(ctx, ToolCallParams{Tool: ToolNameWrite, Input: NewToolInput(ToolInput{Path: "kept", Content: "first"})}); err != nil || result.IsError {
		t.Fatalf("write = %+v err %v", result, err)
	}
	wantBusy := func(what string, err error) {
		t.Helper()
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeExecutorBusy {
			t.Fatalf("%s: err = %v, want executor_busy", what, err)
		}
	}

	// A running process keeps the session's slot.
	var state ProcessState
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: first.ID(), ProcessID: "p_sleep", Argv: []string{"sleep", "30"},
	}, &state); err != nil {
		t.Fatal(err)
	}
	pid := state.PID
	_, err := b.executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_second", Lease: Lease{Token: "lease"}})
	wantBusy("assign while a process runs", err)
	if err := b.executor.Call(ctx, MethodProcessTerminate, ProcessSignalParams{SessionID: first.ID(), ProcessID: "p_sleep"}, &state); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !processAlive(pid) })
	waitFor(t, func() bool {
		var list ProcessListResult
		_ = b.executor.Call(ctx, MethodProcessList, ProcessListParams{SessionID: first.ID()}, &list)
		return len(list.Processes) == 1 && list.Processes[0].State != "running"
	})

	// Idle now: the next session takes its slot; the first session is not
	// ended for the harness.
	second := readySession(t, b, SessionAssignParams{SessionID: "sess_second", Lease: Lease{Token: "lease"}})
	select {
	case <-first.Done():
		t.Fatalf("released session ended: %s", first.EndReason())
	default:
	}
	core.mu.Lock()
	got := len(core.sessions)
	core.mu.Unlock()
	if got != 1 {
		t.Fatalf("core sessions = %d, want 1", got)
	}

	// The released session's next call assigns it again (releasing the idle
	// second one) and finds its workspace as it was.
	result, err := first.CallTool(ctx, ToolCallParams{Tool: ToolNameRead, Input: NewToolInput(ToolInput{Path: "kept"})})
	if err != nil || textOf(result) != "first" {
		t.Fatalf("read after release = %q err %v", textOf(result), err)
	}

	// A tool call in flight keeps the slot: the second session cannot come back.
	started := filepath.Join(core.workspace("cloud", first.ID()), "started")
	done := make(chan ToolResultParams, 1)
	go func() {
		result, _ := first.CallTool(ctx, ToolCallParams{ToolCallID: "call_hold", Tool: ToolNameBash,
			Input: NewToolInput(ToolInput{Command: "touch started; sleep 30"})})
		done <- result
	}()
	waitFor(t, func() bool { _, err := os.Stat(started); return err == nil })
	_, err = second.CallTool(ctx, ToolCallParams{Tool: ToolNameRead, Input: NewToolInput(ToolInput{Path: "kept"})})
	wantBusy("a call of a released session while the other runs a tool", err)
	if _, err := first.CancelTool(ctx, "call_hold", "test"); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.Outcome != ToolOutcomeCanceled {
		t.Fatalf("held call = %+v", result)
	}
	if result, err := second.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "true"})}); err != nil || result.IsError {
		t.Fatalf("second after the call ended = %+v err %v", textOf(result), err)
	}
}
