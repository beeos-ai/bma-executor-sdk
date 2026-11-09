package bmaexecutor

// Seams for the external tests (package bmaexecutor_test), which cannot reach
// unexported state. They exist in test builds only.

// MarkLocalCallStarted stands for a call that was submitted to the tool and
// whose process died before the result was saved.
func MarkLocalCallStarted(r *LocalToolRoute, callID string) error {
	return r.ledger.update(callID, func(c *acpCallState) { c.Phase = acpPhaseStarted })
}
