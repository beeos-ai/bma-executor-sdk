package bmaexecutor

// BrainHost is an AgentHandler that hosts a local brain through a
// BrainAdapter (brain_adapter.go; ADR-0066 of BMA). It is the executor's one
// implementation of the platform semantics of a local brain; the adapter only
// drives the brain process. The executor stays the machine's only connection
// to BMA.
//
//	platform (executor protocol)          BrainHost                        BrainAdapter
//	turn.start                            validate, record the plan        (RunTurn: Open)
//	session.event_delta                   <-                               Delta
//	agent.message / agent.thinking        <-                               Message
//	agent.tool_use                        <-                               ToolUse
//	interaction.request (appr_ id)        <->                              Permission
//	(tool runs through the turn's route)  <->                              Call
//	agent.tool_result                     <-                               ToolResult
//	turn.cancel                           ->                               Prompt's ctx ends
//	idle failed + session.error           <-                               Prompt's error (stop codes)
//
// TurnResume: when the executor process restarts, the platform starts the
// in-flight turn again and the host continues it from the brain's durable
// conversation and its own ledger of tool calls, approvals and acknowledged
// events (agent_acp_resume.go); nothing that already happened is repeated.
// An input the brain had begun is not handed again: the host settles its
// tool calls the platform has no result of (under their own agent.tool_use
// events and approval ids: an answered approval is not asked again, a call
// that ran is not run again) and hands the brain a recovery note instead,
// the same for every brain.
//
// Tools run in the executor's Core, in the turn's working directory, so the
// hands of a local brain are the same as for a platform brain; or on the
// platform's gateway when the turn carries tools_mcp_* (ADR-0029). The brain
// process runs as the Core's RunAs identity, like the tools.
//
// NewACPAgent builds a BrainHost with the adapter its Profile names
// (agent_acp_profiles.go). The host's own source never names a brain.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultACPCancelGrace is how long a canceled turn waits for the brain
	// to end the input before its process is killed.
	DefaultACPCancelGrace = 10 * time.Second
	// DefaultACPKillGrace is the wait between SIGTERM and SIGKILL of the brain's process group.
	DefaultACPKillGrace = 3 * time.Second
	// DefaultACPMaxSessions bounds the running turns of one host.
	DefaultACPMaxSessions = 8
	// DefaultACPResidentSessions is the share of the Core's MaxSessions (in
	// percent) that resident sessions may use by default; the rest stays for
	// sessions assigned to the Core as hands.
	DefaultACPResidentSessions = 75

	acpBrain   = "acp"
	acpMetaKey = "bma"

	// PlatformCredentialHarnessModel is the agent capability's
	// platform_credentials entry for the per-turn model endpoint and token
	// (turn.start metadata model_base_url, model_token, model_token_expires_at).
	PlatformCredentialHarnessModel = "harness_model"
	// PlatformCredentialBrainTools is the platform_credentials entry for the
	// per-turn tools endpoint and token (turn.start metadata tools_mcp_url,
	// tools_mcp_token, tools_mcp_token_expires_at), contract 1.4.0.
	PlatformCredentialBrainTools = "brain_tools"

	// acpModelTokenRefreshWindow: a platform model token with at most this
	// much time left is replaced through model_token.refresh before the
	// request that needs it is forwarded.
	acpModelTokenRefreshWindow  = 10 * time.Minute
	acpModelTokenRefreshTimeout = 10 * time.Second
	// A failed refresh is tried again by a later request after a backoff
	// doubling from acpModelTokenRetryBase to acpModelTokenRetryMax.
	acpModelTokenRetryBase = 5 * time.Second
	acpModelTokenRetryMax  = time.Minute
)

// Model credential kinds (BrainCapabilities.ModelCredentials).
const (
	// BrainModelPlatform: the brain's model requests go through the host's
	// route (the platform's per-turn credential, or the node owner's Upstream).
	BrainModelPlatform = "platform"
	// BrainModelOwner: the brain uses only its node owner's own credentials;
	// the host never hands it the platform's (no harness_model).
	BrainModelOwner = "owner"
)

// DefaultACPSystemPrompt is the base system prompt of a local brain; the
// agent definition's system instructions follow it.
const DefaultACPSystemPrompt = "You are an agent running on the user's computer through BMA (BeeOS Managed Agents).\n" +
	"Your tools run on this computer, in the working directory %s; use paths under it.\n" +
	"Be concise. When a task is done, say what you did."

// acpPermissionModes are the platform permission modes a host takes.
var acpPermissionModes = []string{"default", "accept_edits"}

// acpReasoningEfforts are the thinking levels a host may declare for a model
// (AgentModel.ThinkingLevels): the reasoning effort values of the model APIs,
// lowest first.
var acpReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ACPModelUpstream is where model requests go when turn.start carries no
// platform model endpoint: the node owner's own OpenAI-compatible endpoint.
type ACPModelUpstream struct {
	// BaseURL ends in /v1; requests go to BaseURL + "/chat/completions".
	BaseURL string
	// Model is the default model of a turn (ACPAgentOptions.Model defaults
	// to it). A request goes upstream with the turn's model: this one, or the
	// one of ACPAgentOptions.Models the turn chose.
	Model string
	// Header adds the credential (for example Authorization) to a forwarded request.
	Header func(*http.Request)
	// SessionHeader, when set, names a request header that every request
	// forwarded to this endpoint carries, with the platform session id of the
	// turn it belongs to (turn.start session_id) as its value, so the endpoint
	// can account a request to its session. It is set before Header runs. The
	// value never comes from the brain: a forwarded request is built anew and
	// takes no such header from the brain's request. Requests that go to the
	// platform's own model endpoint (turn.start metadata model_base_url) do
	// not carry it.
	SessionHeader string
}

