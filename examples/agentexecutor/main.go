// Command agentexecutor is a scripted local agent ("brain and hands" on this
// machine) served to the platform through the executor SDK's agent
// capability.
//
// Without flags it also runs a simulated platform on loopback, behind a TCP
// proxy that can drop the network, and walks the whole flow: task, streamed
// events, approval, result, follow-up, cancel, and resend after a dropped
// connection.
//
//	go run ./examples/agentexecutor
//
// With -base-url it serves the same agent to a real platform instead, using
// BMA_ENVIRONMENT_KEY and BMA_ENVIRONMENT_ID from the environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

func main() {
	baseURL := flag.String("base-url", "", "platform base URL; empty runs the local simulation")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if *baseURL == "" {
		err = simulate(ctx)
	} else {
		err = serve(ctx, *baseURL)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// --- the local agent ---

// scriptedAgent stands in for a real model loop. Each turn streams a reply;
// a prompt containing "run" asks permission for one shell command and runs it
// only when allowed; "wait" keeps working until canceled.
type scriptedAgent struct{}

var capability = bmaexecutor.AgentCapability{
	Models:           []bmaexecutor.AgentModel{{ID: "scripted-1", ThinkingLevels: []string{"off", "low", "high"}, DefaultThinkingLevel: "low"}},
	PermissionModes:  []string{"default", "accept_edits"},
	InteractionKinds: []string{bmaexecutor.InteractionKindPermission, bmaexecutor.InteractionKindQuestion},
	MaxSessions:      4,
}

func (scriptedAgent) OpenTurn(_ context.Context, params bmaexecutor.TurnStartParams) (bmaexecutor.TurnStartResult, error) {
	effective := bmaexecutor.AgentTurnConfig{Model: "scripted-1", ThinkingLevel: "low", PermissionMode: "default", Cwd: "."}
	if params.Config != nil {
		if params.Config.ThinkingLevel != "" {
			effective.ThinkingLevel = params.Config.ThinkingLevel
		}
		if params.Config.Cwd != "" {
			effective.Cwd = params.Config.Cwd
		}
	}
	return bmaexecutor.TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "conv_" + params.SessionID,
		Resumed: params.ExecutorSessionRef != "", Effective: effective,
	}, nil
}

func (scriptedAgent) RunTurn(ctx context.Context, turn *bmaexecutor.AgentTurn) error {
	prompt := turn.Params().Input[0].Text
	reply := turn.NewEventID()
	words := strings.Fields("Sure, working on: " + prompt)
	for i, word := range words {
		if i > 0 {
			word = " " + word
		}
		turn.Delta(reply, bmaexecutor.EventDeltaTypeAgentMessage, 0, word)
		if err := pause(ctx, 30*time.Millisecond); err != nil {
			return err
		}
	}
	if _, err := turn.EmitWithID(reply, message(strings.Join(words, " "))); err != nil {
		return err
	}
	if strings.Contains(prompt, "wait") {
		for i := 1; ; i++ {
			select {
			case input := <-turn.Inputs():
				if _, err := turn.Emit(message("Noted: " + input.Input[0].Text)); err != nil {
					return err
				}
			case <-time.After(100 * time.Millisecond):
				if _, err := turn.Emit(message(fmt.Sprintf("still working (%d)", i))); err != nil {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if strings.Contains(prompt, "run") {
		input := map[string]any{"command": "go version"}
		toolUseID, err := turn.Emit(bmaexecutor.AgentToolUseEvent{
			Type: bmaexecutor.AgentEventTypeAgentToolUse, Name: "bash", Input: input,
			EvaluatedPermission: &bmaexecutor.EvaluatedPermission{Type: "ask"},
		})
		if err != nil {
			return err
		}
		answer, err := turn.RequestInteraction(ctx, bmaexecutor.InteractionRequestParams{
			Kind: bmaexecutor.InteractionKindPermission, ToolUseID: toolUseID, ToolName: "bash", ToolInput: input,
			Scopes:    []string{bmaexecutor.InteractionScopeOnce, bmaexecutor.InteractionScopeSession},
			ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		})
		if err != nil {
			return err
		}
		result := bmaexecutor.AgentToolResultEvent{Type: bmaexecutor.AgentEventTypeAgentToolResult, ToolUseID: toolUseID}
		// The platform's answer is advice; local policy has the last word.
		if answer.Outcome == bmaexecutor.InteractionOutcomeAllow {
			result.Content = text("go version go1.24 (simulated)")
		} else {
			result.Content, result.IsError = text("not run: "+answer.Outcome), true
		}
		if _, err := turn.Emit(result); err != nil {
			return err
		}
		if _, err := turn.Emit(message("Done.")); err != nil {
			return err
		}
	}
	for {
		select {
		case input := <-turn.Inputs():
			if _, err := turn.Emit(message("Noted: " + input.Input[0].Text)); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (scriptedAgent) CloseSession(context.Context, string, string) error { return nil }

func message(value string) bmaexecutor.AgentMessageEvent {
	return bmaexecutor.AgentMessageEvent{Type: bmaexecutor.AgentEventTypeAgentMessage, Content: text(value)}
}

func text(value string) []bmaexecutor.ContentBlock {
	return []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: value}}
}

func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newClient(options bmaexecutor.Options) (*bmaexecutor.Client, error) {
	options.Executor = bmaexecutor.ExecutorInfo{Name: "agentexecutor-example", OS: "darwin", Arch: "arm64", Version: "0.1.0"}
	client, err := bmaexecutor.NewClient(options)
	if err != nil {
		return nil, err
	}
	client.ServeAgent(scriptedAgent{}, capability)
	return client, nil
}

func serve(ctx context.Context, baseURL string) error {
	client, err := newClient(bmaexecutor.Options{
		BaseURL: baseURL, EnvironmentKey: os.Getenv("BMA_ENVIRONMENT_KEY"), EnvironmentID: os.Getenv("BMA_ENVIRONMENT_ID"),
	})
	if err != nil {
		return err
	}
	if _, err := client.Start(ctx); err != nil {
		return err
	}
	log.Printf("serving the scripted agent to %s; Ctrl-C to stop", baseURL)
	<-ctx.Done()
	return client.Stop(context.Background(), "shutdown")
}

// --- the simulated platform ---

// platform prints what a real platform would persist and publish, and
// answers permission requests the way a phone user would.
type platform struct {
	mu        sync.Mutex
	acked     map[string]int64
	idle      map[string]chan string
	executor  *bmaexecutor.Executor
	streaming map[string]bool
}

func (p *platform) AckedSequence(_ context.Context, sessionID string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acked[sessionID], nil
}

func (p *platform) AgentEvent(_ context.Context, _ *bmaexecutor.Executor, event bmaexecutor.AgentEventRecord) error {
	p.mu.Lock()
	p.acked[event.SessionID] = event.Sequence
	if len(p.streaming) > 0 {
		// A durable event interrupts the preview line; the next delta
		// starts a fresh one.
		fmt.Println()
		clear(p.streaming)
	}
	p.mu.Unlock()
	fmt.Printf("  [%s #%d %s] %s\n", event.TurnID, event.Sequence, event.Type, summary(event))
	if event.Type == bmaexecutor.AgentEventTypeSessionStatusIdle {
		var idle bmaexecutor.SessionStatusIdleEvent
		_ = json.Unmarshal(event.Event, &idle)
		p.mu.Lock()
		done := p.idle[event.TurnID]
		p.mu.Unlock()
		if done != nil {
			done <- idle.TurnOutcome
		}
	}
	return nil
}

func (p *platform) AgentDelta(_ *bmaexecutor.Executor, delta bmaexecutor.SessionEventDeltaParams) {
	p.mu.Lock()
	first := !p.streaming[delta.EventID]
	p.streaming[delta.EventID] = true
	p.mu.Unlock()
	if first {
		fmt.Printf("  [%s preview %s] ", delta.TurnID, delta.EventType)
	}
	fmt.Print(delta.Text)
}

func (p *platform) InteractionRequested(_ context.Context, executor *bmaexecutor.Executor, request bmaexecutor.InteractionRequestParams) (string, error) {
	fmt.Printf("  [%s interaction] %s %s %v -> shown on desktop and phone\n", request.TurnID, request.Kind, request.ToolName, request.ToolInput)
	go func() {
		time.Sleep(200 * time.Millisecond)
		result, err := executor.RespondInteraction(context.Background(), bmaexecutor.InteractionResponseParams{
			SessionID: request.SessionID, InteractionID: request.InteractionID, Outcome: bmaexecutor.InteractionOutcomeAllow,
			Scope: bmaexecutor.InteractionScopeOnce, Responder: &bmaexecutor.InteractionResponder{Surface: "mobile"},
		})
		fmt.Printf("  [%s interaction] allowed from mobile (accepted=%v err=%v)\n", request.TurnID, result.Accepted, err)
	}()
	return "aint_" + request.InteractionID, nil
}

func (p *platform) InteractionCanceled(_ context.Context, _ *bmaexecutor.Executor, cancel bmaexecutor.InteractionCancelParams) error {
	fmt.Printf("  [interaction %s] withdrawn: %s\n", cancel.InteractionID, cancel.Reason)
	return nil
}

func summary(event bmaexecutor.AgentEventRecord) string {
	var body struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Name        string `json:"name"`
		TurnOutcome string `json:"turn_outcome"`
	}
	_ = json.Unmarshal(event.Event, &body)
	switch {
	case body.TurnOutcome != "":
		return "turn " + body.TurnOutcome
	case body.Name != "":
		return body.Name
	case len(body.Content) > 0:
		return body.Content[0].Text
	}
	return ""
}

func (p *platform) waitTurn(ctx context.Context, turnID string) (string, error) {
	p.mu.Lock()
	done := p.idle[turnID]
	p.mu.Unlock()
	select {
	case outcome := <-done:
		return outcome, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *platform) start(ctx context.Context, params bmaexecutor.TurnStartParams) (bmaexecutor.TurnStartResult, error) {
	p.mu.Lock()
	p.idle[params.TurnID] = make(chan string, 1)
	p.mu.Unlock()
	fmt.Printf("\n> turn.start %s: %q\n", params.TurnID, params.Input[0].Text)
	result, err := p.executor.StartTurn(ctx, params)
	if err == nil {
		fmt.Printf("  effective config: model=%s thinking=%s permission=%s cwd=%s resumed=%v ref=%s\n",
			result.Effective.Model, result.Effective.ThinkingLevel, result.Effective.PermissionMode, result.Effective.Cwd, result.Resumed, result.ExecutorSessionRef)
	}
	return result, err
}

func simulate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	p := &platform{acked: map[string]int64{}, idle: map[string]chan string{}, streaming: map[string]bool{}}
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{Agent: p})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(listener, harness) }()
	proxy, err := newProxy(listener.Addr().String())
	if err != nil {
		return err
	}

	client, err := newClient(bmaexecutor.Options{
		EnvironmentID: "env_local", TokenSource: bmaexecutor.StaticToken("ws://"+proxy.addr(), ""),
		ReconnectBaseDelay: 300 * time.Millisecond,
	})
	if err != nil {
		return err
	}
	if _, err := client.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = client.Stop(context.Background(), "done") }()
	if p.executor, err = harness.WaitExecutor(ctx); err != nil {
		return err
	}
	advertised, _ := p.executor.AgentCapability()
	fmt.Printf("executor registered with agent capability: models=%s permission_modes=%v\n", advertised.Models[0].ID, advertised.PermissionModes)

	session := "sesn_demo"
	first, err := p.start(ctx, bmaexecutor.TurnStartParams{
		SessionID: session, TurnID: "turn_1", InputID: "ain_1",
		Input:  []bmaexecutor.AgentInputBlock{{Type: bmaexecutor.AgentInputBlockTypeText, Text: "run the build"}},
		Config: &bmaexecutor.AgentTurnConfig{ThinkingLevel: "high"},
	})
	if err != nil {
		return err
	}
	if _, err := p.waitTurn(ctx, "turn_1"); err != nil {
		return err
	}

	if _, err := p.start(ctx, bmaexecutor.TurnStartParams{
		SessionID: session, TurnID: "turn_2", InputID: "ain_2", ExecutorSessionRef: first.ExecutorSessionRef,
		Input: []bmaexecutor.AgentInputBlock{{Type: bmaexecutor.AgentInputBlockTypeText, Text: "wait for the deploy"}},
	}); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	fmt.Println("> turn.input ain_3 while the turn runs")
	if _, err := p.executor.SendTurnInput(ctx, bmaexecutor.TurnInputParams{
		SessionID: session, TurnID: "turn_2", InputID: "ain_3",
		Input: []bmaexecutor.AgentInputBlock{{Type: bmaexecutor.AgentInputBlockTypeText, Text: "skip the slow tests"}},
	}); err != nil {
		return err
	}
	fmt.Println("> network drop: events keep queuing on the executor")
	proxy.drop()
	time.Sleep(400 * time.Millisecond)
	fmt.Println("> reconnected: unacknowledged events were resent in order")
	time.Sleep(300 * time.Millisecond)
	fmt.Println("> turn.cancel turn_2")
	if _, err := cancelWhenConnected(ctx, p.executor, bmaexecutor.TurnCancelParams{SessionID: session, TurnID: "turn_2", Reason: "user_interrupt"}); err != nil {
		return err
	}
	if outcome, err := p.waitTurn(ctx, "turn_2"); err != nil || outcome != bmaexecutor.TurnOutcomeCanceled {
		return fmt.Errorf("turn_2 ended %q: %v", outcome, err)
	}

	closed, err := p.executor.CloseAgentSession(ctx, bmaexecutor.SessionCloseParams{SessionID: session, Reason: "done"})
	if err != nil {
		return err
	}
	fmt.Printf("\n> session.close: closed=%v\n", closed.Closed)
	return nil
}

func cancelWhenConnected(ctx context.Context, executor *bmaexecutor.Executor, params bmaexecutor.TurnCancelParams) (bmaexecutor.TurnCancelResult, error) {
	for {
		result, err := executor.CancelTurn(ctx, params)
		if !errors.Is(err, bmaexecutor.ErrNotConnected) {
			return result, err
		}
		if err := pause(ctx, 100*time.Millisecond); err != nil {
			return result, err
		}
	}
}

// proxy forwards TCP to the platform and can cut every connection at once,
// like a laptop losing Wi-Fi.
type proxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	conns    []net.Conn
}

func newProxy(target string) (*proxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxy{listener: listener, target: target}
	go p.accept()
	return p, nil
}

func (p *proxy) addr() string { return p.listener.Addr().String() }

func (p *proxy) accept() {
	for {
		in, err := p.listener.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = in.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, in, out)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
		go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
	}
}

func (p *proxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}
