//go:build !windows

package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type launchRecorder struct {
	mu       sync.Mutex
	requests []LaunchRequest
	launch   func(LaunchRequest) (LaunchSpec, error)
}

func (r *launchRecorder) hook(_ context.Context, req LaunchRequest) (LaunchSpec, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return r.launch(req)
}

func (r *launchRecorder) calls() []LaunchRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func TestLaunchRewritesDirArgvAndEnv(t *testing.T) {
	folder, err := filepath.EvalSymlinks(t.TempDir()) // outside the session workspace
	if err != nil {
		t.Fatal(err)
	}
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		// Wrap the command the way a sandbox wrapper (sandbox-exec, bwrap) does.
		argv := append([]string{"sh", "-c", `echo wrapped; exec "$@"`, "wrapper"}, req.Argv...)
		return LaunchSpec{Dir: folder, Argv: argv, Env: append(slices.Clone(req.Env), "LAUNCH_ROOT="+folder)}, nil
	}}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{Launch: recorder.hook})
	state := h.mustStart(t, ProcessStartParams{
		ProcessID: "p_launch", Argv: []string{"sh", "-c", "pwd; echo $LAUNCH_ROOT $FOO"},
		Cwd: "/requested/elsewhere", Env: map[string]string{"FOO": "bar"},
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_1", "app_ref": "ws_7"}, WaitMS: 5000,
	})
	if state.State != ProcessExited || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("state = %+v", state)
	}
	if got, want := readAll(t, h.executor, h.session, "p_launch"), "wrapped\n"+folder+"\n"+folder+" bar\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("launch calls = %d", len(calls))
	}
	req := calls[0]
	if req.ProcessID != "p_launch" || req.SessionID != h.session || req.AgentSessionID != "sesn_1" ||
		req.Metadata["app_ref"] != "ws_7" || req.Cwd != "/requested/elsewhere" || req.DefaultCwd == "" ||
		!slices.Equal(req.Argv, []string{"sh", "-c", "pwd; echo $LAUNCH_ROOT $FOO"}) || !slices.Contains(req.Env, "FOO=bar") {
		t.Fatalf("launch request = %+v", req)
	}
	if info, err := os.Stat(req.DefaultCwd); err != nil || !info.IsDir() {
		t.Fatalf("default cwd %q: %v", req.DefaultCwd, err)
	}
}

func TestLaunchEmptySpecKeepsTheRequest(t *testing.T) {
	recorder := &launchRecorder{launch: func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{}, nil }}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{Launch: recorder.hook})
	h.mustStart(t, ProcessStartParams{ProcessID: "p_keep", Argv: []string{"sh", "-c", "echo $FOO"}, Env: map[string]string{"FOO": "kept"}, WaitMS: 5000})
	if got := readAll(t, h.executor, h.session, "p_keep"); got != "kept\n" {
		t.Fatalf("output = %q", got)
	}
	// Without a Dir from the hook the requested cwd is confined as before.
	_, err := h.start(t, ProcessStartParams{ProcessID: "p_escape", Argv: []string{"true"}, Cwd: ".."})
	requireCode(t, err, CodeInvalidParams)
}

func TestLaunchRejectsWithReason(t *testing.T) {
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		if req.ProcessID == "p_plain" {
			return LaunchSpec{}, errors.New("wrapper missing")
		}
		return LaunchSpec{}, NewReasonError(CodeForbidden, ReasonWorkspaceBoundary, "outside the granted folder")
	}}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{Launch: recorder.hook})
	_, err := h.start(t, ProcessStartParams{ProcessID: "p_denied", Argv: []string{"true"}})
	rpcErr := requireCode(t, err, CodeForbidden)
	if rpcErr.Data == nil || rpcErr.Data.Reason != "workspace_boundary" || !strings.Contains(rpcErr.Message, "outside the granted folder") {
		t.Fatalf("rejection = %+v data %+v", rpcErr, rpcErr.Data)
	}
	_, err = h.start(t, ProcessStartParams{ProcessID: "p_plain", Argv: []string{"true"}})
	if rpcErr := requireCode(t, err, CodeInternalError); rpcErr.Data == nil || rpcErr.Data.Reason != ReasonSpawnFailed {
		t.Fatalf("plain hook error = %+v", rpcErr)
	}
	var list ProcessListResult
	if err := h.executor.Call(testContext(t), MethodProcessList, ProcessListParams{SessionID: h.session}, &list); err != nil || len(list.Processes) != 0 {
		t.Fatalf("rejected starts left %+v (err %v)", list.Processes, err)
	}
	// A rejected process id can be started again once the hook allows it.
	recorder.launch = func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{}, nil }
	h.mustStart(t, ProcessStartParams{ProcessID: "p_denied", Argv: []string{"true"}, WaitMS: 5000})
}

