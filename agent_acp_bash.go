package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The bash tool of the local route has the platform gateway's semantics
// (ADR-0005 decision 4, the same as the tool route conformance case T6):
// `timeout` is how long the call waits, not how long the command may live.
// A command that outlives the wait is left running; the call answers with its
// pid and the file its output goes to, and the brain reads the file later.
// The command lives on until it exits, until acpBashLifetime, or until the
// turn is canceled or runs out of time (or the session closes, or the agent
// stops); the end of a turn that completed does not kill it. on_timeout "kill"
// stops the command at the end of the wait instead.
const (
	// acpBashSyncWait is the gateway's default synchronous wait; it keeps a
	// call below the 60 s tool timeout of ACP agents.
	acpBashSyncWait = 55 * time.Second
	// acpBashDefaultTimeout applies when the call has no timeout (ms).
	acpBashDefaultTimeout = 120 * time.Second
	// acpBashMaxTimeout is the largest timeout a call can ask for.
	acpBashMaxTimeout = 600 * time.Second
	// acpBashLifetime is how long a command left in the background may run.
	acpBashLifetime = time.Hour
	// acpBashOutputDir (under the turn's working directory) holds the output files.
	acpBashOutputDir = ".bma-bash"
)

// acpBackgroundProc is a command left running past its call.
type acpBackgroundProc struct {
	tree   *processTree
	done   chan struct{}
	output string
}

func (p *acpBackgroundProc) kill() {
	select {
	case <-p.done:
	default:
		_ = p.tree.signal(signalKill)
	}
}

// acpBackground tracks the commands left running, by session.
type acpBackground struct {
	mu    sync.Mutex
	procs map[string][]*acpBackgroundProc
}

func (b *acpBackground) add(sessionID string, p *acpBackgroundProc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.procs == nil {
		b.procs = map[string][]*acpBackgroundProc{}
	}
	live := b.procs[sessionID][:0]
	for _, q := range b.procs[sessionID] {
		select {
		case <-q.done:
		default:
			live = append(live, q)
		}
	}
	b.procs[sessionID] = append(live, p)
}

// killSession ends every command of the session and removes their output files.
func (b *acpBackground) killSession(sessionID string) {
	b.mu.Lock()
	procs := b.procs[sessionID]
	delete(b.procs, sessionID)
	b.mu.Unlock()
	for _, p := range procs {
		p.kill()
		<-p.done
		_ = os.Remove(p.output)
	}
}

func (b *acpBackground) killAll() {
	b.mu.Lock()
	sessions := make([]string, 0, len(b.procs))
	for id := range b.procs {
		sessions = append(sessions, id)
	}
	b.mu.Unlock()
	for _, id := range sessions {
		b.killSession(id)
	}
}

// runBash runs one bash call of the agent in the session's workspace. The
// call's own ctx ends with the call (the turn's cancel or deadline); turn is
// the turn the command belongs to, which decides the fate of a command left
// in the background.
func (a *ACPAgent) runBash(ctx context.Context, turn *acpTurn, sessionID, callID string, in map[string]any) (string, bool, *RouteBackground) {
	command, _ := in["command"].(string)
	if command == "" {
		return "bash requires command", true, nil
	}
	timeout := acpBashDefaultTimeout
	if ms, ok := in["timeout"].(float64); ok && ms > 0 {
		timeout = min(time.Duration(ms)*time.Millisecond, acpBashMaxTimeout)
	}
	onTimeoutKill := in["on_timeout"] == "kill"
	session := a.opts.Core.session(coreKey{acpBrain, sessionID})
	if session == nil {
		return "session_not_active: the session is not open", true, nil
	}
	cwd, err := session.ResolveCwd("")
	if err != nil {
		return err.Error(), true, nil
	}
	env, err := a.opts.Core.Env(nil)
	if err != nil {
		return err.Error(), true, nil
	}
	output, err := a.bashOutputFile(cwd, sessionID, callID)
	if err != nil {
		return "spawn bash: " + err.Error(), true, nil
	}
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "spawn bash: " + err.Error(), true, nil
	}
	defer func() { _ = out.Close() }()
	if runAs := a.opts.Core.opts.RunAs; runAs != nil {
		if err := os.Chown(output, int(runAs.UID), int(runAs.GID)); err != nil {
			return "spawn bash: " + err.Error(), true, nil
		}
	}
	// The command is not tied to the call's ctx: it may outlive the call.
	cmd := shellCommand(context.Background(), command)
	setCredential(cmd, a.opts.Core.opts.RunAs)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = cwd, env, out, out
	tree, err := startCommand(cmd)
	if err != nil {
		return "spawn bash: " + err.Error(), true, nil
	}
	proc := &acpBackgroundProc{tree: tree, done: make(chan struct{}), output: output}
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		tree.release() // what the command left behind keeps running, as with a POSIX process group
		close(proc.done)
	}()
	timer := time.NewTimer(min(timeout, acpBashSyncWait))
	defer timer.Stop()
	select {
	case <-proc.done:
		return a.bashFinished(output, waitErr), false, nil
	case <-ctx.Done():
		proc.kill()
		<-proc.done
		_ = os.Remove(output)
		return "tool was canceled", true, nil
	case <-timer.C:
	}
	if onTimeoutKill {
		proc.kill()
		<-proc.done
		_ = os.Remove(output)
		return "tool timed out", true, nil
	}
	a.background.add(sessionID, proc)
	go a.superviseBackground(turn, proc)
	text := fmt.Sprintf("running in the background (pid %d), output in %s", cmd.Process.Pid, output)
	return text, false, &RouteBackground{PID: cmd.Process.Pid, OutputFile: output}
}

// superviseBackground ends a command left in the background when its turn is
// canceled or runs out of time, or after acpBashLifetime. A turn that
// completed leaves it running.
func (a *ACPAgent) superviseBackground(turn *acpTurn, proc *acpBackgroundProc) {
	lifetime := time.NewTimer(acpBashLifetime)
	defer lifetime.Stop()
	for {
		select {
		case <-proc.done:
			return
		case <-lifetime.C:
			proc.kill()
			return
		case <-turn.ctx.Done():
			if !turn.completed.Load() {
				proc.kill()
				return
			}
			// The turn completed: the command lives on until it exits or its lifetime ends.
			select {
			case <-proc.done:
			case <-lifetime.C:
				proc.kill()
			}
			return
		}
	}
}

// bashOutputFile is the file a command's output goes to, in the working directory.
func (a *ACPAgent) bashOutputFile(cwd, sessionID, callID string) (string, error) {
	dir := filepath.Join(cwd, acpBashOutputDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if runAs := a.opts.Core.opts.RunAs; runAs != nil {
		if err := os.Chown(dir, int(runAs.UID), int(runAs.GID)); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + callID))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".out"), nil
}

// bashFinished is the answer of a command that ended within the wait: its
// output (capped) and exit code, as the gateway renders them.
func (a *ACPAgent) bashFinished(output string, waitErr error) string {
	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	}
	raw, _ := os.ReadFile(output)
	_ = os.Remove(output)
	text := fmt.Sprintf("exit_code: %d", code)
	if limit := a.opts.Core.opts.MaxOutputBytes; len(raw) > limit {
		raw = append(raw[:limit:limit], "\n[truncated]"...)
	}
	if len(raw) > 0 {
		text = string(raw) + "\n" + text
	}
	return text
}
