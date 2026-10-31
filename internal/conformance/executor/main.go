// Command executor is the Go executor peer of the cross-language tests: a Go
// SDK Client that dials the harness at os.Args[1] (bearer "conformance"),
// answers bash with "go:<command>", blocks on tool "block" until canceled,
// echoes MCP frames on server key "echo", and serves an agent that asks for
// permission, then emits agent.message "go:<first input text>". It prints
// READY after registering and exits when the harness closes it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

func main() {
	if err := run(); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		EnvironmentID: "env_conformance",
		Executor:      bmaexecutor.ExecutorInfo{Name: "go-conformance", OS: "any", Arch: "any", Version: "0"},
		Capabilities:  bmaexecutor.Capabilities{Shell: &bmaexecutor.ShellCapability{Enabled: true}},
		TokenSource:   bmaexecutor.StaticToken(os.Args[1], "conformance"),
	})
	if err != nil {
		return err
	}
	client.OnSessionAssign(func(_ context.Context, params bmaexecutor.SessionAssignParams) (bmaexecutor.SessionAssignResult, error) {
		go func() {
			_, _ = client.SessionReady(ctx, bmaexecutor.SessionReadyParams{SessionID: params.SessionID, WorkID: params.WorkID, LeaseToken: params.Lease.Token})
			channel, err := client.OpenMCPChannel(ctx, params.SessionID, "echo")
			if err != nil {
				return
			}
			defer channel.Close()
			for {
				frame, err := channel.Read(ctx)
				if err != nil {
					return
				}
				_ = channel.Write(ctx, []byte(strings.Replace(string(frame), `"ping"`, `"pong"`, 1)))
			}
		}()
		return bmaexecutor.SessionAssignResult{Accepted: true, SessionID: params.SessionID}, nil
	})
	client.OnToolCall(func(callCtx context.Context, params bmaexecutor.ToolCallParams) (bmaexecutor.ToolResultParams, error) {
		if params.Tool == "block" {
			<-callCtx.Done()
			return bmaexecutor.ToolResultParams{Outcome: bmaexecutor.ToolOutcomeSucceeded}, nil
		}
		input, err := params.DecodeInput()
		if err != nil {
			return bmaexecutor.ToolResultParams{}, err
		}
		command := input.Command
		return bmaexecutor.ToolResultParams{
			Outcome: bmaexecutor.ToolOutcomeSucceeded,
			Content: []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: "go:" + command}},
		}, nil
	})
	client.ServeAgent(echoAgent{}, bmaexecutor.AgentCapability{Models: []bmaexecutor.AgentModel{{ID: "go-echo"}}})
	if _, err := client.Start(ctx); err != nil {
		return err
	}
	fmt.Println("READY")
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for client.State() != bmaexecutor.ConnectionStateClosed && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil
}

type echoAgent struct{}

func (echoAgent) OpenTurn(_ context.Context, params bmaexecutor.TurnStartParams) (bmaexecutor.TurnStartResult, error) {
	return bmaexecutor.TurnStartResult{Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "go_" + params.SessionID}, nil
}

func (echoAgent) RunTurn(ctx context.Context, turn *bmaexecutor.AgentTurn) error {
	answer, err := turn.RequestInteraction(ctx, bmaexecutor.InteractionRequestParams{
		Kind: bmaexecutor.InteractionKindPermission, ToolUseID: "tu_go", ToolName: "bash",
	})
	if err != nil {
		return err
	}
	input := ""
	if blocks := turn.Params().Input; len(blocks) > 0 {
		input = blocks[0].Text
	}
	_, err = turn.Emit(bmaexecutor.AgentMessageEvent{
		Type:    bmaexecutor.AgentEventTypeAgentMessage,
		Content: []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: fmt.Sprintf("go:%s:%s", input, answer.Outcome)}},
	})
	return err
}

func (echoAgent) CloseSession(context.Context, string, string) error { return nil }
