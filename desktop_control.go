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
	"unicode/utf8"
)

var ErrDesktopControl = errors.New("desktop_control_not_current")
var ErrDesktopObservation = errors.New("desktop_observation_required")
var ErrDesktopDraining = errors.New("human_takeover_pending")
var ErrDesktopInputUnknown = errors.New("desktop_input_outcome_unknown")

// ErrDesktopResizeUnsupported means no resize was dispatched. Other native
// errors remain uncertain and block input until cleanup is confirmed.
var ErrDesktopResizeUnsupported = errors.New("desktop_resize_unsupported")

// DesktopResizeTimeout bounds the existing native desktop fit operation.
const DesktopResizeTimeout = 15 * time.Second

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

// NativeDesktopResizer is optional. Resize fits the desktop to a viewer of
// width x height device pixels whose devicePixelRatio × 100 is scale (0 = not
// sent: 1×). It must return only after its native operation has ended,
// including after cancellation. Unsupported is a refusal before dispatch; it
// must never represent an uncertain native outcome.
type NativeDesktopResizer interface {
	Resize(ctx context.Context, width, height, scale uint16) error
}

// NativeDesktopRestorer is optional. RestoreDesktop undoes every Resize since
// the previous restore (a no-op when there was none). The gate calls it when a
// human lease ends, after its input is released; it reports its own failures,
// which never change the control outcome.
type NativeDesktopRestorer interface {
	RestoreDesktop(context.Context) error
}

// NativeDesktopLease is optional: an input device that keeps its own exclusive
// control lease, such as a phone whose own agent must stop while a person
// controls it. Hold takes or extends that lease until the human lease's
// deadline: on a grant before the human's first input, and on every renew.
// Drop gives it up when the human lease ends, after the human's input was
// released; resume is true for a handback (the device lets its agent act
// again), false for a release or an expiry. A failed Drop is retried by the
// next release of the same lease.
type NativeDesktopLease interface {
	Hold(ctx context.Context, until time.Time) error
	Drop(ctx context.Context, resume bool) error
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
	Width   uint16  `json:"width,omitempty"`
	Height  uint16  `json:"height,omitempty"`
	// Scale of a resize: the viewer's devicePixelRatio × 100 (DesktopScaleMin–DesktopScaleMax); 0 = not sent (1×).
	Scale uint16 `json:"scale,omitempty"`
	Text  string `json:"text,omitempty"`
}

// The scale a resize may carry (devicePixelRatio × 100): 1× to 4×.
const (
	DesktopScaleMin = 100
	DesktopScaleMax = 400
)

