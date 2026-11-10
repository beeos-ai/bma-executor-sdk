package bmaexecutor

// Opening the agent's session, off the turn.start path.
//
// The contract has the executor answer turn.start "as soon as the turn is
// accepted" and report progress with session.event. Opening the hosted agent's
// session (ACP session/new or session/load, and for Claude Code the mode
// assertion) can take seconds, so OpenTurn only validates and records what the
// turn asked for (acpOpenPlan); RunTurn opens the session before it prompts:
//
//   - a failure ends the turn with session.error and session.status_idle
//     failed, typed with the contract's AgentErrorCode: agent_unavailable (the
//     process could not be started or is gone), agent_auth_required (ACP
//     -32000), agent_protocol_error (the agent rejected the request), else
//     agent_error (also a conversation to resume that does not exist);
//   - turn.cancel or a deadline during the open ends the turn canceled at
//     once. The open itself is not abandoned half way: it runs on to its end
//     and leaves the session open for the next turn, so no agent session is
//     orphaned in the agent process;
//   - a second turn.start that arrives while the open runs joins it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// acpOpenTimeout bounds one open of an agent session (process start included).
const acpOpenTimeout = 2 * time.Minute

// acpOpenPlan is what a turn.start asked the agent session to be.
type acpOpenPlan struct {
	params    TurnStartParams
	cwd       string
	promptCwd string
	model     string
	mode      string
	system    string
	hands     *EnvironmentInfo
}

// acpOpening is an open of a session in flight.
type acpOpening struct {
	done chan struct{}
	err  error // set before done is closed
}

