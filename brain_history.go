package bmaexecutor

// A session's past (session.history) in the shape every brain's transcript
// has: user and assistant text, tool calls and their results. A brain with
// BrainCapabilities.HistoryRebuild writes these into its own transcript
// format before it opens the new conversation.

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Kinds of historyItem.
const (
	historyUser      = "user"
	historyAssistant = "assistant"
	historyCall      = "call"
	historyResult    = "result"
)

// The platform's event types of session.history that the agent channel never sends.
const (
	historyUserMessage      = "user.message"
	historyCustomToolResult = "user.custom_tool_result"
)

// historyItem is one part of a past conversation.
type historyItem struct {
	Kind string
	// Text of a user or assistant message, or of a result.
	Text string
	// CallID names the call (the id of its event); a result names its call.
	CallID  string
	Name    string
	Input   map[string]any
	IsError bool
}

// historyNoResult is the result of a call the log has none for.
const historyNoResult = "[no result was recorded for this call]"

// historyItems is the conversation of events, in order, as a brain replays
// it: every call answered by exactly one result right after its step (a
// call the log has no result for gets an error result, a result of no
// earlier call is left out), since a model refuses a transcript with an
// unanswered call.
func historyItems(events []SessionHistoryEvent) []historyItem {
	var out []historyItem
	var open []string
	known := map[string]bool{}
	closeOpen := func() {
		for _, id := range open {
			out = append(out, historyItem{Kind: historyResult, CallID: id, Text: historyNoResult, IsError: true})
		}
		open = nil
	}
	for _, e := range events {
		p := e.Payload
		switch e.Type {
		case historyUserMessage:
			closeOpen()
			out = append(out, historyItem{Kind: historyUser, Text: historyText(p["content"])})
		case AgentEventTypeAgentMessage:
			closeOpen()
			out = append(out, historyItem{Kind: historyAssistant, Text: historyText(p["content"])})
		case AgentEventTypeAgentToolUse, AgentEventTypeAgentCustomToolUse, AgentEventTypeAgentMcpToolUse:
			name, _ := p["name"].(string)
			input, _ := p["input"].(map[string]any)
			if input == nil {
				input = map[string]any{}
			}
			out = append(out, historyItem{Kind: historyCall, CallID: e.ID, Name: name, Input: input})
			open, known[e.ID] = append(open, e.ID), true
		case AgentEventTypeAgentToolResult, historyCustomToolResult, AgentEventTypeAgentMcpToolResult:
			id := historyResultOf(p)
			i := indexOf(open, id)
			if !known[id] || i < 0 {
				continue
			}
			open = append(open[:i], open[i+1:]...)
			isError, _ := p["is_error"].(bool)
			out = append(out, historyItem{Kind: historyResult, CallID: id, Text: historyText(p["content"]), IsError: isError})
		}
	}
	closeOpen()
	return out
}

// historyResultOf is the call a result names.
func historyResultOf(p map[string]any) string {
	for _, key := range []string{"tool_use_id", "custom_tool_use_id", "mcp_tool_use_id"} {
		if id, _ := p[key].(string); id != "" {
			return id
		}
	}
	return ""
}

func indexOf(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// historyText is the text of content blocks: text blocks as they are, every
// other block named (its bytes are not carried over), as the platform's Go
// brain rebuilds them.
func historyText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			m, _ := b.(map[string]any)
			if m["type"] == "text" {
				text, _ := m["text"].(string)
				parts = append(parts, text)
				continue
			}
			parts = append(parts, fmt.Sprintf("[%v block from an earlier turn, not carried over]", m["type"]))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// historyArguments is a call's input as JSON text.
func historyArguments(input map[string]any) string {
	raw, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// newUUIDv7 is a random UUID version 7 (RFC 9562): the thread and message ids
// brains give their own conversations.
func newUUIDv7() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
