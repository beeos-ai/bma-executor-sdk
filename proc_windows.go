//go:build windows

package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

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

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd", "/C", command)
	setProcessGroup(cmd)
	return cmd
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

func startPTY(*exec.Cmd) (*os.File, error) {
	return nil, errors.New("io_mode=pty is not supported on windows")
}
