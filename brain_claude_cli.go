package bmaexecutor

// The Claude adapter (ADR-0066 of BMA, decision 6): drives the official
// `claude` CLI, unmodified, over its stream-json input and output and its
// control protocol (https://code.claude.com/docs/en/cli-reference,
// https://code.claude.com/docs/en/agent-sdk/typescript.md). Nothing sits
// between the executor and the CLI.
//
//	BrainHost / BrainSink        Claude adapter                      claude -p (stream-json)
//	Open                         one CLI process per conversation    --session-id <uuid> | --resume <uuid>
//	Prompt                       user message                        {"type":"user",...}
//	Delta                        <-                                  stream_event text_delta / thinking_delta
//	Message                      <-                                  stream_event content_block_stop (text, thinking)
//	ToolUse                      <-                                  stream_event content_block_stop (tool_use)
//	Permission                   <->                                 control_request can_use_tool / control_response
//	Call                         <->                                 loopback MCP tools/call (mcp__bma__<tool>)
//	ToolResult                   <-                                  user message tool_result
//	ctx canceled                 ->                                  control_request interrupt; kill after CancelGrace
//	Prompt's error               <-                                  result / assistant.error
//
// Credentials. The CLI runs with its node owner's own credentials (their login
// under HOME, or ANTHROPIC_API_KEY or a third-party provider's variables in
// ACPAgentOptions.Env). They never pass through BMA: no model proxy, no
// harness_model, and the SDK never reads or logs them.
//
// Gates, all fail closed: the CLI binary must be a pinned release (version and
// sha256, claudeCLIPins); it starts with no built-in tool (--tools ""), only
// the injected MCP server (--strict-mcp-config), no settings files
// (--setting-sources ""), and the permission mode default; its system init
// message must list exactly the injected tools and the mode default; a
// permission request for any other tool is refused.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	claudeMCPServerName = "bma"
	claudeToolPrefix    = "mcp__" + claudeMCPServerName + "__"
	// claudeToolUseIDMeta is the tools/call _meta key the CLI names the call's tool_use_id with.
	claudeToolUseIDMeta = "claudecode/toolUseId"
	// cliMaxToolRequestBytes bounds one MCP request of the CLI.
	cliMaxToolRequestBytes = 32 << 20
)

// claudeCLIPin is one release of the official claude CLI this SDK is tested
// with: its version (claude --version) and the sha256 of its binary per
// GOOS/GOARCH. Adding a release is a reviewed change: its contract tests
// (brain_claude_cli_test.go) must pass against it.
type claudeCLIPin struct {
	Version string
	SHA256  map[string]string
}

// claudeCLIPins: the digests are the checksums of Anthropic's release
// manifest (https://downloads.claude.ai/claude-code-releases/<version>/manifest.json,
// the one the official installer verifies). 2.1.318 is also the binary of the
// Agent SDK 0.3.318 platform packages the tests install.
var claudeCLIPins = []claudeCLIPin{
	{Version: "2.1.318", SHA256: map[string]string{
		"darwin/arm64": "677a2ea7bf576cedd20e191d973914ed8a41796f53a63b1923381ddc4d5b5e7d",
		"darwin/amd64": "ee9c8577a528aa8efa478bf552b17efa40bb58aa4981d2913c13d383a4d409a4",
		"linux/arm64":  "53a0d7e219d7b0804ecba5dbc002745e89f786cc946d2de85a6470d5b2d1fb06",
		"linux/amd64":  "708c77b204db30086ff6f1ed34577bda03e1be74ced80557cbf7b54249ebf0bd",
	}},
	{Version: "2.1.322", SHA256: map[string]string{
		"darwin/arm64": "a9d237949db816a0b7afd88a9a8e1df36b3a597a30f5a41d1c0539eb180a3450",
		"darwin/amd64": "b513d15117cbf1bb05095dab41963fd394ff40de1fa99dcc6d7d1033bf292e9b",
		"linux/arm64":  "f220ca50ea4f3222043ff83cc224a1262e0196e9897acc0871efa94235b0c1fd",
		"linux/amd64":  "bfb3703ad5380242328459bc62ada4bde8ae7ba57c6e1eeadb9c00e08874c478",
	}},
}

