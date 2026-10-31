package bmaexecutor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultCoreMaxSessions       = 16
	DefaultCoreMaxMCPServers     = 4
	DefaultCoreBashTimeout       = 10 * time.Minute
	DefaultCoreMaxOutputBytes    = 64 * 1024
	coreMCPMaxConsecutiveFailure = 5
	coreMCPHealthyRun            = 30 * time.Second
	maxGrepMatches               = 200
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// CoreOptions configures the execution core.
type CoreOptions struct {
	// WorkRoot holds every session workspace: WorkRoot/<brain>/<session>.
	WorkRoot string
	// Workspace overrides the per-session directory (for example a desktop
	// app mapping a brain to a project folder). File tools are confined to it.
	Workspace func(brain, sessionID string) string
	// MaxSessions bounds active sessions across all attached brains;
	// MaxSessionsPerBrain bounds one brain (defaults to MaxSessions). A full
	// core answers session.assign with executor_busy (-32009, retryable).
	MaxSessions         int
	MaxSessionsPerBrain int
	// MaxMCPServersPerSession bounds session.assign.mcp_servers (default 4).
	MaxMCPServersPerSession int
	BashTimeout             time.Duration
	// MaxOutputBytes caps each inline output block (default 64 KiB).
	MaxOutputBytes int
	OnLog          func(LogEvent)
}

// Core is the executor's execution core ("hands"): bash with process-group
// cancellation, file tools, a stdio MCP supervisor and per-session cleanup.
// It is independent of who drives it: Attach it to one Client per harness
// ("brain"), whether that is the BMA cloud or a local harness on loopback,
// and every brain gets the same protocol behavior.
//
// Isolation: sessions are keyed by (brain, session_id), so two brains may use
// the same session id without seeing each other; a tool.call can only reach
// sessions its own brain assigned, and each session has its own workspace,
// processes and MCP servers. Capacity is shared: MaxSessions across brains,
// MaxSessionsPerBrain per brain. Bash is not a sandbox: every brain runs as
// the executor's OS user.
type Core struct {
	opts CoreOptions

	mu       sync.Mutex
	brains   map[string]*Client
	sessions map[coreKey]*coreSession
	closed   bool
}

type coreKey struct{ brain, session string }

type coreSession struct {
	key       coreKey
	workspace string
	ctx       context.Context
	cancel    context.CancelFunc

	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// enter registers work that stop must wait for; false once stopping.
func (s *coreSession) enter() bool {
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
	return &Core{opts: opts, brains: map[string]*Client{}, sessions: map[coreKey]*coreSession{}}, nil
}

// Capabilities is what an attached Client should advertise.
func (c *Core) Capabilities() ExecutorCapabilities {
	return ExecutorCapabilities{
		Shell: &ShellCapability{Enabled: true},
		Files: &FilesCapability{Enabled: true, ReadRoots: []string{c.opts.WorkRoot}, WriteRoots: []string{c.opts.WorkRoot}},
		MCP:   &MCPCapability{Stdio: true, MaxServers: int64(c.opts.MaxMCPServersPerSession)},
	}
}

// Attach serves session.assign, session.stop and tool.call for one brain on
// client. Call it before client.Start. brain names the harness ("cloud",
// "local", ...) and must be unique per core.
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
	c.mu.Unlock()
	client.OnSessionAssign(func(ctx context.Context, params SessionAssignParams) (SessionAssignResult, error) {
		return c.assign(brain, client, params)
	})
	client.OnSessionStop(func(ctx context.Context, params SessionStopParams) (SessionStopResult, error) {
		stopped := c.stop(coreKey{brain, params.SessionID})
		if stopped {
			go func() {
				if _, err := client.SessionEnded(context.Background(), SessionEndedParams{SessionID: params.SessionID, Reason: params.Reason}); err != nil {
					c.log(LogWarn, "session.ended for "+params.SessionID+" was not accepted", err)
				}
			}()
		}
		return SessionStopResult{Stopped: true}, nil
	})
	client.OnToolCall(func(ctx context.Context, params ToolCallParams) (ToolResultParams, error) {
		return c.toolCall(ctx, brain, params), nil
	})
	return nil
}

