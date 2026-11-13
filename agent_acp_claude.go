package bmaexecutor

// The claude-agent-acp adapter (ADR-0029 S3, ADR-0027 decision 10): the
// ACPProfileClaudeCode profile of NewACPAgent.
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
// variables in ACPAgentOptions.Env). They never pass through BMA: the adapter
// starts no model proxy, refuses harness_model, and the SDK never reads or
// logs the owner's credentials.
//
// Tools. The agent is started with `tools: []` (no built-in tool) and one
// injected MCP server named "bma", served on loopback by the adapter itself
// (acpToolServer): the six platform tools, mcp__bma__{bash,read,write,edit,
// glob,grep}. Every call of that server goes to the host (BrainSink.Call), so
// it is checked and run exactly as bma-dsh-acp's calls are.
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
//     for a platform tool the host decides (BrainSink.Permission), not Claude
//     Code's own mode.
//   - its idle auto-compaction (a /compact turn of its own shortly before the
//     prompt cache of an idle session expires, spending the owner's credit)
//     is put off past any session's life: claudeIdleCompactionOff.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// claudeIdleCompactionOff sets claude-agent-acp's idle compaction delay to the
// longest timer Node takes (about 24.8 days; idle-compaction.ts reads
// CLAUDE_AGENT_ACP_IDLE_COMPACT_DELAY_MS from the agent's environment), so the
// agent never starts a turn the platform did not ask for. Claude Code's own
// compaction of a running turn is unchanged.
const claudeIdleCompactionOff = "CLAUDE_AGENT_ACP_IDLE_COMPACT_DELAY_MS=2147483647"

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

func newClaudeACPAdapter() *claudeACPAdapter {
	return &claudeACPAdapter{convs: map[string]*claudeACPConv{}}
}

type claudeACPAdapter struct {
	env  BrainEnv
	mcp  *acpToolServer
	warm *acpPrewarm

	mu     sync.Mutex
	proc   *acpProcess
	convs  map[string]*claudeACPConv // by the agent's session id
	closed bool
}

type claudeACPConv struct {
	a           *claudeACPAdapter
	placeholder string
	proc        *acpProcess
	// agentID is the agent's own session id, agentMode the permission mode it reported last.
	agentID   string
	agentMode string
	prompt    *claudeACPPrompt
}

// claudeACPPrompt is the input a conversation is running.
type claudeACPPrompt struct {
	ctx       context.Context
	sink      BrainSink
	violation atomic.Pointer[string]
	mu        sync.Mutex
	chunks    []*claudeChunk // message text not yet committed, in order
}

type claudeChunk struct {
	kind, messageID string
	text            strings.Builder
}

func (a *claudeACPAdapter) log(level LogLevel, message string, err error) {
	if a.env.Log != nil {
		a.env.Log(level, message, err)
	}
}

func (a *claudeACPAdapter) Start(ctx context.Context, env BrainEnv) (BrainCapabilities, error) {
	if env.PlatformModel {
		return BrainCapabilities{}, errors.New("bmaexecutor: the Claude Code profile uses the node owner's own credentials; PlatformModel (harness_model) is not allowed")
	}
	if len(env.Models) > 0 {
		return BrainCapabilities{}, errors.New("bmaexecutor: ACPAgentOptions.Models is for the DSH profile; Claude Code uses the model its owner configured")
	}
	a.env = env
	server, err := startACPToolServer(a)
	if err != nil {
		return BrainCapabilities{}, err
	}
	a.mcp = server
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		server.close()
		return BrainCapabilities{}, err
	}
	id := env.HarnessID
	if id == "" {
		id = "claude-code"
	}
	init := proc.init
	a.startPrewarm()
	return BrainCapabilities{
		Harness: AgentHarnessCapability{
			ID: id, Version: init.AgentInfo.Version, Protocol: "acp/1",
			AgentName: init.AgentInfo.Name, AgentVersion: init.AgentInfo.Version,
			Streaming: "token", PermissionRelay: "full", SessionLoad: init.AgentCapabilities.LoadSession,
		},
		ModelCredentials: BrainModelOwner,
	}, nil
}

// Ref: the agent names a new conversation itself.
func (a *claudeACPAdapter) Ref(string) string { return "" }

