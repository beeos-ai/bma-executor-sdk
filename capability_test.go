package bmaexecutor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// attachInProcess connects a Client for core to a fresh harness over the
// in-process transport.
func attachInProcess(t *testing.T, core *Core, name string, configure func(*Options)) (brain, *Client) {
	t.Helper()
	return attachInProcessHarness(t, core, name, HarnessOptions{}, configure)
}

func attachInProcessHarness(t *testing.T, core *Core, name string, harnessOptions HarnessOptions, configure func(*Options)) (brain, *Client) {
	t.Helper()
	harness := NewHarness(harnessOptions)
	options := Options{
		Executor: ExecutorInfo{Name: "core-test"}, Capabilities: core.Capabilities(),
		TokenSource: StaticToken("pipe://", ""), Dial: InProcessDialer(harness, Principal{}),
	}
	if configure != nil {
		configure(&options)
	}
	client, err := NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Attach(name, client); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "done") })
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return brain{harness: harness, executor: executor}, client
}

type fakeTransfer struct{ calls atomic.Int32 }

func (f *fakeTransfer) TransferFile(_ context.Context, processID string, transfer ProcessFileTransfer) (FileTransferResult, error) {
	f.calls.Add(1)
	return FileTransferResult{Path: transfer.LocalPath, Digest: "sha256:" + processID}, nil
}

type fakeGit struct{ released atomic.Int32 }

func (f *fakeGit) Grant(processID string, env []string) ([]string, func(), error) {
	return append(env, "BMA_TEST_GRANT="+processID), func() { f.released.Add(1) }, nil
}

func readAll(t *testing.T, executor *Executor, sessionID, processID string) string {
	t.Helper()
	ctx := testContext(t)
	var out strings.Builder
	cursor := int64(0)
	for {
		var read ProcessReadResult
		if err := executor.Call(ctx, MethodProcessRead, ProcessReadParams{
			SessionID: sessionID, ProcessID: processID, Stream: StreamStdout, Cursor: cursor, WaitMS: 2000,
		}, &read); err != nil {
			t.Fatalf("process.read: %v", err)
		}
		data, _ := base64.StdEncoding.DecodeString(read.Data)
		out.Write(data)
		cursor = read.NextCursor
		if read.EOF {
			return out.String()
		}
	}
}

