//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// CoreOptions.Launch and ACPAgent: the agent's own bash commands start
// through the host's Launch hook, as process.start commands do.

func launchFixture(t *testing.T, launch LaunchFunc) (*acpFixture, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	core, err := NewCore(CoreOptions{WorkRoot: filepath.Join(t.TempDir(), "work"), Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	return startACPFixture(t, ctx, ACPAgentOptions{Core: core}), ctx
}

func allowBash(p *TurnStartParams) { p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`} }

func TestACPAgentBashStartsThroughLaunch(t *testing.T) {
	recorder := &launchRecorder{launch: func(req LaunchRequest) (LaunchSpec, error) {
		// Wrap the command the way a sandbox wrapper (sandbox-exec, bwrap) does.
		argv := append([]string{"sh", "-c", `echo wrapped > launched.txt; exec "$@"`, "wrapper"}, req.Argv...)
		return LaunchSpec{Argv: argv}, nil
	}}
	f, ctx := launchFixture(t, recorder.hook)
	f.start(t, ctx, "sesn_k1", "turn_1", "bypass", allowBash)
	if got := reported(t, f, "turn_1"); len(got) != 1 || !strings.HasSuffix(got[0], "|false|executed|") {
		t.Fatalf("answer = %q", got)
	}
	workspace := filepath.Join(f.root, "acp", "sesn_k1")
	for _, name := range []string{"launched.txt", "pwned.txt"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("%s: %v (the wrapped command did not run)", name, err)
		}
	}
	calls := recorder.calls()
	if len(calls) != 1 {
		t.Fatalf("launch calls = %+v", calls)
	}
	req := calls[0]
	if req.Tool != ToolNameBash || req.ToolCallID != "call_b" || req.SessionID != "sesn_k1" || req.AgentSessionID != "sesn_k1" ||
		req.ProcessID != "" || req.PackageInstall != nil || len(req.Env) == 0 ||
		!slices.Equal(req.Argv, shellArgv("echo pwned > pwned.txt")) {
		t.Fatalf("launch request = %+v", req)
	}
	if real, _ := filepath.EvalSymlinks(workspace); req.DefaultCwd != real {
		t.Fatalf("default cwd = %q, want %q", req.DefaultCwd, real)
	}
}

func TestACPAgentBashRefusedByLaunchDoesNotRun(t *testing.T) {
	recorder := &launchRecorder{launch: func(LaunchRequest) (LaunchSpec, error) {
		return LaunchSpec{}, NewReasonError(CodeForbidden, ReasonWorkspaceBoundary, "outside the granted folder")
	}}
	f, ctx := launchFixture(t, recorder.hook)
	f.start(t, ctx, "sesn_k2", "turn_1", "bypass", allowBash)
	got := reported(t, f, "turn_1")
	if len(got) != 1 || got[0] != "workspace_boundary: outside the granted folder|true|not_forwarded|workspace_boundary" {
		t.Fatalf("answer = %q", got)
	}
	assertNotRun(t, f, "sesn_k2", "pwned.txt")
	if len(recorder.calls()) != 1 {
		t.Fatalf("launch calls = %+v", recorder.calls())
	}
}
