package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Process limits (v1.1 §1.3, §3). They match DefaultLimits.
const (
	// MaxProcessesPerSession bounds running processes; ended processes are
	// retained separately (see retainEndedLocked).
	MaxProcessesPerSession = 64
	ProcessBufferBytes     = 1024 * 1024
	// A session keeps ended processes' state and output until it ends so a
	// later turn can still read them. Machine sessions live as long as the
	// machine, so retention is bounded: past maxRetainedOutputBytes the
	// oldest ended processes drop their output (reads report a gap), and
	// past maxRetainedEnded the oldest ended processes are forgotten.
	maxRetainedOutputBytes = 64 * 1024 * 1024
	maxRetainedEnded       = 1024
	maxReadBytes           = 32 * 1024
	maxWriteBytes          = 32 * 1024
	maxWaitMS              = 300000
	maxTimeoutMS           = 86400000
	maxGraceMS             = 30000
	maxArgvEntries         = 256
	maxArgBytes            = 4096
	maxArgvTotalBytes      = 16 * 1024
	maxConcurrentReads     = 4
	maxAppliedWrites       = 1024
	processReasonTimeout   = "timeout"
)

// FileTransferer moves one file for a process.start file_transfer. A failure
// is returned both as the error (the process output, exit code 1) and as
// FileTransferResult.Error, which the harness reports.
type FileTransferer interface {
	TransferFile(ctx context.Context, processID string, transfer ProcessFileTransfer) (FileTransferResult, error)
}

// GitCredentialGranter backs process.start git_credentials: Grant returns the
// environment that lets processID fetch Git credentials while it runs, and a
// release function called when the process ends.
type GitCredentialGranter interface {
	Grant(processID string, env []string) ([]string, func(), error)
}

// ProcessOptions configures the process capability. Nil hooks leave the
// matching capability bit unset, and the core rejects those process.start
// fields with capability_unavailable.
type ProcessOptions struct {
	FileTransfer   FileTransferer
	GitCredentials GitCredentialGranter
}

// NewProcessCapability serves process.start/read/write/interrupt/terminate/list
// and process.status (v1.1 §3) on core sessions: pipes or a PTY, bounded
// cursor-addressed output, idempotent starts and writes, timeouts, process
// group signals, and file transfers and Git credentials through opts. A
// session's processes are terminated when it stops.
func NewProcessCapability(core *Core, opts ProcessOptions) Capability {
	p := &processes{core: core, opts: opts}
	return Capability{
		Advertise: func(capabilities *Capabilities) {
			capabilities.Process = &ProcessCapability{
				Enabled: true, MaxProcesses: MaxProcessesPerSession, BufferBytes: ProcessBufferBytes,
				PTY: ptySupported, FileTransfer: opts.FileTransfer != nil, GitCredentials: opts.GitCredentials != nil,
			}
		},
		SessionScoped: true,
		Methods: map[string]Handler{
			MethodProcessStart:     p.start,
			MethodProcessRead:      p.read,
			MethodProcessWrite:     p.write,
			MethodProcessInterrupt: func(ctx context.Context, req Request) (any, error) { return p.signal(req, false) },
			MethodProcessTerminate: func(ctx context.Context, req Request) (any, error) { return p.signal(req, true) },
			MethodProcessList:      p.list,
		},
		OnSessionEnd: func(session *CoreSession) { p.table(session).terminateAll(2 * time.Second) },
	}
}

type processes struct {
	core  *Core
	opts  ProcessOptions
	reads sync.Map // *Client -> *atomic.Int32
}

type processTableKey struct{}

func (p *processes) table(session *CoreSession) *processTable {
	return session.Value(processTableKey{}, func() any {
		return &processTable{processes: map[string]*process{}}
	}).(*processTable)
}

func decode(raw json.RawMessage, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return invalidParams("invalid params: %v", err)
	}
	return nil
}

