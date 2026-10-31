//go:build !windows

package bmaexecutor

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// echoMCPServer is the TestMain fake stdio MCP server.
func echoMCPServer(t *testing.T) MCPServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return MCPServerConfig{ServerKey: "echo", ConnectionOrigin: MCPConnectionOriginEnvironment, Transport: MCPServerTransport{
		Type: MCPTransportTypeStdio, Command: executable, Env: map[string]string{"BMA_TEST_MCP_SERVER": "1"},
	}}
}

// requireMCPEcho initializes the session's echo MCP server through the
// harness end of the MCP channel and calls its echo tool.
func requireMCPEcho(t *testing.T, session *Session) {
	t.Helper()
	ctx := testContext(t)
	channel, err := session.MCP(ctx, "echo")
	if err != nil {
		t.Fatalf("session.MCP: %v", err)
	}
	for _, step := range []struct{ request, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, `"serverInfo"`},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"over-local"}}}`, "echo:over-local"},
	} {
		if err := channel.Write(ctx, []byte(step.request)); err != nil {
			t.Fatal(err)
		}
		frame, err := channel.Read(ctx)
		if err != nil || !strings.Contains(string(frame), step.want) {
			t.Fatalf("mcp %s = %s err %v", step.request, frame, err)
		}
	}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bmamcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func TestMCPChannelInProcess(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	b, _ := attachInProcess(t, core, "local", nil)
	requireMCPEcho(t, readySession(t, b, SessionAssignParams{SessionID: "sess_inproc", MCPServers: []MCPServerConfig{echoMCPServer(t)}}))
}

// The executor dials a harness on a Unix socket with a unix:// channel URL,
// as the TypeScript and Python SDKs do; the MCP channel follows it.
func TestMCPChannelOverUnixSocketURL(t *testing.T) {
	path := shortSocketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	harness := NewHarness(HarnessOptions{Authenticate: func(r *http.Request) (Principal, error) {
		if r.Header.Get("Authorization") != "Bearer local" {
			return Principal{}, os.ErrPermission
		}
		return Principal{}, nil
	}})
	server := &http.Server{Handler: harness}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	core := newTestCore(t, CoreOptions{})
	client, err := NewClient(Options{
		Executor: ExecutorInfo{Name: "unix-hand"}, Capabilities: core.Capabilities(),
		TokenSource: StaticToken("unix://"+path, "local"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Attach("local", client); err != nil {
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
	b := brain{harness: harness, executor: executor}
	requireMCPEcho(t, readySession(t, b, SessionAssignParams{SessionID: "sess_unix", MCPServers: []MCPServerConfig{echoMCPServer(t)}}))
}

// A local brain dials an executor listening on a Unix socket: the brain
// dials the session's MCP channels too, and again after the server restarts.
func TestMCPChannelWhenTheHarnessDialsTheExecutor(t *testing.T) {
	path := shortSocketPath(t)
	dial, closeListener, err := ListenUnixSocket(path, "local-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeListener() })
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
	session := readySession(t, brain{harness: harness, executor: executor}, SessionAssignParams{
		SessionID: "sess_dialback", Lease: Lease{Token: "lease"}, MCPServers: []MCPServerConfig{echoMCPServer(t)},
	})
	requireMCPEcho(t, session)

	// The executor restarts a dead server on a fresh channel; the brain
	// dials it again.
	channel, err := session.MCP(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"exit-now"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		current, err := session.MCP(ctx, "echo")
		return err == nil && current != channel
	})
	requireMCPEcho(t, session)
}