func (a *claudeACPAdapter) Close() error {
	a.mu.Lock()
	a.closed = true
	proc := a.proc
	a.mu.Unlock()
	if proc != nil {
		proc.kill(a.env.KillGrace)
	}
	if a.mcp != nil {
		a.mcp.close()
	}
	return nil
}

func (a *claudeACPAdapter) ensureProcess(ctx context.Context) (*acpProcess, error) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errors.New("agent is closed")
	}
	if a.proc != nil && a.proc.alive() {
		proc := a.proc
		a.mu.Unlock()
		return proc, nil
	}
	a.mu.Unlock()
	proc, err := startACPProcess(ctx, acpLaunch{env: a.env, extraEnv: []string{claudeIdleCompactionOff}}, a)
	if err != nil {
		return nil, err
	}
	if err := claudeCheckPin(proc.init.AgentInfo.Name, proc.init.AgentInfo.Version); err != nil {
		proc.kill(a.env.KillGrace)
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc != nil && a.proc.alive() {
		go proc.kill(a.env.KillGrace)
		return a.proc, nil
	}
	a.proc = proc
	return proc, nil
}

func (a *claudeACPAdapter) Open(ctx context.Context, req BrainOpen) (BrainSession, error) {
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "agent unavailable: " + err.Error()}
	}
	c, _ := req.Current.(*claudeACPConv)
	if c == nil || c.proc != proc {
		placeholder := ""
		if c != nil {
			placeholder = c.placeholder
		}
		if c, err = a.openSession(ctx, proc, placeholder, req.Ref, req.Cwd, req.SystemPrompt); err != nil {
			return nil, err
		}
	}
	if err := a.defaultMode(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func newPlaceholder() string {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	return "bma-local-" + hex.EncodeToString(key)
}

// openSession opens (session/new) or loads (session/load ref) a conversation.
func (a *claudeACPAdapter) openSession(ctx context.Context, proc *acpProcess, placeholder, ref, cwd, systemPrompt string) (*claudeACPConv, error) {
	return a.openConv(ctx, proc, placeholder, ref, cwd, systemPrompt, true)
}

// openConv opens a conversation; register makes the adapter route the
// agent's notifications to it (not for the prewarm's throwaway one).
func (a *claudeACPAdapter) openConv(ctx context.Context, proc *acpProcess, placeholder, ref, cwd, systemPrompt string, register bool) (*claudeACPConv, error) {
	if placeholder == "" {
		placeholder = newPlaceholder()
	}
	c := &claudeACPConv{a: a, placeholder: placeholder}
	mcpServers := []any{map[string]any{
		"type": "http", "name": claudeMCPServerName, "url": a.mcp.url + "/mcp",
		"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer " + placeholder}},
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
	if ref != "" {
		a.register(c, proc, ref)
		err = proc.call(ctx, "session/load", map[string]any{"sessionId": ref, "cwd": cwd, "mcpServers": mcpServers, "_meta": meta}, &res)
		if err != nil {
			a.forget(c)
		}
		var acpErr *acpError
		if errors.As(err, &acpErr) && acpErr.Code == CodeNotFound {
			return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + ref + " was not found; it is not recreated"}
		}
	} else {
		err = proc.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": mcpServers, "_meta": meta}, &res)
		if err == nil {
			if res.SessionID == "" {
				return nil, &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: "agent session: session/new answered without a sessionId"}
			}
			if register {
				a.register(c, proc, res.SessionID)
			} else {
				c.proc, c.agentID = proc, res.SessionID
			}
		}
	}
	if err != nil {
		return nil, acpOpenError(proc, "agent session", err)
	}
	return c, nil
}

func (a *claudeACPAdapter) register(c *claudeACPConv, proc *acpProcess, agentID string) {
	a.mu.Lock()
	c.proc, c.agentID, c.agentMode = proc, agentID, ""
	a.convs[agentID] = c
	a.mu.Unlock()
}

func (a *claudeACPAdapter) forget(c *claudeACPConv) {
	a.mu.Lock()
	if a.convs[c.agentID] == c {
		delete(a.convs, c.agentID)
	}
	a.mu.Unlock()
}

