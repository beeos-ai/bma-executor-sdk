//go:build windows

package bmaexecutor

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// winContext allows for a cold PowerShell start on a CI runner.
func winContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// winProc is a core with the process capability behind a loopback harness
// and one ready session, for the Windows execution tests.
type winProc struct {
	executor *Executor
	session  *Session
}

func newWinProc(t *testing.T, opts CoreOptions, process ProcessOptions) winProc {
	t.Helper()
	core := newTestCore(t, opts)
	if err := core.Register(NewProcessCapability(core, process)); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_win", Lease: Lease{Token: "lease"}})
	return winProc{executor: b.executor, session: session}
}

func (w winProc) start(t *testing.T, params ProcessStartParams) (ProcessState, error) {
	t.Helper()
	params.SessionID = w.session.ID()
	var state ProcessState
	err := w.executor.Call(winContext(t), MethodProcessStart, params, &state)
	return state, err
}

func (w winProc) read(t *testing.T, id, stream string, cursor int64, waitMS int64) ProcessReadResult {
	t.Helper()
	var result ProcessReadResult
	err := w.executor.Call(winContext(t), MethodProcessRead, ProcessReadParams{
		SessionID: w.session.ID(), ProcessID: id, Stream: stream, Cursor: cursor, WaitMS: waitMS,
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// output returns everything the process wrote to stdout once it ended.
func (w winProc) output(t *testing.T, id string) string {
	t.Helper()
	var out strings.Builder
	var cursor int64
	for {
		r := w.read(t, id, StreamStdout, cursor, 30000)
		raw, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(raw)
		cursor = r.NextCursor
		if r.EOF {
			return out.String()
		}
	}
}

// TestWindowsBashEnvHasSystemVariables (G1): a bash tool and a process get
// the variables Windows programs need, and %VAR% expands.
func TestWindowsBashEnvHasSystemVariables(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_env", Lease: Lease{Token: "lease"}})
	result, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "set"})})
	if err != nil || result.IsError {
		t.Fatalf("bash set = %+v err %v", textOf(result), err)
	}
	env := strings.ToUpper(textOf(result))
	for _, name := range []string{"SYSTEMROOT=", "WINDIR=", "COMSPEC=", "PATHEXT=", "TEMP=", "TMP=", "USERPROFILE=", "APPDATA=", "LOCALAPPDATA=", "PROGRAMFILES="} {
		if !strings.Contains(env, "\n"+name) {
			t.Errorf("bash env lacks %s:\n%s", name, env)
		}
	}
	for _, leaked := range []string{"GITHUB_TOKEN=", "RUNNER_TEMP="} {
		if strings.Contains(env, "\n"+leaked) {
			t.Errorf("bash env leaks %s", leaked)
		}
	}
}

// TestWindowsBashKeepsPowerShellDollarsAndQuotes (G2): the command reaches
// cmd.exe unmangled, so PowerShell sees its own $ and quotes.
func TestWindowsBashKeepsPowerShellDollarsAndQuotes(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_ps", Lease: Lease{Token: "lease"}})
	command := `powershell -NoProfile -NonInteractive -Command "$x = 'dollar'; Write-Output \"[$x] $($x.Length)\""`
	result, err := session.CallTool(winContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: command})})
	if err != nil || result.IsError || !strings.Contains(textOf(result), "[dollar] 6") {
		t.Fatalf("bash powershell = %q err %v", textOf(result), err)
	}
}

// TestWindowsProcessStartCmdLineVerbatim (G2): process.start with the
// platform's argv cmd.exe /d /s /c <command>.
func TestWindowsProcessStartCmdLineVerbatim(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	state, err := w.start(t, ProcessStartParams{
		ProcessID: "ps", WaitMS: 60000, Stdin: StdinClosed,
		Argv: []string{"cmd.exe", "/d", "/s", "/c", `powershell -NoProfile -NonInteractive -Command "$v = 7; Write-Output \"v=$v\""`},
	})
	if err != nil || state.State != ProcessExited || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("start = %+v err %v", state, err)
	}
	if out := w.output(t, "ps"); !strings.Contains(out, "v=7") {
		t.Fatalf("output = %q", out)
	}
}

// helperEnv makes the test binary run as the helper command (see runTestHelper).
func helperEnv(mode string, extra ...string) map[string]string {
	env := map[string]string{"BMA_TEST_HELPER": mode}
	for i := 0; i+1 < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	return env
}

