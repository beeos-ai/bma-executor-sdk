//go:build !windows

package bmaexecutor

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// defaultShell is the login shell a terminal runs without argv: the
// requested shell, else SHELL, else bash, else sh.
func defaultShell(shell string, env []string) []string {
	if shell == "" {
		shell = lastEnv(env, "SHELL")
	}
	if shell == "" {
		shell = "/bin/bash"
		if _, err := os.Stat(shell); err != nil {
			shell = "/bin/sh"
		}
	}
	return []string{shell, "-l"}
}

// unixPTY is a process leading its own session on a pseudo-terminal, so its
// pid is the process group hangup and kill signal.
type unixPTY struct {
	master *os.File
	cmd    *exec.Cmd
}

func startTerminalPTY(argv []string, dir string, env []string, rows, cols uint16, runAs *RunAs) (ptyProcess, error) {
	// The PTY outlives the request; terminate ends it. The platform names the command.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // see above
	cmd.Dir, cmd.Env = dir, env
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if runAs != nil {
		attrs.Credential = &syscall.Credential{Uid: runAs.UID, Gid: runAs.GID, Groups: append([]uint32{}, runAs.Groups...)}
	}
	cmd.SysProcAttr = attrs
	master, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: rows, Cols: cols}, attrs)
	if err != nil {
		return nil, err
	}
	return &unixPTY{master: master, cmd: cmd}, nil
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.master.Write(b) }
func (p *unixPTY) pid() int                    { return p.cmd.Process.Pid }
func (p *unixPTY) hangup()                     { _ = syscall.Kill(-p.pid(), syscall.SIGHUP) }
func (p *unixPTY) kill()                       { _ = syscall.Kill(-p.pid(), syscall.SIGKILL) }
func (p *unixPTY) close()                      { _ = p.master.Close() }

func (p *unixPTY) resize(rows, cols uint16) error {
	return pty.Setsize(p.master, &pty.Winsize{Rows: rows, Cols: cols})
}

func (p *unixPTY) wait() (*int, string, error) {
	err := p.cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return ref(0), "exited", nil
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return nil, "signaled", nil
		}
		return ref(exit.ExitCode()), "exited", nil
	}
	return nil, "exited", err
}
