package bmaexecutor

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/coder/websocket"
)

// Conn is one executor control connection: ordered JSON-RPC text frames in
// both directions. The protocol does not depend on the transport; the SDK
// ships WebSocket (cloud, executor dials out), a local socket (Unix domain
// socket carrying the same WebSocket upgrade) and an in-process pipe.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, frame []byte) error
	// Close ends the connection with a WebSocket-style close code; the peer
	// reads it as a *CloseError.
	Close(code int, reason string) error
	// Subprotocol is the negotiated executor protocol.
	Subprotocol() string
}

// CloseError is the error Read returns after the peer closed with a code.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("bmaexecutor: connection closed: %d %s", e.Code, e.Reason)
}

// CloseCode returns the close code carried by err, or -1.
func CloseCode(err error) int {
	var closeErr *CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code
	}
	return -1
}

// Dialer opens the executor's control connection after the token source
// returned token. The default dials token.WSURL over WebSocket.
type Dialer func(ctx context.Context, token ExecutorTokenResponse, protocol string) (Conn, error)

type wsConn struct{ ws *websocket.Conn }

// NewWebSocketConn wraps an accepted or dialed WebSocket as a Conn.
func NewWebSocketConn(ws *websocket.Conn) Conn {
	ws.SetReadLimit(maxFrameBytes)
	return wsConn{ws: ws}
}

func (c wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		if code := websocket.CloseStatus(err); code != -1 {
			return nil, &CloseError{Code: int(code), Reason: err.Error()}
		}
		return nil, err
	}
	return data, nil
}

func (c wsConn) Write(ctx context.Context, frame []byte) error {
	return c.ws.Write(ctx, websocket.MessageText, frame)
}

func (c wsConn) Close(code int, reason string) error {
	return c.ws.Close(websocket.StatusCode(code), reason)
}

func (c wsConn) Subprotocol() string { return c.ws.Subprotocol() }

// WebSocketDialer dials token.WSURL with the bearer token in the
// Authorization header. httpClient may carry a proxy or a custom transport;
// nil uses http.DefaultClient.
func WebSocketDialer(httpClient *http.Client) Dialer {
	return func(ctx context.Context, token ExecutorTokenResponse, protocol string) (Conn, error) {
		return dialWebSocket(ctx, httpClient, token.WSURL, token.Token, protocol)
	}
}

func dialWebSocket(ctx context.Context, httpClient *http.Client, url, token, protocol string) (Conn, error) {
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:   httpClient,
		HTTPHeader:   authHeader(token),
		Subprotocols: []string{protocol},
	})
	if err != nil {
		return nil, fmt.Errorf("bmaexecutor: websocket dial: %w", err)
	}
	if ws.Subprotocol() != protocol {
		_ = ws.Close(websocket.StatusPolicyViolation, protocol+" required")
		return nil, fmt.Errorf("bmaexecutor: harness selected subprotocol %q, need %s", ws.Subprotocol(), protocol)
	}
	return NewWebSocketConn(ws), nil
}

// unixHTTPClient sends every request to the Unix socket at path.
func unixHTTPClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", path)
		},
	}}
}

// UnixSocketDialer dials a harness listening on the Unix domain socket at
// path (the harness serves Harness.ServeHTTP on that listener). The host in
// token.WSURL is ignored; use for example StaticToken("ws://local/", token).
func UnixSocketDialer(path string) Dialer {
	return WebSocketDialer(unixHTTPClient(path))
}

// ListenUnixSocket makes the executor the listening side on a local machine:
// it creates a 0700 directory and a 0600 Unix socket at path and returns a
// Dialer that waits for the next local harness ("brain") to connect with
// Authorization: Bearer token. The roles do not change: the brain is still
// the harness and the executor still registers. Use it as Options.Dial with
// TokenSource StaticToken("ws://local/", "") so the executor reconnects by
// accepting again. Close the returned closer to stop listening.
func ListenUnixSocket(path, token string) (Dialer, func() error, error) {
	if token == "" {
		return nil, nil, errors.New("bmaexecutor: a local socket needs a token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		return nil, nil, err
	}
	accepted := make(chan *websocket.Conn)
	done := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(bearer), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11, ProtocolV1}})
		if err != nil {
			return
		}
		select {
		case accepted <- ws:
			// The connection is hijacked: the executor owns it now.
		case <-done:
			_ = ws.Close(websocket.StatusGoingAway, "executor stopped")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	var once sync.Once
	closer := func() error {
		once.Do(func() { close(done) })
		return server.Close()
	}
	dial := func(ctx context.Context, _ ExecutorTokenResponse, protocol string) (Conn, error) {
		select {
		case ws := <-accepted:
			if ws.Subprotocol() != protocol {
				_ = ws.Close(websocket.StatusPolicyViolation, protocol+" required")
				return nil, fmt.Errorf("bmaexecutor: local harness offered %q, need %s", ws.Subprotocol(), protocol)
			}
			return NewWebSocketConn(ws), nil
		case <-done:
			return nil, ErrClientStopped
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return dial, closer, nil
}

// pipeConn is one end of an in-process connection.
type pipeConn struct {
	protocol string
	in       <-chan []byte
	out      chan<- []byte
	// local is closed by this end's Close; remote is the peer's.
	local, remote *pipeEnd
}

type pipeEnd struct {
	once   sync.Once
	closed chan struct{}
	err    *CloseError
}

// Pipe returns the two ends of an in-process connection that negotiated
// protocol: one for the executor (Client), one for the harness
// (Harness.ServeConn). Frames are copied, so neither side can mutate the
// other's buffers.
func Pipe(protocol string) (executor Conn, harness Conn) {
	toHarness, toExecutor := make(chan []byte, 64), make(chan []byte, 64)
	a, b := &pipeEnd{closed: make(chan struct{})}, &pipeEnd{closed: make(chan struct{})}
	return &pipeConn{protocol: protocol, in: toExecutor, out: toHarness, local: a, remote: b},
		&pipeConn{protocol: protocol, in: toHarness, out: toExecutor, local: b, remote: a}
}

func (p *pipeConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-p.in:
		return frame, nil
	case <-p.remote.closed:
		return nil, p.remote.err
	case <-p.local.closed:
		return nil, p.local.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *pipeConn) Write(ctx context.Context, frame []byte) error {
	select {
	case <-p.local.closed:
		return p.local.err
	case <-p.remote.closed:
		return p.remote.err
	default:
	}
	select {
	case p.out <- append([]byte(nil), frame...):
		return nil
	case <-p.remote.closed:
		return p.remote.err
	case <-p.local.closed:
		return p.local.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *pipeConn) Close(code int, reason string) error {
	p.local.once.Do(func() {
		p.local.err = &CloseError{Code: code, Reason: reason}
		close(p.local.closed)
	})
	return nil
}

func (p *pipeConn) Subprotocol() string { return p.protocol }

// InProcessDialer connects the executor to h in the same process: every dial
// creates a fresh Pipe and serves its harness end with principal.
func InProcessDialer(h *Harness, principal Principal) Dialer {
	return func(_ context.Context, _ ExecutorTokenResponse, protocol string) (Conn, error) {
		executor, harness := Pipe(protocol)
		go h.ServeConn(harness, principal)
		return executor, nil
	}
}