// ClaudeCLIOptions configures the Claude adapter.
type ClaudeCLIOptions struct {
	// Executable is the official claude binary whose digest is checked
	// against the pins (default: ACPAgentOptions.Command[0]). Set it when
	// Command is the owner's launcher that runs that binary.
	Executable string
}

// NewClaudeCLIAdapter is the adapter of the official claude CLI.
func NewClaudeCLIAdapter(opts ClaudeCLIOptions) BrainAdapter {
	return &claudeCLIAdapter{opts: opts, convs: map[string]*cliConv{}}
}

type claudeCLIAdapter struct {
	opts ClaudeCLIOptions
	env  BrainEnv
	pin  claudeCLIPin
	mcp  *cliToolServer

	mu     sync.Mutex
	convs  map[string]*cliConv // by placeholder (the MCP bearer)
	closed bool
}

func (a *claudeCLIAdapter) log(level LogLevel, message string, err error) {
	if a.env.Log != nil {
		a.env.Log(level, message, err)
	}
}

func (a *claudeCLIAdapter) Start(ctx context.Context, env BrainEnv) (BrainCapabilities, error) {
	switch {
	case env.PlatformModel:
		return BrainCapabilities{}, errors.New("bmaexecutor: the Claude CLI uses the node owner's own credentials; PlatformModel (harness_model) is not allowed")
	case len(env.Models) > 0:
		return BrainCapabilities{}, errors.New("bmaexecutor: ACPAgentOptions.Models: the Claude CLI reports the models its owner's account may use")
	}
	a.env = env
	pin, err := a.checkPin(ctx)
	if err != nil {
		return BrainCapabilities{}, err
	}
	a.pin = pin
	server, err := startCLIToolServer(a)
	if err != nil {
		return BrainCapabilities{}, err
	}
	a.mcp = server
	// One process that is never prompted proves the control protocol (a
	// prompt would spend the owner's credit).
	if err := a.probe(ctx); err != nil {
		server.close()
		return BrainCapabilities{}, err
	}
	id := env.HarnessID
	if id == "" {
		id = "claude-code"
	}
	var hostTools []string
	for _, t := range env.HostTools {
		hostTools = append(hostTools, t.Name)
	}
	return BrainCapabilities{
		Harness: AgentHarnessCapability{
			ID: id, Version: pin.Version, Protocol: "claude-cli/stream-json",
			AgentName: "claude", AgentVersion: pin.Version,
			Streaming: "token", PermissionRelay: "full", SessionResume: true, StructuredErrors: true,
		},
		// No per-turn model: the platform hands a turn its agent's model, a
		// model of the tenant's provider config, not one of the owner's Claude
		// models; the CLI runs the model its owner configured.
		ModelCredentials: BrainModelOwner,
		ImageInputs:      true,
		HostTools:        hostTools,
		CustomTools:      true,
	}, nil
}

// checkPin finds the release of the configured binary: its version and its digest.
func (a *claudeCLIAdapter) checkPin(ctx context.Context) (claudeCLIPin, error) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, a.env.Command[0], append(slices.Clone(a.env.Command[1:]), "--version")...)
	cmd.Env = a.processEnv()
	out, err := cmd.Output()
	if err != nil {
		return claudeCLIPin{}, fmt.Errorf("claude --version: %w", err)
	}
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	i := slices.IndexFunc(claudeCLIPins, func(p claudeCLIPin) bool { return p.Version == version })
	if i < 0 {
		return claudeCLIPin{}, fmt.Errorf("claude %s is not a pinned release (supported: %s): refused (fail-closed)", version, claudeCLIPinned())
	}
	pin := claudeCLIPins[i]
	platform := runtime.GOOS + "/" + runtime.GOARCH
	want := pin.SHA256[platform]
	if want == "" {
		return claudeCLIPin{}, fmt.Errorf("claude %s has no pinned digest for %s: refused (fail-closed)", version, platform)
	}
	exe := a.opts.Executable
	if exe == "" {
		exe = a.env.Command[0]
	}
	got, err := fileSHA256(exe)
	if err != nil {
		return claudeCLIPin{}, err
	}
	if got != want {
		return claudeCLIPin{}, fmt.Errorf("%s is not the pinned claude %s binary for %s (sha256 %s, want %s): refused (fail-closed)", exe, version, platform, got, want)
	}
	return pin, nil
}

