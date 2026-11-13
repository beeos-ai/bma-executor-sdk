package bmaexecutor

// The ACP client of the ACP adapter (agent process, JSON-RPC over stdio,
// ACP v1): start and initialize the agent process, calls, notifications, the
// agent's own requests, kill. What a method means is the adapter's business.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// acpHandler answers what the agent process sends.
type acpHandler interface {
	onRequest(ctx context.Context, proc *acpProcess, method string, raw json.RawMessage) (any, *RPCError)
	onNotification(proc *acpProcess, method string, raw json.RawMessage)
}

type acpProcess struct {
	handler acpHandler
	log     func(LogLevel, string, error)
	cmd     *exec.Cmd
	tree    *processTree
	stdin   io.WriteCloser
	wmu     sync.Mutex
	stderr  *tailBuffer
	init    acpInitializeResponse

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
		LoadSession        bool `json:"loadSession"`
		PromptCapabilities struct {
			Image bool `json:"image"`
		} `json:"promptCapabilities"`
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
			// ReasoningEfforts are the levels prompt _meta.bma.reasoningEffort takes (bma-dsh-acp >= 0.2.2).
			ReasoningEfforts []string `json:"reasoningEfforts"`
		} `json:"bma"`
	} `json:"_meta"`
}

type acpSessionResponse struct {
	SessionID string `json:"sessionId"`
	Modes     struct {
		CurrentModeID string `json:"currentModeId"`
	} `json:"modes"`
	Meta struct {
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

// acpUpdate is a session/update notification's update.
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

// chunkText is the text of an agent_message_chunk / agent_thought_chunk ("", false for another content type).
func (u acpUpdate) chunkText() (string, bool) {
	var c struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
		return "", false
	}
	return c.Text, true
}

// chunkKind is the brain text kind of an ACP chunk update.
func acpChunkKind(update string) string {
	if update == "agent_thought_chunk" {
		return BrainKindThinking
	}
	return BrainKindMessage
}

// toolResult is the text content of a finished tool_call_update (done false while it runs).
func (u acpUpdate) toolResult() (content []ContentBlock, isError, done bool) {
	if u.Status != "completed" && u.Status != "failed" {
		return nil, false, false
	}
	var blocks []struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	_ = json.Unmarshal(u.Content, &blocks)
	for _, b := range blocks {
		if b.Content.Type == "text" {
			content = append(content, ContentBlock{Type: ContentBlockTypeText, Text: b.Content.Text})
		}
	}
	return content, u.Status == "failed", true
}

// acpLaunch is how an ACP adapter starts its agent process.
type acpLaunch struct {
	env      BrainEnv
	extraEnv []string
}

// startACPProcess starts the agent and answers its initialize.
func startACPProcess(ctx context.Context, l acpLaunch, h acpHandler) (*acpProcess, error) {
	cmd := exec.Command(l.env.Command[0], l.env.Command[1:]...)
	env := append([]string(nil), l.extraEnv...)
	runAs := l.env.RunAs
	for _, key := range []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"} {
		value, ok := os.LookupEnv(key)
		switch {
		case runAs != nil && key == "HOME":
			value, ok = runAs.Home, runAs.Home != ""
		case runAs != nil && key == "USER":
			value, ok = runAs.User, runAs.User != ""
		}
		if ok {
			env = append(env, key+"="+value)
		}
	}
	cmd.Env = append(env, l.env.Env...)
	cmd.Dir = l.env.StateDir
	setProcessGroupID(cmd)
	// The brain runs as the workload, like the tools it calls (CoreOptions.RunAs).
	setCredential(cmd, runAs)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &acpProcess{handler: h, log: l.env.Log, cmd: cmd, stdin: stdin, stderr: &tailBuffer{max: 16 << 10},
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
		p.logf(LogInfo, "acp agent exited", fmt.Errorf("%v; stderr tail: %s", err, p.stderr.String()))
	}()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	initialize := map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "bma-executor-sdk"}}
	if err := p.call(initCtx, "initialize", initialize, &p.init); err != nil {
		p.kill(l.env.KillGrace)
		return nil, fmt.Errorf("agent initialize: %w (stderr: %s)", err, p.stderr.String())
	}
	if p.init.ProtocolVersion != 1 {
		p.kill(l.env.KillGrace)
		return nil, fmt.Errorf("agent speaks ACP protocol %d, not 1", p.init.ProtocolVersion)
	}
	return p, nil
}

