package bmaexecutor

// ACPAgent is an AgentHandler that hosts a local brain spoken to over the
// Agent Client Protocol (https://agentclientprotocol.com, v1, JSON-RPC over
// stdio): bma-dsh-acp, the shared DSH adapter of BMA as an ACP agent
// (ADR-0027). The executor stays the machine's only connection to BMA;
// ACP is only the process boundary.
//
//	platform (executor protocol)          ACPAgent                         agent (ACP)
//	turn.start                            open/load the session            session/new | session/load (_meta.bma)
//	                                      permission_mode                  session/set_mode
//	(turn runs)                           prompt                           session/prompt (_meta.bma.inputId, toolPolicy)
//	session.event_delta                   <-                               agent_message_chunk / agent_thought_chunk (messageId)
//	agent.message / agent.thinking        <-                               _bma/message (durable text of a messageId)
//	agent.tool_use                        <-                               tool_call (_meta.bma.name, permission)
//	interaction.request (appr_ id)        <->                              session/request_permission
//	(tool runs here, in Core)             <->                              _bma/tools/call
//	agent.tool_result                     <-                               tool_call_update completed | failed
//	turn.cancel                           ->                               session/cancel; kill after CancelGrace
//	idle failed + session.error           <-                               max_tokens -> output_limit_reached, data.bma.stopCode
//
// One agent process serves every session (bma-dsh-acp holds many sessions;
// about 2 MiB each on top of about 90 MiB, see its README). When it does not
// answer a cancel within CancelGrace its process group is killed, which ends
// the other running turns too (they fail); the next turn starts a new process
// and loads its conversation (session/load with the executor_session_ref).
//
// Tools run in the executor's Core, in the turn's working directory, so the
// hands of a local brain are the same as for a platform brain. Model requests
// go to a loopback proxy with a per-session placeholder key; the proxy
// forwards only during a running turn, refuses a request whose tools are not
// the six platform tools, and adds the real credential: the platform's
// per-turn model endpoint when turn.start carries one (metadata
// model_base_url, model_token), otherwise the node owner's Upstream.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultACPCancelGrace is how long a canceled turn waits for the agent's
	// cancelled stop before the agent process is killed.
	DefaultACPCancelGrace = 10 * time.Second
	// DefaultACPKillGrace is the wait between SIGTERM and SIGKILL of the agent process group.
	DefaultACPKillGrace = 3 * time.Second
	// DefaultACPMaxSessions bounds the agent sessions of one ACPAgent.
	DefaultACPMaxSessions = 8

	acpBrain   = "acp"
	acpMetaKey = "bma"
)

// DefaultACPSystemPrompt is the base system prompt of a local brain; the
// agent definition's system instructions follow it.
const DefaultACPSystemPrompt = "You are an agent running on the user's computer through BMA (BeeOS Managed Agents).\n" +
	"Your tools run on this computer, in the working directory %s; use paths under it.\n" +
	"Be concise. When a task is done, say what you did."

// acpPermissionModes are the session modes of bma-dsh-acp.
var acpPermissionModes = []string{"default", "accept_edits"}

// ACPModelUpstream is where model requests go when turn.start carries no
// platform model endpoint: the node owner's own OpenAI-compatible endpoint.
type ACPModelUpstream struct {
	// BaseURL ends in /v1; requests go to BaseURL + "/chat/completions".
	BaseURL string
	// Model replaces the model of each request when set.
	Model string
	// Header adds the credential (for example Authorization) to a forwarded request.
	Header func(*http.Request)
}

