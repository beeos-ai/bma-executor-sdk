package bmaexecutor

// The Codex adapter: drives the official OpenAI `codex` CLI, unmodified,
// through its app server (`codex app-server`, JSON-RPC over stdio;
// https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md).
// Nothing sits between the executor and the CLI.
//
//	BrainHost / BrainSink        Codex adapter                        codex app-server
//	Open                         one app server per conversation      thread/start (dynamicTools) | thread/resume
//	Prompt                       user input                           turn/start {input, model, effort}
//	Delta                        <-                                   item/agentMessage/delta, item/reasoning/*Delta
//	Message                      <-                                   item/completed (agentMessage, reasoning)
//	ToolUse, Permission          <->                                  item/tool/call (server request)
//	Call, ToolResult             <->                                  item/tool/call response {contentItems, success}
//	ctx canceled                 ->                                   turn/interrupt; kill after CancelGrace
//	Prompt's error               <-                                   turn/completed error.codexErrorInfo, error
//
// Credentials. The CLI runs with its node owner's own login (ChatGPT plan or
// API key) in the BMA Codex home: the CODEX_HOME of ACPAgentOptions.Env, a
// home of its own that the owner signed in to (`codex login`). They never
// pass through BMA: no model proxy, no harness_model, and the SDK never reads
// or logs them.
//
// Gates, all fail closed: the CLI binary must be a pinned release (version and
// sha256, codexPins); the Codex home holds no file that configures the CLI
// (config.toml, AGENTS.md, hooks.json: a configuration file adds MCP servers,
// hooks and instructions no command-line setting can take away); every
// built-in tool and injected instruction is turned off on the command line
// (codexSettings), so the model is offered exactly the platform tools and the
// turn's tools, as dynamic tools the host runs; an item of any built-in tool
// or a server request other than a dynamic tool call ends the turn with
// agent_protocol_error.

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// codexPin is one release of the official codex CLI this SDK is tested with:
// its version (codex --version) and the sha256 of its binary per GOOS/GOARCH.
// Adding a release is a reviewed change: its contract tests
// (brain_codex_app_server_test.go) must pass against it.
type codexPin struct {
	Version string
	SHA256  map[string]string
}

// codexPins: the binary inside the release asset codex-<target>.tar.gz of
// https://github.com/openai/codex/releases/tag/rust-v<version>, the asset
// checked against the digest GitHub publishes for it.
var codexPins = []codexPin{
	{Version: "0.168.0", SHA256: map[string]string{
		"darwin/arm64": "1b7ee0f02650f3e2d31f50232699f667a1cbe60f431a57e118f2e785fe372c4b",
		"darwin/amd64": "0e9f20128c837f5087cd1e3a191b96e2a93bacbeb7f1ac89cf0ed09f69819f9a",
		"linux/arm64":  "6fbce31b3288c1515af48c4ad53f1e66c1579626459413c84b57e7c7d76c63ad",
		"linux/amd64":  "a8cb58c528c76e7313d4a70294ec614757b0f4c7bf9683e11c88ecad8c3d421b",
	}},
	{Version: "0.169.0", SHA256: map[string]string{
		"darwin/arm64": "739cda86e15d30a6e2b40cf8dcbbfb988aaf48b305b27065573bb8e9fd41d002",
		"darwin/amd64": "e57da7be8695c079d2d1d156dab79708e43ebb6cf7e34527b8b01d6aa942788e",
		"linux/arm64":  "3bb41c9a80187773e2a59278d4fc355b1a163c31bb5a79581e37d4fe428af047",
		"linux/amd64":  "b452ca20f838a8855a88a41050ee4a694033fff4ea3ecdad5633e6e0e8d13f2b",
	}},
}

// codexDisabledFeatures are the CLI features that bring a built-in tool or
// load something of the owner's (skills, plugins, apps, hooks, memories).
var codexDisabledFeatures = []string{
	"shell_tool", "unified_exec", "view_image", "multi_agent", "multi_agent_v2", "goals", "apps", "plugins", "hooks", "memories",
	"browser_use", "computer_use", "image_generation", "in_app_browser", "tool_suggest", "skill_mcp_dependency_install", "remote_plugin",
	"sleep_tool", "code_mode_host", "skill_search", "workspace_dependencies",
}

// codexSettings are the configuration overrides of every launch: no web
// search, no sub-agents, no question tool of codex's own, no injected
// instructions besides the platform's system prompt (the project's AGENTS.md
// of the working directory stays: project instructions, as Claude Code reads
// a project's CLAUDE.md), no update check.
var codexSettings = []string{
	`web_search="disabled"`, "agents.enabled=false", "tools.experimental_request_user_input.enabled=false",
	"skills.include_instructions=false", "include_permissions_instructions=false", "include_apps_instructions=false",
	"include_collaboration_mode_instructions=false", "include_environment_context=false", "check_for_update_on_startup=false",
}

// codexModelTools are the fields of codex's model catalog that bring tools
// with a model: its tool mode (code mode runs every tool from a JavaScript
// cell and adds tools of its own), apply_patch, sub-agents, extra tools. The
// adapter hands codex its own catalog with them cleared (codexCatalog), so a
// turn's tools are exactly the dynamic tools, called directly.
var codexModelTools = map[string]any{"tool_mode": nil, "apply_patch_tool_type": nil, "multi_agent_version": nil, "experimental_supported_tools": []any{}}

