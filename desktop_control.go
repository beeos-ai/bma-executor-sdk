package bmaexecutor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"
)

var ErrDesktopControl = errors.New("desktop_control_not_current")
var ErrDesktopObservation = errors.New("desktop_observation_required")
var ErrDesktopDraining = errors.New("human_takeover_pending")
var ErrDesktopInputUnknown = errors.New("desktop_input_outcome_unknown")
var ErrDesktopRecoveryRequired = errors.New("desktop_input_recovery_required")

// ComputerInputRecovery is an optional trusted driver capability. Success must
// mean all dispatched Agent actions have stopped AND that driver's held input
// has been cleared. An observation, CLI exit, or human ReleaseAll is not proof.
type ComputerInputRecovery interface{ RecoverInput(context.Context) error }

// NativeDesktopInput must perform one bounded synchronous native operation.
// ReleaseAll releases only keys/buttons this executor injected, not local input.
type NativeDesktopInput interface {
	Apply(context.Context, DesktopInputEvent) error
	ReleaseAll(context.Context) error
}
type DesktopInputEvent struct {
	Type    string  `json:"type"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	W       float64 `json:"w,omitempty"`
	H       float64 `json:"h,omitempty"`
	Buttons uint8   `json:"buttons,omitempty"`
	Keysym  uint32  `json:"keysym,omitempty"`
	Down    bool    `json:"down,omitempty"`
	DY      float64 `json:"dy,omitempty"`
}

// DesktopControlGate guards the physical input device, not model/file work.
// The platform alone grants authority. Media IPC may only use that authority.
type DesktopControlGate struct {
	backend                ComputerUseBackend
	native                 NativeDesktopInput
	mu                     sync.Mutex
	input                  chan struct{}
	epoch                  uint64
	lastSeq                uint64
	key                    []byte
	mediaID                string
	lease, identity, state string
	until, heartbeat       time.Time
	observed               map[string]uint64
	resume                 bool
	timer                  *time.Timer
	closed                 bool
	agentUnknown           bool
}

func NewDesktopControlGate(backend ComputerUseBackend, native NativeDesktopInput) *DesktopControlGate {
	return &DesktopControlGate{backend: backend, native: native, input: make(chan struct{}, 1), state: "agent", observed: map[string]uint64{}}
}
func (g *DesktopControlGate) lock(ctx context.Context) error {
	select {
	case g.input <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g *DesktopControlGate) unlock() { <-g.input }
func (g *DesktopControlGate) Grant(ctx context.Context, epoch uint64, lease, identity string, deadline time.Time) error {
	return g.grant(ctx, epoch, lease, identity, deadline, nil, "")
}
func (g *DesktopControlGate) grant(ctx context.Context, epoch uint64, lease, identity string, deadline time.Time, key []byte, mediaID string) error {
	now := time.Now()
	if g.native == nil || epoch == 0 || lease == "" || identity == "" || !deadline.After(now) || deadline.After(now.Add(30*time.Second)) {
		return ErrDesktopControl
	}
	g.mu.Lock()
	if g.state == "blocked" {
		g.mu.Unlock()
		return ErrDesktopRecoveryRequired
	}
	if g.closed || epoch < g.epoch {
		g.mu.Unlock()
		return ErrDesktopControl
	}
	if epoch == g.epoch {
		valid := g.state == "human" && g.lease == lease && g.identity == identity && bytes.Equal(g.key, key) && g.mediaID == mediaID && now.Before(g.until) && now.Before(g.heartbeat)
		if valid {
			g.until = now.Add(time.Until(deadline))
			g.armLocked()
		}
		g.mu.Unlock()
		if valid {
			return nil
		}
		return ErrDesktopControl
	}
	// A second controller cannot silently replace an active or draining one.
	if g.state == "human" || g.state == "draining" || g.state == "releasing" {
		g.mu.Unlock()
		return ErrDesktopControl
	}
	g.epoch = epoch
	g.state = "draining"
	g.resume = false
	g.observed = map[string]uint64{}
	g.mu.Unlock()
	drain, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := g.lock(drain); err != nil {
		g.mu.Lock()
		if g.epoch == epoch && g.state == "draining" {
			g.state = "released"
		}
		g.mu.Unlock()
		return ErrDesktopDraining
	}
	defer g.unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.epoch != epoch || g.state != "draining" || !deadline.After(time.Now()) {
		return ErrDesktopControl
	}
	if err := g.native.ReleaseAll(drain); err != nil {
		g.state = "blocked"
		return ErrDesktopInputUnknown
	}
	g.lastSeq = 0
	g.key = append([]byte(nil), key...)
	g.mediaID = mediaID
	g.lease = lease
	g.identity = identity
	g.until = time.Now().Add(time.Until(deadline))
	g.heartbeat = time.Now().Add(5 * time.Second)
	g.state = "human"
	g.armLocked()
	return nil
}
func (g *DesktopControlGate) armLocked() {
	if g.timer != nil {
		g.timer.Stop()
	}
	end := g.until
	if g.heartbeat.Before(end) {
		end = g.heartbeat
	}
	epoch, lease := g.epoch, g.lease
	g.timer = time.AfterFunc(time.Until(end), func() {
		g.mu.Lock()
		due := g.epoch == epoch && g.lease == lease && g.state == "human" && (!time.Now().Before(g.until) || !time.Now().Before(g.heartbeat))
		g.mu.Unlock()
		if due {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = g.release(ctx, epoch, lease, false, true)
		}
	})
}
func (g *DesktopControlGate) Release(ctx context.Context, epoch uint64, lease string, resume bool) error {
	return g.release(ctx, epoch, lease, resume, false)
}
func (g *DesktopControlGate) release(ctx context.Context, epoch uint64, lease string, resume, expiredOnly bool) error {
	g.mu.Lock()
	if expiredOnly && (g.state != "human" || time.Now().Before(g.until) && time.Now().Before(g.heartbeat)) {
		g.mu.Unlock()
		return nil
	}
	if epoch != g.epoch || (g.lease != "" && g.lease != lease) {
		g.mu.Unlock()
		return ErrDesktopControl
	}
	if g.state == "releasing" {
		g.mu.Unlock()
		return ErrDesktopControl
	}
	if g.state == "released" {
		allowed := !resume || g.resume
		g.mu.Unlock()
		if !allowed {
			return ErrDesktopControl
		}
		return nil
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	if g.state == "blocked" {
		g.mu.Unlock()
		// Release may retry native cleanup, but never implicitly resume an Agent.
		if resume {
			return ErrDesktopRecoveryRequired
		}
		return g.recover(ctx, epoch, lease)
	}
	g.state = "releasing"
	g.observed = map[string]uint64{}
	g.resume = false
	g.mu.Unlock()
	if err := g.lock(ctx); err != nil {
		g.mu.Lock()
		g.state = "blocked"
		g.mu.Unlock()
		return ErrDesktopInputUnknown
	}
	defer g.unlock()
	// An in-flight Agent call can become unknown while release waits on input.
	// Recheck after draining; human cleanup cannot clear that driver's state.
	g.mu.Lock()
	unknown := g.agentUnknown
	if unknown {
		g.state = "blocked"
	}
	g.mu.Unlock()
	if unknown {
		return ErrDesktopRecoveryRequired
	}
	err := g.native.ReleaseAll(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.key = nil
	g.identity = ""
	if err != nil {
		g.state = "blocked"
		return ErrDesktopInputUnknown
	}
	g.state = "released"
	g.resume = resume
	return nil
}
func (g *DesktopControlGate) Close() {
	g.mu.Lock()
	g.closed = true
	epoch, lease := g.epoch, g.lease
	g.mu.Unlock()
	if g.native != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = g.Release(ctx, epoch, lease, false)
		if closer, ok := g.native.(io.Closer); ok {
			_ = closer.Close()
		}
	}
}
func (g *DesktopControlGate) Observe(ctx context.Context, r ComputerObserveRequest) ([]ContentBlock, error) {
	g.mu.Lock()
	epoch := g.epoch
	eligible := !g.closed && g.state == "released" && g.resume
	g.mu.Unlock()
	blocks, err := g.backend.Observe(ctx, r)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if eligible && !g.closed && g.epoch == epoch && g.state == "released" && g.resume {
		g.observed[r.SessionID] = epoch
	}
	g.mu.Unlock()
	return blocks, nil
}
func (g *DesktopControlGate) Action(ctx context.Context, r ComputerActionRequest) ([]ContentBlock, error) {
	g.mu.Lock()
	epoch := g.epoch
	g.mu.Unlock()
	if err := g.lock(ctx); err != nil {
		return nil, err
	}
	defer g.unlock()
	g.mu.Lock()
	valid := !g.closed && g.epoch == epoch && (g.state == "agent" || g.state == "released" && g.resume && g.observed[r.SessionID] == epoch)
	state := g.state
	g.mu.Unlock()
	if !valid {
		if state == "blocked" {
			return nil, ErrDesktopRecoveryRequired
		}
		if state == "draining" {
			return nil, ErrDesktopDraining
		}
		return nil, ErrDesktopObservation
	}
	blocks, err := g.backend.Action(ctx, r)
	// A driver error may follow dispatch; cancellation is not proof the native
	// action ended. Refuse takeover until this executor has been recovered.
	if err != nil && !errors.Is(err, ErrComputerActionRejected) {
		g.mu.Lock()
		g.state = "blocked"
		g.agentUnknown = true
		g.resume = false
		g.observed = map[string]uint64{}
		g.mu.Unlock()
		return blocks, fmt.Errorf("%w: %w", ErrDesktopRecoveryRequired, err)
	}
	return blocks, err
}

// Recover does not grant input or resume tasks. A new platform grant and
// explicit handback are required afterwards. Unknown Agent actions require
// the Agent driver's own quiescence proof, separately from human input cleanup.
func (g *DesktopControlGate) Recover(ctx context.Context) error {
	g.mu.Lock()
	epoch, lease := g.epoch, g.lease
	g.mu.Unlock()
	return g.recover(ctx, epoch, lease)
}
func (g *DesktopControlGate) recover(ctx context.Context, epoch uint64, lease string) error {
	if err := g.lock(ctx); err != nil {
		return err
	}
	defer g.unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.state != "blocked" || g.epoch != epoch || g.lease != lease {
		return ErrDesktopControl
	}
	if g.agentUnknown {
		recovery, ok := g.backend.(ComputerInputRecovery)
		if !ok {
			return ErrDesktopRecoveryRequired
		}
		if err := recovery.RecoverInput(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrDesktopRecoveryRequired, err)
		}
	}
	if g.native == nil {
		return ErrDesktopRecoveryRequired
	}
	if err := g.native.ReleaseAll(ctx); err != nil {
		return ErrDesktopRecoveryRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	g.key = nil
	g.identity = ""
	g.agentUnknown = false
	g.resume = false
	g.observed = map[string]uint64{}
	g.state = "released"
	return nil
}

// HumanInput receives the authenticated LiveKit participant identity from the
// capture host, never from the payload. Heartbeats cannot extend the platform lease.
func (g *DesktopControlGate) HumanInput(ctx context.Context, epoch uint64, lease, identity string, seq uint64, event DesktopInputEvent) error {
	if err := validateDesktopInput(event); err != nil {
		return err
	}
	if err := g.lock(ctx); err != nil {
		return err
	}
	defer g.unlock()
	g.mu.Lock()
	valid := !g.closed && g.state == "human" && epoch == g.epoch && lease == g.lease && identity == g.identity && seq > g.lastSeq && seq-g.lastSeq <= 4096 && time.Now().Before(g.until) && time.Now().Before(g.heartbeat)
	if valid {
		g.lastSeq = seq
		g.heartbeat = time.Now().Add(5 * time.Second)
		g.armLocked()
	}
	g.mu.Unlock()
	if !valid {
		return ErrDesktopControl
	}
	if event.Type == "heartbeat" {
		return nil
	}
	var err error
	if event.Type == "release_all" {
		err = g.native.ReleaseAll(ctx)
	} else {
		err = g.native.Apply(ctx, event)
	}
	if err != nil {
		g.mu.Lock()
		g.state = "blocked"
		g.mu.Unlock()
		_ = g.native.ReleaseAll(context.WithoutCancel(ctx))
		return ErrDesktopInputUnknown
	}
	return nil
}
func validateDesktopInput(e DesktopInputEvent) error {
	switch e.Type {
	case "heartbeat", "release_all":
		return nil
	case "key":
		if e.Keysym == 0 || e.Keysym > 0x0110ffff {
			return ErrDesktopControl
		}
		return nil
	case "pointer", "wheel":
		if math.IsNaN(e.X) || math.IsNaN(e.Y) || math.IsNaN(e.W) || math.IsNaN(e.H) || math.IsNaN(e.DY) || e.W < 1 || e.H < 1 || e.W > 16384 || e.H > 16384 || e.X < 0 || e.Y < 0 || e.X >= e.W || e.Y >= e.H || e.Buttons > 7 || e.DY < -8 || e.DY > 8 {
			return ErrDesktopControl
		}
		return nil
	}
	return ErrDesktopControl
}

// Heartbeat proves controller liveness only; it cannot renew platform authority.
func (g *DesktopControlGate) Heartbeat(epoch uint64, lease, identity string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.state != "human" || g.epoch != epoch || g.lease != lease || g.identity != identity || !time.Now().Before(g.until) || !time.Now().Before(g.heartbeat) {
		return ErrDesktopControl
	}
	g.heartbeat = time.Now().Add(5 * time.Second)
	g.armLocked()
	if worker, ok := g.native.(*nativeInputProcess); ok {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return worker.Apply(ctx, DesktopInputEvent{Type: "heartbeat"})
	}
	return nil
}
func (g *DesktopControlGate) StopMedia(ctx context.Context, id string) error {
	g.mu.Lock()
	epoch, lease := g.epoch, g.lease
	matches := g.mediaID == id
	g.mu.Unlock()
	if !matches {
		return nil
	}
	return g.Release(ctx, epoch, lease, false)
}
