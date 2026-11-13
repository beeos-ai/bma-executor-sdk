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
// TurnResume: when the executor process restarts, the platform starts the
// in-flight turn again and ACPAgent continues it from the agent's durable
// conversation and its own ledger of tool calls, approvals and acknowledged
// events (agent_acp_resume.go); nothing that already happened is repeated.
//
// Tools run in the executor's Core, in the turn's working directory, so the
// hands of a local brain are the same as for a platform brain. Model requests
// go to a loopback proxy with a per-session placeholder key; the proxy
// forwards only during a running turn, refuses a request whose tools are not
// the six platform tools, and adds the real credential: the platform's
// per-turn model endpoint when turn.start carries one (metadata
// model_base_url, model_token; refreshed with model_token.refresh when at most
// 10 minutes of model_token_expires_at are left), otherwise the node owner's
// Upstream. PlatformModel declares platform_credentials harness_model (a
// sandbox machine of the platform). The agent process runs as the Core's
// RunAs identity, like the tools.

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

// DefaultACPSystemPrompt is the base system prompt of a local brain; the
// agent definition's system instructions follow it.
const DefaultACPSystemPrompt = "You are an agent running on the user's computer through BMA (BeeOS Managed Agents).\n" +
	"Your tools run on this computer, in the working directory %s; use paths under it.\n" +
	"Be concise. When a task is done, say what you did."

// acpPermissionModes are the session modes of bma-dsh-acp.
var acpPermissionModes = []string{"default", "accept_edits"}

// acpReasoningEfforts are the thinking levels a host may declare for a model
// (AgentModel.ThinkingLevels): the reasoning effort values of the model APIs,
// lowest first (OpenAI reasoning_effort; Claude Code effort levels are
// low..max). bma-dsh-acp sends the level as the request's reasoning_effort.
var acpReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// ACP profiles: which agent the ACPAgent hosts.
const (
	// ACPProfileDSH is bma-dsh-acp (ADR-0027), the default.
	ACPProfileDSH = "dsh"
	// ACPProfileClaudeCode is the official claude-agent-acp, unmodified (see
	// agent_acp_claude.go). It uses only the node owner's own Claude
	// credentials and never the platform's model endpoint.
	ACPProfileClaudeCode = "claude-code"
)

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
	// value never comes from the agent: a forwarded request is built anew and
	// takes no such header from the agent's request. Requests that go to the
	// platform's own model endpoint (turn.start metadata model_base_url) do
	// not carry it.
	SessionHeader string
}