// defaultMode makes sure the agent's session mode is "default" and asserts
// it. The mode a session starts in is the agent's (auto, or what the owner's
// settings say), so session/set_mode default follows every session/new and
// session/load; a mode that is not default afterwards refuses the turn.
func (a *claudeACPAdapter) defaultMode(ctx context.Context, c *claudeACPConv) error {
	a.mu.Lock()
	mode := c.agentMode
	a.mu.Unlock()
	if mode != "default" {
		// A successful set_mode means default unless the agent says otherwise
		// first (it reports a clamped mode with current_mode_update, before the answer).
		a.mu.Lock()
		c.agentMode = "default"
		a.mu.Unlock()
		if err := c.proc.call(ctx, "session/set_mode", map[string]any{"sessionId": c.agentID, "modeId": "default"}, nil); err != nil {
			a.mu.Lock()
			c.agentMode = ""
			a.mu.Unlock()
			return acpOpenError(c.proc, "agent set_mode", err)
		}
	}
	a.mu.Lock()
	mode = c.agentMode
	a.mu.Unlock()
	if mode != "default" {
		return &AgentError{Code: AgentErrorCodeAgentError, Message: fmt.Sprintf("mode_not_default: the agent's permission mode is %q, not default; the turn is refused", mode)}
	}
	return nil
}

func (c *claudeACPConv) Ref() string { return c.agentID }

func (c *claudeACPConv) Ready() bool { return c.proc != nil && c.proc.alive() }

func (c *claudeACPConv) Prompt(ctx context.Context, in BrainInput, sink BrainSink) error {
	a := c.a
	p := &claudeACPPrompt{ctx: ctx, sink: sink}
	a.mu.Lock()
	c.prompt = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		c.prompt = nil
		a.mu.Unlock()
	}()
	err := promptACP(ctx, c.proc, map[string]any{"sessionId": c.agentID, "prompt": acpPromptBlocks(in)}, a.env.CancelGrace, a.env.KillGrace)
	p.flush()
	if v := p.violation.Load(); v != nil {
		return errors.New(*v)
	}
	return err
}

func (c *claudeACPConv) Suspend(ctx context.Context) error {
	if c.proc != nil && c.proc.alive() {
		err := c.proc.call(ctx, "session/close", map[string]any{"sessionId": c.agentID}, nil)
		var acpErr *acpError
		if err != nil && !(errors.As(err, &acpErr) && acpErr.Code == CodeNotFound) && c.proc.alive() {
			return fmt.Errorf("agent session/close: %w", err)
		}
	}
	c.a.forget(c)
	return nil
}

func (c *claudeACPConv) Close(ctx context.Context) error {
	c.a.forget(c)
	if c.proc != nil && c.proc.alive() {
		return c.proc.call(ctx, "session/close", map[string]any{"sessionId": c.agentID}, nil)
	}
	return nil
}

func (a *claudeACPAdapter) conv(proc *acpProcess, wire string) *claudeACPConv {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.convs[wire]; c != nil && c.proc == proc {
		return c
	}
	return nil
}

