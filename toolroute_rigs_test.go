//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// boundRoute ends a call with the turn, as ACPAgent.onRequest does for every
// request the agent makes.
type boundRoute struct {
	toolRoute
	turnCtx context.Context
}

func (b boundRoute) call(ctx context.Context, c routedCall) *RouteResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(b.turnCtx, cancel)()
	return b.toolRoute.call(ctx, c)
}

// newLocalRig is the local route: ACPAgent's turn state and a Core.
func newLocalRig(t *testing.T, toolPolicy map[string]string) *routeRig {
	t.Helper()
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	workspace := filepath.Join(core.opts.WorkRoot, "acp", rigSession)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	noErr(t, core.openLocal(acpBrain, rigSession, workspace))
	stateDir := t.TempDir()
	policy := acpEffectivePolicy("default", toolPolicy)

	var mu sync.Mutex
	var agent *ACPAgent
	var ledger *acpLedger
	var turnCtx context.Context
	var cancel context.CancelFunc
	start := func(turn string) toolRoute { // a turn of the (restarted) executor process
		var err error
		if agent == nil {
			agent = &ACPAgent{opts: ACPAgentOptions{Core: core, StateDir: stateDir}}
		}
		if ledger, err = agent.ledger(rigSession); err != nil {
			t.Fatal(err)
		}
		if _, err = ledger.begin(turn); err != nil {
			t.Fatal(err)
		}
		if cancel != nil {
			cancel()
		}
		turnCtx, cancel = context.WithCancel(context.Background())
		at := &acpTurn{agent: agent, ctx: turnCtx, ledger: ledger, turnID: turn, policy: policy}
		at.toolRoute = localToolRoute{agent: agent, turn: at}
		return boundRoute{at.toolRoute, turnCtx}
	}
	current := start(rigTurn)
	t.Cleanup(func() { cancel() })
	return &routeRig{
		workspace: workspace,
		route:     func() toolRoute { mu.Lock(); defer mu.Unlock(); return current },
		approve: func(callID, tool string, input map[string]any, decision string) {
			noErr(t, ledger.update(callID, func(c *acpCallState) {
				c.Decision, c.InteractionID, c.ToolName, c.InputHash = decision, approvalID(rigSession, rigTurn, callID), tool, acpInputHash(input)
			}))
		},
		markStarted: func(callID string) {
			noErr(t, ledger.update(callID, func(c *acpCallState) { c.Phase = acpPhaseStarted }))
		},
		restart: func() { // a new process: nothing in memory, the state directory stays
			mu.Lock()
			defer mu.Unlock()
			agent = nil
			current = start(rigTurn)
		},
		supersede: func() {
			mu.Lock()
			defer mu.Unlock()
			current = start("turn_2")
		},
		cancelTurn: func() { cancel() },
	}
}

// newPlatformRig is the platform route against the gateway double.
func newPlatformRig(t *testing.T, toolPolicy map[string]string) *routeRig {
	t.Helper()
	gw := newStubGateway(t, acpEffectivePolicy("default", toolPolicy))
	var mu sync.Mutex
	approved := map[string]string{} // call id -> interaction id the brain presents
	newRoute := func(token string) toolRoute {
		return newPlatformToolRoute(PlatformToolRouteOptions{Endpoint: gw.server.URL, Token: token, TurnID: rigTurn,
			ApprovalFor: func(callID string) string { mu.Lock(); defer mu.Unlock(); return approved[callID] }})
	}
	current := newRoute("tok-1")
	return &routeRig{
		workspace: gw.workspace,
		route:     func() toolRoute { mu.Lock(); defer mu.Unlock(); return current },
		approve: func(callID, tool string, input map[string]any, decision string) {
			id := approvalID(rigSession, rigTurn, callID)
			gw.answer(id, tool, input, decision)
			mu.Lock()
			approved[callID] = id
			mu.Unlock()
		},
		markStarted: func(callID string) { gw.seedStarted(platformIdempotencyKey(rigSession, rigTurn, callID)) },
		restart: func() { // a new brain process: the gateway keeps the state, the route has none
			mu.Lock()
			defer mu.Unlock()
			current = newRoute("tok-1")
		},
		supersede: func() {
			token := gw.newEpoch()
			mu.Lock()
			defer mu.Unlock()
			current = newRoute(token)
		},
		cancelTurn:   gw.cancelTurn,
		handsOffline: gw.setOffline,
	}
}
