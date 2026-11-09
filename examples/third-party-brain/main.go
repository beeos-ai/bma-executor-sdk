// Command third-party-brain is the smallest brain a third party can write
// against the SDK and the executor contract alone (ADR-0029): it takes a turn,
// decides what to do, asks the user's approval, runs the tool wherever the
// session's hands are, and reports events.
//
// There is no model in it. The "model loop" is one line: a prompt of the form
//
//	bash: <command>
//
// is a request to run that command; anything else is answered with text. A
// real brain replaces decide() with its model and keeps the rest.
//
// What the SDK does for the brain:
//
//   - turn.start, turn.input, turn.cancel and session.close, event ids and
//     sequences, acknowledgements and resend after a reconnect;
//   - the tool route (AgentTurn.Tools): when the session's hands are on another
//     machine the platform hands the turn its tools endpoint, and every call goes
//     there through the platform's gateway, which re-checks policy and approval,
//     runs it at most once and runs it on the hands. The brain never learns where
//     the hands are, and none of its calls can run on this machine by mistake.
//     Tools() is nil when the hands are this machine: this example has no local
//     tools and says so.
//
// Declaring platform_credentials brain_tools (below) is what lets the platform
// give this machine a session whose hands are elsewhere.
//
//	BMA_MACHINE_TOKEN=<machine token> go run ./examples/third-party-brain \
//	  -base-url https://executor.example -machine-id mch_...
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func main() {
	baseURL := flag.String("base-url", "", "BMA executor base URL")
	machineID := flag.String("machine-id", "", "this machine's id (mch_...)")
	flag.Parse()
	token := os.Getenv("BMA_MACHINE_TOKEN")
	if *baseURL == "" || *machineID == "" || token == "" {
		log.Fatal("-base-url, -machine-id and BMA_MACHINE_TOKEN are required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := newClient(bmaexecutor.Options{BaseURL: *baseURL, EnvironmentKey: token, MachineID: *machineID})
	if err != nil {
		log.Fatal(err)
	}
	registered, err := client.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("registered as %s; waiting for turns (Ctrl-C to stop)", registered.ExecutorID)
	select {
	case <-ctx.Done():
	case <-client.Done():
		log.Printf("connection ended: %v", client.Err())
	}
	_ = client.Stop(context.Background(), "shutdown")
}

// capability is what this brain tells the platform about itself.
var capability = bmaexecutor.AgentCapability{
	Harnesses:        []bmaexecutor.AgentHarnessCapability{{ID: "third-party-brain", Version: "0.1.0"}},
	PermissionModes:  []string{"default"},
	InteractionKinds: []string{bmaexecutor.InteractionKindPermission},
	// brain_tools: this machine may be the brain of a session whose hands are
	// on another machine; the platform then hands each turn a tools endpoint.
	PlatformCredentials: []string{bmaexecutor.PlatformCredentialBrainTools},
	MaxSessions:         4,
}

func newClient(options bmaexecutor.Options) (*bmaexecutor.Client, error) {
	options.Executor = bmaexecutor.ExecutorInfo{Name: "third-party-brain", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"}
	client, err := bmaexecutor.NewClient(options)
	if err != nil {
		return nil, err
	}
	client.ServeAgent(brain{}, capability)
	return client, nil
}

type brain struct{}

// OpenTurn answers turn.start: accept, and say what will be applied. Keep it
// short; the work is RunTurn's.
func (brain) OpenTurn(_ context.Context, params bmaexecutor.TurnStartParams) (bmaexecutor.TurnStartResult, error) {
	return bmaexecutor.TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "conv_" + params.SessionID,
		Resumed:   params.ExecutorSessionRef != "",
		Effective: bmaexecutor.AgentTurnConfig{PermissionMode: "default", Harness: "third-party-brain"},
	}, nil
}

// RunTurn is the brain's loop for one turn. Returning nil completes the turn,
// a canceled ctx cancels it, any other error fails it (session.error first).
func (brain) RunTurn(ctx context.Context, turn *bmaexecutor.AgentTurn) error {
	prompt := turn.Params().Input[0].Text
	command, isTool := decide(prompt)
	if !isTool {
		return say(turn, "You said: "+prompt)
	}
	tools := turn.Tools()
	if tools == nil {
		return say(turn, "My hands are this machine and this example brain has no local tools; "+
			"put the hands on another machine (an environment with bma_placement.hands).")
	}

	// 1. The tool call, as an event the user sees.
	callID := "call_" + turn.TurnID() + "_1"
	input := map[string]any{"command": command}
	toolUseID, err := turn.Emit(bmaexecutor.AgentToolUseEvent{
		Type: bmaexecutor.AgentEventTypeAgentToolUse, Name: bmaexecutor.ToolNameBash, Input: input,
		EvaluatedPermission: &bmaexecutor.EvaluatedPermission{Type: "ask"},
	})
	if err != nil {
		return err
	}
	// 2. The user's approval, under the id that is the same after a restart.
	answer, err := turn.RequestInteraction(ctx, bmaexecutor.InteractionRequestParams{
		InteractionID: turn.ApprovalID(callID), Kind: bmaexecutor.InteractionKindPermission,
		ToolUseID: toolUseID, ToolName: bmaexecutor.ToolNameBash, ToolInput: input,
		Scopes:    []string{bmaexecutor.InteractionScopeOnce},
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	result := bmaexecutor.AgentToolResultEvent{Type: bmaexecutor.AgentEventTypeAgentToolResult, ToolUseID: toolUseID}
	if answer.Outcome != bmaexecutor.InteractionOutcomeAllow {
		result.Content, result.IsError = text("not run: the user's answer was "+answer.Outcome), true
		_, err := turn.Emit(result)
		return err
	}
	// 3. The call. The answer has three outcomes: executed, not_forwarded (refused,
	// did not run) and unknown (it may have run: never run it again blindly).
	route := tools.Call(ctx, bmaexecutor.RouteCall{
		ID: callID, ToolUseID: toolUseID, Name: bmaexecutor.ToolNameBash, Input: input, ApprovalID: answer.InteractionID,
	})
	result.Content, result.IsError = text(route.Text), route.IsError
	if _, err := turn.Emit(result); err != nil {
		return err
	}
	return say(turn, fmt.Sprintf("The command finished with outcome %s.", route.Outcome))
}

func (brain) CloseSession(context.Context, string, string) error { return nil }

// decide is the stand-in for a model: "bash: <command>" asks for a tool call.
func decide(prompt string) (command string, isTool bool) {
	command, isTool = strings.CutPrefix(strings.TrimSpace(prompt), "bash:")
	return strings.TrimSpace(command), isTool && strings.TrimSpace(command) != ""
}

func say(turn *bmaexecutor.AgentTurn, value string) error {
	_, err := turn.Emit(bmaexecutor.AgentMessageEvent{Type: bmaexecutor.AgentEventTypeAgentMessage, Content: text(value)})
	return err
}

func text(value string) []bmaexecutor.ContentBlock {
	return []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: value}}
}
