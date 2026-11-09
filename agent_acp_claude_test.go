//go:build !windows

package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Claude Code profile is tested against the real, pinned claude-agent-acp
// (and the real Claude Code binary it starts): npm ci in
// testdata/claude-agent-acp, or BMA_CLAUDE_AGENT_ACP_DIR naming a directory
// with that install. There is no Anthropic credential in a test: the agent's
// model endpoint is a scripted Anthropic Messages API on loopback, and the
// "API key" is a made-up value. Tests that need the agent skip when it is not
// installed; the gates also have tests against a fake agent that always run.

func lookNode() (string, error) {
	path, err := exec.LookPath("node")
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func claudeAgentCommand(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("BMA_CLAUDE_AGENT_ACP_DIR")
	if dir == "" {
		dir = "testdata/claude-agent-acp"
	}
	entry, err := filepath.Abs(filepath.Join(dir, "node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Skipf("claude-agent-acp is not installed (npm ci in testdata/claude-agent-acp): %v", err)
	}
	node, err := lookNode()
	if err != nil {
		t.Skip("node is not installed")
	}
	return []string{node, entry}
}

// scriptedModel is an Anthropic Messages API: it asks for one platform tool
// call, then answers "done" once it has the tool's result.
type scriptedModel struct {
	server *httptest.Server
	tool   string
	input  map[string]any

	mu       sync.Mutex
	requests []modelRequest
	toolUses int
}

type modelRequest struct {
	Tools  []string
	APIKey string
}

func newScriptedModel(t *testing.T, tool string, input map[string]any) *scriptedModel {
	m := &scriptedModel{tool: tool, input: input}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		_, _ = w.Write([]byte("{}"))
		return
	}
	var body struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Tools    []struct{ Name string }
		Messages []json.RawMessage
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	req := modelRequest{APIKey: r.Header.Get("X-Api-Key")}
	for _, tool := range body.Tools {
		req.Tools = append(req.Tools, tool.Name)
	}
	if os.Getenv("BMA_DEBUG_MODEL") != "" && len(body.Messages) > 0 {
		fmt.Fprintf(os.Stderr, "MODEL n=%d last=%.300s\n", len(body.Messages), body.Messages[len(body.Messages)-1])
	}
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	offered := false
	for _, name := range req.Tools {
		offered = offered || name == claudeToolPrefix+m.tool
	}
	// One tool call per prompt ("run it"): Claude Code folds a turn's prompt and
	// tool results into one user message, so count prompts, not messages.
	prompts := 0
	for _, raw := range body.Messages {
		var msg struct {
			Role    string
			Content json.RawMessage
		}
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(raw, &msg) != nil || msg.Role != "user" || json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" && strings.HasPrefix(b.Text, "run it") {
				prompts++
			}
		}
	}
	m.mu.Lock()
	needTool := offered && prompts > m.toolUses
	m.mu.Unlock()
	type block = map[string]any
	var content []block
	stop := "end_turn"
	switch {
	case needTool:
		m.mu.Lock()
		m.toolUses++
		id := fmt.Sprintf("toolu_%02d", m.toolUses)
		m.mu.Unlock()
		content = []block{{"type": "tool_use", "id": id, "name": claudeToolPrefix + m.tool, "input": m.input}}
		stop = "tool_use"
	case offered:
		content = []block{{"type": "text", "text": "done"}}
	default: // a side request (title, summary)
		content = []block{{"type": "text", "text": "ok"}}
	}
	message := map[string]any{"id": "msg_x", "type": "message", "role": "assistant", "model": body.Model, "content": content,
		"stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, data map[string]any) {
		data["type"] = kind
		raw, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw)
	}
	start := map[string]any{}
	for k, v := range message {
		start[k] = v
	}
	start["content"] = []block{}
	event("message_start", map[string]any{"message": start})
	for i, b := range content {
		if b["type"] == "text" {
			event("content_block_start", map[string]any{"index": i, "content_block": block{"type": "text", "text": ""}})
			event("content_block_delta", map[string]any{"index": i, "delta": block{"type": "text_delta", "text": b["text"]}})
		} else {
			event("content_block_start", map[string]any{"index": i, "content_block": block{"type": "tool_use", "id": b["id"], "name": b["name"], "input": block{}}})
			raw, _ := json.Marshal(b["input"])
			event("content_block_delta", map[string]any{"index": i, "delta": block{"type": "input_json_delta", "partial_json": string(raw)}})
		}
		event("content_block_stop", map[string]any{"index": i})
	}
	event("message_delta", map[string]any{"delta": block{"stop_reason": stop, "stop_sequence": nil}, "usage": block{"output_tokens": 5}})
	event("message_stop", map[string]any{})
}

// offeredTools are the tool names of every request that offered tools.
func (m *scriptedModel) offeredTools() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range m.requests {
		for _, name := range r.Tools {
			seen[name] = true
		}
	}
	var names []string
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

const claudeTestAPIKey = "sk-ant-test-made-up-key"