// codexHomeFiles configure the CLI from the Codex home; the adapter refuses a home that has one.
var codexHomeFiles = []string{"config.toml", "AGENTS.md", "AGENTS.override.md", "hooks.json"}

// codexOwnerSettings are the configuration keys an owner may set in
// Command[1:] (as -c key=value): the model provider of their own account.
var codexOwnerSettings = []string{"model_provider=", "model_providers."}

// codexItemTypes are the thread items a turn may have; any other is a
// built-in tool (commandExecution, fileChange, mcpToolCall, webSearch, ...).
var codexItemTypes = []string{"userMessage", "agentMessage", "reasoning", "dynamicToolCall", "contextCompaction", "plan"}

// NewCodexAppServerAdapter is the adapter of the official codex CLI. Its
// Command is the pinned codex binary, optionally followed by -c settings of
// the owner's model provider (codexOwnerSettings); ACPAgentOptions.Env must
// set CODEX_HOME to the BMA Codex home.
func NewCodexAppServerAdapter() BrainAdapter {
	return &codexAdapter{}
}

type codexAdapter struct {
	env   BrainEnv
	pin   codexPin
	home  string
	owner []string // the owner's -c settings
	// catalog is the model catalog the app server runs with (writeCatalog).
	catalog string
	// efforts are the default reasoning effort of each model, and of ""
	// (codex's default model).
	efforts map[string]string

	mu     sync.Mutex
	convs  map[*codexConv]bool
	closed bool
}

func (a *codexAdapter) log(level LogLevel, message string, err error) {
	if a.env.Log != nil {
		a.env.Log(level, message, err)
	}
}

func (a *codexAdapter) Start(ctx context.Context, env BrainEnv) (BrainCapabilities, error) {
	switch {
	case env.PlatformModel:
		return BrainCapabilities{}, errors.New("bmaexecutor: the codex CLI uses the node owner's own credentials; PlatformModel (harness_model) is not allowed")
	case len(env.Models) > 0:
		return BrainCapabilities{}, errors.New("bmaexecutor: ACPAgentOptions.Models: the codex CLI reports the models its owner's account may use")
	}
	a.env, a.convs = env, map[*codexConv]bool{}
	for _, kv := range env.Env {
		if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
			a.home = v
		}
	}
	if !filepath.IsAbs(a.home) {
		return BrainCapabilities{}, fmt.Errorf("bmaexecutor: ACPAgentOptions.Env must set CODEX_HOME to the absolute path of the BMA Codex home, got %q", a.home)
	}
	for i := 1; i < len(env.Command); i += 2 {
		if env.Command[i] != "-c" || i+1 == len(env.Command) ||
			!slices.ContainsFunc(codexOwnerSettings, func(p string) bool { return strings.HasPrefix(env.Command[i+1], p) }) {
			return BrainCapabilities{}, fmt.Errorf("bmaexecutor: the codex command takes only -c settings of the model provider (%s), got %q", strings.Join(codexOwnerSettings, ", "), env.Command[1:])
		}
		a.owner = append(a.owner, env.Command[i], env.Command[i+1])
	}
	if err := a.checkHome(); err != nil {
		return BrainCapabilities{}, err
	}
	pin, err := a.checkPin(ctx)
	if err != nil {
		return BrainCapabilities{}, err
	}
	a.pin = pin
	if err := a.writeCatalog(ctx); err != nil {
		return BrainCapabilities{}, err
	}
	models, err := a.probe(ctx)
	if err != nil {
		return BrainCapabilities{}, err
	}
	id := env.HarnessID
	if id == "" {
		id = ACPProfileCodex
	}
	return BrainCapabilities{
		Harness: AgentHarnessCapability{
			ID: id, Version: pin.Version, Protocol: "codex-app-server/json-rpc",
			AgentName: "codex", AgentVersion: pin.Version,
			Streaming: "token", PermissionRelay: "full", SessionResume: true, StructuredErrors: true,
		},
		// The models and reasoning efforts the CLI reports for the owner's
		// account; a turn's model and thinking level are passed unchanged
		// (turn/start model, effort).
		ModelCredentials: BrainModelOwner,
		ModelChoice:      true,
		Models:           models,
		ImageInputs:      true,
		HostTools:        true,
		CustomTools:      true,
		InputSkills:      true,
	}, nil
}

// checkHome refuses a Codex home with a file that configures the CLI.
func (a *codexAdapter) checkHome() error {
	for _, name := range codexHomeFiles {
		if _, err := os.Lstat(filepath.Join(a.home, name)); err == nil {
			return fmt.Errorf("the Codex home %s has %s, which would configure the codex CLI beyond the platform's settings: refused (fail-closed); the BMA Codex home must hold only the owner's login", a.home, name)
		}
	}
	return nil
}

