//go:build !windows

package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// The example hand against the SDK's Harness: it registers its labels and
// remote_brain (off unless the owner asks), and runs the platform's tool calls
// in the session's workspace.

func connect(t *testing.T, labels []string, remoteBrain bool) (*bmaexecutor.Harness, *bmaexecutor.Executor, string) {
	t.Helper()
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{})
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)
	workRoot := t.TempDir()
	h, err := newHand(bmaexecutor.Options{
		EnvironmentID: "env_local", TokenSource: bmaexecutor.StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), ""),
		Labels: labels, RemoteBrain: remoteBrain,
	}, workRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := h.client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.client.Stop(context.Background(), "test done") })
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return harness, executor, workRoot
}

func TestHandRegistersLabelsAndTheOwnersRemoteBrainChoice(t *testing.T) {
	_, executor, _ := connect(t, []string{"team-a", "gpu"}, true)
	var capabilities bmaexecutor.Capabilities
	if err := executor.Capabilities(&capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.RemoteBrain == nil || !capabilities.RemoteBrain.Enabled {
		t.Fatalf("remote_brain = %+v, want enabled", capabilities.RemoteBrain)
	}
	if capabilities.Process == nil || !capabilities.Process.Enabled || capabilities.Shell == nil || capabilities.Files == nil {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if got := strings.Join(executor.Info().Labels, ","); got != "team-a,gpu" {
		t.Fatalf("labels = %q", got)
	}
}

// The default is closed: nothing in the platform can open it.
func TestHandIsClosedToRemoteBrainsByDefault(t *testing.T) {
	_, executor, _ := connect(t, nil, false)
	var capabilities bmaexecutor.Capabilities
	if err := executor.Capabilities(&capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.RemoteBrain != nil {
		t.Fatalf("remote_brain = %+v, want absent", capabilities.RemoteBrain)
	}
	if len(executor.Info().Labels) != 0 {
		t.Fatalf("labels = %v", executor.Info().Labels)
	}
}

func TestHandRunsToolsInTheSessionWorkspace(t *testing.T) {
	_, executor, workRoot := connect(t, nil, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{SessionID: "sesn_hand"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	call := func(tool string, input bmaexecutor.ToolInput) string {
		t.Helper()
		result, err := session.CallTool(ctx, bmaexecutor.ToolCallParams{Tool: tool, Input: bmaexecutor.NewToolInput(input)})
		if err != nil || result.IsError {
			t.Fatalf("%s: %+v %v", tool, result, err)
		}
		var texts []string
		for _, block := range result.Content {
			texts = append(texts, block.Text)
		}
		return strings.Join(texts, "\n")
	}
	call(bmaexecutor.ToolNameWrite, bmaexecutor.ToolInput{Path: "hello.txt", Content: "from the brain\n"})
	if got := call(bmaexecutor.ToolNameRead, bmaexecutor.ToolInput{Path: "hello.txt"}); got != "from the brain\n" {
		t.Fatalf("read = %q", got)
	}
	if got := call(bmaexecutor.ToolNameBash, bmaexecutor.ToolInput{Command: "pwd"}); !strings.Contains(got, workRoot) || !strings.Contains(got, "sesn_hand") {
		t.Fatalf("pwd = %q, want a workspace of the session under %s", got, workRoot)
	}
}
