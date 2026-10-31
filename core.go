package bmaexecutor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCoreMaxSessions       = 16
	DefaultCoreMaxMCPServers     = 4
	DefaultCoreBashTimeout       = 10 * time.Minute
	DefaultCoreMaxOutputBytes    = 1024 * 1024
	DefaultCoreInlineResultBytes = 48 * 1024
	coreMCPMaxConsecutiveFailure = 5
	coreMCPHealthyRun            = 30 * time.Second
	maxGrepMatches               = 200
	previewBytes                 = 8 * 1024
	maxEnvEntries                = 64
	maxEnvTotalBytes             = 16 * 1024
	// DefaultMaxArtifactBytes bounds one artifact upload (v1.1 §6.5).
	DefaultMaxArtifactBytes = 16 * 1024 * 1024
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// deniedEnv blocks loader/injection variables in request env (v1.1 §3.3, §13).
var deniedEnv = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true, "DYLD_FRAMEWORK_PATH": true,
	"NODE_OPTIONS": true, "PYTHONSTARTUP": true, "BASH_ENV": true, "ENV": true,
	"PERL5OPT": true, "RUBYOPT": true,
}

// baseEnv is the only executor environment a bash tool or process inherits;
// everything else comes from the request or CoreOptions.Env.
var baseEnv = []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"}

// CoreOptions configures the execution core.
type CoreOptions struct {
	// WorkRoot holds every session workspace: WorkRoot/<brain>/<session>.
	WorkRoot string
	// Workspace overrides the per-session directory (for example a machine
	// executor whose sessions all share one directory, or a desktop app
	// mapping a brain to a project folder). Tools are confined to it.
	Workspace func(brain, sessionID string) string
	// MaxSessions bounds active sessions across all attached brains;
	// MaxSessionsPerBrain bounds one brain (defaults to MaxSessions). A full
	// core answers session.assign with executor_busy (-32009, retryable).
	MaxSessions         int
	MaxSessionsPerBrain int
	// MaxMCPServersPerSession bounds session.assign.mcp_servers (default 4).
	MaxMCPServersPerSession int
	// BashTimeout applies when neither tool.call deadline_ms nor the bash
	// timeout_ms is set (default 10 minutes).
	BashTimeout time.Duration
	// MaxOutputBytes caps each captured bash stream (default 1 MiB).
	MaxOutputBytes int
	// InlineResultBytes is the largest bash output returned inline (default
	// 48 KiB, under the 64 KiB control frame). Larger output is uploaded as
	// an artifact when UploadArtifacts is set, otherwise truncated.
	InlineResultBytes int
	// UploadArtifacts advertises the artifacts capability and offloads large
	// bash results with Client.UploadArtifact. Only environment-scoped
	// executors have an artifact store.
	UploadArtifacts bool
	// Env is set on every bash tool and process and cannot be overridden by
	// a request (for example BMA_HOME).
	Env map[string]string
	// EndAfterToolCall marks sessions that carry exactly one tool call: the
	// core ends them (session.ended "completed") after the result is
	// delivered. BMA's durable selfhost queue assigns such sessions.
	EndAfterToolCall func(SessionAssignParams) bool
	OnLog            func(LogEvent)
}

// Handler serves one harness-to-executor request method registered by a
// Capability. A *RPCError result is sent as is; any other error becomes
// internal_error.
type Handler func(ctx context.Context, req Request) (any, error)

// Request is one inbound request routed to a Capability handler.
type Request struct {
	Brain  string
	Method string
	// ID is the JSON-RPC request id, the idempotency key of retried writes.
	ID     string
	Params json.RawMessage
	// Session is the active session named by params.session_id for a
	// SessionScoped capability, nil otherwise.
	Session *CoreSession
	Client  *Client
}

// Capability is an extension of the core: a set of methods behind one or
// more capability bits. The core advertises the bits through Capabilities,
// routes the methods for every attached brain and, for SessionScoped
// capabilities, answers session_not_active (-32007) for unknown sessions and
// calls OnSessionEnd before a session reports session.ended.
type Capability struct {
	// Advertise sets the capability bits on the register capabilities.
	Advertise     func(*Capabilities)
	Methods       map[string]Handler
	SessionScoped bool
	OnSessionEnd  func(*CoreSession)
}

