package bmaexecutor

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// testdata/desktop-input.vectors.json is a copy of
// contracts/bma/desktop-input.vectors.json (beeos-managed-agents).
type desktopInputVectors struct {
	Seed    string `json:"test_seed_hex"`
	Control struct {
		Epoch    uint64 `json:"epoch"`
		LeaseID  string `json:"lease_id"`
		Identity string `json:"identity"`
	} `json:"control"`
	Heartbeat struct {
		Message  DesktopInputMessage `json:"message"`
		Identity string              `json:"sender_identity"`
		Ack      vectorAck           `json:"ack"`
	} `json:"heartbeat"`
	Vectors []struct {
		Name      string             `json:"name"`
		Identity  string             `json:"sender_identity"`
		Packet    DesktopSignedInput `json:"packet"`
		Canonical string             `json:"canonical_hex"`
		Expired   bool               `json:"expired"`
		Invalid   bool               `json:"invalid_event"`
		Ack       vectorAck          `json:"ack"`
		AckFixed  *vectorAck         `json:"ack_fixed_display"`
	} `json:"vectors"`
}

type vectorAck struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
}

func readDesktopInputVectors(t *testing.T) desktopInputVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/desktop-input.vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v desktopInputVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDesktopInputVectorsSigningBytes(t *testing.T) {
	v := readDesktopInputVectors(t)
	for _, row := range v.Vectors {
		t.Run(row.Name, func(t *testing.T) {
			payload, err := row.Packet.SigningBytes()
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(payload); got != row.Canonical {
				t.Fatalf("signed bytes differ:\n got %s\nwant %s", got, row.Canonical)
			}
			signature, err := base64.RawURLEncoding.DecodeString(row.Packet.Signature)
			if err != nil || !ed25519.Verify(row.Packet.Grant.InputKey, payload, signature) {
				t.Fatal("vector signature rejected")
			}
		})
	}
}

type vectorNative struct {
	mu     sync.Mutex
	events []DesktopInputEvent
}

func (n *vectorNative) Apply(_ context.Context, e DesktopInputEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
	return nil
}
func (*vectorNative) ReleaseAll(context.Context) error { return nil }

type vectorResizer struct{ vectorNative }

func (*vectorResizer) Resize(context.Context, uint16, uint16) error { return nil }

// The gate, told the vectors' control, answers every vector's ACK in order.
func TestDesktopInputVectorsGate(t *testing.T) {
	v := readDesktopInputVectors(t)
	seed, err := hex.DecodeString(v.Seed)
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	for _, resizes := range []bool{true, false} {
		var native NativeDesktopInput = &vectorNative{}
		if resizes {
			native = &vectorResizer{}
		}
		ctx := context.Background()
		g := NewDesktopControlGate(&controlBackend{}, native)
		deadline := time.Now().Add(20 * time.Second)
		if err := g.grant(ctx, v.Control.Epoch, v.Control.LeaseID, v.Control.Identity, deadline, priv.Public().(ed25519.PublicKey), "capture"); err != nil {
			t.Fatal(err)
		}
		hb := v.Heartbeat
		if err := g.Heartbeat(hb.Message.Epoch, hb.Message.LeaseID, hb.Identity); (err == nil) != hb.Ack.OK {
			t.Fatalf("heartbeat: %v", err)
		}
		for _, row := range v.Vectors {
			p := row.Packet
			p.Grant.ExpiresAt = uint64(deadline.UnixMilli())
			if row.Expired {
				p.Grant.ExpiresAt = uint64(time.Now().Add(-time.Second).UnixMilli())
			}
			payload, err := p.SigningBytes()
			if err != nil {
				t.Fatalf("%s: %v", row.Name, err)
			}
			p.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))
			raw, _ := json.Marshal(p)
			err = g.SignedInput(ctx, row.Identity, raw, 1920, 1080)
			want := row.Ack
			if !resizes && row.AckFixed != nil {
				want = *row.AckFixed
			}
			got := vectorAck{OK: err == nil}
			if errors.Is(err, ErrDesktopResizeUnsupported) {
				got.Code = "resize_unsupported"
			}
			if got != want {
				t.Errorf("resizes=%v %s: ack %+v (%v), want %+v", resizes, row.Name, got, err, want)
			}
		}
		g.Close()
		events := native.(interface{ texts() []string }).texts()
		if len(events) != 3 || events[1] != "中文输入测试" {
			t.Errorf("text reached the native input as %q", events)
		}
	}
}

func (n *vectorNative) texts() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.events {
		if e.Type == "text" {
			out = append(out, e.Text)
		}
	}
	return out
}
