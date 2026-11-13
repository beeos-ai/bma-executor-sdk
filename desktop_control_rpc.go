package bmaexecutor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const DesktopControlMethod = "screen.control"

type DesktopControlCommand struct {
	SessionID   string `json:"session_id"`
	Action      string `json:"action"`
	MachineID   string `json:"machine_id"`
	Epoch       uint64 `json:"epoch"`
	LeaseID     string `json:"lease_id"`
	Identity    string `json:"identity,omitempty"`
	InputKey    string `json:"input_key,omitempty"`
	ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
}
type DesktopInputMessage struct {
	Epoch    uint64            `json:"epoch"`
	LeaseID  string            `json:"lease_id"`
	Identity string            `json:"identity"`
	Sequence uint64            `json:"seq"`
	Event    DesktopInputEvent `json:"event"`
	Packet   json.RawMessage   `json:"packet,omitempty"`
	Width    float64           `json:"width,omitempty"`
	Height   float64           `json:"height,omitempty"`
}

func (c *Client) ServeDesktopControl(g *DesktopControlGate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g == nil || g.native == nil || c.options.MachineID == "" || c.state != ConnectionStateDisconnected {
		return errors.New("desktop_control_requires_machine_host")
	}
	var display map[string]any
	_ = json.Unmarshal(c.options.Capabilities.Display, &display)
	if display == nil {
		display = map[string]any{}
	}
	display["screen_control"] = true
	c.options.Capabilities.Display, _ = json.Marshal(display)
	c.requestHandlers[DesktopControlMethod] = decodeThen(func(ctx context.Context, p DesktopControlCommand) (map[string]bool, error) {
		if p.MachineID != c.options.MachineID {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid_control_binding"}
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var err error
		switch p.Action {
		case "grant", "renew":
			key, e := base64.RawURLEncoding.DecodeString(p.InputKey)
			if e != nil || len(key) != 32 {
				return nil, ErrDesktopControl
			}
			err = g.grant(ctx, p.Epoch, p.LeaseID, p.Identity, time.UnixMilli(p.ExpiresAtMS), key, p.SessionID)
		case "release", "handback":
			err = g.Release(ctx, p.Epoch, p.LeaseID, p.Action == "handback")
		default:
			err = ErrDesktopControl
		}
		if err != nil {
			return nil, err
		}
		return map[string]bool{"accepted": true}, nil
	})
	return nil
}