func claudeCLIPinned() string {
	var versions []string
	for _, p := range claudeCLIPins {
		versions = append(versions, p.Version)
	}
	return strings.Join(versions, ", ")
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// processEnv is the CLI's environment: PATH, HOME, USER, LANG, TMPDIR (HOME
// and USER of RunAs), then the host's Env.
func (a *claudeCLIAdapter) processEnv() []string {
	var env []string
	runAs := a.env.RunAs
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
	return append(env, a.env.Env...)
}

// probe starts the CLI once without a prompt: it must answer initialize.
func (a *claudeCLIAdapter) probe(ctx context.Context) error {
	dir, err := os.MkdirTemp(a.env.StateDir, "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	p, err := a.spawn(ctx, cliLaunch{cwd: dir, placeholder: "bma-probe", noSession: true})
	if err != nil {
		return err
	}
	p.stop(a.env.KillGrace)
	return nil
}

func (a *claudeCLIAdapter) Ref(sessionID string) string { return claudeSessionUUID(sessionID) }

// claudeSessionUUID is the conversation id of a platform session: a UUID
// version 5 (RFC 9562) of the session id in a fixed namespace, so every
// process on any restart names the same conversation.
func claudeSessionUUID(sessionID string) string {
	// Namespace: UUIDv5 of the URL "https://beeos.ai/bma/claude-session" in the RFC 9562 URL namespace.
	ns := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte("bma-claude-session:" + sessionID))
	sum := h.Sum(nil)
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (a *claudeCLIAdapter) Close() error {
	a.mu.Lock()
	a.closed = true
	convs := slices.Collect(maps.Values(a.convs))
	a.mu.Unlock()
	for _, c := range convs {
		c.stopProcess()
	}
	if a.mcp != nil {
		a.mcp.close()
	}
	return nil
}

// --- conversations ---

type cliConv struct {
	a           *claudeCLIAdapter
	sessionID   string
	uuid        string
	placeholder string
	cwd         string
	system      string

	mu     sync.Mutex
	proc   *cliProc
	model  string
	effort string
	prompt *cliPrompt
	closed bool
}

func (a *claudeCLIAdapter) Open(ctx context.Context, req BrainOpen) (BrainSession, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "agent unavailable: the adapter is closed"}
	}
	if c, _ := req.Current.(*cliConv); c != nil && c.Ready() {
		c.mu.Lock()
		c.model = req.Model
		c.mu.Unlock()
		return c, nil
	}
	c := &cliConv{a: a, sessionID: req.SessionID, uuid: claudeSessionUUID(req.SessionID), placeholder: newPlaceholder(),
		cwd: req.Cwd, system: req.SystemPrompt, model: req.Model}
	if req.Ref != "" && req.Ref != c.uuid {
		return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + req.Ref + " is not this session's (" + c.uuid + ")"}
	}
	// Registered first: the CLI connects to the tool server while it starts.
	a.mu.Lock()
	a.convs[c.placeholder] = c
	a.mu.Unlock()
	if err := c.ensure(ctx, req.Ref != "", req.Effort); err != nil {
		c.forget()
		return nil, err
	}
	return c, nil
}

