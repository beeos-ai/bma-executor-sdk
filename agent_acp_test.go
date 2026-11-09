package bmaexecutor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The ACP agent driver is tested against a fake ACP agent: this test binary
// re-executed with BMA_ACP_FAKE_AGENT=1 (fakeACPAgent below, wired in
// TestMain). It speaks the bma-dsh-acp protocol (ACP v1 + _meta.bma,
// _bma/message, _bma/tools/call); the prompt text selects a script.

type acpFixture struct {
	agent    *ACPAgent
	executor *Executor
	client   *Client
	platform *recordingPlatform
	root     string
	upstream *fakeUpstream
}

type fakeUpstream struct {
	mu       sync.Mutex
	auth     []string
	models   []string
	requests int
	server   *httptest.Server
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.requests++
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.models = append(u.models, body.Model)
		u.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"upstream-ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(u.server.Close)
	return u
}

func startACPFixture(t *testing.T, ctx context.Context, opts ACPAgentOptions) *acpFixture {
	t.Helper()
	return startACPFixtureAt(t, ctx, opts, t.TempDir(), newRecordingPlatform(), nil)
}

// startACPFixtureAt starts an executor process (agent, core, client) on a
// state root and a platform; calling it again with the same root and platform
// is a restart of the executor process.
func startACPFixtureAt(t *testing.T, ctx context.Context, opts ACPAgentOptions, root string, platform *recordingPlatform, hook func(context.Context, string)) *acpFixture {
	t.Helper()
	return startACPFixtureAtCore(t, ctx, opts, root, platform, hook)
}

// startACPFixtureOn is startACPFixture with a platform of the test's own
// (embedding a recordingPlatform).
func startACPFixtureOn(t *testing.T, ctx context.Context, opts ACPAgentOptions, platform interface {
	AgentPlatform
	recorder() *recordingPlatform
}) *acpFixture {
	t.Helper()
	return startACPFixtureAtCore(t, ctx, opts, t.TempDir(), platform, nil)
}