// ACPAgentOptions configures NewACPAgent and NewBrainHost.
type ACPAgentOptions struct {
	// Profile selects the hosted brain of NewACPAgent: ACPProfileDSH (default)
	// or ACPProfileClaudeCode (agent_acp_profiles.go). NewBrainHost ignores it.
	Profile string
	// Command is the brain's argv, for example {"/opt/bma/bma-dsh-acp"} or
	// {"/opt/bma/node", "/opt/bma/bma-dsh-acp.mjs"}. Command[0] must be an
	// absolute path: the brain is never looked up on PATH (a user's own one
	// there must not be started by mistake).
	Command []string
	// Env adds KEY=VALUE entries to the brain's environment (PATH, HOME, USER, LANG and TMPDIR come from the executor).
	Env []string
	// StateDir holds the brain's state.
	StateDir string
	// Core runs the tools.
	Core *Core
	// AllowedRoots bound a turn's cwd (default: Core's work root). A turn
	// without cwd works in Core's per-session workspace.
	AllowedRoots []string
	// Upstream is the model endpoint used when the platform hands none, for
	// a brain whose model requests the host forwards. Required for such a
	// brain unless PlatformModel.
	Upstream ACPModelUpstream
	// PlatformModel asks the platform for the per-turn model credential
	// (agent capability platform_credentials: harness_model), as a sandbox
	// machine of the platform does. Without Upstream a turn the platform
	// hands no credential fails (session.error); it is never sent elsewhere.
	// Refused for a brain that uses only its owner's credentials.
	PlatformModel bool
	// PlatformTools declares platform_credentials brain_tools: this machine
	// may be the brain of a session whose hands are on another machine
	// (ADR-0029). The platform then hands the turn its tools endpoint and token
	// (turn.start metadata tools_mcp_*) and every tool call of that turn goes
	// there, never to this machine's Core (fail closed). A turn without
	// tools_mcp_* runs its tools here.
	PlatformTools bool
	// AskUserQuestion offers the brain the ask_user_question tool (see
	// agent_acp_host_tools.go): a question is a custom tool call of the session
	// (capability custom_tools), so turn it on only when the session's clients
	// answer custom tool calls (user.custom_tool_result); a client that does not
	// leaves the session waiting.
	AskUserQuestion bool
	// ToolPolicy is the host's ceiling on the permission of tools (tool name
	// to allow, ask or deny): the permission of a tool in a turn is the
	// stricter of this value and what the platform's turn.start
	// metadata.tool_policy and the permission mode give (deny over ask over
	// allow), so the platform and the mode can only tighten it, never loosen
	// it. A tool it does not name is not capped. A host sets deny on tools it
	// cannot yet check (fail closed). Applies to the six tools and to host
	// tools, and is enforced by the executor's gate whatever the brain does.
	// Other values are rejected. Nil caps nothing.
	ToolPolicy map[string]string
	// SessionFilesRoot is the executor's capabilities.session_files.root: the
	// directory below which the platform mounts each session's files as
	// <root>/<session id>/{uploads,skills,outputs} (ADR-0021). When set, the
	// file tools of a turn whose tools run on this machine reach, besides the
	// workspace, exactly these three directories of that session: read,
	// glob and grep in uploads and skills (read-only) and in outputs, and
	// write and edit in outputs (deliverables the platform collects). Another
	// session's directories, a path below the root that is none of the three,
	// a "..", and a symbolic link that leaves a directory stay refused;
	// write and edit anywhere else stay confined to the workspace. The session
	// id must be a single safe directory name. Absolute, or empty for
	// the workspace only.
	SessionFilesRoot string
	// InputFiles fetches the bytes of an image the user attached (a file
	// block of turn.start / turn.input; see agent_acp_images.go) when the
	// platform hands the turn no tools endpoint to download it from. With it
	// (or PlatformTools) and a brain that takes images the agent capability
	// declares file_inputs and the harness image_inputs, and the platform
	// then sends image input. Without both, the platform is told the brain
	// takes no image and refuses an image input.
	InputFiles ACPInputFiles
	// HarnessID is the declared harness id (default: the brain's own id).
	HarnessID string
	// Model is the model name the brain uses when the turn config has none (default Upstream.Model).
	Model string
	// Models are the models the host lets a turn choose (agent capability
	// models), for example the models the node owner's account may use: a
	// turn.start config.model must be one of their ids or the turn is refused
	// (-32602, never another model). A model's ThinkingLevels are the
	// reasoning effort values (acpReasoningEfforts: none, minimal, low,
	// medium, high, xhigh, max) a turn may set with config.thinking_level, or
	// DefaultThinkingLevel when it sets none; levels the brain does not take
	// are not declared, so a turn naming one is refused rather than run
	// without it. A model without levels ignores config.thinking_level.
	// Empty: config.model and config.thinking_level are ignored (contract)
	// and every turn runs on Model. Refused for a brain that takes no
	// per-turn model (BrainCapabilities.ModelChoice).
	Models []AgentModel
	// SystemPrompt is the base system prompt, with one %s for the working directory (default DefaultACPSystemPrompt).
	SystemPrompt string
	// MaxSessions bounds the turns running at once (default
	// DefaultACPMaxSessions): a turn.start past it answers executor_busy, and an
	// idle session holds no slot.
	MaxSessions int64
	// MaxResidentSessions bounds the sessions whose brain conversation and Core
	// session are held, running or idle (default DefaultACPResidentSessions of the
	// Core's capacity, at least MaxSessions; it must not exceed the Core's
	// MaxSessions). Past it the least recently used idle session is suspended
	// (BrainSession.Suspend; the brain keeps its conversation) and loaded again
	// by its next turn (with the executor_session_ref). A session with a bash
	// command left running is not suspended.
	MaxResidentSessions int
	CancelGrace         time.Duration
	KillGrace           time.Duration
	OnLog               func(LogEvent)
	// OnInteraction, when set, is told when an approval is asked of the
	// platform and when it ends (answered on any client, expired or
	// withdrawn), so a local UI can show the same card under the same id and
	// drop it when it is settled elsewhere. It is a view only: the answer
	// still comes from the platform. It must not block.
	OnInteraction func(ACPInteractionEvent)
}

// ACPInteractionEvent is one step of a platform interaction asked by the local brain.
type ACPInteractionEvent struct {
	// Phase is "requested" or "ended".
	Phase string
	// Kind is InteractionKindPermission (an approval; InteractionID is the
	// platform interaction) or InteractionKindQuestion (ask_user_question;
	// InteractionID is the agent.custom_tool_use event id the session waits on).
	Kind          string
	SessionID     string
	InteractionID string
	ToolName      string
	// ToolInput is the tool call's input (Phase "requested" only).
	ToolInput map[string]any
	// Outcome is the platform answer when Phase is "ended" (allow, deny,
	// expired, canceled), or "withdrawn" when the turn ended first.
	Outcome string
}