// ACPAgentOptions configures NewACPAgent.
type ACPAgentOptions struct {
	// Command is the agent's argv, for example {"/opt/bma/bma-dsh-acp"} or
	// {"/opt/bma/node", "/opt/bma/bma-dsh-acp.mjs"}. Command[0] must be an
	// absolute path: the agent is never looked up on PATH (a user's own dsh
	// there must not be started by mistake).
	Command []string
	// Env adds KEY=VALUE entries to the agent's environment (PATH, HOME, USER, LANG and TMPDIR come from the executor).
	Env []string
	// StateDir holds the agent's state (BMA_DSH_ACP_HOME = StateDir).
	StateDir string
	// Core runs the tools.
	Core *Core
	// AllowedRoots bound a turn's cwd (default: Core's work root). A turn
	// without cwd works in Core's per-session workspace.
	AllowedRoots []string
	// Upstream is the model endpoint used when the platform hands none.
	Upstream ACPModelUpstream
	// HarnessID is the declared harness id (default "dsh").
	HarnessID string
	// Model is the model name the agent uses when the turn config has none (default Upstream.Model).
	Model string
	// SystemPrompt is the base system prompt, with one %s for the working directory (default DefaultACPSystemPrompt).
	SystemPrompt string
	MaxSessions  int64
	CancelGrace  time.Duration
	KillGrace    time.Duration
	OnLog        func(LogEvent)
}

// ACPAgent hosts an ACP agent process; see the comment at the top of agent_acp.go.
type ACPAgent struct {
	opts    ACPAgentOptions
	proxy   *acpModelProxy
	harness AgentHarnessCapability

	mu       sync.Mutex
	proc     *acpProcess
	sessions map[string]*acpSession
	closed   bool
}

type acpSession struct {
	id          string
	cwd         string
	placeholder string
	proc        *acpProcess // the process the session is open in
	mode        string
	turn        *acpTurn
}

type acpRoute struct {
	baseURL string
	model   string
	header  func(*http.Request)
}

type acpTurn struct {
	turn  *AgentTurn
	ctx   context.Context
	route acpRoute

	mu       sync.Mutex
	messages map[string]string // messageId -> event id
	index    map[string]int64  // event id -> next delta index
	tools    map[string]string // toolCallId -> agent.tool_use event id
}

// NewACPAgent starts the agent process, checks it (ACP initialize, the six
// platform tools) and starts the model proxy. Serve it with
// client.ServeAgent(agent, agent.Capability()).
func NewACPAgent(ctx context.Context, opts ACPAgentOptions) (*ACPAgent, error) {
	if len(opts.Command) == 0 || opts.Core == nil || opts.StateDir == "" {
		return nil, errors.New("bmaexecutor: ACPAgentOptions.Command, Core and StateDir are required")
	}
	if !filepath.IsAbs(opts.Command[0]) {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Command[0] must be an absolute path, got %q (no PATH lookup)", opts.Command[0])
	}
	if opts.Upstream.BaseURL == "" {
		return nil, errors.New("bmaexecutor: ACPAgentOptions.Upstream.BaseURL is required")
	}
	if opts.HarnessID == "" {
		opts.HarnessID = "dsh"
	}
	if opts.Model == "" {
		opts.Model = opts.Upstream.Model
	}
	if opts.Model == "" {
		opts.Model = "default"
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = DefaultACPSystemPrompt
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultACPMaxSessions
	}
	if opts.CancelGrace <= 0 {
		opts.CancelGrace = DefaultACPCancelGrace
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = DefaultACPKillGrace
	}
	if len(opts.AllowedRoots) == 0 {
		opts.AllowedRoots = []string{opts.Core.opts.WorkRoot}
	}
	for i, root := range opts.AllowedRoots {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("bmaexecutor: allowed root %q: %w", root, err)
		}
		opts.AllowedRoots[i] = real
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, err
	}
	a := &ACPAgent{opts: opts, sessions: map[string]*acpSession{}}
	proxy, err := startACPModelProxy(a)
	if err != nil {
		return nil, err
	}
	a.proxy = proxy
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		proxy.close()
		return nil, err
	}
	init := proc.init
	a.harness = AgentHarnessCapability{
		ID: opts.HarnessID, Version: init.AgentInfo.Version + "+dsh" + init.Meta.Bma.DSH, Protocol: "acp/1",
		AgentName: init.AgentInfo.Name, AgentVersion: init.AgentInfo.Version,
		Streaming: init.Meta.Bma.Streaming, PermissionRelay: init.Meta.Bma.PermissionRelay,
		SessionLoad: init.AgentCapabilities.LoadSession, ImageInputs: false,
	}
	return a, nil
}