// Core is the executor's execution core ("hands"): bash with process-group
// cancellation, file tools, a stdio MCP supervisor, per-session cleanup and
// registered capabilities (process.*, terminal.*, skill.install, ...). It is
// independent of who drives it: Attach it to one Client per harness
// ("brain"), whether that is the BMA cloud or a local harness on a local
// socket, and every brain gets the same protocol behavior.
//
// Isolation: sessions are keyed by (brain, session_id), so two brains may use
// the same session id without seeing each other; a request can only reach
// sessions its own brain assigned, and each session has its own workspace,
// processes and MCP servers. Capacity is shared: MaxSessions across brains,
// MaxSessionsPerBrain per brain. Bash is not a sandbox: every brain runs as
// the executor's OS user.
type Core struct {
	opts CoreOptions

	mu           sync.Mutex
	brains       map[string]*Client
	sessions     map[coreKey]*CoreSession
	capabilities []Capability
	methods      map[string]registeredMethod
	closed       bool
}

type registeredMethod struct {
	handler       Handler
	sessionScoped bool
}

type coreKey struct{ brain, session string }

// CoreSession is one assigned session of one brain.
type CoreSession struct {
	key       coreKey
	workspace string
	client    *Client
	params    SessionAssignParams
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
	values  map[any]any
}

func (s *CoreSession) ID() string        { return s.key.session }
func (s *CoreSession) Brain() string     { return s.key.brain }
func (s *CoreSession) Workspace() string { return s.workspace }
func (s *CoreSession) Client() *Client   { return s.client }

// Context is canceled when the session stops.
func (s *CoreSession) Context() context.Context { return s.ctx }

// Assignment is the session.assign that created the session.
func (s *CoreSession) Assignment() SessionAssignParams { return s.params }

// Value returns the per-session value for key, creating it with create on
// first use. Capabilities keep their session state here.
func (s *CoreSession) Value(key any, create func() any) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.values[key]; ok {
		return value
	}
	value := create()
	s.values[key] = value
	return value
}

// enter registers work that stop must wait for; false once stopping.
func (s *CoreSession) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.wg.Add(1)
	return true
}

// NewCore validates options and creates WorkRoot.
func NewCore(opts CoreOptions) (*Core, error) {
	if opts.WorkRoot == "" {
		return nil, errors.New("bmaexecutor: CoreOptions.WorkRoot is required")
	}
	root, err := filepath.Abs(opts.WorkRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	opts.WorkRoot = root
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = DefaultCoreMaxSessions
	}
	if opts.MaxSessionsPerBrain <= 0 || opts.MaxSessionsPerBrain > opts.MaxSessions {
		opts.MaxSessionsPerBrain = opts.MaxSessions
	}
	if opts.MaxMCPServersPerSession <= 0 {
		opts.MaxMCPServersPerSession = DefaultCoreMaxMCPServers
	}
	if opts.BashTimeout <= 0 {
		opts.BashTimeout = DefaultCoreBashTimeout
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultCoreMaxOutputBytes
	}
	if opts.InlineResultBytes <= 0 {
		opts.InlineResultBytes = DefaultCoreInlineResultBytes
	}
	return &Core{
		opts: opts, brains: map[string]*Client{}, sessions: map[coreKey]*CoreSession{},
		methods: map[string]registeredMethod{},
	}, nil
}

// Register adds a capability. Call it before Attach; a method may be
// registered once.
func (c *Core) Register(capability Capability) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.brains) > 0 {
		return errors.New("bmaexecutor: register capabilities before Attach")
	}
	for method := range capability.Methods {
		if _, exists := c.methods[method]; exists {
			return fmt.Errorf("bmaexecutor: method %s is already registered", method)
		}
		switch method {
		case MethodSessionAssign, MethodSessionStop, MethodToolCall, MethodToolCancel, MethodExecutorDrain:
			return fmt.Errorf("bmaexecutor: method %s is served by the core", method)
		}
	}
	for method, handler := range capability.Methods {
		c.methods[method] = registeredMethod{handler: handler, sessionScoped: capability.SessionScoped}
	}
	c.capabilities = append(c.capabilities, capability)
	return nil
}

