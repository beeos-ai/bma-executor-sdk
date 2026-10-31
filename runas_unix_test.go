//go:build !windows

package bmaexecutor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// workloadUser is an unprivileged identity distinct from the executor's.
var workloadUser = RunAs{UID: 65534, GID: 65534, Groups: []uint32{65533}, Home: "/workload-home", User: "workload"}

// TestCoreRunAsRunsEveryWorkloadAsTheWorkloadUser: with RunAs, bash, a pipe
// process, a PTY process and a stdio MCP server all run as the workload
// user (with its supplementary groups, HOME and USER), never as the executor.
// Changing uid needs root, so this runs where the executor would: as root.
func TestCoreRunAsRunsEveryWorkloadAsTheWorkloadUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another uid needs root")
	}
	ctx := testContext(t)
	root, err := os.MkdirTemp("", "runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The test binary doubles as the MCP server; go test keeps it in a
	// private directory the workload user cannot reach.
	self, err := os.ReadFile(must(os.Executable()))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "mcp-server")
	if err := os.WriteFile(executable, self, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "ws")
	runAs := workloadUser
	core := newTestCore(t, CoreOptions{WorkRoot: root, Workspace: func(string, string) string { return workspace }, RunAs: &runAs})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{})); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_runas", Lease: Lease{Token: "lease"}, MCPServers: []MCPServerConfig{{
		ServerKey: "id", ConnectionOrigin: MCPConnectionOriginEnvironment,
		Transport: MCPServerTransport{Type: MCPTransportTypeStdio, Command: executable, Env: map[string]string{"BMA_TEST_MCP_SERVER": "1"}},
	}}})
	info, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if owner := fileOwner(info); owner != "65534:65534" {
		t.Fatalf("created workspace owner = %s, want the workload user", owner)
	}

	const probe = `printf '%s|%s|%s|%s' "$(id -u)" "$(id -G)" "$HOME" "$USER"`
	want := "65534|65534 65533|/workload-home|workload"
	bash, err := session.CallTool(ctx, ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: probe})})
	if err != nil || bash.IsError || !strings.Contains(textOf(bash), want) {
		t.Fatalf("bash identity = %q err %v, want %q", textOf(bash), err, want)
	}
	for _, mode := range []string{"", IOModePTY} {
		id := "pipe"
		if mode == IOModePTY {
			id = "pty"
		}
		var state ProcessState
		if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
			SessionID: session.ID(), ProcessID: id, Argv: []string{"sh", "-c", probe}, IOMode: mode, WaitMS: 5000,
		}, &state); err != nil || state.State != ProcessExited {
			t.Fatalf("%s process = %+v err %v", id, state, err)
		}
		if got := readAll(t, b.executor, session.ID(), id); !strings.Contains(got, want) {
			t.Fatalf("%s process identity = %q, want %q", id, got, want)
		}
	}
	channel, err := session.MCP(ctx, "id")
	if err != nil {
		t.Fatal(err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"id"}}`)); err != nil {
		t.Fatal(err)
	}
	if frame, err := channel.Read(ctx); err != nil || !strings.Contains(string(frame), "uid:65534 gid:65534 user:workload logname:workload") {
		t.Fatalf("stdio MCP identity = %s err %v", frame, err)
	}
}

// TestCoreRunAsRefusesInProcessFileTools: file tools run inside the
// executor, as its user, so with RunAs they would bypass the boundary.
func TestCoreRunAsRefusesInProcessFileTools(t *testing.T) {
	ctx := testContext(t)
	// The executor's own ids: the refusal does not depend on privilege.
	runAs := RunAs{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	core := newTestCore(t, CoreOptions{RunAs: &runAs})
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_files", Lease: Lease{Token: "lease"}})
	for _, tool := range []string{ToolNameRead, ToolNameWrite, ToolNameEdit, ToolNameGlob, ToolNameGrep} {
		result, err := session.CallTool(ctx, ToolCallParams{Tool: tool, Input: NewToolInput(ToolInput{Path: "x", Content: "y", Pattern: "*"})})
		if err != nil || result.Outcome != ToolOutcomeFailed || !strings.Contains(textOf(result), "another OS user") {
			t.Fatalf("%s with RunAs = %+v err %v, want refused", tool, textOf(result), err)
		}
	}
}

func fileOwner(info os.FileInfo) string {
	uid, gid := ownerIDs(info)
	return fmt.Sprintf("%d:%d", uid, gid)
}

func ownerIDs(info os.FileInfo) (uint32, uint32) {
	stat := info.Sys().(*syscall.Stat_t)
	return stat.Uid, stat.Gid
}

func must(value string, err error) string {
	if err != nil {
		panic(err)
	}
	return value
}
