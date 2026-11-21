package bmaexecutor

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// HarnessMCPChannel is the harness end of one MCP channel, isolated by
// (session_id, server_key), on any transport. Frames are raw MCP JSON-RPC
// exactly as the MCP server sent them: the harness is the MCP client.
type HarnessMCPChannel struct {
	conn      Conn
	frames    chan []byte
	closeOnce sync.Once
	closed    chan struct{}
	err       error
	detach    func()
}

func newHarnessMCPChannel(conn Conn) *HarnessMCPChannel {
	return &HarnessMCPChannel{conn: conn, frames: make(chan []byte), closed: make(chan struct{})}
}

// pump reads the connection in the background so that a channel the
// executor closed is detached (and, for a dialed executor, redialed) even
// while nobody reads it.
func (m *HarnessMCPChannel) pump() {
	for {
		frame, err := m.conn.Read(context.Background())
		if err != nil {
			m.err = err
			_ = m.Close()
			return
		}
		select {
		case m.frames <- frame:
		case <-m.closed:
			return
		}
	}
}

// Read returns the next raw MCP frame. A closed channel returns an error;
// the executor reopens a fresh one (and a fresh MCP server process), so call
// Session.MCP again and re-initialize.
func (m *HarnessMCPChannel) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-m.frames:
		return frame, nil
	case <-m.closed:
		if m.err != nil {
			return nil, m.err
		}
		return nil, errors.New("bmaexecutor: mcp channel closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *HarnessMCPChannel) Write(ctx context.Context, frame []byte) error {
	return m.conn.Write(ctx, frame)
}

func (m *HarnessMCPChannel) Close() error {
	m.closeOnce.Do(func() {
		_ = m.conn.Close(int(websocket.StatusNormalClosure), "mcp channel closed")
		close(m.closed)
		if m.detach != nil {
			m.detach()
		}
	})
	return nil
}

// MCP waits for the MCP channel for serverKey (one of the session.assign
// mcp_servers) and returns it. A newer channel for the same key replaces the
// old one; the session's end closes all of them.
func (s *Session) MCP(ctx context.Context, serverKey string) (*HarnessMCPChannel, error) {
	for {
		s.mu.Lock()
		channel := s.mcp[serverKey]
		ended := s.endReason != ""
		changed := s.mcpChanged
		s.mu.Unlock()
		switch {
		case ended:
			return nil, ErrSessionEnded
		case channel != nil:
			return channel, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// mcpSession returns the active session that principal's executor may serve
// MCP servers for.
func (h *Harness) mcpSession(principal Principal, sessionID string) (*Session, error) {
	h.mu.Lock()
	session := h.sessions[sessionID]
	h.mu.Unlock()
	if session == nil || (principal.ExecutorID != "" && principal.ExecutorID != session.executor.id) {
		return nil, errors.New("bmaexecutor: session is not active for this executor")
	}
	return session, nil
}

func (h *Harness) serveMCP(w http.ResponseWriter, r *http.Request, principal Principal) {
	query := r.URL.Query()
	sessionID, serverKey := query.Get("session_id"), query.Get("server_key")
	if sessionID == "" || serverKey == "" {
		http.Error(w, "session_id and server_key are required", http.StatusBadRequest)
		return
	}
	// Only the executor holding the session may serve its MCP servers.
	session, err := h.mcpSession(principal, sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	ws, ok := h.accept(w, r)
	if !ok {
		return
	}
	channel := newHarnessMCPChannel(NewWebSocketConn(ws))
	if err := session.attachMCP(serverKey, channel); err != nil {
		_ = ws.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	// The handler owns the connection until the channel is closed.
	select {
	case <-channel.closed:
	case <-r.Context().Done():
		_ = channel.Close()
	}
}

// dialMCP keeps the MCP channel of serverKey open for a session whose
// executor this harness dialed (Harness.DialExecutor): the harness dials
// it, and again whenever the executor closes it to restart its server,
// until the session ends.
func (s *Session) dialMCP(path, token, serverKey string) {
	target := ChannelTarget{Channel: ChannelMCP, SessionID: s.id, ServerKey: serverKey}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.done
		cancel()
	}()
	for {
		if conn, err := dialWebSocket(ctx, nil, localURL(path), token, ProtocolV11, target); err == nil {
			channel := newHarnessMCPChannel(conn)
			if s.attachMCP(serverKey, channel) == nil {
				select {
				case <-channel.closed:
				case <-s.done:
				}
			} else {
				_ = conn.Close(int(websocket.StatusNormalClosure), "session ended")
			}
		}
		select {
		case <-s.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Session) attachMCP(serverKey string, channel *HarnessMCPChannel) error {
	s.mu.Lock()
	if s.endReason != "" {
		s.mu.Unlock()
		return errors.New("session ended")
	}
	previous := s.mcp[serverKey]
	s.mcp[serverKey] = channel
	channel.detach = func() {
		s.mu.Lock()
		if s.mcp[serverKey] == channel {
			delete(s.mcp, serverKey)
		}
		s.mu.Unlock()
	}
	close(s.mcpChanged)
	s.mcpChanged = make(chan struct{})
	s.mu.Unlock()
	go channel.pump()
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}