// checkPin finds the release of the configured binary: its version and its digest.
func (a *codexAdapter) checkPin(ctx context.Context) (codexPin, error) {
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(vctx, a.env.Command[0], "--version")
	cmd.Env = a.processEnv()
	out, err := cmd.Output()
	if err != nil {
		return codexPin{}, fmt.Errorf("codex --version: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli ")
	i := slices.IndexFunc(codexPins, func(p codexPin) bool { return p.Version == version })
	if i < 0 {
		return codexPin{}, fmt.Errorf("codex %s is not a pinned release (supported: %s): refused (fail-closed)", version, codexPinned())
	}
	pin := codexPins[i]
	platform := runtime.GOOS + "/" + runtime.GOARCH
	want := pin.SHA256[platform]
	if want == "" {
		return codexPin{}, fmt.Errorf("codex %s has no pinned digest for %s: refused (fail-closed)", version, platform)
	}
	got, err := fileSHA256(a.env.Command[0])
	if err != nil {
		return codexPin{}, err
	}
	if got != want {
		return codexPin{}, fmt.Errorf("%s is not the pinned codex %s binary for %s (sha256 %s, want %s): refused (fail-closed)", a.env.Command[0], version, platform, got, want)
	}
	return pin, nil
}

func codexPinned() string {
	var versions []string
	for _, p := range codexPins {
		versions = append(versions, p.Version)
	}
	return strings.Join(versions, ", ")
}

// writeCatalog writes the model catalog codex would use (`codex debug
// models`, for the same home and provider) with the fields of
// codexModelTools cleared, for model_catalog_json.
func (a *codexAdapter) writeCatalog(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, a.env.Command[0], append(append([]string{"debug", "models"}, a.owner...), "-c", "check_for_update_on_startup=false")...)
	cmd.Env = a.processEnv()
	cmd.Dir = a.env.StateDir
	setCredential(cmd, a.env.RunAs)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("codex debug models: %w", err)
	}
	var catalog map[string]any
	if err := json.Unmarshal(out, &catalog); err != nil {
		return fmt.Errorf("codex debug models: %w", err)
	}
	models, _ := catalog["models"].([]any)
	if len(models) == 0 {
		return errors.New("codex debug models reported no models: refused (fail-closed)")
	}
	for _, m := range models {
		model, ok := m.(map[string]any)
		if !ok {
			return errors.New("codex debug models: a model is not an object: refused (fail-closed)")
		}
		for field, value := range codexModelTools {
			model[field] = value
		}
	}
	raw, _ := json.Marshal(catalog)
	path := filepath.Join(a.env.StateDir, "codex-model-catalog.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	if runAs := a.env.RunAs; runAs != nil {
		if err := os.Chown(path, int(runAs.UID), int(runAs.GID)); err != nil {
			return err
		}
	}
	a.catalog = path
	return nil
}

// processEnv is the CLI's environment: PATH, HOME, USER, LANG, TMPDIR (HOME
// and USER of RunAs), then the host's Env (CODEX_HOME among it).
func (a *codexAdapter) processEnv() []string {
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

// args are the app server's arguments: the owner's model provider, then the gates.
func (a *codexAdapter) args() []string {
	args := append([]string{"app-server"}, a.owner...)
	for _, f := range codexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	for _, s := range codexSettings {
		args = append(args, "-c", s)
	}
	raw, _ := json.Marshal(a.catalog)
	return append(args, "-c", "model_catalog_json="+string(raw))
}

// probe starts the app server once: the owner must be signed in, and
// model/list is what the owner's account may use (id, displayName,
// supportedReasoningEfforts, defaultReasoningEffort).
func (a *codexAdapter) probe(ctx context.Context) ([]AgentModel, error) {
	p, err := a.spawn(ctx)
	if err != nil {
		return nil, err
	}
	defer p.stop(a.env.KillGrace)
	var account struct {
		Account            *struct{ Type string } `json:"account"`
		RequiresOpenaiAuth bool                   `json:"requiresOpenaiAuth"`
	}
	if err := p.call(ctx, "account/read", map[string]any{}, &account); err != nil {
		return nil, err
	}
	if account.Account == nil && account.RequiresOpenaiAuth {
		return nil, &AgentError{Code: AgentErrorCodeAgentAuthRequired, Message: "agent_auth_required: codex is not signed in to the BMA Codex home " + a.home + " (CODEX_HOME=" + a.home + " codex login)"}
	}
	var models []AgentModel
	a.efforts = map[string]string{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data []struct {
				ID                        string `json:"id"`
				DisplayName               string `json:"displayName"`
				Hidden                    bool   `json:"hidden"`
				IsDefault                 bool   `json:"isDefault"`
				DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := p.call(ctx, "model/list", params, &page); err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			if m.Hidden {
				continue
			}
			if m.IsDefault {
				a.efforts[""] = m.DefaultReasoningEffort
			}
			a.efforts[m.ID] = m.DefaultReasoningEffort
			model := AgentModel{ID: m.ID, DisplayName: m.DisplayName}
			for _, e := range m.SupportedReasoningEfforts {
				if slices.Contains(acpReasoningEfforts, e.ReasoningEffort) {
					model.ThinkingLevels = append(model.ThinkingLevels, e.ReasoningEffort)
				}
			}
			models = append(models, model)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(models) == 0 {
		return nil, errors.New("codex model/list reported no models: refused (fail-closed)")
	}
	return models, nil
}

// Ref: the platform session id names the conversation; the adapter keeps
// the codex thread of each (codexThreads).
func (a *codexAdapter) Ref(sessionID string) string { return sessionID }

func (a *codexAdapter) Close() error {
	a.mu.Lock()
	a.closed = true
	convs := slices.Collect(maps.Keys(a.convs))
	a.mu.Unlock()
	for _, c := range convs {
		c.stopProcess()
	}
	return nil
}

// --- the thread of a session ---

// codexThread is what the adapter keeps of a session's conversation: the
// codex thread and the tools it was started with (codex fixes a thread's
// dynamic tools when it starts).
type codexThread struct {
	Thread string   `json:"thread"`
	Tools  []string `json:"tools"`
}

func (a *codexAdapter) threadPath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(a.env.StateDir, "codex-threads", hex.EncodeToString(sum[:16])+".json")
}

func (a *codexAdapter) loadThread(sessionID string) (*codexThread, error) {
	raw, err := os.ReadFile(a.threadPath(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t codexThread
	if err := json.Unmarshal(raw, &t); err != nil || t.Thread == "" {
		return nil, fmt.Errorf("codex thread record of %s is unreadable: %v", sessionID, err)
	}
	return &t, nil
}

func (a *codexAdapter) saveThread(sessionID string, t codexThread) error {
	path := a.threadPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(t)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- conversations ---

type codexConv struct {
	a         *codexAdapter
	sessionID string
	cwd       string
	system    string

	mu     sync.Mutex
	proc   *codexProc
	thread codexThread
	model  string
	// tools are the turn's tools besides the six (BrainOpen.Tools).
	tools  []BrainTool
	prompt *codexPrompt
	closed bool
}

func (a *codexAdapter) Open(ctx context.Context, req BrainOpen) (BrainSession, error) {
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "agent unavailable: the adapter is closed"}
	}
	if req.Ref != "" && req.Ref != req.SessionID {
		return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + req.Ref + " is not this session's (" + req.SessionID + ")"}
	}
	if c, _ := req.Current.(*codexConv); c != nil && c.Ready() {
		if err := c.offers(req.Tools); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.model, c.tools = req.Model, req.Tools
		c.mu.Unlock()
		return c, nil
	}
	if err := a.checkHome(); err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: err.Error()}
	}
	known, err := a.loadThread(req.SessionID)
	if err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: err.Error()}
	}
	if req.Ref != "" && known == nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + req.Ref + " was not found; it is not recreated"}
	}
	c := &codexConv{a: a, sessionID: req.SessionID, cwd: req.Cwd, system: req.SystemPrompt, model: req.Model, tools: req.Tools}
	p, err := a.spawn(ctx)
	if err != nil {
		return nil, err
	}
	c.proc = p
	if known != nil {
		c.thread = *known
		if err := c.offers(req.Tools); err != nil {
			p.stop(a.env.KillGrace)
			return nil, err
		}
		if err := c.resume(ctx); err != nil {
			p.stop(a.env.KillGrace)
			return nil, err
		}
	} else if err := c.start(ctx); err != nil {
		p.stop(a.env.KillGrace)
		return nil, err
	}
	a.mu.Lock()
	a.convs[c] = true
	a.mu.Unlock()
	return c, nil
}