// BrainHost hosts a local brain through a BrainAdapter; see the comment at
// the top of brain_host.go.
type BrainHost struct {
	opts    ACPAgentOptions
	adapter BrainAdapter
	caps    BrainCapabilities
	// models is the capability's models (see ACPAgentOptions.Models).
	models  []AgentModel
	harness AgentHarnessCapability

	mu       sync.Mutex
	sessions map[string]*acpSession
	// suspended maps the sessions SuspendSession released to their
	// executor_session_ref, for a platform that sent none.
	suspended map[string]string
	closed    bool

	// hook, set by tests only, runs at named points of tool execution; it
	// may block until ctx ends to stand in for a crash there.
	hook func(ctx context.Context, point string)
}

// ACPAgent is the BrainHost NewACPAgent returns.
type ACPAgent = BrainHost

type acpSession struct {
	id  string
	cwd string
	// brain is the brain's conversation, nil until the first open.
	brain BrainSession
	// mode is the platform permission mode (default, accept_edits).
	mode string
	turn *acpTurn

	// plan is what the latest turn.start asked for; opening is the open of the
	// brain's conversation in flight (see openSession).
	plan    acpOpenPlan
	opening *acpOpening
}

// acpRoute is where a turn's model requests go: the platform's credential
// (platform set), or Upstream.
type acpRoute struct {
	platform *acpPlatformModel
	baseURL  string
	model    string
	header   func(*http.Request)
	// sessionHeader is Upstream.SessionHeader; never set with platform.
	sessionHeader string
}

// acpPlatformModel is the platform's model credential of one turn (turn.start
// metadata model_base_url, model_token, model_token_expires_at). It lives in
// memory only.
type acpPlatformModel struct {
	agent *BrainHost
	turn  *AgentTurn
	// mu serializes refreshes, so concurrent requests share one.
	mu        sync.Mutex
	base      string
	token     string
	expiresAt time.Time // zero: the platform did not say; never refreshed
	failures  int
	retryAt   time.Time
}

// credential is the base URL and token to forward with, refreshed first
// when at most acpModelTokenRefreshWindow is left. A failed refresh keeps
// the current token (the gateway answers 401 once it expires, never a silent
// substitute) and is tried again by a later request after a backoff.
func (m *acpPlatformModel) credential(ctx context.Context) (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.expiresAt.IsZero() || m.expiresAt.Sub(now) > acpModelTokenRefreshWindow || now.Before(m.retryAt) {
		return m.base, m.token
	}
	ctx, cancel := context.WithTimeout(ctx, acpModelTokenRefreshTimeout)
	defer cancel()
	result, err := m.turn.RefreshModelToken(ctx)
	var expiresAt time.Time
	if err == nil {
		if expiresAt, err = time.Parse(time.RFC3339, result.ExpiresAt); err != nil || result.ModelToken == "" {
			err = fmt.Errorf("model_token.refresh returned an unusable credential (token empty=%t, expires_at %q)", result.ModelToken == "", result.ExpiresAt)
		}
	}
	if err != nil {
		m.failures++
		m.retryAt = now.Add(min(acpModelTokenRetryBase<<(m.failures-1), acpModelTokenRetryMax))
		m.agent.log(LogError, fmt.Sprintf("model token refresh failed for session %s turn %s (attempt %d, expires %s); forwarding with the current token",
			m.turn.SessionID(), m.turn.TurnID(), m.failures, m.expiresAt.Format(time.RFC3339)), err)
		return m.base, m.token
	}
	m.token, m.expiresAt, m.failures, m.retryAt = result.ModelToken, expiresAt, 0, time.Time{}
	if result.ModelBaseURL != "" {
		m.base = strings.TrimRight(result.ModelBaseURL, "/")
	}
	m.agent.log(LogInfo, fmt.Sprintf("model token refreshed for session %s turn %s (expires %s)", m.turn.SessionID(), m.turn.TurnID(), expiresAt.Format(time.RFC3339)), nil)
	return m.base, m.token
}

type acpTurn struct {
	agent  *BrainHost
	turn   *AgentTurn
	ctx    context.Context
	ledger *acpLedger
	route  acpRoute
	turnID string
	// toolRoute runs the brain's tool calls: the platform's gateway when the
	// turn carries tools_mcp_* (the hands are elsewhere), otherwise local.
	toolRoute ToolRoute
	// local is this machine's route (the hands are here), nil for a turn with
	// remote hands. It decides the fate of a bash command left in the background.
	local *LocalToolRoute
	// policy is the effective permission of each tool (see acpEffectivePolicy);
	// the executor enforces it, whatever the brain does.
	policy map[string]string
	// hostAsk are the host tools whose ask is only their declared default:
	// neither the turn's tool_policy nor the host's ceiling names them.
	hostAsk map[string]bool
	// custom are the turn's custom tools (agent.custom_tool_use, never approved).
	custom map[string]bool
	// hostAllowed is the ComputerUseApprover's answer per call id, asked
	// once so the tool_use event and the permission request agree.
	hostAllowed map[string]bool
	// effort is the turn's thinking level ("" for none).
	effort string
	// input is the input the brain is working on.
	input string

	mu       sync.Mutex
	messages map[string]string // kind + messageId -> event id
	index    map[string]int64  // event id -> next delta index
	tools    map[string]string // call id -> agent.tool_use event id
	// questions maps an ask_user_question call id to its agent.custom_tool_use event id.
	questions map[string]string
}

