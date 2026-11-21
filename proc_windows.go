//go:build windows

package bmaexecutor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// noFollow: os.Root already refuses links that leave the root on Windows.
const noFollow = 0

type processSignal int

const (
	signalInterrupt processSignal = iota
	signalTerminate
	signalKill
)

// statusControlCExit is the exit code of a process ended by CTRL_C/CTRL_BREAK
// that it did not handle.
const statusControlCExit = 0xC000013A

// processTree is one started command and everything it spawns: a Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE (the Windows counterpart of the
// POSIX process group), plus the console process group used for interrupts.
// It is the model of codex's windows-sandbox-rs and of Windows Terminal's
// process management: the process is created suspended, assigned to the job
// and only then resumed, so no descendant escapes the job.
type processTree struct {
	job windows.Handle
	pid uint32
	// pty is the pseudo console of an io_mode=pty process, nil otherwise.
	pty *conPTY

	mu      sync.Mutex
	process *os.Process
	closed  bool
	// killed and sent record how the executor ended the process, so its exit
	// is reported as a signal (Windows reports only an exit code).
	killed bool
	sent   string
}

func newProcessTree() (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	if err := setJobKillOnClose(job, true); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &processTree{job: job}, nil
}

func setJobKillOnClose(job windows.Handle, kill bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if kill {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set job object limits: %w", err)
	}
	return nil
}

// attach puts the suspended process pid in the job and resumes it.
func (t *processTree) attach(pid int) error {
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE | windows.PROCESS_SUSPEND_RESUME
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := t.assign(handle, pid); err != nil {
		return err
	}
	return resumeProcess(handle)
}

// assign puts the process in the job.
func (t *processTree) assign(handle windows.Handle, pid int) error {
	if err := windows.AssignProcessToJobObject(t.job, handle); err != nil {
		return fmt.Errorf("assign process %d to job: %w", pid, err)
	}
	t.pid = uint32(pid)
	return nil
}

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// resumeProcess resumes every thread of a process created suspended
// (os/exec exposes no thread handle).
func resumeProcess(handle windows.Handle) error {
	if status, _, _ := ntResumeProcess.Call(uintptr(handle)); status != 0 {
		return fmt.Errorf("resume process: NTSTATUS %#x", status)
	}
	return nil
}

// startCommand starts cmd inside a new process tree. Context cancellation
// (cmd.Cancel, when the command was built by setProcessGroup) ends the whole
// tree.
func startCommand(cmd *exec.Cmd) (*processTree, error) {
	ensureConsole()
	tree, err := newProcessTree()
	if err != nil {
		return nil, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return tree.signal(signalKill) }
	}
	if err := cmd.Start(); err != nil {
		tree.close()
		return nil, err
	}
	tree.mu.Lock()
	tree.process = cmd.Process
	tree.mu.Unlock()
	if err := tree.attach(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		tree.close()
		return nil, err
	}
	return tree, nil
}

// signal delivers sig to the tree. Interrupt is a console CTRL_BREAK to the
// command's process group; terminate asks the same way and ends the tree when
// that cannot be delivered; kill ends the whole job.
func (t *processTree) signal(sig processSignal) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	switch sig {
	case signalInterrupt:
		t.sent = "SIGINT"
		return t.interruptLocked()
	case signalTerminate:
		t.sent = "SIGTERM"
		if err := t.interruptLocked(); err == nil {
			return nil
		}
	}
	t.killed = true
	err := windows.TerminateJobObject(t.job, 1)
	if t.process != nil {
		_ = t.process.Kill()
	}
	return err
}

func (t *processTree) interruptLocked() error {
	if t.pty != nil {
		return t.pty.interrupt()
	}
	if t.pid == 0 {
		return errors.New("process is not started")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, t.pid)
}

// exit maps the process state to the exit code or signal name the contract
// reports.
func (t *processTree) exit(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	return t.exitCode(uint32(state.ExitCode()))
}

// exitCode is exit for a raw Windows exit code.
func (t *processTree) exitCode(raw uint32) (int, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	code := int(raw)
	switch {
	case t.killed:
		return -1, "SIGKILL"
	case raw == statusControlCExit:
		if t.sent == "" {
			return -1, "SIGINT"
		}
		return -1, t.sent
	}
	return code, ""
}

// killAll ends every process still in the job, also after the command itself
// exited (a session ends: nothing it started may outlive it).
func (t *processTree) killAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.killed = true
		_ = windows.TerminateJobObject(t.job, 1)
	}
}

// close ends the tree and releases its handles.
func (t *processTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeLocked()
}

func (t *processTree) closeLocked() {
	if t.closed {
		return
	}
	t.closed = true
	_ = windows.CloseHandle(t.job) // KILL_ON_JOB_CLOSE ends what is left
	if t.pty != nil {
		t.pty.closeAsync()
	}
}

// release lets what the command left running keep running (as a POSIX
// process group's orphans do) and drops the handles.
func (t *processTree) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	_ = setJobKillOnClose(t.job, false)
	t.closeLocked()
}

var (
	consoleOnce sync.Once
	kernel32    = windows.NewLazySystemDLL("kernel32.dll")
	user32      = windows.NewLazySystemDLL("user32.dll")
)

// ensureConsole gives the executor a console to share with the commands it
// starts: GenerateConsoleCtrlEvent only reaches processes on the caller's
// console, and without one every child would open its own window. A console
// the executor had to create is hidden.
func ensureConsole() {
	consoleOnce.Do(func() {
		var pids [1]uint32
		if n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), 1); n != 0 {
			return
		}
		if ok, _, _ := kernel32.NewProc("AllocConsole").Call(); ok == 0 {
			return
		}
		if hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call(); hwnd != 0 {
			const swHide = 0
			_, _, _ = user32.NewProc("ShowWindow").Call(hwnd, swHide)
		}
	})
}

// setProcessGroup makes context cancellation end the whole tree: startCommand
// replaces the placeholder Cancel with the job termination.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 2 * time.Second
}

func setProcessGroupID(*exec.Cmd) {}

// shellArgv is the argv a bash tool runs command with: cmd.exe
// `/d /s /c "<command>"`, its command line passed verbatim (see
// windowsCommandLine).
func shellArgv(command string) []string {
	return []string{"cmd", "/d", "/s", "/c", command}
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

// setCredential is never reached: NewCore rejects CoreOptions.RunAs here.
func setCredential(*exec.Cmd, *RunAs) {}
