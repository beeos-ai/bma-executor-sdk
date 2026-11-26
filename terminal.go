package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// terminal.* (executor contract v1.1): PTYs the platform attaches one
// consumer at a time to, each attach over its own outbound host socket
// (bma.terminal.v1, terminal_host.go). This file is the terminals and their
// rules; terminal_host.go moves the frames and terminal_{unix,windows}.go
// start the PTY.

const (
	terminalRingBytes   = 1 << 20  // output kept for resume (the contract's reference size)
	terminalChunkBytes  = 32 << 10 // largest output frame
	terminalWindowBytes = 256 << 10
	// MaxTerminals is how many live terminals one executor serves.
	MaxTerminals       = 16
	terminalCloseGrace = 2 * time.Second
)

// Terminal error codes (TerminalErrorCode).
const (
	terminalCodeWriterBusy   = "terminal_writer_busy"
	terminalCodeInvalidFrame = "terminal_invalid_frame"
	terminalCodeInternal     = "terminal_internal_error"
	terminalCodeStaleWriter  = "terminal_stale_writer"
	terminalCodeReadOnly     = "terminal_read_only"
)

func terminalError(rpc int, code, msg string) *RPCError {
	return &RPCError{Code: rpc, Message: msg, Data: &JSONRPCErrorData{TerminalCode: code}}
}

// TerminalLaunchRequest is one terminal about to start: the command the
// executor would run and where.
type TerminalLaunchRequest struct {
	TerminalID string
	// Kind is human or agent.
	Kind string
	// Argv is the requested argv, else the requested or default shell.
	Argv []string
	// Dir is the absolute directory it would start in: the requested cwd,
	// else TerminalOptions.Dir.
	Dir string
	// Env is the complete environment it would get.
	Env []string
}

// TerminalLaunchFunc is called once per new PTY (terminal.open of a new or
// ended terminal_id), before it is spawned. It may choose the directory,
// wrap argv in a sandbox command and change the environment; an empty field
// of the LaunchSpec keeps the request's. Returning an error refuses the open:
// an *RPCError (NewReasonError) is answered as is, any other error as
// resource_unavailable with terminal_internal_error.
type TerminalLaunchFunc func(ctx context.Context, req TerminalLaunchRequest) (LaunchSpec, error)

// TerminalOptions configures NewTerminals.
type TerminalOptions struct {
	// Env is the base environment of every terminal. Nil: this process's.
	Env []string
	// Dir is the directory a terminal starts in when the request names none.
	// Empty: HOME of Env.
	Dir string
	// RunAs, when set, is the OS identity the PTY's process runs as. Unix only.
	RunAs *RunAs
	// DenyEnv reports variables a terminal.open env entry may not set; they
	// are dropped. Nil allows every name.
	DenyEnv func(name string) bool
	// Launch runs before each new PTY starts; see TerminalLaunchFunc. Nil
	// starts the request's command as is.
	Launch TerminalLaunchFunc
	// OnLog receives a warning when a PTY's process cannot be waited for.
	OnLog func(LogEvent)
}

// Terminals serves terminal.open, terminal.attach and terminal.close on a
// machine-scoped executor: PTYs (ConPTY on Windows) that outlive a client,
// keep 1 MiB of output for a resume, and stream it over the host socket each
// terminal.attach names, one writer at a time. Register Capability on the
// Core and Close it when the executor stops.
type Terminals struct {
	opts TerminalOptions
	dial terminalHostDialer

	mu   sync.Mutex
	byID map[string]*terminal
}

// NewTerminals returns the terminal capability's state.
func NewTerminals(opts TerminalOptions) *Terminals {
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	return &Terminals{opts: opts, dial: dialTerminalHost, byID: map[string]*terminal{}}
}

// Capability advertises terminal (kinds human and agent) and serves its methods.
func (s *Terminals) Capability() Capability {
	return Capability{
		Advertise: func(c *Capabilities) {
			c.Terminal = &TerminalCapability{Enabled: true, Kinds: []string{"human", "agent"}, MaxSessions: MaxTerminals}
		},
		Methods: map[string]Handler{
			MethodTerminalOpen:   s.open,
			MethodTerminalAttach: s.attach,
			MethodTerminalClose:  s.close,
		},
	}
}