func (a *claudeACPAdapter) running(proc *acpProcess, wire string) (*claudeACPConv, *claudeACPPrompt) {
	c := a.conv(proc, wire)
	if c == nil {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return c, c.prompt
}

// fail ends the prompt with an error (the first one wins) and asks the agent to stop.
func (p *claudeACPPrompt) fail(a *claudeACPAdapter, proc *acpProcess, wire, code, detail string) {
	message := code + ": " + detail
	if !p.violation.CompareAndSwap(nil, &message) {
		return
	}
	a.log(LogError, "claude code turn refused: "+message, nil)
	if err := proc.notify("session/cancel", map[string]any{"sessionId": wire}); err != nil {
		a.log(LogDebug, "acp session/cancel", err)
	}
}

// add appends to the message text.
func (p *claudeACPPrompt) add(kind, messageID, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.chunks {
		if c.kind == kind && c.messageID == messageID {
			c.text.WriteString(text)
			return
		}
	}
	c := &claudeChunk{kind: kind, messageID: messageID}
	c.text.WriteString(text)
	p.chunks = append(p.chunks, c)
}

// flush commits the streamed message text as durable messages (Claude Code
// has no separate "this message is complete" notification; a message is
// complete when something else follows it, or the prompt ends).
func (p *claudeACPPrompt) flush() {
	p.mu.Lock()
	chunks := p.chunks
	p.chunks = nil
	p.mu.Unlock()
	for _, c := range chunks {
		p.sink.Message(c.kind, c.messageID, c.text.String())
	}
}

// --- agent -> executor ---

func (a *claudeACPAdapter) onNotification(proc *acpProcess, method string, raw json.RawMessage) {
	switch method {
	case "session/update":
		var n struct {
			SessionID string    `json:"sessionId"`
			Update    acpUpdate `json:"update"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return
		}
		a.trackMode(proc, n.SessionID, n.Update)
		_, p := a.running(proc, n.SessionID)
		if p == nil {
			return
		}
		u := n.Update
		switch u.Kind {
		case "agent_message_chunk", "agent_thought_chunk":
			if text, ok := u.chunkText(); ok {
				kind := acpChunkKind(u.Kind)
				p.add(kind, u.MessageID, text)
				p.sink.Delta(kind, u.MessageID, text)
			}
		case "tool_call", "tool_call_update":
			p.flush()
			// The input arrives with the first update that has it (the tool_call
			// itself carries none).
			if len(u.RawInput) > 0 {
				name, ok := claudePlatformTool(u.Meta.ClaudeCode.ToolName)
				if !ok {
					name = u.Meta.ClaudeCode.ToolName
				}
				p.sink.ToolUse(BrainCall{ID: u.ToolCallID, Name: name, Input: u.RawInput}, "")
			}
			if content, isError, done := u.toolResult(); u.Kind == "tool_call_update" && done {
				p.sink.ToolResult(u.ToolCallID, content, isError)
			}
		}
	case "_claude/sdkMessage":
		a.onSDKMessage(proc, raw)
	}
}

// trackMode follows the agent's reports of its permission mode.
func (a *claudeACPAdapter) trackMode(proc *acpProcess, wire string, u acpUpdate) {
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
	c := a.conv(proc, wire)
	if c == nil {
		return
	}
	a.mu.Lock()
	c.agentMode = mode
	p := c.prompt
	a.mu.Unlock()
	if mode != "default" && p != nil {
		p.fail(a, proc, wire, "mode_not_default", fmt.Sprintf("the agent's permission mode became %q during the turn", mode))
	}
}

// onSDKMessage checks Claude Code's system init message: its tool list and
// its permission mode.
func (a *claudeACPAdapter) onSDKMessage(proc *acpProcess, raw json.RawMessage) {
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
	_, p := a.running(proc, n.SessionID)
	if p == nil {
		return
	}
	allowed := claudeAgentACPPins[proc.init.AgentInfo.Version]
	for _, tool := range n.Message.Tools {
		if _, ok := claudePlatformTool(tool); !ok && !slices.Contains(allowed, tool) {
			p.fail(a, proc, n.SessionID, "tool_catalog_violation",
				fmt.Sprintf("claude code offers the tool %q, which is neither a platform tool nor a known placeholder of claude-agent-acp %s", tool, proc.init.AgentInfo.Version))
			return
		}
	}
	if n.Message.PermissionMode != "" && n.Message.PermissionMode != "default" {
		p.fail(a, proc, n.SessionID, "mode_not_default", fmt.Sprintf("claude code started the turn in the permission mode %q", n.Message.PermissionMode))
	}
}

func (a *claudeACPAdapter) onRequest(ctx context.Context, proc *acpProcess, method string, raw json.RawMessage) (any, *RPCError) {
	if method != "session/request_permission" {
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
	}
	var r acpPermissionRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	_, p := a.running(proc, r.SessionID)
	if p == nil {
		return acpPermissionOutcome(nil, BrainDecision{Outcome: BrainCanceled}), nil
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(p.ctx, cancel)()
	// Only the platform tools (the injected MCP server) may be used at all.
	name, ok := claudePlatformTool(r.ToolCall.Meta.ClaudeCode.ToolName)
	if !ok {
		a.log(LogWarn, fmt.Sprintf("claude code asked to use %q, which is not a platform tool: refused", r.ToolCall.Meta.ClaudeCode.ToolName), nil)
		return acpPermissionOutcome(r.Options, BrainDecision{Outcome: BrainDeny}), nil
	}
	return acpPermissionOutcome(r.Options, p.sink.Permission(ctx, BrainCall{ID: r.ToolCall.ToolCallID, Name: name, Input: r.ToolCall.RawInput})), nil
}

// --- prewarm ---

// Starting Claude Code for the first time in a process costs most of a
// second more than later sessions (measured: the first session/new of a
// claude-agent-acp process ~1.1 s, the next ones ~0.2 s), and without it the
// first turn after registration pays that. Start opens one throwaway session
// in the background, in a scratch directory under StateDir with the same
// options a real session gets, and closes it without ever prompting it (a
// prompt would spend the owner's model credit). A failure is logged at
// LogWarn ("agent prewarm failed") and returned by WaitPrewarm.

type acpPrewarm struct {
	done chan struct{}
	err  error
}

func (a *claudeACPAdapter) startPrewarm() {
	if a.env.DisablePrewarm {
		return
	}
	a.warm = &acpPrewarm{done: make(chan struct{})}
	go func() {
		defer close(a.warm.done)
		ctx, cancel := context.WithTimeout(context.Background(), acpOpenTimeout)
		defer cancel()
		began := time.Now()
		if a.warm.err = a.prewarmSession(ctx); a.warm.err != nil {
			a.log(LogWarn, "agent prewarm failed; the first turn opens its session itself", a.warm.err)
			return
		}
		a.log(LogInfo, fmt.Sprintf("agent prewarmed in %s", time.Since(began).Round(time.Millisecond)), nil)
	}()
}

// WaitPrewarm waits for the background prewarm and returns its error (nil at
// once with DisablePrewarm).
func (a *claudeACPAdapter) WaitPrewarm(ctx context.Context) error {
	if a.warm == nil {
		return nil
	}
	select {
	case <-a.warm.done:
		return a.warm.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *claudeACPAdapter) prewarmSession(ctx context.Context) error {
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return err
	}
	cwd := filepath.Join(a.env.StateDir, "prewarm")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return err
	}
	if runAs := a.env.RunAs; runAs != nil {
		if err := os.Chown(cwd, int(runAs.UID), int(runAs.GID)); err != nil {
			return err
		}
	}
	c, err := a.openConv(ctx, proc, "bma-prewarm-"+fmt.Sprint(time.Now().UnixNano()), "", cwd, a.env.SystemPrompt(cwd), false)
	if err != nil {
		return err
	}
	if err := proc.call(ctx, "session/close", map[string]any{"sessionId": c.agentID}, nil); err != nil {
		a.log(LogDebug, "acp session/close of the prewarm session", err)
	}
	return nil
}

// --- the platform tools as an MCP server on loopback ---

// acpToolServer serves the six platform tools to Claude Code over MCP
// Streamable HTTP (JSON replies; tools/call as an SSE stream). A request carries the session's
// placeholder as its bearer token, so a session can only reach its own turn.
type acpToolServer struct {
	agent  *claudeACPAdapter
	url    string
	server *http.Server
	anon   atomic.Int64
}

func startACPToolServer(a *claudeACPAdapter) (*acpToolServer, error) {
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

// convFor is the conversation whose placeholder is token.
func (a *claudeACPAdapter) convFor(token string) *claudeACPConv {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.convs {
		if subtle.ConstantTimeCompare([]byte(c.placeholder), []byte(token)) == 1 {
			return c
		}
	}
	return nil
}

func (s *acpToolServer) serve(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	conv := s.agent.convFor(token)
	if token == "" || conv == nil {
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
			"result": s.call(r.Context(), conv, callID, p.Name, p.Arguments)})
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	default:
		reply(nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
	}
}

// call runs one tool call of Claude Code through the host.
func (s *acpToolServer) call(ctx context.Context, c *claudeACPConv, callID, name string, input map[string]any) map[string]any {
	a := s.agent
	a.mu.Lock()
	p, mode := c.prompt, c.agentMode
	a.mu.Unlock()
	var result *RouteResult
	switch {
	case p == nil:
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
		defer context.AfterFunc(p.ctx, cancel)()
		result = p.sink.Call(ctx, BrainCall{ID: callID, Name: name, Input: input})
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