func invalidParams(format string, args ...any) *RPCError {
	return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func conflict(reason string) *RPCError {
	return &RPCError{Code: CodeConflict, Message: "conflict", Data: &JSONRPCErrorData{Reason: reason}}
}

func spawnFailed(err error) *RPCError {
	return &RPCError{Code: CodeInternalError, Message: err.Error(), Data: &JSONRPCErrorData{Reason: "spawn_failed"}}
}

// stream is a bounded byte buffer addressed by absolute cursors. When it
// overflows the oldest bytes are dropped and earliest advances (§3.2).
type stream struct {
	buf      []byte
	earliest int64
	eof      bool
}

func (s *stream) next() int64 { return s.earliest + int64(len(s.buf)) }

// release drops the retained bytes; later reads before next() report a gap.
func (s *stream) release() {
	s.earliest = s.next()
	s.buf = nil
}

func (s *stream) append(p []byte) {
	s.buf = append(s.buf, p...)
	if over := len(s.buf) - ProcessBufferBytes; over > 0 {
		s.buf = append([]byte(nil), s.buf[over:]...)
		s.earliest += int64(over)
	}
}

type process struct {
	id        string
	specHash  string
	cmd       *exec.Cmd
	pid       int
	startedAt time.Time

	mu          sync.Mutex
	endedAt     time.Time
	changed     chan struct{}
	stdout      stream
	stderr      stream
	stdin       io.WriteCloser
	stdinClosed bool
	applied     map[string]bool
	state       string
	exitCode    *int
	exitSignal  *string
	stopReason  string
	stderrMode  string
	exited      chan struct{}
	// wait blocks until the process ends and returns its exit code or
	// signal name.
	wait func() (int, string)
	// pty marks io_mode=pty: stdin is the PTY master and cannot be closed.
	pty bool
	// cancel stops an in-executor file transfer (it has no OS process); the
	// argument is the signal name the stop is reported as.
	cancel       func(signal string)
	fileTransfer *FileTransferResult
	// onEnd releases per-process resources (Git credential grant).
	onEnd func()
}

// signal delivers sig to the process group, or stops a file transfer.
func (p *process) signal(sig processSignal) error {
	if p.cancel != nil {
		name := "SIGKILL"
		switch sig {
		case signalInterrupt:
			name = "SIGINT"
		case signalTerminate:
			name = "SIGTERM"
		}
		p.cancel(name)
		return nil
	}
	return signalGroup(p.cmd.Process, sig)
}

// notifyLocked wakes every waiting read. Callers hold p.mu.
func (p *process) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *process) snapshotLocked() ProcessState {
	return ProcessState{
		ProcessID: p.id, State: p.state, PID: p.pid,
		StartedAt: p.startedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:   p.endedAtLocked(), Reason: p.reasonLocked(),
		Cursors:  Cursors{Stdout: p.stdout.next(), Stderr: p.stderr.next()},
		ExitCode: p.exitCode, ExitSignal: p.exitSignal, Stderr: p.stderrMode,
		FileTransfer: p.endedFileTransferLocked(),
	}
}

func (p *process) endedFileTransferLocked() *FileTransferResult {
	if p.endedAt.IsZero() {
		return nil
	}
	return p.fileTransfer
}

func (p *process) endedAtLocked() string {
	if p.endedAt.IsZero() {
		return ""
	}
	return p.endedAt.UTC().Format(time.RFC3339Nano)
}

// reasonLocked explains a failed state; other states carry their exit code
// or signal instead.
func (p *process) reasonLocked() string {
	if p.state != ProcessFailed {
		return ""
	}
	return p.stopReason
}

func (p *process) snapshot() ProcessState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

type processTable struct {
	mu        sync.Mutex
	processes map[string]*process
}

func (t *processTable) get(id string) (*process, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.processes[id]
	if !ok {
		return nil, &RPCError{Code: CodeNotFound, Message: "process not found"}
	}
	return p, nil
}

