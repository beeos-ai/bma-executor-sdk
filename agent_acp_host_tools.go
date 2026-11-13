package bmaexecutor

// Host tools of the DSH profile: tools this machine adds to the six platform
// tools of a local brain. bma-dsh-acp registers what initialize declares
// (_meta.bma.hostTools) and calls them like the six (_bma/tools/call), so
// they run here, under the same policy, approval and turn rules.
//
//	computer_observe, computer_action  only with CoreOptions.ComputerUse (Core advertises computer_use then):
//	                                   the machine's own desktop, the contract's computer use tools.
//	                                   Default ask: a screenshot leaves the machine for the model and
//	                                   an action drives the real desktop, so each call is approved on
//	                                   the platform (any client of the session) unless the turn's
//	                                   tool_policy says otherwise. A backend that implements
//	                                   ComputerUseApprover may let the calls it judges safe run
//	                                   without asking, but only while neither the turn's tool_policy
//	                                   nor ACPAgentOptions.ToolPolicy names the tool.
//	ask_user_question                  with ACPAgentOptions.AskUserQuestion: asks the user and waits. It is a custom tool call of the
//	                                   session (agent.custom_tool_use): the platform pauses the session
//	                                   with requires_action, any client answers with
//	                                   user.custom_tool_result, and the same turn continues with the
//	                                   answer. Default allow: asking is not an operation, and an answer
//	                                   never grants a permission.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ToolNameAskUserQuestion is the host tool of a local brain that asks the
// user (see agent_acp_host_tools.go).
const ToolNameAskUserQuestion = "ask_user_question"

// acpHostTool is one entry of initialize _meta.bma.hostTools.
type acpHostTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	ReadOnly    bool           `json:"readOnly"`
	Kind        string         `json:"kind"`
	Permission  string         `json:"permission"`
}

func acpObject(required []string, properties map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

// The input of ask_user_question follows Claude Code's AskUserQuestion tool:
// 1-4 questions, each with a short header and 2-4 options (label and
// description), multiSelect for several answers; the user may always answer
// in their own words instead.
var acpAskUserQuestion = acpHostTool{
	Name: ToolNameAskUserQuestion,
	Description: "Ask the user one to four questions when you need a decision or information only they have, " +
		"and wait for the answer. Give each question 2-4 concrete options; the user may also answer in their own words. " +
		"Do not use it to ask for permission to use a tool (tools ask on their own), and do not treat an answer as such a permission.",
	Parameters: acpObject([]string{"questions"}, map[string]any{
		"questions": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 4, "description": "The questions, asked together.",
			"items": map[string]any{
				"type": "object", "required": []string{"question", "options"}, "additionalProperties": false,
				"properties": map[string]any{
					"question":    map[string]any{"type": "string", "description": "The complete question, ending with a question mark."},
					"header":      map[string]any{"type": "string", "description": "A very short label for the question (at most 12 characters)."},
					"multiSelect": map[string]any{"type": "boolean", "description": "The user may pick several options."},
					"options": map[string]any{
						"type": "array", "minItems": 2, "maxItems": 4,
						"items": map[string]any{
							"type": "object", "required": []string{"label"}, "additionalProperties": false,
							"properties": map[string]any{
								"label":       map[string]any{"type": "string", "description": "The option as shown (1-5 words)."},
								"description": map[string]any{"type": "string", "description": "What choosing it means."},
							},
						},
					},
				},
			},
		},
	}),
	Kind: "other", Permission: "allow",
}

var acpComputerObserve = acpHostTool{
	Name: ToolNameComputerObserve,
	Description: "Take a screenshot of this computer's screen with the list of its windows (pid, window_id, app, title), " +
		"or of one window (pid and window_id) with its accessibility tree. Coordinates for computer_action are pixels of the screenshot.",
	Parameters: acpObject(nil, map[string]any{
		"pid":       map[string]any{"type": "integer", "description": "Process id of the window to observe (with window_id)."},
		"window_id": map[string]any{"type": "integer", "description": "Window id from the window list (with pid)."},
	}),
	ReadOnly: true, Kind: "read", Permission: "ask",
}

var acpComputerAction = acpHostTool{
	Name: ToolNameComputerAction,
	Description: "Use this computer's mouse and keyboard: click, type, key, scroll or drag. Coordinates are pixels of the last computer_observe " +
		"screenshot (window-local with pid and window_id). Observe before acting and again afterwards to check the result.",
	Parameters: acpObject([]string{"action"}, map[string]any{
		"action":           map[string]any{"type": "string", "enum": []string{ComputerActionClick, ComputerActionType, ComputerActionKey, ComputerActionScroll, ComputerActionDrag}},
		"coordinate":       map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "minItems": 2, "maxItems": 2, "description": "[x, y] for click, scroll and the end of drag."},
		"start_coordinate": map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "minItems": 2, "maxItems": 2, "description": "[x, y] where drag starts."},
		"text":             map[string]any{"type": "string", "description": `Text to type, or a key or chord such as "Return" or "ctrl+s".`},
		"button":           map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}},
		"click_count":      map[string]any{"type": "integer", "minimum": 1, "maximum": 3},
		"scroll_direction": map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}},
		"scroll_amount":    map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
		"pid":              map[string]any{"type": "integer", "description": "Process id of the target window (with window_id)."},
		"window_id":        map[string]any{"type": "integer", "description": "Target window id (with pid); input then goes to that window in the background."},
	}),
	Kind: "other", Permission: "ask",
}

