package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// outageServer answers the first token request, then failStatus for every
// later one until healthy is set. Each control channel registers and, while
// the outage lasts, closes with 1012 (service restart).
type outageServer struct {
	tokens     atomic.Int32
	registered chan struct{}
	healthy    atomic.Bool
	failStatus int
	failFor    int32 // token requests after the first that fail; <0 forever
}

func startOutageServer(t *testing.T, failStatus int, failFor int32) (*outageServer, *httptest.Server) {
	t.Helper()
	o := &outageServer{registered: make(chan struct{}, 8), failStatus: failStatus, failFor: failFor}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			n := o.tokens.Add(1)
			if n > 1 && (o.failFor < 0 || n-1 <= o.failFor) {
				http.Error(w, "upstream unavailable", o.failStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID: "exec_test", Token: "jwt_test", ExpiresIn: 300,
				WSURL: "ws://" + r.Host + "/v1/executors/channel",
			})
		case "/v1/executors/channel":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s := &testSession{ws: ws, log: newFrameLog()}
			go s.readLoop(ctx)
			if err := registerSession(ctx, s, 60000); err != nil {
				return
			}
			o.registered <- struct{}{}
			if !o.healthy.Load() {
				_ = ws.Close(websocket.StatusServiceRestart, "node replaced")
				return
			}
			<-ctx.Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return o, server
}

func fastReconnect(o *Options) {
	o.ReconnectBaseDelay = time.Millisecond
	o.ReconnectMaxDelay = 2 * time.Millisecond
}

// A long outage (more failed token requests than any small fixed budget)
// does not end the client: by default it reconnects until Stop, as gRPC's
// connection backoff and long-running agents do.
func TestClientReconnectsAfterALongOutageByDefault(t *testing.T) {
	o, server := startOutageServer(t, http.StatusServiceUnavailable, 25)
	client := newTestClient(t, server.URL, fastReconnect)
	startTestClient(t, client)
	<-o.registered
	o.healthy.Store(true)
	select {
	case <-o.registered:
	case <-client.Done():
		t.Fatalf("client ended during the outage: %v", client.Err())
	case <-time.After(10 * time.Second):
		t.Fatal("client did not register again after the outage")
	}
	if n := o.tokens.Load(); n < 27 {
		t.Fatalf("token requests = %d, want the 25 failures retried", n)
	}
	// The harness answered executor.register; the client reads it next.
	deadline := time.Now().Add(2 * time.Second)
	for client.State() != ConnectionStateReady {
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want ready", client.State())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A rejected credential (401/403 from the token endpoint, a revoked machine
// token) is not retried: the client ends with ErrCredentialRejected.
func TestClientEndsWhenTheCredentialIsRejectedOnReconnect(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		o, server := startOutageServer(t, status, -1)
		client := newTestClient(t, server.URL, fastReconnect)
		startTestClient(t, client)
		<-o.registered
		select {
		case <-client.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("%d: client kept reconnecting with a rejected credential", status)
		}
		if err := client.Err(); !errors.Is(err, ErrCredentialRejected) {
			t.Fatalf("%d: Err = %v, want ErrCredentialRejected", status, err)
		}
		if n := o.tokens.Load(); n != 2 {
			t.Fatalf("%d: token requests = %d, want 2 (no retry after the rejection)", status, n)
		}
	}
}

// A heartbeat left unanswered past RequestTimeout means the connection is
// dead (a half-open TCP connection after a network cut): the client closes it
// and reconnects, as gRPC keepalive closes a transport whose ping times out.
func TestClientReconnectsWhenAHeartbeatTimesOut(t *testing.T) {
	registered := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID: "exec_test", Token: "jwt_test", ExpiresIn: 300,
				WSURL: "ws://" + r.Host + "/v1/executors/channel",
			})
		case "/v1/executors/channel":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s := &testSession{ws: ws, log: newFrameLog()}
			go s.readLoop(ctx)
			if err := registerSession(ctx, s, 20); err != nil {
				return
			}
			registered <- struct{}{}
			<-ctx.Done() // never answers executor.heartbeat
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, func(o *Options) {
		fastReconnect(o)
		o.RequestTimeout = 100 * time.Millisecond
	})
	startTestClient(t, client)
	<-registered
	select {
	case <-registered:
	case <-client.Done():
		t.Fatalf("client ended: %v", client.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("client kept a connection whose heartbeats time out")
	}
}

