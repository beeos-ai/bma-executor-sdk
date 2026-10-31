// Command agentharness is the Go agent-platform peer of the cross-language
// tests: a Go SDK Harness with an in-memory AgentPlatform that prints
// "LISTEN <ws-url>", starts one turn ("hello") on the first executor, allows
// its permission interaction, and expects the durable events
// session.status_running, agent.message "<language>:hello:allow",
// session.status_idle (completed) with sequences 1..3. It then closes the
// session and prints PASS.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

type platform struct {
	mu     sync.Mutex
	events []bmaexecutor.AgentEventRecord
	idle   chan struct{}
}

func (p *platform) AckedSequence(context.Context, string) (int64, error) { return 0, nil }

func (p *platform) AgentEvent(_ context.Context, _ *bmaexecutor.Executor, event bmaexecutor.AgentEventRecord) error {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.mu.Unlock()
	if event.Type == bmaexecutor.AgentEventTypeSessionStatusIdle {
		close(p.idle)
	}
	return nil
}

func (p *platform) AgentDelta(*bmaexecutor.Executor, bmaexecutor.SessionEventDeltaParams) {}

func (p *platform) InteractionRequested(_ context.Context, executor *bmaexecutor.Executor, request bmaexecutor.InteractionRequestParams) (string, error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = executor.RespondInteraction(ctx, bmaexecutor.InteractionResponseParams{
			SessionID: request.SessionID, InteractionID: request.InteractionID, Outcome: bmaexecutor.InteractionOutcomeAllow,
		})
	}()
	return "pint_go", nil
}

func (p *platform) InteractionCanceled(context.Context, *bmaexecutor.Executor, bmaexecutor.InteractionCancelParams) error {
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := &platform{idle: make(chan struct{})}
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{Agent: p})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(listener, harness) }()
	fmt.Printf("LISTEN ws://%s\n", listener.Addr())

	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		return err
	}
	capability, err := executor.AgentCapability()
	if err != nil || capability == nil || !capability.Enabled {
		return fmt.Errorf("agent capability = %+v, %v", capability, err)
	}
	result, err := executor.StartTurn(ctx, bmaexecutor.TurnStartParams{
		SessionID: "as_go", TurnID: "turn_go", InputID: "in_go",
		Input: []bmaexecutor.AgentInputBlock{{Type: "text", Text: "hello"}},
	})
	if err != nil || !result.Accepted || result.TurnID != "turn_go" {
		return fmt.Errorf("turn.start = %+v, %v", result, err)
	}
	select {
	case <-p.idle:
	case <-ctx.Done():
		return fmt.Errorf("no session.status_idle: %v", ctx.Err())
	}
	p.mu.Lock()
	events := p.events
	p.mu.Unlock()
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.Type
		if event.Sequence != int64(i+1) {
			return fmt.Errorf("event %d has sequence %d", i, event.Sequence)
		}
	}
	if strings.Join(types, ",") != "session.status_running,agent.message,session.status_idle" {
		return fmt.Errorf("event types = %v", types)
	}
	var message bmaexecutor.AgentMessageEvent
	if err := json.Unmarshal(events[1].Event, &message); err != nil || len(message.Content) == 0 ||
		!strings.HasSuffix(message.Content[0].Text, ":hello:allow") {
		return fmt.Errorf("agent.message = %s", events[1].Event)
	}
	var idle bmaexecutor.SessionStatusIdleEvent
	if err := json.Unmarshal(events[2].Event, &idle); err != nil || idle.TurnOutcome != bmaexecutor.TurnOutcomeCompleted {
		return fmt.Errorf("idle = %s", events[2].Event)
	}
	closed, err := executor.CloseAgentSession(ctx, bmaexecutor.SessionCloseParams{SessionID: "as_go", Reason: "done"})
	if err != nil || !closed.Closed {
		return fmt.Errorf("session.close = %+v, %v", closed, err)
	}
	return nil
}
