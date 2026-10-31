//go:build windows

package bmaexecutor

import (
	"context"
	"os/exec"
	"time"
)

// setProcessGroup kills the direct process on cancellation. Windows job
// objects (killing the whole tree) are not implemented yet.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd", "/C", command)
	setProcessGroup(cmd)
	return cmd
}