// startACPFixtureAtCore is startACPFixtureAt; opts.Core, when set, is used
// instead of a new Core on root/work.
func startACPFixtureAtCore(t *testing.T, ctx context.Context, opts ACPAgentOptions, root string, platform interface {
	AgentPlatform
	recorder() *recordingPlatform
}, hook func(context.Context, string)) *acpFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Core == nil {
		core, err := NewCore(CoreOptions{WorkRoot: filepath.Join(root, "work")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(core.Close)
		opts.Core = core
	}
	work := opts.Core.opts.WorkRoot
	upstream := newFakeUpstream(t)
	if opts.Command == nil {
		opts.Command = []string{exe}
	}
	opts.Env = append(opts.Env, "BMA_ACP_FAKE_AGENT=1")
	opts.StateDir = filepath.Join(root, "state")
	if opts.Upstream.BaseURL == "" && !opts.PlatformModel {
		opts.Upstream = ACPModelUpstream{BaseURL: upstream.server.URL + "/v1", Model: "upstream-model",
			Header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer node-owner-key") }}
	}
	if opts.CancelGrace == 0 {
		opts.CancelGrace = time.Second
	}
	agent, err := NewACPAgent(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	agent.hook = hook
	t.Cleanup(func() { _ = agent.Close() })
	// session/new of Claude Code takes several seconds (it starts its CLI).
	requestTimeout := 5 * time.Second
	if opts.Profile == ACPProfileClaudeCode {
		requestTimeout = 60 * time.Second
	}
	harness := NewHarness(HarnessOptions{Agent: platform, RequestTimeout: requestTimeout})
	server := httptest.NewServer(harness)
	t.Cleanup(server.Close)
	client, err := NewClient(Options{
		EnvironmentID: "env_local", Executor: ExecutorInfo{Name: "mac", OS: "darwin", Arch: "arm64", Version: "0.1.0"},
		TokenSource:    StaticToken("ws"+strings.TrimPrefix(server.URL, "http"), "local"),
		RequestTimeout: requestTimeout, ReconnectBaseDelay: 20 * time.Millisecond, Random: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ServeAgent(agent, agent.Capability())
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &acpFixture{agent: agent, executor: executor, client: client, platform: platform.recorder(), root: work, upstream: upstream}
}

func (f *acpFixture) start(t *testing.T, ctx context.Context, session, turn, text string, extra ...func(*TurnStartParams)) TurnStartResult {
	t.Helper()
	params := TurnStartParams{SessionID: session, TurnID: turn, InputID: "in_" + turn,
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: text}}, Config: &AgentTurnConfig{System: "Be brief."}}
	for _, e := range extra {
		e(&params)
	}
	result, err := f.executor.StartTurn(ctx, params)
	if err != nil {
		t.Fatalf("turn.start %s: %v", turn, err)
	}
	return result
}

func eventsOf(events []AgentEventRecord, turnID string) []AgentEventRecord {
	var out []AgentEventRecord
	for _, e := range events {
		if e.TurnID == turnID {
			out = append(out, e)
		}
	}
	return out
}

func eventJSON(t *testing.T, e AgentEventRecord) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Event, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestACPAgentCapabilityDeclaresDSHHarness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	capability, err := f.executor.AgentCapability()
	if err != nil || capability == nil {
		t.Fatalf("capability: %+v %v", capability, err)
	}
	if len(capability.Harnesses) != 1 || capability.Harness == nil {
		t.Fatalf("harnesses = %+v harness = %+v", capability.Harnesses, capability.Harness)
	}
	h := capability.Harnesses[0]
	if h.ID != "dsh" || h.Protocol != "acp/1" || h.Streaming != "token" || h.PermissionRelay != "full" || !h.SessionLoad ||
		h.AgentName != "fake-dsh-acp" || h.Version != "9.9.9+dsh0.2.1-rc.5" || *capability.Harness != h {
		t.Fatalf("harness = %+v", h)
	}
	if !capability.TurnResume || !contains(capability.InteractionKinds, InteractionKindPermission) || !contains(capability.PermissionModes, "accept_edits") {
		t.Fatalf("capability = %+v", capability)
	}
}

func TestACPAgentRefusesARelativeCommand(t *testing.T) {
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	_, err = NewACPAgent(context.Background(), ACPAgentOptions{Command: []string{"bma-dsh-acp"}, Core: core, StateDir: t.TempDir(),
		Upstream: ACPModelUpstream{BaseURL: "http://127.0.0.1:1/v1"}})
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative command: %v", err)
	}
}

func TestACPAgentStreamsAndCommitsMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	result := f.start(t, ctx, "sesn_a", "turn_1", "hello")
	if result.ExecutorSessionRef != "sesn_a" || result.Resumed {
		t.Fatalf("turn.start result = %+v", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	got := eventTypes(eventsOf(f.platform.snapshot(), "turn_1"))
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentThinking, AgentEventTypeAgentMessage, AgentEventTypeSessionStatusIdle}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	msg := eventsOf(f.platform.snapshot(), "turn_1")[2]
	if text := eventJSON(t, msg)["content"].([]any)[0].(map[string]any)["text"]; text != "Hello" {
		t.Fatalf("message = %v", text)
	}
	f.platform.mu.Lock()
	defer f.platform.mu.Unlock()
	var previews []string
	for _, d := range f.platform.deltas {
		if d.EventID == msg.EventID {
			previews = append(previews, d.Text)
		}
	}
	if strings.Join(previews, "|") != "Hel|lo" {
		t.Fatalf("deltas for the message = %v", previews)
	}
}

func TestACPAgentToolPermissionAndExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_t", "turn_1", "run")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	sum := sha256.Sum256([]byte("sesn_t\x00turn_1\x00call_1"))
	if request.InteractionID != "appr_"+hex.EncodeToString(sum[:])[:24] || request.ToolName != "bash" ||
		request.ToolInput["command"] != "echo hi > out.txt && cat out.txt" || !hasEvent(f.platform.snapshot(), request.ToolUseID) {
		t.Fatalf("interaction.request = %+v", request)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_t", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	events := eventsOf(f.platform.snapshot(), "turn_1")
	got := eventTypes(events)
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentToolUse, AgentEventTypeAgentToolResult, AgentEventTypeAgentMessage, AgentEventTypeSessionStatusIdle}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	use := eventJSON(t, events[1])
	if use["name"] != "bash" || use["evaluated_permission"].(map[string]any)["type"] != "ask" || use["input"].(map[string]any)["command"] == nil {
		t.Fatalf("tool_use = %v", use)
	}
	res := eventJSON(t, events[2])
	text := fmt.Sprint(res["content"])
	if res["tool_use_id"] != events[1].EventID || !strings.Contains(text, "hi") || !strings.Contains(text, "exit_code: 0") {
		t.Fatalf("tool_result = %v", res)
	}
	// The tool ran in the session's working directory on this machine.
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_t", "out.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("out.txt = %q, %v", b, err)
	}

	// Deny: the agent is told, nothing runs.
	f.start(t, ctx, "sesn_t", "turn_2", "run", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_t" })
	f.platform.waitFor(t, "second permission", func() bool { return len(f.platform.interactions) == 2 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_t", InteractionID: f.platform.interactions[1].InteractionID, Outcome: InteractionOutcomeDeny}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	denied := eventJSON(t, eventsOf(f.platform.snapshot(), "turn_2")[2])
	if denied["is_error"] != true || !strings.Contains(fmt.Sprint(denied["content"]), "refused") {
		t.Fatalf("denied tool_result = %v", denied)
	}
}

func TestACPAgentFileToolsUseCMAInputNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_f", "turn_1", "files", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"write":"allow","edit":"allow","rm_rf":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	var results []string
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if e.Type == AgentEventTypeAgentMessage {
			results = append(results, fmt.Sprint(eventJSON(t, e)["content"]))
		}
	}
	joined := strings.Join(results, "\n")
	for _, want := range []string{"write:wrote", "edit:edited", "replace_all:edited", "read:X B", "grep:a.txt:1", "count:a.txt:1", "glob:a.txt", "outside:is_error", "unknown:is_error"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestACPAgentStopReasons(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	for turn, text := range map[string]string{"turn_long": "long", "turn_idle": "idle"} {
		f.start(t, ctx, "sesn_"+text, turn, text)
	}
	for turn, code := range map[string]string{"turn_long": "output_limit_reached: ", "turn_idle": "model_stream_idle: "} {
		assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeFailed)
		var message string
		for _, e := range eventsOf(f.platform.snapshot(), turn) {
			if e.Type == AgentEventTypeSessionError {
				message = eventJSON(t, e)["error"].(map[string]any)["message"].(string)
			}
		}
		if !strings.HasPrefix(message, code) {
			t.Fatalf("%s session.error = %q, want prefix %q", turn, message, code)
		}
	}
}

func TestACPAgentCancelWithPendingPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_c", "turn_1", "run")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	started := time.Now()
	if r, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_c", TurnID: "turn_1"}); err != nil || !r.Canceled {
		t.Fatalf("turn.cancel = %+v %v", r, err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if time.Since(started) > 3*time.Second {
		t.Fatalf("cancel took %v", time.Since(started))
	}
	f.platform.waitFor(t, "interaction withdrawn", func() bool { return len(f.platform.canceled) == 1 })
	// The agent stays usable.
	f.start(t, ctx, "sesn_c", "turn_2", "hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_c" })
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

func TestACPAgentKillsAnAgentThatIgnoresCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{CancelGrace: 300 * time.Millisecond, KillGrace: 300 * time.Millisecond})
	f.start(t, ctx, "sesn_d", "turn_1", "deaf")
	time.Sleep(200 * time.Millisecond)
	started := time.Now()
	if _, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_d", TurnID: "turn_1"}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if time.Since(started) > 3*time.Second {
		t.Fatalf("cancel took %v", time.Since(started))
	}
	// A new process serves the next turn; the conversation is loaded, not recreated.
	result := f.start(t, ctx, "sesn_d", "turn_2", "hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_d" })
	if !result.Resumed {
		t.Fatalf("turn.start after restart = %+v", result)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

func TestACPAgentProcessCrashFailsTheTurnAndUnknownSessionIsNotRecreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_x", "turn_1", "crash")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
	// The conversation to resume does not exist: turn.start is still accepted
	// (the open runs after the answer); the turn then fails with session.error.
	f.start(t, ctx, "sesn_missing", "turn_2", "hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_missing" })
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeFailed)
	if got := sessionErrors(f.platform, "turn_2"); len(got) != 1 || got[0].Type != AgentErrorCodeAgentError || !strings.Contains(got[0].Message, "not found") {
		t.Fatalf("session.error of the lost conversation = %+v", got)
	}
}

