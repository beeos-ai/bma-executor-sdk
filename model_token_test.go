package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRefreshModelToken(t *testing.T) {
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		for _, turn := range []string{"turn_ok", "turn_gone"} {
			req, err := s.awaitRequest(ctx, MethodModelTokenRefresh)
			if err != nil {
				return err
			}
			var p ModelTokenRefreshParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return err
			}
			if p.SessionID != "sess_test" || p.TurnID != turn {
				return errors.New("unexpected model_token.refresh params")
			}
			if turn == "turn_gone" {
				return s.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: *req.ID, Error: JSONRPCError{
					Code: -32000, Message: "turn ended", Data: &JSONRPCErrorData{Reason: "turn_inactive"},
				}})
			}
			if err := s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *req.ID, Result: ModelTokenRefreshResult{
				ModelToken: "tok_new", ModelBaseURL: "https://x/v1", ExpiresAt: "2026-11-09T09:00:00Z",
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

	got, err := client.RefreshModelToken(ctx, "sess_test", "turn_ok")
	if err != nil || got.ModelToken != "tok_new" || got.ExpiresAt != "2026-11-09T09:00:00Z" || got.ModelBaseURL != "https://x/v1" {
		t.Fatalf("RefreshModelToken = %+v, %v", got, err)
	}
	_, err = client.RefreshModelToken(ctx, "sess_test", "turn_gone")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Data == nil || rpcErr.Data.Reason != "turn_inactive" {
		t.Fatalf("want turn_inactive RPCError, got %v", err)
	}
}