// NewBrainHost starts the brain through adapter (BrainAdapter.Start) and
// returns its host. Serve it with client.ServeAgent(host, host.Capability()).
func NewBrainHost(ctx context.Context, adapter BrainAdapter, opts ACPAgentOptions) (*BrainHost, error) {
	if len(opts.Command) == 0 || opts.Core == nil || opts.StateDir == "" {
		return nil, errors.New("bmaexecutor: ACPAgentOptions.Command, Core and StateDir are required")
	}
	if !filepath.IsAbs(opts.Command[0]) {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Command[0] must be an absolute path, got %q (no PATH lookup)", opts.Command[0])
	}
	if name := opts.Upstream.SessionHeader; name != "" && !acpHeaderName(name) {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Upstream.SessionHeader %q is not an HTTP header name", name)
	}
	for tool, permission := range opts.ToolPolicy {
		if tool == "" || (permission != "allow" && permission != "ask" && permission != "deny") {
			return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.ToolPolicy[%q] = %q: the tool must be named and the value allow, ask or deny", tool, permission)
		}
	}
	opts.ToolPolicy = maps.Clone(opts.ToolPolicy)
	models, err := acpCheckModels(opts.Models)
	if err != nil {
		return nil, err
	}
	opts.Models = models
	if opts.SessionFilesRoot != "" && !filepath.IsAbs(opts.SessionFilesRoot) {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.SessionFilesRoot must be an absolute path, got %q", opts.SessionFilesRoot)
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
	if coreMax := opts.Core.opts.MaxSessions; opts.MaxResidentSessions > coreMax {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.MaxResidentSessions %d exceeds the Core's MaxSessions %d", opts.MaxResidentSessions, coreMax)
	} else if opts.MaxResidentSessions <= 0 {
		opts.MaxResidentSessions = coreMax * DefaultACPResidentSessions / 100
	}
	opts.MaxResidentSessions = max(opts.MaxResidentSessions, int(opts.MaxSessions))
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
	if runAs := opts.Core.opts.RunAs; runAs != nil {
		// The brain runs as the Core's workload identity and keeps its conversations here.
		if err := os.Chown(opts.StateDir, int(runAs.UID), int(runAs.GID)); err != nil {
			return nil, err
		}
	}
	a := &BrainHost{opts: opts, adapter: adapter, sessions: map[string]*acpSession{}, suspended: map[string]string{}}
	caps, err := adapter.Start(ctx, BrainEnv{
		Command: opts.Command, Env: opts.Env, StateDir: opts.StateDir, RunAs: opts.Core.opts.RunAs,
		PlatformModel: opts.PlatformModel, Upstream: opts.Upstream, Models: opts.Models,
		CancelGrace: opts.CancelGrace, KillGrace: opts.KillGrace,
		HarnessID: opts.HarnessID, Log: a.log,
	})
	if err != nil {
		return nil, err
	}
	a.caps = caps
	a.harness = caps.Harness
	a.harness.ImageInputs = caps.ImageInputs
	if caps.ModelCredentials == BrainModelOwner {
		// The platform checks a session's model against these and hands it
		// on unchanged (contract 1.10.0).
		a.harness.ModelCredentials, a.harness.Models = BrainModelOwner, caps.Models
	}
	a.models = caps.Models
	if a.models == nil {
		a.models = acpAgentModels(opts.Models, caps.ThinkingLevels)
		if len(opts.Models) > 0 && len(caps.ThinkingLevels) == 0 && slices.ContainsFunc(opts.Models, func(m AgentModel) bool { return len(m.ThinkingLevels) > 0 }) {
			a.log(LogWarn, "the brain takes no reasoning effort; the models are declared without thinking levels", nil)
		}
	}
	return a, nil
}

// Capability is the agent capability to serve: one harness, in harnesses[]
// and in the deprecated harness member.
func (a *BrainHost) Capability() AgentCapability {
	h := a.harness
	c := AgentCapability{
		Enabled: true, PermissionModes: acpPermissionModes, InteractionKinds: []string{InteractionKindPermission},
		// Custom tools and ask_user_question are custom tool calls of the session (agent_acp_host_tools.go).
		CustomTools: a.caps.CustomTools,
		// Host tools come with tools_mcp_* (the hands are elsewhere).
		HostTools:   a.caps.HostTools && a.opts.PlatformTools,
		InputSkills: a.caps.InputSkills,
		// The host continues a turn an executor restart interrupted (recover).
		TurnResume:  true,
		FileInputs:  a.fileInputs(h.ImageInputs),
		MaxSessions: a.opts.MaxSessions, Harness: &h, Harnesses: []AgentHarnessCapability{a.harness},
		Models: a.models,
	}
	if a.opts.PlatformTools {
		c.PlatformCredentials = append(c.PlatformCredentials, PlatformCredentialBrainTools)
	}
	if a.opts.PlatformModel {
		c.PlatformCredentials = append(c.PlatformCredentials, PlatformCredentialHarnessModel)
	}
	return c
}

// Close stops the brain.
func (a *BrainHost) Close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	err := a.adapter.Close()
	a.opts.Core.local.background.killAll()
	return err
}

func (a *BrainHost) interaction(e ACPInteractionEvent) {
	if a.opts.OnInteraction != nil {
		a.opts.OnInteraction(e)
	}
}