// ACPAgentOptions configures NewACPAgent.
type ACPAgentOptions struct {
	// Profile selects the hosted agent: ACPProfileDSH (default) or
	// ACPProfileClaudeCode. The Claude Code profile rejects PlatformModel and
	// ignores Upstream and the platform's model credential: the agent talks to
	// its model with the node owner's own credentials, which the owner
	// configures for Claude Code itself (its login under HOME, or Env such as
	// ANTHROPIC_API_KEY / a third-party provider's variables); the SDK never
	// reads, forwards or logs them.
	Profile string
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
	// Required unless PlatformModel.
	Upstream ACPModelUpstream
	// PlatformModel asks the platform for the per-turn model credential
	// (agent capability platform_credentials: harness_model), as a sandbox
	// machine of the platform does. Without Upstream a turn the platform
	// hands no credential fails (session.error); it is never sent elsewhere.
	PlatformModel bool
	// PlatformTools declares platform_credentials brain_tools: this machine
	// may be the brain of a session whose hands are on another machine
	// (ADR-0029). The platform then hands the turn its tools endpoint and token
	// (turn.start metadata tools_mcp_*) and every tool call of that turn goes
	// there, never to this machine's Core (fail closed). A turn without
	// tools_mcp_* runs its tools here.
	PlatformTools bool
	// AskUserQuestion offers the DSH agent the ask_user_question tool (see
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
	// tools, and is enforced by the executor's gate whatever the agent does.
	// NewACPAgent rejects other values. Nil caps nothing.
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
	// (or PlatformTools) and an agent that takes images (initialize
	// promptCapabilities.image) the agent capability declares file_inputs and
	// the harness image_inputs, and the platform then sends image input; the
	// image reaches the agent as an ACP image content block. Without both, the
	// platform is told the agent takes no image and refuses an image input.
	InputFiles ACPInputFiles
	// DisablePrewarm turns off the background prewarm of the Claude Code
	// profile (see WaitPrewarm); the DSH profile has none.
	DisablePrewarm bool
	// HarnessID is the declared harness id (default "dsh").
	HarnessID string
	// Model is the model name the agent uses when the turn config has none (default Upstream.Model).
	Model string
	// Models are the models the host lets a turn choose (agent capability
	// models), for example the models the node owner's account may use: a
	// turn.start config.model must be one of their ids or the turn is refused
	// (-32602, never another model), and the turn's model requests go
	// upstream with that model. A model's ThinkingLevels are the reasoning
	// effort values (acpReasoningEfforts: none, minimal, low, medium, high,
	// xhigh, max) a turn may set with config.thinking_level, or
	// DefaultThinkingLevel when it sets none; the agent sends the level as the
	// request's reasoning_effort. An agent that does not take a reasoning
	// effort (bma-dsh-acp before 0.2.2) gets no levels declared, so a turn
	// naming one is refused rather than run without it. A model without
	// levels ignores config.thinking_level. Empty: config.model and
	// config.thinking_level are ignored (contract) and every turn runs on
	// Model. The DSH profile only.
	Models []AgentModel
	// SystemPrompt is the base system prompt, with one %s for the working directory (default DefaultACPSystemPrompt).
	SystemPrompt string
	// MaxSessions bounds the turns running at once (default
	// DefaultACPMaxSessions): a turn.start past it answers executor_busy, and an
	// idle session holds no slot.
	MaxSessions int64
	// MaxResidentSessions bounds the sessions whose agent session and Core
	// session are held, running or idle (default DefaultACPResidentSessions of the
	// Core's capacity, at least MaxSessions; it must not exceed the Core's
	// MaxSessions). Past it the least recently used idle session is suspended
	// (ACP session/close; the agent keeps its conversation) and loaded again by
	// its next turn (session/load with the executor_session_ref). A session with
	// a bash command left running is not suspended.
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

// ACPInteractionEvent is one step of a platform interaction asked by the local agent.
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

// ACPAgent hosts an ACP agent process; see the comment at the top of agent_acp.go.
type ACPAgent struct {
	opts ACPAgentOptions
	// models is the capability's models: opts.Models with the thinking levels
	// the agent takes (see ACPAgentOptions.Models).
	models  []AgentModel
	proxy   *acpModelProxy
	mcp     *acpToolServer // Claude Code profile: the platform tools as an MCP server
	harness AgentHarnessCapability
	warm    *acpPrewarm // Claude Code profile: the background prewarm, nil without

	mu       sync.Mutex
	proc     *acpProcess
	sessions map[string]*acpSession
	// suspended maps the sessions SuspendSession released to the id the agent
	// knows them by, for a platform that sent no executor_session_ref.
	suspended map[string]string
	closed    bool
	// accepted are the host tools the running agent took (agent_acp_host_tools.go).
	accepted []string

	// hook, set by tests only, runs at named points of tool execution; it
	// may block until ctx ends to stand in for a crash there.
	hook func(ctx context.Context, point string)
}

type acpSession struct {
	id          string
	cwd         string
	placeholder string
	proc        *acpProcess // the process the session is open in
	// mode is the platform permission mode (default, accept_edits); for the
	// DSH profile it is also the agent's session mode.
	mode string
	turn *acpTurn

	// Claude Code profile: agentID is the agent's own session id (DSH uses
	// the platform's), agentMode the permission mode the agent reported last.
	agentID   string
	agentMode string

	// plan is what the latest turn.start asked for; opening is the open of the
	// agent's session in flight (see openSession).
	plan    acpOpenPlan
	opening *acpOpening
}

// wire is the session id the agent knows the session by.
func (s *acpSession) wire() string {
	if s.agentID != "" {
		return s.agentID
	}
	return s.id
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
	agent *ACPAgent
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
	agent  *ACPAgent
	turn   *AgentTurn
	ctx    context.Context
	ledger *acpLedger
	route  acpRoute
	turnID string
	// toolRoute runs the agent's tool calls: the platform's gateway when the
	// turn carries tools_mcp_* (the hands are elsewhere), otherwise local.
	toolRoute ToolRoute
	// local is this machine's route (the hands are here), nil for a turn with
	// remote hands. It decides the fate of a bash command left in the background.
	local *LocalToolRoute
	// claude is the Claude Code profile's state (agent_acp_claude.go).
	claude acpClaudeTurn
	// policy is the effective permission of each tool (see acpEffectivePolicy);
	// the executor enforces it, whatever the agent does.
	policy map[string]string
	// hostAsk are the host tools whose ask is only their declared default:
	// neither the turn's tool_policy nor the host's ceiling names them.
	hostAsk map[string]bool
	// hostAllowed is the ComputerUseApprover's answer per toolCallId, asked
	// once so the tool_use event and the permission request agree.
	hostAllowed map[string]bool
	// effort is the turn's thinking level, sent with each prompt as
	// _meta.bma.reasoningEffort ("" for none).
	effort string

	mu       sync.Mutex
	messages map[string]string // messageId -> event id
	index    map[string]int64  // event id -> next delta index
	tools    map[string]string // toolCallId -> agent.tool_use event id
	// questions maps an ask_user_question toolCallId to its agent.custom_tool_use event id.
	questions map[string]string
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
	switch opts.Profile {
	case "":
		opts.Profile = ACPProfileDSH
	case ACPProfileDSH, ACPProfileClaudeCode:
	default:
		return nil, fmt.Errorf("bmaexecutor: unknown ACPAgentOptions.Profile %q", opts.Profile)
	}
	if opts.Profile == ACPProfileClaudeCode {
		if opts.PlatformModel {
			return nil, errors.New("bmaexecutor: the Claude Code profile uses the node owner's own credentials; PlatformModel (harness_model) is not allowed")
		}
		opts.Upstream = ACPModelUpstream{}
	} else if opts.Upstream.BaseURL == "" && !opts.PlatformModel {
		return nil, errors.New("bmaexecutor: ACPAgentOptions.Upstream.BaseURL is required unless PlatformModel is set")
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
	if len(models) > 0 && opts.Profile == ACPProfileClaudeCode {
		return nil, errors.New("bmaexecutor: ACPAgentOptions.Models is for the DSH profile; Claude Code uses the model its owner configured")
	}
	opts.Models = models
	if opts.SessionFilesRoot != "" && !filepath.IsAbs(opts.SessionFilesRoot) {
		return nil, fmt.Errorf("bmaexecutor: ACPAgentOptions.SessionFilesRoot must be an absolute path, got %q", opts.SessionFilesRoot)
	}
	if opts.HarnessID == "" {
		opts.HarnessID = "dsh"
		if opts.Profile == ACPProfileClaudeCode {
			opts.HarnessID = "claude-code"
		}
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
		// The agent runs as the Core's workload identity and keeps its conversations here.
		if err := os.Chown(opts.StateDir, int(runAs.UID), int(runAs.GID)); err != nil {
			return nil, err
		}
	}
	a := &ACPAgent{opts: opts, sessions: map[string]*acpSession{}, suspended: map[string]string{}}
	if a.claude() {
		// No model proxy: the agent never reaches the platform's model endpoint.
		server, err := startACPToolServer(a)
		if err != nil {
			return nil, err
		}
		a.mcp = server
	} else {
		proxy, err := startACPModelProxy(a)
		if err != nil {
			return nil, err
		}
		a.proxy = proxy
	}
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		a.closeServers()
		return nil, err
	}
	init := proc.init
	if a.claude() {
		a.harness = AgentHarnessCapability{
			ID: opts.HarnessID, Version: init.AgentInfo.Version, Protocol: "acp/1",
			AgentName: init.AgentInfo.Name, AgentVersion: init.AgentInfo.Version,
			Streaming: "token", PermissionRelay: "full", SessionLoad: init.AgentCapabilities.LoadSession,
		}
		a.startPrewarm()
		return a, nil
	}
	a.harness = AgentHarnessCapability{
		ID: opts.HarnessID, Version: init.AgentInfo.Version + "+dsh" + init.Meta.Bma.DSH, Protocol: "acp/1",
		AgentName: init.AgentInfo.Name, AgentVersion: init.AgentInfo.Version,
		Streaming: init.Meta.Bma.Streaming, PermissionRelay: init.Meta.Bma.PermissionRelay,
		SessionLoad: init.AgentCapabilities.LoadSession, ImageInputs: proc.canTakeImages(),
	}
	a.models = acpAgentModels(opts.Models, init.Meta.Bma.ReasoningEfforts)
	if len(opts.Models) > 0 && len(init.Meta.Bma.ReasoningEfforts) == 0 && slices.ContainsFunc(opts.Models, func(m AgentModel) bool { return len(m.ThinkingLevels) > 0 }) {
		a.log(LogWarn, "the agent takes no reasoning effort (bma-dsh-acp >= 0.2.2 does); the models are declared without thinking levels", nil)
	}
	return a, nil
}

// Capability is the agent capability to serve: one harness, in harnesses[]
// and in the deprecated harness member.
func (a *ACPAgent) Capability() AgentCapability {
	h := a.harness
	c := AgentCapability{
		Enabled: true, PermissionModes: acpPermissionModes, InteractionKinds: []string{InteractionKindPermission},
		// ask_user_question is a custom tool call of the session (agent_acp_host_tools.go).
		CustomTools: a.opts.AskUserQuestion && !a.claude(),
		// Claude Code repeats a re-sent prompt as a new user message, so a turn
		// interrupted by an executor restart is not continued (ADR-0013: failed).
		TurnResume:  !a.claude(),
		FileInputs:  a.fileInputs(h.ImageInputs),
		MaxSessions: a.opts.MaxSessions, Harness: &h, Harnesses: []AgentHarnessCapability{a.harness},
		Models: a.models,
	}
	if a.opts.PlatformModel {
		c.PlatformCredentials = append(c.PlatformCredentials, PlatformCredentialHarnessModel)
	}
	if a.opts.PlatformTools {
		c.PlatformCredentials = append(c.PlatformCredentials, PlatformCredentialBrainTools)
	}
	return c
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
	a.opts.Core.local.background.killAll()
	a.closeServers()
	return nil
}

func (a *ACPAgent) closeServers() {
	if a.proxy != nil {
		a.proxy.close()
	}
	if a.mcp != nil {
		a.mcp.close()
	}
}

func (a *ACPAgent) interaction(e ACPInteractionEvent) {
	if a.opts.OnInteraction != nil {
		a.opts.OnInteraction(e)
	}
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
	// With tools_mcp_* the hands are elsewhere: config.Cwd is a path on that
	// machine, not checked here, and the agent works in a scratch workspace.
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
	// Opening the agent's session (session/new, which for Claude Code starts a
	// Claude process) is not done here: turn.start is answered as soon as the
	// turn is accepted (contract) and the open runs first thing in RunTurn,
	// where a failure ends the turn with session.error and a cancel ends it
	// canceled. Everything that can be refused cheaply and for certain is
	// refused above.
	s := a.session(params.SessionID)
	a.mu.Lock()
	if wire, ok := a.suspended[params.SessionID]; ok {
		// Suspended while idle: load its conversation again, by the id the
		// agent knows it by when the platform did not send one.
		delete(a.suspended, params.SessionID)
		if params.ExecutorSessionRef == "" {
			params.ExecutorSessionRef = wire
		}
	}
	resumed := params.ExecutorSessionRef != "" || (s.proc != nil && s.proc.alive())
	ref := s.wire()
	if a.claude() && s.agentID == "" {
		ref = params.ExecutorSessionRef // not known before session/new; see CHANGELOG
	}
	system := config.System
	mountNote := a.sessionFilesNote(params.SessionID, remote)
	if mountNote != "" {
		system = strings.TrimSpace(system + "\n\n" + mountNote)
	}
	s.plan = acpOpenPlan{params: params, cwd: cwd, promptCwd: promptCwd, model: model, effort: level, mode: mode, system: system, hands: config.EnvironmentInfo}
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
		Effective: AgentTurnConfig{Model: model, ThinkingLevel: level, PermissionMode: mode, Cwd: promptCwd, Harness: a.harness.ID},
	}, nil
}