// ptyProcess is a process running on a PTY: its master end for output and
// input, its size, and its process (group or job).
type ptyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	resize(rows, cols uint16) error
	pid() int
	// hangup asks the process to end (SIGHUP to its group; closing the
	// pseudo console on Windows); kill ends it.
	hangup()
	kill()
	// wait blocks until the process ended and its output was drained; code
	// is nil when a signal ended it (reason "signaled").
	wait() (code *int, reason string, err error)
	close()
}

// terminal is one PTY under a terminal_id. Invariants: output sequence
// numbers are byte offsets since the PTY started, [start, end) is buffered;
// at most one attachment; writerEpoch grows with every attach; a recreated
// PTY under the same id has a larger epoch.
type terminal struct {
	id    string
	spec  TerminalOpenParams // what open must match (rows/cols excluded)
	epoch int64

	mu          sync.Mutex
	changed     chan struct{} // closed and replaced on every state change
	pty         ptyProcess
	pid         int
	rows, cols  uint16
	createdAt   time.Time
	ring        []byte
	start, end  int64
	exited      bool
	exitCode    *int
	exitReason  string
	closed      string // close reason once closed
	writerEpoch uint64
	attached    *terminalAttachment
}

func (t *terminal) wake() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// appendOutput adds PTY bytes, dropping the oldest beyond the ring size.
func (t *terminal) appendOutput(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ring = append(t.ring, p...)
	t.end += int64(len(p))
	if over := len(t.ring) - terminalRingBytes; over > 0 {
		t.ring = slices.Delete(t.ring, 0, over)
		t.start += int64(over)
	}
	t.wake()
}

// output returns the bytes from seq on, at most n; ok is false when seq is
// before the buffer (a gap).
func (t *terminal) output(seq int64, n int) (data []byte, ok bool) {
	if seq < t.start {
		return nil, false
	}
	from := int(seq - t.start)
	to := min(len(t.ring), from+n)
	return slices.Clone(t.ring[from:to]), true
}

func (t *terminal) live() bool { return !t.exited && t.closed == "" }

// attach takes the single attachment for a consumer that resumes at fromSeq.
func (t *terminal) attach(epoch, fromSeq int64, canWrite bool) (*terminalAttachment, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case epoch != t.epoch:
		return nil, terminalError(CodeConflict, terminalCodeInvalidFrame,
			fmt.Sprintf("terminal epoch is %d, not %d", t.epoch, epoch))
	case t.closed != "":
		return nil, terminalError(CodeNotFound, terminalCodeInvalidFrame, "terminal is closed")
	case t.attached != nil:
		return nil, terminalError(CodeConflict, terminalCodeWriterBusy, "another client holds the terminal write lease")
	case fromSeq > t.end || fromSeq < 0:
		return nil, terminalError(CodeInvalidParams, terminalCodeInvalidFrame, "attach from_seq is beyond the latest sequence")
	}
	t.writerEpoch++
	a := &terminalAttachment{t: t, writerEpoch: t.writerEpoch, canWrite: canWrite, sent: fromSeq, acked: fromSeq}
	t.attached = a
	t.wake()
	return a, nil
}

func (t *terminal) detach(a *terminalAttachment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attached == a {
		t.attached = nil
		t.wake()
	}
}

// terminalAttachment is the one consumer of a terminal: its writer lease and
// its output position.
type terminalAttachment struct {
	t           *terminal
	writerEpoch uint64
	canWrite    bool
	sent, acked int64 // guarded by t.mu
}

// checkInput applies the writer lease to an input frame.
func (a *terminalAttachment) checkInput(epoch *uint64) string {
	switch {
	case !a.canWrite:
		return terminalCodeReadOnly
	case epoch != nil && *epoch != a.writerEpoch:
		return terminalCodeStaleWriter
	}
	return ""
}

func (a *terminalAttachment) ack(seq int64) {
	t := a.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq > a.acked && seq <= a.sent {
		a.acked = seq
		t.wake()
	}
}

func sameTerminalSpec(a, b TerminalOpenParams) bool {
	return a.Kind == b.Kind && a.Shell == b.Shell && slices.Equal(a.Argv, b.Argv) && a.Cwd == b.Cwd &&
		maps.Equal(a.Env, b.Env)
}

