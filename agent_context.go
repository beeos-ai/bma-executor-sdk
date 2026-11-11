package bmaexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// AgentSessionCompactor is optional. Implementations own durable context and
// receipt publication; a failed preparation must keep the original context.
// No tool may run and the operation must honor cancellation before publication.
type AgentSessionCompactor interface {
	CompactSession(context.Context, SessionCompactParams) (SessionCompactResult, error)
	// Persist a fence against late publication, or return the applied receipt.
	// The SDK owns Settled and overwrites that field after this returns.
	CancelSessionCompaction(context.Context, SessionCompactCancelParams) (SessionCompactCancelResult, error)
}

type agentContextMaintenance struct {
	operation, identity string
	cancel              context.CancelFunc
	done                chan struct{}
	result              SessionCompactResult
	err                 error
}

var contextIdentityPattern = regexp.MustCompile(`^[a-zA-Z0-9_:-]{1,160}$`)
var contextOperationPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func invalidContext() error {
	return &RPCError{Code: CodeInvalidParams, Message: "invalid context maintenance request or receipt"}
}
func validateCompactRequest(p SessionCompactParams) error {
	if !contextIdentityPattern.MatchString(p.SessionID) || !contextIdentityPattern.MatchString(p.ExpectedLastInputID) || !contextOperationPattern.MatchString(p.OperationID) || p.ExpectedContextVersion < 0 || p.ExpectedContextVersion >= 9007199254740991 || p.DeadlineMS < 1 || p.DeadlineMS > 300000 || strings.TrimSpace(p.Model) == "" || len(p.Model) > 256 || p.Metadata == nil {
		return invalidContext()
	}
	for _, value := range p.Metadata {
		if len(value) > 32768 {
			return invalidContext()
		}
	}
	raw, _ := json.Marshal(p.Metadata)
	if len(raw) > 65536 {
		return invalidContext()
	}
	return nil
}
func validateCompactResult(p SessionCompactParams, result SessionCompactResult) error {
	hash := sha256.Sum256([]byte(result.Summary))
	if result.SessionID != p.SessionID || result.OperationID != p.OperationID || result.PreviousContextVersion != p.ExpectedContextVersion || result.ContextVersion != p.ExpectedContextVersion+1 || result.SourceEventCount < 1 || result.SourceEventCount > 9007199254740991 || !utf8.ValidString(result.Summary) || strings.TrimSpace(result.Summary) == "" || len(result.Summary) > 524288 || result.SummarySHA256 != hex.EncodeToString(hash[:]) {
		return invalidContext()
	}
	return nil
}
func (r *agentRuntime) sessionCompactRaw(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionCompactParams
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 7 || bytes.Equal(members["expected_context_version"], []byte("null")) {
		return nil, invalidContext()
	}
	for _, key := range []string{"session_id", "operation_id", "expected_context_version", "expected_last_input_id", "model", "deadline_ms", "metadata"} {
		if _, ok := members[key]; !ok {
			return nil, invalidContext()
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil {
		return nil, invalidContext()
	}
	return r.sessionCompact(ctx, p)
}
func (r *agentRuntime) sessionCompact(ctx context.Context, p SessionCompactParams) (SessionCompactResult, error) {
	var empty SessionCompactResult
	handler, ok := r.handler.(AgentSessionCompactor)
	if !r.capability.ContextMaintenance || !ok {
		return empty, &RPCError{Code: CodeCapabilityUnavailable, Message: "context maintenance is not supported"}
	}
	if err := validateCompactRequest(p); err != nil {
		return empty, err
	}
	s, err := r.session(p.SessionID, true)
	if err != nil {
		return empty, err
	}
	identityBytes, _ := json.Marshal([]any{p.SessionID, p.OperationID, p.ExpectedContextVersion, p.ExpectedLastInputID, p.Model})
	identity := string(identityBytes)
	s.mu.Lock()
	if s.closed || s.closing || s.turn != nil || s.suspending != nil || len(s.pending) > 0 || len(s.customTools) > 0 {
		s.mu.Unlock()
		return empty, &RPCError{Code: CodeConflict, Message: "context maintenance requires a settled idle session"}
	}
	work := s.maintenance
	if work != nil && work.identity != identity {
		s.mu.Unlock()
		return empty, &RPCError{Code: CodeConflict, Message: "another context operation owns this session"}
	}
	if work == nil {
		if !r.acquireSlot() {
			s.mu.Unlock()
			return empty, &RPCError{Code: CodeExecutorBusy, Message: "executor_busy"}
		}
		operationCtx, cancel := context.WithTimeout(r.client.rootCtx, time.Duration(p.DeadlineMS)*time.Millisecond)
		work = &agentContextMaintenance{operation: p.OperationID, identity: identity, cancel: cancel, done: make(chan struct{})}
		s.maintenance = work
		s.suspended = false
		s.mu.Unlock()
		go func() {
			defer cancel()
			defer r.active.Add(-1)
			if work.err = s.flush(operationCtx); work.err == nil {
				work.result, work.err = handler.CompactSession(operationCtx, p)
				if work.err == nil {
					work.err = validateCompactResult(p, work.result)
				}
			}
			s.mu.Lock()
			if s.maintenance == work {
				s.maintenance = nil
			}
			s.lastUsed = r.useSequence.Add(1)
			s.mu.Unlock()
			close(work.done)
		}()
	} else {
		s.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		return empty, ctx.Err()
	case <-work.done:
		return work.result, work.err
	}
}
func cancelAsCompact(p SessionCompactCancelParams) SessionCompactParams {
	return SessionCompactParams{SessionID: p.SessionID, OperationID: p.OperationID, ExpectedContextVersion: p.ExpectedContextVersion, ExpectedLastInputID: p.ExpectedLastInputID, Model: p.Model, DeadlineMS: 1, Metadata: map[string]string{}}
}
func (r *agentRuntime) compactCancelRaw(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionCompactCancelParams
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) != nil || len(members) != 5 || bytes.Equal(members["expected_context_version"], []byte("null")) {
		return nil, invalidContext()
	}
	for _, key := range []string{"session_id", "operation_id", "expected_context_version", "expected_last_input_id", "model"} {
		if _, ok := members[key]; !ok {
			return nil, invalidContext()
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&p) != nil {
		return nil, invalidContext()
	}
	return r.compactCancel(ctx, p)
}
func (r *agentRuntime) compactCancel(ctx context.Context, p SessionCompactCancelParams) (SessionCompactCancelResult, error) {
	var empty SessionCompactCancelResult
	handler, ok := r.handler.(AgentSessionCompactor)
	if !r.capability.ContextMaintenance || !ok {
		return empty, &RPCError{Code: CodeCapabilityUnavailable, Message: "context maintenance is not supported"}
	}
	request := cancelAsCompact(p)
	if err := validateCompactRequest(request); err != nil {
		return empty, err
	}
	identityBytes, _ := json.Marshal([]any{p.SessionID, p.OperationID, p.ExpectedContextVersion, p.ExpectedLastInputID, p.Model})
	identity := string(identityBytes)
	session, _ := r.session(p.SessionID, false)
	if session != nil {
		session.mu.Lock()
		if work := session.maintenance; work != nil && work.operation == p.OperationID {
			if work.identity != identity {
				session.mu.Unlock()
				return empty, &RPCError{Code: CodeConflict, Message: "context operation identity changed"}
			}
			work.cancel()
		}
		session.mu.Unlock()
	}
	// The host must persist its publication fence even if this SDK process
	// has never seen the delayed operation, or just restarted.
	result, err := handler.CancelSessionCompaction(ctx, p)
	if err != nil {
		return empty, err
	}
	if result.SessionID != p.SessionID || result.OperationID != p.OperationID || result.Canceled == (result.Receipt != nil) {
		return empty, invalidContext()
	}
	if result.Receipt != nil {
		if err := validateCompactResult(request, *result.Receipt); err != nil {
			return empty, err
		}
	}
	result.Settled = true
	if session != nil {
		session.mu.Lock()
		if work := session.maintenance; work != nil && work.operation == p.OperationID {
			result.Settled = false
		}
		session.mu.Unlock()
	}
	return result, nil
}