// Capabilities is what an attached Client should advertise.
func (c *Core) Capabilities() Capabilities {
	capabilities := Capabilities{
		Shell: &ShellCapability{Enabled: true},
		Files: &FilesCapability{Enabled: true, ReadRoots: []string{c.opts.WorkRoot}, WriteRoots: []string{c.opts.WorkRoot}},
		MCP:   &MCPCapability{Stdio: true, MaxServers: c.opts.MaxMCPServersPerSession},
	}
	if c.opts.UploadArtifacts {
		capabilities.Artifacts = &ArtifactCapability{Upload: true, MaxBytes: DefaultMaxArtifactBytes}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, capability := range c.capabilities {
		if capability.Advertise != nil {
			capability.Advertise(&capabilities)
		}
	}
	return capabilities
}

// Attach serves session.assign, session.stop, tool.call and every registered
// capability method for one brain on client. Call it before client.Start.
// brain names the harness ("cloud", "local", ...) and must be unique per core.
func (c *Core) Attach(brain string, client *Client) error {
	if !safeName.MatchString(brain) {
		return fmt.Errorf("bmaexecutor: invalid brain name %q", brain)
	}
	c.mu.Lock()
	if _, exists := c.brains[brain]; exists {
		c.mu.Unlock()
		return fmt.Errorf("bmaexecutor: brain %q is already attached", brain)
	}
	c.brains[brain] = client
	methods := make(map[string]registeredMethod, len(c.methods))
	for method, registered := range c.methods {
		methods[method] = registered
	}
	c.mu.Unlock()
	client.OnSessionAssign(func(ctx context.Context, params SessionAssignParams) (SessionAssignResult, error) {
		return c.assign(brain, client, params)
	})
	client.OnSessionStop(func(ctx context.Context, params SessionStopParams) (SessionStopResult, error) {
		if !c.end(coreKey{brain, params.SessionID}, "stopped") {
			return SessionStopResult{}, &RPCError{Code: CodeSessionNotActive, Message: "session is not active"}
		}
		return SessionStopResult{Stopped: true}, nil
	})
	client.OnToolCallAccept(func(params ToolCallParams) error {
		return c.acceptToolCall(brain, params)
	})
	client.OnToolCall(func(ctx context.Context, params ToolCallParams) (ToolResultParams, error) {
		return c.toolCall(ctx, brain, params), nil
	})
	client.afterToolResult(func(params ToolCallParams) {
		session := c.session(coreKey{brain, params.SessionID})
		if session != nil && c.opts.EndAfterToolCall != nil && c.opts.EndAfterToolCall(session.params) {
			c.end(session.key, "completed")
		}
	})
	client.onSessionsLost(func(ids []string) {
		for _, id := range ids {
			if c.stop(coreKey{brain, id}) {
				c.log(LogInfo, "session "+id+" stopped: executor lease expired", nil)
			}
		}
	})
	for method, registered := range methods {
		registered := registered
		method := method
		client.OnRequest(method, func(ctx context.Context, params json.RawMessage) (any, error) {
			return c.serve(ctx, brain, client, method, registered, params)
		})
	}
	return nil
}

func (c *Core) serve(ctx context.Context, brain string, client *Client, method string, registered registeredMethod, params json.RawMessage) (any, error) {
	request := Request{Brain: brain, Method: method, ID: RequestID(ctx), Params: params, Client: client}
	if registered.sessionScoped {
		var scope struct {
			SessionID string `json:"session_id"`
		}
		if len(params) == 0 || json.Unmarshal(params, &scope) != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params"}
		}
		session := c.session(coreKey{brain, scope.SessionID})
		if session == nil || !session.enter() {
			return nil, &RPCError{Code: CodeSessionNotActive, Message: "session is not active"}
		}
		defer session.wg.Done()
		request.Session = session
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(session.ctx, cancel)()
	}
	return registered.handler(ctx, request)
}

