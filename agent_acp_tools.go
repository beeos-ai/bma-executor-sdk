package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Tools of an ACP agent run in Core, in a session Core holds for the local
// brain ("acp") whose workspace is the turn's working directory. Inputs come
// in the CMA agent_toolset shape that bma-dsh-acp and brain-host share
// (file_path, timeout in ms, replace_all, output_mode, case_insensitive) and
// are mapped to Core's ToolInput here.

// openLocal creates (or re-roots) the Core session of a local brain; it has
// no harness client (no session.ready, no artifact upload).
func (c *Core) openLocal(brain, sessionID, workspace string) error {
	key := coreKey{brain, sessionID}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return &RPCError{Code: CodeResourceUnavailable, Message: "executor is shutting down"}
	}
	if s, ok := c.sessions[key]; ok {
		s.mu.Lock()
		s.workspace = workspace
		s.mu.Unlock()
		return nil
	}
	if len(c.sessions) >= c.opts.MaxSessions {
		return busy("executor is at its session capacity")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.sessions[key] = &CoreSession{key: key, workspace: workspace, ctx: ctx, cancel: cancel, values: map[any]any{}}
	return nil
}

// closeLocal stops a local brain's Core session (its running tools end).
func (c *Core) closeLocal(brain, sessionID string) {
	c.stop(coreKey{brain, sessionID})
}

// A tool call has the three outcomes of the platform's tool gateway
// (ADR-0005): not_forwarded (refused, certainly not run, safe to ask again),
// executed (it ran; IsError says the tool itself failed) and unknown (it may
// have run, do not blindly retry).
const (
	acpToolNotForwarded = "not_forwarded"
	acpToolExecuted     = "executed"
	acpToolUnknown      = "unknown"
)

// acpToolResult is the answer to _bma/tools/call. The brain maps it to the
// ACP tool_call_update (IsError: status failed) and the executor to the CMA
// agent.tool_result (is_error); Text starts with "<Code>: " when the call
// was refused or its outcome is unknown.
type acpToolResult struct {
	Outcome string
	Code    string
	Text    string
	IsError bool
	// Replayed: the answer was saved from an earlier call with the same id
	// (not run again).
	Replayed bool
	// Background: a command left running when the call stopped waiting
	// (platform route; the local Core has none).
	Background *toolBackground
}

// toolBackground is a command still running after its call returned.
type toolBackground struct {
	PID        int
	OutputFile string
}

func refused(code, detail string) *acpToolResult {
	return &acpToolResult{Outcome: acpToolNotForwarded, Code: code, Text: code + ": " + detail, IsError: true}
}

// acpEffectivePolicy is the permission of every tool of a turn: the mode's
// defaults (the same as bma-dsh-acp's) overridden by turn.start
// metadata.tool_policy. A tool it does not name is ask (fail-closed).
func acpEffectivePolicy(mode string, overrides map[string]string) map[string]string {
	edits := "ask"
	if mode == "accept_edits" {
		edits = "allow"
	}
	policy := map[string]string{ToolNameBash: "ask", ToolNameWrite: edits, ToolNameEdit: edits,
		ToolNameRead: "allow", ToolNameGlob: "allow", ToolNameGrep: "allow"}
	for tool, permission := range overrides {
		policy[tool] = permission
	}
	return policy
}

