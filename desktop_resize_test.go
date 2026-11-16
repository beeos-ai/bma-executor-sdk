package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type resizeNative struct {
	controlNative
	resize func(context.Context, uint16, uint16) error
}

func (n *resizeNative) Resize(ctx context.Context, w, h uint16) error { return n.resize(ctx, w, h) }

// This encoder follows the published bytes independently of the SDK parser.
func signedResize(t *testing.T, private ed25519.PrivateKey, expires time.Time, sequence uint64, w, h uint16) []byte {
	t.Helper()
	public := private.Public().(ed25519.PublicKey)
	var payload bytes.Buffer
	payload.WriteString("beeos.desktop.input.v1\x00")
	put := func(v any) {
		if err := binary.Write(&payload, binary.BigEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"1", "lease", "viewer"} {
		put(uint64(len(s)))
		payload.WriteString(s)
	}
	put(uint64(1))
	put(uint64(expires.UnixMilli()))
	payload.Write(public)
	put(sequence)
	put(uint8(4))
	put(w)
	put(h)
	key := make([]int, len(public))
	for i, b := range public {
		key[i] = int(b)
	}
	packet := map[string]any{"grant": map[string]any{"epoch": "1", "generation": 1, "lease_id": "lease",
		"holder": "viewer", "input_key": key, "expires_at": expires.UnixMilli()}, "sequence": sequence,
		"input":     map[string]any{"type": "resize", "width": w, "height": h},
		"signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload.Bytes()))}
	raw, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func resizeGrant(t *testing.T, native NativeDesktopInput, until time.Time) (*DesktopControlGate, ed25519.PrivateKey) {
	t.Helper()
	private := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	g := NewDesktopControlGate(&controlBackend{}, native)
	if err := g.grant(t.Context(), 1, "lease", "viewer", until, private.Public().(ed25519.PublicKey), "capture"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g, private
}

func TestSignedResizeBindsDimensionsAndUsesExistingInputGate(t *testing.T) {
	var calls atomic.Int32
	n := &resizeNative{resize: func(_ context.Context, w, h uint16) error {
		if w != 1280 || h != 720 {
			return errors.New("wrong dimensions")
		}
		calls.Add(1)
		return nil
	}}
	until := time.Now().Add(20 * time.Second)
	g, private := resizeGrant(t, n, until)
	raw := signedResize(t, private, until, 1, 1280, 720)
	if err := g.SignedInput(t.Context(), "intruder", raw, 1920, 1080); err == nil {
		t.Fatal("foreign participant resized")
	}
	for _, dimension := range []string{"width", "height"} {
		var packet map[string]any
		_ = json.Unmarshal(raw, &packet)
		packet["input"].(map[string]any)[dimension] = 1000
		tampered, _ := json.Marshal(packet)
		if err := g.SignedInput(t.Context(), "viewer", tampered, 1920, 1080); err == nil {
			t.Fatal("unsigned dimension accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached native resize")
	}
	if err := g.SignedInput(t.Context(), "viewer", raw, 1920, 1080); err != nil {
		t.Fatal("signed resize rejected:", err)
	}
	if calls.Load() != 1 {
		t.Fatal("resize was not applied exactly once")
	}
	if err := g.SignedInput(t.Context(), "viewer", raw, 1920, 1080); err == nil {
		t.Fatal("resize replay accepted")
	}
	if _, err := g.Action(t.Context(), ComputerActionRequest{SessionID: "agent"}); !errors.Is(err, ErrDesktopObservation) {
		t.Fatal("resize gave agent input authority:", err)
	}
	if err := g.HumanInput(t.Context(), 1, "lease", "viewer", 2, DesktopInputEvent{Type: "key", Keysym: 65, Down: true}); err != nil {
		t.Fatal("resize replaced the keyboard gate:", err)
	}
}

func TestSignedResizeUnsupportedPreservesLeaseButFailureBlocks(t *testing.T) {
	until := time.Now().Add(20 * time.Second)
	n := &controlNative{}
	g, private := resizeGrant(t, n, until)
	err := g.SignedInput(t.Context(), "viewer", signedResize(t, private, until, 1, 640, 480), 1920, 1080)
	if err == nil || err.Error() != "desktop_resize_unsupported" {
		t.Fatalf("unsupported resize: %v", err)
	}
	if err := g.HumanInput(t.Context(), 1, "lease", "viewer", 2, DesktopInputEvent{Type: "key", Keysym: 65, Down: true}); err != nil {
		t.Fatal("unsupported resize disabled keyboard:", err)
	}
	failing := &resizeNative{resize: func(context.Context, uint16, uint16) error { return errors.New("native resize failed") }}
	blocked, private := resizeGrant(t, failing, until)
	err = blocked.SignedInput(t.Context(), "viewer", signedResize(t, private, until, 1, 640, 480), 1920, 1080)
	if !errors.Is(err, ErrDesktopInputUnknown) {
		t.Fatalf("actual failure was hidden as unsupported: %v", err)
	}
	if err := blocked.Grant(t.Context(), 2, "new", "other", until); !errors.Is(err, ErrDesktopRecoveryRequired) {
		t.Fatal("failed resize allowed new owner:", err)
	}
}

func TestResizeReleaseCancelsAndWaitsForNativeExit(t *testing.T) {
	entered, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	n := &resizeNative{resize: func(ctx context.Context, _, _ uint16) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-finish
		return ctx.Err()
	}}
	until := time.Now().Add(20 * time.Second)
	g, private := resizeGrant(t, n, until)
	finished := false
	t.Cleanup(func() {
		if !finished {
			close(finish)
		}
	})
	input := make(chan error, 1)
	go func() {
		input <- g.SignedInput(t.Context(), "viewer", signedResize(t, private, until, 1, 640, 480), 1920, 1080)
	}()
	select {
	case <-entered:
	case err := <-input:
		t.Fatalf("resize did not enter native operation: %v", err)
	}
	released := make(chan error, 1)
	go func() { released <- g.Release(t.Context(), 1, "lease", false) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("release did not cancel native resize")
	}
	select {
	case err := <-released:
		t.Fatalf("release completed before native exit: %v", err)
	default:
	}
	if err := g.Grant(t.Context(), 2, "new", "other", until); err == nil {
		t.Fatal("new owner crossed unfinished resize")
	}
	close(finish)
	finished = true
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if err := <-input; !errors.Is(err, ErrDesktopInputUnknown) {
		t.Fatal("cancelled resize reported success:", err)
	}
}

func TestResizeLeaseExpiryCancelsNativeOperation(t *testing.T) {
	entered := make(chan struct{})
	n := &resizeNative{resize: func(ctx context.Context, _, _ uint16) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	until := time.Now().Add(200 * time.Millisecond)
	g, private := resizeGrant(t, n, until)
	input := make(chan error, 1)
	go func() {
		input <- g.SignedInput(t.Context(), "viewer", signedResize(t, private, until, 1, 640, 480), 1920, 1080)
	}()
	select {
	case <-entered:
	case err := <-input:
		t.Fatalf("resize did not enter native operation: %v", err)
	}
	select {
	case err := <-input:
		if !errors.Is(err, ErrDesktopInputUnknown) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native resize survived lease expiry")
	}
}