// threadParams are the settings of thread/start and thread/resume: the
// platform's system prompt replaces the CLI's own; approvals and the
// read-only sandbox only matter should a built-in tool run, which the gates
// refuse anyway.
func (c *codexConv) threadParams() map[string]any {
	return map[string]any{"cwd": c.cwd, "baseInstructions": c.system, "approvalPolicy": "untrusted", "sandbox": "read-only"}
}

func (c *codexConv) start(ctx context.Context) error {
	params := c.threadParams()
	specs := codexToolSpecs(c.tools)
	params["dynamicTools"] = specs
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.proc.call(ctx, "thread/start", params, &r); err != nil {
		return err
	}
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s["name"].(string))
	}
	slices.Sort(names)
	c.thread = codexThread{Thread: r.Thread.ID, Tools: names}
	c.proc.setThread(r.Thread.ID)
	if err := c.a.saveThread(c.sessionID, c.thread); err != nil {
		return &AgentError{Code: AgentErrorCodeAgentError, Message: "record the codex thread: " + err.Error()}
	}
	return nil
}

func (c *codexConv) resume(ctx context.Context) error {
	params := c.threadParams()
	params["threadId"] = c.thread.Thread
	if err := c.proc.call(ctx, "thread/resume", params, nil); err != nil {
		var rpcErr *codexRPCError
		if errors.As(err, &rpcErr) && strings.Contains(rpcErr.Message, "no rollout found") {
			return &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation of " + c.sessionID + " (codex thread " + c.thread.Thread + ") was not found; it is not recreated"}
		}
		return err
	}
	c.proc.setThread(c.thread.Thread)
	return nil
}

// offers refuses a turn whose tools the thread was not started with: codex
// keeps a thread's dynamic tools for its life. A turn that offers fewer
// tools is served; a call of a tool it does not offer is refused (call).
func (c *codexConv) offers(tools []BrainTool) error {
	for name := range brainToolNames(tools) {
		if !slices.Contains(c.thread.Tools, name) {
			return &AgentError{Code: AgentErrorCodeAgentError, Message: "agent_error: the codex conversation of " + c.sessionID + " was started without the tool " + name + " and codex fixes a conversation's tools when it starts; start a new session to use it"}
		}
	}
	return nil
}

func (c *codexConv) Ref() string { return c.sessionID }

func (c *codexConv) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.proc != nil && c.proc.alive()
}

func (c *codexConv) stopProcess() {
	c.mu.Lock()
	proc := c.proc
	c.closed = true
	c.mu.Unlock()
	if proc != nil {
		proc.stop(c.a.env.KillGrace)
	}
}

