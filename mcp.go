package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// MCPChannel is a raw MCP JSON-RPC transport on the executor MCP channel. The
// payload is unframed MCP JSON-RPC: the caller sends initialize,
// notifications/initialized, tools/list and tools/call directly, exactly as
// the protocol appendix defines.
//
// This release provides the channel transport only. MCP process supervision
// (stdio spawn, stderr capture, restart backoff, generation) and automatic
// reconnection stay with the adapter until the SDK grows that lifecycle.
type MCPChannel struct {
	ws *websocket.Conn
}

// OpenMCPChannel opens wss://.../v1/executors/channel?channel=mcp with a fresh
// executor token. sessionID and serverKey identify the environment-owned MCP
// server from session.assign.mcp_servers.
func (c *Client) OpenMCPChannel(ctx context.Context, sessionID, serverKey string) (*MCPChannel, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("bmaexecutor: sessionID is required")
	}
	if strings.TrimSpace(serverKey) == "" {
		return nil, errors.New("bmaexecutor: serverKey is required")
	}

	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return nil, ErrClientStopped
	}

	token, err := c.fetchToken(ctx)
	if err != nil {
		return nil, err
	}
	if len(token.ChannelScope) > 0 && !allowsChannel(token.ChannelScope, ChannelScopeMcp) {
		return nil, errors.New("bmaexecutor: executor token does not allow the mcp channel")
	}

	rawURL := c.options.WSURL
	if rawURL == "" {
		rawURL = token.WSURL
	}
	channelURL, err := mcpChannelURL(rawURL, sessionID, serverKey)
	if err != nil {
		return nil, err
	}

	ws, _, err := websocket.Dial(ctx, channelURL, &websocket.DialOptions{
		HTTPHeader:   authHeader(token.Token),
		Subprotocols: []string{WSSubprotocol},
	})
	if err != nil {
		return nil, fmt.Errorf("bmaexecutor: mcp websocket dial: %w", err)
	}
	ws.SetReadLimit(c.mcpFrameLimit())
	return &MCPChannel{ws: ws}, nil
}

// Read returns one MCP JSON-RPC frame. Frames are newline-delimited JSON on the
// wire but this transport returns one WebSocket message per call.
func (m *MCPChannel) Read(ctx context.Context) ([]byte, error) {
	if m == nil || m.ws == nil {
		return nil, errors.New("bmaexecutor: mcp channel is closed")
	}
	_, data, err := m.ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Write sends one MCP JSON-RPC frame as a text message.
func (m *MCPChannel) Write(ctx context.Context, payload []byte) error {
	if m == nil || m.ws == nil {
		return errors.New("bmaexecutor: mcp channel is closed")
	}
	return m.ws.Write(ctx, websocket.MessageText, payload)
}

// Close closes the MCP channel.
func (m *MCPChannel) Close() error {
	if m == nil || m.ws == nil {
		return nil
	}
	return m.ws.Close(websocket.StatusNormalClosure, "mcp channel closed")
}

func (c *Client) mcpFrameLimit() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registerResult != nil && c.registerResult.MaxMCPFrameBytes > 0 {
		return c.registerResult.MaxMCPFrameBytes
	}
	return maxFrameBytes
}

func mcpChannelURL(rawURL, sessionID, serverKey string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("bmaexecutor: invalid mcp channel URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("bmaexecutor: mcp channel URL must use ws or wss")
	}
	query := parsed.Query()
	query.Set("channel", "mcp")
	query.Set("session_id", sessionID)
	query.Set("server_key", serverKey)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func allowsChannel(scopes []ChannelScope, want ChannelScope) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}