func TestProcessCapabilityOverInProcessTransport(t *testing.T) {
	requireUnix(t)
	transfer, git := &fakeTransfer{}, &fakeGit{}
	core := newTestCore(t, CoreOptions{Env: map[string]string{"BMA_HOME": "/bma-home"}})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{FileTransfer: transfer, GitCredentials: git})); err != nil {
		t.Fatal(err)
	}
	if capabilities := core.Capabilities(); capabilities.Process == nil || !capabilities.Process.PTY ||
		!capabilities.Process.FileTransfer || !capabilities.Process.GitCredentials {
		t.Fatalf("process capability = %+v", capabilities.Process)
	}
	b, _ := attachInProcess(t, core, "cloud", nil)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_proc", Lease: Lease{Token: "lease"}})
	ctx := testContext(t)

	// Pipes: env is baseEnv + request + CoreOptions.Env, plus the Git grant.
	var state ProcessState
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p_env", Argv: []string{"sh", "-c", "echo $BMA_HOME $FOO $BMA_TEST_GRANT"},
		Env: map[string]string{"FOO": "bar"}, GitCredentials: true, WaitMS: 5000,
	}, &state); err != nil {
		t.Fatal(err)
	}
	if state.State != ProcessExited || state.ExitCode == nil || *state.ExitCode != 0 {
		t.Fatalf("state = %+v", state)
	}
	if got := readAll(t, b.executor, session.ID(), "p_env"); got != "/bma-home bar p_env\n" {
		t.Fatalf("env output = %q", got)
	}
	if git.released.Load() != 1 {
		t.Fatal("Git grant was not released when the process ended")
	}

	// PTY: stdin goes to the terminal, output is merged.
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p_pty", Argv: []string{"sh", "-c", "read line; echo got:$line; tty >/dev/null && echo tty"},
		IOMode: IOModePTY,
	}, &state); err != nil {
		t.Fatal(err)
	}
	if state.Stderr != StderrMerge {
		t.Fatalf("pty state = %+v", state)
	}
	if err := b.executor.Call(ctx, MethodProcessWrite, ProcessWriteParams{
		SessionID: session.ID(), ProcessID: "p_pty", Data: base64.StdEncoding.EncodeToString([]byte("hi\n")),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b.executor, session.ID(), "p_pty"); !strings.Contains(got, "got:hi") || !strings.Contains(got, "tty") {
		t.Fatalf("pty output = %q", got)
	}

	// File transfer runs inside the executor as a process.
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p_ft", FileTransfer: &ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: "a.txt"},
		WaitMS: 5000,
	}, &state); err != nil {
		t.Fatal(err)
	}
	if state.FileTransfer == nil || state.FileTransfer.Digest != "sha256:p_ft" || transfer.calls.Load() != 1 {
		t.Fatalf("file transfer state = %+v", state)
	}

	// Idempotent start: the same spec returns the same process, another conflicts.
	var rpcErr *RPCError
	err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p_env", Argv: []string{"true"},
	}, &state)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeConflict || rpcErr.Data == nil || rpcErr.Data.Reason != "process_id_spec_mismatch" {
		t.Fatalf("conflicting start = %v", err)
	}
	var list ProcessListResult
	if err := b.executor.Call(ctx, MethodProcessList, ProcessListParams{SessionID: session.ID()}, &list); err != nil || len(list.Processes) != 3 {
		t.Fatalf("list = %+v err %v", list, err)
	}

	// An unknown session is session_not_active.
	err = b.executor.Call(ctx, MethodProcessList, ProcessListParams{SessionID: "sess_other"}, &list)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeSessionNotActive {
		t.Fatalf("unknown session = %v", err)
	}

	// session.stop terminates a running process before session.ended.
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p_sleep", Argv: []string{"sleep", "30"},
	}, &state); err != nil {
		t.Fatal(err)
	}
	pid := state.PID
	if err := session.Stop(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-ctx.Done():
		t.Fatal("session did not end")
	}
	waitFor(t, func() bool { return !processAlive(pid) })
}

func TestProcessCapabilityWithoutHooksRejectsTheirFields(t *testing.T) {
	requireUnix(t)
	core := newTestCore(t, CoreOptions{})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{})); err != nil {
		t.Fatal(err)
	}
	if p := core.Capabilities().Process; p.FileTransfer || p.GitCredentials {
		t.Fatalf("capability = %+v", p)
	}
	b, _ := attachInProcess(t, core, "cloud", nil)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_nohooks", Lease: Lease{Token: "lease"}})
	var rpcErr *RPCError
	err := b.executor.Call(testContext(t), MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p", Argv: []string{"true"}, GitCredentials: true,
	}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeCapabilityUnavailable || rpcErr.Data.Capability != "process.git_credentials" {
		t.Fatalf("git_credentials without a granter = %v", err)
	}
}