// sessionErrors are the session.error payloads of a turn.
func sessionErrors(p *recordingPlatform, turnID string) []AgentErrorInfo {
	var out []AgentErrorInfo
	for _, e := range p.snapshot() {
		if e.TurnID != turnID || e.Type != AgentEventTypeSessionError {
			continue
		}
		var event SessionErrorEvent
		_ = json.Unmarshal(e.Event, &event)
		out = append(out, event.Error)
	}
	return out
}

func TestACPAgentRefusesForeignHarnessAndCwd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	for name, config := range map[string]*AgentTurnConfig{
		"harness": {Harness: "claude-code"},
		"cwd":     {Cwd: "/etc"},
	} {
		_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_" + name, TurnID: "t_" + name, InputID: "i", Config: config,
			Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}})
		var rpc *RPCError
		want := map[string]int{"harness": CodeCapabilityUnavailable, "cwd": CodeForbidden}[name]
		if !asRPC(err, &rpc) || rpc.Code != want {
			t.Fatalf("%s: err = %v, want code %d", name, err, want)
		}
	}
}

func asRPC(err error, target **RPCError) bool {
	if err == nil {
		return false
	}
	rpc, ok := err.(*RPCError)
	if ok {
		*target = rpc
	}
	return ok
}

func TestACPAgentModelProxy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_m", "turn_1", "model")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	var messages []string
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if e.Type == AgentEventTypeAgentMessage {
			messages = append(messages, fmt.Sprint(eventJSON(t, e)["content"]))
		}
	}
	joined := strings.Join(messages, "\n")
	// allowed tools: forwarded with the node owner's credential and the upstream model; the
	// placeholder never leaves the machine. A catalog with an unknown tool is refused (403).
	if !strings.Contains(joined, "ok:200:upstream-ok") || !strings.Contains(joined, "evil:403") || !strings.Contains(joined, "badkey:403") {
		t.Fatalf("proxy results = %s", joined)
	}
	f.upstream.mu.Lock()
	defer f.upstream.mu.Unlock()
	if f.upstream.requests != 1 || f.upstream.auth[0] != "Bearer node-owner-key" || f.upstream.models[0] != "upstream-model" {
		t.Fatalf("upstream saw %d requests, auth=%v models=%v", f.upstream.requests, f.upstream.auth, f.upstream.models)
	}
	// Outside a running turn the placeholder is refused.
	if status := f.agent.proxyStatusForTest(t, "sesn_m"); status != http.StatusForbidden {
		t.Fatalf("request outside a turn: %d", status)
	}
}

