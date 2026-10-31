package bmaexecutor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type nopAgent struct{}

func (nopAgent) OpenTurn(context.Context, TurnStartParams) (TurnStartResult, error) {
	return TurnStartResult{Accepted: true}, nil
}
func (nopAgent) RunTurn(context.Context, *AgentTurn) error          { return nil }
func (nopAgent) CloseSession(context.Context, string, string) error { return nil }

// An agent executor with a machine-scoped token (TokenSource, no
// environment) negotiates bma.executor.v1.1 and registers without an
// environment_id, as the BMA API's machine executor channel requires.
func TestAgentClientRegistersV11WithMachineToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registered := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, raw, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var request struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(raw, &request)
		registered <- request.Params
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
			"protocol": ProtocolV11, "executor_id": "exec_m", "connection_id": "conn_m", "server_time": time.Now().UTC().Format(time.RFC3339),
			"heartbeat_interval_ms": 60000, "lease_ttl_ms": 120000, "max_control_frame_bytes": 1 << 20,
			"max_mcp_frame_bytes": 1 << 20, "drain": false,
		}})
		_ = ws.Write(ctx, websocket.MessageText, response)
		<-ctx.Done()
	}))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")

	client, err := NewClient(Options{
		TokenSource: StaticToken(wsURL, "machine_jwt"),
		Executor:    ExecutorInfo{Name: "desk", OS: "darwin", Arch: "arm64", Version: "0.1.0"},
	})
	if err != nil {
		t.Fatalf("NewClient without EnvironmentID: %v", err)
	}
	client.ServeAgent(nopAgent{}, AgentCapability{MaxSessions: 1})
	if _, err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = client.Stop(context.Background(), "test") }()
	params := <-registered
	if params["protocol"] != ProtocolV11 || params["environment_id"] != "" {
		t.Fatalf("register params = %v", params)
	}
	capabilities, _ := params["capabilities"].(map[string]any)
	if agent, _ := capabilities["agent"].(map[string]any); agent["enabled"] != true {
		t.Fatalf("capabilities = %v", capabilities)
	}
}