// DesktopTextMaxBytes bounds one text event (UTF-8 bytes).
const DesktopTextMaxBytes = 1024

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
	// dropPending is set while the device lease of the released human lease
	// was not confirmed dropped; dropResume is what that release asked for.
	dropPending, dropResume bool
	releasing               chan struct{}
	timer                   *time.Timer
	closed                  bool
	agentUnknown            bool
	nativeCancel            context.CancelFunc
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
	if g.native == nil || epoch == 0 || lease == "" || identity == "" {
		return rejectDesktopControl(ErrDesktopControl, "invalid_binding", deadline, now)
	}
	if err := desktopControlDeadlineError(deadline, now); err != nil {
		return err
	}
	g.mu.Lock()
	if g.state == "blocked" {
		g.mu.Unlock()
		return rejectDesktopControl(ErrDesktopRecoveryRequired, "recovery_required", deadline, now)
	}
	if g.closed {
		g.mu.Unlock()
		return rejectDesktopControl(ErrDesktopControl, "gate_closed", deadline, now)
	}
	if epoch < g.epoch {
		g.mu.Unlock()
		return rejectDesktopControl(ErrDesktopControl, "stale_epoch", deadline, now)
	}
	if epoch == g.epoch {
		valid := g.state == "human" && g.lease == lease && g.identity == identity && bytes.Equal(g.key, key) && g.mediaID == mediaID && now.Before(g.until) && now.Before(g.heartbeat)
		device, held := g.native.(NativeDesktopLease)
		if valid && !held {
			g.until = now.Add(deadline.Sub(now))
			g.armLocked()
		}
		g.mu.Unlock()
		if !valid {
			return rejectDesktopControl(ErrDesktopControl, "current_lease_mismatch", deadline, now)
		}
		if !held {
			return nil
		}
		// The device lease is extended before the platform lease is: a
		// renew the device refused leaves the old deadline in force.
		if err := device.Hold(ctx, deadline); err != nil {
			return rejectDesktopControl(ErrDesktopControl, "native_lease_refused", deadline, time.Now())
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.epoch != epoch || g.state != "human" || g.lease != lease {
			return rejectDesktopControl(ErrDesktopControl, "state_changed_during_renew", deadline, time.Now())
		}
		g.until = now.Add(deadline.Sub(now))
		g.armLocked()
		return nil
	}
	// A second controller cannot silently replace an active or draining one.
	if g.state == "human" || g.state == "draining" || g.state == "releasing" {
		g.mu.Unlock()
		return rejectDesktopControl(ErrDesktopControl, "controller_busy", deadline, now)
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
		return rejectDesktopControl(ErrDesktopDraining, "drain_failed", deadline, time.Now())
	}
	defer g.unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	drainNow := time.Now()
	if g.closed || g.epoch != epoch || g.state != "draining" {
		return rejectDesktopControl(ErrDesktopControl, "state_changed_during_drain", deadline, drainNow)
	}
	if !deadline.After(drainNow) {
		return rejectDesktopControl(ErrDesktopControl, "deadline_expired_after_drain", deadline, drainNow)
	}
	if err := g.native.ReleaseAll(drain); err != nil {
		g.state = "blocked"
		return rejectDesktopControl(ErrDesktopInputUnknown, "native_release_failed", deadline, time.Now())
	}
	if device, ok := g.native.(NativeDesktopLease); ok {
		// The device stops its own agent before the human's first input.
		if err := device.Hold(ctx, deadline); err != nil {
			// The device may hold its lease anyway: the release of this
			// lease drops it again.
			g.state, g.lease, g.key, g.identity = "released", lease, nil, ""
			g.dropPending, g.dropResume = true, false
			return rejectDesktopControl(ErrDesktopControl, "native_lease_refused", deadline, time.Now())
		}
		g.dropPending = false
	}
	g.lastSeq = 0
	g.key = append([]byte(nil), key...)
	g.mediaID = mediaID
	g.lease = lease
	g.identity = identity
	g.until = now.Add(deadline.Sub(now))
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
		// A lease that is no longer current holds no input: releasing it is
		// done. Handing it back would resume the Agent under another lease.
		if resume {
			return ErrDesktopControl
		}
		return nil
	}
	if g.state == "releasing" {
		// A duplicate release of this lease answers the outcome of the one
		// in flight instead of refusing it.
		done := g.releasing
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		return g.release(ctx, epoch, lease, resume, expiredOnly)
	}
	if g.state == "released" {
		if g.dropPending && resume == g.dropResume {
			// The device lease of this release was not confirmed dropped.
			device := g.native.(NativeDesktopLease)
			g.mu.Unlock()
			if err := device.Drop(ctx, resume); err != nil {
				return ErrDesktopInputUnknown
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.epoch == epoch && g.lease == lease && g.state == "released" && g.dropPending {
				g.dropPending, g.resume = false, resume
			}
			return nil
		}
		allowed := (!resume || g.resume) && !g.dropPending
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
		cancel := g.nativeCancel
		g.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		// Release may retry native cleanup, but never implicitly resume an Agent.
		if resume {
			return ErrDesktopRecoveryRequired
		}
		return g.recover(ctx, epoch, lease)
	}
	g.state = "releasing"
	done := make(chan struct{})
	g.releasing = done
	defer close(done)
	g.observed = map[string]uint64{}
	g.resume = false
	cancel := g.nativeCancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	if err == nil {
		g.restoreDesktop(ctx)
	}
	var dropErr error
	if device, ok := g.native.(NativeDesktopLease); ok && err == nil {
		dropErr = device.Drop(ctx, resume)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.key = nil
	g.identity = ""
	if err != nil {
		g.state = "blocked"
		return ErrDesktopInputUnknown
	}
	g.state = "released"
	if dropErr != nil {
		// No human input is admitted any more; the release is retried for
		// the device lease only.
		g.resume, g.dropPending, g.dropResume = false, true, resume
		return ErrDesktopInputUnknown
	}
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
	g.restoreDesktop(ctx)
	if device, ok := g.native.(NativeDesktopLease); ok {
		if err := device.Drop(ctx, false); err != nil {
			return ErrDesktopRecoveryRequired
		}
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
	g.resume, g.dropPending = false, false
	g.observed = map[string]uint64{}
	g.state = "released"
	return nil
}

// restoreDesktop runs with the input lock held, so no resize interleaves.
func (g *DesktopControlGate) restoreDesktop(ctx context.Context) {
	if restorer, ok := g.native.(NativeDesktopRestorer); ok {
		_ = restorer.RestoreDesktop(ctx)
	}
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
	resizer, canResize := g.native.(NativeDesktopResizer)
	var inputCtx context.Context
	var cancel context.CancelFunc
	if valid {
		g.lastSeq = seq
		g.heartbeat = time.Now().Add(5 * time.Second)
		g.armLocked()
		if event.Type != "heartbeat" && (event.Type != "resize" || canResize) {
			deadline := g.until
			if event.Type == "resize" {
				if bound := time.Now().Add(DesktopResizeTimeout); bound.Before(deadline) {
					deadline = bound
				}
			}
			inputCtx, cancel = context.WithDeadline(ctx, deadline)
			g.nativeCancel = cancel
		}
	}
	g.mu.Unlock()
	if !valid {
		return ErrDesktopControl
	}
	if event.Type == "heartbeat" {
		return nil
	}
	if event.Type == "resize" && !canResize {
		return ErrDesktopResizeUnsupported
	}
	defer func() {
		cancel()
		g.mu.Lock()
		g.nativeCancel = nil
		g.mu.Unlock()
	}()
	var err error
	switch event.Type {
	case "release_all":
		err = g.native.ReleaseAll(inputCtx)
	case "resize":
		err = resizer.Resize(inputCtx, event.Width, event.Height, event.Scale)
	default:
		err = g.native.Apply(inputCtx, event)
	}
	if event.Type == "resize" && errors.Is(err, ErrDesktopResizeUnsupported) {
		return err
	}
	if err == nil {
		err = inputCtx.Err()
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
	case "resize":
		if e.Width == 0 || e.Height == 0 || e.Scale != 0 && (e.Scale < DesktopScaleMin || e.Scale > DesktopScaleMax) {
			return ErrDesktopControl
		}
		return nil
	case "key":
		if e.Keysym == 0 || e.Keysym > 0x0110ffff {
			return ErrDesktopControl
		}
		return nil
	case "text":
		if len(e.Text) < 1 || len(e.Text) > DesktopTextMaxBytes || !utf8.ValidString(e.Text) {
			return ErrDesktopControl
		}
		for _, r := range e.Text {
			if r != '\t' && r != '\r' && r != '\n' && (r < 0x20 || r >= 0x7f && r < 0xa0) {
				return ErrDesktopControl
			}
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
