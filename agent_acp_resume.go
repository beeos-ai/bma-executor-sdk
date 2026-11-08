package bmaexecutor

// TurnResume of ACPAgent: the state a restarted executor needs to continue a
// turn the platform starts again (same turn_id, executor_session_ref).
//
// The conversation itself is the agent's (bma-dsh-acp keeps it durable;
// session/load restores it and a repeated prompt with the same
// _meta.bma.inputId continues the recorded input instead of adding it twice).
// What only the executor knows is what already happened on this computer and
// what the platform already has, so it keeps a small ledger per session,
// one file under StateDir/executor-turns, for the turn that runs:
//
//   - Inputs: input ids whose prompt ended and whose events the platform
//     acknowledged (the outbox was flushed), so they are not prompted again.
//   - Calls[call].Decision: the platform's allow/deny of a tool call, so an
//     approval already answered is not asked again (a pending one is asked
//     again under the same deterministic appr_ id, which the platform keeps
//     as one interaction).
//   - Calls[call].Phase: "started" is written before a tool runs and "done"
//     with its result after it. A call that is "done" is answered from the
//     ledger when the agent asks for it again; one that is "started" has an
//     unknown outcome and is answered with an error instead of running again
//     (ADR-0005, ADR-0013: results of unknown outcome are not replayed).
//   - Calls[call].Reported: the platform acknowledged the call's
//     agent.tool_result, so its events are not emitted again.
//
// The ledger is replaced when another turn starts and removed when the
// session closes. Writes are atomic (temp file and rename) and survive a
// killed process; they are not synced for a power loss.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	acpPhaseStarted = "started"
	acpPhaseDone    = "done"

	acpDecisionAllow = "allow"
	acpDecisionDeny  = "deny"
)

type acpCallState struct {
	Decision string `json:"decision,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Text     string `json:"text,omitempty"`
	IsError  bool   `json:"is_error,omitempty"`
	Reported bool   `json:"reported,omitempty"`
}

type acpLedger struct {
	path string

	mu    sync.Mutex
	state acpLedgerState
}

type acpLedgerState struct {
	SessionID string                   `json:"session_id"`
	TurnID    string                   `json:"turn_id"`
	Inputs    map[string]bool          `json:"inputs,omitempty"`
	Calls     map[string]*acpCallState `json:"calls,omitempty"`
}

func (a *ACPAgent) ledgerPath(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(a.opts.StateDir, "executor-turns", hex.EncodeToString(sum[:16])+".json")
}

// ledger returns the session's ledger, read from disk the first time.
func (a *ACPAgent) ledger(sessionID string) (*acpLedger, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.ledgers[sessionID]; l != nil {
		return l, nil
	}
	l := &acpLedger{path: a.ledgerPath(sessionID), state: acpLedgerState{SessionID: sessionID}}
	raw, err := os.ReadFile(l.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, &l.state); err != nil || l.state.SessionID != sessionID {
			return nil, errors.New("the turn state of session " + sessionID + " is unreadable: " + l.path)
		}
	}
	if a.ledgers == nil {
		a.ledgers = map[string]*acpLedger{}
	}
	a.ledgers[sessionID] = l
	return l, nil
}

func (a *ACPAgent) dropLedger(sessionID string) {
	a.mu.Lock()
	delete(a.ledgers, sessionID)
	a.mu.Unlock()
	_ = os.Remove(a.ledgerPath(sessionID))
}

// begin keeps the ledger when it belongs to turnID (a turn the platform
// starts again) and replaces it otherwise. It reports whether it kept it.
func (l *acpLedger) begin(turnID string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.TurnID == turnID {
		return true, nil
	}
	l.state = acpLedgerState{SessionID: l.state.SessionID, TurnID: turnID}
	return false, l.saveLocked()
}

func (l *acpLedger) saveLocked() error {
	raw, err := json.Marshal(l.state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// update changes one call and saves.
func (l *acpLedger) update(callID string, change func(*acpCallState)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.Calls == nil {
		l.state.Calls = map[string]*acpCallState{}
	}
	c := l.state.Calls[callID]
	if c == nil {
		c = &acpCallState{}
		l.state.Calls[callID] = c
	}
	change(c)
	return l.saveLocked()
}

func (l *acpLedger) call(callID string) acpCallState {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.state.Calls[callID]; c != nil {
		return *c
	}
	return acpCallState{}
}

func (l *acpLedger) inputDone(inputID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.Inputs[inputID]
}

func (l *acpLedger) markInputDone(inputID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.Inputs == nil {
		l.state.Inputs = map[string]bool{}
	}
	l.state.Inputs[inputID] = true
	return l.saveLocked()
}