func validateStart(params ProcessStartParams) error {
	if params.ProcessID == "" || len(params.ProcessID) > 128 {
		return invalidParams("process_id is required")
	}
	if params.IOMode != "" && params.IOMode != IOModePTY {
		return invalidParams("io_mode must be empty or pty")
	}
	if params.FileTransfer != nil {
		if len(params.Argv) != 0 || params.IOMode != "" || params.GitCredentials {
			return invalidParams("file_transfer excludes argv, io_mode and git_credentials")
		}
		transfer := params.FileTransfer
		if transfer.Direction != FileTransferUpload && transfer.Direction != FileTransferDownload {
			return invalidParams("file_transfer.direction must be upload or download")
		}
		if transfer.LocalPath == "" || strings.ContainsRune(transfer.LocalPath, 0) || len(transfer.LocalPath) > maxArgBytes {
			return invalidParams("file_transfer.local_path is invalid")
		}
	} else if len(params.Argv) == 0 || len(params.Argv) > maxArgvEntries {
		return invalidParams("argv must have 1-%d entries", maxArgvEntries)
	}
	total := 0
	for _, arg := range params.Argv {
		if len(arg) > maxArgBytes {
			return invalidParams("argv entry exceeds %d bytes", maxArgBytes)
		}
		total += len(arg)
	}
	if total > maxArgvTotalBytes {
		return invalidParams("argv exceeds %d bytes", maxArgvTotalBytes)
	}
	if params.Stderr != "" && params.Stderr != StderrMerge {
		return invalidParams("stderr must be empty or merge")
	}
	if params.Stdin != "" && params.Stdin != StdinOpen && params.Stdin != StdinClosed {
		return invalidParams("stdin must be open or closed")
	}
	if params.WaitMS < 0 || params.WaitMS > maxWaitMS {
		return invalidParams("wait_ms must be 0-%d", maxWaitMS)
	}
	if params.TimeoutMS < 0 || params.TimeoutMS > maxTimeoutMS {
		return invalidParams("timeout_ms must be 0-%d", maxTimeoutMS)
	}
	return nil
}

func startSpecHash(params ProcessStartParams) string {
	normalized, _ := json.Marshal(struct {
		Argv           []string             `json:"argv"`
		Cwd            string               `json:"cwd"`
		Env            map[string]string    `json:"env"`
		Stdin          string               `json:"stdin"`
		Stderr         string               `json:"stderr"`
		TimeoutMS      int64                `json:"timeout_ms"`
		IOMode         string               `json:"io_mode"`
		FileTransfer   *ProcessFileTransfer `json:"file_transfer"`
		GitCredentials bool                 `json:"git_credentials"`
	}{params.Argv, params.Cwd, params.Env, params.Stdin, params.Stderr, params.TimeoutMS,
		params.IOMode, params.FileTransfer, params.GitCredentials})
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

func (p *processes) start(ctx context.Context, req Request) (any, error) {
	var params ProcessStartParams
	if err := decode(req.Params, &params); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	_ = json.Unmarshal(req.Params, &extra)
	if _, hasPTY := extra["pty"]; hasPTY {
		return nil, invalidParams("pty is not part of process.start; use io_mode pty or terminal.open")
	}
	if err := validateStart(params); err != nil {
		return nil, err
	}
	switch {
	case params.IOMode == IOModePTY && !ptySupported:
		return nil, &RPCError{Code: CodeCapabilityUnavailable, Message: "io_mode pty is not supported on this executor",
			Data: &JSONRPCErrorData{Capability: "process.pty"}}
	case params.FileTransfer != nil && p.opts.FileTransfer == nil:
		return nil, &RPCError{Code: CodeCapabilityUnavailable, Message: "file transfers are not supported by this executor",
			Data: &JSONRPCErrorData{Capability: "process.file_transfer"}}
	case params.GitCredentials && p.opts.GitCredentials == nil:
		return nil, &RPCError{Code: CodeCapabilityUnavailable, Message: "Git credentials are not supported by this executor",
			Data: &JSONRPCErrorData{Capability: "process.git_credentials"}}
	}
	session := req.Session
	table := p.table(session)
	hash := startSpecHash(params)
	table.mu.Lock()
	if existing, ok := table.processes[params.ProcessID]; ok {
		table.mu.Unlock()
		if existing.specHash != hash {
			return nil, conflict("process_id_spec_mismatch")
		}
		return waitState(existing, params.WaitMS), nil
	}
	if table.runningLocked() >= MaxProcessesPerSession {
		table.mu.Unlock()
		return nil, busy("process limit reached")
	}
	proc, err := p.create(session, params)
	if err != nil {
		table.mu.Unlock()
		return nil, err
	}
	proc.specHash = hash
	table.processes[params.ProcessID] = proc
	table.mu.Unlock()
	go p.watch(session, table, proc, time.Duration(params.TimeoutMS)*time.Millisecond)
	return waitState(proc, params.WaitMS), nil
}

// create starts the process for a validated start request. Callers hold the
// table lock.
func (p *processes) create(session *CoreSession, params ProcessStartParams) (*process, error) {
	if params.FileTransfer != nil {
		return p.startTransfer(params), nil
	}
	cwd, err := session.ResolveCwd(params.Cwd)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	env, err := p.core.Env(params.Env)
	if err != nil {
		return nil, invalidParams("%v", err)
	}
	var release func()
	if params.GitCredentials {
		if env, release, err = p.opts.GitCredentials.Grant(params.ProcessID, env); err != nil {
			return nil, spawnFailed(err)
		}
	}
	proc, err := spawn(params, cwd, env)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, spawnFailed(err)
	}
	proc.onEnd = release
	return proc, nil
}