// ensure has a CLI process for the conversation with the given model and
// effort, starting (or restarting) one with --resume once the conversation exists.
func (c *cliConv) ensure(ctx context.Context, resume bool, effort string) error {
	c.mu.Lock()
	proc, model := c.proc, c.model
	c.mu.Unlock()
	if proc != nil && proc.alive() && proc.model == model && proc.effort == effort {
		return nil
	}
	if proc != nil {
		proc.stop(c.a.env.KillGrace)
		resume = true
	}
	p, err := c.a.spawn(ctx, cliLaunch{cwd: c.cwd, placeholder: c.placeholder, system: c.system, uuid: c.uuid, resume: resume, model: model, effort: effort})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.proc, c.effort = p, effort
	c.mu.Unlock()
	return nil
}

func (c *cliConv) Ref() string { return c.uuid }

func (c *cliConv) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.proc != nil && c.proc.alive()
}

func (c *cliConv) stopProcess() {
	c.mu.Lock()
	proc := c.proc
	c.closed = true
	c.mu.Unlock()
	if proc != nil {
		proc.stop(c.a.env.KillGrace)
	}
}

func (c *cliConv) forget() {
	c.a.mu.Lock()
	if c.a.convs[c.placeholder] == c {
		delete(c.a.convs, c.placeholder)
	}
	c.a.mu.Unlock()
}

func (c *cliConv) Suspend(context.Context) error {
	c.stopProcess()
	c.forget()
	return nil
}

func (c *cliConv) Close(context.Context) error {
	c.stopProcess()
	c.forget()
	return nil
}

// cliPrompt is the input a conversation is running.
type cliPrompt struct {
	ctx  context.Context
	sink BrainSink

	mu      sync.Mutex
	blocks  map[int]*cliBlock // stream content blocks by index, of the current message
	message string            // id of the current message
	failure atomic.Pointer[string]
	errText string // the assistant's error type, if any
}

type cliBlock struct {
	kind  string // text, thinking, tool_use
	id    string // tool_use id
	name  string
	text  strings.Builder
	input strings.Builder
}

func (c *cliConv) Prompt(ctx context.Context, in BrainInput, sink BrainSink) error {
	if err := c.ensure(ctx, false, in.Effort); err != nil {
		return err
	}
	c.mu.Lock()
	proc := c.proc
	p := &cliPrompt{ctx: ctx, sink: sink, blocks: map[int]*cliBlock{}}
	c.prompt = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.prompt = nil
		c.mu.Unlock()
	}()
	content := []map[string]any{}
	for _, img := range in.Images {
		content = append(content, map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": img.MimeType, "data": base64.StdEncoding.EncodeToString(img.Data)}})
	}
	content = append(content, map[string]any{"type": "text", "text": in.Text})
	results := proc.subscribe(p)
	defer proc.unsubscribe()
	if err := proc.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil, "session_id": ""}); err != nil {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "write to claude: " + err.Error()}
	}
	select {
	case r := <-results:
		return p.end(r)
	case <-proc.done:
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "claude exited: " + proc.exitText()}
	case <-ctx.Done():
	}
	ictx, cancel := context.WithTimeout(context.Background(), c.a.env.CancelGrace)
	defer cancel()
	_, _ = proc.control(ictx, map[string]any{"subtype": "interrupt"})
	select {
	case <-results:
	case <-proc.done:
	case <-ictx.Done():
		c.a.log(LogWarn, "claude did not end a canceled turn within "+c.a.env.CancelGrace.String()+"; killing it", nil)
		proc.stop(c.a.env.KillGrace)
	}
	return ctx.Err()
}

// cliResult is the result message of a prompt.
type cliResult struct {
	Subtype    string `json:"subtype"`
	IsError    bool   `json:"is_error"`
	StopReason string `json:"stop_reason"`
	Result     string `json:"result"`
	Errors     []string
}

