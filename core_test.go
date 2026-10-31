package bmaexecutor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as a minimal stdio MCP server when re-executed with
// BMA_TEST_MCP_SERVER=1, so the supervisor is tested against a real process.
func TestMain(m *testing.M) {
	if os.Getenv("BMA_TEST_MCP_SERVER") == "1" {
		serveFakeMCP()
		return
	}
	os.Exit(m.Run())
}

func serveFakeMCP() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == nil {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "0"}}
		case "tools/call":
			var params MCPToolsCallParams
			_ = json.Unmarshal(request.Params, &params)
			text := fmt.Sprintf("echo:%v pid:%d", params.Arguments["text"], os.Getpid())
			if params.Name == "env" {
				text = strings.Join(os.Environ(), "\n")
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		default:
			result = map[string]any{}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		fmt.Println(string(raw))
	}
}

type brain struct {
	harness  *Harness
	executor *Executor
}

// attachBrain starts a harness and connects a Client for it to core.
func attachBrain(t *testing.T, core *Core, name string) brain {
	t.Helper()
	harness := NewHarness(HarnessOptions{})
	wsURL := startHarness(t, harness)
	client, err := NewClient(Options{
		EnvironmentID: "local", Executor: ExecutorInfo{Name: "core-test"},
		Capabilities: core.Capabilities(), TokenSource: StaticToken(wsURL, ""),
	})
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
	return brain{harness: harness, executor: executor}
}

func newTestCore(t *testing.T, opts CoreOptions) *Core {
	t.Helper()
	if opts.WorkRoot == "" {
		opts.WorkRoot = t.TempDir()
	}
	core, err := NewCore(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	return core
}

func readySession(t *testing.T, b brain, params SessionAssignParams) *Session {
	t.Helper()
	ctx := testContext(t)
	session, err := b.executor.AssignSession(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return session
}

func ptr[T any](v T) *T { return &v }

func requireUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
}

func TestCoreToolsOverLoopback(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_tools"})

	call := func(tool string, input ToolInput) ToolResultParams {
		t.Helper()
		result, err := session.CallTool(ctx, ToolCallParams{Tool: tool, Input: NewToolInput(input)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := call(ToolNameBash, ToolInput{Command: "printf 'one\\ntwo\\n' > a.txt && pwd"}); result.IsError ||
		!strings.Contains(textOf(result), filepath.Join("local", "sess_tools")) {
		t.Fatalf("bash = %+v", textOf(result))
	}
	if result := call(ToolNameEdit, ToolInput{Path: "a.txt", OldString: "two", NewString: "2"}); result.IsError {
		t.Fatalf("edit = %s", textOf(result))
	}
	if got := textOf(call(ToolNameRead, ToolInput{Path: "a.txt", Offset: 2})); got != "2\n" {
		t.Fatalf("read = %q", got)
	}
	call(ToolNameWrite, ToolInput{Path: "sub/b.go", Content: "package b // needle\n"})
	if got := textOf(call(ToolNameGrep, ToolInput{Pattern: "needle", Glob: "*.go"})); got != "sub/b.go:1:package b // needle" {
		t.Fatalf("grep = %q", got)
	}
	if got := textOf(call(ToolNameGlob, ToolInput{Pattern: "sub/*.go"})); got != "sub/b.go" {
		t.Fatalf("glob = %q", got)
	}
	if result := call(ToolNameBash, ToolInput{Command: "exit 3"}); result.IsError || result.Outcome != ToolOutcomeSucceeded || !strings.Contains(textOf(result), "exit_code: 3") {
		t.Fatalf("nonzero exit = %+v", textOf(result))
	}
	if result := call(ToolNameRead, ToolInput{Path: "../../escape"}); result.Outcome != ToolOutcomeFailed ||
		!strings.Contains(textOf(result), "outside the session workspace") {
		t.Fatalf("escape = %+v", textOf(result))
	}
}

func TestCoreCancelAndStopKillProcessGroups(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	session := readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_kill"})

	// tool.cancel kills the shell and its child sleep.
	done := make(chan ToolResultParams, 1)
	go func() {
		result, _ := session.CallTool(ctx, ToolCallParams{ToolCallID: "call_sleep", Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "sleep 30 & echo $! > child.pid; wait"})})
		done <- result
	}()
	pidFile := filepath.Join(core.workspace("local", "sess_kill"), "child.pid")
	waitFor(t, func() bool { raw, _ := os.ReadFile(pidFile); return len(raw) > 0 })
	started := time.Now()
	if _, err := session.CancelTool(ctx, "call_sleep", "user"); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.Outcome != ToolOutcomeCanceled || time.Since(started) > 5*time.Second {
		t.Fatalf("cancel = %+v after %s", result, time.Since(started))
	}
	requireProcessGone(t, pidFile)

	// session.stop kills a running tool and ends the session.
	_ = os.Remove(pidFile)
	stopped := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "sleep 30 & echo $! > child.pid; wait"})})
		stopped <- err
	}()
	waitFor(t, func() bool { raw, _ := os.ReadFile(pidFile); return len(raw) > 0 })
	if err := session.Stop(ctx, "user_stop"); err != nil {
		t.Fatal(err)
	}
	requireProcessGone(t, pidFile)
	if err := <-stopped; err != nil && !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("call during stop err = %v", err)
	}
}