func (a *ACPAgent) RunTurn(ctx context.Context, turn *AgentTurn) error {
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
	a.mu.Unlock()
	route := a.route(turn, plan.model)
	if !a.claude() && route.baseURL == "" && route.platform == nil {
		return errors.New("the platform handed no model credential for this turn (turn.start metadata model_base_url, model_token)")
	}
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
	for _, tool := range a.hostTools() {
		_, named := effective[tool.Name]
		_, capped := a.opts.ToolPolicy[tool.Name]
		hostAsk[tool.Name] = tool.Permission == "ask" && !named && !capped
	}
	policy := acpCapPolicy(a.withHostTools(effective), a.opts.ToolPolicy)
	a.mu.Lock()
	t := &acpTurn{agent: a, turn: turn, ctx: ctx, ledger: ledger, route: route, turnID: params.TurnID, policy: policy, hostAsk: hostAsk, hostAllowed: map[string]bool{}, effort: plan.effort,
		messages: map[string]string{}, index: map[string]int64{}, tools: map[string]string{}, questions: map[string]string{}}
	if remoteRoute != nil {
		t.toolRoute = remoteRoute(ledger)
	} else {
		t.local = newLocalToolRoute(localRouteConfig{core: a.opts.Core, sessionID: params.SessionID, turnID: params.TurnID, ledger: ledger,
			policy: t.policy, ctx: ctx, log: a.log, hook: a.hook})
		t.toolRoute = t.local
	}
	s.turn = t
	proc := s.proc
	a.mu.Unlock()
	if proc == nil {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "the agent session is not open"}
	}
	defer func() {
		a.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		a.mu.Unlock()
	}()
	if err := a.runInput(ctx, proc, t, s.wire(), params.InputID, params.Input, overrides); err != nil {
		return err
	}
	// turn.input received while the turn ran: prompted in order, in the same turn.
	for {
		select {
		case input := <-turn.Inputs():
			if err := a.runInput(ctx, proc, t, s.wire(), input.InputID, input.Input, overrides); err != nil {
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

func (a *ACPAgent) CloseSession(ctx context.Context, sessionID, _ string) error {
	a.opts.Core.local.background.killSession(sessionID)
	a.mu.Lock()
	s := a.sessions[sessionID]
	delete(a.sessions, sessionID)
	delete(a.suspended, sessionID) // a suspended session has no agent session left to close
	a.mu.Unlock()
	if s != nil && s.proc != nil && s.proc.alive() {
		if err := s.proc.call(ctx, "session/close", map[string]any{"sessionId": s.wire()}, nil); err != nil {
			a.log(LogDebug, "acp session/close "+sessionID, err)
		}
	}
	a.opts.Core.closeLocalSession(sessionID)
	_ = os.Remove(ledgerPath(a.opts.StateDir, sessionID))
	return nil
}

// MaxResidentSessions implements AgentSessionSuspender.
func (a *ACPAgent) MaxResidentSessions() int { return a.opts.MaxResidentSessions }

// SuspendSession implements AgentSessionSuspender: it releases an idle
// session's agent session (ACP session/close: the agent frees the memory or
// process and keeps the conversation for session/load) and its Core session.
// The next OpenTurn of the session loads the conversation again.
func (a *ACPAgent) SuspendSession(ctx context.Context, sessionID string) error {
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
	wire := sessionID
	if s != nil {
		wire = s.wire()
		if s.proc != nil && s.proc.alive() {
			err := s.proc.call(ctx, "session/close", map[string]any{"sessionId": wire}, nil)
			var acpErr *acpError
			if err != nil && !(errors.As(err, &acpErr) && acpErr.Code == CodeNotFound) && s.proc.alive() {
				return fmt.Errorf("agent session/close: %w", err)
			}
		}
	}
	a.mu.Lock()
	if a.sessions[sessionID] == s {
		delete(a.sessions, sessionID)
		a.suspended[sessionID] = wire
	}
	a.mu.Unlock()
	a.opts.Core.suspendLocalSession(sessionID)
	return nil
}

// --- turn helpers ---

// runInput prompts the agent with one input unless an earlier process
// already finished it: an input counts as finished when its prompt ended and
// every event it produced is acknowledged by the platform. After a restart
// the turn's inputs come again (turn.start, turn.input); the agent continues
// the one it recorded without having finished it (same inputId).
func (a *ACPAgent) runInput(ctx context.Context, proc *acpProcess, t *acpTurn, sessionID, inputID string, input []AgentInputBlock, policy map[string]string) error {
	if t.ledger.inputDone(inputID) {
		return nil
	}
	content, err := a.promptContent(ctx, proc, t.turn, input)
	if err != nil {
		return err
	}
	err = a.prompt(ctx, proc, sessionID, inputID, content, policy, t.effort)
	if a.claude() {
		t.flushMessages()
		if violation := t.violationError(); violation != nil {
			return violation
		}
	}
	if err != nil {
		return err
	}
	if err := t.turn.Flush(ctx); err != nil {
		return err
	}
	return t.ledger.markInputDone(inputID)
}

func (a *ACPAgent) prompt(ctx context.Context, proc *acpProcess, sessionID, inputID string, content []any, policy map[string]string, effort string) error {
	params := map[string]any{"sessionId": sessionID, "prompt": content}
	if !a.claude() {
		meta := map[string]any{"inputId": inputID}
		if len(policy) > 0 {
			meta["toolPolicy"] = policy
		}
		if effort != "" {
			meta["reasoningEffort"] = effort
		}
		params["_meta"] = map[string]any{acpMetaKey: meta}
	}
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

func (a *ACPAgent) systemPrompt(cwd, system string, hands *EnvironmentInfo) string {
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
func (a *ACPAgent) sessionFilesNote(sessionID string, remote bool) string {
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

func (a *ACPAgent) modelMeta(s *acpSession, model string, metadata map[string]string) map[string]any {
	meta := map[string]any{"baseUrl": a.proxy.url + "/v1", "token": s.placeholder, "model": model}
	for key, name := range map[string]string{"model_context_window": "contextWindow", "model_max_output_tokens": "maxOutputTokens"} {
		if n, err := strconv.Atoi(metadata[key]); err == nil && n > 0 {
			meta[name] = n
		}
	}
	return meta
}

// route is the platform's per-turn model endpoint when turn.start hands one,
// otherwise Upstream with the turn's model.
func (a *ACPAgent) route(turn *AgentTurn, model string) acpRoute {
	if a.claude() {
		return acpRoute{} // the agent's model is the node owner's own; nothing is forwarded
	}
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
// ACPAgentOptions.Models); "" for the Claude Code profile, which runs on the
// model its owner configured.
func (a *ACPAgent) turnModel(config AgentTurnConfig) (model, level string, err error) {
	if a.claude() {
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
// thinking levels the agent takes (initialize _meta.bma.reasoningEfforts).
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

// sessionOf is the session that proc knows by wireID.
func (a *ACPAgent) sessionOf(proc *acpProcess, wireID string) *acpSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.proc == proc && s.wire() == wireID {
			return s
		}
	}
	return nil
}

// turnFor is the running turn of the session that proc knows by wireID.
func (a *ACPAgent) turnFor(proc *acpProcess, wireID string) *acpTurn {
	s := a.sessionOf(proc, wireID)
	if s == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return s.turn
}

func (a *ACPAgent) claude() bool { return a.opts.Profile == ACPProfileClaudeCode }

// catalogViolation checks a session's tool catalog is exactly the platform
// tools and the declared host tools.
func (a *ACPAgent) catalogViolation(catalog []string) string {
	var want []string
	for name := range a.toolNames() {
		want = append(want, name)
	}
	sort.Strings(want)
	return acpToolsViolation(want, catalog)
}

func acpCatalogViolation(catalog []string) string {
	want := []string{ToolNameBash, ToolNameEdit, ToolNameGlob, ToolNameGrep, ToolNameRead, ToolNameWrite}
	return acpToolsViolation(want, catalog)
}

func acpToolsViolation(want, catalog []string) string {
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
		if a.claude() {
			a.trackClaudeMode(proc, n.SessionID, n.Update)
		}
		if t := a.turnFor(proc, n.SessionID); t != nil {
			t.update(n.Update)
		}
	case "_claude/sdkMessage":
		a.onClaudeSDKMessage(proc, raw)
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
	sessionID := base.SessionID // the platform's id (the agent may know the session by another)
	if t != nil {
		sessionID = t.turn.SessionID()
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
				Meta       struct {
					ClaudeCode struct {
						ToolName string `json:"toolName"`
					} `json:"claudeCode"`
				} `json:"_meta"`
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
		toolName := p.ToolCall.Title
		if a.claude() {
			// Only the platform tools (the injected MCP server) may be used at all;
			// the platform's policy decides here, as bma-dsh-acp does for itself.
			name, ok := claudePlatformTool(p.ToolCall.Meta.ClaudeCode.ToolName)
			if !ok {
				a.log(LogWarn, fmt.Sprintf("claude code asked to use %q, which is not a platform tool: refused", p.ToolCall.Meta.ClaudeCode.ToolName), nil)
				return choose("reject_once"), nil
			}
			toolName = name
			permission := t.policyOf(name)
			t.toolUse(p.ToolCall.ToolCallID, name, p.ToolCall.RawInput, permission)
			switch permission {
			case "allow":
				return choose("allow_once"), nil
			case "deny":
				return choose("reject_once"), nil
			}
		}
		interactionID := ApprovalID(sessionID, t.turn.Params().TurnID, p.ToolCall.ToolCallID)
		if t.ledger.call(p.ToolCall.ToolCallID).Decision == "" && t.hostAllows(ctx, sessionID, p.ToolCall.ToolCallID, toolName, p.ToolCall.RawInput) {
			// The host's ComputerUseApprover lets this call through: it is
			// recorded as allowed for the gate, and reported as allow, so no
			// client is asked.
			t.toolUse(p.ToolCall.ToolCallID, toolName, p.ToolCall.RawInput, "allow")
			if err := t.ledger.decide(p.ToolCall.ToolCallID, acpDecisionAllow, interactionID, toolName, p.ToolCall.RawInput); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: "turn state: " + err.Error()}
			}
			return choose("allow_once"), nil
		}
		toolUseID := t.toolUse(p.ToolCall.ToolCallID, toolName, p.ToolCall.RawInput, "ask")
		// An approval the platform already answered (before a restart) is not asked again.
		switch t.ledger.call(p.ToolCall.ToolCallID).Decision {
		case acpDecisionAllow:
			return choose("allow_once"), nil
		case acpDecisionDeny:
			return choose("reject_once"), nil
		}
		a.interaction(ACPInteractionEvent{Phase: "requested", SessionID: sessionID, InteractionID: interactionID, ToolName: toolName, ToolInput: p.ToolCall.RawInput})
		answer, err := t.turn.RequestInteraction(ctx, InteractionRequestParams{
			Kind: InteractionKindPermission, InteractionID: interactionID,
			ToolUseID: toolUseID, ToolName: toolName, ToolInput: p.ToolCall.RawInput, Scopes: []string{InteractionScopeOnce},
		})
		outcome := string(answer.Outcome)
		if err != nil {
			outcome = "withdrawn"
		}
		a.interaction(ACPInteractionEvent{Phase: "ended", SessionID: sessionID, InteractionID: interactionID, ToolName: toolName, Outcome: outcome})
		decide := func(decision string) error {
			return t.ledger.decide(p.ToolCall.ToolCallID, decision, interactionID, toolName, p.ToolCall.RawInput)
		}
		switch {
		case err != nil:
			return cancelled, nil
		case answer.Outcome == InteractionOutcomeAllow:
			if err := decide(acpDecisionAllow); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: "turn state: " + err.Error()}
			}
			return choose("allow_once"), nil
		case answer.Outcome == InteractionOutcomeDeny:
			if err := decide(acpDecisionDeny); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: "turn state: " + err.Error()}
			}
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
		result := refused("stale_turn", "no turn of this session is running in this agent process")
		switch {
		case t == nil:
		case p.Name == ToolNameAskUserQuestion && !a.claude():
			result = refused("permission_denied", "the ask_user_question tool is not allowed in this session")
			if t.policyOf(p.Name) != "deny" {
				result = t.askUser(ctx, p.ToolCallID, p.Input)
			}
		default:
			result = t.toolRoute.Call(ctx, RouteCall{ID: p.ToolCallID, Name: p.Name, Input: p.Input})
		}
		response := map[string]any{"text": result.Text, "isError": result.IsError, "outcome": result.Outcome, "errorCode": result.Code}
		if images := acpImages(result.Images); len(images) > 0 {
			response["images"] = images
		}
		return response, nil
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
		ClaudeCode struct {
			ToolName string `json:"toolName"`
		} `json:"claudeCode"`
	} `json:"_meta"`
	// Claude Code profile: the agent's permission mode.
	CurrentModeID string          `json:"currentModeId"`
	ConfigOptions json.RawMessage `json:"configOptions"`
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
	if t.agent.claude() {
		t.updateClaude(u)
		return
	}
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
		if name == ToolNameAskUserQuestion {
			return // its agent.custom_tool_use is the call's event (askUser)
		}
		permission := u.Meta.Bma.Permission
		if permission == "" {
			permission = "ask"
		}
		if permission == "ask" && t.ledger.call(u.ToolCallID).Decision == "" && t.hostAllows(t.ctx, t.turn.SessionID(), u.ToolCallID, name, u.RawInput) {
			permission = "allow"
		}
		t.toolUse(u.ToolCallID, name, u.RawInput, permission)
	case "tool_call_update":
		t.toolResult(u)
	}
}

// toolResult emits the agent.tool_result of a finished tool call.
func (t *acpTurn) toolResult(u acpUpdate) {
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
	_ = t.turn.emitAcked(AgentToolResultEvent{Type: AgentEventTypeAgentToolResult, ToolUseID: toolUseID, Content: content, IsError: u.Status == "failed"}, func() {
		if err := t.ledger.update(u.ToolCallID, func(c *acpCallState) { c.Reported = true }); err != nil {
			t.agent.log(LogWarn, "turn state: tool result of "+u.ToolCallID+" acknowledged but not recorded", err)
		}
		if t.agent.hook != nil {
			t.agent.hook(t.ctx, "reported")
		}
	})
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
			HostTools       []string `json:"hostTools"`
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
	a.accepted = proc.init.Meta.Bma.HostTools
	return proc, nil
}

func (a *ACPAgent) startProcess(ctx context.Context) (*acpProcess, error) {
	cmd := exec.Command(a.opts.Command[0], a.opts.Command[1:]...)
	var env []string
	if !a.claude() {
		env = append(env, "BMA_DSH_ACP_HOME="+a.opts.StateDir)
	}
	runAs := a.opts.Core.opts.RunAs
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
	cmd.Env = append(env, a.opts.Env...)
	cmd.Dir = a.opts.StateDir
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
	initialize := map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]any{"name": "bma-executor-sdk"}}
	if meta := a.initializeMeta(); meta != nil {
		initialize["_meta"] = meta
	}
	if err := p.call(initCtx, "initialize", initialize, &p.init); err != nil {
		p.kill(a.opts.KillGrace)
		return nil, fmt.Errorf("agent initialize: %w (stderr: %s)", err, p.stderr.String())
	}
	if p.init.ProtocolVersion != 1 {
		p.kill(a.opts.KillGrace)
		return nil, fmt.Errorf("agent speaks ACP protocol %d, not 1", p.init.ProtocolVersion)
	}
	if a.claude() {
		if err := claudeCheckPin(p.init.AgentInfo.Name, p.init.AgentInfo.Version); err != nil {
			p.kill(a.opts.KillGrace)
			return nil, err
		}
		return p, nil
	}
	violation := acpCatalogViolation(p.init.Meta.Bma.Tools)
	if violation == "" {
		violation = a.hostToolsViolation(p.init.Meta.Bma.HostTools)
	}
	if violation != "" {
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
