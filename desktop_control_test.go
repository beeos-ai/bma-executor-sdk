package bmaexecutor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type controlBackend struct {
	entered chan struct{}
	finish  chan struct{}
	mu      sync.Mutex
	actions int
	err     error
}

func (b *controlBackend) Observe(context.Context, ComputerObserveRequest) ([]ContentBlock, error) {
	return nil, nil
}
func (b *controlBackend) Action(ctx context.Context, _ ComputerActionRequest) ([]ContentBlock, error) {
	if b.entered != nil {
		close(b.entered)
		<-b.finish
	}
	b.mu.Lock()
	b.actions++
	b.mu.Unlock()
	return nil, b.err
}

type controlNative struct {
	mu       sync.Mutex
	held     bool
	released int
}

func (n *controlNative) Apply(_ context.Context, e DesktopInputEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.held = e.Down
	return nil
}
func (n *controlNative) ReleaseAll(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.held = false
	n.released++
	return nil
}
func TestControlDrainRejectsNewActionsAndRequiresPerSessionObservation(t *testing.T) {
	ctx := context.Background()
	b := &controlBackend{entered: make(chan struct{}), finish: make(chan struct{})}
	n := &controlNative{}
	g := NewDesktopControlGate(b, n)
	defer g.Close()
	action := make(chan error, 1)
	go func() { _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); action <- err }()
	<-b.entered
	grant := make(chan error, 1)
	go func() { grant <- g.Grant(ctx, 1, "lease", "viewer", time.Now().Add(20*time.Second)) }()
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		state := g.state
		g.mu.Unlock()
		if state == "draining" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not drain")
		}
		time.Sleep(time.Millisecond)
	}
	close(b.finish)
	if err := <-action; err != nil {
		t.Fatal(err)
	}
	if err := <-grant; err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); err == nil {
		t.Fatal("human and Agent input overlapped")
	}
	if _, err := g.Observe(ctx, ComputerObserveRequest{SessionID: "a"}); err != nil {
		t.Fatal("observation should remain available")
	}
	if err := g.Release(ctx, 1, "lease", true); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); !errors.Is(err, ErrDesktopObservation) {
		t.Fatal("human-period observation authorized input", err)
	}
	b.entered = nil
	if _, err := g.Observe(ctx, ComputerObserveRequest{SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "b"}); err == nil {
		t.Fatal("one session resumed another")
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
}
func TestControlDrainTimeoutNeverGrantsHumanInput(t *testing.T) {
	ctx := context.Background()
	b := &controlBackend{entered: make(chan struct{}), finish: make(chan struct{})}
	g := NewDesktopControlGate(b, &controlNative{})
	defer g.Close()
	done := make(chan struct{})
	go func() { _, _ = g.Action(ctx, ComputerActionRequest{}); close(done) }()
	<-b.entered
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := g.Grant(short, 1, "l", "v", time.Now().Add(time.Second)); !errors.Is(err, ErrDesktopDraining) {
		t.Fatal(err)
	}
	if err := g.HumanInput(short, 1, "l", "v", 1, DesktopInputEvent{Type: "key", Keysym: 65, Down: true}); err == nil {
		t.Fatal("timed out action granted control")
	}
	close(b.finish)
	<-done
}
func TestControlLeaseReplayHeartbeatAndRelease(t *testing.T) {
	ctx := context.Background()
	n := &controlNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "old", "viewer", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	key := DesktopInputEvent{Type: "key", Keysym: 0xffe1, Down: true}
	if err := g.HumanInput(ctx, 1, "old", "viewer", 1, key); err != nil {
		t.Fatal(err)
	}
	if err := g.HumanInput(ctx, 1, "old", "viewer", 1, key); err == nil {
		t.Fatal("duplicate input accepted")
	}
	if err := g.HumanInput(ctx, 1, "old", "stranger", 2, key); err == nil {
		t.Fatal("wrong participant accepted")
	}
	g.mu.Lock()
	g.heartbeat = time.Now().Add(-time.Millisecond)
	g.armLocked()
	g.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		n.mu.Lock()
		held := n.held
		n.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held key survived heartbeat expiry")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{}); err == nil {
		t.Fatal("disconnect automatically resumed Agent")
	}
	if err := g.Grant(ctx, 2, "new", "viewer", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.HumanInput(ctx, 1, "old", "viewer", 2, key); err == nil {
		t.Fatal("old lease injected into new lease")
	}
	if err := g.Release(ctx, 2, "new", false); err != nil {
		t.Fatal(err)
	}
}