// Capability is the agent capability to serve: one harness, in harnesses[]
// and in the deprecated harness member.
func (a *ACPAgent) Capability() AgentCapability {
	h := a.harness
	return AgentCapability{
		Enabled: true, PermissionModes: acpPermissionModes, InteractionKinds: []string{InteractionKindPermission},
		MaxSessions: a.opts.MaxSessions, Harness: &h, Harnesses: []AgentHarnessCapability{a.harness},
	}
}

// Close stops the agent process and the model proxy.
func (a *ACPAgent) Close() error {
	a.mu.Lock()
	a.closed = true
	proc := a.proc
	a.mu.Unlock()
	if proc != nil {
		proc.kill(a.opts.KillGrace)
	}
	a.proxy.close()
	return nil
}

func (a *ACPAgent) log(level LogLevel, message string, err error) {
	if a.opts.OnLog != nil {
		a.opts.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// --- AgentHandler ---

func (a *ACPAgent) OpenTurn(ctx context.Context, params TurnStartParams) (TurnStartResult, error) {
	config := AgentTurnConfig{}
	if params.Config != nil {
		config = *params.Config
	}
	if config.Harness != "" && config.Harness != a.harness.ID {
		return TurnStartResult{}, &RPCError{Code: CodeCapabilityUnavailable, Message: "capability_unavailable: harness " + config.Harness + " is not hosted here",
			Data: &JSONRPCErrorData{Capability: "harness:" + config.Harness}}
	}
	mode := config.PermissionMode
	if mode == "" {
		mode = "default"
	}
	if !slices.Contains(acpPermissionModes, mode) {
		return TurnStartResult{}, &RPCError{Code: CodeInvalidParams, Message: "unknown permission_mode " + mode}
	}
	cwd, err := a.turnCwd(params.SessionID, config.Cwd)
	if err != nil {
		return TurnStartResult{}, err
	}
	model := config.Model
	if model == "" {
		model = a.opts.Model
	}
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return TurnStartResult{}, &RPCError{Code: CodeResourceUnavailable, Message: "agent unavailable: " + err.Error(), Data: &JSONRPCErrorData{Retryable: true}}
	}
	s := a.session(params.SessionID)
	resumed := params.ExecutorSessionRef != "" || s.proc == proc
	if s.proc != proc {
		meta := map[string]any{"sessionId": params.SessionID, "systemPrompt": a.systemPrompt(cwd, config.System), "model": a.modelMeta(s, model, params.Metadata)}
		var res acpSessionResponse
		if params.ExecutorSessionRef != "" {
			err = proc.call(ctx, "session/load", map[string]any{"sessionId": params.ExecutorSessionRef, "cwd": cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
			var acpErr *acpError
			if errors.As(err, &acpErr) && acpErr.Code == CodeNotFound {
				return TurnStartResult{}, &RPCError{Code: CodeNotFound, Message: "the local conversation " + params.ExecutorSessionRef + " was not found; it is not recreated"}
			}
		} else {
			err = proc.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
		}
		if err != nil {
			return TurnStartResult{}, &RPCError{Code: CodeInternalError, Message: "agent session: " + err.Error()}
		}
		if violation := acpCatalogViolation(res.Meta.Bma.ToolCatalog); violation != "" {
			return TurnStartResult{}, &RPCError{Code: CodeInternalError, Message: violation}
		}
		a.mu.Lock()
		s.proc, s.mode = proc, "default"
		a.mu.Unlock()
	}
	if s.mode != mode {
		if err := proc.call(ctx, "session/set_mode", map[string]any{"sessionId": params.SessionID, "modeId": mode}, nil); err != nil {
			return TurnStartResult{}, &RPCError{Code: CodeInternalError, Message: "agent set_mode: " + err.Error()}
		}
		s.mode = mode
	}
	if err := a.opts.Core.openLocal(acpBrain, params.SessionID, cwd); err != nil {
		return TurnStartResult{}, err
	}
	a.mu.Lock()
	s.cwd = cwd
	a.mu.Unlock()
	return TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: params.SessionID, Resumed: resumed,
		Effective: AgentTurnConfig{Model: model, PermissionMode: mode, Cwd: cwd, Harness: a.harness.ID},
	}, nil
}

func (a *ACPAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
	params := turn.Params()
	a.mu.Lock()
	s := a.sessions[params.SessionID]
	a.mu.Unlock()
	if s == nil || s.proc == nil {
		return errors.New("the agent session is not open")
	}
	t := &acpTurn{turn: turn, ctx: ctx, route: a.route(params.Metadata), messages: map[string]string{}, index: map[string]int64{}, tools: map[string]string{}}
	a.mu.Lock()
	s.turn = t
	proc := s.proc
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		a.mu.Unlock()
	}()
	var policy map[string]string
	if raw := params.Metadata["tool_policy"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return fmt.Errorf("turn.start metadata.tool_policy must be a JSON object of tool: allow|ask|deny")
		}
	}
	if err := a.prompt(ctx, proc, s.id, params.InputID, inputText(params.Input), policy); err != nil {
		return err
	}
	// turn.input received while the turn ran: prompted in order, in the same turn.
	for {
		select {
		case input := <-turn.Inputs():
			if err := a.prompt(ctx, proc, s.id, input.InputID, inputText(input.Input), policy); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (a *ACPAgent) CloseSession(ctx context.Context, sessionID, _ string) error {
	a.mu.Lock()
	s := a.sessions[sessionID]
	delete(a.sessions, sessionID)
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	if s.proc != nil && s.proc.alive() {
		if err := s.proc.call(ctx, "session/close", map[string]any{"sessionId": sessionID}, nil); err != nil {
			a.log(LogDebug, "acp session/close "+sessionID, err)
		}
	}
	a.opts.Core.closeLocal(acpBrain, sessionID)
	return nil
}

// --- turn helpers ---

func (a *ACPAgent) prompt(ctx context.Context, proc *acpProcess, sessionID, inputID, text string, policy map[string]string) error {
	meta := map[string]any{"inputId": inputID}
	if len(policy) > 0 {
		meta["toolPolicy"] = policy
	}
	type outcome struct {
		res acpPromptResponse
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var res acpPromptResponse
		err := proc.call(context.Background(), "session/prompt", map[string]any{
			"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": text}}, "_meta": map[string]any{acpMetaKey: meta},
		}, &res)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return acpStop(ctx, o.res, o.err)
	case <-ctx.Done():
	}
	if err := proc.notify("session/cancel", map[string]any{"sessionId": sessionID}); err != nil {
		a.log(LogDebug, "acp session/cancel", err)
	}
	timer := time.NewTimer(a.opts.CancelGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		a.log(LogWarn, "acp agent did not end a canceled turn within "+a.opts.CancelGrace.String()+"; killing it", nil)
		a.killProcess(proc)
	}
	return ctx.Err()
}

// acpStop maps the end of session/prompt to RunTurn's result (the SDK
// writes session.error "<code>: <detail>" for an error).
func acpStop(ctx context.Context, res acpPromptResponse, err error) error {
	var rpc *acpError
	switch {
	case errors.As(err, &rpc):
		var data struct {
			Bma *struct {
				StopCode string `json:"stopCode"`
				Detail   string `json:"detail"`
			} `json:"bma"`
		}
		if len(rpc.Data) > 0 && json.Unmarshal(rpc.Data, &data) == nil && data.Bma != nil && data.Bma.StopCode != "" {
			return fmt.Errorf("%s: %s", data.Bma.StopCode, data.Bma.Detail)
		}
		return errors.New(rpc.Message)
	case err != nil:
		return err
	}
	switch res.StopReason {
	case "end_turn":
		return nil
	case "cancelled":
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the agent cancelled the turn")
	case "max_tokens":
		detail := "the model reply was cut at its output limit"
		if res.Meta.Bma.Detail != "" {
			detail = res.Meta.Bma.Detail
		}
		return fmt.Errorf("output_limit_reached: %s", detail)
	default:
		return fmt.Errorf("agent stopped: %s", res.StopReason)
	}
}

func inputText(blocks []AgentInputBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == AgentInputBlockTypeText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (a *ACPAgent) systemPrompt(cwd, system string) string {
	prompt := fmt.Sprintf(a.opts.SystemPrompt, cwd)
	if system != "" {
		prompt += "\n\n" + system
	}
	return prompt
}

func (a *ACPAgent) modelMeta(s *acpSession, model string, metadata map[string]string) map[string]any {
	meta := map[string]any{"baseUrl": a.proxy.url + "/v1", "token": s.placeholder, "model": model}
	for key, name := range map[string]string{"model_context_window": "contextWindow", "model_max_output_tokens": "maxOutputTokens"} {
		if n, err := strconv.Atoi(metadata[key]); err == nil && n > 0 {
			meta[name] = n
		}
	}
	return meta
}

// route is the platform's per-turn model endpoint when turn.start hands one, otherwise Upstream.
func (a *ACPAgent) route(metadata map[string]string) acpRoute {
	if base, token := metadata["model_base_url"], metadata["model_token"]; base != "" && token != "" {
		return acpRoute{baseURL: strings.TrimRight(base, "/"), header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }}
	}
	u := a.opts.Upstream
	return acpRoute{baseURL: strings.TrimRight(u.BaseURL, "/"), model: u.Model, header: u.Header}
}

func (a *ACPAgent) turnCwd(sessionID, requested string) (string, error) {
	if requested == "" {
		dir := a.opts.Core.workspace(acpBrain, sessionID)
		if err := a.opts.Core.createWorkspace(dir); err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(dir)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(requested))
	if err == nil {
		for _, root := range a.opts.AllowedRoots {
			if rel, relErr := filepath.Rel(root, real); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return real, nil
			}
		}
	}
	return "", &RPCError{Code: CodeForbidden, Message: "forbidden: cwd is outside the executor's allowed roots"}
}

