//go:build windows

package bmaexecutor

import (
	"encoding/base64"
	"strings"
	"testing"
)

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
	err := w.executor.Call(testContext(t), MethodProcessStart, params, &state)
	return state, err
}

func (w winProc) read(t *testing.T, id, stream string, cursor int64, waitMS int64) ProcessReadResult {
	t.Helper()
	var result ProcessReadResult
	err := w.executor.Call(testContext(t), MethodProcessRead, ProcessReadParams{
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
		r := w.read(t, id, StreamStdout, cursor, 10000)
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
	result, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: command})})
	if err != nil || result.IsError || !strings.Contains(textOf(result), "[dollar] 6") {
		t.Fatalf("bash powershell = %q err %v", textOf(result), err)
	}
}

// TestWindowsProcessStartCmdLineVerbatim (G2): process.start with the
// platform's argv cmd.exe /d /s /c <command>.
func TestWindowsProcessStartCmdLineVerbatim(t *testing.T) {
	w := newWinProc(t, CoreOptions{}, ProcessOptions{})
	state, err := w.start(t, ProcessStartParams{
		ProcessID: "ps", WaitMS: 30000, Stdin: StdinClosed,
		Argv: []string{"cmd.exe", "/d", "/s", "/c", `powershell -NoProfile -NonInteractive -Command "$v = 7; Write-Output \"v=$v\""`},
	})
	if err != nil || state.State != ProcessExited || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("start = %+v err %v", state, err)
	}
	if out := w.output(t, "ps"); !strings.Contains(out, "v=7") {
		t.Fatalf("output = %q", out)
	}
}