// acpInputHash identifies a tool call's input (map keys are sorted by encoding/json).
func acpInputHash(input map[string]any) string {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// gate is the executor's own check before a tool runs, the local copy of the
// platform gateway's (ADR-0005): the brain is not trusted to have asked.
// It returns nil when the call may run:
//
//   - the call belongs to the session's running turn (an older brain process,
//     an ended or canceled turn, or a turn the state has moved past is
//     stale_turn);
//   - deny refuses (permission_denied);
//   - ask needs the platform's allow for this very call: its interaction
//     answered allow (recorded when interaction.response came), under the
//     deterministic appr_ id, for the same tool and input (approval_required
//     when there is none, approval_invalid when it does not match). The
//     approval is for one call id; the ledger answers a repeated call with
//     the first result instead of running it again.
func (t *acpTurn) gate(sessionID, callID, name string, in map[string]any) *acpToolResult {
	if t.ctx.Err() != nil || t.ledger.turnID() != t.turnID {
		return refused("stale_turn", "the turn this tool call belongs to is over")
	}
	switch permission := t.policy[name]; permission {
	case "allow":
		return nil
	case "deny":
		return refused("permission_denied", fmt.Sprintf("the %s tool is not allowed in this session", name))
	}
	call := t.ledger.call(callID)
	switch {
	case call.Decision == "":
		return refused("approval_required", fmt.Sprintf("the %s tool needs the user's approval and none was given for this call", name))
	case call.Decision != acpDecisionAllow:
		return refused("approval_invalid", "the user did not approve this call")
	case call.InteractionID != approvalID(sessionID, t.turnID, callID):
		return refused("approval_invalid", "the approval belongs to another session, turn or call")
	case call.ToolName != name || call.InputHash != acpInputHash(in):
		return refused("approval_invalid", "the approval was given for another tool or input")
	}
	return nil
}

// runToolOnce runs a tool call at most once across executor restarts: the
// ledger says "started" before the tool runs and "done" with its result
// after. A call asked for again is answered from the ledger; one whose
// start was recorded without a result has an unknown outcome and is not run
// again. A call the gate refuses is not recorded: it did not run and may be
// asked for again once approved.
func (a *ACPAgent) runToolOnce(ctx context.Context, t *acpTurn, sessionID, callID, name string, in map[string]any) *acpToolResult {
	ledger := t.ledger
	stateError := func(err error) *acpToolResult {
		return &acpToolResult{Outcome: acpToolUnknown, Code: "turn_state", Text: "turn state: " + err.Error(), IsError: true}
	}
	switch call := ledger.call(callID); call.Phase {
	case acpPhaseDone:
		outcome := call.Outcome
		if outcome == "" {
			outcome = acpToolExecuted
		}
		return &acpToolResult{Outcome: outcome, Code: call.Code, Text: call.Text, IsError: call.IsError, Replayed: true}
	case acpPhaseStarted:
		text := "outcome_unknown: the tool call was started before the executor restarted; whether it took effect is unknown, so it was not run again."
		if err := ledger.update(callID, func(c *acpCallState) {
			c.Phase, c.Text, c.IsError, c.Outcome, c.Code = acpPhaseDone, text, true, acpToolUnknown, "outcome_unknown"
		}); err != nil {
			return stateError(err)
		}
		return &acpToolResult{Outcome: acpToolUnknown, Code: "outcome_unknown", Text: text, IsError: true}
	}
	if denied := t.gate(sessionID, callID, name, in); denied != nil {
		a.log(LogWarn, fmt.Sprintf("tool call %s (%s) of session %s refused: %s", callID, name, sessionID, denied.Text), nil)
		return denied
	}
	if a.hook != nil {
		a.hook(ctx, "tool:before")
		if ctx.Err() != nil {
			return &acpToolResult{Outcome: acpToolNotForwarded, Code: "canceled", Text: "canceled: The tool call was cancelled.", IsError: true}
		}
	}
	if err := ledger.update(callID, func(c *acpCallState) { c.Phase = acpPhaseStarted }); err != nil {
		return stateError(err)
	}
	var text string
	var isError bool
	var background *toolBackground
	if name == ToolNameBash {
		text, isError, background = a.runBash(ctx, t, sessionID, callID, in)
	} else {
		text, isError = a.runTool(ctx, sessionID, callID, name, in)
	}
	if err := ledger.update(callID, func(c *acpCallState) {
		c.Phase, c.Text, c.IsError, c.Outcome, c.Code = acpPhaseDone, text, isError, acpToolExecuted, ""
	}); err != nil {
		return stateError(err)
	}
	if a.hook != nil {
		a.hook(ctx, "tool:after")
	}
	return &acpToolResult{Outcome: acpToolExecuted, Text: text, IsError: isError, Background: background}
}

// runTool runs a file tool in Core (bash is runBash).
func (a *ACPAgent) runTool(ctx context.Context, sessionID, callID, name string, in map[string]any) (string, bool) {
	if !coreTools[name] || name == ToolNameBash {
		return fmt.Sprintf("tool %q is not available on this computer", name), true
	}
	str := func(k string) string { v, _ := in[k].(string); return v }
	num := func(k string) int64 { v, _ := in[k].(float64); return int64(v) }
	flag := func(k string) bool { v, _ := in[k].(bool); return v }
	input := ToolInput{Path: str("file_path"), Offset: num("offset"), Limit: num("limit"), Content: str("content"),
		OldString: str("old_string"), NewString: str("new_string"), Pattern: str("pattern"), Glob: str("glob")}
	switch name {
	case ToolNameGlob, ToolNameGrep:
		input.Path = str("path")
		if name == ToolNameGrep && flag("case_insensitive") {
			input.Pattern = "(?i)" + input.Pattern
		}
	case ToolNameEdit:
		if flag("replace_all") {
			return a.replaceAll(sessionID, input)
		}
	}
	raw, _ := json.Marshal(input)
	result := a.opts.Core.toolCall(ctx, acpBrain, ToolCallParams{SessionID: sessionID, ToolCallID: callID, Tool: name, Input: raw})
	var texts []string
	for _, block := range result.Content {
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	text := strings.Join(texts, "\n")
	isError := result.IsError || result.Outcome != ToolOutcomeSucceeded
	if name == ToolNameGrep && !isError {
		text = grepOutput(text, str("output_mode"))
	}
	return text, isError
}

// replaceAll is edit with replace_all (Core's edit replaces exactly one match).
func (a *ACPAgent) replaceAll(sessionID string, input ToolInput) (string, bool) {
	session := a.opts.Core.session(coreKey{acpBrain, sessionID})
	if session == nil {
		return "session_not_active: the session is not open", true
	}
	path, err := session.resolve(input.Path)
	if err != nil {
		return err.Error(), true
	}
	if input.OldString == "" {
		return "edit requires old_string and new_string", true
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true
	}
	n := strings.Count(string(raw), input.OldString)
	if n == 0 {
		return "old_string was not found", true
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), input.OldString, input.NewString)), 0o600); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("edited %s (%d replacements)", filepath.Base(path), n), false
}

// grepOutput reshapes Core's "file:line:text" matches for output_mode.
func grepOutput(matches, mode string) string {
	if mode == "" || mode == "content" || matches == "" {
		return matches
	}
	var order []string
	counts := map[string]int{}
	for _, line := range strings.Split(matches, "\n") {
		file, _, _ := strings.Cut(line, ":")
		if counts[file] == 0 {
			order = append(order, file)
		}
		counts[file]++
	}
	out := make([]string, len(order))
	for i, file := range order {
		out[i] = file
		if mode == "count" {
			out[i] = fmt.Sprintf("%s:%d", file, counts[file])
		}
	}
	return strings.Join(out, "\n")
}
