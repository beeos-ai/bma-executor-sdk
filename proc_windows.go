//go:build windows

package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// noFollow: os.Root already refuses links that leave the root on Windows.
const noFollow = 0

type processSignal int

const (
	signalInterrupt processSignal = iota
	signalTerminate
	signalKill
	ptySupported = false
)

// setProcessGroup kills the direct process on cancellation. Windows job
// objects (killing the whole tree) are not implemented yet.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}

func setProcessGroupID(*exec.Cmd) {}

// shellCommand runs command with cmd.exe: `cmd.exe /d /s /c "<command>"`
// with the command line passed verbatim (see windowsCommandLine).
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd", "/d", "/s", "/c", command)
	setCommandLine(cmd)
	setProcessGroup(cmd)
	return cmd
}

// setCommandLine passes cmd.exe's command line verbatim; Go's argv quoting
// breaks quotes and $ in what cmd hands to the program it runs.
func setCommandLine(cmd *exec.Cmd) {
	line, ok := windowsCommandLine(cmd.Args)
	if !ok {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}

// Windows has no process-group signals; interrupt is unsupported and
// terminate/kill end the direct child only.
func signalGroup(process *os.Process, signal processSignal) error {
	if process == nil {
		return errors.New("process is not started")
	}
	if signal == signalInterrupt {
		return errors.New("interrupt is not supported on windows")
	}
	return process.Kill()
}

func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	return state.ExitCode(), ""
}

// setCredential is never reached: NewCore rejects CoreOptions.RunAs here.
func setCredential(*exec.Cmd, *RunAs) {}

func startPTY(*exec.Cmd, *RunAs) (*os.File, error) {
	return nil, errors.New("io_mode=pty is not supported on windows")
}
