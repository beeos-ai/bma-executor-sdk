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
