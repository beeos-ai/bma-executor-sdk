package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// The example brain run against a platform made of the SDK's Harness (the
// executor protocol) and a minimal tool gateway (MCP over HTTP): it declares
// brain_tools, asks approval, and sends the call to the gateway, never to this
// machine.

// gateway is just enough of the platform's tool gateway to see what a brain sends.
type gateway struct {
	mu    sync.Mutex
	calls []json.RawMessage
	auth  []string
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	switch request.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "s1")
		reply(w, request.ID, map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "gateway", "version": "0"}})
	case "tools/call":
		g.mu.Lock()
		g.calls = append(g.calls, request.Params)
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		reply(w, request.ID, map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": "ran on the hands"}},
			"structuredContent": map[string]any{"outcome": "executed"},
		})
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func reply(w http.ResponseWriter, id *int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// platform records the turn's events and answers every permission with allow.
type platform struct {
	mu       sync.Mutex
	events   map[string][]bmaexecutor.AgentEventRecord
	idle     map[string]chan struct{}
	executor *bmaexecutor.Executor
	asked    []bmaexecutor.InteractionRequestParams
}

func (p *platform) AckedSequence(context.Context, string) (int64, error) { return 0, nil }

func (p *platform) AgentEvent(_ context.Context, _ *bmaexecutor.Executor, e bmaexecutor.AgentEventRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events[e.TurnID] = append(p.events[e.TurnID], e)
	if e.Type == bmaexecutor.AgentEventTypeSessionStatusIdle {
		close(p.idle[e.TurnID])
	}
	return nil
}

func (p *platform) AgentDelta(*bmaexecutor.Executor, bmaexecutor.SessionEventDeltaParams) {}

func (p *platform) InteractionRequested(_ context.Context, _ *bmaexecutor.Executor, request bmaexecutor.InteractionRequestParams) (string, error) {
	p.mu.Lock()
	p.asked = append(p.asked, request)
	p.mu.Unlock()
	go func() {
		_, _ = p.executor.RespondInteraction(context.Background(), bmaexecutor.InteractionResponseParams{
			SessionID: request.SessionID, InteractionID: request.InteractionID, Outcome: bmaexecutor.InteractionOutcomeAllow,
			Scope: bmaexecutor.InteractionScopeOnce, Responder: &bmaexecutor.InteractionResponder{Surface: "test"},
		})
	}()
	return request.InteractionID, nil
}

func (p *platform) InteractionCanceled(context.Context, *bmaexecutor.Executor, bmaexecutor.InteractionCancelParams) error {
	return nil
}

// text returns the text blocks of the turn's events of one type.
func (p *platform) texts(turnID, eventType string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, e := range p.events[turnID] {
		if e.Type != eventType {
			continue
		}
		var body struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(e.Event, &body)
		for _, block := range body.Content {
			out = append(out, block.Text)
		}
	}
	return out
}

func runTurn(t *testing.T, p *platform, executor *bmaexecutor.Executor, params bmaexecutor.TurnStartParams) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p.mu.Lock()
	p.idle[params.TurnID] = make(chan struct{})
	done := p.idle[params.TurnID]
	p.mu.Unlock()
	if _, err := executor.StartTurn(ctx, params); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("turn %s did not end", params.TurnID)
	}
}

func start(t *testing.T) (*platform, *bmaexecutor.Executor) {
	t.Helper()
	return startFor(t, brain{})
}

// startFor is start with agent in place of the brain.
func startFor(t *testing.T, agent bmaexecutor.AgentHandler) (*platform, *bmaexecutor.Executor) {
	t.Helper()
	p := &platform{events: map[string][]bmaexecutor.AgentEventRecord{}, idle: map[string]chan struct{}{}}
	harness := bmaexecutor.NewHarness(bmaexecutor.HarnessOptions{Agent: p})
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)
	client, err := newClientFor(bmaexecutor.Options{EnvironmentID: "env_local", TokenSource: bmaexecutor.StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), "")}, agent)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	if p.executor, err = harness.WaitExecutor(ctx); err != nil {
		t.Fatal(err)
	}
	return p, p.executor
}

func input(prompt string) []bmaexecutor.AgentInputBlock {
	return []bmaexecutor.AgentInputBlock{{Type: bmaexecutor.AgentInputBlockTypeText, Text: prompt}}
}