// startClaudeFixture starts an ACPAgent hosting the real claude-agent-acp,
// with an empty HOME (no login, no settings) unless settings is set.
func startClaudeFixture(t *testing.T, ctx context.Context, model *scriptedModel, opts ACPAgentOptions, settings string) *acpFixture {
	t.Helper()
	home := t.TempDir()
	if settings != "" {
		noErr(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
		noErr(t, os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(settings), 0o644))
	}
	t.Setenv("HOME", home)
	opts.Profile = ACPProfileClaudeCode
	opts.Command = claudeAgentCommand(t)
	opts.Env = append(opts.Env, "ANTHROPIC_BASE_URL="+model.server.URL, "ANTHROPIC_API_KEY="+claudeTestAPIKey,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_AUTOUPDATER=1")
	opts.CancelGrace = 10 * time.Second
	return startACPFixture(t, ctx, opts)
}

func claudeEvents(t *testing.T, f *acpFixture, turn string) []string {
	t.Helper()
	return eventTypes(eventsOf(f.platform.snapshot(), turn))
}

func TestClaudeCodeProfileRunsAnApprovedPlatformTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "printf hi > out.txt; cat out.txt"})
	// The owner's settings start Claude Code in a mode that would skip asking:
	// the gate sets it to default.
	f := startClaudeFixture(t, ctx, model, ACPAgentOptions{}, `{"permissions":{"defaultMode":"acceptEdits"}}`)

	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.Harnesses) != 1 || capability.Harnesses[0].ID != "claude-code" ||
		capability.Harnesses[0].AgentName != claudeAgentACPName || capability.Harnesses[0].Protocol != "acp/1" {
		t.Fatalf("capability = %+v, %v", capability, err)
	}
	if len(capability.PlatformCredentials) != 0 || capability.TurnResume {
		t.Fatalf("a Claude Code brain must not declare platform credentials (harness_model) nor turn_resume: %+v", capability)
	}

	f.start(t, ctx, "sesn_c", "turn_1", "run it")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	if request.InteractionID != approvalID("sesn_c", "turn_1", "toolu_01") || request.ToolName != "bash" || request.ToolInput["command"] == nil {
		t.Fatalf("interaction.request = %+v", request)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_c", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)

	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentToolUse, AgentEventTypeAgentToolResult, AgentEventTypeAgentMessage, AgentEventTypeSessionStatusIdle}
	if got := claudeEvents(t, f, "turn_1"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	events := eventsOf(f.platform.snapshot(), "turn_1")
	use, result, message := eventJSON(t, events[1]), eventJSON(t, events[2]), eventJSON(t, events[3])
	if use["name"] != "bash" || use["evaluated_permission"].(map[string]any)["type"] != "ask" || use["input"].(map[string]any)["command"] == nil {
		t.Fatalf("tool_use = %v", use)
	}
	if text := fmt.Sprint(result["content"]); result["tool_use_id"] != events[1].EventID || !strings.Contains(text, "hi") || !strings.Contains(text, "exit_code: 0") {
		t.Fatalf("tool_result = %v", result)
	}
	if fmt.Sprint(message["content"]) != "[map[text:done type:text]]" {
		t.Fatalf("agent.message = %v", message)
	}
	// The tool ran here, in the session's working directory.
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_c", "out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("out.txt = %q, %v", b, err)
	}
	// Claude Code has no tool of its own: only the injected platform tools (and a placeholder).
	for _, name := range model.offeredTools() {
		if _, ok := claudePlatformTool(name); !ok && name != "DeferredToolPlaceholder" {
			t.Fatalf("the model was offered %q: %v", name, model.offeredTools())
		}
	}
	// The owner's own credential reached the model endpoint; BMA's model proxy was not involved.
	if model.requests[0].APIKey != claudeTestAPIKey || f.upstream.requests != 0 {
		t.Fatalf("model api key %q, BMA upstream requests %d", model.requests[0].APIKey, f.upstream.requests)
	}
}

func TestClaudeCodeProfileDenyRunsNothingAndPolicyAllowAsksNobody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "touch ran.txt"})
	f := startClaudeFixture(t, ctx, model, ACPAgentOptions{}, "")

	f.start(t, ctx, "sesn_c", "turn_1", "run it")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_c", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeDeny}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_c", "ran.txt")); err == nil {
		t.Fatal("a denied tool ran")
	}

	// The platform's policy allows the tool: no interaction, it runs.
	f.start(t, ctx, "sesn_c", "turn_2", "run it", func(p *TurnStartParams) {
		p.ExecutorSessionRef = "ignored-for-an-open-session"
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if len(f.platform.interactions) != 1 {
		t.Fatalf("interactions = %d, want only the first turn's", len(f.platform.interactions))
	}
	t.Logf("turn_2 events: %v", claudeEvents(t, f, "turn_2"))
	use := eventJSON(t, eventsOf(f.platform.snapshot(), "turn_2")[1])
	if use["evaluated_permission"].(map[string]any)["type"] != "allow" {
		t.Fatalf("tool_use = %v", use)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_c", "ran.txt")); err != nil {
		t.Fatalf("an allowed tool did not run: %v", err)
	}
}

func TestClaudeCodeProfileRunsTheToolsOnTheRemoteHands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	model := newScriptedModel(t, "bash", map[string]any{"command": "echo pwned > pwned.txt"})
	f := startClaudeFixture(t, ctx, model, ACPAgentOptions{PlatformTools: true}, "")
	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.PlatformCredentials) != 1 || capability.PlatformCredentials[0] != PlatformCredentialBrainTools {
		t.Fatalf("platform_credentials = %+v, %v", capability, err)
	}
	f.start(t, ctx, "sesn_c", "turn_1", "run it", remoteTurn(gw.server.URL, "tok-1"))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if _, err := os.Stat(filepath.Join(gw.workspace, "pwned.txt")); err != nil {
		t.Fatalf("the tool did not run on the hands machine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_c", "pwned.txt")); err == nil {
		t.Fatal("the tool also ran on the brain's machine")
	}
	// The MCP call to the gateway carried Claude Code's call id as the idempotency identity and the tool_use id.
	var call *stubRequest
	for _, r := range gw.recorded() {
		if r.Method == "tools/call" {
			call = &r
		}
	}
	if call == nil || !strings.Contains(string(call.Params), platformIdempotencyKey("sesn_c", "turn_1", "toolu_01")) {
		t.Fatalf("gateway calls: %+v", gw.recorded())
	}
}

