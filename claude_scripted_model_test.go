//go:build !windows

package bmaexecutor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The Claude adapter is tested against the real, pinned claude CLI with no
// Anthropic credential: its model endpoint is this scripted Anthropic
// Messages API on loopback, and the "API key" is a made-up value.

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
	Model  string
	Tools  []string
	APIKey string
	// Texts are the text blocks of the request's user messages.
	Texts []string
	// Images counts the image blocks of its user messages.
	Images int
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
	req := modelRequest{Model: body.Model, APIKey: r.Header.Get("X-Api-Key")}
	for _, tool := range body.Tools {
		req.Tools = append(req.Tools, tool.Name)
	}
	if os.Getenv("BMA_DEBUG_MODEL") != "" && len(body.Messages) > 0 {
		fmt.Fprintf(os.Stderr, "MODEL n=%d last=%.300s\n", len(body.Messages), body.Messages[len(body.Messages)-1])
	}
	for _, raw := range body.Messages {
		var msg struct {
			Role    string
			Content json.RawMessage
		}
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(raw, &msg) == nil && msg.Role == "user" && json.Unmarshal(msg.Content, &blocks) == nil {
			for _, b := range blocks {
				switch b.Type {
				case "text":
					req.Texts = append(req.Texts, b.Text)
				case "image":
					req.Images++
				}
			}
		}
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

func claudeEvents(t *testing.T, f *acpFixture, turn string) []string {
	t.Helper()
	return eventTypes(eventsOf(f.platform.snapshot(), turn))
}

// sawPromptAfter reports whether a request after the first `after` ones carried a user text starting with prefix.
func (m *scriptedModel) sawPromptAfter(after int, prefix string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests[min(after, len(m.requests)):] {
		for _, text := range r.Texts {
			if strings.HasPrefix(text, prefix) {
				return true
			}
		}
	}
	return false
}
