//go:build !windows

package bmaexecutor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// processHarness is a core with the process capability behind an in-process
// harness and one ready session.
type processHarness struct {
	executor *Executor
	session  string
	statuses chan ProcessStatusParams
}

func newProcessHarness(t *testing.T, opts CoreOptions, process ProcessOptions) processHarness {
	t.Helper()
	core := newTestCore(t, opts)
	if err := core.Register(NewProcessCapability(core, process)); err != nil {
		t.Fatal(err)
	}
	statuses := make(chan ProcessStatusParams, 256)
	b, _ := attachInProcessHarness(t, core, "cloud", HarnessOptions{
		OnProcessStatus: func(_ *Executor, status ProcessStatusParams) { statuses <- status },
	}, nil)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_p", Lease: Lease{Token: "lease"}})
	return processHarness{executor: b.executor, session: session.ID(), statuses: statuses}
}

func (h processHarness) start(t *testing.T, params ProcessStartParams) (ProcessState, error) {
	t.Helper()
	params.SessionID = h.session
	var state ProcessState
	err := h.executor.Call(testContext(t), MethodProcessStart, params, &state)
	return state, err
}

func (h processHarness) mustStart(t *testing.T, params ProcessStartParams) ProcessState {
	t.Helper()
	state, err := h.start(t, params)
	if err != nil {
		t.Fatalf("start %s: %v", params.ProcessID, err)
	}
	return state
}

func (h processHarness) read(t *testing.T, params ProcessReadParams) (ProcessReadResult, error) {
	t.Helper()
	params.SessionID = h.session
	if params.Stream == "" {
		params.Stream = StreamStdout
	}
	var result ProcessReadResult
	err := h.executor.Call(testContext(t), MethodProcessRead, params, &result)
	return result, err
}

func requireCode(t *testing.T, err error, code int) *RPCError {
	t.Helper()
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != code {
		t.Fatalf("error = %v, want %d", err, code)
	}
	return rpcErr
}

func TestStreamDropsOldestBytesAndReportsGap(t *testing.T) {
	var s stream
	s.append(make([]byte, ProcessBufferBytes))
	s.append([]byte("tail"))
	if s.earliest != 4 || s.next() != ProcessBufferBytes+4 {
		t.Fatalf("earliest = %d next = %d", s.earliest, s.next())
	}
	p := &process{id: "p", changed: make(chan struct{}), stdout: s, state: ProcessRunning}
	result, err := p.read(ProcessReadParams{Stream: StreamStdout, Cursor: 0, MaxBytes: 8}, nil)
	if err != nil || !result.Gap || result.NextCursor != 4 || result.Data != "" {
		t.Fatalf("gap read = %+v err = %v", result, err)
	}
	result, err = p.read(ProcessReadParams{Stream: StreamStdout, Cursor: 4, MaxBytes: 8}, nil)
	if err != nil || !result.Truncated || result.NextCursor != 12 {
		t.Fatalf("truncated read = %+v err = %v", result, err)
	}
	_, err = p.read(ProcessReadParams{Stream: StreamStdout, Cursor: s.next() + 1, MaxBytes: 8}, nil)
	requireCode(t, err, CodeInvalidParams)
}

func TestProcessStartRejectsUnsafeInput(t *testing.T) {
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	for name, params := range map[string]ProcessStartParams{
		"empty argv":  {ProcessID: "a"},
		"cwd escape":  {ProcessID: "b", Argv: []string{"true"}, Cwd: ".."},
		"denied env":  {ProcessID: "c", Argv: []string{"true"}, Env: map[string]string{"LD_PRELOAD": "x"}},
		"bad stdin":   {ProcessID: "d", Argv: []string{"true"}, Stdin: "maybe"},
		"bad wait_ms": {ProcessID: "e", Argv: []string{"true"}, WaitMS: maxWaitMS + 1},
	} {
		if _, err := h.start(t, params); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			requireCode(t, err, CodeInvalidParams)
		}
	}
	_, err := h.start(t, ProcessStartParams{ProcessID: "f", Argv: []string{"/nonexistent/missing-binary"}})
	if rpcErr := requireCode(t, err, CodeInternalError); rpcErr.Data == nil || rpcErr.Data.Reason != "spawn_failed" {
		t.Fatalf("spawn failure = %+v", rpcErr)
	}
	var list ProcessListResult
	if err := h.executor.Call(testContext(t), MethodProcessList, ProcessListParams{SessionID: h.session}, &list); err != nil || len(list.Processes) != 0 {
		t.Fatalf("failed starts left %+v (err %v)", list.Processes, err)
	}
}

