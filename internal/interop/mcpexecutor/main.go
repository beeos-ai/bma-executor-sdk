// Command mcpexecutor is the Go executor peer of the cross-language interop
// tests (TypeScript and Python). It dials the harness at -url (ws://, wss://
// or unix://path, bearer -token), answers bash with "go:<command>", blocks
// the bash command "block" until canceled, and on every assigned session opens MCP
// channel "echo" and answers each frame with "ping" replaced by "pong". It
// prints "REGISTERED" and exits 0 when stdin closes or the harness ends it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func main() {
	url := flag.String("url", "", "harness channel URL")
	token := flag.String("token", "", "bearer token")
	flag.Parse()
	if err := run(*url, *token); err != nil {
		fmt.Println("FAIL", err)
		os.Exit(1)
	}
}

func run(url, token string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		EnvironmentID: "env_interop",
		Executor:      bmaexecutor.ExecutorInfo{Name: "go-interop", OS: "any", Arch: "any", Version: "0"},
		Capabilities:  bmaexecutor.Capabilities{Shell: &bmaexecutor.ShellCapability{Enabled: true}},
		TokenSource:   bmaexecutor.StaticToken(url, token),
	})
	if err != nil {
		return err
	}
	client.OnSessionAssign(func(_ context.Context, params bmaexecutor.SessionAssignParams) (bmaexecutor.SessionAssignResult, error) {
		go func() {
			_, _ = client.SessionReady(ctx, bmaexecutor.SessionReadyParams{SessionID: params.SessionID, WorkID: params.WorkID, LeaseToken: params.Lease.Token})
			channel, err := client.OpenMCPChannel(ctx, params.SessionID, "echo")
			if err != nil {
				fmt.Fprintln(os.Stderr, "mcp:", err)
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
	client.Tools.Bash.Handle(func(callCtx context.Context, input bmaexecutor.BashInput, _ bmaexecutor.ToolContext) (bmaexecutor.ToolOutput, error) {
		if input.Command == "block" {
			<-callCtx.Done()
			return bmaexecutor.ToolOutput{}, nil
		}
		return bmaexecutor.TextOutput("go:" + input.Command), nil
	})
	if _, err := client.Start(ctx); err != nil {
		return err
	}
	fmt.Println("REGISTERED")
	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()
	select {
	case <-stdinClosed:
		return client.Stop(context.Background(), "done")
	case <-client.Done():
		return nil
	}
}
