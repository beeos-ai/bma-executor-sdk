//go:build !windows

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
)

// HandsFunc makes the hands behind the gateway double for one case: the
// machine that runs a call that passed the gateway's checks, and the directory
// its tools work in (the Rig's Workspace). turn ends when the turn is canceled.
type HandsFunc func(t *testing.T, turn context.Context) (hands gatewaystub.Hands, workspace string)

// DefaultHands are a Core of their own in a temporary directory, with the
// workspace "hands" in it (CoreHands).
func DefaultHands(t *testing.T, turn context.Context) (gatewaystub.Hands, string) {
	t.Helper()
	root := t.TempDir()
	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	workspace := filepath.Join(root, "hands")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	return CoreHands(t, turn, core, workspace), workspace
}

// PlatformFactory is the Factory of the SDK's PlatformToolRoute against the
// gateway double, with the hands hands makes. nil hands: a Core of its own in a
// temporary directory (DefaultHands). With your own hands it checks that they are
// what the platform's gateway needs: the same results as the SDK's local
// hands for the same calls.
func PlatformFactory(hands HandsFunc) Factory {
	if hands == nil {
		hands = DefaultHands
	}
	return func(t *testing.T, toolPolicy map[string]string) *Rig {
		t.Helper()
		var workspace string
		gw := gatewaystub.New(t, gatewaystub.Options{Policy: EffectivePolicy(toolPolicy), Hands: func(turn context.Context) gatewaystub.Hands {
			var h gatewaystub.Hands
			h, workspace = hands(t, turn)
			return h
		}})
		var mu sync.Mutex
		approved := map[string]string{} // call id -> interaction id the brain presents
		newRoute := func(token string) bmaexecutor.ToolRoute {
			route, err := bmaexecutor.NewPlatformToolRoute(bmaexecutor.PlatformToolRouteOptions{
				Endpoint: gw.URL(), Token: token, SessionID: SessionID, TurnID: TurnID,
				ApprovalFor: func(callID string) string { mu.Lock(); defer mu.Unlock(); return approved[callID] }})
			if err != nil {
				t.Fatal(err)
			}
			return route
		}
		current := newRoute(gatewaystub.InitialToken)
		return &Rig{
			Workspace: workspace,
			Route:     func() bmaexecutor.ToolRoute { mu.Lock(); defer mu.Unlock(); return current },
			Approve: func(callID, tool string, input map[string]any, decision string) {
				id := bmaexecutor.ApprovalID(SessionID, TurnID, callID)
				gw.Answer(id, tool, input, decision)
				mu.Lock()
				approved[callID] = id
				mu.Unlock()
			},
			MarkStarted: func(callID string) { gw.SeedStarted(gatewaystub.IdempotencyKey(SessionID, TurnID, callID)) },
			Restart: func() { // a new brain process: the gateway keeps the state, the route has none
				mu.Lock()
				defer mu.Unlock()
				current = newRoute(gatewaystub.InitialToken)
			},
			Supersede: func() {
				token := gw.NewEpoch()
				mu.Lock()
				defer mu.Unlock()
				current = newRoute(token)
			},
			CancelTurn:   gw.CancelTurn,
			HandsOffline: gw.SetOffline,
		}
	}
}

// CoreHands are hands that run every call in a Core, in workspace, through a
// LocalToolRoute that allows every tool (the gateway has already decided). They
// are what the SDK's own hands do for a call, so a platform route in front of
// them must answer as the local route does. The call's SyncWait is not applied.
func CoreHands(t *testing.T, turn context.Context, core *bmaexecutor.Core, workspace string) gatewaystub.Hands {
	t.Helper()
	route, err := bmaexecutor.NewLocalToolRoute(bmaexecutor.LocalToolRouteOptions{
		Core: core, StateDir: t.TempDir(), SessionID: "hands", TurnID: "hands", Workspace: workspace, Context: turn,
		ToolPolicy: map[string]string{"bash": "allow", "write": "allow", "edit": "allow", "read": "allow", "glob": "allow", "grep": "allow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return coreHands{route}
}

type coreHands struct{ route *bmaexecutor.LocalToolRoute }

func (h coreHands) Run(ctx context.Context, call gatewaystub.Call) gatewaystub.Reply {
	got := h.route.Call(ctx, bmaexecutor.RouteCall{ID: call.Key, Name: call.Name, Input: call.Input})
	reply := gatewaystub.Reply{Outcome: got.Outcome, Code: got.Code, Text: got.Text, IsError: got.IsError}
	if got.Background != nil {
		reply.PID, reply.OutputFile = got.Background.PID, got.Background.OutputFile
	}
	return reply
}

// EffectivePolicy is the permission of every tool in the default permission
// mode (bash, write and edit ask; read, glob and grep allow) with overrides
// applied: the policy a Factory's route and a gateway double must enforce.
func EffectivePolicy(overrides map[string]string) map[string]string {
	policy := map[string]string{"bash": "ask", "write": "ask", "edit": "ask", "read": "allow", "glob": "allow", "grep": "allow"}
	for tool, permission := range overrides {
		policy[tool] = permission
	}
	return policy
}