func TestProcessTimeoutReportsFailedAndExplicitEnvOnly(t *testing.T) {
	t.Setenv("BMA_EXECUTOR_TEST_SECRET", "leak")
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	state := h.mustStart(t, ProcessStartParams{
		ProcessID: "env", Argv: []string{"sh", "-c", `printf '%s|%s' "$BMA_EXECUTOR_TEST_SECRET" "$EXPLICIT"`},
		Env: map[string]string{"EXPLICIT": "yes"}, WaitMS: 5000,
	})
	if state.State != ProcessExited || *state.ExitCode != 0 {
		t.Fatalf("env process = %+v", state)
	}
	if got := readAll(t, h.executor, h.session, "env"); got != "|yes" {
		t.Fatalf("stdout = %q: host env leaked or explicit env missing", got)
	}

	state = h.mustStart(t, ProcessStartParams{ProcessID: "slow", Argv: []string{"sleep", "30"}, TimeoutMS: 100, WaitMS: 5000})
	if state.State != ProcessFailed || state.Reason != processReasonTimeout || state.EndedAt == "" {
		t.Fatalf("timed-out process = %+v, want failed with reason timeout and ended_at", state)
	}
	if first := <-h.statuses; first.ProcessID != "env" || first.EndedAt == "" || first.Reason != "" {
		t.Fatalf("exited status notification = %+v, want ended_at and no reason", first)
	}
	status := <-h.statuses
	if status.ProcessID != "slow" || status.State != ProcessFailed || status.Reason != processReasonTimeout || status.EndedAt != state.EndedAt {
		t.Fatalf("status notification = %+v", status)
	}
}

func TestEndedProcessesKeepStateAndOutputAndDoNotCountAsRunning(t *testing.T) {
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	run := func(id string, argv ...string) ProcessState {
		state := h.mustStart(t, ProcessStartParams{ProcessID: id, Argv: argv, Stderr: StderrMerge, WaitMS: 5000})
		if state.State == ProcessRunning {
			t.Fatalf("%s still running", id)
		}
		return state
	}
	first := run("first", "sh", "-c", "echo out; echo err >&2; exit 3")
	if first.EndedAt == "" || first.Reason != "" || first.ExitCode == nil || *first.ExitCode != 3 {
		t.Fatalf("first = %+v, want exited 3 with ended_at", first)
	}
	// More ended processes than the running limit: none of them blocks a
	// new start, and the first one is still addressable.
	for i := range MaxProcessesPerSession + 4 {
		run(fmt.Sprintf("p%d", i), "true")
	}
	result, err := h.read(t, ProcessReadParams{ProcessID: "first", MaxBytes: maxReadBytes, WaitMS: 2000})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(result.Data)
	if !result.EOF || string(data) != "out\nerr\n" || result.State != ProcessExited || result.EndedAt != first.EndedAt {
		t.Fatalf("read after exit = %+v data %q", result, data)
	}
}