func (a *BrainHost) log(level LogLevel, message string, err error) {
	if a.opts.OnLog != nil {
		a.opts.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// --- AgentHandler ---

func (a *BrainHost) OpenTurn(ctx context.Context, params TurnStartParams) (TurnStartResult, error) {
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
	// With tools_mcp_* the hands are elsewhere: config.Cwd is a path on that
	// machine, not checked here, and the brain works in a scratch workspace.
	remote := handsAreRemote(params.Metadata)
	requestedCwd, handsCwd := config.Cwd, ""
	if remote {
		requestedCwd, handsCwd = "", config.Cwd
		if handsCwd == "" && config.EnvironmentInfo != nil {
			handsCwd = config.EnvironmentInfo.Cwd
		}
	}
	cwd, err := a.turnCwd(params.SessionID, requestedCwd)
	if err != nil {
		return TurnStartResult{}, err
	}
	promptCwd := cwd
	if remote {
		promptCwd = handsCwd
	}
	model, level, err := a.turnModel(config)
	if err != nil {
		return TurnStartResult{}, err
	}
	tools, err := a.turnTools(config, remote)
	if err != nil {
		return TurnStartResult{}, err
	}
	receipt, err := inputSkillReceipt(params.InputID, config.InputSkills)
	if err != nil {
		return TurnStartResult{}, err
	}
	// Opening the brain's conversation is not done here: turn.start is
	// answered as soon as the turn is accepted (contract) and the open runs
	// first thing in RunTurn, where a failure ends the turn with session.error
	// and a cancel ends it canceled. Everything that can be refused cheaply and
	// for certain is refused above.
	s := a.session(params.SessionID)
	a.mu.Lock()
	if ref, ok := a.suspended[params.SessionID]; ok {
		// Suspended while idle: load its conversation again, by the ref the
		// brain knows it by when the platform did not send one.
		delete(a.suspended, params.SessionID)
		if params.ExecutorSessionRef == "" {
			params.ExecutorSessionRef = ref
		}
	}
	resumed := params.ExecutorSessionRef != "" || (s.brain != nil && s.brain.Ready())
	ref := cmp.Or(a.adapter.Ref(params.SessionID), params.ExecutorSessionRef)
	if s.brain != nil {
		ref = s.brain.Ref()
	}
	system := config.System
	mountNote := a.sessionFilesNote(params.SessionID, remote)
	if mountNote != "" {
		system = strings.TrimSpace(system + "\n\n" + mountNote)
	}
	s.plan = acpOpenPlan{params: params, cwd: cwd, promptCwd: promptCwd, model: model, effort: level, mode: mode, system: system, hands: config.EnvironmentInfo,
		tools: tools, skills: config.InputSkills}
	a.mu.Unlock()
	if err := a.opts.Core.openLocal(acpBrain, params.SessionID, cwd); err != nil {
		return TurnStartResult{}, err
	}
	if mountNote != "" {
		if err := a.opts.Core.setLocalMounts(params.SessionID, a.opts.SessionFilesRoot); err != nil {
			return TurnStartResult{}, &RPCError{Code: CodeInternalError, Message: "session files: " + err.Error()}
		}
	}
	a.mu.Lock()
	s.cwd = cwd
	a.mu.Unlock()
	ledger, err := a.ledger(params.SessionID)
	if err == nil {
		_, err = ledger.begin(params.TurnID)
	}
	if err != nil {
		return TurnStartResult{}, &RPCError{Code: CodeInternalError, Message: "turn state: " + err.Error()}
	}
	return TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: ref, Resumed: resumed,
		Effective: AgentTurnConfig{Model: model, ThinkingLevel: level, PermissionMode: mode, Cwd: promptCwd, Harness: a.harness.ID, InputSkillReceipt: receipt},
	}, nil
}

func (a *BrainHost) RunTurn(ctx context.Context, turn *AgentTurn) error {
	params := turn.Params()
	a.mu.Lock()
	s := a.sessions[params.SessionID]
	a.mu.Unlock()
	if s == nil {
		return errors.New("the agent session is not open")
	}
	if err := a.openSession(ctx, s); err != nil {
		return err
	}
	ledger, err := a.ledger(params.SessionID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	plan := s.plan
	brain := s.brain
	a.mu.Unlock()
	route := a.route(turn, plan.model)
	remoteRoute, err := a.remoteToolRoute(turn)
	if err != nil {
		return err
	}
	var overrides map[string]string
	if raw := params.Metadata["tool_policy"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
			return fmt.Errorf("turn.start metadata.tool_policy must be a JSON object of tool: allow|ask|deny")
		}
		for tool, permission := range overrides {
			if permission != "allow" && permission != "ask" && permission != "deny" {
				return fmt.Errorf("turn.start metadata.tool_policy.%s must be allow, ask or deny", tool)
			}
		}
	}
	a.mu.Lock()
	mode := s.mode
	a.mu.Unlock()
	effective := acpEffectivePolicy(mode, overrides)
	hostAsk := map[string]bool{}
	custom := map[string]bool{}
	for _, tool := range plan.tools {
		_, named := effective[tool.Name]
		_, capped := a.opts.ToolPolicy[tool.Name]
		hostAsk[tool.Name] = tool.Permission == "ask" && !named && !capped
		custom[tool.Name] = tool.Custom
	}
	policy := acpCapPolicy(withTurnTools(effective, plan.tools), a.opts.ToolPolicy)
	a.mu.Lock()
	t := &acpTurn{agent: a, turn: turn, ctx: ctx, ledger: ledger, route: route, turnID: params.TurnID, policy: policy, hostAsk: hostAsk, custom: custom, hostAllowed: map[string]bool{}, effort: plan.effort,
		messages: map[string]string{}, index: map[string]int64{}, tools: map[string]string{}, questions: map[string]string{}}
	if remoteRoute != nil {
		t.toolRoute = remoteRoute(ledger)
	} else {
		t.local = newLocalToolRoute(localRouteConfig{core: a.opts.Core, sessionID: params.SessionID, turnID: params.TurnID, ledger: ledger,
			policy: t.policy, ctx: ctx, log: a.log, hook: a.hook})
		t.toolRoute = t.local
	}
	s.turn = t
	a.mu.Unlock()
	if brain == nil {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "the agent session is not open"}
	}
	defer func() {
		a.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		a.mu.Unlock()
	}()
	if err := a.runInput(ctx, brain, t, params.InputID, params.Input, plan.skills, overrides); err != nil {
		return err
	}
	// turn.input received while the turn ran: prompted in order, in the same turn.
	for {
		select {
		case input := <-turn.Inputs():
			if err := a.runInput(ctx, brain, t, input.InputID, input.Input, nil, overrides); err != nil {
				return err
			}
		default:
			if t.local != nil && ctx.Err() == nil {
				t.local.Complete()
			}
			return nil
		}
	}
}

func (a *BrainHost) CloseSession(ctx context.Context, sessionID, _ string) error {
	a.opts.Core.local.background.killSession(sessionID)
	a.mu.Lock()
	s := a.sessions[sessionID]
	delete(a.sessions, sessionID)
	delete(a.suspended, sessionID) // a suspended session has no brain conversation left to close
	a.mu.Unlock()
	if s != nil && s.brain != nil && s.brain.Ready() {
		if err := s.brain.Close(ctx); err != nil {
			a.log(LogDebug, "brain session close "+sessionID, err)
		}
	}
	a.opts.Core.closeLocalSession(sessionID)
	_ = os.Remove(ledgerPath(a.opts.StateDir, sessionID))
	return nil
}

// MaxResidentSessions implements AgentSessionSuspender.
func (a *BrainHost) MaxResidentSessions() int { return a.opts.MaxResidentSessions }

// SuspendSession implements AgentSessionSuspender: it releases an idle
// session's brain conversation (BrainSession.Suspend: the brain frees the
// memory or process and keeps the conversation) and its Core session. The
// next OpenTurn of the session loads the conversation again.
func (a *BrainHost) SuspendSession(ctx context.Context, sessionID string) error {
	a.mu.Lock()
	s := a.sessions[sessionID]
	if s != nil && (s.turn != nil || s.opening != nil) {
		a.mu.Unlock()
		return errors.New("the session has a turn in progress")
	}
	a.mu.Unlock()
	if a.opts.Core.local.background.hasLive(sessionID) {
		return errors.New("the session has a command running in the background")
	}
	ref := a.adapter.Ref(sessionID)
	if s != nil && s.brain != nil {
		ref = s.brain.Ref()
		if err := s.brain.Suspend(ctx); err != nil {
			return err
		}
	}
	a.mu.Lock()
	if a.sessions[sessionID] == s {
		delete(a.sessions, sessionID)
		a.suspended[sessionID] = ref
	}
	a.mu.Unlock()
	a.opts.Core.suspendLocalSession(sessionID)
	return nil
}

