package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"
)

func TestSignedInputBindsLeaseKeyParticipantAndSequence(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n := &controlNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	deadline := time.Now().Add(20 * time.Second)
	if err = g.Grant(ctx, 1, "lease", "viewer", deadline); err != nil {
		t.Fatal(err)
	}
	g.key = pub
	var payload bytes.Buffer
	payload.WriteString("beeos.desktop.input.v1\x00")
	put := func(v any) { _ = binary.Write(&payload, binary.BigEndian, v) }
	for _, s := range []string{"1", "lease", "viewer"} {
		put(uint64(len(s)))
		payload.WriteString(s)
	}
	put(uint64(1))
	put(uint64(deadline.UnixMilli()))
	payload.Write(pub)
	put(uint64(1))
	put(uint8(2))
	put(uint32(0xffe1))
	put(uint8(1))
	numbers := []int{}
	for _, b := range pub {
		numbers = append(numbers, int(b))
	}
	packet := map[string]any{"grant": map[string]any{"epoch": "1", "generation": 1, "lease_id": "lease", "holder": "viewer", "input_key": numbers, "expires_at": deadline.UnixMilli()}, "sequence": 1, "input": map[string]any{"type": "key", "keysym": 0xffe1, "down": true}, "signature": base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload.Bytes()))}
	raw, _ := json.Marshal(packet)
	if err = g.SignedInput(ctx, "intruder", raw, 1920, 1080); err == nil {
		t.Fatal("wrong LiveKit identity accepted")
	}
	packet["sequence"] = 2
	tampered, _ := json.Marshal(packet)
	if err = g.SignedInput(ctx, "viewer", tampered, 1920, 1080); err == nil {
		t.Fatal("unsigned sequence modification accepted")
	}
	if err = g.SignedInput(ctx, "viewer", raw, 1920, 1080); err != nil {
		t.Fatal(err)
	}
	if err = g.SignedInput(ctx, "viewer", raw, 1920, 1080); err == nil {
		t.Fatal("signed replay accepted")
	}
	if err = g.Release(ctx, 1, "lease", false); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	held := n.held
	n.mu.Unlock()
	if held {
		t.Fatal("release left Shift held")
	}
}
