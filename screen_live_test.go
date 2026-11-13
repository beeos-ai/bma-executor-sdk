package bmaexecutor

import (
	"context"
	"encoding/json"
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
