package bmaexecutor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startRejectingHarness serves h but answers the first reject MCP channel
// dials with 404, as a non-owner replica did in staging. It returns the
// harness and a counter of MCP channel dials.
func startRejectingHarness(t *testing.T, core *Core, name string, reject int64) (brain, *atomic.Int64) {
	t.Helper()
	harness := NewHarness(HarnessOptions{})
	var dials atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") == ChannelMCP && dials.Add(1) <= reject {
			http.NotFound(w, r)
			return
		}
		harness.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(Options{
		EnvironmentID: "local", Executor: ExecutorInfo{Name: "core-test"},
		Capabilities: core.Capabilities(), TokenSource: StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), ""),
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
	return brain{harness: harness, executor: executor}, &dials
}

func fastMCPRetry(t *testing.T) {
	t.Helper()
	base, ceiling := coreMCPRetryBase, coreMCPRetryMax
	coreMCPRetryBase, coreMCPRetryMax = 2*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { coreMCPRetryBase, coreMCPRetryMax = base, ceiling })
}

func fakeMCPServer(t *testing.T, key string) MCPServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return MCPServerConfig{ServerKey: key, ConnectionOrigin: MCPConnectionOriginEnvironment, Transport: MCPServerTransport{
		Type: MCPTransportTypeStdio, Command: executable, Env: map[string]string{"BMA_TEST_MCP_SERVER": "1"},
	}}
}

// The MCP channel keeps retrying with capped backoff past the old five-dial
// limit, so a channel the server rejects for a while still connects.
func TestCoreMCPChannelRetriesUntilItConnects(t *testing.T) {
	fastMCPRetry(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	b, dials := startRejectingHarness(t, core, "local", 12)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_mcp_retry", MCPServers: []MCPServerConfig{fakeMCPServer(t, "a")}})

	channel, err := session.MCP(ctx, "a")
	if err != nil {
		t.Fatalf("mcp after %d dials: %v", dials.Load(), err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := channel.Read(ctx)
	if err != nil || !strings.Contains(string(frame), `"serverInfo"`) {
		t.Fatalf("initialize = %s err %v", frame, err)
	}
	if got := dials.Load(); got != 13 {
		t.Fatalf("dials = %d, want 12 rejected + 1 accepted", got)
	}
}

// Ending the session stops the retries and the supervisor goroutine.
func TestCoreMCPRetryStopsWhenTheSessionEnds(t *testing.T) {
	fastMCPRetry(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	b, dials := startRejectingHarness(t, core, "local", 1<<40)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_mcp_stop", MCPServers: []MCPServerConfig{fakeMCPServer(t, "a")}})

	waitFor(t, func() bool { return dials.Load() > 8 })
	if err := session.Stop(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !supervisorRunning() })
	settled := dials.Load()
	time.Sleep(100 * time.Millisecond)
	if got := dials.Load(); got != settled {
		t.Fatalf("dials after stop went %d -> %d", settled, got)
	}
}

func supervisorRunning() bool {
	buf := make([]byte, 1<<20)
	return strings.Contains(string(buf[:runtime.Stack(buf, true)]), "superviseMCP")
}

func TestMCPRetryDelayIsCappedAndJittered(t *testing.T) {
	for failures := 1; failures <= 64; failures++ {
		ceiling := coreMCPRetryBase << min(failures-1, 20)
		if ceiling > coreMCPRetryMax {
			ceiling = coreMCPRetryMax
		}
		for range 20 {
			if got := mcpRetryDelay(failures); got < ceiling/2 || got > ceiling {
				t.Fatalf("delay(%d) = %s, want in [%s, %s]", failures, got, ceiling/2, ceiling)
			}
		}
	}
}

// A bare command name is resolved with the PATH the server is started with
// (transport.env), not the executor's own, so a server whose directory only
// the MCP env PATH lists still starts.
func TestCoreMCPBareCommandResolvesWithServerEnvPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("copies the test binary under a bare name")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const name = "bma-bare-mcp-server"
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath(name); err == nil {
		t.Fatalf("%s must not be on the executor PATH", name)
	}
	server := fakeMCPServer(t, "a")
	server.Transport.Command = name
	server.Transport.Env["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")

	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	b, _ := startRejectingHarness(t, core, "local", 0)
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_mcp_bare", MCPServers: []MCPServerConfig{server}})
	channel, err := session.MCP(ctx, "a")
	if err != nil {
		t.Fatalf("mcp: %v", err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := channel.Read(ctx)
	if err != nil || !strings.Contains(string(frame), `"serverInfo"`) {
		t.Fatalf("initialize = %s err %v", frame, err)
	}
}

func TestResolveMCPCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix executable bits")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	path := other + string(os.PathListSeparator) + dir

	if got, err := resolveMCPCommand("tool", path); err != nil || got != tool {
		t.Fatalf("bare = %q, %v; want %q", got, err, tool)
	}
	if got, err := resolveMCPCommand(tool, ""); err != nil || got != tool {
		t.Fatalf("absolute = %q, %v; want %q", got, err, tool)
	}
	for _, tc := range []struct{ name, command, path string }{
		{"not on PATH", "tool", other},
		{"not executable", "plain", dir},
		{"empty PATH", "tool", ""},
		{"empty entry is not the working directory", "tool", ":"},
		{"relative PATH entry", "tool", "."},
		{"relative path", "./tool", dir},
		{"relative subpath", "sub/tool", dir},
		{"backslash path", `sub\tool`, dir},
	} {
		if got, err := resolveMCPCommand(tc.command, tc.path); err == nil {
			t.Errorf("%s: resolved to %q, want an error", tc.name, got)
		}
	}
}
