package bmaexecutor

// Opening the brain's conversation, off the turn.start path.
//
// The contract has the executor answer turn.start "as soon as the turn is
// accepted" and report progress with session.event. Opening the brain's
// conversation (BrainAdapter.Open) can take seconds, so OpenTurn only
// validates and records what the turn asked for (acpOpenPlan); RunTurn opens
// the conversation before it prompts:
//
//   - a failure ends the turn with session.error and session.status_idle
//     failed, typed with the contract's AgentErrorCode by the adapter:
//     agent_unavailable (the process could not be started or is gone),
//     agent_auth_required, agent_protocol_error (the brain rejected the
//     request), else agent_error (also a conversation to resume that does not
//     exist);
//   - turn.cancel or a deadline during the open ends the turn canceled at
//     once. The open itself is not abandoned half way: it runs on to its end
//     and leaves the conversation open for the next turn, so no conversation
//     is orphaned in the brain;
//   - a second turn.start that arrives while the open runs joins it.

import (
	"context"
	"time"
)

// acpOpenTimeout bounds one open of a brain conversation (process start included).
const acpOpenTimeout = 2 * time.Minute

// acpOpenPlan is what a turn.start asked the brain conversation to be.
type acpOpenPlan struct {
	params    TurnStartParams
	cwd       string
	promptCwd string
	model     string
	effort    string // thinking level ("" none)
	mode      string
	system    string
	hands     *EnvironmentInfo
}

// acpOpening is an open of a session in flight.
type acpOpening struct {
	done chan struct{}
	err  error // set before done is closed
}

// openSession makes the brain's conversation ready for the turn recorded in
// s.plan, or fails with an *AgentError. It returns when ctx ends, without
// stopping the open.
func (a *BrainHost) openSession(ctx context.Context, s *acpSession) error {
	a.mu.Lock()
	op := s.opening
	if op == nil {
		op = &acpOpening{done: make(chan struct{})}
		s.opening = op
		plan, current := s.plan, s.brain
		go func() {
			openCtx, cancel := context.WithTimeout(context.Background(), acpOpenTimeout)
			defer cancel()
			brain, err := a.adapter.Open(openCtx, BrainOpen{
				SessionID: plan.params.SessionID, Ref: plan.params.ExecutorSessionRef, Current: current,
				Cwd: plan.cwd, SystemPrompt: a.systemPrompt(plan.promptCwd, plan.system, plan.hands),
				Mode: plan.mode, Model: plan.model, Metadata: plan.params.Metadata,
			})
			a.mu.Lock()
			if err == nil {
				s.brain, s.mode = brain, plan.mode
			}
			op.err = err
			s.opening = nil
			a.mu.Unlock()
			close(op.done)
		}()
	}
	a.mu.Unlock()
	select {
	case <-op.done:
		return op.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
