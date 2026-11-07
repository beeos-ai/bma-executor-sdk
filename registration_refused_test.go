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

const refusedReason = "runtime registration refused"

// refusalServer registers every control channel and then runs after(n, ws),
// n counting registrations from 1. A register answered by registerError (a
// JSON-RPC error code, 0 for a success) is not counted as registered.
type refusalServer struct {
	registrations atomic.Int32
	registerError int
	heartbeatMS   int64
	after         func(n int32, ws *websocket.Conn)
}

func startRefusalServer(t *testing.T, s *refusalServer) *httptest.Server {
	t.Helper()
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
			session := &testSession{ws: ws, log: newFrameLog()}
			go session.readLoop(ctx)
			n := s.registrations.Add(1)
			if s.registerError != 0 && n > 1 {
				frame, err := session.awaitRequest(ctx, MethodExecutorRegister)
				if err != nil {
					return
				}
				_ = session.write(rpcErrorEnvelope{JSONRPC: "2.0", ID: *frame.ID, Error: JSONRPCError{
					Code: s.registerError, Message: "capability not available",
				}})
				<-ctx.Done()
				return
			}
			if err := registerSession(ctx, session, s.heartbeatMS); err != nil {
				return
			}
			s.after(n, ws)
			<-ctx.Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func waitDone(t *testing.T, client *Client, within time.Duration) {
	t.Helper()
	select {
	case <-client.Done():
	case <-time.After(within):
		t.Fatalf("client did not end; state = %s", client.State())
	}
}

// The platform answers executor.register and then closes 4002 "runtime
// registration refused" when the executor advertises neither process nor
// agent. Reconnecting cannot fix that: after 3 refusals in a row the client
// ends with ErrRegistrationRefused instead of reconnecting forever.
func TestClientEndsAfterConsecutiveRegistrationRefusals(t *testing.T) {
	s := &refusalServer{heartbeatMS: 60000, after: func(_ int32, ws *websocket.Conn) {
		_ = ws.Close(4002, refusedReason)
	}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, fastReconnect)
	startTestClient(t, client)
	waitDone(t, client, 5*time.Second)
	err := client.Err()
	if !errors.Is(err, ErrRegistrationRefused) {
		t.Fatalf("Err = %v, want ErrRegistrationRefused", err)
	}
	if CloseCode(err) != 4002 {
		t.Fatalf("Err does not carry the 4002 close: %v", err)
	}
	if n := s.registrations.Load(); n != 3 {
		t.Fatalf("registrations = %d, want 3", n)
	}
}

// 4002 "daemon runtime ended" (a newer registration or a server restart
// ended the runtime) is legitimate and never counts as a configuration error.
func TestClientKeepsReconnectingAfterRuntimeEnded4002(t *testing.T) {
	s := &refusalServer{heartbeatMS: 60000, after: func(_ int32, ws *websocket.Conn) {
		_ = ws.Close(4002, "daemon runtime ended")
	}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, fastReconnect)
	startTestClient(t, client)
	deadline := time.Now().Add(5 * time.Second)
	for s.registrations.Load() < 8 {
		if time.Now().After(deadline) {
			t.Fatalf("registrations = %d, want >= 8", s.registrations.Load())
		}
		select {
		case <-client.Done():
			t.Fatalf("client ended: %v", client.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A refusal after the connection was stable (it outlived a heartbeat
// interval) is not a registration refusal: the streak is only for closes
// right after registering.
func TestClientDoesNotCountRefusalsOfAStableConnection(t *testing.T) {
	s := &refusalServer{heartbeatMS: 20, after: func(_ int32, ws *websocket.Conn) {
		time.Sleep(120 * time.Millisecond)
		_ = ws.Close(4002, refusedReason)
	}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, fastReconnect)
	startTestClient(t, client)
	deadline := time.Now().Add(5 * time.Second)
	for s.registrations.Load() < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("registrations = %d, want >= 6", s.registrations.Load())
		}
		select {
		case <-client.Done():
			t.Fatalf("client ended: %v", client.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A server that answers executor.register with a JSON-RPC error for the
// executor's capabilities (the platform may validate before it responds) is
// the same configuration error: capability_unavailable and invalid_params
// count like the 4002 refusal.
func TestClientEndsAfterRegisterRPCErrors(t *testing.T) {
	for _, code := range []int{CodeCapabilityUnavailable, CodeInvalidParams} {
		s := &refusalServer{registerError: code, heartbeatMS: 60000, after: func(_ int32, ws *websocket.Conn) {
			_ = ws.Close(1012, "node replaced") // the first connection registers, then is lost
		}}
		server := startRefusalServer(t, s)
		client := newTestClient(t, server.URL, fastReconnect)
		startTestClient(t, client)
		waitDone(t, client, 5*time.Second)
		err := client.Err()
		var rpcErr *RPCError
		if !errors.Is(err, ErrRegistrationRefused) || !errors.As(err, &rpcErr) || rpcErr.Code != code {
			t.Fatalf("code %d: Err = %v, want ErrRegistrationRefused wrapping the RPC error", code, err)
		}
	}
}

// WaitReady returns after the registration survived one heartbeat interval,
// not when executor.register returned: the platform may still refuse the
// runtime right after answering it.
func TestWaitReadyNeedsAStableRegistration(t *testing.T) {
	s := &refusalServer{heartbeatMS: 300, after: func(int32, *websocket.Conn) {}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, nil)
	started := time.Now()
	startTestClient(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("WaitReady returned after %s, before one heartbeat interval (300ms)", elapsed)
	}
	if client.State() != ConnectionStateReady {
		t.Fatalf("state = %s", client.State())
	}
}

// A connection refused right after registering is never reported ready; the
// wait ends with the configuration error once the client gives up.
func TestWaitReadyNeverSucceedsForRefusedRegistrations(t *testing.T) {
	s := &refusalServer{heartbeatMS: 60000, after: func(_ int32, ws *websocket.Conn) {
		_ = ws.Close(4002, refusedReason)
	}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, fastReconnect)
	startTestClient(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx); !errors.Is(err, ErrRegistrationRefused) {
		t.Fatalf("WaitReady = %v, want ErrRegistrationRefused", err)
	}
}

// WaitReady honours its context while the connection is not yet stable.
func TestWaitReadyHonoursContext(t *testing.T) {
	s := &refusalServer{heartbeatMS: 60000, after: func(int32, *websocket.Conn) {}}
	server := startRefusalServer(t, s)
	client := newTestClient(t, server.URL, nil)
	startTestClient(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady = %v, want context.DeadlineExceeded", err)
	}
}
