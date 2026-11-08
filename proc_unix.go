//go:build !windows

package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type processSignal = syscall.Signal

// noFollow opens a session output without following a final symlink (and
// without blocking on a FIFO).
const noFollow = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

const (
	signalInterrupt = syscall.SIGINT
	signalTerminate = syscall.SIGTERM
	signalKill      = syscall.SIGKILL
	// ptySupported reports whether process.start io_mode "pty" works here.
	ptySupported = true
)

// setProcessGroup runs cmd in its own process group and makes context
// cancellation kill the whole group, so children of a canceled tool or a
// stopped MCP server do not outlive it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd.Process, signalKill) }
	cmd.WaitDelay = 2 * time.Second
}

// setProcessGroupID puts a process.start child in its own process group so
// interrupt/terminate reach every descendant.
func setProcessGroupID(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processTree is the process group of one started command.
type processTree struct{ process *os.Process }

// startCommand starts cmd (already in its own process group, see
// setProcessGroup/setProcessGroupID).
func startCommand(cmd *exec.Cmd) (*processTree, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processTree{process: cmd.Process}, nil
}

func (t *processTree) signal(sig processSignal) error { return signalGroup(t.process, sig) }

func (t *processTree) exit(state *os.ProcessState) (int, string) { return exitStatus(state) }

// killAll is a no-op: a group's leftovers outlive an ended command, as they
// always did on POSIX.
func (t *processTree) killAll() {}

// close is a no-op: context cancellation already killed the group, and the
// group id may be recycled once its leader was reaped.
func (t *processTree) close() {}

// release leaves the group's remaining processes running.
func (t *processTree) release() {}

// setCommandLine is a Windows concern (cmd.exe quoting).
func setCommandLine(*exec.Cmd) {}

// shellCommand runs command with a login bash (sh when bash is missing).
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-lc", command)
	setProcessGroup(cmd)
	return cmd
}

func signalGroup(process *os.Process, signal processSignal) error {
	if process == nil {
		return errors.New("process is not started")
	}
	err := syscall.Kill(-process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -1, signalName(status.Signal())
	}
	return state.ExitCode(), ""
}

func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	default:
		return "SIG" + strconv.Itoa(int(signal))
	}
}

// startPTY runs cmd on a new 24x80 pseudo-terminal. The child leads its own
// session, so its pid is the process group interrupt/terminate signal. The
// master carries input and output.
func startPTY(cmd *exec.Cmd, runAs *RunAs) (*os.File, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	setCredential(cmd, runAs)
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
}

// setCredential makes cmd run as runAs (nil keeps the executor's identity).
func setCredential(cmd *exec.Cmd, runAs *RunAs) {
	if runAs == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: runAs.UID, Gid: runAs.GID, Groups: append([]uint32{}, runAs.Groups...),
	}
}