// end maps the end of a prompt to its result (contract stop codes).
func (p *cliPrompt) end(r cliResult) error {
	if f := p.failure.Load(); f != nil {
		return errors.New(*f)
	}
	p.mu.Lock()
	errText := p.errText
	p.mu.Unlock()
	switch {
	case errText == "max_output_tokens" || r.StopReason == "max_tokens":
		return fmt.Errorf("%s: %s", AgentErrorCodeOutputLimitReached, "the model reply was cut at its output limit")
	case slices.Contains([]string{"authentication_failed", "oauth_org_not_allowed", "billing_error", "cloud_credential_error", "account_on_hold", "verification_required"}, errText):
		return &AgentError{Code: AgentErrorCodeAgentAuthRequired, Message: "claude: " + errText + ": " + r.Result}
	case errText != "":
		return &AgentError{Code: AgentErrorCodeAgentError, Message: "claude: " + errText + ": " + r.Result}
	case r.IsError || r.Subtype != "success":
		return &AgentError{Code: AgentErrorCodeAgentError, Message: "claude: " + r.Subtype + ": " + r.Result}
	}
	return nil
}

func (p *cliPrompt) fail(code, detail string) {
	message := code + ": " + detail
	p.failure.CompareAndSwap(nil, &message)
}

// --- the CLI process ---

type cliLaunch struct {
	cwd, placeholder, system, uuid, model, effort string
	resume, noSession                             bool
}

type cliProc struct {
	a      *claudeCLIAdapter
	cmd    *exec.Cmd
	tree   *processTree
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr *tailBuffer
	done   chan struct{}
	model  string
	effort string

	mu      sync.Mutex
	nextID  int64
	waiting map[string]chan cliControlResponse
	prompt  *cliPrompt
	results chan cliResult
	perms   map[string]context.CancelFunc // pending can_use_tool by request id
	exitErr error
	tools   []string // the injected tools the init message must list
}

type cliControlResponse struct {
	Subtype  string          `json:"subtype"`
	ID       string          `json:"request_id"`
	Response json.RawMessage `json:"response"`
	Error    string          `json:"error"`
}

// mcpConfig is the --mcp-config of a conversation: the loopback tool server
// with the conversation's placeholder as bearer.
func (a *claudeCLIAdapter) mcpConfig(placeholder string) string {
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{claudeMCPServerName: map[string]any{
		"type": "http", "url": a.mcp.url + "/mcp", "headers": map[string]string{"Authorization": "Bearer " + placeholder}}}})
	return string(raw)
}

// args are the CLI's arguments for a launch.
func (a *claudeCLIAdapter) args(l cliLaunch) []string {
	args := append(slices.Clone(a.env.Command[1:]),
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--permission-prompt-tool", "stdio", "--permission-mode", "default",
		"--tools", "", "--mcp-config", a.mcpConfig(l.placeholder), "--strict-mcp-config", "--setting-sources", "")
	switch {
	case l.noSession:
		args = append(args, "--no-session-persistence")
	case l.resume:
		args = append(args, "--resume", l.uuid)
	default:
		args = append(args, "--session-id", l.uuid)
	}
	if l.system != "" {
		args = append(args, "--append-system-prompt", l.system)
	}
	if l.model != "" && l.model != "default" {
		args = append(args, "--model", l.model)
	}
	if l.effort != "" {
		args = append(args, "--effort", l.effort)
	}
	return args
}

func (a *claudeCLIAdapter) spawn(ctx context.Context, l cliLaunch) (*cliProc, error) {
	cmd := exec.Command(a.env.Command[0], a.args(l)...)
	cmd.Env = a.processEnv()
	cmd.Dir = l.cwd
	setProcessGroupID(cmd)
	setCredential(cmd, a.env.RunAs)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &cliProc{a: a, cmd: cmd, stdin: stdin, stderr: &tailBuffer{max: 16 << 10}, done: make(chan struct{}),
		model: l.model, effort: l.effort, waiting: map[string]chan cliControlResponse{}, perms: map[string]context.CancelFunc{},
		tools: a.toolNames()}
	cmd.Stderr = p.stderr
	tree, err := startCommand(cmd)
	if err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "start claude: " + err.Error()}
	}
	p.tree = tree
	go p.readLoop(stdout)
	go func() {
		err := cmd.Wait()
		tree.close()
		p.mu.Lock()
		p.exitErr = err
		p.mu.Unlock()
		close(p.done)
	}()
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := p.control(ictx, map[string]any{"subtype": "initialize"}); err != nil {
		p.stop(a.env.KillGrace)
		text := p.exitText()
		if strings.Contains(text, "No conversation found") {
			return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + l.uuid + " was not found; it is not recreated"}
		}
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "claude initialize: " + err.Error() + ": " + text}
	}
	return p, nil
}