func TestBrainDeclaresBrainTools(t *testing.T) {
	_, executor := start(t)
	advertised, err := executor.AgentCapability()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(advertised.PlatformCredentials, ","); got != bmaexecutor.PlatformCredentialBrainTools {
		t.Fatalf("platform_credentials = %q, want brain_tools", got)
	}
	if len(advertised.Harnesses) != 1 || advertised.Harnesses[0].ID != "third-party-brain" {
		t.Fatalf("harnesses = %+v", advertised.Harnesses)
	}
}

// A turn with the platform's tools endpoint: approval asked under the
// deterministic id, the call sent to the gateway with the bearer token and the
// approval, the result reported.
func TestBrainRunsItsToolOnTheHandsThroughTheGateway(t *testing.T) {
	g := &gateway{}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	p, executor := start(t)

	runTurn(t, p, executor, bmaexecutor.TurnStartParams{
		SessionID: "sesn_1", TurnID: "turn_1", InputID: "ain_1", Input: input("bash: echo hi"),
		Metadata: map[string]string{"tools_mcp_url": gw.URL, "tools_mcp_token": "tok-1"},
	})

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.calls) != 1 || g.auth[0] != "Bearer tok-1" {
		t.Fatalf("gateway calls = %d, auth = %v", len(g.calls), g.auth)
	}
	var call struct {
		Name string         `json:"name"`
		Args map[string]any `json:"arguments"`
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(g.calls[0], &call); err != nil {
		t.Fatal(err)
	}
	if call.Name != "bash" || call.Args["command"] != "echo hi" {
		t.Fatalf("call = %+v", call)
	}
	p.mu.Lock()
	asked := p.asked[0].InteractionID
	p.mu.Unlock()
	if !strings.HasPrefix(asked, "appr_") || call.Meta["ai.beeos.bma/approval"] != asked {
		t.Fatalf("approval presented %v, asked under %q", call.Meta["ai.beeos.bma/approval"], asked)
	}
	if call.Meta["ai.beeos.bma/idempotency_key"] == nil || call.Meta["ai.beeos.bma/tool_use_id"] == nil {
		t.Fatalf("_meta = %v", call.Meta)
	}
	if got := p.texts("turn_1", "agent.tool_result"); len(got) != 1 || got[0] != "ran on the hands" {
		t.Fatalf("tool results = %v", got)
	}
	if got := p.texts("turn_1", "agent.message"); len(got) != 1 || !strings.Contains(got[0], "executed") {
		t.Fatalf("messages = %v", got)
	}
}

// Without a tools endpoint the hands are this machine, which the example has no tools for.
func TestBrainWithoutAToolsEndpointRunsNothing(t *testing.T) {
	g := &gateway{}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	p, executor := start(t)

	runTurn(t, p, executor, bmaexecutor.TurnStartParams{SessionID: "sesn_2", TurnID: "turn_2", InputID: "ain_2", Input: input("bash: echo hi")})

	if len(g.calls) != 0 || len(p.texts("turn_2", "agent.tool_result")) != 0 {
		t.Fatal("a tool ran")
	}
	if got := p.texts("turn_2", "agent.message"); len(got) != 1 || !strings.Contains(got[0], "no local tools") {
		t.Fatalf("messages = %v", got)
	}
}

// Half of the credential is refused, not run anywhere else.
func TestBrainWithAHalfCredentialDoesNotRunTheCall(t *testing.T) {
	p, executor := start(t)
	runTurn(t, p, executor, bmaexecutor.TurnStartParams{
		SessionID: "sesn_3", TurnID: "turn_3", InputID: "ain_3", Input: input("bash: echo hi"),
		Metadata: map[string]string{"tools_mcp_url": "http://127.0.0.1:1"},
	})
	results := p.texts("turn_3", "agent.tool_result")
	if len(results) != 1 || !strings.HasPrefix(results[0], "tools_unavailable:") {
		t.Fatalf("tool results = %v", results)
	}
}

func TestBrainAnswersPlainText(t *testing.T) {
	p, executor := start(t)
	runTurn(t, p, executor, bmaexecutor.TurnStartParams{SessionID: "sesn_4", TurnID: "turn_4", InputID: "ain_4", Input: input("hello")})
	if got := p.texts("turn_4", "agent.message"); len(got) != 1 || got[0] != "You said: hello" {
		t.Fatalf("messages = %v", got)
	}
}