func TestACPAgentTurnInputRunsInTheSameTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_i", "turn_1", "slow")
	if _, err := f.executor.SendTurnInput(ctx, TurnInputParams{SessionID: "sesn_i", TurnID: "turn_1", InputID: "in_more",
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	var n int
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if e.Type == AgentEventTypeAgentMessage {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("messages in the turn = %d, want 2 (slow + hello)", n)
	}
}

// --- fake ACP agent (runs in the re-executed test binary) ---

type fakeConn struct {
	mu       sync.Mutex
	out      *bufio.Writer
	nextID   int
	waiting  map[int]chan json.RawMessage
	cancel   map[string]chan struct{}
	models   map[string]map[string]any
	nextCall int
}

func (c *fakeConn) send(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(v)
	c.out.Write(append(b, '\n'))
	c.out.Flush()
}

func (c *fakeConn) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *fakeConn) request(method string, params any) json.RawMessage {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-ch
}

func (c *fakeConn) update(session string, update map[string]any) {
	c.notify("session/update", map[string]any{"sessionId": session, "update": update})
}

func (c *fakeConn) message(session, id, kind, text string) {
	c.notify("_bma/message", map[string]any{"sessionId": session, "messageId": id, "kind": kind, "text": text})
}

// toolResult is what _bma/tools/call answered.
type fakeToolResult struct {
	Text      string `json:"text"`
	IsError   bool   `json:"isError"`
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"errorCode"`
}

// toolFull announces the call like bma-dsh-acp does (tool_call, then
// _bma/tools/call) and returns the executor's whole answer.
func (c *fakeConn) toolFull(session, callID, name string, input map[string]any) fakeToolResult {
	c.update(session, map[string]any{"sessionUpdate": "tool_call", "toolCallId": callID, "title": name, "kind": "execute", "status": "pending", "rawInput": input,
		"_meta": map[string]any{"bma": map[string]any{"name": name, "permission": "allow"}}})
	return c.toolRaw(session, callID, name, input)
}

// toolRaw calls _bma/tools/call without announcing the call first.
func (c *fakeConn) toolRaw(session, callID, name string, input map[string]any) fakeToolResult {
	raw := c.request("_bma/tools/call", map[string]any{"sessionId": session, "toolCallId": callID, "name": name, "input": input})
	var r struct {
		Result fakeToolResult `json:"result"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.Result
}

func (c *fakeConn) tool(session, callID, name string, input map[string]any) (string, bool) {
	r := c.toolFull(session, callID, name, input)
	return r.Text, r.IsError
}

