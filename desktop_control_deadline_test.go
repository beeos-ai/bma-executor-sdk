package bmaexecutor

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// screen.control carries an integer-millisecond deadline. Reconstructing its
// monotonic counterpart must not shorten that instant: SignedInput checks the
// unchanged expiry returned to the viewer against that same stored deadline.
func TestScreenControlPreservesWireDeadline(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, renew := range []bool{false, true} {
		name := "grant"
		if renew {
			name = "renew"
		}
		t.Run(name, func(t *testing.T) {
			// Exercise actual wall-clock samples at the wire's millisecond
			// boundary; no expiry adjustment or tolerance makes a packet valid.
			for i := range 32 {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					gate := NewDesktopControlGate(&controlBackend{}, &controlNative{})
					defer gate.Close()
					deadline := time.UnixMilli(time.Now().Add(20 * time.Second).UnixMilli())
					if err := gate.grant(t.Context(), 1, "lease", "viewer", deadline, public, "capture"); err != nil {
						t.Fatal(err)
					}
					if renew {
						deadline = deadline.Add(time.Second)
						if err := gate.grant(t.Context(), 1, "lease", "viewer", deadline, public, "capture"); err != nil {
							t.Fatal(err)
						}
					}
					gate.mu.Lock()
					stored := gate.until
					gate.mu.Unlock()
					if !stored.Equal(deadline) {
						t.Errorf("stored deadline differs from wire deadline by %s", stored.Sub(deadline))
					}
					if stored == stored.Round(0) {
						t.Error("stored deadline lost the local monotonic clock")
					}
					packet := deadlineInputPacket(t, public, private, deadline.UnixMilli(), 1)
					if err := gate.SignedInput(t.Context(), "viewer", packet, 1920, 1080); err != nil {
						t.Errorf("unchanged granted expiry refused: %v", err)
					}
					later := deadlineInputPacket(t, public, private, deadline.Add(time.Millisecond).UnixMilli(), 2)
					if err := gate.SignedInput(t.Context(), "viewer", later, 1920, 1080); err == nil {
						t.Error("expiry beyond the granted deadline accepted")
					}
					expired := deadlineInputPacket(t, public, private, time.Now().Add(-time.Millisecond).UnixMilli(), 2)
					if err := gate.SignedInput(t.Context(), "viewer", expired, 1920, 1080); err == nil {
						t.Error("expired signed input accepted")
					}
				})
			}
		})
	}
}

func deadlineInputPacket(t *testing.T, public ed25519.PublicKey, private ed25519.PrivateKey, expiry int64, sequence uint64) json.RawMessage {
	t.Helper()
	var signed bytes.Buffer
	signed.WriteString("beeos.desktop.input.v1\x00")
	put := func(value any) {
		if err := binary.Write(&signed, binary.BigEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"1", "lease", "viewer"} {
		put(uint64(len(value)))
		signed.WriteString(value)
	}
	put(uint64(1))
	put(uint64(expiry))
	signed.Write(public)
	put(sequence)
	put(uint8(2))
	put(uint32(0x61))
	put(uint8(1))
	packet := DesktopSignedInput{Grant: DesktopInputGrant{Epoch: "1", Generation: 1, LeaseID: "lease", Holder: "viewer",
		InputKey: public, ExpiresAt: uint64(expiry)}, Sequence: sequence,
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signed.Bytes()))}
	packet.Input.Type, packet.Input.Keysym, packet.Input.Down = "key", 0x61, true
	raw, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
