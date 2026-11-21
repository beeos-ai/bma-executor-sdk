package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// ToolRoute is where a brain's tool calls go to run (ADR-0029, brain-hands
// split): the seam between a brain and its hands. A brain written against the
// SDK gets the route of its turn from AgentTurn.Tools and never needs to know
// where the hands are.
//
// There are two routes. PlatformToolRoute is the platform's execute-only tool
// gateway (ADR-0005): it checks the turn's credential and epoch, re-evaluates
// the tool policy, verifies the user's approval, runs each call id at most
// once and runs the tool on the session's hands machine, which may refuse it
// again by its own policy. LocalToolRoute (the hands are this machine) answers
// with the same outcomes and refuses for the same reasons with the same error
// codes. Both are held to the tool route conformance suite (package
// conformance, cases T1–T11), which a route of your own can run too.
//
// A ToolRoute is bound to one turn of one session and is safe for concurrent
// use. Its calls end with ctx: a canceled ctx abandons the call, which does not
// kill a process already started on the hands (the platform's turn cancel
// does).
type ToolRoute interface {
	// Call runs one tool call and always answers: a refusal, a transport
	// failure or an unclear outcome is a RouteResult with IsError set, never
	// a Go error. A repeated Call with the same RouteCall.ID is answered with
	// the first result (RouteResult.Replayed), not run again.
	Call(ctx context.Context, call RouteCall) *RouteResult
}

// ApprovalID is the interaction id to ask a tool call's approval under
// (InteractionRequestParams.InteractionID) and to present with the call
// (RouteCall.ApprovalID): "appr_" and 24 hex characters of sha256 over session,
// turn and call id, NUL separated (ADR-0027). It is the same after a reconnect
// or an executor restart, so the platform keeps one interaction per call and an
// approval already answered is not asked again. The turn is part of it because
// providers may reuse call ids such as "call_0" in every turn.
func ApprovalID(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "appr_" + hex.EncodeToString(sum[:])[:24]
}

// RouteCall is one tool call of a brain, in the CMA agent_toolset input shape
// (bash, read, write, edit, glob, grep; for example {"command": "ls"} for bash).
type RouteCall struct {
	// ID is the brain's tool call id. It is the call's idempotency identity:
	// the same ID in the same turn is run once, also when the brain process
	// restarts and asks again. It must be unique within the turn.
	ID string
	// ToolUseID is the event id of the call's agent.tool_use event (optional;
	// it ties the gateway's audit record to the event).
	ToolUseID string
	// Name is the tool: bash, read, write, edit, glob or grep.
	Name string
	// Input is the tool's input as the brain's JSON decoder produced it.
	Input map[string]any
	// ApprovalID is the interaction id of the user's approval of this call
	// (AgentTurn.RequestInteraction answered allow), or "" when the call needs
	// none. The gateway verifies it: one approval is good for one call, the
	// same tool and the same input.
	ApprovalID string
}

// The three outcomes of a tool call, as the platform's tool gateway says them
// (ADR-0005).
const (
	// RouteOutcomeNotForwarded: the call was refused and certainly did not
	// run (policy, approval, a stale turn, hands offline); it is safe to ask
	// again, for example once approved.
	RouteOutcomeNotForwarded = "not_forwarded"
	// RouteOutcomeExecuted: the tool ran; RouteResult.IsError says whether the
	// tool itself failed.
	RouteOutcomeExecuted = "executed"
	// RouteOutcomeUnknown: the call may have run (the connection broke after
	// it was sent, the hands restarted); do not blindly run it again.
	RouteOutcomeUnknown = "unknown"
)

// RouteResult is the answer to a RouteCall.
type RouteResult struct {
	// Outcome is a RouteOutcome* value.
	Outcome string
	// Code is the reason of a refusal or an unclear outcome, for example
	// permission_denied, approval_required, approval_invalid, stale_turn,
	// environment_unavailable, in_progress, idempotency_key_reused,
	// tools_unavailable (open set; contract schemas/common.json lists them).
	Code string
	// Text is the tool's output, or "<Code>: <detail>" when Code is set.
	Text string
	// IsError: the call failed, was refused or its outcome is unknown.
	IsError bool
	// Replayed: the answer was saved from an earlier call with the same ID
	// (not run again).
	Replayed bool
	// Background: a command left running when the call stopped waiting
	// (bash past its synchronous wait).
	Background *RouteBackground
	// Images are image blocks of the result (computer_observe screenshots),
	// for the model with Text. A replayed result has none.
	Images []ContentBlock
}

// RouteBackground is a command still running after its call returned.
type RouteBackground struct {
	PID        int
	OutputFile string
}

// refusingRoute answers every call with a refusal: the turn's tool
// credential is unusable, so no call may run anywhere (fail closed).
type refusingRoute struct{ detail string }

func (r refusingRoute) Call(context.Context, RouteCall) *RouteResult {
	return refused("tools_unavailable", r.detail)
}
