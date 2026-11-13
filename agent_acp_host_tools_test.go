package bmaexecutor

import (
	"context"
	"encoding/json"
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

// Host tools (agent_acp_host_tools.go) against the real bma-dsh-acp
// (BMA_DSH_ACP_BIN, as TestACPAgentWithBmaDshAcp): ask_user_question is a
// custom tool call answered by the platform in the same turn; computer_observe
// is approved on the platform, runs in Core's ComputerUseBackend and its
// screenshot reaches the model.

const hostToolPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

type fakeComputer struct {
	mu      sync.Mutex
	actions []ComputerActionRequest
}

func (c *fakeComputer) Observe(context.Context, ComputerObserveRequest) ([]ContentBlock, error) {
	return []ContentBlock{{Type: ContentBlockTypeImage, MimeType: "image/png", Data: hostToolPNG}, {Type: ContentBlockTypeText, Text: "screen screenshot 1x1"}}, nil
}

func (c *fakeComputer) Action(_ context.Context, req ComputerActionRequest) ([]ContentBlock, error) {
	c.mu.Lock()
	c.actions = append(c.actions, req)
	c.mu.Unlock()
	return []ContentBlock{{Type: ContentBlockTypeText, Text: "clicked"}}, nil
}

// scriptedToolModel answers the first request of a turn with one call of
// name(args) and the next with "done: <tool result>"; it records each
// request's tool names and whether it carried an image.
type scriptedToolModel struct {
	mu     sync.Mutex
	name   string
	args   string
	tools  [][]string
	images int
	server *httptest.Server
}

func newScriptedToolModel(t *testing.T) *scriptedToolModel {
	m := &scriptedToolModel{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
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
		sort.Strings(names)
		images := strings.Count(string(rawJSON(body.Messages)), "data:image/")
		if os.Getenv("DEBUG_HOST") != "" {
			t.Logf("model request: %s", rawJSON(body.Messages))
		}
		m.mu.Lock()
		m.tools = append(m.tools, names)
		m.images += images
		name, args := m.name, m.args
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta map[string]any, finish any) {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		var toolText string
		for _, msg := range body.Messages {
			if msg.Role == "tool" {
				_ = json.Unmarshal(msg.Content, &toolText)
			}
		}
		if last := body.Messages[len(body.Messages)-1]; last.Role == "tool" || (last.Role == "user" && toolText != "") {
			send(map[string]any{"content": "done: " + toolText}, nil)
			send(map[string]any{}, "stop")
		} else {
			send(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_h1", "type": "function",
				"function": map[string]any{"name": name, "arguments": args}}}}, nil)
			send(map[string]any{}, "tool_calls")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.server.Close)
	return m
}

func rawJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func bmaDshAcpCommand(t *testing.T) []string {
	t.Helper()
	bin := os.Getenv("BMA_DSH_ACP_BIN")
	if bin == "" {
		t.Skip("BMA_DSH_ACP_BIN is not set")
	}
	if !strings.HasSuffix(bin, ".mjs") {
		return []string{bin}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found for the .mjs bundle")
	}
	return []string{node, bin}
}

