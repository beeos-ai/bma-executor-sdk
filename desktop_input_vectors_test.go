package bmaexecutor

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestSignedInputGoldenVectorsKeepExistingTagsAndAddResize(t *testing.T) {
	raw, err := os.ReadFile("testdata/desktop-input.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Name      string             `json:"name"`
			Canonical string             `json:"canonical_hex"`
			Packet    DesktopSignedInput `json:"packet"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Vectors) != 4 {
		t.Fatal("missing original or resize contract vector")
	}
	for _, row := range fixture.Vectors {
		t.Run(row.Name, func(t *testing.T) {
			payload, err := desktopInputPayload(row.Packet, row.Packet.Grant.InputKey)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(payload); got != row.Canonical {
				t.Fatalf("canonical bytes differ: %s", got)
			}
			signature, err := base64.RawURLEncoding.DecodeString(row.Packet.Signature)
			if err != nil || !ed25519.Verify(row.Packet.Grant.InputKey, payload, signature) {
				t.Fatal("golden signature rejected")
			}
		})
	}
}