func (c *codexConv) forget() {
	c.a.mu.Lock()
	delete(c.a.convs, c)
	c.a.mu.Unlock()
}

func (c *codexConv) Suspend(context.Context) error {
	c.stopProcess()
	c.forget()
	return nil
}

// Close ends the conversation: the thread is left to codex's own history
// (the owner's to delete), the record that resumes it is removed.
func (c *codexConv) Close(context.Context) error {
	c.stopProcess()
	c.forget()
	_ = os.Remove(c.a.threadPath(c.sessionID))
	return nil
}

// codexPrompt is the turn a conversation is running.
type codexPrompt struct {
	ctx   context.Context
	sink  BrainSink
	tools map[string]bool // the turn's tools, by platform name

	mu      sync.Mutex
	turnID  string
	early   []json.RawMessage // turn/completed before turn/start answered
	started chan string       // turn/started while the turn id is unknown
	done    chan codexTurnEnd
	failure atomic.Pointer[AgentError]
	// text holds the streamed text of each item, for its durable message.
	text map[string]*strings.Builder
	// streamed: the current model response streamed content; idleRetried:
	// a stream already stalled before any content and codex asked again.
	streamed, idleRetried bool
}

type codexTurnEnd struct {
	Status string `json:"status"`
	Error  *struct {
		Message           string          `json:"message"`
		CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
		AdditionalDetails string          `json:"additionalDetails"`
	} `json:"error"`
}

func (p *codexPrompt) fail(code, detail string) bool {
	return p.failure.CompareAndSwap(nil, &AgentError{Code: code, Message: code + ": " + detail})
}

func (c *codexConv) Prompt(ctx context.Context, in BrainInput, sink BrainSink) error {
	c.mu.Lock()
	proc, model, tools := c.proc, c.model, c.tools
	c.mu.Unlock()
	if proc == nil || !proc.alive() {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex exited: " + proc.exitText()}
	}
	p := &codexPrompt{ctx: ctx, sink: sink, tools: brainToolNames(tools), done: make(chan codexTurnEnd, 1), text: map[string]*strings.Builder{}}
	proc.setPrompt(p)
	defer proc.setPrompt(nil)
	c.mu.Lock()
	c.prompt = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.prompt = nil
		c.mu.Unlock()
	}()
	input := []map[string]any{}
	if len(in.Skills) > 0 {
		// Selected skills of this input: a text item of the input, as for
		// every brain that reads skills from its input.
		raw, _ := json.Marshal(in.Skills)
		input = append(input, map[string]any{"type": "text", "text": "Skills explicitly selected for this input only:\n" + string(raw)})
	}
	for _, img := range in.Images {
		input = append(input, map[string]any{"type": "image", "url": "data:" + img.MimeType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)})
	}
	input = append(input, map[string]any{"type": "text", "text": in.Text})
	params := map[string]any{"threadId": c.thread.Thread, "input": input}
	if model == "default" {
		model = ""
	}
	if model != "" {
		params["model"] = model
	}
	// The turn's thinking level, or the model's default: codex keeps a
	// turn's effort for the turns after it unless told otherwise.
	if effort := cmp.Or(in.Effort, c.a.efforts[model]); effort != "" {
		params["effort"] = effort
	}
	for compacted := false; ; compacted = true {
		turn, err := c.startTurn(ctx, proc, p, "turn/start", params)
		if err != nil {
			return err
		}
		end, err := c.wait(ctx, proc, p, turn)
		if err != nil || compacted || !end.overflow() || p.failure.Load() != nil {
			if err != nil {
				return err
			}
			return p.end(end)
		}
		// The conversation does not fit the model's context: codex compacts
		// it (a turn of its own) and the input runs once more, as for every
		// brain; a second overflow ends the turn.
		turn, err = c.startTurn(ctx, proc, p, "thread/compact/start", map[string]any{"threadId": c.thread.Thread})
		if err != nil {
			return err
		}
		if end, err = c.wait(ctx, proc, p, turn); err != nil {
			return err
		}
		if end.Status != "completed" {
			return p.end(end)
		}
	}
}