func TestLaunchRunsOncePerProcessAndNotForTransfers(t *testing.T) {
	recorder := &launchRecorder{launch: func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{}, nil }}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{Launch: recorder.hook, FileTransfer: &fakeTransfer{}})
	params := ProcessStartParams{ProcessID: "p_once", Argv: []string{"true"}, WaitMS: 5000}
	h.mustStart(t, params)
	h.mustStart(t, params) // replay: same spec, no second launch
	h.mustStart(t, ProcessStartParams{ProcessID: "p_xfer", WaitMS: 5000,
		FileTransfer: &ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: "/tmp/x"}})
	if calls := recorder.calls(); len(calls) != 1 || calls[0].ProcessID != "p_once" {
		t.Fatalf("launch calls = %+v", calls)
	}
}

type fakeSessionTransfer struct {
	mu    sync.Mutex
	calls []ProcessContext
}

func (f *fakeSessionTransfer) TransferFile(context.Context, string, ProcessFileTransfer) (FileTransferResult, error) {
	return FileTransferResult{}, errors.New("TransferFile must not be called when TransferSessionFile exists")
}

func (f *fakeSessionTransfer) TransferSessionFile(_ context.Context, process ProcessContext, transfer ProcessFileTransfer) (FileTransferResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, process)
	f.mu.Unlock()
	return FileTransferResult{Path: transfer.LocalPath, Digest: "sha256:session"}, nil
}

func TestSessionFileTransfererGetsTheSession(t *testing.T) {
	transfer := &fakeSessionTransfer{}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{FileTransfer: transfer})
	state := h.mustStart(t, ProcessStartParams{ProcessID: "p_st", WaitMS: 5000,
		Metadata:     map[string]string{MetadataAgentSessionID: "sesn_9", "k": "v"},
		FileTransfer: &ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: "/tmp/y"}})
	if state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("state = %+v", state)
	}
	transfer.mu.Lock()
	defer transfer.mu.Unlock()
	if len(transfer.calls) != 1 || transfer.calls[0].ProcessID != "p_st" || transfer.calls[0].AgentSessionID != "sesn_9" ||
		transfer.calls[0].SessionID != h.session || transfer.calls[0].Metadata["k"] != "v" {
		t.Fatalf("session transfer calls = %+v", transfer.calls)
	}
}

func installParams(id string, packages ...string) ProcessStartParams {
	return ProcessStartParams{
		ProcessID: id, Stderr: StderrMerge, WaitMS: 5000,
		Metadata:       map[string]string{MetadataAgentSessionID: "sesn_pkg"},
		PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: packages},
	}
}

