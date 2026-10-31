//go:build !windows

package bmaexecutor

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup runs cmd in its own process group and makes context
// cancellation kill the whole group, so children of a canceled tool or a
// stopped MCP server do not outlive it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	setProcessGroup(cmd)
	return cmd
}