func (s *Terminals) open(ctx context.Context, req Request) (any, error) {
	var p TerminalOpenParams
	err := json.Unmarshal(req.Params, &p)
	if err != nil || p.TerminalID == "" || (p.Kind != "human" && p.Kind != "agent") {
		return nil, &RPCError{Code: CodeInvalidParams,
			Message: "terminal.open needs terminal_id and kind human or agent"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := int64(1)
	if t, ok := s.byID[p.TerminalID]; ok {
		t.mu.Lock()
		live := t.live()
		t.mu.Unlock()
		if live {
			if !sameTerminalSpec(t.spec, p) {
				return nil, terminalError(CodeConflict, terminalCodeInvalidFrame, "terminal_id is open with a different spec")
			}
			return t.result(), nil
		}
		epoch = t.epoch + 1
	} else if s.liveCount() >= MaxTerminals {
		return nil, &RPCError{Code: CodeResourceUnavailable, Message: "terminal session limit reached",
			Data: &JSONRPCErrorData{Retryable: true}}
	}
	t, err := s.start(ctx, p, epoch)
	if err != nil {
		if rpc, ok := err.(*RPCError); ok {
			return nil, rpc
		}
		return nil, &RPCError{Code: CodeResourceUnavailable, Message: err.Error(),
			Data: &JSONRPCErrorData{TerminalCode: terminalCodeInternal}}
	}
	s.byID[p.TerminalID] = t
	return t.result(), nil
}

func (s *Terminals) liveCount() int {
	n := 0
	for _, t := range s.byID {
		t.mu.Lock()
		if t.live() {
			n++
		}
		t.mu.Unlock()
	}
	return n
}

func (t *terminal) result() TerminalOpenResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TerminalOpenResult{TerminalID: t.id, Epoch: t.epoch, PID: t.pid, Rows: t.rows, Cols: t.cols,
		EarliestSeq: t.start, LatestSeq: t.end, CreatedAt: t.createdAt.UTC().Format(time.RFC3339Nano)}
}

// start runs the PTY: the requested argv or shell, else the default shell,
// in the requested directory, else TerminalOptions.Dir or HOME, in a UTF-8
// locale, as the Launch hook changes it.
func (s *Terminals) start(ctx context.Context, p TerminalOpenParams, epoch int64) (*terminal, error) {
	env := slices.Clone(s.opts.Env)
	argv := p.Argv
	if len(argv) == 0 {
		argv = defaultShell(p.Shell, env)
	}
	dir := p.Cwd
	if dir == "" {
		dir = s.opts.Dir
	}
	if dir == "" {
		dir = lastEnv(env, "HOME")
	}
	env = append(env, "TERM=xterm-256color")
	for k, v := range p.Env {
		if s.opts.DenyEnv == nil || !s.opts.DenyEnv(k) {
			env = append(env, k+"="+v)
		}
	}
	env = withUTF8Locale(env)
	if dir != "" {
		env = append(slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "PWD=") }), "PWD="+dir)
	}
	if s.opts.Launch != nil {
		spec, err := s.opts.Launch(ctx, TerminalLaunchRequest{TerminalID: p.TerminalID, Kind: p.Kind,
			Argv: slices.Clone(argv), Dir: dir, Env: slices.Clone(env)})
		if err != nil {
			return nil, err
		}
		if len(spec.Argv) > 0 {
			argv = spec.Argv
		}
		if spec.Dir != "" {
			dir = spec.Dir
		}
		if spec.Env != nil {
			env = spec.Env
		}
	}
	rows, cols := p.Rows, p.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	proc, err := startTerminalPTY(argv, dir, env, rows, cols, s.opts.RunAs)
	if err != nil {
		return nil, fmt.Errorf("start terminal: %w", err)
	}
	t := &terminal{id: p.TerminalID, spec: p, epoch: epoch, changed: make(chan struct{}), pty: proc,
		pid: proc.pid(), rows: rows, cols: cols, createdAt: time.Now()}
	go t.pump(s.opts.OnLog)
	return t, nil
}

