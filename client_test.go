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

func TestClientRegistersAndRunsToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wsURL string
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer env_key" {
				http.Error(w, "authorization", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID:   "exec_test",
				Token:        "jwt_test",
				ExpiresIn:    300,
				WSURL:        wsURL,
				ChannelScope: []string{ChannelControl},
			})
		case "/v1/executors/channel":
			if got := r.Header.Get("Authorization"); got != "Bearer jwt_test" {
				serverDone <- errString("bad websocket authorization: " + got)
				http.Error(w, "authorization", http.StatusUnauthorized)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV1}})
			if err != nil {
				serverDone <- err
				return
			}
			ws.SetReadLimit(maxFrameBytes)
			defer ws.CloseNow()
			serverDone <- runExecutorServer(ctx, ws)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	wsURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/executors/channel"

	client, err := NewClient(Options{
		EnvironmentKey: "env_key",
		EnvironmentID:  "env_test",
		Executor:       ExecutorInfo{Name: "test", OS: "linux", Arch: "arm64", Version: "0.1.0"},
		Capabilities:   Capabilities{Shell: &ShellCapability{Enabled: true}},
		BaseURL:        server.URL,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.OnToolCall(func(_ context.Context, params ToolCallParams) (ToolResultParams, error) {
		return ToolResultParams{
			SessionID:  params.SessionID,
			ToolCallID: params.ToolCallID,
			Outcome:    ToolOutcomeSucceeded,
			Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: "ok"}},
		}, nil
	})

	registered, err := client.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if registered.ExecutorID != "exec_test" {
		t.Fatalf("unexpected executor id %q", registered.ExecutorID)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("server did not finish: %v", ctx.Err())
	}

	ack, err := client.ReportToolResult(ctx, ToolResultParams{
		SessionID:  "sess_test",
		ToolCallID: "toolu_test",
		Outcome:    ToolOutcomeSucceeded,
		Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: "ok"}},
	})
	if err != nil {
		t.Fatalf("duplicate ReportToolResult: %v", err)
	}
	if !ack.Accepted {
		t.Fatal("duplicate ReportToolResult was not accepted")
	}
	if err := client.Stop(ctx, "test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func runExecutorServer(ctx context.Context, ws *websocket.Conn) error {
	read := func() (rpcInbound, error) {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return rpcInbound{}, err
		}
		var message rpcInbound
		if err := json.Unmarshal(data, &message); err != nil {
			return rpcInbound{}, err
		}
		return message, nil
	}
	write := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return ws.Write(ctx, websocket.MessageText, data)
	}

	register, err := read()
	if err != nil {
		return err
	}
	if register.Method != MethodExecutorRegister || register.ID == nil {
		return errString("expected executor.register request")
	}
	var registerParams ExecutorRegisterParams
	if err := json.Unmarshal(register.Params, &registerParams); err != nil {
		return err
	}
	if registerParams.Protocol != ProtocolV1 || registerParams.EnvironmentID != "env_test" {
		return errString("unexpected register params")
	}
	if err := write(rpcSuccessEnvelope{
		JSONRPC: "2.0",
		ID:      *register.ID,
		Result: ExecutorRegisterResult{
			ExecutorID:           "exec_test",
			ConnectionID:         "conn_test",
			ServerTime:           "2026-10-30T12:00:00Z",
			HeartbeatIntervalMS:  5000,
			LeaseTTLMS:           60000,
			MaxControlFrameBytes: 65536,
			MaxMCPFrameBytes:     1048576,
		},
	}); err != nil {
		return err
	}

	toolCallParams, err := json.Marshal(ToolCallParams{
		SessionID:  "sess_test",
		ToolCallID: "toolu_test",
		Tool:       ToolNameBash,
		Input:      mustJSON(ToolInput{Command: "true"}),
	})
	if err != nil {
		return err
	}
	if err := write(map[string]any{
		"jsonrpc": "2.0",
		"id":      "srv_tool_1",
		"method":  MethodToolCall,
		"params":  json.RawMessage(toolCallParams),
	}); err != nil {
		return err
	}

	ack, err := read()
	if err != nil {
		return err
	}
	if ack.ID == nil || *ack.ID != "srv_tool_1" {
		return errString("expected tool.call acknowledgement")
	}
	var ackResult Ack
	if err := json.Unmarshal(ack.Result, &ackResult); err != nil || !ackResult.Accepted {
		return errString("tool.call was not acknowledged")
	}

	result, err := read()
	if err != nil {
		return err
	}
	if result.Method != MethodToolResult || result.ID == nil {
		return errString("expected tool.result request")
	}
	var resultParams ToolResultParams
	if err := json.Unmarshal(result.Params, &resultParams); err != nil {
		return err
	}
	if resultParams.ToolCallID != "toolu_test" || resultParams.Outcome != ToolOutcomeSucceeded {
		return errString("unexpected tool.result params")
	}
	if err := write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *result.ID, Result: Ack{Accepted: true}}); err != nil {
		return err
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