// startTransfer runs a file transfer inside the executor as a process: it
// is listed, read, timed out and terminated like any other, its errors are
// its output and it exits 0 or 1 with the outcome in FileTransfer. A stop
// reports the signal and no outcome.
func (p *processes) startTransfer(params ProcessStartParams) *process {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	proc := &process{
		id: params.ProcessID, startedAt: time.Now(), changed: make(chan struct{}), stdinClosed: true,
		applied: map[string]bool{}, state: ProcessRunning, exited: make(chan struct{}),
		stderrMode: params.Stderr,
	}
	proc.stderr.eof = true
	var stopSignal string
	proc.cancel = func(signal string) {
		proc.mu.Lock()
		if stopSignal == "" {
			stopSignal = signal
		}
		proc.mu.Unlock()
		cancel()
	}
	go func() {
		defer close(done)
		defer cancel()
		result, err := p.opts.FileTransfer.TransferFile(ctx, params.ProcessID, *params.FileTransfer)
		proc.mu.Lock()
		defer proc.mu.Unlock()
		if err != nil {
			proc.stdout.append([]byte(err.Error() + "\n"))
		}
		proc.stdout.eof = true
		if stopSignal == "" {
			proc.fileTransfer = &result
		}
		proc.notifyLocked()
	}()
	proc.wait = func() (int, string) {
		<-done
		proc.mu.Lock()
		defer proc.mu.Unlock()
		switch {
		case stopSignal != "":
			return -1, stopSignal
		case proc.fileTransfer.Error != nil:
			return 1, ""
		}
		return 0, ""
	}
	return proc
}

