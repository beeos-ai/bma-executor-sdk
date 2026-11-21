package bmaexecutor

// The native input child owns held keys. Parent EOF/crash or five seconds of
// inactivity releases them even if the Agent executor itself cannot run defer.
import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const nativeInputArg = "--beework-native-input-worker"

func RunNativeDesktopInputWorker() bool {
	if len(os.Args) != 2 || os.Args[1] != nativeInputArg {
		return false
	}
	backend, err := newNativeDesktopInput()
	encoder := json.NewEncoder(os.Stdout)
	_ = encoder.Encode(map[string]bool{"ok": err == nil})
	if err != nil {
		return true
	}
	runNativeInputLoop(os.Stdin, os.Stdout, backend, 5*time.Second)
	return true
}

func runNativeInputLoop(input io.Reader, output io.Writer, backend NativeDesktopInput, idleLimit time.Duration) {
	defer backend.ReleaseAll(context.Background())
	encoder := json.NewEncoder(output)
	var mu sync.Mutex
	last := time.Now()
	released := false
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(idleLimit / 5)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				if !released && time.Since(last) >= idleLimit {
					released = backend.ReleaseAll(context.Background()) == nil
				}
				mu.Unlock()
			}
		}
	}()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 16384)
	for scanner.Scan() {
		var e DesktopInputEvent
		err := json.Unmarshal(scanner.Bytes(), &e)
		if err == nil {
			err = validateDesktopInput(e)
		}
		if err == nil {
			mu.Lock()
			last = time.Now()
			released = false
			switch e.Type {
			case "release_all":
				err = backend.ReleaseAll(context.Background())
			case "heartbeat":
			default:
				err = backend.Apply(context.Background(), e)
			}
			mu.Unlock()
		}
		if encoder.Encode(map[string]bool{"ok": err == nil}) != nil {
			return
		}
	}
}

type nativeInputProcess struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *os.File
	reader *bufio.Reader
	closed bool
}

func NewNativeDesktopInput() (NativeDesktopInput, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return startNativeInputCommand(newNativeInputCommand(exe))
}

func newNativeInputCommand(exe string) *exec.Cmd {
	cmd := exec.Command(exe, nativeInputArg)
	// Independent child: EOF/watchdog owns cleanup; never install a context kill.
	setProcessGroupID(cmd)
	// No platform or model credentials are needed by this native helper.
	for _, key := range []string{"HOME", "PATH", "TMPDIR", "LANG"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	return cmd
}

func startNativeInputCommand(cmd *exec.Cmd) (*nativeInputProcess, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	p := &nativeInputProcess{cmd: cmd, in: in, out: out.(*os.File), reader: bufio.NewReader(out)}
	_ = p.out.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := p.reader.ReadBytes('\n')
	var result struct {
		OK bool `json:"ok"`
	}
	if err != nil || json.Unmarshal(line, &result) != nil || !result.OK {
		p.Close()
		return nil, errors.New("native_input_worker_unavailable")
	}
	return p, nil
}
func (p *nativeInputProcess) Apply(ctx context.Context, e DesktopInputEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrDesktopControl
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if f, ok := p.in.(*os.File); ok {
		_ = f.SetWriteDeadline(deadline)
	}
	_ = p.out.SetReadDeadline(deadline)
	if err := json.NewEncoder(p.in).Encode(e); err != nil {
		p.abortLocked()
		return ErrDesktopInputUnknown
	}
	line, err := p.reader.ReadBytes('\n')
	var result struct {
		OK bool `json:"ok"`
	}
	if err != nil || json.Unmarshal(line, &result) != nil {
		p.abortLocked()
		return ErrDesktopInputUnknown
	}
	if !result.OK {
		return ErrDesktopInputUnknown
	}
	return nil
}
func (p *nativeInputProcess) ReleaseAll(ctx context.Context) error {
	return p.Apply(ctx, DesktopInputEvent{Type: "release_all"})
}
func (p *nativeInputProcess) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	_ = p.in.Close()
	p.mu.Unlock()
	// EOF makes the helper release all before exiting. Do not kill it while keys
	// may still be held; it also has an independent idle watchdog.
	go func() { _ = p.cmd.Wait(); _ = p.out.Close() }()
	return nil
}

func (p *nativeInputProcess) abortLocked() {
	if p.closed {
		return
	}
	p.closed = true
	_ = p.in.Close()
	go func() { _ = p.cmd.Wait(); _ = p.out.Close() }()
}
