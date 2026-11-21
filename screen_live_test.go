package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestScreenLiveUsesMachineAuthorityAndIsNotATool(t *testing.T) {
	c, err := NewClient(Options{BaseURL: "https://example.test", EnvironmentKey: "test", MachineID: "mch_test", Executor: ExecutorInfo{Name: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	if err := c.ServeScreenLive(func(context.Context, ScreenLiveCommand) (ScreenLiveResult, error) {
		called++
		return ScreenLiveResult{Published: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(c.toolHandlers) != 0 {
		t.Fatal("screen capture became a model tool")
	}
	var display map[string]bool
	_ = json.Unmarshal(c.registeredCapabilities().Display, &display)
	if !display["screen_live"] {
		t.Fatal("capability missing")
	}
	h := c.requestHandlers[ScreenLiveMethod]
	p := ScreenLiveCommand{Type: "screen_live_start", MachineID: "mch_other", SessionID: "s", Room: "r", Generation: 1}
	raw, _ := json.Marshal(p)
	if _, err = h(context.Background(), raw); err == nil || called != 0 {
		t.Fatal("wrong machine admitted")
	}
	p.MachineID = "mch_test"
	raw, _ = json.Marshal(p)
	if _, err = h(context.Background(), raw); err != nil || called != 1 {
		t.Fatalf("correct machine: %v", err)
	}
	for _, bad := range []ScreenLiveCommand{
		{Type: "screen_live_start", Width: 1920},
		{Type: "screen_live_start", Height: 1080},
		{Type: "screen_live_renew", Width: 1920, Height: 1080},
	} {
		bad.MachineID, bad.SessionID, bad.Room, bad.Generation = "mch_test", "s", "r", 1
		raw, _ = json.Marshal(bad)
		if _, err = h(context.Background(), raw); err == nil {
			t.Fatalf("size %dx%d on %s admitted", bad.Width, bad.Height, bad.Type)
		}
	}
	p.Width, p.Height = 1920, 1080
	raw, _ = json.Marshal(p)
	if _, err = h(context.Background(), raw); err != nil || called != 2 {
		t.Fatalf("start with a session size: %v", err)
	}
}

func TestScreenLiveParentAcknowledgesPublication(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	replyReader, replyWriter := io.Pipe()
	defer requestReader.Close()
	defer requestWriter.Close()
	defer replyReader.Close()
	defer replyWriter.Close()
	p := NewScreenLivePipe(replyReader, requestWriter)
	go func() {
		var row struct {
			ID      string            `json:"id"`
			Command ScreenLiveCommand `json:"command"`
		}
		_ = json.NewDecoder(requestReader).Decode(&row)
		_ = json.NewEncoder(replyWriter).Encode(map[string]any{"type": "screen_live_result", "id": row.ID, "ok": true})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := p.Call(ctx, ScreenLiveCommand{Type: "screen_live_start"})
	if err != nil || !r.Published || !r.Accepted {
		t.Fatalf("%+v %v", r, err)
	}
	_ = replyWriter.Close()
	select {
	case <-p.Done():
	case <-ctx.Done():
		t.Fatal("lost parent not detected")
	}
}

func TestScreenLiveBlockedParentWriteHasDeadline(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	replyReader, replyWriter := io.Pipe()
	defer requestReader.Close()
	defer requestWriter.Close()
	defer replyReader.Close()
	defer replyWriter.Close()
	p := NewScreenLivePipe(replyReader, requestWriter)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := p.Call(ctx, ScreenLiveCommand{Type: "screen_live_start"}); err == nil {
		t.Fatal("unread pipe succeeded")
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline blocked by parent write")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("unknown parent command not retired")
	}
}

func TestScreenLiveConcurrentCallsKeepWireOrder(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	replyReader, replyWriter := io.Pipe()
	defer requestReader.Close()
	defer requestWriter.Close()
	defer replyReader.Close()
	defer replyWriter.Close()
	p := NewScreenLivePipe(replyReader, requestWriter)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	peerDone := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(requestReader)
		for i := 1; i <= 16; i++ {
			var row struct {
				ID string `json:"id"`
			}
			if err := decoder.Decode(&row); err != nil {
				peerDone <- err
				return
			}
			if row.ID != fmt.Sprint(i) {
				peerDone <- fmt.Errorf("wire id %s, expected %d", row.ID, i)
				return
			}
			if err := json.NewEncoder(replyWriter).Encode(map[string]any{"type": "screen_live_result", "id": row.ID, "ok": true}); err != nil {
				peerDone <- err
				return
			}
		}
		peerDone <- nil
	}()
	outcomes := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() { _, err := p.Call(ctx, ScreenLiveCommand{Type: "screen_live_renew"}); outcomes <- err }()
	}
	for i := 0; i < 16; i++ {
		if err := <-outcomes; err != nil {
			t.Fatal(err)
		}
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestScreenLiveTimeoutDoesNotEndParentLifeline(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	replyReader, replyWriter := io.Pipe()
	defer requestReader.Close()
	defer replyReader.Close()
	p := NewScreenLivePipe(replyReader, requestWriter)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.Call(ctx, ScreenLiveCommand{Type: "screen_live_start"}); err == nil {
		t.Fatal("expected timeout")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("media lane remained live")
	}
	select {
	case <-p.ParentDone():
		t.Fatal("media timeout killed parent lifeline")
	default:
	}
	replyWriter.Close()
	select {
	case <-p.ParentDone():
	case <-time.After(time.Second):
		t.Fatal("parent EOF did not close lifeline")
	}
}

func TestScreenLiveMissingStartAckStillAllowsStop(t *testing.T) {
	requestReader, requestWriter := io.Pipe()
	replyReader, replyWriter := io.Pipe()
	t.Cleanup(func() {
		requestReader.Close()
		requestWriter.Close()
		replyReader.Close()
		replyWriter.Close()
	})
	p := NewScreenLivePipe(replyReader, requestWriter)
	requests := make(chan ScreenLiveCommand, 2)
	parentDone := make(chan error, 1)
	go func() {
		reader := json.NewDecoder(requestReader)
		for i := 0; i < 2; i++ {
			var row struct {
				ID      string            `json:"id"`
				Command ScreenLiveCommand `json:"command"`
			}
			if err := reader.Decode(&row); err != nil {
				parentDone <- err
				return
			}
			requests <- row.Command
			if i == 1 {
				parentDone <- json.NewEncoder(replyWriter).Encode(map[string]any{
					"type": "screen_live_result", "id": row.ID, "ok": true,
				})
			}
		}
	}()
	start := ScreenLiveCommand{Type: "screen_live_start", SessionID: "view", Room: "room", Generation: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := p.Call(ctx, start); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start: got %v, want missing ACK timeout", err)
	}
	stop := start
	stop.Type = "screen_live_stop"
	cleanup, finish := context.WithTimeout(context.Background(), time.Second)
	defer finish()
	if result, err := p.Call(cleanup, stop); err != nil || !result.Accepted {
		t.Fatalf("stop after missing start ACK: result=%+v error=%v", result, err)
	}
	if err := <-parentDone; err != nil {
		t.Fatal(err)
	}
	if first, second := <-requests, <-requests; first != start || second != stop {
		t.Fatal("parent did not receive start and stop for the same attempt")
	}
}
