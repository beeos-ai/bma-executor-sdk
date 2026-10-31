package bmaexecutor

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// HarnessMCPChannel is the harness end of one MCP channel, isolated by
// (session_id, server_key). Frames are raw MCP JSON-RPC exactly as the MCP
// server sent them: the harness is the MCP client.
type HarnessMCPChannel struct {
	ws        *websocket.Conn
	closeOnce sync.Once
	closed    chan struct{}
	detach    func()
}

// Read returns the next raw MCP frame. A transport error closes the channel;
// the executor reopens a fresh one (and a fresh MCP server process), so call
// Session.MCP again and re-initialize.
func (m *HarnessMCPChannel) Read(ctx context.Context) ([]byte, error) {
	_, data, err := m.ws.Read(ctx)
	if err != nil && ctx.Err() == nil {
		_ = m.Close()
	}
	return data, err
}

func (m *HarnessMCPChannel) Write(ctx context.Context, frame []byte) error {
	return m.ws.Write(ctx, websocket.MessageText, frame)
}

func (m *HarnessMCPChannel) Close() error {
	m.closeOnce.Do(func() {
		_ = m.ws.Close(websocket.StatusNormalClosure, "mcp channel closed")
		close(m.closed)
		if m.detach != nil {
			m.detach()
		}
	})
	return nil
}

// MCP waits for the executor to open the MCP channel for serverKey (one of
// the session.assign mcp_servers) and returns it. A newer channel for the
// same key replaces the old one; the session's end closes all of them.
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

func (h *Harness) serveMCP(w http.ResponseWriter, r *http.Request, principal Principal) {
	query := r.URL.Query()
	sessionID, serverKey := query.Get("session_id"), query.Get("server_key")
	if sessionID == "" || serverKey == "" {
		http.Error(w, "session_id and server_key are required", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	session := h.sessions[sessionID]
	h.mu.Unlock()
	// Only the executor holding the session may serve its MCP servers.
	if session == nil || (principal.ExecutorID != "" && principal.ExecutorID != session.executor.id) {
		http.Error(w, "session is not active for this executor", http.StatusForbidden)
		return
	}
	ws, ok := h.accept(w, r)
	if !ok {
		return
	}
	channel := &HarnessMCPChannel{ws: ws, closed: make(chan struct{})}
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
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}
