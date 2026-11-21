package bmaexecutor

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestScreenPipeInputWorkerPreservesHeartbeatAndCancelsOnEOF(t *testing.T) {
	executor, host := net.Pipe()
	t.Cleanup(func() { executor.Close(); host.Close() })
	p := NewScreenLivePipe(executor, executor)
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var started atomic.Int32
	p.SetInputHandler(func(ctx context.Context, message DesktopInputMessage) error {
		if message.Event.Type == "heartbeat" {
			return nil
		}
		if started.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	write := func(id string, message DesktopInputMessage) error {
		_ = host.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
		return json.NewEncoder(host).Encode(map[string]any{"type": "screen_input", "id": id, "message": message})
	}
	packet := json.RawMessage(`{"input":{"type":"resize","width":1280,"height":720}}`)
	if err := write("resize", DesktopInputMessage{Packet: packet}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := write("queued-input", DesktopInputMessage{Packet: packet}); err != nil {
		t.Fatal("native operation blocked pipe reader:", err)
	}
	if err := write("heartbeat", DesktopInputMessage{Event: DesktopInputEvent{Type: "heartbeat"}}); err != nil {
		t.Fatal(err)
	}
	_ = host.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var ack struct {
		ID string
		OK bool
	}
	if err := json.NewDecoder(host).Decode(&ack); err != nil {
		t.Fatal("heartbeat blocked behind native resize:", err)
	}
	if ack.ID != "heartbeat" || !ack.OK {
		t.Fatalf("unexpected acknowledgement: %+v", ack)
	}
	_ = host.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("EOF did not cancel active input")
	}
	select {
	case <-p.ParentDone():
	case <-time.After(time.Second):
		t.Fatal("input worker did not finish after EOF")
	}
	if started.Load() != 1 {
		t.Fatal("queued input ran after EOF")
	}
}

// Every refused input says why (bma.input.ack.v1 code), so a viewer and the
// operators can tell a stale lease from a slow or uncertain native input.
func TestScreenPipeInputRefusalCarriesItsReason(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{ErrDesktopControl, "control_not_current"},
		{ErrDesktopDraining, "human_takeover_pending"},
		{ErrDesktopInputUnknown, "input_outcome_unknown"},
		{ErrDesktopRecoveryRequired, "recovery_required"},
		{ErrDesktopResizeUnsupported, "resize_unsupported"},
		{context.DeadlineExceeded, "input_timeout"},
	} {
		executor, host := net.Pipe()
		p := NewScreenLivePipe(executor, executor)
		p.SetInputHandler(func(context.Context, DesktopInputMessage) error { return tc.err })
		_ = host.SetDeadline(time.Now().Add(time.Second))
		if err := json.NewEncoder(host).Encode(map[string]any{"type": "screen_input", "id": "i",
			"message": DesktopInputMessage{Packet: json.RawMessage(`{"input":{"type":"pointer"}}`)}}); err != nil {
			t.Fatal(err)
		}
		var ack struct {
			ID   string
			OK   bool
			Code string
		}
		if err := json.NewDecoder(host).Decode(&ack); err != nil {
			t.Fatal(err)
		}
		if ack.OK || ack.Code != tc.code {
			t.Fatalf("%v: ack %+v, want code %s", tc.err, ack, tc.code)
		}
		executor.Close()
		host.Close()
	}
}