// A reconnect token request that never answers (a dead pooled connection
// after a network cut; http.DefaultClient has no timeout) is abandoned after
// RequestTimeout and retried instead of hanging the reconnect for good.
func TestClientRetriesAReconnectTokenRequestThatHangs(t *testing.T) {
	var tokens atomic.Int32
	registered := make(chan struct{}, 8)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			if tokens.Add(1) == 2 {
				select { // the second request never answers
				case <-release:
				case <-r.Context().Done():
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID: "exec_test", Token: "jwt_test", ExpiresIn: 300,
				WSURL: "ws://" + r.Host + "/v1/executors/channel",
			})
		case "/v1/executors/channel":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s := &testSession{ws: ws, log: newFrameLog()}
			go s.readLoop(ctx)
			if err := registerSession(ctx, s, 60000); err != nil {
				return
			}
			registered <- struct{}{}
			if tokens.Load() == 1 {
				_ = ws.Close(websocket.StatusServiceRestart, "node replaced")
				return
			}
			<-ctx.Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) }) // before server.Close (cleanups run last-in first-out)
	client := newTestClient(t, server.URL, func(o *Options) {
		fastReconnect(o)
		o.RequestTimeout = 200 * time.Millisecond
	})
	startTestClient(t, client)
	<-registered
	select {
	case <-registered:
	case <-client.Done():
		t.Fatalf("client ended: %v", client.Err())
	case <-time.After(5 * time.Second):
		t.Fatalf("reconnect hung on a token request that never answers (token requests = %d)", tokens.Load())
	}
}

// countingTransport counts CloseIdleConnections calls.
type countingTransport struct {
	http.RoundTripper
	closed atomic.Int32
}

func (t *countingTransport) CloseIdleConnections() { t.closed.Add(1) }

// Each reconnect attempt drops the HTTP client's idle connections: after a
// network cut the pooled (HTTP/2) connection is dead, and reusing it made
// every later attempt time out although the network was back.
func TestClientDropsIdleConnectionsOnReconnect(t *testing.T) {
	o, server := startOutageServer(t, http.StatusServiceUnavailable, 1)
	transport := &countingTransport{RoundTripper: http.DefaultTransport}
	client := newTestClient(t, server.URL, func(opts *Options) {
		fastReconnect(opts)
		opts.HTTPClient = &http.Client{Transport: transport}
	})
	startTestClient(t, client)
	<-o.registered
	o.healthy.Store(true)
	select {
	case <-o.registered:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not register again")
	}
	if n := transport.closed.Load(); n != 2 {
		t.Fatalf("CloseIdleConnections calls = %d, want 2 (one per reconnect attempt)", n)
	}
}

// A positive MaxReconnectAttempts still bounds the retries; the client then
// ends with ErrReconnectExhausted carrying the last failure.
func TestClientEndsAfterMaxReconnectAttempts(t *testing.T) {
	o, server := startOutageServer(t, http.StatusServiceUnavailable, -1)
	client := newTestClient(t, server.URL, func(opts *Options) {
		fastReconnect(opts)
		opts.MaxReconnectAttempts = 3
	})
	startTestClient(t, client)
	<-o.registered
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not end after 3 failed reconnects")
	}
	err := client.Err()
	if !errors.Is(err, ErrReconnectExhausted) {
		t.Fatalf("Err = %v, want ErrReconnectExhausted", err)
	}
	if n := o.tokens.Load(); n != 4 {
		t.Fatalf("token requests = %d, want 1 + 3 reconnect attempts", n)
	}
}