func (p *acpProcess) logf(level LogLevel, message string, err error) {
	if p.log != nil {
		p.log(level, message, err)
	}
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
				p.logf(LogWarn, "acp agent wrote a non-JSON line on stdout", nil)
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
			result, rpcErr := p.handler.onRequest(ctx, p, m.Method, m.Params)
			response := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if rpcErr != nil {
				response["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
			} else {
				response["result"] = result
			}
			_ = p.write(response)
		}()
	case m.Method == "$/cancel_request":
		var c struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &c) == nil {
			p.cancelInflight(string(c.RequestID))
		}
	case m.Method != "":
		p.handler.onNotification(p, m.Method, m.Params)
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

// promptACP runs session/prompt; when ctx ends it sends session/cancel and
// kills the process if the agent does not end the prompt within grace.
func promptACP(ctx context.Context, proc *acpProcess, params map[string]any, grace, killGrace time.Duration) error {
	type outcome struct {
		res acpPromptResponse
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var res acpPromptResponse
		err := proc.call(context.Background(), "session/prompt", params, &res)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return acpStop(ctx, o.res, o.err)
	case <-ctx.Done():
	}
	if err := proc.notify("session/cancel", map[string]any{"sessionId": params["sessionId"]}); err != nil {
		proc.logf(LogDebug, "acp session/cancel", err)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		proc.logf(LogWarn, "acp agent did not end a canceled turn within "+grace.String()+"; killing it", nil)
		proc.kill(killGrace)
	}
	return ctx.Err()
}

// acpStop maps the end of session/prompt to the prompt's result (the SDK
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
			return &AgentError{Code: data.Bma.StopCode, Message: data.Bma.StopCode + ": " + data.Bma.Detail}
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
		return &AgentError{Code: AgentErrorCodeOutputLimitReached, Message: AgentErrorCodeOutputLimitReached + ": " + detail}
	default:
		return fmt.Errorf("agent stopped: %s", res.StopReason)
	}
}

// acpPermissionOutcome answers session/request_permission with the option of
// kind for decision.
func acpPermissionOutcome(options []acpPermissionOption, d BrainDecision) any {
	kind := ""
	switch d.Outcome {
	case BrainAllow:
		kind = "allow_once"
	case BrainDeny:
		kind = "reject_once"
	}
	for _, o := range options {
		if kind != "" && o.Kind == kind {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// acpPermissionRequest is session/request_permission's params.
type acpPermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string         `json:"toolCallId"`
		Title      string         `json:"title"`
		RawInput   map[string]any `json:"rawInput"`
	} `json:"toolCall"`
	Options []acpPermissionOption `json:"options"`
}

// acpOpenError types a failed call to the agent while its session is opened.
func acpOpenError(proc *acpProcess, what string, err error) error {
	message := what + ": " + err.Error()
	var acpErr *acpError
	switch {
	case !proc.alive():
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: message}
	case errors.As(err, &acpErr) && acpErr.Code == acpAuthRequired:
		return &AgentError{Code: AgentErrorCodeAgentAuthRequired, Message: message}
	case errors.As(err, &acpErr) && (acpErr.Code == acpInvalidParams || acpErr.Code == acpMethodNotFound):
		return &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: message}
	}
	return &AgentError{Code: AgentErrorCodeAgentError, Message: fmt.Sprint(message)}
}

// ACP (JSON-RPC) error codes the open maps.
const (
	acpAuthRequired   = -32000
	acpInvalidParams  = -32602
	acpMethodNotFound = -32601
)

// acpImageBlocks is the ACP prompt content of one input: its text, then its images.
func acpPromptBlocks(in BrainInput) []any {
	if len(in.Images) == 0 {
		return []any{map[string]any{"type": "text", "text": in.Text}}
	}
	var content []any
	if in.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": in.Text})
	}
	for _, img := range in.Images {
		content = append(content, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(img.Data), "mimeType": img.MimeType})
	}
	return content
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

func (e *acpError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("acp error %d: %s: %.500s", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("acp error %d: %s", e.Code, e.Message)
}

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
