//go:build !windows

package bmaexecutor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The Codex adapter is tested against the real, pinned codex CLI with no
// OpenAI credential: its model provider is this scripted OpenAI Responses API
// on loopback (a provider of the owner's own, codexOwnerSettings), and the
// "API key" is a made-up value.

// scriptedResponses asks for one call of tool per "run it" prompt, then
// answers "done" once it has the tool's output.
type scriptedResponses struct {
	server *httptest.Server
	tool   string
	input  map[string]any

	mu       sync.Mutex
	requests []responsesRequest
	toolUses int
}

type responsesRequest struct {
	Model  string
	Effort string
	Tools  []string
	APIKey string
	// Texts are the input_text parts of the request's user messages.
	Texts []string
	// Images counts the input_image parts of its user messages.
	Images int
}

func newScriptedResponses(t *testing.T, tool string, input map[string]any) *scriptedResponses {
	m := &scriptedResponses{tool: tool, input: input}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func (m *scriptedResponses) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"models":[]}`))
		return
	}
	var body struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools []codexWireTool `json:"tools"`
		Input []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Tools   []codexWireTool `json:"tools"`
		} `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	req := responsesRequest{Model: body.Model, Effort: body.Reasoning.Effort, APIKey: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	// The tools come in the request's tools, or (for a model of codex's
	// catalog) in an additional_tools input item, namespaced.
	req.Tools = codexToolNames(body.Tools)
	for _, item := range body.Input {
		if item.Type == "additional_tools" {
			req.Tools = append(req.Tools, codexToolNames(item.Tools)...)
		}
	}
	prompts := 0
	for _, item := range body.Input {
		var parts []struct{ Type, Text string }
		if item.Type != "message" || item.Role != "user" || json.Unmarshal(item.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			switch part.Type {
			case "input_text":
				req.Texts = append(req.Texts, part.Text)
				if strings.HasPrefix(part.Text, "run it") {
					prompts++
				}
			case "input_image":
				req.Images++
			}
		}
	}
	if os.Getenv("BMA_DEBUG_MODEL") != "" {
		fmt.Fprintf(os.Stderr, "MODEL model=%s tools=%v texts=%q\n", req.Model, req.Tools, req.Texts)
	}
	m.mu.Lock()
	tool := m.tool
	offered := slices.Contains(req.Tools, tool)
	m.requests = append(m.requests, req)
	needTool := offered && prompts > m.toolUses
	if needTool {
		m.toolUses++
	}
	n, uses := len(m.requests), m.toolUses
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, data map[string]any) {
		data["type"] = kind
		raw, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw)
	}
	id := fmt.Sprintf("resp_%d", n)
	event("response.created", map[string]any{"response": map[string]any{"id": id}})
	if needTool {
		args, _ := json.Marshal(m.input)
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", n), "call_id": fmt.Sprintf("call_%02d", uses), "name": tool, "arguments": string(args)}
		event("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	} else {
		msg := fmt.Sprintf("msg_%d", n)
		event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "id": msg, "content": []any{}}})
		event("response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": msg, "delta": "done"})
		event("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "id": msg,
			"content": []any{map[string]any{"type": "output_text", "text": "done"}}}})
	}
	event("response.completed", map[string]any{"response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
		"input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}})
}

type codexWireTool struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Tools []codexWireTool `json:"tools"`
}

// codexToolNames are the names of the tools of a request; a namespace's
// tools by their own names, except that of codex's default namespace
// (functions) as a qualified name only when not the default.
func codexToolNames(tools []codexWireTool) []string {
	var names []string
	for _, t := range tools {
		switch {
		case t.Type == "namespace" && t.Name == "functions":
			names = append(names, codexToolNames(t.Tools)...)
		case t.Type == "namespace":
			for _, n := range codexToolNames(t.Tools) {
				names = append(names, t.Name+"."+n)
			}
		case t.Name != "":
			names = append(names, t.Name)
		default:
			names = append(names, t.Type)
		}
	}
	return names
}

// offeredTools are the tool names of every request.
func (m *scriptedResponses) offeredTools() []string {
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

func (m *scriptedResponses) sawPromptAfter(after int, prefix string) bool {
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

func (m *scriptedResponses) setTool(tool string) {
	m.mu.Lock()
	m.tool = tool
	m.mu.Unlock()
}

func (m *scriptedResponses) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

const codexTestAPIKey = "sk-test-made-up-key"

// codexProviderSettings point the CLI at a scripted Responses endpoint, as an
// owner points it at their own provider.
func codexProviderSettings(baseURL string) []string {
	return []string{"-c", `model_provider="bma-test"`, "-c", `model_providers.bma-test.name="bma-test"`,
		"-c", `model_providers.bma-test.base_url="` + baseURL + `/v1"`, "-c", `model_providers.bma-test.wire_api="responses"`,
		"-c", `model_providers.bma-test.env_key="BMA_CODEX_TEST_KEY"`}
}