// A package install goes through Launch like a command: the hook sees the
// session, the install and the installer's fixed command, and may wrap it.
func TestLaunchWrapsAPackageInstall(t *testing.T) {
	installer := &fakeInstaller{}
	folder := t.TempDir()
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		argv := append([]string{"sh", "-c", `echo "wrapped $PWD $WRAP"; exec "$@"`, "sh"}, req.Argv...)
		return LaunchSpec{Dir: folder, Argv: argv, Env: append(slices.Clone(req.Env), "WRAP=yes")}, nil
	}}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{PackageInstaller: installer, Launch: recorder.hook})
	state := h.mustStart(t, installParams("pkg_wrap", "jq", "g++"))
	if state.State != ProcessExited || *state.ExitCode != 0 {
		t.Fatalf("install = %+v", state)
	}
	resolved, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(t, h.executor, h.session, "pkg_wrap")
	if !strings.Contains(out, "wrapped "+resolved+" yes\n") || !strings.HasSuffix(out, ":apt jq g++") {
		t.Fatalf("output = %q", out)
	}
	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("launch calls = %+v", calls)
	}
	req := calls[0]
	if req.PackageInstall == nil || req.PackageInstall.Manager != PackageManagerApt || !slices.Equal(req.PackageInstall.Packages, []string{"jq", "g++"}) {
		t.Fatalf("PackageInstall = %+v", req.PackageInstall)
	}
	if req.AgentSessionID != "sesn_pkg" || req.ProcessID != "pkg_wrap" || len(req.Argv) == 0 || len(req.Env) == 0 {
		t.Fatalf("request = %+v", req)
	}
	if got := installer.calls(); len(got) != 1 {
		t.Fatalf("installer calls = %+v", got)
	}
}

// A hook that refuses an install leaves no process and answers its reason; an
// empty spec keeps the installer's own command.
func TestLaunchRefusesAPackageInstall(t *testing.T) {
	installer := &fakeInstaller{}
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		if req.PackageInstall == nil {
			t.Errorf("a command reached the install hook: %+v", req)
		}
		return LaunchSpec{}, NewReasonError(CodeForbidden, ReasonSessionRejected, "installs are not allowed here")
	}}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{PackageInstaller: installer, Launch: recorder.hook})
	_, err := h.start(t, installParams("pkg_denied", "jq"))
	rpcErr := requireCode(t, err, CodeForbidden)
	if rpcErr.Data == nil || rpcErr.Data.Reason != ReasonSessionRejected {
		t.Fatalf("refusal = %+v data %+v", rpcErr, rpcErr.Data)
	}
	var list ProcessListResult
	if err := h.executor.Call(testContext(t), MethodProcessList, ProcessListParams{SessionID: h.session}, &list); err != nil || len(list.Processes) != 0 {
		t.Fatalf("refused install left %+v (err %v)", list.Processes, err)
	}
	recorder.launch = func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{}, nil }
	state := h.mustStart(t, installParams("pkg_denied", "jq"))
	if state.State != ProcessExited || *state.ExitCode != 0 {
		t.Fatalf("install after the hook allows it = %+v", state)
	}
	if got := readAll(t, h.executor, h.session, "pkg_denied"); !strings.HasSuffix(got, ":apt jq") {
		t.Fatalf("empty spec must keep the installer's command, output = %q", got)
	}
}

// A replayed install does not launch again; a relative or missing Dir keeps
// the installer's own directory rules.
func TestLaunchRunsOncePerPackageInstall(t *testing.T) {
	recorder := &launchRecorder{launch: func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{}, nil }}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{PackageInstaller: &fakeInstaller{}, Launch: recorder.hook})
	h.mustStart(t, installParams("pkg_once", "jq"))
	h.mustStart(t, installParams("pkg_once", "jq"))
	if calls := recorder.calls(); len(calls) != 1 {
		t.Fatalf("launch calls = %+v", calls)
	}
	recorder.launch = func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{Dir: "relative"}, nil }
	_, err := h.start(t, installParams("pkg_rel", "jq"))
	requireCode(t, err, CodeInternalError)
}

