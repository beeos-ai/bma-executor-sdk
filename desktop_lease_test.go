package bmaexecutor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// leaseNative is an input device with its own exclusive lease (a phone).
type leaseNative struct {
	controlNative
	mu         sync.Mutex
	holds      []time.Time
	drops      []bool
	holdErr    error
	dropErr    error
	holdingNow bool
}

func (n *leaseNative) Hold(_ context.Context, until time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.holdErr != nil {
		return n.holdErr
	}
	n.holds = append(n.holds, until)
	n.holdingNow = true
	return nil
}

func (n *leaseNative) Drop(_ context.Context, resume bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dropErr != nil {
		return n.dropErr
	}
	n.drops = append(n.drops, resume)
	n.holdingNow = false
	return nil
}

func (n *leaseNative) state() (holds int, drops []bool, holding bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.holds), append([]bool(nil), n.drops...), n.holdingNow
}

func TestDeviceLeaseHeldOnGrantAndRenewDroppedWithHandback(t *testing.T) {
	ctx := context.Background()
	n := &leaseNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if holds, _, holding := n.state(); holds != 1 || !holding {
		t.Fatalf("grant did not hold the device lease: holds=%d", holds)
	}
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	if holds, _, _ := n.state(); holds != 2 {
		t.Fatalf("renew did not extend the device lease: holds=%d", holds)
	}
	if err := g.Release(ctx, 1, "l", true); err != nil {
		t.Fatal(err)
	}
	if _, drops, holding := n.state(); holding || len(drops) != 1 || !drops[0] {
		t.Fatalf("handback did not drop with resume: %v", drops)
	}
	if err := g.Release(ctx, 1, "l", true); err != nil {
		t.Fatal("duplicate handback refused", err)
	}
	if _, drops, _ := n.state(); len(drops) != 1 {
		t.Fatalf("duplicate handback dropped again: %v", drops)
	}
}

func TestDeviceLeaseRefusedGrantAdmitsNoInputAndReleaseDropsIt(t *testing.T) {
	ctx := context.Background()
	n := &leaseNative{holdErr: errors.New("phone_input_paused")}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(20*time.Second)); !errors.Is(err, ErrDesktopControl) {
		t.Fatal("refused device lease granted control", err)
	}
	if err := g.HumanInput(ctx, 1, "l", "v", 1, DesktopInputEvent{Type: "key", Keysym: 65, Down: true}); err == nil {
		t.Fatal("input admitted without the device lease")
	}
	n.mu.Lock()
	n.holdErr = nil
	n.mu.Unlock()
	if err := g.Release(ctx, 1, "l", false); err != nil {
		t.Fatal(err)
	}
	if _, drops, _ := n.state(); len(drops) != 1 || drops[0] {
		t.Fatalf("release of the refused grant did not drop the device lease: %v", drops)
	}
	if err := g.Grant(ctx, 2, "m", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceLeaseRefusedRenewKeepsOldDeadline(t *testing.T) {
	ctx := context.Background()
	n := &leaseNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	first := time.Now().Add(10 * time.Second)
	if err := g.Grant(ctx, 1, "l", "v", first); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	until := g.until
	g.mu.Unlock()
	n.mu.Lock()
	n.holdErr = errors.New("control_lease_lost")
	n.mu.Unlock()
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(25*time.Second)); err == nil {
		t.Fatal("refused device renew extended the lease")
	}
	g.mu.Lock()
	after := g.until
	g.mu.Unlock()
	if !after.Equal(until) {
		t.Fatalf("deadline moved from %v to %v", until, after)
	}
}

func TestDeviceLeaseFailedDropIsRetriedByTheSameRelease(t *testing.T) {
	ctx := context.Background()
	n := &leaseNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.dropErr = errors.New("phone offline")
	n.mu.Unlock()
	if err := g.Release(ctx, 1, "l", true); !errors.Is(err, ErrDesktopInputUnknown) {
		t.Fatal("unconfirmed device drop reported success", err)
	}
	if err := g.HumanInput(ctx, 1, "l", "v", 1, DesktopInputEvent{Type: "key", Keysym: 65, Down: true}); err == nil {
		t.Fatal("input admitted after release")
	}
	if err := g.Release(ctx, 1, "l", false); err == nil {
		t.Fatal("a release turned the pending handback into a plain release")
	}
	n.mu.Lock()
	n.dropErr = nil
	n.mu.Unlock()
	if err := g.Release(ctx, 1, "l", true); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	resumed, pending := g.resume, g.dropPending
	g.mu.Unlock()
	if !resumed || pending {
		t.Fatalf("retried handback: resume=%v pending=%v", resumed, pending)
	}
}

func TestDeviceLeaseDroppedOnExpiry(t *testing.T) {
	ctx := context.Background()
	n := &leaseNative{}
	g := NewDesktopControlGate(&controlBackend{}, n)
	defer g.Close()
	if err := g.Grant(ctx, 1, "l", "v", time.Now().Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.heartbeat = time.Now().Add(-time.Millisecond)
	g.armLocked()
	g.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		if _, drops, _ := n.state(); len(drops) == 1 {
			if drops[0] {
				t.Fatal("expiry resumed the device's agent")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("expired lease kept the device lease")
		}
		time.Sleep(time.Millisecond)
	}
}