func fakeACPAgent() {
	c := &fakeConn{out: bufio.NewWriter(os.Stdout), waiting: map[int]chan json.RawMessage{}, cancel: map[string]chan struct{}{}, models: map[string]map[string]any{}}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	for in.Scan() {
		var m struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		line := append([]byte(nil), in.Bytes()...)
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m.Method == "" {
			var id int
			_ = json.Unmarshal(*m.ID, &id)
			c.mu.Lock()
			ch := c.waiting[id]
			delete(c.waiting, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- line
			}
			continue
		}
		if m.ID == nil {
			if m.Method == "session/cancel" {
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(m.Params, &p)
				c.mu.Lock()
				if ch := c.cancel[p.SessionID]; ch != nil {
					close(ch)
					delete(c.cancel, p.SessionID)
				}
				c.mu.Unlock()
			}
			continue
		}
		id := *m.ID
		go func() {
			result, rpcErr := fakeHandle(c, m.Method, m.Params)
			resp := map[string]any{"jsonrpc": "2.0", "id": id}
			if rpcErr != nil {
				resp["error"] = rpcErr
			} else {
				resp["result"] = result
			}
			c.send(resp)
		}()
	}
}

var fakeTools = []string{"bash", "read", "write", "edit", "glob", "grep"}

func fakeHandle(c *fakeConn, method string, raw json.RawMessage) (any, map[string]any) {
	var p struct {
		SessionID string            `json:"sessionId"`
		Prompt    []json.RawMessage `json:"prompt"`
		Meta      struct {
			Bma map[string]any `json:"bma"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true},
			"agentInfo": map[string]any{"name": "fake-dsh-acp", "version": "9.9.9"},
			"_meta":     map[string]any{"bma": map[string]any{"dsh": "0.2.1-rc.5", "tools": fakeTools, "streaming": "token", "permissionRelay": "full"}}}, nil
	case "session/new", "session/load":
		// BMA_ACP_FAKE_NEW_DELAY_MS makes opening a session slow, and
		// BMA_ACP_FAKE_NEW_ERROR (an ACP error code) makes it fail.
		if ms, _ := strconv.Atoi(os.Getenv("BMA_ACP_FAKE_NEW_DELAY_MS")); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		if code, _ := strconv.Atoi(os.Getenv("BMA_ACP_FAKE_NEW_ERROR")); code != 0 {
			return nil, map[string]any{"code": code, "message": "scripted failure"}
		}
		sid, _ := p.Meta.Bma["sessionId"].(string)
		if method == "session/load" {
			sid = p.SessionID
			if sid == "sesn_missing" {
				return nil, map[string]any{"code": -32002, "message": "Resource not found", "data": map[string]any{"sessionId": sid}}
			}
		}
		if p.Meta.Bma["systemPrompt"] == nil || !strings.Contains(p.Meta.Bma["systemPrompt"].(string), "Be brief.") {
			return nil, map[string]any{"code": -32602, "message": "system prompt missing the agent instructions"}
		}
		c.mu.Lock()
		c.models[sid], _ = p.Meta.Bma["model"].(map[string]any)
		c.mu.Unlock()
		return map[string]any{"sessionId": sid, "_meta": map[string]any{"bma": map[string]any{"toolCatalog": fakeTools}}}, nil
	case "session/set_mode", "session/close":
		return map[string]any{}, nil
	case "session/prompt":
		var block struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(p.Prompt[0], &block)
		if strings.HasPrefix(block.Text, "resume") {
			inputID, _ := p.Meta.Bma["inputId"].(string)
			policy, _ := p.Meta.Bma["toolPolicy"].(map[string]any)
			return fakeResume(c, p.SessionID, inputID, policy["bash"] == "allow", block.Text == "resume-amnesia")
		}
		return fakePrompt(c, p.SessionID, block.Text)
	}
	return nil, map[string]any{"code": -32601, "message": "method not found"}
}

func fakePrompt(c *fakeConn, sid, text string) (any, map[string]any) {
	endTurn := map[string]any{"stopReason": "end_turn"}
	canceled := make(chan struct{})
	c.mu.Lock()
	c.cancel[sid] = canceled
	c.mu.Unlock()
	switch text {
	case "hello":
		c.update(sid, map[string]any{"sessionUpdate": "agent_thought_chunk", "messageId": "t1", "content": map[string]any{"type": "text", "text": "hm"}})
		c.update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": "Hel"}})
		c.update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "m1", "content": map[string]any{"type": "text", "text": "lo"}})
		c.message(sid, "t1", "thinking", "hm")
		c.message(sid, "m1", "message", "Hello")
		return endTurn, nil
	case "slow":
		time.Sleep(300 * time.Millisecond)
		c.message(sid, "m-slow", "message", "slow done")
		return endTurn, nil
	case "run":
		c.mu.Lock()
		c.nextCall++
		callID := fmt.Sprintf("call_%d", c.nextCall)
		c.mu.Unlock()
		input := map[string]any{"command": "echo hi > out.txt && cat out.txt"}
		c.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": callID, "title": "bash", "kind": "execute", "status": "pending", "rawInput": input,
			"_meta": map[string]any{"bma": map[string]any{"name": "bash", "permission": "ask"}}})
		answer := make(chan json.RawMessage, 1)
		go func() {
			answer <- c.request("session/request_permission", map[string]any{"sessionId": sid,
				"toolCall": map[string]any{"toolCallId": callID, "title": "bash", "rawInput": input},
				"options":  []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}})
		}()
		var raw json.RawMessage
		select {
		case raw = <-answer:
		case <-canceled:
			return map[string]any{"stopReason": "cancelled"}, nil
		}
		var r struct {
			Result struct {
				Outcome struct {
					Outcome  string `json:"outcome"`
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			} `json:"result"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.Result.Outcome.OptionID != "allow" {
			c.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": callID, "status": "failed",
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "refused by the user"}}}})
			c.message(sid, "m-deny", "message", "ok, not running it")
			return endTurn, nil
		}
		out, isErr := c.tool(sid, callID, "bash", input)
		status := "completed"
		if isErr {
			status = "failed"
		}
		c.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": callID, "status": status,
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": out}}}})
		c.message(sid, "m2", "message", "done")
		return endTurn, nil
	case "bypass", "bypass-swap", "ask-ignore":
		// A brain that does not honor the permission relay: it calls the tool
		// without asking (bypass), asks for one input and runs another
		// (bypass-swap), or runs the tool whatever the platform answered (ask-ignore).
		input := map[string]any{"command": "echo pwned > pwned.txt"}
		if text != "bypass" {
			asked := input
			if text == "bypass-swap" {
				asked = map[string]any{"command": "echo harmless"}
			}
			c.update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call_b", "title": "bash", "kind": "execute", "status": "pending", "rawInput": asked,
				"_meta": map[string]any{"bma": map[string]any{"name": "bash", "permission": "ask"}}})
			answer := make(chan json.RawMessage, 1)
			go func() {
				answer <- c.request("session/request_permission", map[string]any{"sessionId": sid,
					"toolCall": map[string]any{"toolCallId": "call_b", "title": "bash", "rawInput": asked},
					"options":  []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}})
			}()
			select {
			case <-answer:
			case <-canceled:
				return map[string]any{"stopReason": "cancelled"}, nil
			}
		}
		r := c.toolFull(sid, "call_b", "bash", input)
		status := "completed"
		if r.IsError {
			status = "failed"
		}
		c.update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call_b", "status": status,
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": r.Text}}}})
		c.message(sid, "m-"+text, "message", fmt.Sprintf("%s|%v|%s|%s", r.Text, r.IsError, r.Outcome, r.ErrorCode))
		return endTurn, nil
	case "late":
		// Ends the turn at once; calls a tool later, when the turn is over.
		home := os.Getenv("BMA_DSH_ACP_HOME")
		go func() {
			for {
				if _, err := os.Stat(filepath.Join(home, "go")); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			r := c.toolFull(sid, "call_late", "bash", map[string]any{"command": "echo late > late.txt"})
			b, _ := json.Marshal(r)
			_ = os.WriteFile(filepath.Join(home, "late.json"), b, 0o600)
		}()
		return endTurn, nil
	case "files":
		say := func(label, text string, isErr bool) {
			if isErr {
				text = "is_error " + text
			}
			c.message(sid, "m-"+label, "message", label+":"+text)
		}
		steps := []struct {
			label, tool string
			input       map[string]any
		}{
			{"write", "write", map[string]any{"file_path": "a.txt", "content": "A A\n"}},
			{"edit", "edit", map[string]any{"file_path": "a.txt", "old_string": "A A", "new_string": "X A"}},
			{"replace_all", "edit", map[string]any{"file_path": "a.txt", "old_string": "A", "new_string": "B", "replace_all": true}},
			{"read", "read", map[string]any{"file_path": "a.txt"}},
			{"grep", "grep", map[string]any{"pattern": "x b", "case_insensitive": true}},
			{"count", "grep", map[string]any{"pattern": "B", "output_mode": "count"}},
			{"glob", "glob", map[string]any{"pattern": "*.txt"}},
			{"outside", "read", map[string]any{"file_path": "/etc/hosts"}},
			{"unknown", "rm_rf", map[string]any{}},
		}
		for i, s := range steps {
			out, isErr := c.tool(sid, fmt.Sprintf("call_f%d", i), s.tool, s.input)
			say(s.label, out, isErr)
		}
		return endTurn, nil
	case "long":
		return map[string]any{"stopReason": "max_tokens", "_meta": map[string]any{"bma": map[string]any{"stopCode": "output_limit_reached", "detail": "cut at 8192"}}}, nil
	case "idle":
		return nil, map[string]any{"code": -32603, "message": "model_stream_idle: no data for 90s", "data": map[string]any{"bma": map[string]any{"stopCode": "model_stream_idle", "detail": "no data for 90s"}}}
	case "deaf":
		select {} // ignores session/cancel
	case "crash":
		os.Exit(3)
	case "whoami":
		state := "writable"
		if err := os.WriteFile(filepath.Join(os.Getenv("BMA_DSH_ACP_HOME"), "probe"), nil, 0o600); err != nil {
			state = err.Error()
		}
		c.message(sid, "m-who", "message", fmt.Sprintf("uid=%d home=%s user=%s state=%s", os.Getuid(), os.Getenv("HOME"), os.Getenv("USER"), state))
		return endTurn, nil
	case "model", "model-twice":
		if text == "model-twice" {
			c.mu.Lock()
			model := c.models[sid]
			c.mu.Unlock()
			for i := range 2 {
				body := `{"model":"m","stream":true,"messages":[],"tools":[{"type":"function","function":{"name":"bash"}}]}`
				req, _ := http.NewRequest("POST", model["baseUrl"].(string)+"/chat/completions", strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+model["token"].(string))
				status := "error"
				if resp, err := http.DefaultClient.Do(req); err == nil {
					status = fmt.Sprint(resp.StatusCode)
					resp.Body.Close()
				}
				c.message(sid, fmt.Sprintf("m-%d", i), "message", fmt.Sprintf("request %d: %s", i, status))
			}
			return endTurn, nil
		}
		c.mu.Lock()
		model := c.models[sid]
		c.mu.Unlock()
		post := func(token string, tools []string) string {
			var defs []any
			for _, t := range tools {
				defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t}})
			}
			body, _ := json.Marshal(map[string]any{"model": model["model"], "stream": true, "messages": []any{}, "tools": defs})
			req, _ := http.NewRequest("POST", model["baseUrl"].(string)+"/chat/completions", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "error"
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Sprint(resp.StatusCode)
			}
			return fmt.Sprintf("200:%s", map[bool]string{true: "upstream-ok", false: string(b)}[strings.Contains(string(b), "upstream-ok")])
		}
		token := model["token"].(string)
		c.message(sid, "m-ok", "message", "ok:"+post(token, []string{"bash", "read"}))
		c.message(sid, "m-evil", "message", "evil:"+post(token, []string{"bash", "evil"}))
		c.message(sid, "m-bad", "message", "badkey:"+post("wrong", []string{"bash"}))
		return endTurn, nil
	}
	return endTurn, nil
}

