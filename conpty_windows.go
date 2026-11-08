//go:build windows

package bmaexecutor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ptySupported reports whether process.start io_mode "pty" works here:
// ConPTY exists from Windows 10 1809 (build 17763).
var ptySupported = conPTYAvailable()

func conPTYAvailable() bool {
	return windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole").Find() == nil
}

const (
	ptyRows = 24
	ptyCols = 80
	// ctrlC is what a terminal sends for an interrupt; ConPTY turns it into a
	// CTRL_C_EVENT for the client applications.
	ctrlC = "\x03"
)

// conPTY is a Windows pseudo console (ConPTY): the executor writes the
// terminal's input to in and reads its output from out.
type conPTY struct {
	hpc windows.Handle
	in  *os.File
	out *activityFile

	closeOnce sync.Once
}

// activityFile is the terminal's output pipe that remembers when it last
// delivered bytes.
type activityFile struct {
	*os.File
	last atomic.Int64 // unix nanoseconds
}

func (f *activityFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.last.Store(time.Now().UnixNano())
	}
	return n, err
}

const (
	// ptyDrainQuiet is how long the output must stay silent before a pseudo
	// console whose process ended is closed, and ptyDrainMax bounds the wait.
	// ConPTY renders the client's last output to the pipe asynchronously, and
	// closing it first discards what is still pending (node-pty waits too).
	ptyDrainQuiet = 200 * time.Millisecond
	ptyDrainMax   = 3 * time.Second
)

// closeAfterDrain closes the pseudo console of an ended process once its
// output went quiet.
func (c *conPTY) closeAfterDrain() {
	c.closeOnce.Do(func() {
		go func() {
			start := time.Now()
			c.out.last.CompareAndSwap(0, start.UnixNano())
			for time.Since(start) < ptyDrainMax &&
				time.Since(time.Unix(0, c.out.last.Load())) < ptyDrainQuiet {
				time.Sleep(25 * time.Millisecond)
			}
			windows.ClosePseudoConsole(c.hpc)
			_ = c.in.Close()
		}()
	})
}

func (c *conPTY) interrupt() error {
	_, err := c.in.WriteString(ctrlC)
	return err
}

// closeAsync closes the pseudo console. ClosePseudoConsole can block until the
// output pipe is drained, which the output pump does, so it must not run on
// the caller's goroutine.
func (c *conPTY) closeAsync() {
	c.closeOnce.Do(func() {
		go func() {
			windows.ClosePseudoConsole(c.hpc)
			_ = c.in.Close()
		}()
	})
}

// spawnPTY runs the command on a pseudo console, in its own Job Object. Output
// is one stream (the terminal merges stderr), so the process reports stderr
// "merge". The process is created suspended, assigned to the job, then resumed.
func spawnPTY(params ProcessStartParams, cmd *exec.Cmd, _ *RunAs) (*process, error) {
	if cmd.Err != nil {
		return nil, fmt.Errorf("spawn %s on a pty: %w", params.Argv[0], cmd.Err)
	}
	proc, err := startConPTY(params, cmd)
	if err != nil {
		return nil, fmt.Errorf("spawn %s on a pty: %w", params.Argv[0], err)
	}
	return proc, nil
}