func spawn(params ProcessStartParams, cwd string, env []string) (*process, error) {
	cmd := exec.Command(params.Argv[0], params.Argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	if params.IOMode == IOModePTY {
		return spawnPTY(params, cmd)
	}
	setProcessGroupID(cmd)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	merge := params.Stderr == StderrMerge
	var stderrR, stderrW *os.File
	if merge {
		// One fd for both: exec dup2s the same file onto 1 and 2.
		cmd.Stdout, cmd.Stderr = stdoutW, stdoutW
	} else {
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	}
	var stdin io.WriteCloser
	if params.Stdin != StdinClosed {
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{stdoutR, stdoutW, stderrR, stderrW} {
			if f != nil {
				_ = f.Close()
			}
		}
		return nil, fmt.Errorf("spawn %s: %w", params.Argv[0], err)
	}
	// The child holds its own copies; closing ours lets readers see EOF.
	_ = stdoutW.Close()
	if stderrW != nil {
		_ = stderrW.Close()
	}
	proc := newCommandProcess(params.ProcessID, cmd)
	proc.stdin, proc.stdinClosed = stdin, stdin == nil
	go proc.pump(stdoutR, func(p *process) *stream { return &p.stdout })
	if merge {
		proc.stderrMode = StderrMerge
		proc.stderr.eof = true
	} else {
		go proc.pump(stderrR, func(p *process) *stream { return &p.stderr })
	}
	return proc, nil
}

func newCommandProcess(id string, cmd *exec.Cmd) *process {
	return &process{
		id: id, cmd: cmd, pid: cmd.Process.Pid, startedAt: time.Now(),
		changed: make(chan struct{}), applied: map[string]bool{},
		state: ProcessRunning, exited: make(chan struct{}),
		wait: func() (int, string) {
			_ = cmd.Wait()
			return exitStatus(cmd.ProcessState)
		},
	}
}

// spawnPTY runs the command on a pseudo-terminal. Output is one stream (the
// terminal merges stderr), so the process reports stderr "merge".
func spawnPTY(params ProcessStartParams, cmd *exec.Cmd) (*process, error) {
	master, err := startPTY(cmd)
	if err != nil {
		return nil, fmt.Errorf("spawn %s on a pty: %w", params.Argv[0], err)
	}
	proc := newCommandProcess(params.ProcessID, cmd)
	proc.stdin, proc.pty = master, true
	proc.stderrMode = StderrMerge
	proc.stderr.eof = true
	go proc.pump(master, func(p *process) *stream { return &p.stdout })
	return proc, nil
}

func (p *process) pump(r *os.File, pick func(*process) *stream) {
	defer func() { _ = r.Close() }()
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Read(chunk)
		p.mu.Lock()
		if n > 0 {
			pick(p).append(chunk[:n])
		}
		if err != nil {
			pick(p).eof = true
		}
		p.notifyLocked()
		p.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// runningLocked counts processes that have not ended. Callers hold t.mu.
func (t *processTable) runningLocked() int {
	running := 0
	for _, p := range t.processes {
		select {
		case <-p.exited:
		default:
			running++
		}
	}
	return running
}

// retainEndedLocked enforces the ended-process retention bounds, oldest
// ended first. Callers hold t.mu.
func (t *processTable) retainEndedLocked() {
	type ended struct {
		p       *process
		endedAt time.Time
		bytes   int
	}
	var all []ended
	retained := 0
	for _, p := range t.processes {
		p.mu.Lock()
		if !p.endedAt.IsZero() {
			size := len(p.stdout.buf) + len(p.stderr.buf)
			all = append(all, ended{p: p, endedAt: p.endedAt, bytes: size})
			retained += size
		}
		p.mu.Unlock()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].endedAt.Before(all[j].endedAt) })
	for i, e := range all {
		if len(all)-i > maxRetainedEnded {
			delete(t.processes, e.p.id)
			retained -= e.bytes
			continue
		}
		if retained <= maxRetainedOutputBytes {
			return
		}
		e.p.mu.Lock()
		e.p.stdout.release()
		e.p.stderr.release()
		e.p.mu.Unlock()
		retained -= e.bytes
	}
}

// watch records the exit state, enforces timeout_ms and sends the advisory
// process.status notification.
func (p *processes) watch(session *CoreSession, table *processTable, proc *process, timeout time.Duration) {
	if timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			proc.mu.Lock()
			if proc.stopReason == "" {
				proc.stopReason = processReasonTimeout
			}
			proc.mu.Unlock()
			terminate(proc, 2*time.Second)
		})
		defer timer.Stop()
	}
	code, signal := proc.wait()
	if proc.onEnd != nil {
		proc.onEnd()
	}
	proc.mu.Lock()
	switch {
	case proc.stopReason == processReasonTimeout:
		proc.state = ProcessFailed
	case signal != "":
		proc.state = ProcessKilled
	default:
		proc.state = ProcessExited
	}
	if signal != "" {
		proc.exitSignal = &signal
	} else {
		proc.exitCode = &code
	}
	if proc.stdin != nil && !proc.stdinClosed {
		_ = proc.stdin.Close()
		proc.stdinClosed = true
	}
	proc.endedAt = time.Now()
	close(proc.exited)
	proc.notifyLocked()
	status := ProcessStatusParams{
		SessionID: session.ID(), ProcessID: proc.id, State: proc.state,
		ExitCode: proc.exitCode, ExitSignal: proc.exitSignal,
		EndedAt: proc.endedAtLocked(), Reason: proc.reasonLocked(),
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	proc.mu.Unlock()
	table.mu.Lock()
	table.retainEndedLocked()
	table.mu.Unlock()
	_ = session.client.Notify(context.Background(), MethodProcessStatus, status)
}

