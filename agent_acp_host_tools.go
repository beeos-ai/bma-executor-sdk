package bmaexecutor

// The tools of a turn besides the six platform tools (BrainOpen.Tools): host
// tools and the custom tools of the session's agent. The brain offers them to
// the model and calls them like the six (BrainSink.Call), under the same
// policy, approval and turn rules.
//
//	custom tools                       turn.start config.custom_tools (contract 1.7.0): a call is an
//	                                   agent.custom_tool_use of the session that the user's application
//	                                   answers (agent.custom_tool_result); the same turn continues with
//	                                   the answer. Never approved.
//	computer_observe, computer_action  the hands' computer use tools: turn.start config.host_tools when the
//	                                   hands are elsewhere (tools_mcp_*, contract 1.8.0, run there), or with
//	                                   CoreOptions.ComputerUse this machine's own desktop.
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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ToolNameAskUserQuestion is the host tool of a local brain that asks the
// user (see agent_acp_host_tools.go).
const ToolNameAskUserQuestion = "ask_user_question"

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
var acpAskUserQuestion = BrainTool{
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

// computerStepDescription is the description input of both computer use
// tools: what a client shows as the step's title. The tools do not read it,
// and a call without it still runs.
var computerStepDescription = map[string]any{"type": "string", "description": "What this step does, in one sentence in the user's language, " +
	`naming the app and the object, for example "在飞书里搜索联系人 Q". At most 40 characters; no coordinates.`}

var acpComputerObserve = BrainTool{
	Name: ToolNameComputerObserve,
	Description: "Take a screenshot of this computer's screen with the list of its windows (pid, window_id, app, title), " +
		"or of one window (pid and window_id) with its accessibility tree. Coordinates for computer_action are pixels of the screenshot. " +
		"For a desktop app, use these tools rather than bash: open it with computer_action open_app, then observe its window by pid and window_id. " +
		"Do not look for apps or read environment variables with bash.",
	Parameters: acpObject([]string{"description"}, map[string]any{
		"description": computerStepDescription,
		"pid":         map[string]any{"type": "integer", "description": "Process id of the window to observe (with window_id)."},
		"window_id":   map[string]any{"type": "integer", "description": "Window id from the window list (with pid)."},
	}),
	ReadOnly: true, Kind: "read", Permission: "ask",
}

var acpComputerAction = BrainTool{
	Name: ToolNameComputerAction,
	Description: "Use this computer's mouse and keyboard: click, type, key, scroll or drag; open_app opens an app by name or bundle id, " +
		"brings it to the front and returns its pid and the windows you can use; menu runs an item of an app's menu bar by its labels " +
		`(menu_path, for example ["Window", "Weixin"] to show a window) with the pid and window_id of one of its windows. ` +
		"Observe before acting and again afterwards to check the result.\n" +
		"Rules:\n" +
		"- Send input (click, type, key, scroll, drag) with the pid and window_id of the window it is for, " +
		"with coordinates from that window's screenshot: observe the window first.\n" +
		"- Only act on windows the window list shows; windows that are minimized, hidden or on another Space cannot be used: " +
		"bring them back with open_app or the app's Window menu.\n" +
		"- type ends with a line \"typed: verified\", \"typed: mismatch\" or \"typed: unverified\". Unless it is verified, observe the field " +
		"and compare its text with what you meant before any Return or send. If only part landed, type just the missing rest; if it is " +
		"wrong otherwise, select all, delete and type it once more; if it is still wrong, stop and report.\n" +
		"- Never press Return in a message box until you have just read the box and its text is exactly the message.\n" +
		"- Before sending, submitting or paying, read the recipient or target on screen and check it is the one the user asked for.\n" +
		"- If the same approach fails twice, try a different one; after three different approaches fail, stop and ask the user. " +
		"Do not experiment with the tool itself.\n" +
		"- Open apps with open_app; do not use bash, osascript or open to find, start or control apps.",
	Parameters: acpObject([]string{"action", "description"}, map[string]any{
		"description": computerStepDescription,
		"action":      map[string]any{"type": "string", "enum": []string{ComputerActionClick, ComputerActionType, ComputerActionKey, ComputerActionScroll, ComputerActionDrag, ComputerActionOpenApp, ComputerActionMenu}},
		"app":         map[string]any{"type": "string", "description": `App to open with open_app: its name such as "TextEdit", or its bundle id such as "com.apple.TextEdit".`},
		"menu_path": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1, "maxLength": maxMenuLabel},
			"minItems": 1, "maxItems": maxMenuItems, "description": `Menu labels from the menu bar to the item for menu, exactly as shown, for example ["File", "Save As..."].`},
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

// turnTools are a turn's tools besides the six: ask_user_question
// (ACPAgentOptions.AskUserQuestion), the hands' computer use tools
// (config.host_tools when the hands are elsewhere, this machine's own with
// CoreOptions.ComputerUse) when the brain offers host tools, and the agent's
// custom tools.
func (a *BrainHost) turnTools(config AgentTurnConfig, remote bool) ([]BrainTool, error) {
	var tools []BrainTool
	switch {
	case len(config.HostTools) > 0 && (!remote || !a.caps.HostTools):
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: host_tools are only for an executor that declared agent.host_tools, with tools_mcp_*"}
	case a.caps.HostTools && a.opts.AskUserQuestion:
		tools = append(tools, acpAskUserQuestion)
	}
	if a.caps.HostTools {
		seen := map[string]bool{}
		for _, h := range config.HostTools {
			if (h.Name != ToolNameComputerObserve && h.Name != ToolNameComputerAction) || seen[h.Name] || strings.TrimSpace(h.Description) == "" ||
				h.InputSchema["type"] != "object" || (h.Permission != "" && h.Permission != "allow" && h.Permission != "ask") {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: host_tools entry " + h.Name + " is not a computer use tool with a description, an object input_schema and permission allow or ask, once"}
			}
			seen[h.Name] = true
			kind := "other"
			if h.ReadOnly {
				kind = "read"
			}
			tools = append(tools, BrainTool{Name: h.Name, Description: h.Description, Parameters: h.InputSchema, ReadOnly: h.ReadOnly, Kind: kind,
				Permission: cmp.Or(h.Permission, "ask")})
		}
		if !remote && a.opts.Core.opts.ComputerUse != nil {
			tools = append(tools, acpComputerObserve, acpComputerAction)
		}
	}
	if len(config.CustomTools) > 0 && !a.caps.CustomTools {
		return nil, &RPCError{Code: CodeCapabilityUnavailable, Message: "capability_unavailable: custom tools are not offered by this brain",
			Data: &JSONRPCErrorData{Capability: "custom_tools"}}
	}
	for _, c := range config.CustomTools {
		if brainToolNames(tools)[c.Name] {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: custom tool " + c.Name + " has the name of one of the executor's tools"}
		}
		tools = append(tools, BrainTool{Name: c.Name, Description: c.Description, Parameters: c.InputSchema, Kind: "other", Custom: true})
	}
	return tools, nil
}

// withTurnTools adds each tool's default permission to a policy that does
// not name it: a host tool's declared one, allow for a custom tool.
func withTurnTools(policy map[string]string, tools []BrainTool) map[string]string {
	for _, t := range tools {
		if _, ok := policy[t.Name]; !ok {
			policy[t.Name] = cmp.Or(t.Permission, "allow")
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

// checkComputer runs the checks and the host's ComputerUsePreflighter of a
// computer use call this machine serves, once per call and before its
// approval, and returns its refusal (nil: go on). Calls of other tools, and
// computer use on the hands of another machine, are not checked here.
func (t *acpTurn) checkComputer(ctx context.Context, call BrainCall) *RouteResult {
	if t.local == nil || (call.Name != ToolNameComputerObserve && call.Name != ToolNameComputerAction) {
		return nil
	}
	core := t.agent.opts.Core
	if core == nil || !core.hasComputerTool(call.Name) {
		return nil
	}
	t.mu.Lock()
	checks, ok := t.computerChecks[call.ID]
	t.mu.Unlock()
	if ok {
		return checks.Refusal
	}
	checks.Refusal = core.computerCheck(ctx, coreKey{acpBrain, t.turn.SessionID()}, call.Name, call.Input)
	t.mu.Lock()
	t.computerChecks[call.ID] = checks
	t.mu.Unlock()
	return checks.Refusal
}

// customToolUseID is the event id of the agent.custom_tool_use of one call
// of a custom tool or of ask_user_question: the same for the same session,
// turn and call after an executor restart, so an answer given meanwhile
// comes back.
func customToolUseID(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "evt_" + sessionID + "_ask_" + hex.EncodeToString(sum[:])[:16]
}

// customTool runs one call as a custom tool call of the session
// (agent.custom_tool_use) and returns the answer's text as the tool result.
// When the turn ends first the call is given up (contract).
func (t *acpTurn) customTool(ctx context.Context, callID, name string, input map[string]any) (text string, isError bool, ok bool) {
	sessionID := t.turn.SessionID()
	id := customToolUseID(sessionID, t.turnID, callID)
	t.mu.Lock()
	t.questions[callID] = id
	t.mu.Unlock()
	t.agent.interaction(ACPInteractionEvent{Phase: "requested", Kind: InteractionKindQuestion, SessionID: sessionID, InteractionID: id, ToolName: name, ToolInput: input})
	_, answer, err := t.turn.requestCustomTool(ctx, id, name, input)
	outcome := string(InteractionOutcomeAnswered)
	if err != nil {
		outcome = "withdrawn"
	}
	t.agent.interaction(ACPInteractionEvent{Phase: "ended", Kind: InteractionKindQuestion, SessionID: sessionID, InteractionID: id, ToolName: name, Outcome: outcome})
	if err != nil {
		_, _ = t.turn.Emit(toolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: id, IsError: true,
			Content: []ContentBlock{{Type: ContentBlockTypeText, Text: "The call was withdrawn before the user answered."}}})
		return "", false, false
	}
	var texts []string
	for _, b := range answer.Content {
		if b.Type == ContentBlockTypeText && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n"), answer.IsError, true
}

// withdrawnCall is the result the brain reads for a custom tool call the turn gave up.
func withdrawnCall() *RouteResult {
	return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: "canceled", Text: "canceled: The call was withdrawn before the user answered.", IsError: true}
}

// askUser runs one ask_user_question call and returns the user's answer.
func (t *acpTurn) askUser(ctx context.Context, callID string, input map[string]any) *RouteResult {
	if questions, _ := input["questions"].([]any); len(questions) == 0 {
		return &RouteResult{Outcome: RouteOutcomeNotForwarded, Code: "invalid_tool_input", Text: "invalid_tool_input: questions is required", IsError: true}
	}
	text, isError, ok := t.customTool(ctx, callID, ToolNameAskUserQuestion, input)
	switch {
	case !ok:
		return withdrawnCall()
	case text == "":
		text = "The user gave no answer."
	case !isError:
		text = "The user answered: " + text
	}
	return &RouteResult{Outcome: RouteOutcomeExecuted, Text: text, IsError: isError}
}