func (a *ACPAgent) session(id string) *acpSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		return s
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	s := &acpSession{id: id, placeholder: "bma-local-" + hex.EncodeToString(key)}
	a.sessions[id] = s
	return s
}

// turnFor is the running turn of a session in proc.
func (a *ACPAgent) turnFor(proc *acpProcess, sessionID string) *acpTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[sessionID]
	if s == nil || s.proc != proc {
		return nil
	}
	return s.turn
}

// approvalID is the platform interaction id of a tool call: deterministic
// across reconnects and restarts (ADR-0027: appr_ + 24 hex of sha256(session NUL turn NUL call)).
// The turn is in it because providers may reuse call ids such as "call_0" in every turn.
func approvalID(sessionID, turnID, callID string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + turnID + "\x00" + callID))
	return "appr_" + hex.EncodeToString(sum[:])[:24]
}

func acpCatalogViolation(catalog []string) string {
	want := []string{ToolNameBash, ToolNameEdit, ToolNameGlob, ToolNameGrep, ToolNameRead, ToolNameWrite}
	got := append([]string(nil), catalog...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Sprintf("agent exposes tools [%s], not exactly the platform tools: refused (fail-closed)", strings.Join(got, ","))
	}
	return ""
}

// --- agent -> executor ---

func (a *ACPAgent) onNotification(proc *acpProcess, method string, raw json.RawMessage) {
	switch method {
	case "session/update":
		var n struct {
			SessionID string    `json:"sessionId"`
			Update    acpUpdate `json:"update"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return
		}
		if t := a.turnFor(proc, n.SessionID); t != nil {
			t.update(n.Update)
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
		if t := a.turnFor(proc, m.SessionID); t != nil {
			t.commit(m.MessageID, m.Kind, m.Text)
		}
	case "$/cancel_request":
		var c struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(raw, &c) == nil {
			proc.cancelInflight(string(c.RequestID))
		}
	}
}

func (a *ACPAgent) onRequest(ctx context.Context, proc *acpProcess, method string, raw json.RawMessage) (any, *RPCError) {
	var base struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(raw, &base)
	t := a.turnFor(proc, base.SessionID)
	if t != nil {
		// The turn's end (cancel, deadline) also ends what the agent asked for.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(t.ctx, cancel)
		defer stop()
	}
	switch method {
	case "session/request_permission":
		var p struct {
			ToolCall struct {
				ToolCallID string         `json:"toolCallId"`
				Title      string         `json:"title"`
				RawInput   map[string]any `json:"rawInput"`
			} `json:"toolCall"`
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		cancelled := map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
		if t == nil {
			return cancelled, nil
		}
		choose := func(kind string) any {
			for _, o := range p.Options {
				if o.Kind == kind {
					return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
				}
			}
			return cancelled
		}
		toolUseID := t.toolUse(p.ToolCall.ToolCallID, p.ToolCall.Title, p.ToolCall.RawInput, "ask")
		answer, err := t.turn.RequestInteraction(ctx, InteractionRequestParams{
			Kind: InteractionKindPermission, InteractionID: approvalID(base.SessionID, t.turn.Params().TurnID, p.ToolCall.ToolCallID),
			ToolUseID: toolUseID, ToolName: p.ToolCall.Title, ToolInput: p.ToolCall.RawInput, Scopes: []string{InteractionScopeOnce},
		})
		switch {
		case err != nil:
			return cancelled, nil
		case answer.Outcome == InteractionOutcomeAllow:
			return choose("allow_once"), nil
		case answer.Outcome == InteractionOutcomeDeny:
			return choose("reject_once"), nil
		default: // expired, canceled
			return cancelled, nil
		}
	case "_bma/tools/call":
		var p struct {
			ToolCallID string         `json:"toolCallId"`
			Name       string         `json:"name"`
			Input      map[string]any `json:"input"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		if t == nil {
			return map[string]any{"text": "no turn is running in this session", "isError": true}, nil
		}
		text, isError := a.runTool(ctx, base.SessionID, p.ToolCallID, p.Name, p.Input)
		return map[string]any{"text": text, "isError": isError}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
}

type acpUpdate struct {
	Kind       string          `json:"sessionUpdate"`
	MessageID  string          `json:"messageId"`
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	Content    json.RawMessage `json:"content"`
	RawInput   map[string]any  `json:"rawInput"`
	Meta       struct {
		Bma struct {
			Name       string `json:"name"`
			Permission string `json:"permission"`
		} `json:"bma"`
	} `json:"_meta"`
}

func (t *acpTurn) eventID(messageID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.messages[messageID]
	if !ok {
		id = t.turn.NewEventID()
		t.messages[messageID] = id
	}
	return id
}

func (t *acpTurn) update(u acpUpdate) {
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
		t.mu.Unlock()
		t.turn.Delta(id, eventType, index, c.Text)
	case "tool_call":
		name := u.Meta.Bma.Name
		if name == "" {
			name = u.Title
		}
		permission := u.Meta.Bma.Permission
		if permission == "" {
			permission = "ask"
		}
		t.toolUse(u.ToolCallID, name, u.RawInput, permission)
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		t.mu.Lock()
		toolUseID := t.tools[u.ToolCallID]
		t.mu.Unlock()
		if toolUseID == "" {
			return
		}
		var blocks []struct {
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(u.Content, &blocks)
		var content []ContentBlock
		for _, b := range blocks {
			if b.Content.Type == "text" {
				content = append(content, ContentBlock{Type: ContentBlockTypeText, Text: b.Content.Text})
			}
		}
		_, _ = t.turn.Emit(AgentToolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: toolUseID, Content: content, IsError: u.Status == "failed"})
	}
}

// toolUse emits agent.tool_use once per tool call and returns its event id.
func (t *acpTurn) toolUse(callID, name string, input map[string]any, permission string) string {
	t.mu.Lock()
	if id := t.tools[callID]; id != "" {
		t.mu.Unlock()
		return id
	}
	id := t.turn.NewEventID()
	t.tools[callID] = id
	t.mu.Unlock()
	if input == nil {
		input = map[string]any{}
	}
	_, _ = t.turn.EmitWithID(id, AgentToolUseEvent{Type: AgentEventTypeAgentToolUse, Name: name, Input: input, EvaluatedPermission: &EvaluatedPermission{Type: permission}})
	return id
}

func (t *acpTurn) commit(messageID, kind, text string) {
	key := "agent_message_chunk\x00" + messageID
	if kind == "thinking" {
		key = "agent_thought_chunk\x00" + messageID
	}
	id := t.eventID(key)
	t.mu.Lock()
	delete(t.messages, key)
	t.mu.Unlock()
	content := []ContentBlock{{Type: ContentBlockTypeText, Text: text}}
	if kind == "thinking" {
		_, _ = t.turn.EmitWithID(id, AgentThinkingEvent{Type: AgentEventTypeAgentThinking, Content: content})
		return
	}
	_, _ = t.turn.EmitWithID(id, AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: content})
}