// proxyStatusForTest posts to the model proxy with a session's placeholder.
func (a *ACPAgent) proxyStatusForTest(t *testing.T, sessionID string) int {
	t.Helper()
	a.mu.Lock()
	key := a.sessions[sessionID].placeholder
	a.mu.Unlock()
	req, _ := http.NewRequest("POST", a.proxy.url+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestACPAgentWithBmaDshAcp drives the real bma-dsh-acp (BMA repository,
// brain-host/packages/dsh-acp) when BMA_DSH_ACP_BIN names a build of it
// (the SEA binary, or the .mjs bundle run with node). The model is a
// scripted OpenAI-compatible upstream: a bash call, then a final text.
func TestACPAgentWithBmaDshAcp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	bin := os.Getenv("BMA_DSH_ACP_BIN")
	if bin == "" {
		t.Skip("BMA_DSH_ACP_BIN is not set")
	}
	command := []string{bin}
	if strings.HasSuffix(bin, ".mjs") {
		node, err := exec.LookPath("node") // the test resolves it; ACPAgent itself never looks on PATH
		if err != nil {
			t.Skip("node not found for the .mjs bundle")
		}
		command = []string{node, bin}
	}
	var mu sync.Mutex
	var tools [][]string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var names []string
		for _, tool := range body.Tools {
			names = append(names, tool.Function.Name)
		}
		mu.Lock()
		tools = append(tools, names)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any, finish any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if body.Messages[len(body.Messages)-1].Role == "tool" {
			send(map[string]any{"content": "done: "}, nil)
			send(map[string]any{"content": "wrote r.txt"}, nil)
			send(map[string]any{}, "stop")
		} else {
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_real_1", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": `{"command":"echo real > r.txt && cat r.txt"}`}}}}, nil)
			send(map[string]any{}, "tool_calls")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer model.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "scripted"}})
	capability, _ := f.executor.AgentCapability()
	if h := capability.Harnesses[0]; h.AgentName != "bma-dsh-acp" || h.Streaming != "token" || h.PermissionRelay != "full" {
		t.Fatalf("harness = %+v", h)
	}
	f.start(t, ctx, "sesn_real", "turn_1", "write r.txt")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	if request.InteractionID != ApprovalID("sesn_real", "turn_1", "call_real_1") || request.ToolInput["command"] != "echo real > r.txt && cat r.txt" {
		t.Fatalf("interaction.request = %+v", request)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_real", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	got := strings.Join(eventTypes(eventsOf(f.platform.snapshot(), "turn_1")), ",")
	if got != "session.status_running,agent.tool_use,agent.tool_result,agent.message,session.status_idle" {
		t.Fatalf("events = %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_real", "r.txt")); err != nil || strings.TrimSpace(string(b)) != "real" {
		t.Fatalf("r.txt = %q %v", b, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, names := range tools {
		sort.Strings(names)
		if strings.Join(names, ",") != "bash,edit,glob,grep,read,write" {
			t.Fatalf("model saw tools %v", names)
		}
	}
	f.platform.mu.Lock()
	deltas := len(f.platform.deltas)
	f.platform.mu.Unlock()
	if deltas == 0 {
		t.Fatal("no token deltas reached the platform")
	}
}

func TestACPAgentOnInteractionReportsTheSameApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	var seen []ACPInteractionEvent
	got := func() []ACPInteractionEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]ACPInteractionEvent(nil), seen...)
	}
	f := startACPFixture(t, ctx, ACPAgentOptions{OnInteraction: func(e ACPInteractionEvent) { mu.Lock(); seen = append(seen, e); mu.Unlock() }})
	f.start(t, ctx, "sesn_o", "turn_1", "run")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	id := f.platform.interactions[0].InteractionID
	if e := got(); len(e) != 1 || e[0].Phase != "requested" || e[0].SessionID != "sesn_o" || e[0].InteractionID != id || e[0].ToolName != "bash" || e[0].ToolInput["command"] != "echo hi > out.txt && cat out.txt" {
		t.Fatalf("requested = %+v, platform id %s", e, id)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_o", InteractionID: id, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if e := got(); len(e) != 2 || e[1].Phase != "ended" || e[1].InteractionID != id || e[1].Outcome != InteractionOutcomeAllow {
		t.Fatalf("ended = %+v", e)
	}

	// A canceled turn withdraws the card too.
	f.start(t, ctx, "sesn_o", "turn_2", "run", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_o" })
	f.platform.waitFor(t, "second permission", func() bool { return len(f.platform.interactions) == 2 })
	if _, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_o", TurnID: "turn_2"}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCanceled)
	if e := got(); len(e) != 4 || e[3].Phase != "ended" || e[3].Outcome != "withdrawn" || e[3].InteractionID != f.platform.interactions[1].InteractionID {
		t.Fatalf("withdrawn = %+v", e)
	}
}

