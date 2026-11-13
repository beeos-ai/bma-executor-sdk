package bmaexecutor

// The DSH adapter: bma-dsh-acp, the shared DSH adapter of BMA as an ACP agent
// (ADR-0027), driven over ACP v1 on stdio.
//
//	BrainHost / BrainSink                 DSH adapter                       bma-dsh-acp (ACP)
//	Open                                  open/load the session             session/new | session/load (_meta.bma, customTools)
//	                                      permission mode                   session/set_mode
//	Prompt                                prompt                            session/prompt (_meta.bma.inputId, toolPolicy, reasoningEffort,
//	                                                                        hostTools, inputSkills)
//	Delta                                 <-                                agent_message_chunk / agent_thought_chunk (messageId)
//	Message                               <-                                _bma/message (durable text of a messageId)
//	ToolUse                               <-                                tool_call (_meta.bma.name, permission)
//	Permission                            <->                               session/request_permission
//	Call                                  <->                               _bma/tools/call
//	ToolResult                            <-                                tool_call_update completed | failed
//	ctx canceled                          ->                                session/cancel; kill after CancelGrace
//
// One agent process serves every session (about 2 MiB each on top of about
// 90 MiB, see its README). When it does not answer a cancel within
// CancelGrace its process group is killed, which ends the other running turns
// too; the next turn starts a new process and loads its conversation.
//
// Model requests go to a loopback proxy with a per-session placeholder key
// (agent_acp_proxy.go); the proxy forwards only during a running prompt,
// refuses a request whose tools are not the allowed tools, and adds the
// turn's real credential.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
)

// newDSHAdapter is the adapter of bma-dsh-acp.
func newDSHAdapter() *dshAdapter {
	return &dshAdapter{convs: map[string]*dshConv{}}
}

type dshAdapter struct {
	env   BrainEnv
	proxy *acpModelProxy

	mu     sync.Mutex
	proc   *acpProcess
	convs  map[string]*dshConv // by platform session id (the id the agent knows)
	closed bool
}

type dshConv struct {
	a           *dshAdapter
	id          string
	placeholder string
	proc        *acpProcess
	mode        string
	// tools are the turn's tools besides the six (BrainOpen.Tools); the
	// custom tools among them are bound when the session opens.
	tools  []BrainTool
	prompt *dshPrompt
}

// dshPrompt is the input the conversation is running.
type dshPrompt struct {
	ctx   context.Context
	sink  BrainSink
	route BrainModelRoute
	// tools are the tools its model requests may carry.
	tools map[string]bool
}

// dshCustomTools are the custom tools among tools, as session _meta.bma.customTools.
func dshCustomTools(tools []BrainTool) []BrainTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t BrainTool) bool { return !t.Custom })
}

// dshHostTools are the host tools among tools, as prompt _meta.bma.hostTools.
func dshHostTools(tools []BrainTool) []BrainTool {
	return slices.DeleteFunc(slices.Clone(tools), func(t BrainTool) bool { return t.Custom })
}

func (a *dshAdapter) log(level LogLevel, message string, err error) {
	if a.env.Log != nil {
		a.env.Log(level, message, err)
	}
}

func (a *dshAdapter) Start(ctx context.Context, env BrainEnv) (BrainCapabilities, error) {
	if env.Upstream.BaseURL == "" && !env.PlatformModel {
		return BrainCapabilities{}, errors.New("bmaexecutor: ACPAgentOptions.Upstream.BaseURL is required unless PlatformModel is set")
	}
	a.env = env
	proxy, err := startACPModelProxy(a)
	if err != nil {
		return BrainCapabilities{}, err
	}
	a.proxy = proxy
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		proxy.close()
		return BrainCapabilities{}, err
	}
	init := proc.init
	id := env.HarnessID
	if id == "" {
		id = "dsh"
	}
	return BrainCapabilities{
		Harness: AgentHarnessCapability{
			ID: id, Version: init.AgentInfo.Version + "+dsh" + init.Meta.Bma.DSH, Protocol: "acp/1",
			AgentName: init.AgentInfo.Name, AgentVersion: init.AgentInfo.Version,
			Streaming: init.Meta.Bma.Streaming, PermissionRelay: init.Meta.Bma.PermissionRelay,
			SessionLoad: init.AgentCapabilities.LoadSession, ImageInputs: init.AgentCapabilities.PromptCapabilities.Image,
		},
		ModelCredentials: BrainModelPlatform,
		ModelChoice:      true,
		ThinkingLevels:   init.Meta.Bma.ReasoningEfforts,
		ImageInputs:      init.AgentCapabilities.PromptCapabilities.Image,
		HostTools:        true,
		CustomTools:      true,
		InputSkills:      true,
		InputIdempotent:  true,
	}, nil
}

func (a *dshAdapter) Ref(sessionID string) string { return sessionID }

func (a *dshAdapter) Close() error {
	a.mu.Lock()
	a.closed = true
	proc := a.proc
	a.mu.Unlock()
	if proc != nil {
		proc.kill(a.env.KillGrace)
	}
	if a.proxy != nil {
		a.proxy.close()
	}
	return nil
}