// --- gates, against a fake claude-agent-acp ---

func startFakeClaude(t *testing.T, ctx context.Context, mode string) (*ACPAgent, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	agent, err := NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCode, Command: []string{exe}, Env: []string{"BMA_ACP_FAKE_CLAUDE=" + mode},
		StateDir: t.TempDir(), Core: core, CancelGrace: time.Second})
	if agent != nil {
		t.Cleanup(func() { _ = agent.Close() })
	}
	return agent, err
}

func TestClaudeCodeProfileRefusesAnUnpinnedRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := startFakeClaude(t, ctx, "unpinned"); err == nil || !strings.Contains(err.Error(), "not a pinned release") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeCodeProfileRejectsPlatformModel(t *testing.T) {
	_, err := NewACPAgent(context.Background(), ACPAgentOptions{Profile: ACPProfileClaudeCode, PlatformModel: true, Command: []string{"/bin/false"}, StateDir: t.TempDir(), Core: &Core{}})
	if err == nil || !strings.Contains(err.Error(), "PlatformModel") {
		t.Fatalf("err = %v", err)
	}
}

// The agent's mode stays something else after session/set_mode default: the turn is refused.
func TestClaudeCodeProfileRefusesATurnWhoseModeIsNotDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent, err := startFakeClaude(t, ctx, "stuck")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = agent.OpenTurn(ctx, TurnStartParams{SessionID: "sesn_f", TurnID: "turn_1", InputID: "in_1", Config: &AgentTurnConfig{}}); err != nil {
		t.Fatalf("OpenTurn: %v (the session is opened after turn.start is answered)", err)
	}
	var agentErr *AgentError
	err = agent.openSession(ctx, agent.session("sesn_f"))
	if !errors.As(err, &agentErr) || agentErr.Code != AgentErrorCodeAgentError || !strings.Contains(err.Error(), "mode_not_default") {
		t.Fatalf("openSession err = %v", err)
	}
	// A mode that is default afterwards is accepted.
	agent2, err := startFakeClaude(t, ctx, "ok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent2.OpenTurn(ctx, TurnStartParams{SessionID: "sesn_f", TurnID: "turn_1", InputID: "in_1", Config: &AgentTurnConfig{}}); err != nil {
		t.Fatalf("OpenTurn: %v", err)
	}
	if err := agent2.openSession(ctx, agent2.session("sesn_f")); err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if s := agent2.sessions["sesn_f"]; s.agentID != "claude-session-1" || s.agentMode != "default" {
		t.Fatalf("session = %+v", s)
	}
}

// Claude Code reports a tool that is not a platform tool: the turn fails and the agent is told to stop.
func TestClaudeCodeProfileFailsATurnWithAForeignTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, _ := os.Executable()
	opts := ACPAgentOptions{Profile: ACPProfileClaudeCode, Command: []string{exe}, Env: []string{"BMA_ACP_FAKE_CLAUDE=catalog"}, CancelGrace: time.Second}
	f := startACPFixtureAtCore(t, ctx, opts, t.TempDir(), newRecordingPlatform(), nil)
	f.start(t, ctx, "sesn_f", "turn_1", "go")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
	var failure string
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if m := eventJSON(t, e); m["type"] == AgentEventTypeSessionError {
			failure = fmt.Sprint(m["error"])
		}
	}
	if !strings.Contains(failure, "tool_catalog_violation") || !strings.Contains(failure, `"Bash"`) {
		t.Fatalf("session.error = %q", failure)
	}
}

func TestClaudePlatformToolNames(t *testing.T) {
	for in, want := range map[string]string{"mcp__bma__bash": "bash", "mcp__bma__grep": "grep"} {
		if got, ok := claudePlatformTool(in); !ok || got != want {
			t.Errorf("%s = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"Bash", "mcp__other__bash", "mcp__bma__rm", "mcp__bma__", "DeferredToolPlaceholder", ""} {
		if got, ok := claudePlatformTool(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}