// The id is appr_ + sha256(session NUL turn NUL call)[:24] (ADR-0027, DSH S3): shared vectors with the
// TypeScript adapter (brain-host dsh-adapter events-approval.test.ts).
func TestApprovalIDVectors(t *testing.T) {
	if got := ApprovalID("sesn_t", "turn_1", "call_w"); got != "appr_4eadd9a6cda5f6ed52115828" {
		t.Fatalf("pinned vector = %s", got)
	}
	if got := ApprovalID("sesn_t", "turn_2", "call_w"); got != "appr_2cd3b7c0bd1f82076e65801e" {
		t.Fatalf("pinned vector turn_2 = %s", got)
	}
	// A provider that reuses call_0 in every turn must not collide within a session.
	if ApprovalID("sesn_t", "turn_1", "call_0") == ApprovalID("sesn_t", "turn_2", "call_0") {
		t.Fatal("two turns of one session share an id")
	}
	// Same turn after a restart: same id.
	if ApprovalID("sesn_t", "turn_1", "call_0") != ApprovalID("sesn_t", "turn_1", "call_0") {
		t.Fatal("the id is not deterministic")
	}
	if ApprovalID("sesn_t", "turn_1c", "all_w") == ApprovalID("sesn_t", "turn_1", "call_w") {
		t.Fatal("field boundaries can be shifted")
	}
}
