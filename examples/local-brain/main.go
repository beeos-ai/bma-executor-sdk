// Command local-brain is a minimal local harness ("brain") driving the same
// executor execution core as the cloud, over loopback with the same protocol:
//
//	go run ./examples/local-brain -mcp-command "go run ./examples/echo-mcp"
//
// The "model" is a fixed script of tool calls. Replace script with a model
// loop to build a real local agent.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-sdks/go/executor"
)

func main() {
	mcpCommand := flag.String("mcp-command", "go run ./examples/echo-mcp", "stdio MCP server command line")
	workRoot := flag.String("work-root", os.TempDir()+"/bma-local-brain", "session workspaces")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Brain: a harness on loopback; the per-run token keeps other local
	// processes from attaching as the executor.
	token := randomToken()
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{
		Authenticate: func(r *http.Request) (bmaexecutor.Principal, error) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				return bmaexecutor.Principal{}, errors.New("bad token")
			}
			return bmaexecutor.Principal{ExecutorID: "exe_local"}, nil
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go func() { _ = http.Serve(listener, harness) }()

	// Hand: the execution core behind an executor client, as in bmad/Desktop.
	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: *workRoot})
	check(err)
	defer core.Close()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		EnvironmentID: "local", Executor: bmaexecutor.ExecutorInfo{Name: "local-hand"},
		Capabilities: core.Capabilities(),
		TokenSource:  bmaexecutor.StaticToken("ws://"+listener.Addr().String(), token),
	})
	check(err)
	check(core.Attach("local", client))
	_, err = client.Start(ctx)
	check(err)
	defer client.Stop(context.Background(), "done")

	executor, err := harness.WaitExecutor(ctx)
	check(err)
	argv := strings.Fields(*mcpCommand)
	session, err := executor.AssignSession(ctx, bmaexecutor.SessionAssignParams{
		SessionID: "sess_demo",
		MCPServers: []bmaexecutor.MCPServerConfig{{
			ServerKey: "echo", ConnectionOrigin: bmaexecutor.MCPConnectionOriginEnvironment,
			Transport: bmaexecutor.MCPServerTransport{Type: bmaexecutor.MCPTransportTypeStdio, Command: &argv[0], Args: argv[1:], Cwd: ptr(mustGetwd())},
		}},
	})
	check(err)
	check(session.Ready(ctx))

	// Scripted "model": two built-in tool calls, then the MCP server.
	for _, command := range []string{"echo hello from $(uname -s) > note.txt", "cat note.txt"} {
		result, err := session.CallTool(ctx, bmaexecutor.ToolCallParams{Tool: bmaexecutor.ToolNameBash, Input: bmaexecutor.ToolInput{Command: ptr(command)}})
		check(err)
		fmt.Printf("bash %q -> %s\n", command, text(result))
	}
	mcp, err := session.MCP(ctx, "echo")
	check(err)
	for _, request := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"local-brain","version":"0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"MCP via the executor"}}}`,
	} {
		check(mcp.Write(ctx, []byte(request)))
		response, err := mcp.Read(ctx)
		check(err)
		fmt.Printf("mcp -> %s\n", response)
	}
	check(session.Stop(ctx, "script finished"))
}

func text(result bmaexecutor.ToolResultParams) string {
	var parts []string
	for _, block := range result.Content {
		if block.Text != nil {
			parts = append(parts, strings.TrimSpace(*block.Text))
		}
	}
	return strings.Join(parts, " | ")
}

func randomToken() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func mustGetwd() string {
	dir, err := os.Getwd()
	check(err)
	return dir
}

func ptr[T any](v T) *T { return &v }

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
