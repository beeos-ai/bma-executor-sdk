//go:build !windows

package bmaexecutor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A brain whose tools run here is offered the platform's web tools of
// turn.start config.platform_tools; their calls go to the platform tools
// endpoint (metadata platform_tools_mcp_*), never to this machine.

// platformEndpoint is the platform tools endpoint: MCP over HTTP, answering
// every tools/call with a web_search result text.
type platformEndpoint struct {
	mu    sync.Mutex
	calls []map[string]any
	auth  []string
	srv   *httptest.Server
}

func newPlatformEndpoint(t *testing.T) *platformEndpoint {
	e := &platformEndpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}}
		if req.Method == "tools/call" {
			e.mu.Lock()
			e.calls = append(e.calls, req.Params)
			e.auth = append(e.auth, r.Header.Get("Authorization"))
			e.mu.Unlock()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Web search results for \"bma\""}},
				"isError": false, "structuredContent": map[string]any{"outcome": "executed", "replayed": false, "provider": "exa"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

var webSearchTool = PlatformTool{Name: "web_search", Description: "Search the public web.", Permission: "allow",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}

func platformToolsTurn(url, token string, tools ...PlatformTool) func(*TurnStartParams) {
	return func(p *TurnStartParams) {
		p.Metadata = map[string]string{}
		if url != "" {
			p.Metadata[metaPlatformToolsURL] = url
		}
		if token != "" {
			p.Metadata[metaPlatformToolsToken] = token
			p.Metadata[metaPlatformToolsExpiresAt] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		}
		p.Config.PlatformTools = tools
	}
}

func TestBrainHostDeclaresPlatformToolsUnlessHandsMayBeElsewhere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	c, err := f.executor.AgentCapability()
	if err != nil || !c.PlatformTools || !slices.Equal(c.PlatformCredentials, []string{PlatformCredentialPlatformTools}) {
		t.Fatalf("capability %+v, %v", c, err)
	}
	remote := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true})
	if c, err := remote.executor.AgentCapability(); err != nil || c.PlatformTools ||
		slices.Contains(c.PlatformCredentials, PlatformCredentialPlatformTools) {
		t.Fatalf("a host whose hands may be elsewhere: %+v, %v", c, err)
	}
}

func TestBrainHostRunsPlatformToolsOnThePlatform(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e := newPlatformEndpoint(t)
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_w", "turn_1", "websearch", platformToolsTurn(e.srv.URL+"/v1/machines/mch_1/mcp", "ptok", webSearchTool))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := lastMessage(t, f, "turn_1"); !strings.HasPrefix(got, `Web search results for "bma"|false|executed|`) {
		t.Fatalf("answer = %q", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) != 1 || e.calls[0]["name"] != "web_search" || e.auth[0] != "Bearer ptok" {
		t.Fatalf("endpoint calls %v auth %v", e.calls, e.auth)
	}
	meta, _ := e.calls[0]["_meta"].(map[string]any)
	if key, _ := meta[metaIdempotency].(string); !strings.HasPrefix(key, "bma-tool-") {
		t.Fatalf("no idempotency key: %v", meta)
	}
	var used bool
	for _, ev := range eventsOf(f.platform.snapshot(), "turn_1") {
		if m := eventJSON(t, ev); m["type"] == AgentEventTypeAgentToolUse && m["name"] == "web_search" {
			used = true
		}
	}
	if !used {
		t.Fatal("no agent.tool_use web_search")
	}
}

// Without the credential a platform tool is refused, never run here.
func TestBrainHostPlatformToolsNeedTheirCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_w", "turn_1", "websearch", platformToolsTurn("", "", webSearchTool))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := lastMessage(t, f, "turn_1"); !strings.Contains(got, "|true|not_forwarded|tools_unavailable") {
		t.Fatalf("answer = %q", got)
	}
}

// Only web_search and web_fetch are platform tools of a brain whose tools
// run here: another entry is invalid params.
func TestBrainHostRefusesOtherPlatformTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	image := webSearchTool
	image.Name = "generate_image"
	params := TurnStartParams{SessionID: "sesn_w", TurnID: "turn_1", InputID: "in_1",
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}, Config: &AgentTurnConfig{}}
	platformToolsTurn("https://x/v1/machines/m/mcp", "t", image)(&params)
	if _, err := f.executor.StartTurn(ctx, params); err == nil || !strings.Contains(err.Error(), "invalid params") {
		t.Fatalf("turn.start with generate_image: %v", err)
	}
}