// ensureProcess returns the running agent process, starting (and checking) a new one when needed.
func (a *dshAdapter) ensureProcess(ctx context.Context) (*acpProcess, error) {
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
	proc, err := startACPProcess(ctx, acpLaunch{env: a.env, extraEnv: []string{"BMA_DSH_ACP_HOME=" + a.env.StateDir}}, a)
	if err != nil {
		return nil, err
	}
	if violation := acpCatalogViolation(proc.init.Meta.Bma.Tools); violation != "" {
		proc.kill(a.env.KillGrace)
		return nil, errors.New(violation)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc != nil && a.proc.alive() { // lost a start race
		go proc.kill(a.env.KillGrace)
		return a.proc, nil
	}
	a.proc = proc
	return proc, nil
}

func (a *dshAdapter) Open(ctx context.Context, req BrainOpen) (BrainSession, error) {
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "agent unavailable: " + err.Error()}
	}
	c, _ := req.Current.(*dshConv)
	if c != nil && c.proc == proc && !sameBrainTools(dshCustomTools(c.tools), dshCustomTools(req.Tools)) {
		// The agent binds its custom tools when the session opens: close it
		// and load it again (its conversation is durable) with the new ones.
		if err := c.Suspend(ctx); err != nil {
			return nil, acpOpenError(proc, "agent session/close", err)
		}
		req.Ref = c.id
		c.proc = nil
	}
	if c == nil || c.proc != proc {
		c = &dshConv{a: a, id: req.SessionID, placeholder: c.placeholderOrNew()}
		meta := map[string]any{"sessionId": req.SessionID, "systemPrompt": req.SystemPrompt, "model": a.modelMeta(c, req.Model, req.Metadata)}
		if custom := dshCustomTools(req.Tools); len(custom) > 0 {
			meta["customTools"] = custom
		}
		var res acpSessionResponse
		if req.Ref != "" {
			err = proc.call(ctx, "session/load", map[string]any{"sessionId": req.Ref, "cwd": req.Cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
			var acpErr *acpError
			if errors.As(err, &acpErr) && acpErr.Code == CodeNotFound {
				return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + req.Ref + " was not found; it is not recreated"}
			}
		} else {
			err = proc.call(ctx, "session/new", map[string]any{"cwd": req.Cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
		}
		if err != nil {
			return nil, acpOpenError(proc, "agent session", err)
		}
		if violation := acpToolsViolation(slices.Sorted(maps.Keys(brainToolNames(dshCustomTools(req.Tools)))), res.Meta.Bma.ToolCatalog); violation != "" {
			return nil, &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: violation}
		}
		c.proc, c.mode = proc, "default"
		a.mu.Lock()
		a.convs[req.SessionID] = c
		a.mu.Unlock()
	}
	c.tools = req.Tools
	if c.mode != req.Mode {
		if err := proc.call(ctx, "session/set_mode", map[string]any{"sessionId": req.SessionID, "modeId": req.Mode}, nil); err != nil {
			return nil, acpOpenError(proc, "agent set_mode", err)
		}
		c.mode = req.Mode
	}
	return c, nil
}

// placeholderOrNew keeps a conversation's placeholder key across reopens.
func (c *dshConv) placeholderOrNew() string {
	if c != nil {
		return c.placeholder
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	return "bma-local-" + hex.EncodeToString(key)
}

func (a *dshAdapter) modelMeta(c *dshConv, model string, metadata map[string]string) map[string]any {
	meta := map[string]any{"baseUrl": a.proxy.url + "/v1", "token": c.placeholder, "model": model}
	for key, name := range map[string]string{"model_context_window": "contextWindow", "model_max_output_tokens": "maxOutputTokens"} {
		if n, err := strconv.Atoi(metadata[key]); err == nil && n > 0 {
			meta[name] = n
		}
	}
	return meta
}

func (c *dshConv) Ref() string { return c.id }

func (c *dshConv) Ready() bool { return c.proc != nil && c.proc.alive() }

func (c *dshConv) Prompt(ctx context.Context, in BrainInput, sink BrainSink) error {
	if in.Model == nil || (in.Model.baseURL == "" && in.Model.platform == nil) {
		return errors.New("the platform handed no model credential for this turn (turn.start metadata model_base_url, model_token)")
	}
	a := c.a
	a.mu.Lock()
	c.prompt = &dshPrompt{ctx: ctx, sink: sink, route: *in.Model, tools: brainToolNames(c.tools)}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		c.prompt = nil
		a.mu.Unlock()
	}()
	meta := map[string]any{"inputId": in.InputID}
	if len(in.ToolPolicy) > 0 {
		meta["toolPolicy"] = in.ToolPolicy
	}
	if in.Effort != "" {
		meta["reasoningEffort"] = in.Effort
	}
	if host := dshHostTools(c.tools); len(host) > 0 {
		meta["hostTools"] = host
	}
	if len(in.Skills) > 0 {
		meta["inputSkills"] = in.Skills
	}
	params := map[string]any{"sessionId": c.id, "prompt": acpPromptBlocks(in), "_meta": map[string]any{acpMetaKey: meta}}
	return promptACP(ctx, c.proc, params, a.env.CancelGrace, a.env.KillGrace)
}

func (c *dshConv) Suspend(ctx context.Context) error {
	a := c.a
	if c.proc != nil && c.proc.alive() {
		err := c.proc.call(ctx, "session/close", map[string]any{"sessionId": c.id}, nil)
		var acpErr *acpError
		if err != nil && !(errors.As(err, &acpErr) && acpErr.Code == CodeNotFound) && c.proc.alive() {
			return fmt.Errorf("agent session/close: %w", err)
		}
	}
	a.forget(c)
	return nil
}

func (c *dshConv) Close(ctx context.Context) error {
	a := c.a
	a.forget(c)
	if c.proc != nil && c.proc.alive() {
		return c.proc.call(ctx, "session/close", map[string]any{"sessionId": c.id}, nil)
	}
	return nil
}

func (a *dshAdapter) forget(c *dshConv) {
	a.mu.Lock()
	if a.convs[c.id] == c {
		delete(a.convs, c.id)
	}
	a.mu.Unlock()
}

// running is the conversation proc knows by wire and its running prompt.
func (a *dshAdapter) running(proc *acpProcess, wire string) (*dshConv, *dshPrompt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.convs[wire]
	if c == nil || c.proc != proc {
		return nil, nil
	}
	return c, c.prompt
}

// routeFor is the running prompt of the conversation holding placeholder
// (agent_acp_proxy.go), with its session id; nil when none runs.
func (a *dshAdapter) routeFor(placeholder string) (*dshPrompt, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.convs {
		if c.placeholder == placeholder {
			return c.prompt, c.id
		}
	}
	return nil, ""
}

// --- agent -> executor ---

func (a *dshAdapter) onNotification(proc *acpProcess, method string, raw json.RawMessage) {
	switch method {
	case "session/update":
		var n struct {
			SessionID string    `json:"sessionId"`
			Update    acpUpdate `json:"update"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return
		}
		_, p := a.running(proc, n.SessionID)
		if p == nil {
			return
		}
		u := n.Update
		switch u.Kind {
		case "agent_message_chunk", "agent_thought_chunk":
			if text, ok := u.chunkText(); ok {
				p.sink.Delta(acpChunkKind(u.Kind), u.MessageID, text)
			}
		case "tool_call":
			name := u.Meta.Bma.Name
			if name == "" {
				name = u.Title
			}
			permission := u.Meta.Bma.Permission
			if permission == "" {
				permission = "ask"
			}
			p.sink.ToolUse(BrainCall{ID: u.ToolCallID, Name: name, Input: u.RawInput}, permission)
		case "tool_call_update":
			if content, isError, done := u.toolResult(); done {
				p.sink.ToolResult(u.ToolCallID, content, isError)
			}
		}
	case "_bma/message":
		var m struct {
			SessionID string `json:"sessionId"`
			MessageID string `json:"messageId"`
			Kind      string `json:"kind"`
			Text      string `json:"text"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		if _, p := a.running(proc, m.SessionID); p != nil {
			kind := BrainKindMessage
			if m.Kind == "thinking" {
				kind = BrainKindThinking
			}
			p.sink.Message(kind, m.MessageID, m.Text)
		}
	}
}

func (a *dshAdapter) onRequest(ctx context.Context, proc *acpProcess, method string, raw json.RawMessage) (any, *RPCError) {
	var base struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &base)
	_, p := a.running(proc, base.SessionID)
	if p != nil {
		// The prompt's end (cancel, deadline) also ends what the agent asked for.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(p.ctx, cancel)()
	}
	switch method {
	case "session/request_permission":
		var r acpPermissionRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		if p == nil {
			return acpPermissionOutcome(nil, BrainDecision{Outcome: BrainCanceled}), nil
		}
		d := p.sink.Permission(ctx, BrainCall{ID: r.ToolCall.ToolCallID, Name: r.ToolCall.Title, Input: r.ToolCall.RawInput})
		return acpPermissionOutcome(r.Options, d), nil
	case "_bma/tools/call":
		var r struct {
			ToolCallID string         `json:"toolCallId"`
			Name       string         `json:"name"`
			Input      map[string]any `json:"input"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		result := refused("stale_turn", "no turn of this session is running in this agent process")
		if p != nil {
			result = p.sink.Call(ctx, BrainCall{ID: r.ToolCallID, Name: r.Name, Input: r.Input})
		}
		response := map[string]any{"text": result.Text, "isError": result.IsError, "outcome": result.Outcome, "errorCode": result.Code}
		if images := acpImages(result.Images); len(images) > 0 {
			response["images"] = images
		}
		return response, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
}

// acpPlatformTools are the six platform tools (CMA agent_toolset names).
var acpPlatformTools = []string{ToolNameBash, ToolNameEdit, ToolNameGlob, ToolNameGrep, ToolNameRead, ToolNameWrite}

func acpCatalogViolation(catalog []string) string {
	return acpToolsViolation(slices.Sorted(slices.Values(acpPlatformTools)), catalog)
}