// --- process ---

type acpProcess struct {
	agent  *ACPAgent
	cmd    *exec.Cmd
	tree   *processTree
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr *tailBuffer
	init   acpInitializeResponse

	mu       sync.Mutex
	nextID   int64
	waiting  map[int64]chan acpWire
	inflight map[string]context.CancelFunc
	done     chan struct{}
	exitErr  error
}

type acpWire struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

type acpInitializeResponse struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession bool `json:"loadSession"`
	} `json:"agentCapabilities"`
	AgentInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	Meta struct {
		Bma struct {
			DSH             string   `json:"dsh"`
			Tools           []string `json:"tools"`
			Streaming       string   `json:"streaming"`
			PermissionRelay string   `json:"permissionRelay"`
		} `json:"bma"`
	} `json:"_meta"`
}

type acpSessionResponse struct {
	SessionID string `json:"sessionId"`
	Meta      struct {
		Bma struct {
			ToolCatalog []string `json:"toolCatalog"`
		} `json:"bma"`
	} `json:"_meta"`
}

type acpPromptResponse struct {
	StopReason string `json:"stopReason"`
	Meta       struct {
		Bma struct {
			StopCode string `json:"stopCode"`
			Detail   string `json:"detail"`
		} `json:"bma"`
	} `json:"_meta"`
}