// declaredHostTools are the tools this agent offers bma-dsh-acp beyond the
// six (DSH profile only).
func (a *ACPAgent) declaredHostTools() []acpHostTool {
	if a.claude() {
		return nil
	}
	var tools []acpHostTool
	if a.opts.AskUserQuestion {
		tools = append(tools, acpAskUserQuestion)
	}
	if a.opts.Core.opts.ComputerUse != nil {
		tools = append(tools, acpComputerObserve, acpComputerAction)
	}
	return tools
}

// hostTools are the declared host tools the running agent took (initialize
// _meta.bma.hostTools of its answer). An agent from before host tools
// reports none and keeps exactly the six.
func (a *ACPAgent) hostTools() []acpHostTool {
	a.mu.Lock()
	accepted := a.accepted
	a.mu.Unlock()
	var out []acpHostTool
	for _, t := range a.declaredHostTools() {
		if slices.Contains(accepted, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// toolNames are the platform tools and the host tools: exactly what the agent may offer the model.
func (a *ACPAgent) toolNames() map[string]bool {
	names := map[string]bool{}
	for name := range coreTools {
		names[name] = true
	}
	for _, t := range a.hostTools() {
		names[t.Name] = true
	}
	return names
}

// withHostTools adds each host tool's default permission to a policy that does not name it.
func (a *ACPAgent) withHostTools(policy map[string]string) map[string]string {
	for _, t := range a.hostTools() {
		if _, ok := policy[t.Name]; !ok {
			policy[t.Name] = t.Permission
		}
	}
	return policy
}

// hostAllows reports whether a computer use call may run without asking: its
// ask is only the declared default (hostAsk) and the backend's
// ComputerUseApprover says the user need not approve it. The answer for a
// call is kept: the tool_use event and the permission request use the same.
func (t *acpTurn) hostAllows(ctx context.Context, sessionID, callID, tool string, input map[string]any) bool {
	if !t.hostAsk[tool] || t.policyOf(tool) != "ask" || (tool != ToolNameComputerObserve && tool != ToolNameComputerAction) {
		return false
	}
	core := t.agent.opts.Core
	if core == nil || core.opts.ComputerUse == nil {
		return false
	}
	t.mu.Lock()
	allowed, ok := t.hostAllowed[callID]
	t.mu.Unlock()
	if ok {
		return allowed
	}
	allowed = !core.computerNeedsApproval(ctx, sessionID, tool, input)
	t.mu.Lock()
	t.hostAllowed[callID] = allowed
	t.mu.Unlock()
	return allowed
}

// askUserID is the event id of the agent.custom_tool_use of one
// ask_user_question call: the same for the same session, turn and call after
// an executor restart, so an answer given meanwhile comes back.
func askUserID(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "evt_" + sessionID + "_ask_" + hex.EncodeToString(sum[:])[:16]
}

// askUser runs one ask_user_question call as a custom tool call of the
// session and returns the user's answer as the tool result.
func (t *acpTurn) askUser(ctx context.Context, callID string, input map[string]any) *RouteResult {
	if questions, _ := input["questions"].([]any); len(questions) == 0 {
		return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: "invalid_tool_input", Text: "invalid_tool_input: questions is required", IsError: true}
	}
	sessionID := t.turn.SessionID()
	id := askUserID(sessionID, t.turnID, callID)
	t.mu.Lock()
	t.questions[callID] = id
	t.mu.Unlock()
	t.agent.interaction(ACPInteractionEvent{Phase: "requested", Kind: InteractionKindQuestion, SessionID: sessionID, InteractionID: id, ToolName: ToolNameAskUserQuestion, ToolInput: input})
	_, answer, err := t.turn.requestCustomTool(ctx, id, ToolNameAskUserQuestion, input)
	outcome := string(InteractionOutcomeAnswered)
	if err != nil {
		outcome = "withdrawn"
	}
	t.agent.interaction(ACPInteractionEvent{Phase: "ended", Kind: InteractionKindQuestion, SessionID: sessionID, InteractionID: id, ToolName: ToolNameAskUserQuestion, Outcome: outcome})
	if err != nil {
		// The turn ended (cancel, deadline) before an answer: the call is given up (contract).
		_, _ = t.turn.Emit(AgentToolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: id, IsError: true,
			Content: []ContentBlock{{Type: ContentBlockTypeText, Text: "The question was withdrawn before the user answered."}}})
		return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: "canceled", Text: "canceled: The question was withdrawn before the user answered.", IsError: true}
	}
	var texts []string
	for _, b := range answer.Content {
		if b.Type == ContentBlockTypeText && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" {
		text = "The user gave no answer."
	}
	if !answer.IsError {
		text = "The user answered: " + text
	}
	return &RouteResult{Outcome: RouteOutcomeExecuted, Text: text, IsError: answer.IsError}
}

// hostToolsMeta is initialize's _meta for bma-dsh-acp.
func (a *ACPAgent) initializeMeta() map[string]any {
	tools := a.declaredHostTools()
	if len(tools) == 0 {
		return nil
	}
	return map[string]any{acpMetaKey: map[string]any{"hostTools": tools}}
}

// hostToolsViolation checks the agent took only declared host tools.
func (a *ACPAgent) hostToolsViolation(got []string) string {
	var want []string
	for _, t := range a.declaredHostTools() {
		want = append(want, t.Name)
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			raw, _ := json.Marshal(got)
			return fmt.Sprintf("agent took host tools %s, not among the declared [%s]: refused (fail-closed)", raw, strings.Join(want, ","))
		}
	}
	return ""
}