// startTurn sends a request that starts a turn and returns the turn's id:
// that of the response (turn/start), or of the first turn/started after it
// (thread/compact/start).
func (c *codexConv) startTurn(ctx context.Context, proc *codexProc, p *codexPrompt, method string, params map[string]any) (string, error) {
	p.mu.Lock()
	p.turnID, p.early, p.started = "", nil, make(chan string, 1)
	p.mu.Unlock()
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := proc.call(ctx, method, params, &started); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	turn := started.Turn.ID
	if turn == "" {
		select {
		case turn = <-p.started:
		case <-proc.done:
			return "", &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex exited: " + proc.exitText()}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	p.mu.Lock()
	p.turnID = turn
	early := p.early
	p.mu.Unlock()
	for _, raw := range early {
		proc.onTurnCompleted(p, raw)
	}
	return turn, nil
}

// wait waits for the end of the turn; when ctx ends it interrupts the turn
// (killing codex after CancelGrace) and returns ctx.Err().
func (c *codexConv) wait(ctx context.Context, proc *codexProc, p *codexPrompt, turn string) (codexTurnEnd, error) {
	select {
	case end := <-p.done:
		return end, nil
	case <-proc.done:
		if f := p.failure.Load(); f != nil {
			return codexTurnEnd{}, f
		}
		return codexTurnEnd{}, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex exited: " + proc.exitText()}
	case <-ctx.Done():
	}
	ictx, cancel := context.WithTimeout(context.Background(), c.a.env.CancelGrace)
	defer cancel()
	proc.interrupt(ictx, c.thread.Thread, turn)
	select {
	case <-p.done:
	case <-proc.done:
	case <-ictx.Done():
		c.a.log(LogWarn, "codex did not end a canceled turn within "+c.a.env.CancelGrace.String()+"; killing it", nil)
		proc.stop(c.a.env.KillGrace)
	}
	return codexTurnEnd{}, ctx.Err()
}

// overflow: the turn ended because the conversation does not fit the model's context.
func (e codexTurnEnd) overflow() bool {
	return e.Error != nil && codexError(e.Error.CodexErrorInfo, e.Error.Message+" "+e.Error.AdditionalDetails).Code == AgentErrorCodeContextTooLarge
}

// end maps the end of a turn to its result (contract stop codes).
func (p *codexPrompt) end(end codexTurnEnd) error {
	if f := p.failure.Load(); f != nil {
		return f
	}
	switch end.Status {
	case "completed":
		return nil
	case "interrupted":
		return &AgentError{Code: AgentErrorCodeAgentError, Message: "agent_error: codex interrupted the turn"}
	}
	if end.Error == nil {
		return &AgentError{Code: AgentErrorCodeAgentError, Message: "agent_error: codex ended the turn " + end.Status}
	}
	return codexError(end.Error.CodexErrorInfo, end.Error.Message+" "+end.Error.AdditionalDetails)
}

// codexError maps a codexErrorInfo to the contract's stop codes.
func codexError(info json.RawMessage, message string) *AgentError {
	message = strings.TrimSpace(message)
	var kind string
	if json.Unmarshal(info, &kind) != nil {
		var detail map[string]struct {
			HTTPStatusCode *int `json:"httpStatusCode"`
		}
		_ = json.Unmarshal(info, &detail)
		for _, d := range detail {
			if d.HTTPStatusCode != nil && (*d.HTTPStatusCode == 401 || *d.HTTPStatusCode == 403) {
				kind = "unauthorized"
			}
		}
	}
	switch {
	case strings.Contains(message, "reason: max_output_tokens"):
		// The reply was cut at its output limit (an incomplete response).
		return &AgentError{Code: AgentErrorCodeOutputLimitReached, Message: AgentErrorCodeOutputLimitReached + ": the model reply was cut at its output limit"}
	case strings.Contains(message, `"context_length_exceeded"`):
		// The API's own code, where codex 0.168 reports it as other.
		kind = "contextWindowExceeded"
	case strings.Contains(message, "idle timeout waiting for SSE"):
		return &AgentError{Code: AgentErrorCodeModelStreamIdle, Message: AgentErrorCodeModelStreamIdle + ": " + message}
	}
	switch kind {
	case "contextWindowExceeded":
		return &AgentError{Code: AgentErrorCodeContextTooLarge, Message: AgentErrorCodeContextTooLarge + ": " + message}
	case "unauthorized", "usageLimitExceeded":
		return &AgentError{Code: AgentErrorCodeAgentAuthRequired, Message: AgentErrorCodeAgentAuthRequired + ": codex: " + message}
	}
	return &AgentError{Code: AgentErrorCodeAgentError, Message: AgentErrorCodeAgentError + ": codex: " + message}
}

// --- the app server process ---

type codexProc struct {
	a      *codexAdapter
	cmd    *exec.Cmd
	tree   *processTree
	stdin  io.WriteCloser
	wmu    sync.Mutex
	stderr *tailBuffer
	done   chan struct{}

	mu      sync.Mutex
	nextID  int64
	waiting map[int64]chan codexResponse
	prompt  *codexPrompt
	exitErr error
	// thread is the codex thread the process runs (none for the probe).
	thread string
}

type codexResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *codexRPCError  `json:"error"`
}

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *codexRPCError) Error() string { return fmt.Sprintf("codex: %s (%d)", e.Message, e.Code) }

func (a *codexAdapter) spawn(ctx context.Context) (*codexProc, error) {
	cmd := exec.Command(a.env.Command[0], a.args()...)
	cmd.Env = a.processEnv()
	cmd.Dir = a.env.StateDir
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
	p := &codexProc{a: a, cmd: cmd, stdin: stdin, stderr: &tailBuffer{max: 16 << 10}, done: make(chan struct{}), waiting: map[int64]chan codexResponse{}}
	cmd.Stderr = p.stderr
	tree, err := startCommand(cmd)
	if err != nil {
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "start codex: " + err.Error()}
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
	// experimentalApi: the dynamic tools of thread/start.
	if err := p.call(ictx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "bma-executor-sdk", "version": a.pin.Version},
		"capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		p.stop(a.env.KillGrace)
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex initialize: " + err.Error() + ": " + p.exitText()}
	}
	if err := p.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		p.stop(a.env.KillGrace)
		return nil, &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex initialized: " + err.Error()}
	}
	return p, nil
}