// ensureProcess returns the running agent process, starting (and checking) a new one when needed.
func (a *ACPAgent) ensureProcess(ctx context.Context) (*acpProcess, error) {
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
	proc, err := a.startProcess(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proc != nil && a.proc.alive() { // lost a start race
		go proc.kill(a.opts.KillGrace)
		return a.proc, nil
	}
	a.proc = proc
	return proc, nil
}

func (a *ACPAgent) startProcess(ctx context.Context) (*acpProcess, error) {
	cmd := exec.Command(a.opts.Command[0], a.opts.Command[1:]...)
	env := []string{"BMA_DSH_ACP_HOME=" + a.opts.StateDir}
	for _, key := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	cmd.Env = append(env, a.opts.Env...)
	cmd.Dir = a.opts.StateDir
	setProcessGroupID(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &acpProcess{agent: a, cmd: cmd, stdin: stdin, stderr: &tailBuffer{max: 16 << 10},
		waiting: map[int64]chan acpWire{}, inflight: map[string]context.CancelFunc{}, done: make(chan struct{})}
	cmd.Stderr = p.stderr
	tree, err := startCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	p.tree = tree
	go p.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		tree.close() // nothing the agent started outlives it
		p.mu.Lock()
		p.exitErr = err
		waiting := p.waiting
		p.waiting = map[int64]chan acpWire{}
		for _, cancel := range p.inflight {
			cancel()
		}
		p.mu.Unlock()
		close(p.done)
		for _, ch := range waiting {
			ch <- acpWire{}
		}
		a.log(LogInfo, "acp agent exited", fmt.Errorf("%v; stderr tail: %s", err, p.stderr.String()))
	}()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.call(initCtx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "bma-executor-sdk"}}, &p.init); err != nil {
		p.kill(a.opts.KillGrace)
		return nil, fmt.Errorf("agent initialize: %w (stderr: %s)", err, p.stderr.String())
	}
	if p.init.ProtocolVersion != 1 {
		p.kill(a.opts.KillGrace)
		return nil, fmt.Errorf("agent speaks ACP protocol %d, not 1", p.init.ProtocolVersion)
	}
	if violation := acpCatalogViolation(p.init.Meta.Bma.Tools); violation != "" {
		p.kill(a.opts.KillGrace)
		return nil, errors.New(violation)
	}
	return p, nil
}

