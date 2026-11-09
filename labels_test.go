package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func registerPayload(t *testing.T, mutate func(*Options)) map[string]json.RawMessage {
	t.Helper()
	got := make(chan json.RawMessage, 1)
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		frame, err := s.awaitRequest(ctx, MethodExecutorRegister)
		if err != nil {
			return err
		}
		got <- frame.Params
		return s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *frame.ID, Result: ExecutorRegisterResult{Protocol: ProtocolV11,
			ExecutorID: "exec_test", ConnectionID: "conn_test", ServerTime: "2026-11-10T00:00:00Z", HeartbeatIntervalMS: 60000, LeaseTTLMS: 60000,
			MaxControlFrameBytes: 65536, MaxMCPFrameBytes: 1 << 20}})
	})
	client := newTestClient(t, ts.server.URL, mutate)
	startTestClient(t, client)
	select {
	case raw := <-got:
		var params map[string]json.RawMessage
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatal(err)
		}
		return params
	case <-time.After(5 * time.Second):
		t.Fatal("no executor.register")
		return nil
	}
}

func TestRegisterReportsLabelsAndRemoteBrainOnlyWhenTheOwnerConfiguredThem(t *testing.T) {
	off := registerPayload(t, nil)
	var capsOff map[string]json.RawMessage
	_ = json.Unmarshal(off["capabilities"], &capsOff)
	if _, ok := capsOff["remote_brain"]; ok {
		t.Fatalf("remote_brain is on by default: %s", off["capabilities"])
	}
	if strings.Contains(string(off["executor"]), "labels") {
		t.Fatalf("labels without configuration: %s", off["executor"])
	}

	on := registerPayload(t, func(o *Options) {
		o.RemoteBrain = true
		o.Labels = []string{"team-a", "gpu", "team-a"}
		o.Executor.Labels = []string{"mac.m4"}
	})
	var caps struct {
		RemoteBrain *RemoteBrainCapability `json:"remote_brain"`
	}
	var executor ExecutorInfo
	_ = json.Unmarshal(on["capabilities"], &caps)
	_ = json.Unmarshal(on["executor"], &executor)
	if caps.RemoteBrain == nil || !caps.RemoteBrain.Enabled {
		t.Fatalf("capabilities = %s", on["capabilities"])
	}
	if strings.Join(executor.Labels, ",") != "mac.m4,team-a,gpu" {
		t.Fatalf("labels = %v", executor.Labels)
	}
}

func TestLabelsAreCheckedAgainstTheContract(t *testing.T) {
	bad := [][]string{{""}, {"Upper"}, {"os:linux"}, {strings.Repeat("a", 65)}, {"has space"}}
	for _, labels := range bad {
		if _, err := NewClient(Options{EnvironmentKey: "k", Executor: ExecutorInfo{Name: "t"}, Labels: labels}); err == nil {
			t.Errorf("label %q accepted", labels)
		}
	}
	many := make([]string, 33)
	for i := range many {
		many[i] = "l" + strings.Repeat("x", i)
	}
	if _, err := NewClient(Options{EnvironmentKey: "k", Executor: ExecutorInfo{Name: "t"}, Labels: many}); err == nil {
		t.Error("33 labels accepted")
	}
	if _, err := NewClient(Options{EnvironmentKey: "k", Executor: ExecutorInfo{Name: "t"}, Labels: many[:32]}); err != nil {
		t.Errorf("32 labels: %v", err)
	}
}

func TestRefreshToolsToken(t *testing.T) {
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		for _, turn := range []string{"turn_ok", "turn_gone"} {
			req, err := s.awaitRequest(ctx, MethodToolsTokenRefresh)
			if err != nil {
				return err
			}
			var p ToolsTokenRefreshParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return err
			}
			if p.SessionID != "sess_test" || p.TurnID != turn {
				return errors.New("unexpected tools_token.refresh params")
			}
			if turn == "turn_gone" {
				return s.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: *req.ID, Error: JSONRPCError{
					Code: -32000, Message: "stale epoch", Data: &JSONRPCErrorData{Reason: "forbidden"},
				}})
			}
			if err := s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *req.ID, Result: ToolsTokenRefreshResult{
				ToolsMCPToken: "tok_new", ToolsMCPURL: "https://x/v1/machines/m/mcp", ExpiresAt: "2026-11-10T09:00:00Z",
			}}); err != nil {
				return err
			}
		}
		return nil
	})
	client := newTestClient(t, ts.server.URL, nil)
	startTestClient(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := client.RefreshToolsToken(ctx, "sess_test", "turn_ok")
	if err != nil || got.ToolsMCPToken != "tok_new" || got.ToolsMCPURL != "https://x/v1/machines/m/mcp" {
		t.Fatalf("RefreshToolsToken = %+v, %v", got, err)
	}
	_, err = client.RefreshToolsToken(ctx, "sess_test", "turn_gone")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Data == nil || rpcErr.Data.Reason != "forbidden" {
		t.Fatalf("want forbidden RPCError, got %v", err)
	}
}