// Close stops every session of every brain.
func (c *Core) Close() {
	c.mu.Lock()
	c.closed = true
	keys := make([]coreKey, 0, len(c.sessions))
	for key := range c.sessions {
		keys = append(keys, key)
	}
	c.mu.Unlock()
	for _, key := range keys {
		c.stop(key)
	}
}

func busy(message string) error {
	retryable := true
	return &RPCError{Code: JSONRPCErrorCodeExecutorBusy, Message: message, Data: &JSONRPCErrorData{Retryable: &retryable}}
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
	if len(params.MCPServers) > c.opts.MaxMCPServersPerSession {
		return SessionAssignResult{}, &RPCError{Code: JSONRPCErrorCodeInvalidParams, Message: "too many mcp_servers"}
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
	ctx, cancel := context.WithCancel(context.Background())
	session := &coreSession{key: key, workspace: c.workspace(brain, params.SessionID), ctx: ctx, cancel: cancel}
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
func (c *Core) prepare(client *Client, session *coreSession, params SessionAssignParams) {
	var first sync.WaitGroup
	for _, server := range params.MCPServers {
		if server.ConnectionOrigin != MCPConnectionOriginEnvironment {
			continue // the harness connects service-origin servers itself
		}
		if server.Transport.Type != MCPTransportTypeStdio || server.Transport.Command == nil {
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

func (c *Core) status(client *Client, session *coreSession, status SessionStatus, detail string) {
	_ = client.Notify(session.ctx, MethodSessionStatus, SessionStatusParams{SessionID: session.key.session, Status: status, Detail: &detail})
}

// stop cancels the session (killing its process groups and MCP servers) and
// waits for them. It reports whether the session existed.
func (c *Core) stop(key coreKey) bool {
	c.mu.Lock()
	session := c.sessions[key]
	delete(c.sessions, key)
	c.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	session.stopped = true
	session.mu.Unlock()
	session.cancel()
	session.wg.Wait()
	return true
}

func (c *Core) session(key coreKey) *coreSession {
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

	started := time.Now()
	var result ToolResultParams
	var err error
	switch params.Tool {
	case ToolNameBash:
		result, err = c.bash(ctx, session, params.Input)
	case ToolNameRead:
		result, err = c.read(session, params.Input)
	case ToolNameWrite:
		result, err = c.write(session, params.Input)
	case ToolNameEdit:
		result, err = c.edit(session, params.Input)
	case ToolNameGlob:
		result, err = c.glob(session, params.Input)
	case ToolNameGrep:
		result, err = c.grep(session, params.Input)
	default:
		err = fmt.Errorf("capability_unavailable: tool %q is not supported", params.Tool)
	}
	if err != nil {
		return failedToolResult(params, err.Error())
	}
	duration := time.Since(started).Milliseconds()
	result.DurationMS = &duration
	return result
}

func textResult(isError bool, texts ...string) ToolResultParams {
	result := ToolResultParams{Outcome: ToolOutcomeSucceeded, IsError: isError}
	for _, text := range texts {
		text := text
		result.Content = append(result.Content, ContentBlock{Type: ContentBlockTypeText, Text: &text})
	}
	return result
}

// resolve confines path to the session workspace.
func (s *coreSession) resolve(path *string) (string, error) {
	if path == nil || *path == "" {
		return s.workspace, nil
	}
	resolved := *path
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(s.workspace, resolved)
	}
	resolved = filepath.Clean(resolved)
	rel, err := filepath.Rel(s.workspace, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the session workspace", *path)
	}
	return resolved, nil
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

func (c *Core) bash(ctx context.Context, session *coreSession, input ToolInput) (ToolResultParams, error) {
	if input.Command == nil || *input.Command == "" {
		return ToolResultParams{}, errors.New("bash requires command")
	}
	dir, err := session.resolve(input.Cwd)
	if err != nil {
		return ToolResultParams{}, err
	}
	timeout := c.opts.BashTimeout
	if input.TimeoutMS != nil && *input.TimeoutMS > 0 {
		timeout = time.Duration(*input.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := shellCommand(ctx, *input.Command)
	cmd.Dir = dir
	stdout := &cappedBuffer{limit: c.opts.MaxOutputBytes}
	stderr := &cappedBuffer{limit: c.opts.MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		// Canceled, stopped or timed out: the process group is gone. The
		// client reports outcome canceled for tool.cancel.
		return ToolResultParams{}, fmt.Errorf("bash interrupted: %w", ctx.Err())
	}
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return ToolResultParams{}, runErr
		}
		exitCode = exitErr.ExitCode()
	}
	texts := []string{stdout.String()}
	if stderr.buf.Len() > 0 {
		texts = append(texts, stderr.String())
	}
	texts = append(texts, fmt.Sprintf("exit_code: %d", exitCode))
	return textResult(exitCode != 0, texts...), nil
}

func (c *Core) read(session *coreSession, input ToolInput) (ToolResultParams, error) {
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
	if input.Offset != nil && *input.Offset > 0 {
		offset = *input.Offset
	}
	if input.Limit != nil && *input.Limit > 0 {
		limit = *input.Limit
	}
	out := &cappedBuffer{limit: c.opts.MaxOutputBytes}
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

func (c *Core) write(session *coreSession, input ToolInput) (ToolResultParams, error) {
	path, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if input.Content == nil {
		return ToolResultParams{}, errors.New("write requires content")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return ToolResultParams{}, err
	}
	if err := os.WriteFile(path, []byte(*input.Content), 0o600); err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, fmt.Sprintf("wrote %d bytes", len(*input.Content))), nil
}

func (c *Core) edit(session *coreSession, input ToolInput) (ToolResultParams, error) {
	path, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if input.OldString == nil || input.NewString == nil || *input.OldString == "" {
		return ToolResultParams{}, errors.New("edit requires old_string and new_string")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ToolResultParams{}, err
	}
	if count := strings.Count(string(raw), *input.OldString); count != 1 {
		return textResult(true, fmt.Sprintf("old_string must match exactly once; matched %d times", count)), nil
	}
	updated := strings.Replace(string(raw), *input.OldString, *input.NewString, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, "edited "+filepath.Base(path)), nil
}

func (c *Core) glob(session *coreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == nil || *input.Pattern == "" {
		return ToolResultParams{}, errors.New("glob requires pattern")
	}
	base, err := session.resolve(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	pattern, err := session.resolve(stringPtr(filepath.Join(base, *input.Pattern)))
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

func (c *Core) grep(session *coreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == nil || *input.Pattern == "" {
		return ToolResultParams{}, errors.New("grep requires pattern")
	}
	expression, err := regexp.Compile(*input.Pattern)
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
		if input.Glob != nil && *input.Glob != "" {
			if ok, _ := filepath.Match(*input.Glob, entry.Name()); !ok {
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
func (c *Core) superviseMCP(client *Client, session *coreSession, server MCPServerConfig, firstDone func()) {
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

func (c *Core) runMCPOnce(client *Client, session *coreSession, server MCPServerConfig, opened func()) error {
	transport := server.Transport
	command, err := exec.LookPath(*transport.Command)
	if err != nil {
		return err
	}
	if transport.CommandSHA256 != nil && *transport.CommandSHA256 != "" {
		if err := verifySHA256(command, *transport.CommandSHA256); err != nil {
			return err
		}
	}
	dir, err := session.resolve(transport.Cwd)
	if err != nil {
		return err
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