// Close stops every session of every brain without reporting session.ended.
func (c *Core) Close() {
	c.mu.Lock()
	c.closed = true
	keys := make([]coreKey, 0, len(c.sessions))
	for key := range c.sessions {
		keys = append(keys, key)
	}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.stop(key)
		}()
	}
	wg.Wait()
}

func busy(message string) error {
	return &RPCError{Code: CodeExecutorBusy, Message: message, Data: &JSONRPCErrorData{Retryable: true}}
}

func (c *Core) workspace(brain, sessionID string) string {
	if c.opts.Workspace != nil {
		return c.opts.Workspace(brain, sessionID)
	}
	name := sessionID
	if !safeName.MatchString(name) {
		name = hex.EncodeToString([]byte(sessionID))
	}
	return filepath.Join(c.opts.WorkRoot, brain, name)
}

func (c *Core) assign(brain string, client *Client, params SessionAssignParams) (SessionAssignResult, error) {
	if params.SessionID == "" || params.Lease.Token == "" {
		return SessionAssignResult{}, &RPCError{Code: CodeInvalidParams, Message: "session_id and lease.token are required"}
	}
	if len(params.MCPServers) > c.opts.MaxMCPServersPerSession {
		return SessionAssignResult{}, &RPCError{Code: CodeInvalidParams, Message: "too many mcp_servers"}
	}
	key := coreKey{brain, params.SessionID}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return SessionAssignResult{Accepted: false, SessionID: params.SessionID}, nil
	}
	if _, exists := c.sessions[key]; exists {
		// A replayed assign (for example after reconnect) is idempotent.
		c.mu.Unlock()
		return SessionAssignResult{Accepted: true, SessionID: params.SessionID}, nil
	}
	perBrain := 0
	for existing := range c.sessions {
		if existing.brain == brain {
			perBrain++
		}
	}
	switch {
	case len(c.sessions) >= c.opts.MaxSessions:
		c.mu.Unlock()
		return SessionAssignResult{}, busy("executor is at its session capacity")
	case perBrain >= c.opts.MaxSessionsPerBrain:
		c.mu.Unlock()
		return SessionAssignResult{}, busy("this harness is at its session capacity on the executor")
	}
	// Sessions outlive one connection: their context is not the request's.
	ctx, cancel := context.WithCancel(context.Background())
	session := &CoreSession{
		key: key, workspace: c.workspace(brain, params.SessionID), client: client, params: params,
		ctx: ctx, cancel: cancel, values: map[any]any{},
	}
	c.sessions[key] = session
	c.mu.Unlock()

	if err := os.MkdirAll(session.workspace, 0o700); err != nil {
		c.stop(key)
		return SessionAssignResult{}, fmt.Errorf("create workspace: %w", err)
	}
	go c.prepare(client, session, params)
	return SessionAssignResult{Accepted: true, SessionID: params.SessionID}, nil
}

// prepare starts the session's environment-origin stdio MCP servers, waits
// for each first attempt, then reports session.ready.
func (c *Core) prepare(client *Client, session *CoreSession, params SessionAssignParams) {
	var first sync.WaitGroup
	for _, server := range params.MCPServers {
		if server.ConnectionOrigin != MCPConnectionOriginEnvironment {
			continue // the harness connects service-origin servers itself
		}
		if server.Transport.Type != MCPTransportTypeStdio || server.Transport.Command == "" {
			c.status(client, session, SessionStatusError, "mcp_unavailable: "+server.ServerKey+": only stdio is supported")
			continue
		}
		if !session.enter() {
			return
		}
		first.Add(1)
		go func() {
			defer session.wg.Done()
			c.superviseMCP(client, session, server, first.Done)
		}()
	}
	first.Wait()
	if session.ctx.Err() != nil {
		return
	}
	if _, err := client.SessionReady(session.ctx, SessionReadyParams{
		SessionID: params.SessionID, WorkID: params.WorkID, LeaseToken: params.Lease.Token,
	}); err != nil {
		c.log(LogWarn, "session.ready for "+params.SessionID+" was not accepted", err)
	}
}

func (c *Core) status(client *Client, session *CoreSession, status string, detail string) {
	_ = client.Notify(session.ctx, MethodSessionStatus, SessionStatusParams{SessionID: session.key.session, Status: status, Detail: detail})
}