// --- turn helpers ---

// runInput hands the brain one input unless an earlier process already
// finished it: an input counts as finished when its prompt ended and every
// event it produced is acknowledged by the platform. After a restart the
// turn's inputs come again (turn.start, turn.input); the brain continues the
// one it recorded without having finished it (same input id).
func (a *BrainHost) runInput(ctx context.Context, brain BrainSession, t *acpTurn, inputID string, input []AgentInputBlock, skills []InputSkillDocument, policy map[string]string) error {
	if t.ledger.inputDone(inputID) {
		return nil
	}
	t.mu.Lock()
	t.input = inputID
	t.mu.Unlock()
	var in BrainInput
	if t.ledger.inputBegun(inputID) {
		note, err := t.recover(ctx)
		if err != nil {
			return err
		}
		in = BrainInput{InputID: inputID + ":recovered", Text: note}
	} else {
		var err error
		if in, err = a.brainInput(ctx, t.turn, input); err != nil {
			return err
		}
		in.InputID, in.Skills = inputID, skills
	}
	in.ToolPolicy, in.Effort = policy, t.effort
	if a.caps.ModelCredentials == BrainModelPlatform {
		route := t.route
		in.Model = &route
	}
	if err := brain.Prompt(ctx, in, t); err != nil {
		return err
	}
	if err := t.turn.Flush(ctx); err != nil {
		return err
	}
	return t.ledger.markInputDone(inputID)
}

func (a *BrainHost) systemPrompt(cwd, system string, hands *EnvironmentInfo) string {
	prompt := fmt.Sprintf(a.opts.SystemPrompt, cwd)
	if hands != nil {
		prompt += "\n\n" + handsDescription(hands)
	}
	if system != "" {
		prompt += "\n\n" + system
	}
	return prompt
}

// sessionFilesNote tells the model where the session's files are and what it
// may do there ("" without ACPAgentOptions.SessionFilesRoot, and when the
// tools run on another machine). A session id that is no safe directory name
// has no files directory; the note is then left out and the tools stay on the
// workspace.
func (a *BrainHost) sessionFilesNote(sessionID string, remote bool) string {
	if a.opts.SessionFilesRoot == "" || remote {
		return ""
	}
	mounts, err := sessionMountsFor(a.opts.SessionFilesRoot, sessionID)
	if err != nil {
		a.log(LogWarn, "session files are not reachable for session "+sessionID, err)
		return ""
	}
	return fmt.Sprintf("Session files (absolute paths; use the read, glob and grep tools): the files the user gave this session are in %s and its skills in %s, both read-only. "+
		"Write the deliverables you want the user to get into %s (write and edit work there and nowhere outside the working directory).",
		mounts[0].dir, mounts[1].dir, mounts[2].dir)
}

// route is the platform's per-turn model endpoint when turn.start hands one,
// otherwise Upstream with the turn's model.
func (a *BrainHost) route(turn *AgentTurn, model string) acpRoute {
	metadata := turn.Params().Metadata
	if base, token := metadata["model_base_url"], metadata["model_token"]; base != "" && token != "" {
		// An unparsable or missing expiry is never refreshed (the platform did not say).
		expiresAt, _ := time.Parse(time.RFC3339, metadata["model_token_expires_at"])
		return acpRoute{platform: &acpPlatformModel{agent: a, turn: turn, base: strings.TrimRight(base, "/"), token: token, expiresAt: expiresAt}}
	}
	u := a.opts.Upstream
	return acpRoute{baseURL: strings.TrimRight(u.BaseURL, "/"), model: model, header: u.Header, sessionHeader: u.SessionHeader}
}

// turnModel is the model and thinking level of a turn (see
// ACPAgentOptions.Models); "" for a brain that runs the model its owner
// configured.
func (a *BrainHost) turnModel(config AgentTurnConfig) (model, level string, err error) {
	if !a.caps.ModelChoice {
		return "", "", nil
	}
	if len(a.models) == 0 {
		return a.opts.Model, "", nil // the contract: no models declared, config.model is ignored
	}
	model = cmp.Or(config.Model, a.opts.Model)
	i := slices.IndexFunc(a.models, func(m AgentModel) bool { return m.ID == model })
	if i < 0 {
		if config.Model == "" {
			return model, "", nil // the host's default, not a choice of the turn
		}
		return "", "", &RPCError{Code: CodeInvalidParams, Message: "invalid params: model " + model + " is not one of this executor's models"}
	}
	m := a.models[i]
	switch {
	case len(m.ThinkingLevels) == 0:
		return model, "", nil
	case config.ThinkingLevel == "":
		return model, m.DefaultThinkingLevel, nil
	case !slices.Contains(m.ThinkingLevels, config.ThinkingLevel):
		return "", "", &RPCError{Code: CodeInvalidParams, Message: "invalid params: thinking_level " + config.ThinkingLevel + " is not one the model " + model + " takes (" + strings.Join(m.ThinkingLevels, ", ") + ")"}
	}
	return model, config.ThinkingLevel, nil
}

// acpCheckModels validates ACPAgentOptions.Models and returns a copy.
func acpCheckModels(models []AgentModel) ([]AgentModel, error) {
	out := make([]AgentModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" || slices.ContainsFunc(out, func(o AgentModel) bool { return o.ID == m.ID }) {
			return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Models: model ids must be set and distinct, got %q", m.ID)
		}
		for i, level := range m.ThinkingLevels {
			if !slices.Contains(acpReasoningEfforts, level) || slices.Contains(m.ThinkingLevels[:i], level) {
				return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Models[%s].ThinkingLevels: %q must be one of %s, once", m.ID, level, strings.Join(acpReasoningEfforts, ", "))
			}
		}
		if m.DefaultThinkingLevel != "" && !slices.Contains(m.ThinkingLevels, m.DefaultThinkingLevel) {
			return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.Models[%s].DefaultThinkingLevel %q is not one of its ThinkingLevels", m.ID, m.DefaultThinkingLevel)
		}
		m.ThinkingLevels = slices.Clone(m.ThinkingLevels)
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// acpAgentModels is the capability's models: the host's, keeping only the
// thinking levels the brain takes.
func acpAgentModels(models []AgentModel, efforts []string) []AgentModel {
	if len(models) == 0 {
		return nil
	}
	out := make([]AgentModel, len(models))
	for i, m := range models {
		levels := slices.DeleteFunc(slices.Clone(m.ThinkingLevels), func(l string) bool { return !slices.Contains(efforts, l) })
		m.ThinkingLevels = nil
		if len(levels) > 0 {
			m.ThinkingLevels = levels
		}
		if !slices.Contains(levels, m.DefaultThinkingLevel) {
			m.DefaultThinkingLevel = ""
		}
		out[i] = m
	}
	return out
}

func (a *BrainHost) turnCwd(sessionID, requested string) (string, error) {
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

func (a *BrainHost) session(id string) *acpSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		return s
	}
	s := &acpSession{id: id}
	a.sessions[id] = s
	return s
}

