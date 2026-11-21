package bmaexecutor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// suspendServer registers every control channel and then neither answers nor
// closes it, as the platform end of a connection looks to a host that was
// suspended (a paused sandbox, a sleeping laptop): nothing tells the executor
// that the connection is dead. With closeFirst the first channel closes right
// after registering instead.
type suspendServer struct {
	closeFirst atomic.Bool
	registered chan time.Time
}

func startSuspendServer(t *testing.T) (*suspendServer, *httptest.Server) {
	t.Helper()
	s := &suspendServer{registered: make(chan time.Time, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/environments/env_test/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID: "exec_test", Token: "jwt_test", ExpiresIn: 300,
				WSURL: "ws://" + r.Host + "/v1/environments/env_test/channel",
			})
		case "/v1/environments/env_test/channel":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			session := &testSession{ws: ws, log: newFrameLog()}
			go session.readLoop(ctx)
			if err := registerSession(ctx, session, 60000); err != nil {
				return
			}
			s.registered <- time.Now()
			if s.closeFirst.CompareAndSwap(true, false) {
				_ = ws.Close(websocket.StatusServiceRestart, "node replaced")
				return
			}
			<-ctx.Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return s, server
}

// skewableWallClock lets a test move the wall clock ahead of the monotonic
// one, which is what a host sees after a suspend.
func skewableWallClock(client *Client) *atomic.Int64 {
	var skew atomic.Int64
	client.wallNow = func() time.Time { return time.Now().Round(0).Add(time.Duration(skew.Load())) }
	return &skew
}

// bmadBackoff is the reconnect backoff machine executors run with (bmad: 1s
// doubling to 5s); the heartbeat interval (60s here) and RequestTimeout (30s,
// the default) are what would otherwise bound noticing the dead connection.
func bmadBackoff(o *Options) {
	o.ReconnectBaseDelay = time.Second
	o.ReconnectMaxDelay = 5 * time.Second
	o.RequestTimeout = DefaultRequestTimeout
}

// After a suspend the client drops its connection and registers again at once,
// without waiting for a heartbeat to time out or for a reconnect backoff.
func TestClientReconnectsAtOnceWhenTheHostResumes(t *testing.T) {
	s, server := startSuspendServer(t)
	client := newTestClient(t, server.URL, bmadBackoff)
	skew := skewableWallClock(client)
	startTestClient(t, client)
	<-s.registered

	// Nothing happens without a suspend: the dead connection stays.
	select {
	case <-s.registered:
		t.Fatal("client reconnected without a suspend")
	case <-time.After(2 * suspendCheckInterval):
	}

	resumed := time.Now()
	skew.Store(int64(10 * time.Minute))
	select {
	case at := <-s.registered:
		if took := at.Sub(resumed); took > suspendCheckInterval+time.Second {
			t.Fatalf("registered again %s after the resume, want within one check and a connect", took)
		}
	case <-client.Done():
		t.Fatalf("client ended: %v", client.Err())
	case <-time.After(10 * time.Second):
		t.Fatal("client kept the connection it had before the suspend")
	}
}

// A client waiting out a reconnect backoff when the host suspends tries again
// as soon as it resumes instead of sleeping out the rest of the backoff.
func TestClientRetriesAtOnceWhenTheHostResumesDuringABackoff(t *testing.T) {
	s, server := startSuspendServer(t)
	s.closeFirst.Store(true)
	client := newTestClient(t, server.URL, func(o *Options) {
		o.ReconnectBaseDelay = 20 * time.Second // the first retry waits 10–20s
		o.ReconnectMaxDelay = 20 * time.Second
	})
	skew := skewableWallClock(client)
	startTestClient(t, client)
	<-s.registered
	deadline := time.Now().Add(2 * time.Second)
	for client.State() != ConnectionStateDisconnected {
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want disconnected after the close", client.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(suspendCheckInterval) // the client has read the clocks once

	resumed := time.Now()
	skew.Store(int64(time.Hour))
	select {
	case at := <-s.registered:
		if took := at.Sub(resumed); took > suspendCheckInterval+time.Second {
			t.Fatalf("registered again %s after the resume, want within one check and a connect", took)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("client slept out its backoff after the resume")
	}
}