// end stops the session and reports session.ended with reason. It reports
// whether the session existed.
func (c *Core) end(key coreKey, reason string) bool {
	session := c.session(key)
	if session == nil {
		return false
	}
	go func() {
		if !c.stop(key) {
			return
		}
		if _, err := session.client.SessionEnded(context.Background(), SessionEndedParams{SessionID: key.session, Reason: reason}); err != nil {
			c.log(LogWarn, "session.ended for "+key.session+" was not accepted", err)
		}
	}()
	return true
}

// stop cancels the session, runs every capability's OnSessionEnd (killing
// its processes) and waits for in-flight work. It reports whether this call
// removed the session.
func (c *Core) stop(key coreKey) bool {
	c.mu.Lock()
	session := c.sessions[key]
	delete(c.sessions, key)
	capabilities := append([]Capability(nil), c.capabilities...)
	c.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	session.stopped = true
	session.mu.Unlock()
	session.cancel()
	for _, capability := range capabilities {
		if capability.OnSessionEnd != nil {
			capability.OnSessionEnd(session)
		}
	}
	session.wg.Wait()
	return true
}

func (c *Core) session(key coreKey) *CoreSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[key]
}

func (c *Core) log(level LogLevel, message string, err error) {
	if c.opts.OnLog != nil {
		c.opts.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// --- tools ---

var coreTools = map[string]bool{
	ToolNameBash: true, ToolNameRead: true, ToolNameWrite: true, ToolNameEdit: true, ToolNameGlob: true, ToolNameGrep: true,
}

// acceptToolCall rejects a tool.call before it is acknowledged: an unknown
// session is session_not_active and an unsupported tool capability_unavailable.
func (c *Core) acceptToolCall(brain string, params ToolCallParams) error {
	if c.session(coreKey{brain, params.SessionID}) == nil {
		return &RPCError{Code: CodeSessionNotActive, Message: "session is not active"}
	}
	if !coreTools[params.Tool] {
		return &RPCError{Code: CodeCapabilityUnavailable, Message: "capability_unavailable",
			Data: &JSONRPCErrorData{Capability: "tool:" + params.Tool}}
	}
	return nil
}

func (c *Core) toolCall(ctx context.Context, brain string, params ToolCallParams) ToolResultParams {
	session := c.session(coreKey{brain, params.SessionID})
	if session == nil {
		return failedToolResult(params, "session_not_active: the session is not assigned to this harness")
	}
	if !session.enter() {
		return failedToolResult(params, "session_not_active: the session is stopping")
	}
	defer session.wg.Done()
	// session.stop cancels the tool as well as tool.cancel and the deadline.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopWatch := context.AfterFunc(session.ctx, cancel)
	defer stopWatch()

	var input ToolInput
	if len(params.Input) > 0 {
		if err := json.Unmarshal(params.Input, &input); err != nil {
			return failedToolResult(params, "invalid tool input: "+err.Error())
		}
	}
	started := time.Now()
	var result ToolResultParams
	var err error
	switch params.Tool {
	case ToolNameBash:
		result = c.bash(ctx, session, params, input)
	case ToolNameRead:
		result, err = c.read(session, input)
	case ToolNameWrite:
		result, err = c.write(session, input)
	case ToolNameEdit:
		result, err = c.edit(session, input)
	case ToolNameGlob:
		result, err = c.glob(session, input)
	case ToolNameGrep:
		result, err = c.grep(session, input)
	default:
		err = fmt.Errorf("capability_unavailable: tool %q is not supported", params.Tool)
	}
	if err != nil {
		result = failedToolResult(params, err.Error())
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

func textResult(isError bool, texts ...string) ToolResultParams {
	result := ToolResultParams{Outcome: ToolOutcomeSucceeded, IsError: isError}
	for _, text := range texts {
		result.Content = append(result.Content, ContentBlock{Type: ContentBlockTypeText, Text: text})
	}
	return result
}

// resolve confines path to the session workspace.
func (s *CoreSession) resolve(path string) (string, error) {
	if path == "" {
		return s.workspace, nil
	}
	resolved := path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(s.workspace, resolved)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(s.workspace, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the session workspace", path)
	}
	return resolved, nil
}

// ResolveCwd returns the real directory for a working directory request: a
// relative cwd is under the workspace, an absolute one must resolve (through
// symlinks) under it.
func (s *CoreSession) ResolveCwd(cwd string) (string, error) {
	rootReal, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return "", fmt.Errorf("work root: %w", err)
	}
	target := rootReal
	if cwd != "" {
		if filepath.IsAbs(cwd) {
			target = filepath.Clean(cwd)
		} else {
			target = filepath.Join(rootReal, cwd)
		}
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	rel, err := filepath.Rel(rootReal, targetReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("cwd must be under the executor work root")
	}
	return targetReal, nil
}

// Env builds a bash or process environment: baseEnv from the executor, the
// request env (bounded, loader variables denied), then CoreOptions.Env.
func (c *Core) Env(explicit map[string]string) ([]string, error) {
	if len(explicit) > maxEnvEntries {
		return nil, fmt.Errorf("env exceeds %d entries", maxEnvEntries)
	}
	total := 0
	values := map[string]string{}
	for _, key := range baseEnv {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range explicit {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.Contains(value, "\x00") {
			return nil, fmt.Errorf("env key %q is invalid", key)
		}
		if deniedEnv[strings.ToUpper(key)] {
			return nil, fmt.Errorf("env %s is not allowed", key)
		}
		total += len(key) + len(value)
		values[key] = value
	}
	if total > maxEnvTotalBytes {
		return nil, fmt.Errorf("env exceeds %d bytes", maxEnvTotalBytes)
	}
	for key, value := range c.opts.Env {
		values[key] = value
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env, nil
}

type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[truncated]"
	}
	return b.buf.String()
}

type shellExecution struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

// bash runs one command with a login shell in its own process group. A
// non-zero exit is a succeeded tool call whose content reports exit_code.
func (c *Core) bash(ctx context.Context, session *CoreSession, params ToolCallParams, input ToolInput) ToolResultParams {
	if input.Command == "" {
		return failedToolResult(params, "bash requires command")
	}
	if input.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(input.TimeoutMS)*time.Millisecond)
		defer cancel()
	} else if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.BashTimeout)
		defer cancel()
	}
	execution, err := c.runShell(ctx, session, input)
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return ToolResultParams{Outcome: ToolOutcomeCanceled, IsError: true,
			Content: []ContentBlock{{Type: ContentBlockTypeText, Text: "tool was canceled"}}}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failedToolResult(params, "tool timed out")
	case err != nil:
		return failedToolResult(params, err.Error())
	}
	return c.bashResult(ctx, session, params, execution)
}