// openSession makes the agent's session ready for the turn recorded in
// s.plan, or fails with an *AgentError. It returns when ctx ends, without
// stopping the open.
func (a *ACPAgent) openSession(ctx context.Context, s *acpSession) error {
	a.mu.Lock()
	op := s.opening
	if op == nil {
		op = &acpOpening{done: make(chan struct{})}
		s.opening = op
		plan := s.plan
		go func() {
			openCtx, cancel := context.WithTimeout(context.Background(), acpOpenTimeout)
			defer cancel()
			err := a.doOpen(openCtx, s, plan)
			a.mu.Lock()
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

func (a *ACPAgent) doOpen(ctx context.Context, s *acpSession, plan acpOpenPlan) error {
	params := plan.params
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: "agent unavailable: " + err.Error()}
	}
	if a.claude() {
		a.mu.Lock()
		open := s.proc == proc
		a.mu.Unlock()
		if !open {
			if err := a.openClaudeSession(ctx, proc, s, params, plan.cwd, a.systemPrompt(plan.promptCwd, plan.system, plan.hands)); err != nil {
				return err
			}
		}
		if err := a.claudeDefaultMode(ctx, proc, s); err != nil {
			return err
		}
		a.mu.Lock()
		s.mode = plan.mode
		a.mu.Unlock()
		return nil
	}
	a.mu.Lock()
	open := s.proc == proc
	a.mu.Unlock()
	if !open {
		meta := map[string]any{"sessionId": params.SessionID, "systemPrompt": a.systemPrompt(plan.promptCwd, plan.system, plan.hands), "model": a.modelMeta(s, plan.model, params.Metadata)}
		var res acpSessionResponse
		if params.ExecutorSessionRef != "" {
			err = proc.call(ctx, "session/load", map[string]any{"sessionId": params.ExecutorSessionRef, "cwd": plan.cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
			var acpErr *acpError
			if errors.As(err, &acpErr) && acpErr.Code == CodeNotFound {
				return &AgentError{Code: AgentErrorCodeAgentError, Message: "the local conversation " + params.ExecutorSessionRef + " was not found; it is not recreated"}
			}
		} else {
			err = proc.call(ctx, "session/new", map[string]any{"cwd": plan.cwd, "mcpServers": []any{}, "_meta": map[string]any{acpMetaKey: meta}}, &res)
		}
		if err != nil {
			return acpOpenError(proc, "agent session", err)
		}
		if violation := a.catalogViolation(res.Meta.Bma.ToolCatalog); violation != "" {
			return &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: violation}
		}
		a.mu.Lock()
		s.proc, s.mode = proc, "default"
		a.mu.Unlock()
	}
	a.mu.Lock()
	current := s.mode
	a.mu.Unlock()
	if current != plan.mode {
		if err := proc.call(ctx, "session/set_mode", map[string]any{"sessionId": params.SessionID, "modeId": plan.mode}, nil); err != nil {
			return acpOpenError(proc, "agent set_mode", err)
		}
		a.mu.Lock()
		s.mode = plan.mode
		a.mu.Unlock()
	}
	return nil
}

// acpOpenError types a failed call to the agent while its session is opened.
func acpOpenError(proc *acpProcess, what string, err error) error {
	message := what + ": " + err.Error()
	var acpErr *acpError
	switch {
	case !proc.alive():
		return &AgentError{Code: AgentErrorCodeAgentUnavailable, Message: message}
	case errors.As(err, &acpErr) && acpErr.Code == acpAuthRequired:
		return &AgentError{Code: AgentErrorCodeAgentAuthRequired, Message: message}
	case errors.As(err, &acpErr) && (acpErr.Code == acpInvalidParams || acpErr.Code == acpMethodNotFound):
		return &AgentError{Code: AgentErrorCodeAgentProtocolError, Message: message}
	}
	return &AgentError{Code: AgentErrorCodeAgentError, Message: fmt.Sprint(message)}
}

// ACP (JSON-RPC) error codes the open maps.
const (
	acpAuthRequired   = -32000
	acpInvalidParams  = -32602
	acpMethodNotFound = -32601
)

// --- prewarm ---

// Prewarm of the Claude Code profile. Starting Claude Code for the first time
// in a process costs most of a second more than later sessions (measured: the
// first session/new of a claude-agent-acp process ~1.1 s, the next ones
// ~0.2 s), and without it the first turn after registration pays that. The
// agent process itself is already running and initialized when NewACPAgent
// returns (like bma-dsh-acp, which the platform keeps resident); NewACPAgent
// also opens one throwaway session in the background, in a scratch directory
// under StateDir with the same options a real session gets, and closes it
// without ever prompting it (a prompt would spend the owner's model credit).
//
// A real session is not made ahead of time and adopted later: a Claude Code
// session fixes its working directory, system prompt and tool-server credential
// when it is created, and those belong to the platform session that has not
// arrived yet. So the gain is the cold start, not session/new itself.
//
// The prewarm never delays or fails registration: it runs after NewACPAgent
// returns. A failure is logged at LogWarn ("agent prewarm failed") and is
// returned by WaitPrewarm; the first turn then opens its session as it would
// have anyway.

// WaitPrewarm waits for the background prewarm of NewACPAgent and returns its
// error. It returns nil at once when the profile has no prewarm
// (ACPProfileDSH) or ACPAgentOptions.DisablePrewarm is set.
func (a *ACPAgent) WaitPrewarm(ctx context.Context) error {
	if a.warm == nil {
		return nil
	}
	select {
	case <-a.warm.done:
		return a.warm.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type acpPrewarm struct {
	done chan struct{}
	err  error
}

func (a *ACPAgent) startPrewarm() {
	if !a.claude() || a.opts.DisablePrewarm {
		return
	}
	a.warm = &acpPrewarm{done: make(chan struct{})}
	go func() {
		defer close(a.warm.done)
		ctx, cancel := context.WithTimeout(context.Background(), acpOpenTimeout)
		defer cancel()
		began := time.Now()
		if a.warm.err = a.prewarmSession(ctx); a.warm.err != nil {
			a.log(LogWarn, "agent prewarm failed; the first turn opens its session itself", a.warm.err)
			return
		}
		a.log(LogInfo, fmt.Sprintf("agent prewarmed in %s", time.Since(began).Round(time.Millisecond)), nil)
	}()
}

// prewarmSession opens a Claude Code session and closes it unused.
func (a *ACPAgent) prewarmSession(ctx context.Context) error {
	proc, err := a.ensureProcess(ctx)
	if err != nil {
		return err
	}
	cwd := filepath.Join(a.opts.StateDir, "prewarm")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		return err
	}
	if runAs := a.opts.Core.opts.RunAs; runAs != nil {
		if err := os.Chown(cwd, int(runAs.UID), int(runAs.GID)); err != nil {
			return err
		}
	}
	s := &acpSession{id: "prewarm", placeholder: "bma-prewarm-" + fmt.Sprint(time.Now().UnixNano())}
	if err := a.openClaudeSession(ctx, proc, s, TurnStartParams{}, cwd, a.systemPrompt(cwd, "", nil)); err != nil {
		return err
	}
	if err := proc.call(ctx, "session/close", map[string]any{"sessionId": s.wire()}, nil); err != nil {
		a.log(LogDebug, "acp session/close of the prewarm session", err)
	}
	return nil
}