func TestEndedProcessRetentionIsBounded(t *testing.T) {
	table := &processTable{processes: map[string]*process{}}
	base := time.Now()
	ended := func(id string, at time.Duration, size int) *process {
		p := &process{id: id, changed: make(chan struct{}), exited: make(chan struct{}), state: ProcessExited, endedAt: base.Add(at)}
		close(p.exited)
		p.stdout.append(make([]byte, size))
		table.processes[id] = p
		return p
	}
	running := &process{id: "running", changed: make(chan struct{}), exited: make(chan struct{}), state: ProcessRunning}
	running.stdout.append(make([]byte, ProcessBufferBytes))
	table.processes["running"] = running
	oldest := ended("oldest", 0, ProcessBufferBytes)
	for i := range maxRetainedOutputBytes / ProcessBufferBytes {
		ended(fmt.Sprintf("e%d", i), time.Duration(i+1)*time.Millisecond, ProcessBufferBytes)
	}
	table.retainEndedLocked()
	if len(oldest.stdout.buf) != 0 || oldest.stdout.next() != ProcessBufferBytes {
		t.Fatalf("oldest output not released: len %d next %d", len(oldest.stdout.buf), oldest.stdout.next())
	}
	if _, ok := table.processes["oldest"]; !ok {
		t.Fatal("output budget must keep the process state")
	}
	if len(table.processes["e0"].stdout.buf) != ProcessBufferBytes || len(running.stdout.buf) != ProcessBufferBytes {
		t.Fatal("newer ended output or running output was released")
	}
	for i := range maxRetainedEnded {
		ended(fmt.Sprintf("n%d", i), time.Hour+time.Duration(i), 0)
	}
	table.retainEndedLocked()
	if _, ok := table.processes["oldest"]; ok {
		t.Fatal("oldest ended process kept past the count bound")
	}
	if _, ok := table.processes["running"]; !ok {
		t.Fatal("running process must never be evicted")
	}
	endedCount := 0
	for _, p := range table.processes {
		if !p.endedAt.IsZero() {
			endedCount++
		}
	}
	if endedCount != maxRetainedEnded {
		t.Fatalf("retained ended = %d, want %d", endedCount, maxRetainedEnded)
	}
}

func TestResolveCwdFollowsSymlinksOutOfRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	session := &CoreSession{workspace: root}
	if _, err := session.ResolveCwd("link"); err == nil {
		t.Fatal("symlink escaping the work root must be rejected")
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ResolveCwd("sub"); err != nil {
		t.Fatalf("subdirectory: %v", err)
	}
}

func TestPTYProcessRunsOnTerminalAndRejectsCloseStdin(t *testing.T) {
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	h.mustStart(t, ProcessStartParams{
		ProcessID: "pty", IOMode: IOModePTY, Stderr: StderrMerge,
		Argv: []string{"sh", "-c", `if [ -t 0 ] && [ -t 1 ]; then echo tty; fi; stty size; read x; echo "got:$x"`},
	})
	ctx := testContext(t)
	err := h.executor.Call(ctx, MethodProcessWrite, ProcessWriteParams{SessionID: h.session, ProcessID: "pty", CloseStdin: true}, nil)
	requireCode(t, err, CodeCapabilityUnavailable)
	if err := h.executor.Call(ctx, MethodProcessWrite, ProcessWriteParams{
		SessionID: h.session, ProcessID: "pty", Data: base64.StdEncoding.EncodeToString([]byte("hello\n")),
	}, nil); err != nil {
		t.Fatal(err)
	}
	output := readAll(t, h.executor, h.session, "pty")
	for _, want := range []string{"tty\r\n", "24 80\r\n", "got:hello\r\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("pty output %q lacks %q", output, want)
		}
	}
}

func TestPTYProcessInterruptReachesForegroundGroup(t *testing.T) {
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	h.mustStart(t, ProcessStartParams{
		ProcessID: "pty", IOMode: IOModePTY,
		Argv: []string{"sh", "-c", `trap 'echo got-int; exit 130' INT; echo armed; while :; do sleep 0.1; done`},
	})
	if result, err := h.read(t, ProcessReadParams{ProcessID: "pty", WaitMS: 5000}); err != nil || result.Data == "" {
		t.Fatalf("armed read = %+v err %v", result, err)
	}
	time.Sleep(100 * time.Millisecond)
	ctx := testContext(t)
	if err := h.executor.Call(ctx, MethodProcessInterrupt, ProcessSignalParams{SessionID: h.session, ProcessID: "pty"}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var list ProcessListResult
		_ = h.executor.Call(context.Background(), MethodProcessList, ProcessListParams{SessionID: h.session}, &list)
		return len(list.Processes) == 1 && list.Processes[0].State == ProcessExited &&
			list.Processes[0].ExitCode != nil && *list.Processes[0].ExitCode == 130
	})
}
