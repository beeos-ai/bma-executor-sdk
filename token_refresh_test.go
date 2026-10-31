package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// refreshHarness serves one control channel per dial and hands each session
// to serve. Tokens come from a TokenSource that numbers them.
func refreshHarness(t *testing.T, serve func(ctx context.Context, s *testSession) error) (*Client, *atomic.Int32, chan error) {
	t.Helper()
	dials := &atomic.Int32{}
	done := make(chan error, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
		if err != nil {
			done <- err
			return
		}
		defer ws.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s := &testSession{ws: ws, log: newFrameLog()}
		go s.readLoop(ctx)
		done <- serve(ctx, s)
	}))
	t.Cleanup(server.Close)
	var minted atomic.Int32
	client := newTestClient(t, server.URL, func(o *Options) {
		o.TokenSource = func(context.Context) (ExecutorTokenResponse, error) {
			n := minted.Add(1)
			return ExecutorTokenResponse{
				ExecutorID: "exec_test", Token: fmt.Sprintf("tok-%d", n), ExpiresIn: 1,
				WSURL: "ws" + server.URL[len("http"):],
			}, nil
		}
	})
	return client, dials, done
}

// The client refreshes its token on the open connection at about 70% of the
// token lifetime instead of waiting for the 4001 close.
func TestClientRefreshesTokenInsideTheConnection(t *testing.T) {
	client, dials, done := refreshHarness(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		start := time.Now()
		for i := 2; i <= 4; i++ {
			frame, err := s.awaitRequest(ctx, MethodExecutorTokenRefresh)
			if err != nil {
				return fmt.Errorf("refresh %d: %w", i, err)
			}
			var params ExecutorTokenRefreshParams
			if err := json.Unmarshal(frame.Params, &params); err != nil {
				return err
			}
			if want := fmt.Sprintf("tok-%d", i); params.Token != want {
				return fmt.Errorf("refresh token = %q, want %q", params.Token, want)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				return fmt.Errorf("refresh %d came %s after the previous token, after it expired", i, elapsed)
			}
			start = time.Now()
			if err := s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *frame.ID,
				Result: ExecutorTokenRefreshResult{ExpiresAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339)}}); err != nil {
				return err
			}
		}
		return nil
	})
	startTestClient(t, client)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("harness did not see three refreshes")
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("control channel dialed %d times, want 1", n)
	}
}

// A harness without executor.token.refresh answers -32601: the client stops
// refreshing on that connection and keeps the 4001 reconnect behaviour.
func TestClientStopsRefreshingWhenTheHarnessLacksTheMethod(t *testing.T) {
	client, _, done := refreshHarness(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		frame, err := s.awaitRequest(ctx, MethodExecutorTokenRefresh)
		if err != nil {
			return err
		}
		if err := s.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: *frame.ID,
			Error: JSONRPCError{Code: -32601, Message: "unknown method executor.token.refresh"}}); err != nil {
			return err
		}
		wait, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := s.awaitRequest(wait, MethodExecutorTokenRefresh); err == nil {
			return fmt.Errorf("client refreshed again after -32601")
		}
		return nil
	})
	startTestClient(t, client)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("harness did not finish")
	}
}

func TestReconnectDelayHonoursTheCap(t *testing.T) {
	client := newTestClient(t, "http://127.0.0.1:1", func(o *Options) {
		o.ReconnectBaseDelay = time.Second
		o.ReconnectMaxDelay = 5 * time.Second
		o.Random = func() float64 { return 1 }
	})
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second} {
		if got := client.reconnectDelay(attempt); got != want {
			t.Fatalf("reconnectDelay(%d) = %s, want %s", attempt, got, want)
		}
	}
	defaults := newTestClient(t, "http://127.0.0.1:1", func(o *Options) { o.Random = func() float64 { return 1 } })
	if got := defaults.reconnectDelay(20); got != 30*time.Second {
		t.Fatalf("default cap = %s, want 30s", got)
	}
}