func (c *Core) runShell(ctx context.Context, session *CoreSession, input ToolInput) (shellExecution, error) {
	cwd, err := session.ResolveCwd(input.Cwd)
	if err != nil {
		return shellExecution{}, err
	}
	env, err := c.Env(input.Env)
	if err != nil {
		return shellExecution{}, err
	}
	cmd := shellCommand(ctx, input.Command)
	cmd.Dir = cwd
	cmd.Env = env
	stdout := &cappedBuffer{limit: c.opts.MaxOutputBytes}
	stderr := &cappedBuffer{limit: c.opts.MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	execution := shellExecution{
		Stdout: stdout.buf.String(), Stderr: stderr.buf.String(), Truncated: stdout.truncated || stderr.truncated,
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case ctx.Err() != nil:
		return execution, ctx.Err()
	case errors.As(runErr, &exitErr):
		execution.ExitCode = exitErr.ExitCode()
	default:
		return execution, fmt.Errorf("spawn bash: %w", runErr)
	}
	return execution, nil
}

// bashResult returns stdout, stderr and exit_code blocks inline. When they do
// not fit, the full execution JSON is uploaded as an artifact first and the
// result carries a bounded preview plus the artifact reference (v1.1
// §6.5/§6.7); without an artifact store the output is truncated.
func (c *Core) bashResult(ctx context.Context, session *CoreSession, params ToolCallParams, execution shellExecution) ToolResultParams {
	content := make([]ContentBlock, 0, 3)
	if execution.Stdout != "" {
		content = append(content, ContentBlock{Type: ContentBlockTypeText, Text: execution.Stdout})
	}
	if execution.Stderr != "" {
		content = append(content, ContentBlock{Type: ContentBlockTypeText, Text: execution.Stderr})
	}
	content = append(content, ContentBlock{Type: ContentBlockTypeText, Text: fmt.Sprintf("exit_code: %d", execution.ExitCode)})
	size := 0
	for _, block := range content {
		size += len(block.Text)
	}
	inline := c.opts.InlineResultBytes
	if size <= inline {
		return ToolResultParams{Outcome: ToolOutcomeSucceeded, Content: content}
	}
	if !c.opts.UploadArtifacts {
		return ToolResultParams{Outcome: ToolOutcomeSucceeded, Content: []ContentBlock{
			{Type: ContentBlockTypeText, Text: truncate(execution.Stdout, inline/2)},
			{Type: ContentBlockTypeText, Text: truncate(execution.Stderr, inline/4)},
			{Type: ContentBlockTypeText, Text: fmt.Sprintf("exit_code: %d (output truncated to fit the result; %d bytes total)",
				execution.ExitCode, len(execution.Stdout)+len(execution.Stderr))},
		}}
	}
	full, err := json.Marshal(execution)
	if err != nil {
		return failedToolResult(params, "encode bash result: "+err.Error())
	}
	ref, err := session.client.UploadArtifact(ctx, session.key.session, "bash-"+params.ToolCallID+".json", "application/json", full)
	if err != nil {
		return failedToolResult(params, "upload bash result artifact: "+err.Error())
	}
	return ToolResultParams{
		Outcome: ToolOutcomeSucceeded,
		Content: []ContentBlock{
			{Type: ContentBlockTypeText, Text: truncate(execution.Stdout, previewBytes)},
			{Type: ContentBlockTypeText, Text: fmt.Sprintf("exit_code: %d (output truncated; full result in artifact %s, %d bytes)",
				execution.ExitCode, ref.ArtifactID, ref.SizeBytes)},
			NewArtifactContentBlock(ref),
		},
		Artifacts: []ArtifactRef{ref},
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (c *Core) read(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	path, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return ToolResultParams{}, err
	}
	defer file.Close()
	offset, limit := int64(1), int64(-1)
	if input.Offset > 0 {
		offset = input.Offset
	}
	if input.Limit > 0 {
		limit = input.Limit
	}
	out := &cappedBuffer{limit: c.opts.InlineResultBytes}
	reader := bufio.NewReader(file)
	for line := int64(1); limit != 0; line++ {
		text, err := reader.ReadString('\n')
		if line >= offset && text != "" {
			_, _ = out.Write([]byte(text))
			if limit > 0 {
				limit--
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ToolResultParams{}, err
		}
	}
	return textResult(false, out.String()), nil
}

func (c *Core) write(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	path, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ToolResultParams{}, err
	}
	if err := os.WriteFile(path, []byte(input.Content), 0o600); err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, fmt.Sprintf("wrote %d bytes", len(input.Content))), nil
}

func (c *Core) edit(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	path, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if input.OldString == "" {
		return ToolResultParams{}, errors.New("edit requires old_string and new_string")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if count := strings.Count(string(raw), input.OldString); count != 1 {
		return textResult(true, fmt.Sprintf("old_string must match exactly once; matched %d times", count)), nil
	}
	updated := strings.Replace(string(raw), input.OldString, input.NewString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, "edited "+filepath.Base(path)), nil
}

func (c *Core) glob(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == "" {
		return ToolResultParams{}, errors.New("glob requires pattern")
	}
	base, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	pattern, err := session.resolve(filepath.Join(base, input.Pattern))
	if err != nil {
		return ToolResultParams{}, err
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ToolResultParams{}, err
	}
	for i, match := range matches {
		if rel, err := filepath.Rel(session.workspace, match); err == nil {
			matches[i] = rel
		}
	}
	return textResult(false, strings.Join(matches, "\n")), nil
}

func (c *Core) grep(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == "" {
		return ToolResultParams{}, errors.New("grep requires pattern")
	}
	expression, err := regexp.Compile(input.Pattern)
	if err != nil {
		return ToolResultParams{}, err
	}
	root, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || len(matches) >= maxGrepMatches {
			return nil
		}
		if input.Glob != "" {
			if ok, _ := filepath.Match(input.Glob, entry.Name()); !ok {
				return nil
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		rel, _ := filepath.Rel(session.workspace, path)
		scanner := bufio.NewScanner(file)
		for line := 1; scanner.Scan() && len(matches) < maxGrepMatches; line++ {
			if expression.Match(scanner.Bytes()) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, line, scanner.Text()))
			}
		}
		return nil
	})
	if walkErr != nil {
		return ToolResultParams{}, walkErr
	}
	return textResult(false, strings.Join(matches, "\n")), nil
}

