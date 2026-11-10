package bmaexecutor

// The Claude Code profile of ACPAgent (ADR-0029 S3, ADR-0027 decision 8).
//
// The brain is the official claude-agent-acp
// (https://github.com/agentclientprotocol/claude-agent-acp, npm
// @agentclientprotocol/claude-agent-acp), started unmodified from a pinned
// release: the node owner installs that exact version (claudeAgentACPPins) and
// ACPAgentOptions.Command points at it. The binary is not patched, repackaged
// or re-signed, and its own login and settings are left alone.
//
// Credentials. Claude Code runs with the credentials of the node's owner
// (its login under HOME, or ANTHROPIC_API_KEY or a third-party provider's
// variables in ACPAgentOptions.Env). They never pass through BMA: the profile
// starts no model proxy, declares no harness_model, ignores the platform's
// model endpoint, and the SDK never reads or logs the owner's credentials.
//
// Tools. The agent is started with `tools: []` (no built-in tool) and one
// injected MCP server named "bma", served on loopback by the ACPAgent itself
// (acpToolServer): the six platform tools, mcp__bma__{bash,read,write,edit,
// glob,grep}. Every call of that server goes through the turn's toolRoute, so
// it is checked and run exactly as bma-dsh-acp's calls are: on this machine,
// or on the platform's gateway when the turn carries tools_mcp_* (a remote
// hands machine).
//
// Gates, all fail closed:
//   - session/new is followed by session/set_mode default and the mode is
//     asserted default (claude-agent-acp starts in "auto" unless the owner's
//     settings say otherwise; a transcript can bring back bypassPermissions);
//     a turn is refused when the mode is not default, and the tool server
//     refuses calls while it is not;
//   - the agent's system init message (requested with emitRawSDKMessages) must
//     list only the six platform tools plus the placeholder tools the pinned
//     version is known to add (claudeAgentACPPins); anything else fails the turn;
//   - a permission request for any tool that is not a platform tool is rejected;
//     for a platform tool the platform's policy decides (allow, deny, or ask the
//     platform's user through an interaction), not Claude Code's own mode.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// claudeAgentACPName is agentInfo.name of the supported agent.
const claudeAgentACPName = "@agentclientprotocol/claude-agent-acp"

// claudeAgentACPPins are the claude-agent-acp releases this SDK is tested with
// (npm integrity of 0.101.0: sha512-c9YGBfdJrLcKiinRpXuDuibC+CRGqQpe9+ey/+TuuZ7ejHlVyEELbbvRkxHZN+Hf5XGecJD5i0kvP2opCseATA==)
// and, per release, the placeholder tools Claude Code reports besides the
// platform tools. Adding a release is a reviewed change: check that its tool
// list is still only the injected tools and what it adds.
var claudeAgentACPPins = map[string][]string{
	// DeferredToolPlaceholder: "Reserved placeholder that keeps deferred tool
	// loading active; never call this tool." (in the model's tool list of every
	// request; also allowed in the init message in case a release reports it).
	"0.101.0": {"DeferredToolPlaceholder"},
}

const (
	claudeMCPServerName = "bma"
	claudeToolPrefix    = "mcp__" + claudeMCPServerName + "__"
	claudeToolUseIDMeta = "claudecode/toolUseId"
)

func claudeCheckPin(name, version string) error {
	if name != claudeAgentACPName {
		return fmt.Errorf("the Claude Code profile needs %s, the agent is %q", claudeAgentACPName, name)
	}
	if _, ok := claudeAgentACPPins[version]; !ok {
		pinned := make([]string, 0, len(claudeAgentACPPins))
		for v := range claudeAgentACPPins {
			pinned = append(pinned, v)
		}
		slices.Sort(pinned)
		return fmt.Errorf("claude-agent-acp %s is not a pinned release (supported: %s): refused (fail-closed)", version, strings.Join(pinned, ", "))
	}
	return nil
}

// claudePlatformTool maps Claude Code's tool name to the platform tool's name.
func claudePlatformTool(claudeName string) (string, bool) {
	name, ok := strings.CutPrefix(claudeName, claudeToolPrefix)
	return name, ok && slices.Contains(acpPlatformTools, name)
}

// acpPlatformTools are the six platform tools (CMA agent_toolset names).
var acpPlatformTools = []string{ToolNameBash, ToolNameEdit, ToolNameGlob, ToolNameGrep, ToolNameRead, ToolNameWrite}

// policyOf is the turn's permission for a tool (an unlisted tool asks).
func (t *acpTurn) policyOf(name string) string {
	if p := t.policy[name]; p != "" {
		return p
	}
	return "ask"
}

// --- session ---

