//go:build !windows

package gatewaystub

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ShellHands are the default hands: bash in workspace, nothing else. A command
// runs in its own process group; past its synchronous wait (the smaller of the
// call's timeout in ms, the call's SyncWait and 55 s) it is left running and the
// call answers with its pid and the file its output goes to (ADR-0005 decision
// 4). turn ends the whole group, in the background too.
func ShellHands(turn context.Context, workspace string) Hands {
	return &shellHands{turn: turn, workspace: workspace}
}

type shellHands struct {
	turn      context.Context
	workspace string

	mu  sync.Mutex
	seq int
}

func (h *shellHands) Run(_ context.Context, call Call) Reply {
	if call.Name != "bash" {
		return Reply{Text: fmt.Sprintf("tool %q is not available on these hands", call.Name), IsError: true}
	}
	command, _ := call.Input["command"].(string)
	wait := 55 * time.Second
	if ms, ok := call.Input["timeout"].(float64); ok && ms > 0 {
		wait = min(wait, time.Duration(ms)*time.Millisecond)
	}
	if call.SyncWait > 0 {
		wait = min(wait, call.SyncWait)
	}
	h.mu.Lock()
	h.seq++
	outFile := filepath.Join(h.workspace, fmt.Sprintf(".bash-%d.out", h.seq))
	h.mu.Unlock()
	spawnFailed := func(err error) Reply {
		return Reply{Outcome: OutcomeNotForwarded, Code: "spawn_failed", Text: "spawn_failed: " + err.Error(), IsError: true}
	}
	out, err := os.Create(outFile)
	if err != nil {
		return spawnFailed(err)
	}
	defer func() { _ = out.Close() }()
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = h.workspace, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return spawnFailed(err)
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	go func() { // the turn's end kills the process group, in the background too
		select {
		case <-h.turn.Done():
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-done:
		}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		if h.turn.Err() != nil {
			return Reply{Text: "tool was canceled", IsError: true}
		}
		return finished(outFile, waitErr)
	case <-timer.C:
		return Reply{Text: fmt.Sprintf("running in the background (pid %d), output in %s", pgid, outFile), PID: pgid, OutputFile: outFile}
	}
}

func finished(outFile string, err error) Reply {
	raw, _ := os.ReadFile(outFile)
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	text := fmt.Sprintf("exit_code: %d", code)
	if len(raw) > 0 {
		text = string(raw) + "\n" + text
	}
	return Reply{Text: text}
}