func (p *codexProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *codexProc) exitText() string {
	if p == nil {
		return "not started"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%v: %s", p.exitErr, lastLine(p.stderr.String()))
}

func (p *codexProc) stop(grace time.Duration) {
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

func (p *codexProc) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// call sends a request and decodes its result into out (nil: ignored).
func (p *codexProc) call(ctx context.Context, method string, params any, out any) error {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan codexResponse, 1)
	p.waiting[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
	}()
	if err := p.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "write to codex: " + err.Error()}
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return r.Error
		}
		if out != nil {
			if err := json.Unmarshal(r.Result, out); err != nil {
				return &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: "agent_protocol_error: codex " + method + " result: " + err.Error()}
			}
		}
		return nil
	case <-p.done:
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "codex exited: " + p.exitText()}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *codexProc) setPrompt(prompt *codexPrompt) {
	p.mu.Lock()
	p.prompt = prompt
	p.mu.Unlock()
}

func (p *codexProc) setThread(thread string) {
	p.mu.Lock()
	p.thread = thread
	p.mu.Unlock()
}

func (p *codexProc) current() *codexPrompt {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompt
}

func (p *codexProc) interrupt(ctx context.Context, thread, turn string) {
	if turn == "" {
		return
	}
	if err := p.call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, nil); err != nil {
		p.a.log(LogDebug, "codex turn/interrupt", err)
	}
}

// failTurn ends the running turn at a stop the executor's semantics have.
func (p *codexProc) failTurn(prompt *codexPrompt, code, detail string) {
	if !prompt.fail(code, detail) {
		return
	}
	prompt.mu.Lock()
	turn := prompt.turnID
	prompt.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.a.env.CancelGrace)
		defer cancel()
		p.mu.Lock()
		thread := p.thread
		p.mu.Unlock()
		p.interrupt(ctx, thread, turn)
	}()
}

func (p *codexProc) readLoop(r io.Reader) {
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

// codexMessage is one stdout line of the app server.
type codexMessage struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *codexRPCError   `json:"error"`
}

func (p *codexProc) dispatch(line []byte) {
	p.a.log(LogDebug, fmt.Sprintf("codex stdout: %.2000s", line), nil)
	var m codexMessage
	if json.Unmarshal(line, &m) != nil {
		p.a.log(LogWarn, "codex wrote a non-JSON line on stdout", nil)
		return
	}
	switch {
	case m.ID != nil && m.Method == "":
		var id int64
		if json.Unmarshal(*m.ID, &id) == nil {
			p.mu.Lock()
			ch := p.waiting[id]
			p.mu.Unlock()
			if ch != nil {
				ch <- codexResponse{Result: m.Result, Error: m.Error}
			}
		}
	case m.ID != nil:
		go p.onRequest(*m.ID, m.Method, m.Params)
	default:
		p.onNotification(m.Method, m.Params)
	}
}

func (p *codexProc) onNotification(method string, raw json.RawMessage) {
	prompt := p.current()
	if prompt == nil {
		return
	}
	switch method {
	case "turn/completed":
		p.onTurnCompleted(prompt, raw)
		return
	case "turn/started":
		var n struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(raw, &n)
		prompt.mu.Lock()
		if prompt.turnID == "" && prompt.started != nil {
			select {
			case prompt.started <- n.Turn.ID:
			default:
			}
		}
		prompt.mu.Unlock()
		return
	}
	var n struct {
		TurnID  string          `json:"turnId"`
		ItemID  string          `json:"itemId"`
		Delta   string          `json:"delta"`
		Item    json.RawMessage `json:"item"`
		Error   json.RawMessage `json:"error"`
		Retry   bool            `json:"willRetry"`
		Summary int             `json:"summaryIndex"`
	}
	if json.Unmarshal(raw, &n) != nil {
		return
	}
	prompt.mu.Lock()
	ours := prompt.turnID == "" || n.TurnID == "" || n.TurnID == prompt.turnID
	prompt.mu.Unlock()
	if !ours || (prompt.failure.Load() != nil) {
		return // another turn's, or the turn ended at a stop
	}
	switch method {
	case "item/agentMessage/delta":
		prompt.delta(BrainKindMessage, n.ItemID, n.Delta)
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		prompt.delta(BrainKindThinking, n.ItemID, n.Delta)
	case "item/started":
		var item struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(n.Item, &item)
		if !slices.Contains(codexItemTypes, item.Type) {
			p.failTurn(prompt, AgentErrorCodeAgentProtocolError, "tool_catalog_violation: codex started a built-in "+item.Type+", not a platform tool")
		}
	case "item/completed":
		p.onItemCompleted(prompt, n.Item)
	case "error":
		// Codex retries a request the API refused; the executor's semantics
		// have no retry for credentials the API refuses.
		var e struct {
			CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			Message        string          `json:"message"`
			Details        string          `json:"additionalDetails"`
		}
		_ = json.Unmarshal(n.Error, &e)
		if n.Retry && strings.Contains(e.Message+" "+e.Details, "idle timeout waiting for SSE") {
			// A stream that stalls after content ends the turn; one that
			// stalls before any is asked once more (codex's retry).
			prompt.mu.Lock()
			again := prompt.streamed || prompt.idleRetried
			prompt.streamed, prompt.idleRetried = false, true
			prompt.mu.Unlock()
			if again {
				p.failTurn(prompt, AgentErrorCodeModelStreamIdle, "the model stream stalled: "+e.Details)
			}
			return
		}
		if n.Retry {
			if err := codexError(e.CodexErrorInfo, e.Message+" "+e.Details); err.Code == AgentErrorCodeAgentAuthRequired {
				p.failTurn(prompt, err.Code, strings.TrimPrefix(err.Message, err.Code+": "))
			}
		}
	}
}