// toolNames are the MCP names of the tools the CLI must offer: the six
// platform tools and the host tools.
func (a *claudeCLIAdapter) toolNames() []string {
	var names []string
	for name := range a.env.AllowedTools() {
		names = append(names, claudeToolPrefix+name)
	}
	slices.Sort(names)
	return names
}

func (p *cliProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *cliProc) exitText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%v: %s", p.exitErr, lastLine(p.stderr.String()))
}

func (p *cliProc) stop(grace time.Duration) {
	if !p.alive() {
		return
	}
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return
	case <-time.After(grace):
	}
	_ = p.tree.signal(signalTerminate)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = p.tree.signal(signalKill)
		<-p.done
	}
}

func (p *cliProc) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// control sends a control request and waits for its response.
func (p *cliProc) control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := fmt.Sprintf("bma_%d", p.nextID)
	ch := make(chan cliControlResponse, 1)
	p.waiting[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
	}()
	if err := p.write(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Subtype != "success" {
			return nil, fmt.Errorf("control %v: %s", request["subtype"], r.Error)
		}
		return r.Response, nil
	case <-p.done:
		return nil, errors.New("claude exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *cliProc) subscribe(prompt *cliPrompt) <-chan cliResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompt, p.results = prompt, make(chan cliResult, 1)
	return p.results
}

func (p *cliProc) unsubscribe() {
	p.mu.Lock()
	p.prompt, p.results = nil, nil
	p.mu.Unlock()
}

func (p *cliProc) current() *cliPrompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompt
}

func (p *cliProc) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			p.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

