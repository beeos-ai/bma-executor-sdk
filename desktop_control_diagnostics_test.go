package bmaexecutor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func requireControlRejection(t *testing.T, err, cause error, reason string) *desktopControlRejection {
	t.Helper()
	var rejection *desktopControlRejection
	if !errors.Is(err, cause) || !errors.As(err, &rejection) || rejection.reason != reason {
		t.Fatalf("rejection = %#v, want %v (%s)", err, cause, reason)
	}
	if err.Error() != cause.Error() {
		t.Fatalf("wire error changed: %q", err.Error())
	}
	return rejection
}

func TestControlDiagnosticsPreserveDeadlineRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration time.Duration
		reason   string
	}{
		{"expired", -time.Second, "deadline_expired"},
		{"remote_clock_ahead", 31 * time.Second, "deadline_too_long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &controlNative{}
			g := NewDesktopControlGate(&controlBackend{}, n)
			defer g.Close()
			err := g.Grant(context.Background(), 1, "private-lease", "private-viewer", time.Now().Add(tc.duration))
			r := requireControlRejection(t, err, ErrDesktopControl, tc.reason)
			if tc.duration < 0 && r.remainingMS >= 0 || tc.duration > 0 && r.remainingMS <= 30000 {
				t.Fatalf("incorrect rejection-time delta: %d", r.remainingMS)
			}
			if g.epoch != 0 || g.state != "agent" || n.released != 0 {
				t.Fatal("rejected deadline changed authority or touched native input")
			}
		})
	}
}

func TestControlDeadlineClockSkewAndDispatchDelay(t *testing.T) {
	// The platform signs at platformNow; delivery takes delay, and the executor's
	// wall clock differs by skew. No actual clock adjustment or sleeps are needed.
	platformNow := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	deadline := platformNow.Add(30 * time.Second)
	for _, tc := range []struct {
		name            string
		skewMS, delayMS int64
		wantRemainingMS int64
		wantReason      string
	}{
		{"equal_clocks", 0, 100, 29900, ""},
		{"one_ms_behind_with_transport", -1, 10, 29991, ""},
		{"behind_more_than_transport", -50, 10, 30040, "deadline_too_long"},
		{"behind_equal_to_transport", -10, 10, 30000, ""},
		{"ahead_within_window", 10000, 200, 19800, ""},
		{"expires_at_receipt", 30000, 0, 0, "deadline_expired"},
		{"delivery_after_expiry", 0, 31000, -1000, "deadline_expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := platformNow.Add(time.Duration(tc.delayMS+tc.skewMS) * time.Millisecond)
			if got := deadline.Sub(now).Milliseconds(); got != tc.wantRemainingMS || got != 30000-tc.delayMS-tc.skewMS {
				t.Fatalf("remaining = %d, want %d", got, tc.wantRemainingMS)
			}
			err := desktopControlDeadlineError(deadline, now)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("valid deadline rejected: %v", err)
				}
				return
			}
			r := requireControlRejection(t, err, ErrDesktopControl, tc.wantReason)
			if r.remainingMS != tc.wantRemainingMS {
				t.Fatalf("diagnostic remaining = %d, want %d", r.remainingMS, tc.wantRemainingMS)
			}
		})
	}
}

func TestControlDiagnosticsDistinguishAuthorityAndCleanup(t *testing.T) {
	t.Run("replayed_lease", func(t *testing.T) {
		g := NewDesktopControlGate(&controlBackend{}, &controlNative{})
		defer g.Close()
		ctx := context.Background()
		deadline := time.Now().Add(20 * time.Second)
		if err := g.Grant(ctx, 2, "lease", "viewer", deadline); err != nil {
			t.Fatal(err)
		}
		requireControlRejection(t, g.Grant(ctx, 1, "lease", "viewer", deadline), ErrDesktopControl, "stale_epoch")
		requireControlRejection(t, g.Grant(ctx, 2, "other", "viewer", deadline), ErrDesktopControl, "current_lease_mismatch")
		requireControlRejection(t, g.Grant(ctx, 3, "other", "viewer", deadline), ErrDesktopControl, "controller_busy")
		if g.epoch != 2 || g.lease != "lease" || g.state != "human" {
			t.Fatal("diagnostic path replaced the active controller")
		}
	})
	t.Run("draining", func(t *testing.T) {
		g := NewDesktopControlGate(&controlBackend{}, &controlNative{})
		defer g.Close()
		g.input <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := g.Grant(ctx, 1, "lease", "viewer", time.Now().Add(20*time.Second))
		<-g.input
		requireControlRejection(t, err, ErrDesktopDraining, "drain_failed")
		if g.state != "released" {
			t.Fatal("draining failure admitted control")
		}
	})
	t.Run("native_cleanup", func(t *testing.T) {
		g := NewDesktopControlGate(&controlBackend{}, diagnosticCleanupFailure{})
		err := g.Grant(context.Background(), 1, "lease", "viewer", time.Now().Add(20*time.Second))
		requireControlRejection(t, err, ErrDesktopInputUnknown, "native_release_failed")
		if g.state != "blocked" {
			t.Fatal("unknown native result no longer blocks input")
		}
		g.Close()
	})
}

type diagnosticCleanupFailure struct{}

func (diagnosticCleanupFailure) Apply(context.Context, DesktopInputEvent) error { return nil }
func (diagnosticCleanupFailure) ReleaseAll(context.Context) error {
	return errors.New("private-driver-details")
}

func TestControlRPCDiagnosticNeverLogsCredentials(t *testing.T) {
	var logged []LogEvent
	c, err := NewClient(Options{BaseURL: "https://example.test", EnvironmentKey: "private-environment", MachineID: "private-machine", Executor: ExecutorInfo{Name: "test"}, OnLog: func(e LogEvent) { logged = append(logged, e) }})
	if err != nil {
		t.Fatal(err)
	}
	g := NewDesktopControlGate(&controlBackend{}, &controlNative{})
	defer g.Close()
	if err := c.ServeDesktopControl(g); err != nil {
		t.Fatal(err)
	}
	p := DesktopControlCommand{SessionID: "private-session", MachineID: "private-machine", LeaseID: "private-lease", Identity: "private-identity", Epoch: 1, Action: "grant", InputKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ExpiresAtMS: time.Now().Add(31 * time.Second).UnixMilli()}
	raw, _ := json.Marshal(p)
	_, err = c.requestHandlers[DesktopControlMethod](context.Background(), raw)
	requireControlRejection(t, err, ErrDesktopControl, "deadline_too_long")
	if len(logged) != 1 || logged[0].Level != LogWarn || logged[0].Err != nil {
		t.Fatalf("unsafe or missing diagnostic: %#v", logged)
	}
	message := logged[0].Message
	if !strings.Contains(message, "action=grant reason=deadline_too_long remaining_ms=") {
		t.Fatalf("missing useful diagnostic: %q", message)
	}
	for _, secret := range []string{"private-", p.InputKey} {
		if strings.Contains(message, secret) {
			t.Fatal("diagnostic leaked a control binding or key")
		}
	}
	if got := desktopControlDiagnostic("private-action\nsecret", errors.New("private-details")); got != "desktop control rejected action=unsupported reason=unclassified" {
		t.Fatalf("untrusted action/error escaped allowlist: %q", got)
	}
}
