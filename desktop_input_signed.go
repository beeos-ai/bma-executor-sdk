package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"time"
)

type DesktopInputGrant struct {
	Epoch      string `json:"epoch"`
	Generation uint64 `json:"generation"`
	LeaseID    string `json:"lease_id"`
	Holder     string `json:"holder"`
	InputKey   []byte `json:"input_key"`
	ExpiresAt  uint64 `json:"expires_at"`
}
type DesktopSignedInput struct {
	Grant    DesktopInputGrant `json:"grant"`
	Sequence uint64            `json:"sequence"`
	Input    struct {
		Type    string `json:"type"`
		X       uint16 `json:"x"`
		Y       uint16 `json:"y"`
		Buttons uint8  `json:"buttons"`
		Keysym  uint32 `json:"keysym"`
		Down    bool   `json:"down"`
		Width   uint16 `json:"width"`
		Height  uint16 `json:"height"`
		// Scale is the viewer's devicePixelRatio × 100 of a resize (100–400); 0 = not sent.
		Scale uint16 `json:"scale,omitempty"`
		Text  string `json:"text"`
	} `json:"input"`
	Signature string `json:"signature"`
}

func (g *DesktopControlGate) SignedInput(ctx context.Context, identity string, raw json.RawMessage, w, h float64) error {
	if len(raw) > 16384 {
		return ErrDesktopControl
	}
	var p DesktopSignedInput
	if json.Unmarshal(raw, &p) != nil {
		return ErrDesktopControl
	}
	epoch, err := strconv.ParseUint(p.Grant.Epoch, 10, 64)
	if err != nil {
		return ErrDesktopControl
	}
	g.mu.Lock()
	key := append([]byte(nil), g.key...)
	until := g.until
	g.mu.Unlock()
	if len(key) != ed25519.PublicKeySize || !bytes.Equal(key, p.Grant.InputKey) || p.Grant.Holder != identity || p.Grant.Generation != 1 || p.Grant.ExpiresAt > uint64(until.UnixMilli()) || p.Grant.ExpiresAt <= uint64(time.Now().UnixMilli()) {
		return ErrDesktopControl
	}
	payload, err := desktopInputPayload(p, key)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(p.Signature)
	if err != nil || !ed25519.Verify(key, payload, signature) {
		return ErrDesktopControl
	}
	event := DesktopInputEvent{Type: p.Input.Type, X: float64(p.Input.X), Y: float64(p.Input.Y), W: w, H: h, Buttons: p.Input.Buttons, Keysym: p.Input.Keysym, Down: p.Input.Down, Width: p.Input.Width, Height: p.Input.Height, Scale: p.Input.Scale, Text: p.Input.Text}
	if event.Type == "pointer" && (event.Buttons == 8 || event.Buttons == 16) {
		event.Type = "wheel"
		event.DY = 1
		if event.Buttons == 8 {
			event.DY = -1
		}
		event.Buttons = 0
	}
	return g.HumanInput(ctx, epoch, p.Grant.LeaseID, identity, p.Sequence, event)
}

// SigningBytes are the bytes the packet's Ed25519 signature covers, with the
// grant's own input key (contracts/bma/desktop-relay.md §5b).
func (p DesktopSignedInput) SigningBytes() ([]byte, error) {
	return desktopInputPayload(p, p.Grant.InputKey)
}

// desktopInputPayload preserves the existing v1 bytes; resize adds tag 4 and
// its two uint16 dimensions (tag 6 and a third uint16, the scale, when the
// resize carries one), text tag 5 with a uint16 length and its UTF-8.
func desktopInputPayload(p DesktopSignedInput, key []byte) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("beeos.desktop.input.v1\x00")
	put := func(v any) { _ = binary.Write(&b, binary.BigEndian, v) }
	for _, s := range []string{p.Grant.Epoch, p.Grant.LeaseID, p.Grant.Holder} {
		if len(s) < 1 || len(s) > 128 {
			return nil, ErrDesktopControl
		}
		put(uint64(len(s)))
		b.WriteString(s)
	}
	put(p.Grant.Generation)
	put(p.Grant.ExpiresAt)
	b.Write(key)
	put(p.Sequence)
	switch p.Input.Type {
	case "pointer":
		put(uint8(1))
		put(p.Input.X)
		put(p.Input.Y)
		put(p.Input.Buttons)
	case "key":
		put(uint8(2))
		put(p.Input.Keysym)
		v := uint8(0)
		if p.Input.Down {
			v = 1
		}
		put(v)
	case "release_all":
		put(uint8(3))
	case "resize":
		if p.Input.Scale == 0 {
			put(uint8(4))
			put(p.Input.Width)
			put(p.Input.Height)
			break
		}
		if p.Input.Scale < DesktopScaleMin || p.Input.Scale > DesktopScaleMax {
			return nil, ErrDesktopControl
		}
		put(uint8(6))
		put(p.Input.Width)
		put(p.Input.Height)
		put(p.Input.Scale)
	case "text":
		if len(p.Input.Text) < 1 || len(p.Input.Text) > DesktopTextMaxBytes {
			return nil, ErrDesktopControl
		}
		put(uint8(5))
		put(uint16(len(p.Input.Text)))
		b.WriteString(p.Input.Text)
	default:
		return nil, ErrDesktopControl
	}
	return b.Bytes(), nil
}
