package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type compactTestAgent struct {
	nopAgent
	compact func(context.Context, SessionCompactParams) (SessionCompactResult, error)
}

func (a compactTestAgent) CompactSession(ctx context.Context, p SessionCompactParams) (SessionCompactResult, error) {
	return a.compact(ctx, p)
}
func compactParams() SessionCompactParams {
	return SessionCompactParams{SessionID: "session_compact", OperationID: "context_once", ExpectedContextVersion: 0, ExpectedLastInputID: "input_source", Model: "model-1", DeadlineMS: 2000, Metadata: map[string]string{}}
}
func compactResult(p SessionCompactParams) SessionCompactResult {
	text := "Keep prior agreement and material references."
	hash := sha256.Sum256([]byte(text))
	return SessionCompactResult{SessionID: p.SessionID, OperationID: p.OperationID, PreviousContextVersion: p.ExpectedContextVersion, ContextVersion: p.ExpectedContextVersion + 1, Summary: text, SummarySHA256: hex.EncodeToString(hash[:]), SourceEventCount: 12}
}
func compactRuntime(t *testing.T, handler compactTestAgent) *agentRuntime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	c := &Client{rootCtx: ctx, sessions: map[string]struct{}{}}
	r := &agentRuntime{client: c, handler: handler, capability: AgentCapability{ContextMaintenance: true}, sessions: map[string]*agentSession{}}
	drained := make(chan struct{})
	close(drained)
	r.sessions[compactParams().SessionID] = &agentSession{runtime: r, id: compactParams().SessionID, drained: drained, wake: make(chan struct{}, 1), results: map[string]TurnStartResult{}, pending: map[string]chan InteractionResponseParams{}, customTools: map[string]chan AgentCustomToolResultParams{}}
	return r
}
func requireContextCode(t *testing.T, err error, code int) {
	t.Helper()
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != code {
		t.Fatalf("want RPC %d, got %v", code, err)
	}
}

func TestContextMaintenanceJoinsUnknownRetryAndRejectsCompetingTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	r := compactRuntime(t, compactTestAgent{compact: func(_ context.Context, p SessionCompactParams) (SessionCompactResult, error) {
		calls.Add(1)
		close(started)
		<-release
		return compactResult(p), nil
	}})
	p := compactParams()
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := r.sessionCompact(ctx, p); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	changed := p
	changed.ExpectedLastInputID = "different_source"
	_, err := r.sessionCompact(context.Background(), changed)
	requireContextCode(t, err, CodeConflict)
	_, err = r.turnStart(context.Background(), TurnStartParams{SessionID: p.SessionID, TurnID: "must_not_run", InputID: "another", Input: []AgentInputBlock{{Type: "text", Text: "blocked"}}})
	requireContextCode(t, err, CodeConflict)
	if victim := r.claimVictim(0, nil, map[*agentSession]bool{}); victim != nil {
		t.Fatal("maintenance was suspended")
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	retryCancel()
	// The caller may disconnect again while the same operation is pending.
	// Joining it must never invoke the handler a second time.
	if _, err := r.sessionCompact(retryCtx, p); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	session := r.sessions[p.SessionID]
	session.mu.Lock()
	work := session.maintenance
	session.mu.Unlock()
	close(release)
	<-work.done
	if work.err != nil || work.result.OperationID != p.OperationID || work.result.ContextVersion != 1 {
		t.Fatal(work.result, work.err)
	}

	if calls.Load() != 1 {
		t.Fatal("unknown retry repeated maintenance", calls.Load())
	}
	if len(r.sessions[p.SessionID].outbox) != 0 {
		t.Fatal("maintenance produced an ordinary turn event")
	}
}

func TestContextMaintenanceCancelRemainsReservedUntilSettled(t *testing.T) {
	started, aborted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r := compactRuntime(t, compactTestAgent{compact: func(ctx context.Context, _ SessionCompactParams) (SessionCompactResult, error) {
		close(started)
		<-ctx.Done()
		close(aborted)
		<-release
		return SessionCompactResult{}, ctx.Err()
	}})
	p := compactParams()
	finished := make(chan error, 1)
	go func() { _, err := r.sessionCompact(context.Background(), p); finished <- err }()
	<-started
	wrong, err := r.compactCancel(context.Background(), SessionCompactCancelParams{SessionID: p.SessionID, OperationID: "another"})
	if err != nil || wrong.Canceled {
		t.Fatal(wrong, err)
	}
	right, err := r.compactCancel(context.Background(), SessionCompactCancelParams{SessionID: p.SessionID, OperationID: p.OperationID})
	if err != nil || !right.Canceled {
		t.Fatal(right, err)
	}
	<-aborted
	changed := p
	changed.OperationID = "while_aborting"
	_, err = r.sessionCompact(context.Background(), changed)
	requireContextCode(t, err, CodeConflict)
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(r.sessions[p.SessionID].outbox) != 0 {
		t.Fatal("cancellation emitted an ordinary turn")
	}
}

func TestContextMaintenanceWireAndReceiptMustBeExact(t *testing.T) {
	var calls atomic.Int64
	r := compactRuntime(t, compactTestAgent{compact: func(_ context.Context, p SessionCompactParams) (SessionCompactResult, error) {
		calls.Add(1)
		return compactResult(p), nil
	}})
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { delete(v, "expected_context_version") },
		func(v map[string]any) { v["expected_context_version"] = nil },
		func(v map[string]any) { v["expected_context_version"] = -1 },
		func(v map[string]any) { v["deadline_ms"] = 300001 },
		func(v map[string]any) { v["expected_last_input_id"] = "" },
		func(v map[string]any) { v["extra_control"] = true },
	} {
		raw, _ := json.Marshal(compactParams())
		var value map[string]any
		_ = json.Unmarshal(raw, &value)
		mutate(value)
		raw, _ = json.Marshal(value)
		_, err := r.sessionCompactRaw(context.Background(), raw)
		requireContextCode(t, err, CodeInvalidParams)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid operation reached handler")
	}
	p := compactParams()
	result := compactResult(p)
	if validateCompactResult(p, result) != nil {
		t.Fatal("valid receipt refused")
	}
	result.SummarySHA256 = "wrong"
	if validateCompactResult(p, result) == nil {
		t.Fatal("unverified summary accepted")
	}
	result = compactResult(p)
	result.Summary = string([]byte{0xff})
	hash := sha256.Sum256([]byte(result.Summary))
	result.SummarySHA256 = hex.EncodeToString(hash[:])
	if validateCompactResult(p, result) == nil {
		t.Fatal("invalid UTF-8 changed during JSON encoding")
	}
	_, err := (&agentRuntime{}).sessionCompact(context.Background(), p)
	requireContextCode(t, err, CodeCapabilityUnavailable)
}

func TestContextMaintenanceCapabilityRequiresItsHandler(t *testing.T) {
	client := &Client{requestHandlers: map[string]func(context.Context, json.RawMessage) (any, error){}}
	client.ServeAgent(nopAgent{}, AgentCapability{ContextMaintenance: true})
	if client.agent.capability.ContextMaintenance {
		t.Fatal("unsupported maintenance advertised")
	}
	_, err := client.agent.sessionCompact(context.Background(), compactParams())
	requireContextCode(t, err, CodeCapabilityUnavailable)
}
