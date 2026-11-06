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
