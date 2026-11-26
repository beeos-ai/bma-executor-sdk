//go:build windows

package bmaexecutor

import (
	"errors"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// defaultShell is what a terminal runs without argv: the requested shell,
// else COMSPEC (cmd.exe).
func defaultShell(shell string, env []string) []string {
	if shell != "" {
		return []string{shell}
	}
	for i := len(env) - 1; i >= 0; i-- {
		if name, value, ok := strings.Cut(env[i], "="); ok && strings.EqualFold(name, "COMSPEC") && value != "" {
			return []string{value}
		}
	}
	return []string{`C:\Windows\System32\cmd.exe`}
}

// winPTY is a process on a pseudo console (ConPTY) in its own Job Object.
// The output pipe ends only once the pseudo console is closed, so the exit
// is awaited apart from the output: when the process ends, the console is
// closed after its output went quiet and the reader then sees EOF.
type winPTY struct {
	c    conPTYStart
	done chan struct{}
	code uint32
	ok   bool

	closeOnce sync.Once
}

func startTerminalPTY(argv []string, dir string, env []string, rows, cols uint16, _ *RunAs) (ptyProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the platform names the command
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir, cmd.Env = dir, env
	c, err := startOnConPTY(cmd, rows, cols)
	if err != nil {
		return nil, err
	}
	p := &winPTY{c: c, done: make(chan struct{})}
	go func() {
		p.code, p.ok = c.waitExit()
		c.pty.closeAfterDrain()
		close(p.done)
	}()
	return p, nil
}

func (p *winPTY) Read(b []byte) (int, error)  { return p.c.pty.out.Read(b) }
func (p *winPTY) Write(b []byte) (int, error) { return p.c.pty.in.Write(b) }
func (p *winPTY) pid() int                    { return p.c.pid }

// hangup closes the pseudo console: its clients get CTRL_CLOSE_EVENT.
func (p *winPTY) hangup() { p.c.pty.closeAsync() }
func (p *winPTY) kill()   { p.c.tree.killAll() }

func (p *winPTY) resize(rows, cols uint16) error {
	return windows.ResizePseudoConsole(p.c.pty.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (p *winPTY) wait() (*int, string, error) {
	<-p.done
	if !p.ok {
		return nil, "exited", errors.New("exit code not readable")
	}
	code, signal := p.c.tree.exitCode(p.code)
	if signal != "" {
		return nil, "signaled", nil
	}
	return ref(code), "exited", nil
}

func (p *winPTY) close() {
	p.closeOnce.Do(func() {
		p.c.tree.close()
		_ = p.c.pty.out.Close()
	})
}