// lastEnv is the last value of name in env ("" when unset).
func lastEnv(env []string, name string) string {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// withUTF8Locale leaves a UTF-8 character locale alone; otherwise it drops
// LC_ALL and LC_CTYPE (which would override LANG) and sets LANG=C.UTF-8,
// which glibc, musl and macOS ship. A service-started executor has no
// locale, and under the C locale readline echoes multibyte input as octal
// escapes.
func withUTF8Locale(env []string) []string {
	var effective string
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if effective = lastEnv(env, name); effective != "" {
			break
		}
	}
	lower := strings.ToLower(effective)
	if strings.Contains(lower, "utf-8") || strings.Contains(lower, "utf8") {
		return env
	}
	env = slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LC_CTYPE=") || strings.HasPrefix(kv, "LANG=")
	})
	return append(env, "LANG=C.UTF-8")
}

// pump copies PTY output into the ring until the PTY ends, then records the exit.
func (t *terminal) pump(onLog func(LogEvent)) {
	buf := make([]byte, terminalChunkBytes)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.appendOutput(buf[:n])
		}
		if err != nil {
			break
		}
	}
	code, reason, err := t.pty.wait()
	t.pty.close()
	if err != nil && onLog != nil {
		onLog(LogEvent{Level: LogWarn, Message: "terminal wait failed: " + t.id, Err: err})
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exited, t.exitCode, t.exitReason = true, code, reason
	t.wake()
}

func (s *Terminals) attach(ctx context.Context, req Request) (any, error) {
	var p TerminalAttachParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.TerminalID == "" || p.Host.URL == "" || p.Host.Ticket == "" {
		return nil, &RPCError{Code: CodeInvalidParams,
			Message: "terminal.attach needs terminal_id and host{url,ticket}"}
	}
	s.mu.Lock()
	t := s.byID[p.TerminalID]
	s.mu.Unlock()
	if t == nil {
		return nil, terminalError(CodeNotFound, terminalCodeInvalidFrame, "unknown terminal_id")
	}
	a, err := t.attach(p.Epoch, p.FromSeq, p.CanWrite)
	if err != nil {
		return nil, err
	}
	conn, err := s.dial(ctx, p.Host)
	if err != nil {
		t.detach(a)
		return nil, &RPCError{Code: CodeResourceUnavailable, Message: "terminal host: " + err.Error(),
			Data: &JSONRPCErrorData{Retryable: true, TerminalCode: terminalCodeInternal}}
	}
	if err := serveTerminalAttachment(ctx, conn, a); err != nil {
		t.detach(a)
		return nil, &RPCError{Code: CodeResourceUnavailable, Message: "terminal host: " + err.Error(),
			Data: &JSONRPCErrorData{Retryable: true, TerminalCode: terminalCodeInternal}}
	}
	return TerminalAttachResult{Accepted: true, WriterEpoch: a.writerEpoch}, nil
}

func (s *Terminals) close(_ context.Context, req Request) (any, error) {
	var p TerminalCloseParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.TerminalID == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "terminal.close needs terminal_id"}
	}
	s.mu.Lock()
	t := s.byID[p.TerminalID]
	delete(s.byID, p.TerminalID)
	s.mu.Unlock()
	if t == nil {
		return TerminalCloseResult{Closed: true}, nil
	}
	reason := p.Reason
	if reason == "" {
		reason = "closed"
	}
	return TerminalCloseResult{Closed: true, ExitCode: t.terminate(reason)}, nil
}

// terminate closes the terminal: hang up its process, kill it after the
// grace period, and wait for the exit.
func (t *terminal) terminate(reason string) *int {
	t.mu.Lock()
	if t.closed == "" {
		t.closed = reason
		t.wake()
	}
	exited, changed := t.exited, t.changed
	t.mu.Unlock()
	if !exited {
		t.pty.hangup()
		deadline := time.After(terminalCloseGrace)
		for !exited {
			select {
			case <-changed:
			case <-deadline:
				t.pty.kill()
				deadline = nil
			}
			t.mu.Lock()
			exited, changed = t.exited, t.changed
			t.mu.Unlock()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exitCode
}

// Close ends every terminal (executor shutdown).
func (s *Terminals) Close() {
	s.mu.Lock()
	all := make([]*terminal, 0, len(s.byID))
	for id, t := range s.byID {
		all = append(all, t)
		delete(s.byID, id)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range all {
		wg.Add(1)
		go func() { defer wg.Done(); t.terminate("shutdown") }()
	}
	wg.Wait()
}
