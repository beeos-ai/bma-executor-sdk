//go:build !windows

package bmaexecutor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

const testSecretValue = "s3cr3t-value-9f2c"

// TestStdioMCPSecretEnvReachesOnlyTheServer: transport.secret_env (values the
// server resolved from secret: references) is advertised, set in the stdio
// MCP server's environment, and nowhere else: not in its argv, not in the
// session's bash tools or processes, not in the core's logs.
func TestStdioMCPSecretEnvReachesOnlyTheServer(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var logsMu sync.Mutex
	var logs strings.Builder
	core := newTestCore(t, CoreOptions{OnLog: func(event LogEvent) {
		logsMu.Lock()
		fmt.Fprintf(&logs, "%s %v\n", event.Message, event.Err)
		logsMu.Unlock()
	}})
	if mcp := core.Capabilities().MCP; mcp == nil || !mcp.SecretEnv {
		t.Fatalf("capabilities.mcp = %+v, want secret_env advertised", mcp)
	}
	if err := core.Register(NewProcessCapability(core, ProcessOptions{})); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_mcp_secret", Lease: Lease{Token: "lease"}, MCPServers: []MCPServerConfig{{
		ServerKey: "env", ConnectionOrigin: MCPConnectionOriginEnvironment,
		Transport: MCPServerTransport{Type: MCPTransportTypeStdio, Command: executable,
			Env:       map[string]string{"BMA_TEST_MCP_SERVER": "1"},
			SecretEnv: map[string]string{"API_TOKEN": testSecretValue}},
	}}})
	channel, err := session.MCP(ctx, "env")
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"env"}}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := channel.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &response); err != nil || len(response.Result.Content) != 1 {
		t.Fatalf("mcp env frame = %s err %v", frame, err)
	}
	if !strings.Contains(response.Result.Content[0].Text, "API_TOKEN="+testSecretValue) {
		t.Fatalf("stdio MCP env = %q, want API_TOKEN from secret_env", response.Result.Content[0].Text)
	}

	bash, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: "env; ps -eo args"})})
	if err != nil || bash.IsError {
		t.Fatalf("bash = %+v err %v", textOf(bash), err)
	}
	if strings.Contains(textOf(bash), testSecretValue) || strings.Contains(textOf(bash), "API_TOKEN") {
		t.Fatalf("the secret reached the session's bash env or a process argv:\n%s", textOf(bash))
	}
	logsMu.Lock()
	defer logsMu.Unlock()
	if strings.Contains(logs.String(), testSecretValue) {
		t.Fatalf("core logs carry the secret:\n%s", logs.String())
	}
}

// TestAssignRejectsUnusableMCPSecretEnv: secret_env obeys the env rules
// (no denied or loader names), must not shadow an env name, and is for
// stdio servers only. Errors name the variable, never its value.
func TestAssignRejectsUnusableMCPSecretEnv(t *testing.T) {
	requireUnix(t)
	ctx := testContext(t)
	core := newTestCore(t, CoreOptions{DenyEnv: func(name string) bool { return name == "BMA_MACHINE_TOKEN" }})
	b := attachBrain(t, core, "local")
	for name, transport := range map[string]MCPServerTransport{
		"denied":    {Type: MCPTransportTypeStdio, Command: "/bin/true", SecretEnv: map[string]string{"BMA_MACHINE_TOKEN": testSecretValue}},
		"loader":    {Type: MCPTransportTypeStdio, Command: "/bin/true", SecretEnv: map[string]string{"LD_PRELOAD": testSecretValue}},
		"duplicate": {Type: MCPTransportTypeStdio, Command: "/bin/true", Env: map[string]string{"TOKEN": "x"}, SecretEnv: map[string]string{"TOKEN": testSecretValue}},
		"http":      {Type: MCPTransportTypeHttp, URL: "https://mcp.example.test", SecretEnv: map[string]string{"TOKEN": testSecretValue}},
	} {
		_, err := b.executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_" + name, Lease: Lease{Token: "lease"}, MCPServers: []MCPServerConfig{{
			ServerKey: "srv", ConnectionOrigin: MCPConnectionOriginEnvironment, Transport: transport,
		}}})
		rpcErr := requireCode(t, err, CodeInvalidParams)
		if !strings.Contains(rpcErr.Message, `"srv"`) || strings.Contains(rpcErr.Message, testSecretValue) {
			t.Fatalf("%s: message %q must name the server without the value", name, rpcErr.Message)
		}
	}
}