func TestToolCallRejectedBeforeAck(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	b, _ := attachInProcess(t, core, "cloud", nil)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_tools", Lease: Lease{Token: "lease"}})
	ctx := testContext(t)
	var rpcErr *RPCError
	err := b.executor.Call(ctx, MethodToolCall, ToolCallParams{SessionID: session.ID(), ToolCallID: "c1", Tool: ToolNameComputerAction}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeCapabilityUnavailable || rpcErr.Data.Capability != "tool:computer_action" {
		t.Fatalf("unsupported tool = %v", err)
	}
	err = b.executor.Call(ctx, MethodToolCall, ToolCallParams{SessionID: "sess_missing", ToolCallID: "c2", Tool: ToolNameBash}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeSessionNotActive {
		t.Fatalf("unknown session = %v", err)
	}
}

func TestEndAfterToolCallEndsOneShotSessions(t *testing.T) {
	requireUnix(t)
	core := newTestCore(t, CoreOptions{EndAfterToolCall: func(p SessionAssignParams) bool { return p.Metadata["source"] != "harness" }})
	b, _ := attachInProcess(t, core, "cloud", nil)
	ctx := testContext(t)
	work := readySession(t, b, SessionAssignParams{SessionID: "sess_work", Lease: Lease{Token: "lease"}})
	if result, err := work.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "echo queued"})}); err != nil ||
		!strings.Contains(textOf(result), "queued") {
		t.Fatalf("result = %+v err %v", result, err)
	}
	select {
	case <-work.Done():
		if work.EndReason() != "completed" {
			t.Fatalf("end reason = %q", work.EndReason())
		}
	case <-ctx.Done():
		t.Fatal("one-shot session did not end after its tool call")
	}
	harnessSession := readySession(t, b, SessionAssignParams{SessionID: "sess_h", Lease: Lease{Token: "lease"}, Metadata: map[string]string{"source": "harness"}})
	if _, err := harnessSession.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "true"})}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-harnessSession.Done():
		t.Fatal("harness session ended after one tool call")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBashLargeOutputIsTruncatedWithoutArtifacts(t *testing.T) {
	requireUnix(t)
	core := newTestCore(t, CoreOptions{InlineResultBytes: 1024})
	b, _ := attachInProcess(t, core, "cloud", nil)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_big", Lease: Lease{Token: "lease"}})
	result, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "head -c 5000 /dev/zero | tr '\\0' x"})})
	if err != nil {
		t.Fatal(err)
	}
	if text := textOf(result); len(text) > 1024 || !strings.Contains(text, "output truncated to fit the result; 5000 bytes total") {
		t.Fatalf("result = %q", text)
	}
}

// The executor listens on a local socket and a local brain dials it: the
// roles stay harness/executor whoever connects first.
func TestListenUnixSocketLetsALocalBrainDialTheExecutor(t *testing.T) {
	requireUnix(t)
	dir, err := os.MkdirTemp("", "bmaexec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "run", "executor.sock")
	dial, closeListener, err := ListenUnixSocket(path, "local-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeListener() })
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v err %v", info.Mode(), err)
	}

	core := newTestCore(t, CoreOptions{})
	client, err := NewClient(Options{
		Executor: ExecutorInfo{Name: "local-hand"}, Capabilities: core.Capabilities(),
		TokenSource: StaticToken("ws://local/", ""), Dial: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Attach("local", client); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	started := make(chan error, 1)
	go func() {
		_, err := client.Start(ctx)
		started <- err
	}()
	t.Cleanup(func() { _ = client.Stop(context.Background(), "done") })

	harness := NewHarness(HarnessOptions{})
	if err := harness.DialExecutor(ctx, path, "wrong"); err == nil {
		t.Fatal("a brain with the wrong token connected")
	}
	if err := harness.DialExecutor(ctx, path, "local-token"); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := readySession(t, brain{harness: harness, executor: executor}, SessionAssignParams{SessionID: "sess_local", Lease: Lease{Token: "lease"}})
	result, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "echo local"})})
	if err != nil || !strings.Contains(textOf(result), "local") {
		t.Fatalf("result = %+v err %v", result, err)
	}
}

