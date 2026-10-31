// Command harness is the cross-language conformance peer: a Go SDK Harness
// that prints "LISTEN <ws-url>", waits for one executor and drives the
// standard scenario. It exits 0 after printing PASS. The executor under test
// must answer bash with "<language>:<command>" and block on tool "block"
// until canceled (see typescript/test/executor.test.ts).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/go/executor"
)

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
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{
		Authenticate: func(r *http.Request) (bmaexecutor.Principal, error) {
			if r.Header.Get("Authorization") != "Bearer conformance" {
				return bmaexecutor.Principal{}, fmt.Errorf("missing bearer token")
			}
			return bmaexecutor.Principal{}, nil
		},
	})
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
	session, err := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{SessionID: "sess_conformance"})
	if err != nil {
		return fmt.Errorf("assign: %w", err)
	}
	if err := session.Ready(ctx); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	command := "hello"
	call := bmaexecutor.ToolCallParams{ToolCallID: "call_1", Tool: bmaexecutor.ToolNameBash, Input: bmaexecutor.NewToolInput(bmaexecutor.ToolInput{Command: command})}
	for range 2 { // the repeat must return the recorded result
		result, err := session.CallTool(ctx, call)
		if err != nil {
			return fmt.Errorf("tool.call: %w", err)
		}
		if result.Outcome != bmaexecutor.ToolOutcomeSucceeded || len(result.Content) == 0 || result.Content[0].Text == "" ||
			!strings.HasSuffix(result.Content[0].Text, ":hello") {
			return fmt.Errorf("tool.result = %+v", result)
		}
	}
	blocked := make(chan bmaexecutor.ToolResultParams, 1)
	go func() {
		result, _ := session.CallTool(ctx, bmaexecutor.ToolCallParams{ToolCallID: "call_block", Tool: "block"})
		blocked <- result
	}()
	for {
		canceled, err := session.CancelTool(ctx, "call_block", "conformance")
		if err != nil {
			return fmt.Errorf("tool.cancel: %w", err)
		}
		if canceled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result := <-blocked; result.Outcome != bmaexecutor.ToolOutcomeCanceled {
		return fmt.Errorf("canceled result = %+v", result)
	}
	if err := session.Stop(ctx, "done"); err != nil {
		return fmt.Errorf("session.stop: %w", err)
	}
	return nil
}
