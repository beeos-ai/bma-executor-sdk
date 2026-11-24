//go:build !windows

package bmaexecutor

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pastOfFork is a session's past as session.history answers it: a message,
// a tool call and its result, the answer, and a call the log has no result
// for (a turn canceled during it).
var pastOfFork = []SessionHistoryEvent{
	{BmaSequence: 1, ID: "sevt_u1", Type: "user.message", Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "remember 42"}, map[string]any{"type": "image"}}}},
	{BmaSequence: 2, ID: "sevt_c1", Type: "agent.tool_use", Payload: map[string]any{"name": "bash", "input": map[string]any{"command": "echo 42"}}},
	{BmaSequence: 3, ID: "sevt_r1", Type: "agent.tool_result", Payload: map[string]any{"tool_use_id": "sevt_c1", "content": []any{map[string]any{"type": "text", "text": "OUT42"}}}},
	{BmaSequence: 4, ID: "sevt_a1", Type: "agent.message", Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "noted 42"}}}},
	{BmaSequence: 5, ID: "sevt_u2", Type: "user.message", Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "run nothing"}}}},
	{BmaSequence: 6, ID: "sevt_c2", Type: "agent.tool_use", Payload: map[string]any{"name": "bash", "input": map[string]any{"command": "sleep 9"}}},
	{BmaSequence: 7, ID: "sevt_r9", Type: "agent.tool_result", Payload: map[string]any{"tool_use_id": "sevt_gone", "content": []any{}}},
}

func TestHistoryItemsAnswerEveryCallOnce(t *testing.T) {
	got := historyItems(pastOfFork)
	want := []historyItem{
		{Kind: historyUser, Text: "remember 42\n[image block from an earlier turn, not carried over]"},
		{Kind: historyCall, CallID: "sevt_c1", Name: "bash", Input: map[string]any{"command": "echo 42"}},
		{Kind: historyResult, CallID: "sevt_c1", Text: "OUT42"},
		{Kind: historyAssistant, Text: "noted 42"},
		{Kind: historyUser, Text: "run nothing"},
		{Kind: historyCall, CallID: "sevt_c2", Name: "bash", Input: map[string]any{"command": "sleep 9"}},
		{Kind: historyResult, CallID: "sevt_c2", Text: historyNoResult, IsError: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("historyItems =\n%+v\nwant\n%+v", got, want)
	}
}

func TestClaudeProjectIsTheCLIsDirectoryName(t *testing.T) {
	if got := claudeProject("/private/var/x_y.z/ä"); got != "-private-var-x-y-z--" {
		t.Fatalf("claudeProject = %q", got)
	}
	long := "/" + strings.Repeat("a", 250)
	if got, want := claudeProject(long), "-"+strings.Repeat("a", 199)+"-feo44x"; got != want {
		t.Fatalf("claudeProject(long) = %q, want %q", got, want)
	}
}

// historyRebuilt runs the first turn of a session that has a past on the
// platform and no conversation here: it is rebuilt (context_rebuilt), and the
// model request carries the past (user text, the call's result, the answer)
// with the turn's tools.
func historyRebuilt(t *testing.T, f *acpFixture, ctx context.Context, bodies func() []string) {
	f.platform.mu.Lock()
	f.platform.history = map[string][]SessionHistoryEvent{"sesn_fork": pastOfFork}
	f.platform.mu.Unlock()
	res := f.start(t, ctx, "sesn_fork", "turn_1", "what was it")
	if !res.ContextRebuilt || res.Resumed || !strings.HasPrefix(res.ExecutorSessionRef, "sesn_fork@") {
		t.Fatalf("turn.start = %+v, want context_rebuilt, not resumed", res)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	all := bodies()
	if len(all) == 0 {
		t.Fatal("no model request")
	}
	first := all[0]
	for _, want := range []string{"remember 42", "OUT42", "noted 42", "sevt_c1", historyNoResult, "what was it"} {
		if !strings.Contains(first, want) {
			t.Fatalf("the first model request lacks %q: %.2000s", want, first)
		}
	}
	// A session without a past starts as before.
	plain := f.start(t, ctx, "sesn_new", "turn_1", "hello")
	if plain.ContextRebuilt || plain.Resumed {
		t.Fatalf("turn.start of a new session = %+v", plain)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
}

func TestClaudeCLIRebuildsASessionFromItsHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "true"})
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())
	if capability, err := f.executor.AgentCapability(); err != nil || !capability.HistoryRebuild {
		t.Fatalf("capability = %+v, %v: want history_rebuild", capability, err)
	}
	historyRebuilt(t, f, ctx, func() []string {
		model.mu.Lock()
		defer model.mu.Unlock()
		var out []string
		for _, r := range model.requests {
			if len(r.Tools) > 0 {
				out = append(out, r.Body)
			}
		}
		return out
	})
}

func TestCodexRebuildsASessionFromItsHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "true"})
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())
	if capability, err := f.executor.AgentCapability(); err != nil || !capability.HistoryRebuild {
		t.Fatalf("capability = %+v, %v: want history_rebuild", capability, err)
	}
	historyRebuilt(t, f, ctx, func() []string {
		model.mu.Lock()
		defer model.mu.Unlock()
		var out []string
		for _, r := range model.requests {
			out = append(out, r.Body)
		}
		return out
	})
	if tools := model.offeredTools(); len(tools) == 0 {
		t.Fatal("the rebuilt thread offers no tools")
	}
}

// The DSH brain hands a new conversation the session's past (session/new
// _meta.bma.history, the events as session.history answered them) when
// bma-dsh-acp rebuilds one (initialize _meta.bma.historyRebuild).
func TestDSHRebuildsASessionFromItsHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	if capability, err := f.executor.AgentCapability(); err != nil || !capability.HistoryRebuild {
		t.Fatalf("capability = %+v, %v: want history_rebuild", capability, err)
	}
	f.platform.mu.Lock()
	f.platform.history = map[string][]SessionHistoryEvent{"sesn_fork": pastOfFork}
	f.platform.mu.Unlock()
	res := f.start(t, ctx, "sesn_fork", "turn_1", "hello")
	if !res.ContextRebuilt || res.Resumed {
		t.Fatalf("turn.start = %+v, want context_rebuilt, not resumed", res)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	plain := f.start(t, ctx, "sesn_new", "turn_2", "hello")
	if plain.ContextRebuilt {
		t.Fatalf("turn.start of a new session = %+v", plain)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	got := callsOf(t, f, "history")
	want := []string{dshWireID(res.ExecutorSessionRef) + " sevt_u1,sevt_c1,sevt_r1,sevt_a1,sevt_u2,sevt_c2,sevt_r9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("session/new history = %q, want %q", got, want)
	}
}

// An agent that does not rebuild (bma-dsh-acp before 0.3.5) is not declared
// to: such a session starts without its past, as before.
func TestDSHWithoutHistoryRebuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Env: []string{"BMA_ACP_FAKE_NO_HISTORY=1"}})
	if capability, err := f.executor.AgentCapability(); err != nil || capability.HistoryRebuild {
		t.Fatalf("capability = %+v, %v: want no history_rebuild", capability, err)
	}
}
