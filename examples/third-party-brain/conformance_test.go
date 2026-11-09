//go:build !windows

package main

import (
	"context"
	"sync"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
	"github.com/beeos-ai/bma-executor-sdk/conformance"
	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
)

// The route this brain's tool calls take (AgentTurn.Tools, the platform's tool
// gateway) run against the tool route conformance suite (T1–T11): the turns are
// started through the executor protocol, the way the platform starts them, and
// the route is the one the brain gets for the turn. Wire the suite into the
// tests of your own brain like this.

// routeProbe is the brain, except that its turn only hands out the route the
// brain would call its tools through.
type routeProbe struct {
	brain
	routes chan bmaexecutor.ToolRoute
}

func (p routeProbe) RunTurn(_ context.Context, turn *bmaexecutor.AgentTurn) error {
	p.routes <- turn.Tools()
	return nil
}

// presenting presents the approval of each call the way the brain does: the id
// the user answered, with the call.
type presenting struct {
	route bmaexecutor.ToolRoute
	mu    *sync.Mutex
	asked map[string]string
}

func (p presenting) Call(ctx context.Context, c bmaexecutor.RouteCall) *bmaexecutor.RouteResult {
	p.mu.Lock()
	c.ApprovalID = p.asked[c.ID]
	p.mu.Unlock()
	return p.route.Call(ctx, c)
}

func brainRig(t *testing.T, toolPolicy map[string]string) *conformance.Rig {
	t.Helper()
	turnCtx, cancelTurn := context.WithCancel(context.Background())
	t.Cleanup(cancelTurn)
	hands, workspace := conformance.DefaultHands(t, turnCtx)
	gw := gatewaystub.New(t, gatewaystub.Options{Policy: conformance.EffectivePolicy(toolPolicy), Hands: func(context.Context) gatewaystub.Hands { return hands }})

	probe := routeProbe{routes: make(chan bmaexecutor.ToolRoute, 1)}
	var mu sync.Mutex
	asked := map[string]string{}
	// turn starts the turn of the session with the tool credential token on a
	// brain process of its own (a restart is a new process: the executor does
	// not run a turn id it has seen again) and returns the route the brain got.
	turn := func(token string) bmaexecutor.ToolRoute {
		p, executor := startFor(t, probe)
		runTurn(t, p, executor, bmaexecutor.TurnStartParams{
			SessionID: conformance.SessionID, TurnID: conformance.TurnID, InputID: "ain_1", Input: input("go"),
			Metadata: map[string]string{"tools_mcp_url": gw.URL(), "tools_mcp_token": token},
		})
		return presenting{route: <-probe.routes, mu: &mu, asked: asked}
	}
	current := turn(gatewaystub.InitialToken)
	return &conformance.Rig{
		Workspace: workspace,
		Route:     func() bmaexecutor.ToolRoute { mu.Lock(); defer mu.Unlock(); return current },
		Approve: func(callID, tool string, input map[string]any, decision string) {
			id := bmaexecutor.ApprovalID(conformance.SessionID, conformance.TurnID, callID)
			gw.Answer(id, tool, input, decision)
			mu.Lock()
			asked[callID] = id
			mu.Unlock()
		},
		MarkStarted: func(callID string) {
			gw.SeedStarted(gatewaystub.IdempotencyKey(conformance.SessionID, conformance.TurnID, callID))
		},
		Restart: func() { // the platform starts the turn again; the gateway keeps the state
			route := turn(gatewaystub.InitialToken)
			mu.Lock()
			current = route
			mu.Unlock()
		},
		Supersede: func() {
			route := turn(gw.NewEpoch())
			mu.Lock()
			current = route
			mu.Unlock()
		},
		CancelTurn:   gw.CancelTurn,
		HandsOffline: gw.SetOffline,
	}
}

func TestBrainRoutePassesTheToolRouteConformanceSuite(t *testing.T) {
	conformance.RunToolRouteConformance(t, brainRig)
}