func (a *ACPAgent) openClaudeSession(ctx context.Context, proc *acpProcess, s *acpSession, params TurnStartParams, cwd, systemPrompt string) error {
	mcpServers := []any{map[string]any{
		"type": "http", "name": claudeMCPServerName, "url": a.mcp.url + "/mcp",
		"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer " + s.placeholder}},
	}}
	meta := map[string]any{
		"systemPrompt": map[string]any{"append": systemPrompt},
		"claudeCode": map[string]any{
			"options": map[string]any{
				"tools":                           []any{}, // no built-in tool: the injected platform tools only
				"allowDangerouslySkipPermissions": false,
			},
			"emitRawSDKMessages": []any{map[string]any{"type": "system", "subtype": "init"}},
		},
	}
	var res acpSessionResponse
	var err error
	if ref := params.ExecutorSessionRef; ref != "" {
		a.mu.Lock()
		s.proc, s.agentID, s.agentMode = proc, ref, ""
		a.mu.Unlock()
		err = proc.call(ctx, "session/load", map[string]any{"sessionId": ref, "cwd": cwd, "mcpServers": mcpServers, "_meta": meta}, &res)
		if err != nil {
			a.mu.Lock()
			s.proc, s.agentID = nil, ""
			a.mu.Unlock()
		}
		var acpErr *acpError
		if errors.As(err, &acpErr) && acpErr.Code == CodeNotFound {
			return &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + ref + " was not found; it is not recreated"}
		}
	} else {
		err = proc.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": mcpServers, "_meta": meta}, &res)
		if err == nil {
			if res.SessionID == "" {
				return &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: "agent session: session/new answered without a sessionId"}
			}
			a.mu.Lock()
			s.proc, s.agentID, s.agentMode = proc, res.SessionID, ""
			a.mu.Unlock()
		}
	}
	if err != nil {
		return acpOpenError(proc, "agent session", err)
	}
	return nil
}

// claudeDefaultMode makes sure the agent's session mode is "default" and
// asserts it. The mode a session starts in is the agent's (auto, or what the
// owner's settings say), so session/set_mode default follows every
// session/new and session/load; a mode that is not default afterwards
// refuses the turn.
func (a *ACPAgent) claudeDefaultMode(ctx context.Context, proc *acpProcess, s *acpSession) error {
	a.mu.Lock()
	mode := s.agentMode
	a.mu.Unlock()
	if mode != "default" {
		// A successful set_mode means default unless the agent says otherwise
		// first (it reports a clamped mode with current_mode_update, before the answer).
		a.mu.Lock()
		s.agentMode = "default"
		a.mu.Unlock()
		if err := proc.call(ctx, "session/set_mode", map[string]any{"sessionId": s.wire(), "modeId": "default"}, nil); err != nil {
			a.mu.Lock()
			s.agentMode = ""
			a.mu.Unlock()
			return acpOpenError(proc, "agent set_mode", err)
		}
	}
	a.mu.Lock()
	mode = s.agentMode
	a.mu.Unlock()
	if mode != "default" {
		return &AgentError{Code: AgentErrorCodeAgentError, Message: fmt.Sprintf("mode_not_default: the agent's permission mode is %q, not default; the turn is refused", mode)}
	}
	return nil
}

// trackClaudeMode follows the agent's reports of its permission mode.
func (a *ACPAgent) trackClaudeMode(proc *acpProcess, wire string, u acpUpdate) {
	mode := ""
	switch u.Kind {
	case "current_mode_update":
		mode = u.CurrentModeID
	case "config_option_update":
		var options []struct {
			ID           string          `json:"id"`
			CurrentValue json.RawMessage `json:"currentValue"`
		}
		if json.Unmarshal(u.ConfigOptions, &options) != nil {
			return
		}
		for _, o := range options {
			if o.ID == "mode" {
				_ = json.Unmarshal(o.CurrentValue, &mode)
			}
		}
	}
	if mode == "" {
		return
	}
	s := a.sessionOf(proc, wire)
	if s == nil {
		return
	}
	a.mu.Lock()
	s.agentMode = mode
	t := s.turn
	a.mu.Unlock()
	if mode != "default" && t != nil {
		t.failClaude(proc, wire, "mode_not_default", fmt.Sprintf("the agent's permission mode became %q during the turn", mode))
	}
}