// --- stdio MCP supervisor ---

// superviseMCP runs one stdio MCP server per (brain, session, server_key)
// and bridges it to an MCP channel opened by client: each channel frame is
// one line on the server's stdin, each stdout line one channel frame. When
// either side ends, both are torn down and restarted with backoff, so every
// channel talks to a fresh server; after repeated failures the server is
// reported as mcp_unavailable. firstDone is called after the first attempt.
func (c *Core) superviseMCP(client *Client, session *CoreSession, server MCPServerConfig, firstDone func()) {
	var once sync.Once
	defer once.Do(firstDone)
	failures := 0
	for session.ctx.Err() == nil {
		started := time.Now()
		err := c.runMCPOnce(client, session, server, func() { once.Do(firstDone) })
		once.Do(firstDone)
		if session.ctx.Err() != nil {
			return
		}
		if time.Since(started) > coreMCPHealthyRun {
			failures = 0
		}
		failures++
		c.log(LogWarn, "mcp server "+server.ServerKey+" ended", err)
		if failures >= coreMCPMaxConsecutiveFailure {
			c.status(client, session, SessionStatusError, "mcp_unavailable: "+server.ServerKey)
			return
		}
		select {
		case <-session.ctx.Done():
			return
		case <-time.After(time.Duration(1<<failures) * 100 * time.Millisecond):
		}
	}
}

