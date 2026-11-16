package bmaexecutor

import (
	"errors"
	"fmt"
	"time"
)

// Keep the existing sentinel/wire error while retaining the exact rejecting
// branch for local diagnostics. Never put control bindings or keys in logs.
type desktopControlRejection struct {
	cause       error
	reason      string
	remainingMS int64
}

func (e *desktopControlRejection) Error() string { return e.cause.Error() }
func (e *desktopControlRejection) Unwrap() error { return e.cause }

func rejectDesktopControl(cause error, reason string, deadline, now time.Time) error {
	return &desktopControlRejection{cause: cause, reason: reason, remainingMS: deadline.Sub(now).Milliseconds()}
}

func desktopControlDeadlineError(deadline, now time.Time) error {
	if !deadline.After(now) {
		return rejectDesktopControl(ErrDesktopControl, "deadline_expired", deadline, now)
	}
	if deadline.After(now.Add(30 * time.Second)) {
		return rejectDesktopControl(ErrDesktopControl, "deadline_too_long", deadline, now)
	}
	return nil
}

func desktopControlDiagnostic(action string, err error) string {
	switch action {
	case "grant", "renew", "release", "handback":
	default:
		action = "unsupported"
	}
	var rejected *desktopControlRejection
	if errors.As(err, &rejected) {
		return fmt.Sprintf("desktop control rejected action=%s reason=%s remaining_ms=%d", action, rejected.reason, rejected.remainingMS)
	}
	return fmt.Sprintf("desktop control rejected action=%s reason=unclassified", action)
}
