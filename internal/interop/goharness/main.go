// Command goharness is the Go harness peer of the cross-language interop tests
// (python/tests/test_executor_interop.py). It listens on TCP or a Unix domain
// socket, prints "LISTEN <url>", drives one executor through a tool session
// and an agent turn, prints "PASS" and exits 0; any mismatch exits 1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

type platform struct {
	mu     sync.Mutex
	events []bmaexecutor.AgentEventRecord
	acked  map[string]int64
	idle   chan bmaexecutor.AgentEventRecord
}

func (p *platform) AckedSequence(_ context.Context, sessionID string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked[sessionID], nil
}

func (p *platform) AgentEvent(_ context.Context, _ *bmaexecutor.Executor, event bmaexecutor.AgentEventRecord) error {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.acked[event.SessionID] = event.Sequence
	p.mu.Unlock()
	if event.Type == bmaexecutor.AgentEventTypeSessionStatusIdle {
		p.idle <- event
	}
	return nil
}

func (p *platform) AgentDelta(*bmaexecutor.Executor, bmaexecutor.SessionEventDeltaParams) {}

func (p *platform) InteractionRequested(_ context.Context, _ *bmaexecutor.Executor, r bmaexecutor.InteractionRequestParams) (string, error) {
	return "pint_" + r.InteractionID, nil
}

func (p *platform) InteractionCanceled(context.Context, *bmaexecutor.Executor, bmaexecutor.InteractionCancelParams) error {
	return nil
}

func main() {
	unixPath := flag.String("unix", "", "listen on this Unix domain socket instead of TCP")
	pipeName := flag.String("npipe", "", "listen on this Windows named pipe (name after \\\\.\\pipe\\) instead of TCP")
	flag.Parse()
	if err := run(*unixPath, *pipeName); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(unixPath, pipeName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := &platform{acked: map[string]int64{}, idle: make(chan bmaexecutor.AgentEventRecord, 4)}
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{RequestTimeout: 5 * time.Second, Agent: agent})

	var (
		listener net.Listener
		url      string
		err      error
	)
	if pipeName != "" {
		listener, err = bmaexecutor.NamedPipeListener(pipeName)
		url = "npipe:////./pipe/" + pipeName
	} else if unixPath != "" {
		listener, err = net.Listen("unix", unixPath)
		url = "unix://" + unixPath
	} else {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			url = "ws://" + listener.Addr().String() + "/v1/executors/channel"
		}
	}
	if err != nil {
		return err
	}
	server := &http.Server{Handler: harness}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	fmt.Println("LISTEN", url)

	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		return err
	}
	if executor.Protocol() != bmaexecutor.ProtocolV11 {
		return fmt.Errorf("protocol %q, want %q", executor.Protocol(), bmaexecutor.ProtocolV11)
	}
	session, err := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{SessionID: "ses_go"})
	if err != nil {
		return fmt.Errorf("assign: %w", err)
	}
	if err := session.Ready(ctx); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	command := "echo hi"
	result, err := session.CallTool(ctx, bmaexecutor.ToolCallParams{ToolCallID: "call_go_1", Tool: "bash", Input: toolInput(command)})
	if err != nil {
		return fmt.Errorf("tool.call: %w", err)
	}
	if result.Outcome != bmaexecutor.ToolOutcomeSucceeded || len(result.Content) != 1 || result.Content[0].Text != "py:echo hi" {
		raw, _ := json.Marshal(result)
		return fmt.Errorf("unexpected tool.result %s", raw)
	}
	// Cancel a long call: the Python handler is canceled and reports canceled.
	sleep := "sleep"
	pending := make(chan bmaexecutor.ToolResultParams, 1)
	go func() {
		r, _ := session.CallTool(ctx, bmaexecutor.ToolCallParams{ToolCallID: "call_go_2", Tool: "bash", Input: toolInput(sleep)})
		pending <- r
	}()
	time.Sleep(200 * time.Millisecond)
	if canceled, err := session.CancelTool(ctx, "call_go_2", "interop"); err != nil || !canceled {
		return fmt.Errorf("tool.cancel: %v %v", canceled, err)
	}
	if r := <-pending; r.Outcome != bmaexecutor.ToolOutcomeCanceled {
		return fmt.Errorf("canceled outcome %q", r.Outcome)
	}
	if err := session.Stop(ctx, "done"); err != nil {
		return fmt.Errorf("session.stop: %w", err)
	}

	// Agent capability.
	if _, err := executor.StartTurn(ctx, bmaexecutor.TurnStartParams{
		SessionID: "ses_agent", TurnID: "turn_1", InputID: "in_1",
		Input: []bmaexecutor.AgentInputBlock{{Type: "text", Text: "hello"}},
	}); err != nil {
		return fmt.Errorf("turn.start: %w", err)
	}
	select {
	case idle := <-agent.idle:
		var event struct {
			TurnOutcome string `json:"turn_outcome"`
		}
		_ = json.Unmarshal(idle.Event, &event)
		if event.TurnOutcome != "completed" {
			return fmt.Errorf("turn outcome %q", event.TurnOutcome)
		}
	case <-ctx.Done():
		return errors.New("no session.status_idle")
	}
	agent.mu.Lock()
	types := make([]string, 0, len(agent.events))
	for i, event := range agent.events {
		if event.Sequence != int64(i+1) {
			agent.mu.Unlock()
			return fmt.Errorf("event %d has sequence %d", i, event.Sequence)
		}
		types = append(types, event.Type)
	}
	agent.mu.Unlock()
	want := []string{"session.status_running", "agent.message", "session.status_idle"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		return fmt.Errorf("agent events %v, want %v", types, want)
	}
	if closed, err := executor.CloseAgentSession(ctx, bmaexecutor.SessionCloseParams{SessionID: "ses_agent", Reason: "done"}); err != nil || !closed.Closed {
		return fmt.Errorf("session.close: %v %v", closed, err)
	}
	return nil
}

func toolInput(command string) json.RawMessage {
	raw, _ := json.Marshal(bmaexecutor.ToolInput{Command: command})
	return raw
}
