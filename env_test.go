//go:build !windows

package bmaexecutor

import (
	"os"
	"strings"
	"testing"
)

// TestCoreDenyEnvHidesExecutorPrivateVariables: DenyEnv names never reach a
// bash tool, a process or a stdio MCP server, whether inherited, set by
// CoreOptions.Env or requested.
func TestCoreDenyEnvHidesExecutorPrivateVariables(t *testing.T) {
	requireUnix(t)
	t.Setenv("BMA_MACHINE_TOKEN", "machine-secret")
	t.Setenv("BMA_HOME", "/private/bma-home")
	ctx := testContext(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	core := newTestCore(t, CoreOptions{
		Env: map[string]string{"VISIBLE": "yes"},
		DenyEnv: func(name string) bool {
			return strings.EqualFold(name, "BMA_MACHINE_TOKEN") || strings.EqualFold(name, "BMA_HOME")
		},
	})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{})); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_env", Lease: Lease{Token: "lease"}, MCPServers: []MCPServerConfig{{
		ServerKey: "env", ConnectionOrigin: MCPConnectionOriginEnvironment,
		Transport: MCPServerTransport{Type: MCPTransportTypeStdio, Command: executable, Env: map[string]string{"BMA_TEST_MCP_SERVER": "1"}},
	}}})
	requireHidden := func(where, env string) {
		t.Helper()
		if strings.Contains(env, "BMA_MACHINE_TOKEN") || strings.Contains(env, "machine-secret") ||
			strings.Contains(env, "BMA_HOME") || strings.Contains(env, "/private/bma-home") {
			t.Fatalf("%s env leaks executor-private variables:\n%s", where, env)
		}
	}

	bash, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "env"})})
	if err != nil || bash.IsError || !strings.Contains(textOf(bash), "VISIBLE=yes") {
		t.Fatalf("bash env = %+v err %v", textOf(bash), err)
	}
	requireHidden("bash", textOf(bash))
	requested, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "env", Env: map[string]string{"bma_home": "/x"}})})
	if err != nil || requested.Outcome != ToolOutcomeFailed {
		t.Fatalf("bash with denied request env = %+v err %v", textOf(requested), err)
	}

	var state ProcessState
	err = b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{SessionID: session.ID(), ProcessID: "env", Argv: []string{"env"}, WaitMS: 5000}, &state)
	if err != nil || state.State != ProcessExited {
		t.Fatalf("process env = %+v err %v", state, err)
	}
	requireHidden("process", readAll(t, b.executor, session.ID(), "env"))
	err = b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{SessionID: session.ID(), ProcessID: "denied", Argv: []string{"env"}, Env: map[string]string{"BMA_MACHINE_TOKEN": "x"}}, &state)
	requireCode(t, err, CodeInvalidParams)

	channel, err := session.MCP(ctx, "env")
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"env"}}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := channel.Read(ctx)
	if err != nil || !strings.Contains(string(frame), "PATH=") {
		t.Fatalf("mcp env = %s err %v", frame, err)
	}
	requireHidden("stdio MCP", string(frame))
}

func TestNewCoreRejectsDeniedEnv(t *testing.T) {
	_, err := NewCore(CoreOptions{WorkRoot: t.TempDir(), Env: map[string]string{"BMA_HOME": "/x"},
		DenyEnv: func(name string) bool { return name == "BMA_HOME" }})
	if err == nil {
		t.Fatal("NewCore accepted an Env entry DenyEnv denies")
	}
}

// TestProcessOptionsEnvReachesProcessesOnly: ProcessOptions.Env is set on
// every process, over the request env, and never on a bash tool.
func TestProcessOptionsEnvReachesProcessesOnly(t *testing.T) {
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{Env: map[string]string{"SKILL_ROOT": "/canonical"}})); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_penv", Lease: Lease{Token: "lease"}})
	var state ProcessState
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: session.ID(), ProcessID: "p", Argv: []string{"sh", "-c", `printf %s "$SKILL_ROOT"`},
		Env: map[string]string{"SKILL_ROOT": "/override"}, WaitMS: 5000,
	}, &state); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b.executor, session.ID(), "p"); got != "/canonical" {
		t.Fatalf("process SKILL_ROOT = %q, want /canonical", got)
	}
	bash, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: `printf '[%s]' "$SKILL_ROOT"`})})
	if err != nil || !strings.HasPrefix(textOf(bash), "[]") {
		t.Fatalf("bash SKILL_ROOT = %q err %v", textOf(bash), err)
	}
}