func acpToolsViolation(want, catalog []string) string {
	got := append([]string(nil), catalog...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Sprintf("agent exposes tools [%s], not exactly the platform tools: refused (fail-closed)", strings.Join(got, ","))
	}
	return ""
}

// --- BrainSink ---

// policyOf is the turn's permission for a tool (an unlisted tool asks).
func (t *acpTurn) policyOf(name string) string {
	if p := t.policy[name]; p != "" {
		return p
	}
	return "ask"
}

func (t *acpTurn) eventID(key string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.messages[key]
	if !ok {
		id = t.turn.NewEventID()
		t.messages[key] = id
	}
	return id
}

// begin records that the brain started working on the current input.
func (t *acpTurn) begin() {
	t.mu.Lock()
	input := t.input
	t.mu.Unlock()
	if err := t.ledger.markInputBegun(input); err != nil {
		t.agent.log(LogWarn, "turn state: input "+input+" begun but not recorded", err)
	}
}

func (t *acpTurn) Delta(kind, messageID, text string) {
	t.begin()
	eventType := EventDeltaTypeAgentMessage
	if kind == BrainKindThinking {
		eventType = EventDeltaTypeAgentThinking
	}
	id := t.eventID(kind + "\x00" + messageID)
	t.mu.Lock()
	index := t.index[id]
	t.index[id]++
	t.mu.Unlock()
	t.turn.Delta(id, eventType, index, text)
}

func (t *acpTurn) Message(kind, messageID, text string) {
	t.begin()
	key := kind + "\x00" + messageID
	id := t.eventID(key)
	t.mu.Lock()
	delete(t.messages, key)
	t.mu.Unlock()
	content := []ContentBlock{{Type: ContentBlockTypeText, Text: text}}
	if kind == BrainKindThinking {
		_, _ = t.turn.EmitWithID(id, AgentThinkingEvent{Type: AgentEventTypeAgentThinking, Content: content})
		return
	}
	_, _ = t.turn.EmitWithID(id, AgentMessageEvent{Type: AgentEventTypeAgentMessage, Content: content})
}

// customCall: the call's event is its agent.custom_tool_use (Call), and it is never approved.
func (t *acpTurn) customCall(name string) bool {
	return t.custom[name] || name == ToolNameAskUserQuestion
}

func (t *acpTurn) ToolUse(call BrainCall, permission string) {
	if t.customCall(call.Name) {
		return
	}
	if permission == "" {
		permission = t.policyOf(call.Name)
	}
	if permission == "ask" && t.ledger.call(call.ID).Decision == "" && t.hostAllows(t.ctx, t.turn.SessionID(), call.ID, call.Name, call.Input) {
		permission = "allow"
	}
	t.toolUse(call.ID, call.Name, call.Input, permission)
}

// ToolResult emits the agent.tool_result of a finished tool call.
func (t *acpTurn) ToolResult(callID string, content []ContentBlock, isError bool) {
	t.mu.Lock()
	toolUseID := t.tools[callID]
	t.mu.Unlock()
	if toolUseID == "" {
		return
	}
	_ = t.turn.emitAcked(toolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: toolUseID, Content: content, IsError: isError}, func() {
		if err := t.ledger.update(callID, func(c *acpCallState) { c.Reported = true }); err != nil {
			t.agent.log(LogWarn, "turn state: tool result of "+callID+" acknowledged but not recorded", err)
		}
		if t.agent.hook != nil {
			t.agent.hook(t.ctx, "reported")
		}
	})
}

// toolResultEvent is agent.tool_result with is_error always present, as the
// platform brains write it (CMA: a boolean of every tool result).
type toolResultEvent struct {
	Type      string         `json:"type"`
	ToolUseID string         `json:"tool_use_id"`
	Content   []ContentBlock `json:"content,omitempty"`
	IsError   bool           `json:"is_error"`
}

// toolUse emits agent.tool_use once per tool call and returns its event id.
func (t *acpTurn) toolUse(callID, name string, input map[string]any, permission string) string {
	if t.ledger.call(callID).Reported {
		return "" // the platform already has this call's events (an earlier process)
	}
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
	t.begin()
	if err := t.ledger.update(callID, func(c *acpCallState) { c.ToolUseID, c.ToolName, c.Input, c.Permission = id, name, input, permission }); err != nil {
		t.agent.log(LogWarn, "turn state: tool call "+callID+" not recorded", err)
	}
	t.emitToolUse(callID, id, name, input, permission)
	return id
}

// emitToolUse emits a call's agent.tool_use and records its acknowledgement.
func (t *acpTurn) emitToolUse(callID, id, name string, input map[string]any, permission string) {
	_ = t.turn.emitAckedWithID(id, AgentToolUseEvent{Type: AgentEventTypeAgentToolUse, Name: name, Input: input, EvaluatedPermission: &EvaluatedPermission{Type: permission}}, func() {
		if err := t.ledger.update(callID, func(c *acpCallState) { c.UseReported = true }); err != nil {
			t.agent.log(LogWarn, "turn state: tool use of "+callID+" acknowledged but not recorded", err)
		}
	})
}