func TestACPAgentAskUserQuestionIsACustomToolCallAnsweredInTheSameTurn(t *testing.T) {
	command := bmaDshAcpCommand(t)
	model := newScriptedToolModel(t)
	model.name, model.args = ToolNameAskUserQuestion, `{"questions":[{"question":"Which format?","header":"Format","options":[{"label":"Markdown"},{"label":"PDF"}]}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var seen []ACPInteractionEvent
	var seenMu sync.Mutex
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, AskUserQuestion: true, Upstream: ACPModelUpstream{BaseURL: model.server.URL + "/v1", Model: "scripted"},
		OnInteraction: func(e ACPInteractionEvent) { seenMu.Lock(); seen = append(seen, e); seenMu.Unlock() }})
	capability, _ := f.executor.AgentCapability()
	if !capability.CustomTools {
		t.Fatalf("capability.custom_tools = false: %+v", capability)
	}
	f.start(t, ctx, "sesn_q", "turn_1", "make a report")
	var use AgentEventRecord
	f.platform.waitFor(t, "agent.custom_tool_use", func() bool {
		for _, e := range f.platform.events {
			if eventJSON(t, e)["type"] == AgentEventTypeAgentCustomToolUse {
				use = e
				return true
			}
		}
		return false
	})
	if use.EventID != askUserID("sesn_q", "turn_1", "call_h1") {
		t.Fatalf("custom tool use id = %s", use.EventID)
	}
	body := eventJSON(t, use)
	if body["name"] != ToolNameAskUserQuestion || !strings.Contains(fmt.Sprint(body["input"]), "Which format?") {
		t.Fatalf("custom tool use = %v", body)
	}
	if len(f.platform.interactions) != 0 {
		t.Fatalf("a question must not ask for a permission: %+v", f.platform.interactions)
	}
	result, err := f.executor.RespondCustomToolResult(ctx, AgentCustomToolResultParams{SessionID: "sesn_q", ToolUseID: use.EventID,
		Content: []ContentBlock{{Type: ContentBlockTypeText, Text: `"Which format?"="Markdown"`}}})
	if err != nil || !result.Accepted {
		t.Fatalf("custom tool result: %+v %v", result, err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	events := eventsOf(f.platform.snapshot(), "turn_1")
	got := strings.Join(eventTypes(events), ",")
	if got != "session.status_running,agent.custom_tool_use,agent.message,session.status_idle" {
		t.Fatalf("events = %s", got)
	}
	if text := fmt.Sprint(eventJSON(t, events[2])["content"]); !strings.Contains(text, `The user answered: "Which format?"="Markdown"`) {
		t.Fatalf("message = %s", text)
	}
	model.mu.Lock()
	tools := strings.Join(model.tools[0], ",")
	model.mu.Unlock()
	if tools != "ask_user_question,bash,edit,glob,grep,read,write" {
		t.Fatalf("model saw tools %s", tools)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 || seen[0].Kind != InteractionKindQuestion || seen[0].InteractionID != use.EventID || seen[1].Phase != "ended" || seen[1].Outcome != "answered" {
		t.Fatalf("OnInteraction = %+v", seen)
	}
}

func TestACPAgentAskUserQuestionCanceledWithTheTurn(t *testing.T) {
	command := bmaDshAcpCommand(t)
	model := newScriptedToolModel(t)
	model.name, model.args = ToolNameAskUserQuestion, `{"questions":[{"question":"Go on?","options":[{"label":"Yes"},{"label":"No"}]}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, AskUserQuestion: true, Upstream: ACPModelUpstream{BaseURL: model.server.URL + "/v1", Model: "scripted"}})
	f.start(t, ctx, "sesn_qc", "turn_1", "ask")
	id := askUserID("sesn_qc", "turn_1", "call_h1")
	f.platform.waitFor(t, "agent.custom_tool_use", func() bool { return hasEvent(f.platform.events, id) })
	if _, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_qc", TurnID: "turn_1"}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	var gaveUp bool
	for _, e := range eventsOf(f.platform.snapshot(), "turn_1") {
		if m := eventJSON(t, e); m["type"] == AgentEventTypeAgentToolResult && m["tool_use_id"] == id && m["is_error"] == true {
			gaveUp = true
		}
	}
	if !gaveUp {
		t.Fatalf("no failed agent.tool_result gave up the question: %v", eventTypes(eventsOf(f.platform.snapshot(), "turn_1")))
	}
	if result, err := f.executor.RespondCustomToolResult(ctx, AgentCustomToolResultParams{SessionID: "sesn_qc", ToolUseID: id,
		Content: []ContentBlock{{Type: ContentBlockTypeText, Text: "Yes"}}}); err != nil || result.Accepted {
		t.Fatalf("a late answer must not be accepted: %+v %v", result, err)
	}
}

// approvingComputer is a fakeComputer whose host decides what needs approval.
type approvingComputer struct {
	fakeComputer
	needs    bool
	sessions []string
}

func (c *approvingComputer) NeedsObserveApproval(_ context.Context, req ComputerObserveRequest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = append(c.sessions, req.SessionID)
	return c.needs
}

func (c *approvingComputer) NeedsActionApproval(context.Context, ComputerActionRequest) bool {
	return c.needs
}

