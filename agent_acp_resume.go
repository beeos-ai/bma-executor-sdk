package bmaexecutor

// TurnResume of ACPAgent: the state a restarted executor needs to continue a
// turn the platform starts again (same turn_id, executor_session_ref).
//
// The conversation itself is the brain's (each adapter resumes it by its
// executor_session_ref). What only the executor knows is what already
// happened on this computer and what the platform already has, so it keeps
// a small ledger per session, one file under StateDir/executor-turns, for the
// turn that runs:
//
//   - Inputs: input ids whose prompt ended and whose events the platform
//     acknowledged (the outbox was flushed), so they are not prompted again.
//   - Begun: input ids the brain started working on (it reported output or a
//     tool call, so its conversation holds the input). Such an input that is
//     not done is not handed again: the host settles its unreported tool
//     calls and hands the brain a recovery note instead (recover).
//   - Calls[call].ToolUseID, ToolName, Input, Seq: the call's agent.tool_use
//     event and what it asked for, in order, so a restarted executor can
//     settle it under the same event and approval id.
//   - Calls[call].Decision: the platform's allow/deny of a tool call, so an
//     approval already answered is not asked again (a pending one is asked
//     again under the same deterministic appr_ id, which the platform keeps
//     as one interaction). It is also the evidence the executor checks before
//     it runs an ask tool (ACPAgent's tool gate, toolroute_local.go).
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
	"slices"
	"sync"
)

const (
	acpPhaseStarted = "started"
	acpPhaseDone    = "done"

	acpDecisionAllow = "allow"
	acpDecisionDeny  = "deny"
)

type acpCallState struct {
	// Decision is the platform's answer to the call's permission interaction
	// (interaction.response); InteractionID, ToolName and InputHash say what it
	// was given for, so the executor can check the call it runs is that one.
	Decision      string `json:"decision,omitempty"`
	InteractionID string `json:"interaction_id,omitempty"`
	ToolName      string `json:"tool_name,omitempty"`
	InputHash     string `json:"input_hash,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Text          string `json:"text,omitempty"`
	IsError       bool   `json:"is_error,omitempty"`
	// Outcome is executed (also when empty) or unknown; Code the reason of an unknown one.
	Outcome  string `json:"outcome,omitempty"`
	Code     string `json:"code,omitempty"`
	Reported bool   `json:"reported,omitempty"`
	// ToolUseID is the call's agent.tool_use event id; Input its input; Seq
	// its place among the turn's calls.
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Seq       int            `json:"seq,omitempty"`
	// Permission is the evaluated permission of its agent.tool_use, and
	// UseReported that the platform acknowledged that event.
	Permission  string `json:"permission,omitempty"`
	UseReported bool   `json:"use_reported,omitempty"`
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
	Begun     map[string]bool          `json:"begun,omitempty"`
	Calls     map[string]*acpCallState `json:"calls,omitempty"`
}

func ledgerPath(stateDir, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(stateDir, "executor-turns", hex.EncodeToString(sum[:16])+".json")
}

// ledger returns the session's ledger, read from disk the first time. There
// is one in-memory copy per file in a Core, however many routes use it.
func (a *BrainHost) ledger(sessionID string) (*acpLedger, error) {
	return a.opts.Core.local.ledger(ledgerPath(a.opts.StateDir, sessionID), sessionID)
}

func (s *localState) ledger(path, sessionID string) (*acpLedger, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.ledgers[path]; l != nil {
		return l, nil
	}
	l := &acpLedger{path: path, state: acpLedgerState{SessionID: sessionID}}
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
	if s.ledgers == nil {
		s.ledgers = map[string]*acpLedger{}
	}
	s.ledgers[path] = l
	return l, nil
}

// forgetLedgers drops the in-memory turn state of a session and keeps its file,
// so the ledger is read again when the session's next turn opens.
func (s *localState) forgetLedgers(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, l := range s.ledgers {
		if l.sessionID() == sessionID {
			delete(s.ledgers, path)
		}
	}
}

// dropLedgers removes the turn state of a session from memory and disk.
func (s *localState) dropLedgers(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, l := range s.ledgers {
		if l.sessionID() == sessionID {
			delete(s.ledgers, path)
			_ = os.Remove(path)
		}
	}
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
		c = &acpCallState{Seq: len(l.state.Calls) + 1}
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

func (l *acpLedger) sessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.SessionID
}

// decide records the platform's answer to a call's permission interaction and
// what it was given for, so the gate can check that the call it runs is that one.
func (l *acpLedger) decide(callID, decision, interactionID, toolName string, input map[string]any) error {
	return l.update(callID, func(c *acpCallState) {
		c.Decision, c.InteractionID, c.ToolName, c.InputHash = decision, interactionID, toolName, acpInputHash(input)
	})
}

// turnID is the turn the ledger belongs to.
func (l *acpLedger) turnID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.TurnID
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

// inputBegun reports whether the brain started working on the input.
func (l *acpLedger) inputBegun(inputID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.Begun[inputID]
}

// markInputBegun records that the brain started working on the input (once).
func (l *acpLedger) markInputBegun(inputID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.Begun[inputID] {
		return nil
	}
	if l.state.Begun == nil {
		l.state.Begun = map[string]bool{}
	}
	l.state.Begun[inputID] = true
	return l.saveLocked()
}

// acpPendingCall is a call of the turn whose result the platform does not have.
type acpPendingCall struct {
	ID string
	acpCallState
}

// unreported are the turn's calls with an agent.tool_use event whose
// agent.tool_result was not acknowledged, in order.
func (l *acpLedger) unreported() []acpPendingCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []acpPendingCall
	for id, c := range l.state.Calls {
		if c.ToolUseID != "" && !c.Reported {
			out = append(out, acpPendingCall{ID: id, acpCallState: *c})
		}
	}
	slices.SortFunc(out, func(a, b acpPendingCall) int { return a.Seq - b.Seq })
	return out
}