// cliMessage is one stdout line of the CLI.
type cliMessage struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Response        json.RawMessage `json:"response"`
	Event           json.RawMessage `json:"event"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Error           string          `json:"error"`
	Tools           []string        `json:"tools"`
	PermissionMode  string          `json:"permissionMode"`
}

func (p *cliProc) dispatch(line []byte) {
	var m cliMessage
	if json.Unmarshal(line, &m) != nil {
		p.a.log(LogWarn, "claude wrote a non-JSON line on stdout", nil)
		return
	}
	switch m.Type {
	case "control_response":
		var r cliControlResponse
		if json.Unmarshal(m.Response, &r) == nil {
			p.mu.Lock()
			ch := p.waiting[r.ID]
			p.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
		return
	case "control_request":
		go p.onControl(m.RequestID, m.Request)
		return
	case "control_cancel_request":
		p.mu.Lock()
		cancel := p.perms[m.RequestID]
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	prompt := p.current()
	if prompt == nil || (m.ParentToolUseID != nil && *m.ParentToolUseID != "") {
		return // nothing running, or a subagent's message
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			p.checkInit(prompt, m)
		}
	case "stream_event":
		p.onStream(prompt, m.Event)
	case "assistant":
		if m.Error != "" {
			prompt.mu.Lock()
			prompt.errText = m.Error
			prompt.mu.Unlock()
		}
	case "user":
		p.onUser(prompt, m.Message)
	case "result":
		var r cliResult
		_ = json.Unmarshal(line, &r)
		p.mu.Lock()
		results := p.results
		p.mu.Unlock()
		if results != nil {
			select {
			case results <- r:
			default:
			}
		}
	}
}

// checkInit fails the turn unless the CLI offers exactly the injected tools in the mode default.
func (p *cliProc) checkInit(prompt *cliPrompt, m cliMessage) {
	got := slices.Sorted(slices.Values(m.Tools))
	if !slices.Equal(got, p.tools) {
		prompt.fail(AgentErrorCodeAgentProtocolError, fmt.Sprintf("tool_catalog_violation: claude offers the tools [%s], not exactly [%s]", strings.Join(got, ","), strings.Join(p.tools, ",")))
		p.interrupt()
		return
	}
	if m.PermissionMode != "default" {
		prompt.fail(AgentErrorCodeAgentProtocolError, fmt.Sprintf("mode_not_default: claude started the turn in the permission mode %q", m.PermissionMode))
		p.interrupt()
	}
}

func (p *cliProc) interrupt() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.a.env.CancelGrace)
		defer cancel()
		_, _ = p.control(ctx, map[string]any{"subtype": "interrupt"})
	}()
}

func (p *cliProc) onStream(prompt *cliPrompt, raw json.RawMessage) {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	prompt.mu.Lock()
	switch e.Type {
	case "message_start":
		prompt.message, prompt.blocks = e.Message.ID, map[int]*cliBlock{}
		prompt.mu.Unlock()
		return
	case "content_block_start":
		prompt.blocks[e.Index] = &cliBlock{kind: e.ContentBlock.Type, id: e.ContentBlock.ID, name: e.ContentBlock.Name}
		prompt.mu.Unlock()
		return
	}
	b := prompt.blocks[e.Index]
	key := fmt.Sprintf("%s:%d", prompt.message, e.Index)
	prompt.mu.Unlock()
	if b == nil {
		return
	}
	switch e.Type {
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			b.text.WriteString(e.Delta.Text)
			prompt.sink.Delta(BrainKindMessage, key, e.Delta.Text)
		case "thinking_delta":
			b.text.WriteString(e.Delta.Thinking)
			prompt.sink.Delta(BrainKindThinking, key, e.Delta.Thinking)
		case "input_json_delta":
			b.input.WriteString(e.Delta.PartialJSON)
		}
	case "content_block_stop":
		switch b.kind {
		case "text":
			if strings.TrimSpace(b.text.String()) != "" {
				prompt.sink.Message(BrainKindMessage, key, b.text.String())
			}
		case "thinking":
			prompt.sink.Message(BrainKindThinking, key, b.text.String())
		case "tool_use":
			input := map[string]any{}
			if s := b.input.String(); s != "" {
				_ = json.Unmarshal([]byte(s), &input)
			}
			name, ok := claudeHostTool(b.name, p.tools)
			if !ok {
				name = b.name
			}
			prompt.sink.ToolUse(BrainCall{ID: b.id, Name: name, Input: input}, "")
		}
	}
}

// claudeHostTool maps an injected MCP tool name to the platform's tool name.
func claudeHostTool(mcpName string, injected []string) (string, bool) {
	if !slices.Contains(injected, mcpName) {
		return "", false
	}
	return strings.TrimPrefix(mcpName, claudeToolPrefix), true
}

func (p *cliProc) onUser(prompt *cliPrompt, raw json.RawMessage) {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		prompt.sink.ToolResult(b.ToolUseID, claudeResultText(b.Content), b.IsError)
	}
}

// claudeResultText is the text of a tool_result's content (a string or blocks).
func claudeResultText(raw json.RawMessage) []ContentBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []ContentBlock{{Type: ContentBlockTypeText, Text: s}}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var out []ContentBlock
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, ContentBlock{Type: ContentBlockTypeText, Text: b.Text})
		}
	}
	return out
}

// onControl answers the CLI's control requests: can_use_tool goes to the host.
func (p *cliProc) onControl(id string, raw json.RawMessage) {
	var r struct {
		Subtype   string         `json:"subtype"`
		ToolName  string         `json:"tool_name"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Subtype != "can_use_tool" {
		_ = p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id,
			"error": "unsupported control request " + r.Subtype}})
		return
	}
	answer := func(result map[string]any) {
		_ = p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": result}})
	}
	deny := func(message string) {
		answer(map[string]any{"behavior": "deny", "message": message, "toolUseID": r.ToolUseID})
	}
	prompt := p.current()
	name, ok := claudeHostTool(r.ToolName, p.tools)
	switch {
	case prompt == nil:
		deny("No turn of this session is running.")
		return
	case !ok:
		p.a.log(LogWarn, fmt.Sprintf("claude asked to use %q, which is not a platform tool: refused", r.ToolName), nil)
		deny("The tool " + r.ToolName + " is not available.")
		return
	}
	ctx, cancel := context.WithCancel(prompt.ctx)
	defer cancel()
	p.mu.Lock()
	p.perms[id] = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.perms, id)
		p.mu.Unlock()
	}()
	d := prompt.sink.Permission(ctx, BrainCall{ID: r.ToolUseID, Name: name, Input: r.Input})
	switch d.Outcome {
	case BrainAllow:
		result := map[string]any{"behavior": "allow", "updatedInput": r.Input, "toolUseID": r.ToolUseID}
		if d.Session {
			result["updatedPermissions"] = []any{map[string]any{"type": "addRules", "rules": []any{map[string]any{"toolName": r.ToolName}},
				"behavior": "allow", "destination": "session"}}
		}
		answer(result)
	case BrainDeny:
		message := d.Message
		if message == "" {
			message = refusalText
		}
		deny(message)
	default:
		deny("The request was withdrawn before it was answered.")
	}
}