func requireProcessGone(t *testing.T, pidFile string) {
	t.Helper()
	raw, _ := os.ReadFile(pidFile)
	var pid int
	if _, err := fmt.Sscan(string(raw), &pid); err != nil {
		t.Fatalf("pid file %q: %v", raw, err)
	}
	waitFor(t, func() bool {
		process, err := os.FindProcess(pid)
		return err != nil || process.Signal(syscall.Signal(0)) != nil
	})
}

func TestCoreMultiBrainIsolationAndCapacity(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{MaxSessions: 2, MaxSessionsPerBrain: 1})
	cloud := attachBrain(t, core, "cloud")
	local := attachBrain(t, core, "local")
	third := attachBrain(t, core, "third")

	// The same session id in two brains is two isolated sessions.
	cloudSession := readySession(t, cloud, SessionAssignParams{SessionID: "sess_same"})
	localSession := readySession(t, local, SessionAssignParams{SessionID: "sess_same"})
	write := func(session *Session, text string) {
		if _, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameWrite, Input: NewToolInput(ToolInput{Path: "owner", Content: text})}); err != nil {
			t.Fatal(err)
		}
	}
	write(cloudSession, "cloud")
	write(localSession, "local")
	result, err := cloudSession.CallTool(ctx, ToolCallParams{Tool: ToolNameRead, Input: NewToolInput(ToolInput{Path: "owner"})})
	if err != nil || textOf(result) != "cloud" {
		t.Fatalf("cloud read = %q err %v", textOf(result), err)
	}

	// A brain cannot reach a session another brain assigned.
	cross := core.toolCall(ctx, "third", ToolCallParams{SessionID: "sess_same", ToolCallID: "x", Tool: ToolNameRead, Input: NewToolInput(ToolInput{Path: "owner"})})
	if cross.Outcome != ToolOutcomeFailed || !strings.Contains(textOf(cross), "session_not_active") {
		t.Fatalf("cross-brain call = %+v", textOf(cross))
	}

	// Per-brain capacity, then total capacity: both executor_busy.
	var rpcErr *RPCError
	if _, err := cloud.executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_second"}); !errors.As(err, &rpcErr) || rpcErr.Code != CodeExecutorBusy {
		t.Fatalf("per-brain capacity err = %v", err)
	}
	if _, err := third.executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_third"}); !errors.As(err, &rpcErr) || rpcErr.Code != CodeExecutorBusy {
		t.Fatalf("total capacity err = %v", err)
	}
	// Stopping one brain's session frees capacity for another brain only.
	if err := cloudSession.Stop(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	readySession(t, third, SessionAssignParams{SessionID: "sess_third"})
	if raw, _ := os.ReadFile(filepath.Join(core.workspace("local", "sess_same"), "owner")); string(raw) != "local" {
		t.Fatalf("local workspace = %q", raw)
	}
}

func TestCoreStdioMCPChannel(t *testing.T) {
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	b := attachBrain(t, core, "local")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := func(key string) MCPServerConfig {
		return MCPServerConfig{ServerKey: key, ConnectionOrigin: MCPConnectionOriginEnvironment, Transport: MCPServerTransport{
			Type: MCPTransportTypeStdio, Command: executable, Env: map[string]string{"BMA_TEST_MCP_SERVER": "1"},
		}}
	}
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_mcp", MCPServers: []MCPServerConfig{server("a"), server("b")}})

	roundTrip := func(key, request string) string {
		t.Helper()
		channel, err := session.MCP(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := channel.Write(ctx, []byte(request)); err != nil {
			t.Fatal(err)
		}
		frame, err := channel.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return string(frame)
	}
	if got := roundTrip("a", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); !strings.Contains(got, `"serverInfo"`) {
		t.Fatalf("initialize = %s", got)
	}
	a := roundTrip("a", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	b2 := roundTrip("b", `{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	if !strings.Contains(a, "echo:hi") || !strings.Contains(b2, `"id":"x"`) {
		t.Fatalf("tools/call a=%s b=%s", a, b2)
	}
	// (session, server_key) isolation: each key is its own server process.
	pidOf := func(frame string) string { return frame[strings.Index(frame, "pid:"):] }
	if pidOf(a) == pidOf(b2) {
		t.Fatalf("servers a and b share a process: %s %s", a, b2)
	}
	if err := session.Stop(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.MCP(ctx, "a"); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("mcp after stop err = %v", err)
	}
}
