//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The exported local route (ADR-0029 S3b): its options, and the parts of its
// gate the conformance suite does not drive (the suite's rigs never present an
// approval id with the call).

func newLocalRoute(t *testing.T, mutate func(*LocalToolRouteOptions)) (*LocalToolRoute, LocalToolRouteOptions) {
	t.Helper()
	root := t.TempDir()
	core, err := NewCore(CoreOptions{WorkRoot: root})
	noErr(t, err)
	t.Cleanup(core.Close)
	workspace := filepath.Join(root, "ws")
	noErr(t, os.MkdirAll(workspace, 0o755))
	opts := LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: rigSession, TurnID: rigTurn, Workspace: workspace,
		ToolPolicy: map[string]string{"bash": "ask"}}
	if mutate != nil {
		mutate(&opts)
	}
	route, err := NewLocalToolRoute(opts)
	if mutate == nil {
		noErr(t, err)
	}
	if err != nil {
		return nil, opts
	}
	return route, opts
}

func TestNewLocalToolRouteChecksItsOptions(t *testing.T) {
	for name, mutate := range map[string]func(*LocalToolRouteOptions){
		"core":      func(o *LocalToolRouteOptions) { o.Core = nil },
		"state dir": func(o *LocalToolRouteOptions) { o.StateDir = "" },
		"session":   func(o *LocalToolRouteOptions) { o.SessionID = "" },
		"turn":      func(o *LocalToolRouteOptions) { o.TurnID = "" },
		"workspace": func(o *LocalToolRouteOptions) { o.Workspace = "" },
		"relative":  func(o *LocalToolRouteOptions) { o.Workspace = "ws" },
		"missing":   func(o *LocalToolRouteOptions) { o.Workspace = filepath.Join(o.Workspace, "nope") },
		"mode":      func(o *LocalToolRouteOptions) { o.PermissionMode = "yolo" },
		"policy":    func(o *LocalToolRouteOptions) { o.ToolPolicy = map[string]string{"bash": "maybe"} },
	} {
		if route, _ := newLocalRoute(t, func(o *LocalToolRouteOptions) { mutate(o) }); route != nil {
			t.Fatalf("%s: NewLocalToolRoute accepted the options", name)
		}
	}
}

func TestLocalToolRouteCallNeedsAnIDAndName(t *testing.T) {
	route, _ := newLocalRoute(t, nil)
	for _, call := range []RouteCall{{Name: "bash"}, {ID: "c1"}} {
		wantRefused(t, route.Call(context.Background(), call), "invalid_tool_input")
	}
}

// RecordApproval takes only the platform's allow or deny, for the interaction asked.
func TestLocalToolRouteRecordApproval(t *testing.T) {
	route, opts := newLocalRoute(t, nil)
	id := ApprovalID(rigSession, rigTurn, "c1")
	run := bashInput("touch ok.txt")
	request := InteractionRequestParams{InteractionID: id, ToolName: "bash", ToolInput: run}
	if err := route.RecordApproval("c1", request, InteractionResponseParams{InteractionID: "appr_other", Outcome: InteractionOutcomeAllow}); err == nil {
		t.Fatal("an answer to another interaction was recorded")
	}
	// Expired and canceled answers leave the call without an approval.
	noErr(t, route.RecordApproval("c1", request, InteractionResponseParams{InteractionID: id, Outcome: InteractionOutcomeExpired}))
	wantRefused(t, route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: run}), "approval_required")

	noErr(t, route.RecordApproval("c1", request, InteractionResponseParams{InteractionID: id, Outcome: InteractionOutcomeAllow}))
	// The approval id the brain presents must be the one the user answered.
	wantRefused(t, route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: run, ApprovalID: "appr_forged"}), "approval_invalid")
	wantExecuted(t, route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: run, ApprovalID: id}))
	if !fileExists(opts.Workspace, "ok.txt") {
		t.Fatal("the approved call did not run in the workspace")
	}
}

// A route of a newer turn of the session makes the earlier one stale, and the
// same turn again (a restart) keeps what the turn state knows.
func TestLocalToolRouteTurnsOfASession(t *testing.T) {
	first, opts := newLocalRoute(t, func(o *LocalToolRouteOptions) { o.ToolPolicy = map[string]string{"bash": "allow"} })
	if first == nil {
		t.Fatal("no route")
	}
	wantExecuted(t, first.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: bashInput("true")}))

	same, err := NewLocalToolRoute(opts)
	noErr(t, err)
	if got := same.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: bashInput("true")}); !got.Replayed {
		t.Fatalf("the same turn again = %+v, want the first result replayed", got)
	}
	opts.TurnID = "turn_2"
	_, err = NewLocalToolRoute(opts)
	noErr(t, err)
	wantRefused(t, first.Call(context.Background(), RouteCall{ID: "c2", Name: "bash", Input: bashInput("true")}), "stale_turn")
	wantRefused(t, same.Call(context.Background(), RouteCall{ID: "c2", Name: "bash", Input: bashInput("true")}), "stale_turn")
}

// CloseSession releases the session's turn state; a canceled Context makes calls stale.
func TestLocalToolRouteCloseSessionAndContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	route, opts := newLocalRoute(t, func(o *LocalToolRouteOptions) {
		o.ToolPolicy = map[string]string{"bash": "allow"}
		o.Context = ctx
	})
	wantExecuted(t, route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: bashInput("true")}))
	cancel()
	got := route.Call(context.Background(), RouteCall{ID: "c2", Name: "bash", Input: bashInput("true")})
	wantRefused(t, got, "stale_turn")

	route.CloseSession()
	if entries, _ := os.ReadDir(filepath.Join(opts.StateDir, "executor-turns")); len(entries) != 0 {
		t.Fatalf("turn state left behind: %v", entries)
	}
	if opts.Core.session(coreKey{acpBrain, rigSession}) != nil {
		t.Fatal("the Core session is still open")
	}
}