// CoreOptions.Launch: a bash tool.call goes through the hook like a
// process.start. It sees the session, the call and the shell argv, and its
// directory, argv and environment are what runs.
func TestCoreLaunchWrapsABashToolCall(t *testing.T) {
	ctx := testContext(t)
	folder, err := filepath.EvalSymlinks(t.TempDir()) // outside the session workspace
	if err != nil {
		t.Fatal(err)
	}
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		argv := append([]string{"sh", "-c", `echo wrapped; exec "$@"`, "wrapper"}, req.Argv...)
		return LaunchSpec{Dir: folder, Argv: argv, Env: append(slices.Clone(req.Env), "LAUNCH_ROOT="+folder)}, nil
	}}
	core := newTestCore(t, CoreOptions{Launch: recorder.hook})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_launch",
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_l", "app_ref": "ws_7"}})
	result, err := session.CallTool(ctx, ToolCallParams{ToolCallID: "call_1", Tool: ToolNameBash,
		Input: NewToolInput(ToolInput{Command: "pwd; echo $LAUNCH_ROOT $FOO", Env: map[string]string{"FOO": "bar"}})})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := textOf(result), "wrapped\n"+folder+"\n"+folder+" bar\n"; result.IsError || !strings.HasPrefix(got, want) {
		t.Fatalf("bash = %+v %q, want prefix %q", result, got, want)
	}
	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("launch calls = %+v", calls)
	}
	req := calls[0]
	if req.Tool != ToolNameBash || req.ToolCallID != "call_1" || req.ProcessID != "" || req.SessionID != "sess_launch" ||
		req.AgentSessionID != "sesn_l" || req.Metadata["app_ref"] != "ws_7" || req.Client == nil || req.Cwd != "" ||
		!slices.Equal(req.Argv, shellArgv("pwd; echo $LAUNCH_ROOT $FOO")) || !slices.Contains(req.Env, "FOO=bar") {
		t.Fatalf("launch request = %+v", req)
	}
	if info, err := os.Stat(req.DefaultCwd); err != nil || !info.IsDir() {
		t.Fatalf("default cwd %q: %v", req.DefaultCwd, err)
	}
	// File tools do not go through the hook.
	if result, err := session.CallTool(ctx, ToolCallParams{ToolCallID: "call_2", Tool: ToolNameWrite,
		Input: NewToolInput(ToolInput{Path: "a.txt", Content: "x"})}); err != nil || result.IsError {
		t.Fatalf("write = %+v, %v", result, err)
	}
	if calls := recorder.calls(); len(calls) != 1 {
		t.Fatalf("a file tool called Launch: %+v", calls)
	}
}

// A refused bash tool.call fails with the hook's reason and starts nothing.
func TestCoreLaunchRefusesABashToolCall(t *testing.T) {
	ctx := testContext(t)
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		if req.ToolCallID == "call_plain" {
			return LaunchSpec{}, errors.New("wrapper missing")
		}
		return LaunchSpec{}, NewReasonError(CodeForbidden, ReasonWorkspaceBoundary, "outside the granted folder")
	}}
	core := newTestCore(t, CoreOptions{Launch: recorder.hook})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_refuse"})
	call := func(id string) ToolResultParams {
		t.Helper()
		result, err := session.CallTool(ctx, ToolCallParams{ToolCallID: id, Tool: ToolNameBash,
			Input: NewToolInput(ToolInput{Command: "touch started"})})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := call("call_denied"); result.Outcome != ToolOutcomeFailed || !result.IsError ||
		textOf(result) != "workspace_boundary: outside the granted folder" {
		t.Fatalf("refused = %+v %q", result, textOf(result))
	}
	if result := call("call_plain"); result.Outcome != ToolOutcomeFailed ||
		!strings.HasPrefix(textOf(result), "spawn_failed: launch: wrapper missing") {
		t.Fatalf("plain hook error = %+v %q", result, textOf(result))
	}
	if _, err := os.Stat(filepath.Join(core.opts.WorkRoot, "local", "sess_refuse", "started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused command ran: %v", err)
	}
	recorder.launch = func(LaunchRequest) (LaunchSpec, error) { return LaunchSpec{Dir: "relative"}, nil }
	if result := call("call_rel"); result.Outcome != ToolOutcomeFailed || !strings.HasPrefix(textOf(result), "spawn_failed: ") {
		t.Fatalf("relative dir = %+v %q", result, textOf(result))
	}
}
