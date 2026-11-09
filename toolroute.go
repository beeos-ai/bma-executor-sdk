package bmaexecutor

import "context"

// A toolRoute is where a brain's tool call goes to run (ADR-0029, brain-hands
// split): the seam between the brain's shell and the hands.
//
//   - localToolRoute: the brain and the hands are on one machine; the call runs
//     in this executor's Core, after the executor's own re-check (policy,
//     approval, turn) and at most once (the turn ledger).
//   - platformToolRoute (toolroute_platform.go): the hands are elsewhere; the
//     call goes to the platform's execute-only tool gateway (ADR-0005), which
//     does the same checks and runs it in the session's machine.
//
// Both answer with the gateway's three outcomes (not_forwarded, executed,
// unknown) and refuse for the same reasons with the same error codes. The
// conformance suite (toolroute_conformance_test.go) runs every case against
// both routes.
//
// The seam is unexported until the SDK's brain-side API (AgentTurn.Tools) is
// settled; nothing in the protocol or the exported API changes.
type toolRoute interface {
	// call runs one tool call. A route is bound to one turn of one session;
	// the answer is never nil. ctx ends with the turn (cancel, deadline).
	call(ctx context.Context, c routedCall) *acpToolResult
}

// routedCall is one tool call of a brain, in the CMA agent_toolset input shape.
type routedCall struct {
	SessionID string
	// CallID is the brain's tool call id; it is also the idempotency identity
	// (a repeated call is answered with the first result, not run again).
	CallID string
	// ToolUseID is the id of the agent.tool_use event of the call (optional).
	ToolUseID string
	Name      string
	Input     map[string]any
}

// localToolRoute runs a call in this executor's Core (see ACPAgent.runToolOnce).
type localToolRoute struct {
	agent *ACPAgent
	turn  *acpTurn
}

func (r localToolRoute) call(ctx context.Context, c routedCall) *acpToolResult {
	return r.agent.runToolOnce(ctx, r.turn, c.SessionID, c.CallID, c.Name, c.Input)
}