func TestControlReleaseCannotBeOvertakenOrUndoHandback(t *testing.T) {
	ctx := context.Background()
	g := NewDesktopControlGate(&controlBackend{}, &controlNative{})
	defer g.Close()
	if err := g.Grant(ctx, 1, "one", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.lock(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Release(ctx, 1, "one", true) }()
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		releasing := g.state == "releasing"
		g.mu.Unlock()
		if releasing {
			break
		}
		if time.Now().After(deadline) {
			g.unlock()
			t.Fatal("release did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := g.Grant(ctx, 2, "two", "v", time.Now().Add(20*time.Second)); err == nil {
		g.unlock()
		t.Fatal("grant overtook release")
	}
	g.unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := g.Release(ctx, 1, "one", false); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	resumed := g.resume
	g.mu.Unlock()
	if !resumed {
		t.Fatal("duplicate release undid completed handback")
	}
	if err := g.Grant(ctx, 2, "two", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.Release(ctx, 2, "two", false); err != nil {
		t.Fatal(err)
	}
	if err := g.Release(ctx, 2, "two", true); err == nil {
		t.Fatal("late handback resumed disconnected lease")
	}
}
func TestControlRenewCannotReplaceBindingOrKey(t *testing.T) {
	ctx := context.Background()
	g := NewDesktopControlGate(&controlBackend{}, &controlNative{})
	defer g.Close()
	key := make([]byte, 32)
	other := make([]byte, 32)
	other[0] = 1
	until := time.Now().Add(20 * time.Second)
	if err := g.grant(ctx, 1, "lease", "v", until, key, "media"); err != nil {
		t.Fatal(err)
	}
	if err := g.grant(ctx, 1, "lease", "v", until, other, "media"); err == nil {
		t.Fatal("renew changed key")
	}
	if err := g.grant(ctx, 1, "lease", "v", until, key, "other"); err == nil {
		t.Fatal("renew changed media binding")
	}
	if err := g.grant(ctx, 1, "lease", "v", until, key, "media"); err != nil {
		t.Fatal(err)
	}
	if err := g.release(ctx, 1, "lease", false, true); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	state := g.state
	g.mu.Unlock()
	if state != "human" {
		t.Fatal("stale expiry released a renewed lease")
	}
}

type failingActionBackend struct{ err error }

func (b *failingActionBackend) Observe(context.Context, ComputerObserveRequest) ([]ContentBlock, error) {
	return nil, nil
}
func (b *failingActionBackend) Action(context.Context, ComputerActionRequest) ([]ContentBlock, error) {
	return nil, b.err
}

type recoverableActionBackend struct {
	failingActionBackend
	recoveryErr error
	recovered   bool
}

func (b *recoverableActionBackend) RecoverInput(context.Context) error {
	b.recovered = true
	return b.recoveryErr
}

type failingNativeCleanup struct {
	controlNative
	fail bool
}

func (n *failingNativeCleanup) ReleaseAll(ctx context.Context) error {
	if n.fail {
		return errors.New("cleanup failed")
	}
	return n.controlNative.ReleaseAll(ctx)
}

func TestDriverRejectionDoesNotPoisonControlAndUnknownNeedsDriverProof(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, output string
		rejected     bool
	}{
		{"structured refusal", `{"code":"invalid_arguments"}`, true},
		{"partial input", `{"code":"type_text_incomplete"}`, false},
		{"plain text", `invalid_arguments`, false},
		{"undecodable", `{`, false},
		{"unknown", `{"code":"future_error"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver, _, err := fakeCua(t, "BMA_TEST_CUA_ACTION_STDOUT="+tc.output)
			if err != nil {
				t.Fatal(err)
			}
			native := &controlNative{}
			g := NewDesktopControlGate(driver, native)
			defer g.Close()
			req := ComputerActionRequest{Action: "click", Coordinate: &[2]float64{1, 2}}
			_, err = g.Action(ctx, req)
			if errors.Is(err, ErrComputerActionRejected) != tc.rejected {
				t.Fatal("wrong classification", err)
			}
			if tc.rejected {
				driver.env = nil
				// Use the helper again with only the failure injection removed.
				driver.env = []string{"BMA_TEST_HELPER=cua"}
				if _, err = g.Action(ctx, req); err != nil {
					t.Fatal("known refusal blocked next action", err)
				}
				if err = g.Grant(ctx, 1, "lease", "viewer", time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, ErrDesktopRecoveryRequired) {
					t.Fatal(err)
				}
				if err = g.Recover(ctx); !errors.Is(err, ErrDesktopRecoveryRequired) {
					t.Fatal("human cleanup cleared unknown Agent action", err)
				}
				if err = g.Grant(ctx, 1, "lease", "viewer", time.Now().Add(time.Second)); err == nil {
					t.Fatal("unknown input granted control")
				}
			}
		})
	}
}
func TestControlledRecoveryRequiresBothSourcesAndNeverResumesOldAuthority(t *testing.T) {
	ctx := context.Background()
	b := &recoverableActionBackend{failingActionBackend: failingActionBackend{err: context.DeadlineExceeded}, recoveryErr: errors.New("still running")}
	n := &failingNativeCleanup{}
	g := NewDesktopControlGate(b, n)
	defer g.Close()
	_, _ = g.Action(ctx, ComputerActionRequest{})
	if err := g.Recover(ctx); err == nil {
		t.Fatal("unproven Agent quiescence recovered")
	}
	b.recoveryErr = nil
	n.fail = true
	if err := g.Recover(ctx); err == nil {
		t.Fatal("failed human cleanup recovered")
	}
	n.fail = false
	if err := g.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); err == nil {
		t.Fatal("recovery resumed Agent")
	}
	if err := g.HumanInput(ctx, 0, "", "", 1, DesktopInputEvent{Type: "heartbeat"}); err == nil {
		t.Fatal("recovery restored human credentials")
	}
	b.err = nil
	if err := g.Grant(ctx, 1, "new", "v", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := g.Release(ctx, 1, "new", true); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); err == nil {
		t.Fatal("missing new observation")
	}
	_, _ = g.Observe(ctx, ComputerObserveRequest{SessionID: "a"})
	if _, err := g.Action(ctx, ComputerActionRequest{SessionID: "a"}); err != nil {
		t.Fatal(err)
	}
}
func TestNativeCleanupCanRetryWithoutClearingAgentUnknown(t *testing.T) {
	ctx := context.Background()
	n := &failingNativeCleanup{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "lease", "v", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n.fail = true
	if err := g.Release(ctx, 1, "lease", false); err == nil {
		t.Fatal("cleanup failure ignored")
	}
	n.fail = false
	if err := g.Release(ctx, 1, "lease", true); err == nil {
		t.Fatal("blocked handback resumed Agent")
	}
	if err := g.Release(ctx, 1, "lease", false); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Action(ctx, ComputerActionRequest{}); err == nil {
		t.Fatal("cleanup resumed Agent")
	}
}

func TestReleaseWaitingOnAgentCannotClearLateUnknownResult(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "handback"}[resume], func(t *testing.T) {
			ctx := context.Background()
			b := &controlBackend{entered: make(chan struct{}), finish: make(chan struct{}), err: errors.New("driver response lost")}
			g := NewDesktopControlGate(b, &controlNative{})
			defer g.Close()
			action := make(chan error, 1)
			go func() { _, err := g.Action(ctx, ComputerActionRequest{}); action <- err }()
			<-b.entered
			release := make(chan error, 1)
			go func() { release <- g.Release(ctx, 0, "", resume) }()
			deadline := time.Now().Add(time.Second)
			for {
				g.mu.Lock()
				state := g.state
				g.mu.Unlock()
				if state == "releasing" {
					break
				}
				if time.Now().After(deadline) {
					close(b.finish)
					t.Fatal("release did not wait on Agent")
				}
				time.Sleep(time.Millisecond)
			}
			close(b.finish)
			if err := <-action; !errors.Is(err, ErrDesktopRecoveryRequired) {
				t.Fatal(err)
			}
			if err := <-release; !errors.Is(err, ErrDesktopRecoveryRequired) {
				t.Fatalf("late unknown result was cleared: %v", err)
			}
			if err := g.Grant(ctx, 1, "new", "viewer", time.Now().Add(time.Second)); err == nil {
				t.Fatal("unknown Agent action allowed takeover")
			}
		})
	}
}
