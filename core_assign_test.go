package bmaexecutor

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// assignRecorder is a CoreOptions.OnAssign that records what it saw.
type assignRecorder struct {
	mu   sync.Mutex
	seen []map[string]string
	fail error
}

func (r *assignRecorder) onAssign(s *CoreSession, p SessionAssignParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, p.Metadata)
	return r.fail
}

func (r *assignRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func replayAssign(t *testing.T, b brain, params SessionAssignParams) (SessionAssignResult, error) {
	t.Helper()
	if params.WorkID == "" {
		params.WorkID = "work_replay"
	}
	if params.Lease.Token == "" {
		params.Lease.Token = "lease_replay"
	}
	var result SessionAssignResult
	err := b.executor.call(testContext(t), MethodSessionAssign, params, &result)
	return result, err
}

// A replayed session.assign (before every tool call, on reconnect, before a
// token expires) replaces the session's metadata: the latest one wins, and an
// assign without a key removes it. The host hears of every assign.
func TestCoreReplayedAssignReplacesMetadata(t *testing.T) {
	requireUnix(t)
	rec := &assignRecorder{}
	core := newTestCore(t, CoreOptions{OnAssign: rec.onAssign})
	b := attachBrain(t, core, "platform")
	params := SessionAssignParams{SessionID: "sess_egress", Metadata: map[string]string{"agent_session_id": "sess_egress", "egress": "one"}}
	readySession(t, b, params)
	if rec.count() != 1 {
		t.Fatalf("OnAssign calls after the first assign = %d, want 1", rec.count())
	}

	params.Metadata = map[string]string{"agent_session_id": "sess_egress", "egress": "two"}
	if result, err := replayAssign(t, b, params); err != nil || !result.Accepted {
		t.Fatalf("replay = %+v, %v", result, err)
	}
	session := core.session(coreKey{"platform", "sess_egress"})
	if got := session.Assignment().Metadata["egress"]; got != "two" {
		t.Fatalf("metadata egress after replay = %q, want two", got)
	}

	params.Metadata = map[string]string{"agent_session_id": "sess_egress"}
	if _, err := replayAssign(t, b, params); err != nil {
		t.Fatal(err)
	}
	if _, ok := session.Assignment().Metadata["egress"]; ok {
		t.Fatalf("metadata kept egress after an assign without it: %v", session.Assignment().Metadata)
	}
	if rec.count() != 3 {
		t.Fatalf("OnAssign calls = %d, want 3 (every assign)", rec.count())
	}
	if got := rec.seen[1]["egress"]; got != "two" {
		t.Fatalf("OnAssign saw egress %q on the replay, want two", got)
	}
}

// A replayed assign the host refuses leaves the session's previous metadata,
// and a first assign it refuses creates no session.
func TestCoreOnAssignRefusal(t *testing.T) {
	requireUnix(t)
	rec := &assignRecorder{}
	core := newTestCore(t, CoreOptions{OnAssign: rec.onAssign})
	b := attachBrain(t, core, "platform")

	rec.fail = errors.New("egress: not a handoff")
	_, err := replayAssign(t, b, SessionAssignParams{SessionID: "sess_refused"})
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams || !strings.Contains(rpc.Message, "egress: not a handoff") {
		t.Fatalf("refused first assign = %v, want invalid params naming the refusal", err)
	}
	if core.session(coreKey{"platform", "sess_refused"}) != nil {
		t.Fatal("a refused first assign left a session")
	}

	rec.fail = nil
	readySession(t, b, SessionAssignParams{SessionID: "sess_kept", Metadata: map[string]string{"egress": "good"}})
	rec.fail = errors.New("egress: expired")
	if _, err := replayAssign(t, b, SessionAssignParams{SessionID: "sess_kept", Metadata: map[string]string{"egress": "bad"}}); err == nil {
		t.Fatal("a refused replay was accepted")
	}
	if got := core.session(coreKey{"platform", "sess_kept"}).Assignment().Metadata["egress"]; got != "good" {
		t.Fatalf("a refused replay changed the metadata to %q", got)
	}
}

// The executor declares the forwarder through register capabilities.
func TestCapabilitiesEgressJSON(t *testing.T) {
	raw, err := json.Marshal(Capabilities{Egress: &EgressCapability{Forwarder: true}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"egress":{"forwarder":true}}` {
		t.Fatalf("capabilities = %s", raw)
	}
}