// A ComputerUseApprover decides only while the ask is the tools' declared
// default: the platform's tool_policy or the host's ceiling naming the tool
// brings the approval back.
func TestACPAgentComputerUseApprover(t *testing.T) {
	command := bmaDshAcpCommand(t)
	for name, c := range map[string]struct {
		needs    bool
		policy   string
		ceiling  map[string]string
		wantAsks bool
	}{
		"host lets it run":      {},
		"host asks":             {needs: true, wantAsks: true},
		"platform names it":     {policy: `{"computer_observe":"ask"}`, wantAsks: true},
		"host ceiling names it": {ceiling: map[string]string{ToolNameComputerObserve: "ask"}, wantAsks: true},
	} {
		t.Run(name, func(t *testing.T) {
			model := newScriptedToolModel(t)
			model.name, model.args = ToolNameComputerObserve, `{}`
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			computer := &approvingComputer{needs: c.needs}
			core, err := NewCore(CoreOptions{WorkRoot: filepath.Join(t.TempDir(), "work"), ComputerUse: computer})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(core.Close)
			f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, Core: core, ToolPolicy: c.ceiling, Upstream: ACPModelUpstream{BaseURL: model.server.URL + "/v1", Model: "scripted"}})
			f.start(t, ctx, "sesn_ca", "turn_1", "look at the screen", func(p *TurnStartParams) {
				if c.policy != "" {
					p.Metadata = map[string]string{"tool_policy": c.policy}
				}
			})
			if c.wantAsks {
				f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
				if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_ca", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
					t.Fatal(err)
				}
			}
			assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
			if !c.wantAsks && len(f.platform.interactions) != 0 {
				t.Fatalf("asked %d times, want none", len(f.platform.interactions))
			}
			events := eventsOf(f.platform.snapshot(), "turn_1")
			if got := strings.Join(eventTypes(events), ","); got != "session.status_running,agent.tool_use,agent.tool_result,agent.message,session.status_idle" {
				t.Fatalf("events = %s", got)
			}
			want := "allow"
			if c.wantAsks {
				want = "ask"
			}
			if got := fmt.Sprint(eventJSON(t, events[1])["evaluated_permission"]); got != "map[type:"+want+"]" {
				t.Fatalf("evaluated_permission = %s, want %s", got, want)
			}
			if !c.wantAsks && (len(computer.sessions) != 1 || computer.sessions[0] != "sesn_ca") {
				t.Fatalf("approver saw sessions %v", computer.sessions)
			}
		})
	}
}

func TestACPAgentComputerUseIsAHostToolApprovedOnThePlatform(t *testing.T) {
	command := bmaDshAcpCommand(t)
	model := newScriptedToolModel(t)
	model.name, model.args = ToolNameComputerObserve, `{}`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	computer := &fakeComputer{}
	core, err := NewCore(CoreOptions{WorkRoot: filepath.Join(t.TempDir(), "work"), ComputerUse: computer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, Core: core, AskUserQuestion: true, Upstream: ACPModelUpstream{BaseURL: model.server.URL + "/v1", Model: "scripted"}})
	f.start(t, ctx, "sesn_cu", "turn_1", "look at the screen")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	if request.ToolName != ToolNameComputerObserve || request.InteractionID != ApprovalID("sesn_cu", "turn_1", "call_h1") {
		t.Fatalf("interaction.request = %+v", request)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_cu", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	events := eventsOf(f.platform.snapshot(), "turn_1")
	if got := strings.Join(eventTypes(events), ","); got != "session.status_running,agent.tool_use,agent.tool_result,agent.message,session.status_idle" {
		t.Fatalf("events = %s", got)
	}
	if use := eventJSON(t, events[1]); use["name"] != ToolNameComputerObserve {
		t.Fatalf("tool_use = %v", use)
	}
	if text := fmt.Sprint(eventJSON(t, events[2])["content"]); !strings.Contains(text, "screen screenshot 1x1") {
		t.Fatalf("tool_result = %s", text)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.images != 1 {
		t.Fatalf("the screenshot reached the model %d times, want 1", model.images)
	}
	if tools := strings.Join(model.tools[0], ","); tools != "ask_user_question,bash,computer_action,computer_observe,edit,glob,grep,read,write" {
		t.Fatalf("model saw tools %s", tools)
	}
}
