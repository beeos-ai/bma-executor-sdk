// Command mcpharness is the Go harness peer of the cross-language interop
// tests (TypeScript and Python). It listens on TCP or, with -unix, a Unix
// domain socket or, with -npipe, a Windows named pipe, prints
// "LISTEN <ws://, unix:// or npipe:// URL>", negotiates v1.1
// with one executor, calls bash "echo hi" (want "<lang>:echo hi"), sends
// {"method":"ping"} on the session's MCP channel "echo" (want "pong"),
// prints "PASS" and exits 0; any mismatch prints "FAIL <reason>" and exits 1.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/executor"
)

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
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{RequestTimeout: 5 * time.Second})
	network, address, url := "tcp", "127.0.0.1:0", ""
	if unixPath != "" {
		network, address, url = "unix", unixPath, "unix://"+unixPath
	}
	var listener net.Listener
	var err error
	if pipeName != "" {
		listener, err = bmaexecutor.NamedPipeListener(pipeName)
		url = "npipe:////./pipe/" + pipeName
	} else {
		listener, err = net.Listen(network, address)
	}
	if err != nil {
		return err
	}
	if url == "" {
		url = "ws://" + listener.Addr().String() + "/v1/executors/channel"
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
		return fmt.Errorf("protocol %q, want %s", executor.Protocol(), bmaexecutor.ProtocolV11)
	}
	session, err := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{SessionID: "ses_go"})
	if err != nil {
		return fmt.Errorf("assign: %w", err)
	}
	if err := session.Ready(ctx); err != nil {
		return fmt.Errorf("ready: %w", err)
	}
	result, err := session.CallTool(ctx, bmaexecutor.ToolCallParams{ToolCallID: "call_go_1", Tool: bmaexecutor.ToolNameBash,
		Input: bmaexecutor.NewToolInput(bmaexecutor.ToolInput{Command: "echo hi"})})
	if err != nil {
		return fmt.Errorf("tool.call: %w", err)
	}
	if result.Outcome != bmaexecutor.ToolOutcomeSucceeded || len(result.Content) != 1 || !strings.HasSuffix(result.Content[0].Text, ":echo hi") {
		raw, _ := json.Marshal(result)
		return fmt.Errorf("tool.result %s", raw)
	}
	channel, err := session.MCP(ctx, "echo")
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	if err := channel.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		return fmt.Errorf("mcp write: %w", err)
	}
	frame, err := channel.Read(ctx)
	if err != nil || !strings.Contains(string(frame), `"pong"`) {
		return fmt.Errorf("mcp echo = %s err %v", frame, err)
	}
	return session.Stop(ctx, "done")
}
