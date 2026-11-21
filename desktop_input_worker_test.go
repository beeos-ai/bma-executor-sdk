package bmaexecutor

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"testing"
	"time"
)

func TestNativeInputWorkerEOFAndIndependentWatchdogReleaseHeldKeys(t *testing.T) {
	for _, cause := range []string{"parent_eof", "idle_watchdog"} {
		t.Run(cause, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			replies, output := io.Pipe()
			defer replies.Close()
			n := &controlNative{}
			done := make(chan struct{})
			go func() { runNativeInputLoop(reader, output, n, 30*time.Millisecond); output.Close(); close(done) }()
			if err := json.NewEncoder(writer).Encode(DesktopInputEvent{Type: "key", Keysym: 0xffe1, Down: true}); err != nil {
				t.Fatal(err)
			}
			var ack struct {
				OK bool `json:"ok"`
			}
			if err := json.NewDecoder(replies).Decode(&ack); err != nil || !ack.OK {
				t.Fatal("input not acknowledged", err)
			}
			n.mu.Lock()
			held := n.held
			n.mu.Unlock()
			if !held {
				t.Fatal("fixture did not hold Shift")
			}
			if cause == "parent_eof" {
				writer.Close()
			}
			deadline := time.Now().Add(time.Second)
			for {
				n.mu.Lock()
				held = n.held
				n.mu.Unlock()
				if !held {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Shift survived loss of parent or heartbeat")
				}
				time.Sleep(time.Millisecond)
			}
			writer.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("worker did not exit on EOF")
			}
		})
	}
}

func TestNativeInputProcessStartsWithIndependentProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper fixture")
	}
	cmd := newNativeInputCommand("/bin/sh")
	cmd.Args = []string{"/bin/sh", "-c", `printf '{"ok":true}\n'; while IFS= read -r line; do printf '{"ok":true}\n'; done`}
	p, err := startNativeInputCommand(cmd)
	if err != nil {
		t.Fatal("production parent could not start native helper", err)
	}
	defer p.Close()
	if err = p.Apply(context.Background(), DesktopInputEvent{Type: "heartbeat"}); err != nil {
		t.Fatal(err)
	}
}