func waitState(p *process, waitMS int64) ProcessState {
	if waitMS > 0 {
		timer := time.NewTimer(time.Duration(waitMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-p.exited:
		case <-timer.C:
		}
	}
	state := p.snapshot()
	state.Accepted = true
	return state
}

func (p *processes) read(ctx context.Context, req Request) (any, error) {
	var params ProcessReadParams
	if err := decode(req.Params, &params); err != nil {
		return nil, err
	}
	if params.MaxBytes == 0 {
		params.MaxBytes = maxReadBytes
	}
	if params.MaxBytes < 0 || params.MaxBytes > maxReadBytes {
		return nil, invalidParams("max_bytes must be 1-%d", maxReadBytes)
	}
	if params.WaitMS < 0 || params.WaitMS > maxWaitMS {
		return nil, invalidParams("wait_ms must be 0-%d", maxWaitMS)
	}
	if params.Stream != StreamStdout && params.Stream != StreamStderr {
		return nil, invalidParams("stream must be stdout or stderr")
	}
	proc, err := p.table(req.Session).get(params.ProcessID)
	if err != nil {
		return nil, err
	}
	if params.WaitMS > 0 {
		counter, _ := p.reads.LoadOrStore(req.Client, new(atomic.Int32))
		reads := counter.(*atomic.Int32)
		if reads.Add(1) > maxConcurrentReads {
			reads.Add(-1)
			return nil, busy("too many concurrent process reads")
		}
		defer reads.Add(-1)
	}
	return proc.read(params, ctx.Done())
}

func (p *process) read(params ProcessReadParams, cancel <-chan struct{}) (ProcessReadResult, error) {
	deadline := time.Now().Add(time.Duration(params.WaitMS) * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	pick := &p.stdout
	if params.Stream == StreamStderr {
		pick = &p.stderr
	}
	if params.Cursor < 0 || params.Cursor > pick.next() {
		return ProcessReadResult{}, invalidParams("cursor is out of range")
	}
	// Block only when the caller is at the end of a live stream.
	for params.Cursor == pick.next() && !pick.eof && time.Now().Before(deadline) {
		changed := p.changed
		p.mu.Unlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-changed:
		case <-timer.C:
		case <-cancel:
		}
		timer.Stop()
		p.mu.Lock()
		select {
		case <-cancel:
			deadline = time.Now()
		default:
		}
	}
	result := ProcessReadResult{
		ProcessID: p.id, Stream: params.Stream, EarliestCursor: pick.earliest,
		State: p.state, EndedAt: p.endedAtLocked(), Reason: p.reasonLocked(),
		ExitCode: p.exitCode, ExitSignal: p.exitSignal, FileTransfer: p.endedFileTransferLocked(),
	}
	if params.Cursor < pick.earliest {
		result.Gap = true
		result.FromCursor, result.NextCursor = pick.earliest, pick.earliest
		return result, nil
	}
	offset := int(params.Cursor - pick.earliest)
	available := pick.buf[offset:]
	if len(available) > params.MaxBytes {
		available = available[:params.MaxBytes]
		result.Truncated = true
	}
	result.Data = base64.StdEncoding.EncodeToString(available)
	result.FromCursor = params.Cursor
	result.NextCursor = params.Cursor + int64(len(available))
	result.EOF = pick.eof && result.NextCursor == pick.next()
	return result, nil
}

func (p *processes) write(ctx context.Context, req Request) (any, error) {
	var params ProcessWriteParams
	if err := decode(req.Params, &params); err != nil {
		return nil, err
	}
	if (params.Data == "") == !params.CloseStdin {
		return nil, invalidParams("exactly one of data or close_stdin is required")
	}
	data, err := base64.StdEncoding.DecodeString(params.Data)
	if err != nil {
		return nil, invalidParams("data must be base64")
	}
	if len(data) > maxWriteBytes {
		return nil, invalidParams("data exceeds %d bytes", maxWriteBytes)
	}
	proc, err := p.table(req.Session).get(params.ProcessID)
	if err != nil {
		return nil, err
	}
	if err := proc.write(req.ID, data, params.CloseStdin); err != nil {
		return nil, err
	}
	return ProcessWriteResult{Accepted: true}, nil
}

// write applies one stdin write or close. The JSON-RPC id is the
// idempotency key: a replay is not re-applied.
func (p *process) write(requestID string, data []byte, closeStdin bool) error {
	if closeStdin && p.pty {
		return &RPCError{Code: CodeCapabilityUnavailable, Message: "close_stdin is not supported for PTY-backed processes"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if requestID != "" && p.applied[requestID] {
		return nil
	}
	if p.stdinClosed {
		return conflict("stdin_closed")
	}
	if closeStdin {
		_ = p.stdin.Close()
		p.stdinClosed = true
	} else if _, err := p.stdin.Write(data); err != nil {
		return conflict("stdin_closed")
	}
	if requestID != "" && len(p.applied) < maxAppliedWrites {
		p.applied[requestID] = true
	}
	return nil
}

func (p *processes) signal(req Request, terminateProcess bool) (any, error) {
	var params ProcessSignalParams
	if err := decode(req.Params, &params); err != nil {
		return nil, err
	}
	proc, err := p.table(req.Session).get(params.ProcessID)
	if err != nil {
		return nil, err
	}
	if !terminateProcess {
		select {
		case <-proc.exited:
		default:
			if err := proc.signal(signalInterrupt); err != nil {
				return nil, &RPCError{Code: CodeInternalError, Message: "interrupt: " + err.Error()}
			}
		}
		state := proc.snapshot()
		state.Accepted = true
		return state, nil
	}
	grace := 5 * time.Second
	if params.GraceMS != nil {
		if *params.GraceMS < 0 || *params.GraceMS > maxGraceMS {
			return nil, invalidParams("grace_ms must be 0-%d", maxGraceMS)
		}
		grace = time.Duration(*params.GraceMS) * time.Millisecond
	}
	terminate(proc, grace)
	state := proc.snapshot()
	state.Accepted = true
	return state, nil
}

// terminate sends SIGTERM to the process group, waits grace, then SIGKILL.
// It returns once the process has exited. Terminating an exited process is a
// no-op.
func terminate(p *process, grace time.Duration) {
	select {
	case <-p.exited:
		return
	default:
	}
	if grace > 0 {
		_ = p.signal(signalTerminate)
		select {
		case <-p.exited:
			return
		case <-time.After(grace):
		}
	}
	_ = p.signal(signalKill)
	<-p.exited
}

func (t *processTable) terminateAll(grace time.Duration) {
	t.mu.Lock()
	processes := make([]*process, 0, len(t.processes))
	for _, p := range t.processes {
		processes = append(processes, p)
	}
	t.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range processes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			terminate(p, grace)
		}()
	}
	wg.Wait()
}

func (p *processes) list(ctx context.Context, req Request) (any, error) {
	table := p.table(req.Session)
	table.mu.Lock()
	processes := make([]*process, 0, len(table.processes))
	for _, proc := range table.processes {
		processes = append(processes, proc)
	}
	table.mu.Unlock()
	result := ProcessListResult{Processes: make([]ProcessState, 0, len(processes))}
	for _, proc := range processes {
		result.Processes = append(result.Processes, proc.snapshot())
	}
	sort.Slice(result.Processes, func(i, j int) bool {
		return result.Processes[i].StartedAt < result.Processes[j].StartedAt
	})
	return result, nil
}

var errSkillInstallerMissing = errors.New("bmaexecutor: NewSkillInstallCapability needs an installer")

// SkillInstaller installs one skill revision; the result state is "ready" or
// "failed" with error_code and error. It must be idempotent by (skill_id,
// revision_id, digest).
type SkillInstaller func(ctx context.Context, params SkillInstallParams) SkillInstallResult

// NewSkillInstallCapability serves skill.install (skills.install) with install.
// Installs run one at a time.
func NewSkillInstallCapability(install SkillInstaller) (Capability, error) {
	if install == nil {
		return Capability{}, errSkillInstallerMissing
	}
	var mu sync.Mutex
	return Capability{
		Advertise: func(capabilities *Capabilities) { capabilities.Skills = &SkillsCapability{Install: true} },
		Methods: map[string]Handler{
			MethodSkillInstall: func(ctx context.Context, req Request) (any, error) {
				var params SkillInstallParams
				if err := decode(req.Params, &params); err != nil {
					return nil, err
				}
				mu.Lock()
				defer mu.Unlock()
				return install(ctx, params), nil
			},
		},
	}, nil
}