func (c *Core) runMCPOnce(client *Client, session *CoreSession, server MCPServerConfig, opened func()) error {
	transport := server.Transport
	command, err := exec.LookPath(transport.Command)
	if err != nil {
		return err
	}
	if transport.CommandSHA256 != "" {
		if err := verifySHA256(command, transport.CommandSHA256); err != nil {
			return err
		}
	}
	// The harness chose the command, so an absolute cwd is honored; a
	// relative one is inside the session workspace.
	dir := session.workspace
	if transport.Cwd != "" {
		dir = transport.Cwd
		if !filepath.IsAbs(dir) {
			if dir, err = session.resolve(transport.Cwd); err != nil {
				return err
			}
		}
	}
	ctx, cancel := context.WithCancel(session.ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, transport.Args...)
	setProcessGroup(cmd)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for name, value := range transport.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &cappedBuffer{limit: 8 * 1024}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
		if stderr.buf.Len() > 0 {
			c.log(LogDebug, "mcp server "+server.ServerKey+" stderr: "+stderr.String(), nil)
		}
	}()

	channel, err := client.OpenMCPChannel(ctx, session.key.session, server.ServerKey)
	if err != nil {
		return err
	}
	defer channel.Close()
	opened()

	errs := make(chan error, 2)
	go func() {
		for {
			frame, err := channel.Read(ctx)
			if err != nil {
				errs <- fmt.Errorf("mcp channel: %w", err)
				return
			}
			if _, err := stdin.Write(append(bytes.TrimRight(frame, "\r\n"), '\n')); err != nil {
				errs <- fmt.Errorf("mcp stdin: %w", err)
				return
			}
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			if err := channel.Write(ctx, line); err != nil {
				errs <- fmt.Errorf("mcp channel: %w", err)
				return
			}
		}
		errs <- fmt.Errorf("mcp server exited: %v", scanner.Err())
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func verifySHA256(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(got, strings.TrimPrefix(want, "sha256:")) {
		return fmt.Errorf("mcp command %s sha256 mismatch", path)
	}
	return nil
}