// Permission decides a call: the turn's policy, the host's approver, a
// decision the platform already gave (before a restart), or the platform's
// user through an interaction with the call's deterministic approval id.
func (t *acpTurn) Permission(ctx context.Context, call BrainCall) BrainDecision {
	a := t.agent
	sessionID := t.turn.SessionID()
	interactionID := ApprovalID(sessionID, t.turnID, call.ID)
	permission := t.policyOf(call.Name)
	if t.customCall(call.Name) {
		// Not an operation on a machine: its custom tool call is the event,
		// and Call refuses it when the turn's policy denies it.
		return BrainDecision{Outcome: BrainAllow}
	}
	decide := func(decision string) error {
		return t.ledger.decide(call.ID, decision, interactionID, call.Name, call.Input)
	}
	if permission == "ask" && t.ledger.call(call.ID).Decision == "" && t.hostAllows(ctx, sessionID, call.ID, call.Name, call.Input) {
		// The host's ComputerUseApprover lets this call through: it is
		// recorded as allowed for the gate, and reported as allow, so no
		// client is asked.
		t.toolUse(call.ID, call.Name, call.Input, "allow")
		if err := decide(acpDecisionAllow); err != nil {
			a.log(LogError, "turn state: decision of "+call.ID+" not recorded", err)
			return BrainDecision{Outcome: BrainCanceled}
		}
		return BrainDecision{Outcome: BrainAllow}
	}
	toolUseID := t.toolUse(call.ID, call.Name, call.Input, permission)
	switch permission {
	case "allow":
		return BrainDecision{Outcome: BrainAllow}
	case "deny":
		return BrainDecision{Outcome: BrainDeny, Message: "The tool " + call.Name + " is not allowed in this session."}
	}
	// An approval the platform already answered (before a restart) is not asked again.
	switch t.ledger.call(call.ID).Decision {
	case acpDecisionAllow:
		return BrainDecision{Outcome: BrainAllow}
	case acpDecisionDeny:
		return BrainDecision{Outcome: BrainDeny, Message: refusalText}
	}
	a.interaction(ACPInteractionEvent{Phase: "requested", SessionID: sessionID, InteractionID: interactionID, ToolName: call.Name, ToolInput: call.Input})
	answer, err := t.turn.RequestInteraction(ctx, InteractionRequestParams{
		Kind: InteractionKindPermission, InteractionID: interactionID,
		// The user may allow the tool for the rest of the session: the
		// platform records that grant and answers the session's later
		// requests for the tool itself (contract 1.10.0, ADR-0068), so every
		// call is still asked here and a revoked grant asks the user again.
		ToolUseID: toolUseID, ToolName: call.Name, ToolInput: call.Input, Scopes: []string{InteractionScopeOnce, InteractionScopeSession},
	})
	outcome := string(answer.Outcome)
	if err != nil {
		outcome = "withdrawn"
	}
	a.interaction(ACPInteractionEvent{Phase: "ended", SessionID: sessionID, InteractionID: interactionID, ToolName: call.Name, Outcome: outcome})
	switch {
	case err != nil:
		return BrainDecision{Outcome: BrainCanceled}
	case answer.Outcome == InteractionOutcomeAllow:
		if err := decide(acpDecisionAllow); err != nil {
			a.log(LogError, "turn state: decision of "+call.ID+" not recorded", err)
			return BrainDecision{Outcome: BrainCanceled}
		}
		return BrainDecision{Outcome: BrainAllow}
	case answer.Outcome == InteractionOutcomeDeny:
		if err := decide(acpDecisionDeny); err != nil {
			a.log(LogError, "turn state: decision of "+call.ID+" not recorded", err)
			return BrainDecision{Outcome: BrainCanceled}
		}
		return BrainDecision{Outcome: BrainDeny, Message: refusalText}
	default: // expired, canceled
		return BrainDecision{Outcome: BrainCanceled}
	}
}

// refusalText is what the model reads when the user denied a call.
const refusalText = "The user denied this tool call."

// Call runs one tool call: a custom tool and ask_user_question as a custom
// tool call of the session, anything else through the turn's route.
func (t *acpTurn) Call(ctx context.Context, call BrainCall) *RouteResult {
	if t.customCall(call.Name) {
		if t.policyOf(call.Name) == "deny" {
			return refused("permission_denied", "the "+call.Name+" tool is not allowed in this session")
		}
		if call.Name == ToolNameAskUserQuestion {
			return t.askUser(ctx, call.ID, call.Input)
		}
		text, isError, ok := t.customTool(ctx, call.ID, call.Name, call.Input)
		if !ok {
			return withdrawnCall()
		}
		return &RouteResult{Outcome: RouteOutcomeExecuted, Text: text, IsError: isError}
	}
	t.mu.Lock()
	toolUseID := t.tools[call.ID]
	t.mu.Unlock()
	return t.toolRoute.Call(ctx, RouteCall{ID: call.ID, ToolUseID: toolUseID, Name: call.Name, Input: call.Input})
}

// recover settles the tool calls of an interrupted input that have no
// acknowledged result, under their own agent.tool_use events and approval
// ids (an answered approval is not asked again, a pending one is asked again;
// the route replays a call that ran and does not run again one whose outcome
// is unknown), and returns the note that tells the brain how they ended.
func (t *acpTurn) recover(ctx context.Context) (string, error) {
	lines := []string{}
	for _, c := range t.ledger.unreported() {
		if !c.UseReported {
			// The platform never got its agent.tool_use: emitted again (event
			// ids are this process's).
			c.ToolUseID = t.turn.NewEventID()
			if err := t.ledger.update(c.ID, func(s *acpCallState) { s.ToolUseID = c.ToolUseID }); err != nil {
				return "", err
			}
			t.emitToolUse(c.ID, c.ToolUseID, c.ToolName, c.Input, c.Permission)
		}
		t.mu.Lock()
		t.tools[c.ID] = c.ToolUseID
		t.mu.Unlock()
		call := BrainCall{ID: c.ID, Name: c.ToolName, Input: c.Input}
		raw, _ := json.Marshal(c.Input)
		label := c.ToolName + " " + string(raw)
		var text string
		var isError bool
		switch d := t.Permission(ctx, call); d.Outcome {
		case BrainAllow:
			r := t.Call(ctx, call)
			text, isError = r.Text, r.IsError
			lines = append(lines, fmt.Sprintf("- %s: settled after the restart. Result%s:\n%s", label, map[bool]string{true: " (error)"}[isError], text))
		case BrainDeny:
			text, isError = d.Message, true
			lines = append(lines, fmt.Sprintf("- %s: not approved: %s", label, text))
		default:
			return "", cmp.Or(ctx.Err(), errors.New("the approval of "+c.ID+" was withdrawn"))
		}
		t.ToolResult(c.ID, []ContentBlock{{Type: ContentBlockTypeText, Text: text}}, isError)
	}
	if len(lines) == 0 {
		lines = append(lines, "- none")
	}
	return strings.Join(append(append([]string{recoveryNote}, lines...), "Continue the original task from here."), "\n"), nil
}

// recoveryNote opens the input that continues a turn after an executor restart.
const recoveryNote = "[platform] The executor restarted while this turn was running. The interrupted tool calls were settled as follows:"
