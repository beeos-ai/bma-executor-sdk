package bmaexecutor

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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

// ChannelTarget names the channel a Dialer opens: the control channel, or
// the MCP channel of one session.assign mcp_servers entry
// (?channel=mcp&session_id=&server_key= on WebSocket transports).
type ChannelTarget struct {
	Channel   string // ChannelControl or ChannelMCP
	SessionID string
	ServerKey string
}

// Dialer opens an executor channel after the token source returned token.
// The default dials token.WSURL over WebSocket: ws://, wss:// or
// unix:///path/to.sock (the same WebSocket upgrade on a Unix domain socket,
// as in the TypeScript and Python SDKs).
type Dialer func(ctx context.Context, token ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error)

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

// WebSocketDialer dials token.WSURL (ws://, wss:// or unix://) with the
// bearer token in the Authorization header. httpClient may carry a proxy or a
// custom transport; nil uses http.DefaultClient. A unix:// URL always dials
// its socket.
func WebSocketDialer(httpClient *http.Client) Dialer {
	return func(ctx context.Context, token ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error) {
		return dialWebSocket(ctx, httpClient, token.WSURL, token.Token, protocol, target)
	}
}

// channelURL adds the channel query of target to rawURL. A unix:// URL
// becomes ws://local/ on the HTTP client of its socket.
func channelURL(httpClient *http.Client, rawURL string, target ChannelTarget) (string, *http.Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("bmaexecutor: invalid channel URL: %w", err)
	}
	switch parsed.Scheme {
	case "ws", "wss":
	case "unix":
		httpClient = unixHTTPClient(parsed.Path)
		parsed = &url.URL{Scheme: "ws", Host: "local", Path: "/", RawQuery: parsed.RawQuery}
	default:
		return "", nil, errors.New("bmaexecutor: channel URL must use ws, wss or unix")
	}
	if target.Channel == ChannelMCP {
		query := parsed.Query()
		query.Set("channel", ChannelMCP)
		query.Set("session_id", target.SessionID)
		query.Set("server_key", target.ServerKey)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), httpClient, nil
}

func dialWebSocket(ctx context.Context, httpClient *http.Client, rawURL, token, protocol string, target ChannelTarget) (Conn, error) {
	url, httpClient, err := channelURL(httpClient, rawURL, target)
	if err != nil {
		return nil, err
	}
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
// path (the harness serves Harness.ServeHTTP on that listener). token.WSURL
// is ignored; StaticToken("unix://"+path, token) with the default dialer is
// equivalent.
func UnixSocketDialer(path string) Dialer {
	return func(ctx context.Context, token ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error) {
		return dialWebSocket(ctx, nil, "unix://"+path, token.Token, protocol, target)
	}
}

// ListenUnixSocket makes the executor the listening side on a local machine:
// it creates a 0700 directory and a 0600 Unix socket at path and returns a
// Dialer that waits for the next local harness ("brain") to connect with
// Authorization: Bearer token. The roles do not change: the brain is still
// the harness and the executor still registers. The brain (Harness.
// DialExecutor) also dials the MCP channels of its sessions; an MCP dial
// waits for that connection. Use it as Options.Dial with TokenSource
// StaticToken("ws://local/", "") so the executor reconnects by accepting
// again. Close the returned closer to stop listening.
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
	l := &unixListener{accepted: map[ChannelTarget]chan *websocket.Conn{}, done: make(chan struct{})}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(bearer), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		query := r.URL.Query()
		target := ChannelTarget{Channel: ChannelControl}
		if query.Get("channel") == ChannelMCP {
			target = ChannelTarget{Channel: ChannelMCP, SessionID: query.Get("session_id"), ServerKey: query.Get("server_key")}
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ProtocolV11, ProtocolV1}})
		if err != nil {
			return
		}
		select {
		case l.slot(target) <- ws:
			// The connection is hijacked: the executor owns it now.
		case <-l.done:
			_ = ws.Close(websocket.StatusGoingAway, "executor stopped")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	var once sync.Once
	closer := func() error {
		once.Do(func() { close(l.done) })
		return server.Close()
	}
	dial := func(ctx context.Context, _ ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error) {
		select {
		case ws := <-l.slot(target):
			if ws.Subprotocol() != protocol {
				_ = ws.Close(websocket.StatusPolicyViolation, protocol+" required")
				return nil, fmt.Errorf("bmaexecutor: local harness offered %q, need %s", ws.Subprotocol(), protocol)
			}
			return NewWebSocketConn(ws), nil
		case <-l.done:
			return nil, ErrClientStopped
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return dial, closer, nil
}

// unixListener hands accepted connections to dials of the same channel.
type unixListener struct {
	mu       sync.Mutex
	accepted map[ChannelTarget]chan *websocket.Conn
	done     chan struct{}
}

func (l *unixListener) slot(target ChannelTarget) chan *websocket.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	slot := l.accepted[target]
	if slot == nil {
		slot = make(chan *websocket.Conn)
		l.accepted[target] = slot
	}
	return slot
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
// creates a fresh Pipe and serves its harness end with principal, as the
// control channel or as an MCP channel.
func InProcessDialer(h *Harness, principal Principal) Dialer {
	return func(_ context.Context, _ ExecutorTokenResponse, protocol string, target ChannelTarget) (Conn, error) {
		if target.Channel == ChannelMCP {
			session, err := h.mcpSession(principal, target.SessionID)
			if err != nil {
				return nil, err
			}
			executor, harness := Pipe(protocol)
			if err := session.attachMCP(target.ServerKey, newHarnessMCPChannel(harness)); err != nil {
				return nil, err
			}
			return executor, nil
		}
		executor, harness := Pipe(protocol)
		go h.ServeConn(harness, principal)
		return executor, nil
	}
}
