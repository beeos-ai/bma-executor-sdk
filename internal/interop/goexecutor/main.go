// Command goexecutor is the Go executor peer of the cross-language interop
// tests (python/tests/test_executor_interop.py). It connects to the harness at
// -url, answers bash tool calls with "go:<command>", serves a one-message agent,
// and runs until stdin closes.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

type agent struct{}

func (agent) OpenTurn(_ context.Context, params bmaexecutor.TurnStartParams) (bmaexecutor.TurnStartResult, error) {
	return bmaexecutor.TurnStartResult{
		Accepted: true, TurnID: params.TurnID, ExecutorSessionRef: "go_" + params.SessionID,
		Effective: bmaexecutor.AgentTurnConfig{Model: "go-model"},
	}, nil
}

func (agent) RunTurn(ctx context.Context, turn *bmaexecutor.AgentTurn) error {
	text := "go says: " + turn.Params().Input[0].Text
	if _, err := turn.Emit(bmaexecutor.AgentMessageEvent{
		Type: bmaexecutor.AgentEventTypeAgentMessage, Content: []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: text}},
	}); err != nil {
		return err
	}
	return nil
}

func (agent) CloseSession(context.Context, string, string) error { return nil }

func main() {
	url := flag.String("url", "", "harness channel URL")
	token := flag.String("token", "", "bearer token")
	flag.Parse()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		EnvironmentID:  "env_interop",
		Executor:       bmaexecutor.ExecutorInfo{Name: "go-interop", OS: "linux", Arch: "amd64", Version: "0.0.0"},
		TokenSource:    bmaexecutor.StaticToken(*url, *token),
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
	client.OnSessionAssign(func(_ context.Context, params bmaexecutor.SessionAssignParams) (bmaexecutor.SessionAssignResult, error) {
		go func() {
			_, _ = client.SessionReady(context.Background(), bmaexecutor.SessionReadyParams{
				SessionID: params.SessionID, WorkID: params.WorkID, LeaseToken: params.Lease.Token,
			})
		}()
		return bmaexecutor.SessionAssignResult{Accepted: true, SessionID: params.SessionID}, nil
	})
	client.OnToolCall(func(ctx context.Context, call bmaexecutor.ToolCallParams) (bmaexecutor.ToolResultParams, error) {
		input, err := call.DecodeInput()
		if err != nil {
			return bmaexecutor.ToolResultParams{}, err
		}
		command := input.Command
		if command == "sleep" {
			<-ctx.Done()
			return bmaexecutor.ToolResultParams{}, ctx.Err()
		}
		text := "go:" + command
		return bmaexecutor.ToolResultParams{
			Outcome: bmaexecutor.ToolOutcomeSucceeded,
			Content: []bmaexecutor.ContentBlock{{Type: bmaexecutor.ContentBlockTypeText, Text: text}},
		}, nil
	})
	client.ServeAgent(agent{}, bmaexecutor.AgentCapability{Models: []bmaexecutor.AgentModel{{ID: "go-model"}}})
	if _, err := client.Start(context.Background()); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
	fmt.Println("REGISTERED", client.ExecutorID())
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = client.Stop(context.Background(), "stdin closed")
}