// grandchildPID waits for the helper to publish its grandchild's pid.
func grandchildPID(t *testing.T, pidfile string) uint32 {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidfile); err == nil {
			pid, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
			if err != nil {
				t.Fatal(err)
			}
			return uint32(pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("helper did not publish its grandchild pid")
	return 0
}

func winProcessAlive(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	event, _ := windows.WaitForSingleObject(handle, 0)
	return event == uint32(windows.WAIT_TIMEOUT)
}

func requireDead(t *testing.T, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !winProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d is still running", pid)
}

func testExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestWindowsTerminateEndsTheProcessTree (G4).
func TestWindowsTerminateEndsTheProcessTree(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	_, err := w.start(t, ProcessStartParams{
		ProcessID: "tree", Argv: []string{testExecutable(t)}, Stdin: StdinClosed, Env: helperEnv("tree", "BMA_TEST_PIDFILE", pidfile),
	})
	if err != nil {
		t.Fatal(err)
	}
	child := grandchildPID(t, pidfile)
	if !winProcessAlive(child) {
		t.Fatalf("grandchild %d is not running", child)
	}
	var state ProcessState
	if err := w.executor.Call(winContext(t), MethodProcessTerminate,
		ProcessSignalParams{SessionID: w.session.ID(), ProcessID: "tree", GraceMS: ptr(int64(0))}, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != ProcessKilled || state.ExitSignal == nil || *state.ExitSignal != "SIGKILL" {
		t.Fatalf("terminate = %+v", state)
	}
	requireDead(t, child)
}

// TestWindowsTimeoutEndsTheProcessTree (G4).
func TestWindowsTimeoutEndsTheProcessTree(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	if _, err := w.start(t, ProcessStartParams{
		ProcessID: "slow", Argv: []string{testExecutable(t)}, Stdin: StdinClosed, TimeoutMS: 1500,
		Env: helperEnv("tree", "BMA_TEST_PIDFILE", pidfile),
	}); err != nil {
		t.Fatal(err)
	}
	child := grandchildPID(t, pidfile)
	r := w.read(t, "slow", StreamStdout, 0, 30000)
	if r.State != ProcessFailed || r.Reason != "timeout" {
		t.Fatalf("read = %+v", r)
	}
	requireDead(t, child)
}

// TestWindowsSessionEndEndsLeftoverProcesses (G4): a process that exited and
// left a child behind does not outlive its session.
func TestWindowsSessionEndEndsLeftoverProcesses(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	if _, err := w.start(t, ProcessStartParams{
		ProcessID: "orphan", Argv: []string{testExecutable(t)}, Stdin: StdinClosed, WaitMS: 30000,
		Env: helperEnv("orphan", "BMA_TEST_PIDFILE", pidfile),
	}); err != nil {
		t.Fatal(err)
	}
	child := grandchildPID(t, pidfile)
	if !winProcessAlive(child) {
		t.Fatalf("leftover child %d is not running", child)
	}
	if err := w.session.Stop(winContext(t), "done"); err != nil {
		t.Fatal(err)
	}
	requireDead(t, child)
}

// TestWindowsBashTimeoutEndsTheProcessTree (G4): the bash tool's timeout ends
// the command's descendants, not only cmd.exe.
func TestWindowsBashTimeoutEndsTheProcessTree(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_bash", Lease: Lease{Token: "lease"}})
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	result, err := session.CallTool(winContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{
		Command: `"` + testExecutable(t) + `"`, TimeoutMS: 3000,
		Env: helperEnv("tree", "BMA_TEST_PIDFILE", pidfile),
	})})
	if err != nil || !result.IsError {
		t.Fatalf("bash = %q err %v", textOf(result), err)
	}
	requireDead(t, grandchildPID(t, pidfile))
}

// TestWindowsInterruptReachesTheProcess (G5): process.interrupt is a console
// CTRL_BREAK that the process handles (the executor has no console of its own
// under a service or a CI runner; it creates a hidden one).
func TestWindowsInterruptReachesTheProcess(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	if _, err := w.start(t, ProcessStartParams{
		ProcessID: "int", Argv: []string{testExecutable(t)}, Stdin: StdinClosed, Env: helperEnv("interrupt"),
	}); err != nil {
		t.Fatal(err)
	}
	// The helper installs its handler before printing "ready".
	if r := w.read(t, "int", StreamStdout, 0, 30000); !strings.Contains(decodeData(t, r), "ready") {
		t.Fatalf("read = %+v", r)
	}
	var state ProcessState
	if err := w.executor.Call(winContext(t), MethodProcessInterrupt,
		ProcessSignalParams{SessionID: w.session.ID(), ProcessID: "int"}, &state); err != nil {
		t.Fatal(err)
	}
	final := w.read(t, "int", StreamStdout, 0, 30000)
	for final.State == ProcessRunning {
		final = w.read(t, "int", StreamStdout, final.NextCursor, 30000)
	}
	if final.State != ProcessExited || final.ExitCode == nil || *final.ExitCode != 0 {
		t.Fatalf("after interrupt: %+v", final)
	}
	if out := w.output(t, "int"); !strings.Contains(out, "interrupted") {
		t.Fatalf("output = %q", out)
	}
}

// TestWindowsTerminateIsGraceful (G5): terminate asks first (CTRL_BREAK), so a
// process that handles it exits by itself within the grace period.
func TestWindowsTerminateIsGraceful(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	if _, err := w.start(t, ProcessStartParams{
		ProcessID: "term", Argv: []string{testExecutable(t)}, Stdin: StdinClosed, Env: helperEnv("interrupt"),
	}); err != nil {
		t.Fatal(err)
	}
	if r := w.read(t, "term", StreamStdout, 0, 30000); !strings.Contains(decodeData(t, r), "ready") {
		t.Fatalf("read = %+v", r)
	}
	var state ProcessState
	if err := w.executor.Call(winContext(t), MethodProcessTerminate,
		ProcessSignalParams{SessionID: w.session.ID(), ProcessID: "term", GraceMS: ptr(int64(20000))}, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != ProcessExited || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("terminate = %+v", state)
	}
}

func decodeData(t *testing.T, r ProcessReadResult) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
