//go:build !windows

package bmaexecutor_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
	"github.com/beeos-ai/bma-executor-sdk/conformance"
)

// The tool route conformance suite (ADR-0029 §5, T1–T11) against both routes
// of the SDK. The suite lives in package conformance so that a brain or hands
// of your own can run the same cases (see examples/third-party-brain and
// examples/third-party-hand).

func TestToolRouteConformance(t *testing.T) {
	t.Run("local", func(t *testing.T) { conformance.RunToolRouteConformance(t, newLocalRig) })
	t.Run("platform", func(t *testing.T) { conformance.RunToolRouteConformance(t, conformance.PlatformFactory(nil)) })
}

// newLocalRig is the local route: a Core, and the turn state under a state
// directory that outlives the simulated restarts.
func newLocalRig(t *testing.T, toolPolicy map[string]string) *conformance.Rig {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()
	workspace := filepath.Join(root, "acp", conformance.SessionID)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		core   *bmaexecutor.Core
		route  *bmaexecutor.LocalToolRoute
		cancel context.CancelFunc
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		cancel()
		core.Close()
	})
	// start begins turn in a Core of "a process" (a new Core when fresh): nothing
	// in memory is shared with the Core before.
	start := func(turn string, fresh bool) {
		if fresh {
			if core != nil {
				core.Close()
			}
			var err error
			if core, err = bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: root}); err != nil {
				t.Fatal(err)
			}
		}
		if cancel != nil {
			cancel()
		}
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		var err error
		route, err = bmaexecutor.NewLocalToolRoute(bmaexecutor.LocalToolRouteOptions{Core: core, StateDir: stateDir,
			SessionID: conformance.SessionID, TurnID: turn, Workspace: workspace, ToolPolicy: toolPolicy, Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
	}
	start(conformance.TurnID, true)
	return &conformance.Rig{
		Workspace: workspace,
		Route:     func() bmaexecutor.ToolRoute { mu.Lock(); defer mu.Unlock(); return route },
		Approve: func(callID, tool string, input map[string]any, decision string) {
			mu.Lock()
			defer mu.Unlock()
			id := bmaexecutor.ApprovalID(conformance.SessionID, conformance.TurnID, callID)
			request := bmaexecutor.InteractionRequestParams{InteractionID: id, ToolName: tool, ToolInput: input}
			answer := bmaexecutor.InteractionResponseParams{InteractionID: id, Outcome: decision}
			if err := route.RecordApproval(callID, request, answer); err != nil {
				t.Fatal(err)
			}
		},
		MarkStarted: func(callID string) {
			mu.Lock()
			defer mu.Unlock()
			if err := bmaexecutor.MarkLocalCallStarted(route, callID); err != nil {
				t.Fatal(err)
			}
		},
		Restart:    func() { mu.Lock(); defer mu.Unlock(); start(conformance.TurnID, true) },
		Supersede:  func() { mu.Lock(); defer mu.Unlock(); start("turn_2", false) },
		CancelTurn: func() { mu.Lock(); defer mu.Unlock(); cancel() },
	}
}

// The hands' refusal text (T8) is the same on both routes.
func TestToolRouteHandsRefusalTextIsTheSame(t *testing.T) {
	var texts []string
	for _, f := range []conformance.Factory{newLocalRig, conformance.PlatformFactory(nil)} {
		rig := f(t, map[string]string{"write": "allow"})
		ctx := t.Context()
		got := rig.Route().Call(ctx, bmaexecutor.RouteCall{ID: "c1", Name: "write", Input: map[string]any{"file_path": "../escape.txt", "content": "x"}})
		texts = append(texts, got.Text)
	}
	if texts[0] != texts[1] {
		t.Fatalf("local %q, platform %q", texts[0], texts[1])
	}
}
