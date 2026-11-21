//go:build !windows

package bmaexecutor

import (
	"context"
	"strings"
	"testing"
)

// The exported brain-side API (ADR-0029 S3): AgentTurn.Tools and the
// PlatformToolRoute a third-party brain gets from it.

func newToolsTurn(metadata map[string]string) *AgentTurn {
	runtime := &agentRuntime{client: &Client{}}
	session := &agentSession{runtime: runtime, id: rigSession}
	return &AgentTurn{session: session, params: TurnStartParams{SessionID: rigSession, TurnID: rigTurn, Metadata: metadata}}
}

// A turn whose hands are this machine has no route: the brain runs its tools itself.
func TestAgentTurnToolsIsNilWhenHandsAreLocal(t *testing.T) {
	if got := newToolsTurn(nil).Tools(); got != nil {
		t.Fatalf("Tools() = %#v, want nil", got)
	}
	if got := newToolsTurn(map[string]string{"model_token": "x"}).Tools(); got != nil {
		t.Fatalf("Tools() with model credentials only = %#v, want nil", got)
	}
}

// tools_mcp_* makes the platform's gateway the route of every call; the call
// id is the idempotency identity and the approval id travels in _meta.
func TestAgentTurnToolsIsThePlatformRoute(t *testing.T) {
	gw := newGateway(t, map[string]string{"bash": "ask"})
	turn := newToolsTurn(map[string]string{"tools_mcp_url": gw.URL(), "tools_mcp_token": "tok-1"})
	tools := turn.Tools()
	if tools == nil {
		t.Fatal("Tools() = nil, want the platform route")
	}
	if turn.Tools() != tools {
		t.Fatal("Tools() returned another route for the same turn")
	}
	ctx := context.Background()
	call := RouteCall{ID: "c1", ToolUseID: "evt_1", Name: "bash", Input: wireInput(bashInput("echo hi > out.txt"))}
	wantRefused(t, tools.Call(ctx, call), "approval_required")
	if fileExists(gw.Workspace(), "out.txt") {
		t.Fatal("a call without approval ran")
	}
	id := turn.ApprovalID("c1")
	if id != ApprovalID(rigSession, rigTurn, "c1") || !strings.HasPrefix(id, "appr_") || len(id) != len("appr_")+24 {
		t.Fatalf("ApprovalID = %q", id)
	}
	if other := turn.ApprovalID("c2"); other == id {
		t.Fatal("two calls share an approval id")
	}
	gw.Answer(id, "bash", call.Input, "allow")
	call.ApprovalID = id
	got := tools.Call(ctx, call)
	wantExecuted(t, got)
	if !fileExists(gw.Workspace(), "out.txt") {
		t.Fatal("the approved call did not run on the hands")
	}
	again := tools.Call(ctx, call)
	if !again.Replayed || again.Outcome != RouteOutcomeExecuted {
		t.Fatalf("a repeated call id = %+v, want the first result replayed", again)
	}
}

// One half of the credential never falls back to a local run.
func TestAgentTurnToolsRefusesAHalfCredential(t *testing.T) {
	for name, metadata := range map[string]map[string]string{
		"url only":   {"tools_mcp_url": "https://executor.example/v1/machines/mch_a/mcp"},
		"token only": {"tools_mcp_token": "tok"},
		"bad url":    {"tools_mcp_url": "://nope", "tools_mcp_token": "tok"},
	} {
		tools := newToolsTurn(metadata).Tools()
		if tools == nil {
			t.Fatalf("%s: Tools() = nil, a local run would follow", name)
		}
		wantRefused(t, tools.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: bashInput("true")}), "tools_unavailable")
	}
}

func TestPlatformToolRouteCallNeedsAnIDAndName(t *testing.T) {
	gw := newGateway(t, map[string]string{"bash": "allow"})
	route, err := NewPlatformToolRoute(PlatformToolRouteOptions{Endpoint: gw.URL(), Token: "tok-1", SessionID: rigSession, TurnID: rigTurn})
	noErr(t, err)
	for _, call := range []RouteCall{{Name: "bash"}, {ID: "c1"}} {
		wantRefused(t, route.Call(context.Background(), call), "invalid_tool_input")
	}
	if n := len(gw.Recorded()); n != 0 {
		t.Fatalf("%d requests reached the gateway", n)
	}
}

func TestNewPlatformToolRouteChecksItsOptions(t *testing.T) {
	ok := PlatformToolRouteOptions{Endpoint: "https://executor.example/v1/machines/mch_a/mcp", Token: "t", SessionID: "s", TurnID: "u"}
	if _, err := NewPlatformToolRoute(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PlatformToolRouteOptions){
		"endpoint": func(o *PlatformToolRouteOptions) { o.Endpoint = "" },
		"token":    func(o *PlatformToolRouteOptions) { o.Token = "" },
		"session":  func(o *PlatformToolRouteOptions) { o.SessionID = "" },
		"turn":     func(o *PlatformToolRouteOptions) { o.TurnID = "" },
		"scheme":   func(o *PlatformToolRouteOptions) { o.Endpoint = "ftp://x/y" },
	} {
		opts := ok
		mutate(&opts)
		route, err := NewPlatformToolRoute(opts)
		if err == nil || route != nil {
			t.Fatalf("%s: NewPlatformToolRoute accepted %+v", name, opts)
		}
		if strings.Contains(err.Error(), "token") && strings.Contains(err.Error(), opts.Token) && opts.Token != "" {
			t.Fatalf("%s: the error leaks the token: %v", name, err)
		}
	}
}

// Both routes are ToolRoutes; the compiler checks it.
var (
	_ ToolRoute = (*PlatformToolRoute)(nil)
	_ ToolRoute = (*LocalToolRoute)(nil)
)