// --- the tools as an MCP server on loopback ---

// cliToolServer serves the platform tools and the host tools to the CLI
// over MCP Streamable HTTP; a request carries its conversation's
// placeholder as bearer, so a conversation reaches only its own turn.
type cliToolServer struct {
	a      *claudeCLIAdapter
	url    string
	server *http.Server
	anon   atomic.Int64
}

func startCLIToolServer(a *claudeCLIAdapter) (*cliToolServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &cliToolServer{a: a, url: "http://" + listener.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.serve)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

func (s *cliToolServer) close() { _ = s.server.Close() }

func (s *cliToolServer) conv(token string) (*cliConv, bool) {
	if token == "bma-probe" {
		return nil, true
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	for placeholder, c := range s.a.convs {
		if subtle.ConstantTimeCompare([]byte(placeholder), []byte(token)) == 1 {
			return c, true
		}
	}
	return nil, false
}

func (s *cliToolServer) serve(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	conv, known := s.conv(token)
	if token == "" || !known {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cliMaxToolRequestBytes))
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
	if len(req.ID) == 0 {
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
		reply(map[string]any{"tools": s.a.toolList()}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || !s.a.env.AllowedTools()[p.Name] {
			reply(nil, map[string]any{"code": -32602, "message": "unknown tool " + p.Name})
			return
		}
		callID, _ := p.Meta[claudeToolUseIDMeta].(string)
		// A call can outlast the CLI's first-byte budget for an HTTP MCP
		// request: answer as an SSE stream, the result as its one event.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": s.call(r.Context(), conv, callID, p.Name, p.Arguments)})
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	default:
		reply(nil, map[string]any{"code": -32601, "message": "method not found: " + req.Method})
	}
}

func (s *cliToolServer) call(ctx context.Context, c *cliConv, callID, name string, input map[string]any) map[string]any {
	var result *RouteResult
	var prompt *cliPrompt
	if c != nil {
		c.mu.Lock()
		prompt = c.prompt
		c.mu.Unlock()
	}
	if prompt == nil {
		result = refused("stale_turn", "no turn of this session is running")
	} else {
		if callID == "" {
			callID = fmt.Sprintf("mcp-%d", s.anon.Add(1))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(prompt.ctx, cancel)()
		result = prompt.sink.Call(ctx, BrainCall{ID: callID, Name: name, Input: input})
	}
	content := []any{map[string]any{"type": "text", "text": result.Text}}
	for _, img := range result.Images {
		content = append(content, map[string]any{"type": "image", "data": img.Data, "mimeType": img.MimeType})
	}
	return map[string]any{"content": content, "isError": result.IsError}
}

// toolList is the MCP tools/list: the six platform tools and the host tools.
func (a *claudeCLIAdapter) toolList() []any {
	tools := claudeToolList()
	for _, t := range a.env.HostTools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Parameters})
	}
	return tools
}

func newPlaceholder() string {
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	return "bma-local-" + hex.EncodeToString(key)
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
