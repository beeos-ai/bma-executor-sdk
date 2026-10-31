package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// MCPChannel is a raw MCP JSON-RPC transport on the executor MCP channel. The
// payload is unframed MCP JSON-RPC: the caller sends initialize,
// notifications/initialized, tools/list and tools/call directly, exactly as
// the protocol appendix defines. It runs on the client's transport:
// WebSocket, a Unix socket or in-process.
type MCPChannel struct {
	conn Conn
}

// OpenMCPChannel opens the MCP channel (?channel=mcp on WebSocket
// transports) with a fresh executor token through Options.Dial. sessionID
// and serverKey identify the environment-owned MCP server from
// session.assign.mcp_servers.
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
	if len(token.ChannelScope) > 0 && !allowsChannel(token.ChannelScope, ChannelMCP) {
		return nil, errors.New("bmaexecutor: executor token does not allow the mcp channel")
	}
	if c.options.WSURL != "" {
		token.WSURL = c.options.WSURL
	}
	conn, err := c.options.Dial(ctx, token, ProtocolV11, ChannelTarget{Channel: ChannelMCP, SessionID: sessionID, ServerKey: serverKey})
	if err != nil {
		return nil, fmt.Errorf("bmaexecutor: mcp channel dial: %w", err)
	}
	if ws, ok := conn.(wsConn); ok {
		ws.ws.SetReadLimit(c.mcpFrameLimit())
	}
	return &MCPChannel{conn: conn}, nil
}

// Read returns one MCP JSON-RPC frame: one transport message per call.
func (m *MCPChannel) Read(ctx context.Context) ([]byte, error) {
	return m.conn.Read(ctx)
}

// Write sends one MCP JSON-RPC frame as a text message.
func (m *MCPChannel) Write(ctx context.Context, payload []byte) error {
	return m.conn.Write(ctx, payload)
}

// Close closes the MCP channel.
func (m *MCPChannel) Close() error {
	return m.conn.Close(1000, "mcp channel closed")
}

func (c *Client) mcpFrameLimit() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registerResult != nil && c.registerResult.MaxMCPFrameBytes > 0 {
		return c.registerResult.MaxMCPFrameBytes
	}
	return maxFrameBytes
}

func allowsChannel(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}