func startConPTY(params ProcessStartParams, cmd *exec.Cmd) (*process, error) {
	line, ok := windowsCommandLine(cmd.Args)
	if !ok {
		parts := make([]string, len(cmd.Args))
		for i, arg := range cmd.Args {
			parts[i] = escapeWindowsArg(arg)
		}
		line = strings.Join(parts, " ")
	}
	application, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return nil, err
		}
	}
	var env *uint16
	if cmd.Env != nil {
		block, err := environmentBlock(cmd.Env)
		if err != nil {
			return nil, err
		}
		env = &block[0]
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("create pty input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("create pty output pipe: %w", err)
	}
	var hpc windows.Handle
	err = windows.CreatePseudoConsole(windows.Coord{X: ptyCols, Y: ptyRows}, inRead, outWrite, 0, &hpc)
	// The pseudo console holds its own duplicates of its ends.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, fmt.Errorf("create pseudo console: %w", err)
	}
	pty := &conPTY{
		hpc: hpc,
		in:  os.NewFile(uintptr(inWrite), "conpty-in"),
		out: &activityFile{File: os.NewFile(uintptr(outRead), "conpty-out")},
	}
	fail := func(err error) (*process, error) {
		pty.closeAsync()
		_ = pty.out.Close()
		return nil, err
	}

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(err)
	}
	defer attributes.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it.
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return fail(err)
	}
	startup := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attributes.List(),
	}

	tree, err := newProcessTree()
	if err != nil {
		return fail(err)
	}
	tree.pty = pty
	var info windows.ProcessInformation
	variant := os.Getenv("BMA_CONPTY_VARIANT") // TEMPORARY diagnostics
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if variant == "nosuspend" {
		flags = uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	}
	if variant == "cpr" {
		_, _ = pty.in.WriteString("\x1b[1;1R")
	}
	if variant == "cprlate" {
		go func() { time.Sleep(300 * time.Millisecond); _, _ = pty.in.WriteString("\x1b[1;1R") }()
	}
	if err := windows.CreateProcess(application, commandLine, nil, nil, variant == "inherit", flags, env, dir, &startup.StartupInfo, &info); err != nil {
		tree.close()
		_ = pty.out.Close()
		return nil, err
	}
	if err := tree.assign(info.Process, int(info.ProcessId)); err != nil {
		_ = windows.TerminateProcess(info.Process, 1)
		_ = windows.CloseHandle(info.Process)
		_ = windows.CloseHandle(info.Thread)
		tree.close()
		_ = pty.out.Close()
		return nil, err
	}
	if variant == "nosuspend" {
		// no resume needed
	} else if _, err := windows.ResumeThread(info.Thread); err != nil {
		_ = windows.TerminateProcess(info.Process, 1)
		_ = windows.CloseHandle(info.Process)
		_ = windows.CloseHandle(info.Thread)
		tree.close()
		_ = pty.out.Close()
		return nil, fmt.Errorf("resume process: %w", err)
	}
	_ = windows.CloseHandle(info.Thread)

	proc := newProcess(params.ProcessID, int(info.ProcessId), tree, func() (int, string) {
		defer windows.CloseHandle(info.Process)
		_, _ = windows.WaitForSingleObject(info.Process, windows.INFINITE)
		var code uint32
		if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
			return -1, ""
		}
		// The terminal ends with its process: closing it lets the output
		// reader see EOF after the last bytes.
		pty.closeAfterDrain()
		return tree.exitCode(code)
	})
	proc.stdin, proc.pty = pty.in, true
	proc.stderrMode = StderrMerge
	proc.stderr.eof = true
	go proc.pump(pty.out, func(p *process) *stream { return &p.stdout })
	return proc, nil
}

// environmentBlock is the UTF-16 environment CreateProcess takes: NAME=value
// entries sorted by name (case-insensitively), each NUL-terminated, and an
// extra NUL at the end.
func environmentBlock(env []string) ([]uint16, error) {
	if len(env) == 0 {
		return []uint16{0, 0}, nil
	}
	sorted := append([]string(nil), env...)
	// "=C:=C:\dir" style entries start with '='; the name begins after it.
	name := func(entry string) string {
		rest := entry[min(1, len(entry)):]
		n, _, _ := strings.Cut(rest, "=")
		return strings.ToUpper(entry[:len(entry)-len(rest)] + n)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return name(sorted[i]) < name(sorted[j]) })
	var block []uint16
	for _, entry := range sorted {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("environment entry contains NUL")
		}
		block = append(block, utf16.Encode([]rune(entry))...)
		block = append(block, 0)
	}
	return append(block, 0), nil
}