// killProcess kills proc; its sessions are no longer open anywhere.
func (a *ACPAgent) killProcess(proc *acpProcess) {
	proc.kill(a.opts.KillGrace)
}

func (p *acpProcess) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *acpProcess) kill(grace time.Duration) {
	if !p.alive() {
		return
	}
	_ = p.stdin.Close()
	_ = p.tree.signal(signalTerminate)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = p.tree.signal(signalKill)
		<-p.done
	}
}

func (p *acpProcess) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var m acpWire
			if json.Unmarshal(line, &m) == nil {
				p.dispatch(m)
			} else {
				p.agent.log(LogWarn, "acp agent wrote a non-JSON line on stdout", nil)
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *acpProcess) dispatch(m acpWire) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		ctx, cancel := context.WithCancel(context.Background())
		key := string(m.ID)
		p.mu.Lock()
		p.inflight[key] = cancel
		p.mu.Unlock()
		go func() {
			defer func() {
				p.mu.Lock()
				delete(p.inflight, key)
				p.mu.Unlock()
				cancel()
			}()
			result, rpcErr := p.agent.onRequest(ctx, p, m.Method, m.Params)
			response := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if rpcErr != nil {
				response["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
			} else {
				response["result"] = result
			}
			_ = p.write(response)
		}()
	case m.Method != "":
		p.agent.onNotification(p, m.Method, m.Params)
	default:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return
		}
		p.mu.Lock()
		ch := p.waiting[id]
		delete(p.waiting, id)
		p.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (p *acpProcess) cancelInflight(key string) {
	p.mu.Lock()
	cancel := p.inflight[key]
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *acpProcess) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (p *acpProcess) call(ctx context.Context, method string, params, out any) error {
	ch := make(chan acpWire, 1)
	p.mu.Lock()
	if !p.alive() {
		p.mu.Unlock()
		return p.exited()
	}
	p.nextID++
	id := p.nextID
	p.waiting[id] = ch
	p.mu.Unlock()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return fmt.Errorf("write to agent: %w", err)
	}
	select {
	case m := <-ch:
		switch {
		case m.Error != nil:
			return &acpError{Code: m.Error.Code, Message: m.Error.Message, Data: m.Error.Data}
		case m.ID == nil && m.Result == nil:
			return p.exited()
		case out != nil:
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return ctx.Err()
	}
}

func (p *acpProcess) exited() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Errorf("the agent process exited (%v): %s", p.exitErr, lastLine(p.stderr.String()))
}

func (p *acpProcess) notify(method string, params any) error {
	return p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// acpError is a JSON-RPC error answered by the agent.
type acpError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *acpError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

// tailBuffer keeps the last max bytes written (the agent's stderr, for errors and logs).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