func (p *codexPrompt) delta(kind, itemID, text string) {
	if text == "" {
		return
	}
	p.mu.Lock()
	b := p.text[kind+itemID]
	if b == nil {
		b = &strings.Builder{}
		p.text[kind+itemID] = b
	}
	b.WriteString(text)
	p.streamed = true
	p.mu.Unlock()
	p.sink.Delta(kind, itemID, text)
}

func (p *codexProc) onItemCompleted(prompt *codexPrompt, raw json.RawMessage) {
	var item struct {
		Type    string   `json:"type"`
		ID      string   `json:"id"`
		Text    string   `json:"text"`
		Summary []string `json:"summary"`
		Content []string `json:"content"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return
	}
	switch item.Type {
	case "agentMessage":
		if strings.TrimSpace(item.Text) != "" {
			prompt.sink.Message(BrainKindMessage, item.ID, item.Text)
		}
	case "reasoning":
		text := strings.Join(item.Summary, "\n\n")
		if text == "" {
			text = strings.Join(item.Content, "\n\n")
		}
		if text == "" {
			prompt.mu.Lock()
			if b := prompt.text[BrainKindThinking+item.ID]; b != nil {
				text = b.String()
			}
			prompt.mu.Unlock()
		}
		if strings.TrimSpace(text) != "" {
			prompt.sink.Message(BrainKindThinking, item.ID, text)
		}
	}
}

func (p *codexProc) onTurnCompleted(prompt *codexPrompt, raw json.RawMessage) {
	var n struct {
		Turn struct {
			ID string `json:"id"`
			codexTurnEnd
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &n) != nil {
		return
	}
	prompt.mu.Lock()
	switch prompt.turnID {
	case "":
		prompt.early = append(prompt.early, raw)
		prompt.mu.Unlock()
		return
	case n.Turn.ID:
	default:
		prompt.mu.Unlock()
		return
	}
	prompt.mu.Unlock()
	select {
	case prompt.done <- n.Turn.codexTurnEnd:
	default:
	}
}

// onRequest answers the app server's requests: a dynamic tool call goes to
// the host; anything else is a built-in's and ends the turn.
func (p *codexProc) onRequest(id json.RawMessage, method string, raw json.RawMessage) {
	reply := func(result any) {
		_ = p.write(map[string]any{"id": id, "result": result})
	}
	prompt := p.current()
	if method != "item/tool/call" {
		_ = p.write(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "not supported by this client: " + method}})
		if prompt != nil {
			p.failTurn(prompt, AgentErrorCodeAgentProtocolError, "tool_catalog_violation: codex asked "+method+", which only a built-in tool asks")
		}
		return
	}
	var r struct {
		TurnID    string         `json:"turnId"`
		CallID    string         `json:"callId"`
		Tool      string         `json:"tool"`
		Namespace *string        `json:"namespace"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(raw, &r)
	text := func(s string) []map[string]any { return []map[string]any{{"type": "inputText", "text": s}} }
	switch {
	case prompt == nil || prompt.failure.Load() != nil:
		reply(map[string]any{"contentItems": text("No turn of this session is running."), "success": false})
		return
	case r.Namespace != nil || !prompt.tools[r.Tool]:
		p.a.log(LogWarn, fmt.Sprintf("codex called %q, which is not a tool of this turn: refused", r.Tool), nil)
		reply(map[string]any{"contentItems": text("The tool " + r.Tool + " is not available."), "success": false})
		return
	}
	ctx, cancel := context.WithCancel(prompt.ctx)
	defer cancel()
	call := BrainCall{ID: r.CallID, Name: r.Tool, Input: r.Arguments}
	d := prompt.sink.Permission(ctx, call)
	var result *RouteResult
	switch d.Outcome {
	case BrainAllow:
		result = prompt.sink.Call(ctx, call)
	case BrainDeny:
		result = &RouteResult{Text: cmp.Or(d.Message, refusalText), IsError: true}
	default:
		result = &RouteResult{Text: "The request was withdrawn before it was answered.", IsError: true}
	}
	prompt.sink.ToolResult(call.ID, []ContentBlock{{Type: ContentBlockTypeText, Text: result.Text}}, result.IsError)
	items := text(result.Text)
	for _, img := range result.Images {
		items = append(items, map[string]any{"type": "inputImage", "imageUrl": "data:" + img.MimeType + ";base64," + img.Data})
	}
	reply(map[string]any{"contentItems": items, "success": !result.IsError})
}

// codexToolSpecs are the dynamic tools of a thread: the six platform tools
// and the turn's tools, under the platform's names.
func codexToolSpecs(tools []BrainTool) []map[string]any {
	var specs []map[string]any
	for _, t := range claudeToolList() {
		m := t.(map[string]any)
		specs = append(specs, map[string]any{"type": "function", "name": m["name"], "description": m["description"], "inputSchema": m["inputSchema"]})
	}
	for _, t := range tools {
		specs = append(specs, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "inputSchema": t.Parameters})
	}
	return specs
}