// onClaudeSDKMessage checks Claude Code's system init message: its tool list
// and its permission mode.
func (a *ACPAgent) onClaudeSDKMessage(proc *acpProcess, raw json.RawMessage) {
	var n struct {
		SessionID string `json:"sessionId"`
		Message   struct {
			Type           string   `json:"type"`
			Subtype        string   `json:"subtype"`
			Tools          []string `json:"tools"`
			PermissionMode string   `json:"permissionMode"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &n) != nil || n.Message.Type != "system" || n.Message.Subtype != "init" {
		return
	}
	t := a.turnFor(proc, n.SessionID)
	if t == nil {
		return
	}
	allowed := claudeAgentACPPins[proc.init.AgentInfo.Version]
	for _, tool := range n.Message.Tools {
		if _, ok := claudePlatformTool(tool); !ok && !slices.Contains(allowed, tool) {
			t.failClaude(proc, n.SessionID, "tool_catalog_violation",
				fmt.Sprintf("claude code offers the tool %q, which is neither a platform tool nor a known placeholder of claude-agent-acp %s", tool, proc.init.AgentInfo.Version))
			return
		}
	}
	if n.Message.PermissionMode != "" && n.Message.PermissionMode != "default" {
		t.failClaude(proc, n.SessionID, "mode_not_default", fmt.Sprintf("claude code started the turn in the permission mode %q", n.Message.PermissionMode))
	}
}

// --- turn ---

// acpClaudeTurn is the Claude Code profile's state of a turn.
type acpClaudeTurn struct {
	violation atomic.Pointer[string]
	chunks    []*claudeChunk // message text not yet committed, in order
}

type claudeChunk struct {
	kind, messageID string
	text            strings.Builder
}

// failClaude ends the turn with an error (the first one wins) and asks the
// agent to stop.
func (t *acpTurn) failClaude(proc *acpProcess, wire, code, detail string) {
	message := code + ": " + detail
	if !t.claude.violation.CompareAndSwap(nil, &message) {
		return
	}
	t.agent.log(LogError, "claude code turn "+t.turnID+" refused: "+message, nil)
	if err := proc.notify("session/cancel", map[string]any{"sessionId": wire}); err != nil {
		t.agent.log(LogDebug, "acp session/cancel", err)
	}
}

func (t *acpTurn) violationError() error {
	if v := t.claude.violation.Load(); v != nil {
		return errors.New(*v)
	}
	return nil
}

func (t *acpTurn) updateClaude(u acpUpdate) {
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
			return
		}
		eventType := EventDeltaTypeAgentMessage
		if u.Kind == "agent_thought_chunk" {
			eventType = EventDeltaTypeAgentThinking
		}
		id := t.eventID(u.Kind + "\x00" + u.MessageID)
		t.mu.Lock()
		index := t.index[id]
		t.index[id]++
		t.addChunk(u.Kind, u.MessageID, c.Text)
		t.mu.Unlock()
		t.turn.Delta(id, eventType, index, c.Text)
	case "tool_call", "tool_call_update":
		t.flushMessages()
		// The input arrives with the first update that has it (the tool_call
		// itself carries none).
		if len(u.RawInput) > 0 {
			name, ok := claudePlatformTool(u.Meta.ClaudeCode.ToolName)
			if !ok {
				name = u.Meta.ClaudeCode.ToolName
			}
			t.toolUse(u.ToolCallID, name, u.RawInput, t.policyOf(name))
		}
		if u.Kind == "tool_call_update" {
			t.toolResult(u)
		}
	}
}

// addChunk appends to the message text; t.mu is held.
func (t *acpTurn) addChunk(kind, messageID, text string) {
	for _, c := range t.claude.chunks {
		if c.kind == kind && c.messageID == messageID {
			c.text.WriteString(text)
			return
		}
	}
	c := &claudeChunk{kind: kind, messageID: messageID}
	c.text.WriteString(text)
	t.claude.chunks = append(t.claude.chunks, c)
}

// flushMessages commits the streamed message text as durable events (Claude
// Code has no separate "this message is complete" notification; a message is
// complete when something else follows it, or the prompt ends).
func (t *acpTurn) flushMessages() {
	t.mu.Lock()
	chunks := t.claude.chunks
	t.claude.chunks = nil
	t.mu.Unlock()
	for _, c := range chunks {
		kind := "message"
		if c.kind == "agent_thought_chunk" {
			kind = "thinking"
		}
		t.commit(c.messageID, kind, c.text.String())
	}
}

// --- the platform tools as an MCP server on loopback ---

// acpToolServer serves the six platform tools to Claude Code over MCP
// Streamable HTTP (JSON replies; tools/call as an SSE stream). A request carries the session's
// placeholder as its bearer token, so a session can only reach its own turn.
type acpToolServer struct {
	agent  *ACPAgent
	url    string
	server *http.Server
	anon   atomic.Int64
}

func startACPToolServer(a *ACPAgent) (*acpToolServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &acpToolServer{agent: a, url: "http://" + listener.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.serve)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

func (s *acpToolServer) close() { _ = s.server.Close() }

const acpMaxToolRequestBytes = 32 << 20

// sessionFor is the session whose placeholder is token.
func (a *ACPAgent) sessionFor(token string) *acpSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if subtle.ConstantTimeCompare([]byte(s.placeholder), []byte(token)) == 1 {
			return s
		}
	}
	return nil
}

func (s *acpToolServer) serve(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	session := s.agent.sessionFor(token)
	if token == "" || session == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost { // no server-initiated stream
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, acpMaxToolRequestBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification or a response
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any, rpcErr map[string]any) {
		out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			out["error"] = rpcErr
		} else {
			out["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := "2025-06-18"
		if slices.Contains([]string{"2025-03-26", "2025-06-18", "2025-11-25"}, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		reply(map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "bma-executor-sdk", "version": "0"}}, nil)
	case "ping":
		reply(map[string]any{}, nil)
	case "tools/list":
		reply(map[string]any{"tools": claudeToolList()}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || !slices.Contains(acpPlatformTools, p.Name) {
			reply(nil, map[string]any{"code": -32602, "message": "unknown tool " + p.Name})
			return
		}
		callID, _ := p.Meta[claudeToolUseIDMeta].(string)
		// A call can outlast Claude Code's first-byte budget for an HTTP MCP
		// request (60 s: a person deciding an approval, a brain reconnecting,
		// a long command). Answer it as a Streamable HTTP SSE stream: the
		// headers go out at once, the result is its one event.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"result": s.call(r.Context(), session, callID, p.Name, p.Arguments)})
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	default:
		reply(nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
	}
}

// call runs one tool call of Claude Code through the turn's route.
func (s *acpToolServer) call(ctx context.Context, session *acpSession, callID, name string, input map[string]any) map[string]any {
	a := s.agent
	a.mu.Lock()
	t, mode := session.turn, session.agentMode
	a.mu.Unlock()
	var result *RouteResult
	switch {
	case t == nil:
		result = refused("stale_turn", "no turn of this session is running")
	case mode != "default":
		result = refused("mode_not_default", fmt.Sprintf("the agent's permission mode is %q, not default", mode))
	default:
		if callID == "" {
			// Claude Code names the call; without a name it cannot have been approved.
			callID = fmt.Sprintf("mcp-%d", s.anon.Add(1))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(t.ctx, cancel)()
		t.mu.Lock()
		toolUseID := t.tools[callID]
		t.mu.Unlock()
		result = t.toolRoute.Call(ctx, RouteCall{ID: callID, ToolUseID: toolUseID, Name: name, Input: input})
	}
	// No structuredContent: Claude Code would show the model that instead of
	// the text. The outcome is in the text ("<code>: ..." for a refused or
	// unknown call).
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": result.Text}}, "isError": result.IsError}
}

func claudeToolList() []any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	boolean := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	tool := func(name, desc string, required []string, props map[string]any) map[string]any {
		return map[string]any{"name": name, "description": desc,
			"inputSchema": map[string]any{"type": "object", "properties": props, "required": required}}
	}
	return []any{
		tool(ToolNameBash, "Run a shell command in the working directory. A command that runs longer than the wait is left running; the call then returns its pid and the file its output goes to.",
			[]string{"command"}, map[string]any{
				"command":     str("The command to run."),
				"timeout":     integer("How long to wait for the command, in milliseconds (default 120000, at most 600000)."),
				"on_timeout":  map[string]any{"type": "string", "enum": []string{"background", "kill"}, "description": "At the end of the wait: leave the command running (default) or stop it."},
				"description": str("What the command does."),
			}),
		tool(ToolNameRead, "Read a file.", []string{"file_path"}, map[string]any{
			"file_path": str("Path of the file."), "offset": integer("First line to read."), "limit": integer("Number of lines to read."),
		}),
		tool(ToolNameWrite, "Write a file, replacing its content.", []string{"file_path", "content"}, map[string]any{
			"file_path": str("Path of the file."), "content": str("The new content."),
		}),
		tool(ToolNameEdit, "Replace text in a file.", []string{"file_path", "old_string", "new_string"}, map[string]any{
			"file_path": str("Path of the file."), "old_string": str("The text to replace."), "new_string": str("The replacement."),
			"replace_all": boolean("Replace every occurrence instead of exactly one."),
		}),
		tool(ToolNameGlob, "Find files by glob pattern.", []string{"pattern"}, map[string]any{
			"pattern": str("The glob pattern."), "path": str("Directory to search in."),
		}),
		tool(ToolNameGrep, "Search file contents with a regular expression.", []string{"pattern"}, map[string]any{
			"pattern": str("The regular expression."), "path": str("File or directory to search in."), "glob": str("Only files matching this glob."),
			"output_mode":      map[string]any{"type": "string", "enum": []string{"content", "files_with_matches", "count"}, "description": "What to return."},
			"case_insensitive": boolean("Ignore case."),
		}),
	}
}