func TestClientStopsOnReplacedAndReconnectsAfterDrain(t *testing.T) {
	var dials atomic.Int32
	harnessEnds := make(chan Conn, 4)
	client, err := NewClient(Options{
		Executor: ExecutorInfo{Name: "close-codes"}, TokenSource: StaticToken("pipe://", ""),
		ReconnectBaseDelay: time.Millisecond,
		Dial: func(context.Context, ExecutorTokenResponse, string) (Conn, error) {
			dials.Add(1)
			executorEnd, harnessEnd := Pipe(ProtocolV11)
			harnessEnds <- harnessEnd
			go answerRegister(harnessEnd)
			return executorEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	first := <-harnessEnds
	_ = first.Close(4005, "drained")
	second := <-harnessEnds // 4005 reconnects
	_ = second.Close(4004, "replaced")
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("client kept running after 4004")
	}
	if !errors.Is(client.Err(), ErrReplaced) || dials.Load() != 2 {
		t.Fatalf("err = %v dials = %d", client.Err(), dials.Load())
	}
}

// answerRegister plays a minimal harness: it answers executor.register.
func answerRegister(conn Conn) {
	for {
		raw, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var message rpcInbound
		if json.Unmarshal(raw, &message) != nil || message.ID == nil || message.Method != MethodExecutorRegister {
			continue
		}
		payload, _ := json.Marshal(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *message.ID, Result: ExecutorRegisterResult{
			ExecutorID: "exec_1", ConnectionID: "conn", Protocol: ProtocolV11, HeartbeatIntervalMS: 60000, LeaseTTLMS: 60000,
		}})
		_ = conn.Write(context.Background(), payload)
	}
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process.Signal(syscall.Signal(0)) == nil
}

// A client that stays disconnected past the harness lease gives up its
// sessions (v1.1 §3.9) and keeps reconnecting.
func TestClientDropsSessionsAfterLeaseExpiry(t *testing.T) {
	var dials atomic.Int32
	first := make(chan Conn, 1)
	client, err := NewClient(Options{
		Executor: ExecutorInfo{Name: "lease"}, TokenSource: StaticToken("pipe://", ""),
		ReconnectBaseDelay: 20 * time.Millisecond, MaxReconnectAttempts: -1,
		Dial: func(context.Context, ExecutorTokenResponse, string) (Conn, error) {
			if dials.Add(1) > 1 {
				return nil, errors.New("harness unreachable")
			}
			executorEnd, harnessEnd := Pipe(ProtocolV11)
			first <- harnessEnd
			go answerRegisterWithLease(harnessEnd, 50)
			return executorEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lost := make(chan []string, 1)
	client.onSessionsLost(func(ids []string) { lost <- ids })
	ctx := testContext(t)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "done") })
	if _, err := client.assignSession(SessionAssignParams{SessionID: "sess_lease"}); err != nil {
		t.Fatal(err)
	}
	_ = (<-first).Close(1011, "harness restarted")
	select {
	case ids := <-lost:
		if len(ids) != 1 || ids[0] != "sess_lease" || len(client.ActiveSessions()) != 0 {
			t.Fatalf("lost = %v active = %v", ids, client.ActiveSessions())
		}
	case <-ctx.Done():
		t.Fatal("sessions were kept past the lease")
	}
	select {
	case <-client.Done():
		t.Fatal("unlimited reconnects stopped")
	default:
	}
}

func answerRegisterWithLease(conn Conn, leaseMS int64) {
	for {
		raw, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var message rpcInbound
		if json.Unmarshal(raw, &message) != nil || message.ID == nil || message.Method != MethodExecutorRegister {
			continue
		}
		payload, _ := json.Marshal(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *message.ID, Result: ExecutorRegisterResult{
			ExecutorID: "exec_1", ConnectionID: "conn", Protocol: ProtocolV11, HeartbeatIntervalMS: 60000, LeaseTTLMS: leaseMS,
		}})
		_ = conn.Write(context.Background(), payload)
	}
}

// A 4004 that arrives while the executor re-registers stops it too.
func TestClientStopsOnReplacedDuringReregister(t *testing.T) {
	var dials atomic.Int32
	first := make(chan Conn, 1)
	client, err := NewClient(Options{
		Executor: ExecutorInfo{Name: "close-codes"}, TokenSource: StaticToken("pipe://", ""),
		ReconnectBaseDelay: time.Millisecond,
		Dial: func(context.Context, ExecutorTokenResponse, string) (Conn, error) {
			executorEnd, harnessEnd := Pipe(ProtocolV11)
			if dials.Add(1) == 1 {
				first <- harnessEnd
				go answerRegister(harnessEnd)
			} else {
				_ = harnessEnd.Close(4004, "replaced")
			}
			return executorEnd, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_ = (<-first).Close(4005, "drained")
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatalf("client kept running after 4004 during re-register (dials %d)", dials.Load())
	}
	if !errors.Is(client.Err(), ErrReplaced) || dials.Load() != 2 {
		t.Fatalf("err = %v dials = %d", client.Err(), dials.Load())
	}
}
