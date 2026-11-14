//go:build !windows

package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A conversation the brain no longer has (its volume is gone, the machine is
// another one, its own store was emptied) is not resumed silently: turn.start
// answers resumed false and a handle the platform has not seen, and the new
// conversation does not hold the earlier input. Resumed true is only for a
// conversation the brain confirms it has.

func claudeLostState(t *testing.T, remove func(root string) string) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "true"})
	root := t.TempDir()
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, root)
	first := f.start(t, ctx, "sesn_l", "turn_1", "remember 42", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	_ = f.agent.Close()

	next := remove(root)
	before := len(model.requests)
	g := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, next)
	lost := g.start(t, ctx, "sesn_l", "turn_2", "what was it?", func(p *TurnStartParams) {
		p.ExecutorSessionRef = first.ExecutorSessionRef
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	if lost.Resumed || lost.ExecutorSessionRef == first.ExecutorSessionRef || !strings.HasPrefix(lost.ExecutorSessionRef, "sesn_l@") {
		t.Fatalf("turn.start with the conversation gone = %+v, want resumed false and a new handle (was %s)", lost, first.ExecutorSessionRef)
	}
	assertIdle(t, g.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if model.sawPromptAfter(before, "remember 42") {
		t.Fatal("the new conversation holds the earlier input")
	}
	// The new conversation is the session's from now on: resumed by its own handle.
	_ = g.agent.Close()
	h := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, next)
	again := h.start(t, ctx, "sesn_l", "turn_3", "and now?", func(p *TurnStartParams) { p.ExecutorSessionRef = lost.ExecutorSessionRef })
	if !again.Resumed || again.ExecutorSessionRef != lost.ExecutorSessionRef {
		t.Fatalf("turn.start of the new conversation = %+v, want it resumed", again)
	}
	assertIdle(t, h.platform.waitIdle(t, "turn_3"), TurnOutcomeCompleted)
}

func TestClaudeCLIContextLostWhenTheVolumeIsGone(t *testing.T) {
	claudeLostState(t, func(string) string { return t.TempDir() })
}

func TestClaudeCLIContextLostWhenTheCLIsOwnStoreIsGone(t *testing.T) {
	claudeLostState(t, func(root string) string {
		noErr(t, os.RemoveAll(filepath.Join(root, "home", ".claude")))
		return root
	})
}

func codexLostState(t *testing.T, remove func(root string) string) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "true"})
	root := t.TempDir()
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{}, root)
	first := f.start(t, ctx, "sesn_l", "turn_1", "remember 42", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	_ = f.agent.Close()

	next := remove(root)
	before := model.count()
	g := startCodexFixture(t, ctx, model, ACPAgentOptions{}, next)
	lost := g.start(t, ctx, "sesn_l", "turn_2", "what was it?", func(p *TurnStartParams) {
		p.ExecutorSessionRef = first.ExecutorSessionRef
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	if lost.Resumed || lost.ExecutorSessionRef == first.ExecutorSessionRef || !strings.HasPrefix(lost.ExecutorSessionRef, "sesn_l@") {
		t.Fatalf("turn.start with the conversation gone = %+v, want resumed false and a new handle (was %s)", lost, first.ExecutorSessionRef)
	}
	assertIdle(t, g.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if model.sawPromptAfter(before, "remember 42") {
		t.Fatal("the new conversation holds the earlier input")
	}
}

func TestCodexContextLostWhenTheVolumeIsGone(t *testing.T) {
	codexLostState(t, func(string) string { return t.TempDir() })
}

// The adapter's own record of the thread is there but codex's rollout is not
// (its home was emptied): the record alone does not make the conversation exist.
func TestCodexContextLostWhenTheRolloutIsGone(t *testing.T) {
	codexLostState(t, func(root string) string {
		noErr(t, os.RemoveAll(filepath.Join(root, "codex-home", "sessions")))
		return root
	})
}

// The real bma-dsh-acp: a state directory without the session's log answers
// session/load resource-not-found, which is a lost conversation; the new one
// does not hold the earlier input and has a handle the agent accepts.
func TestDshContextLostWhenTheVolumeIsGone(t *testing.T) {
	command := dshAcpCommand(t)
	var mu sync.Mutex
	var bodies []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, delta := range []map[string]any{{"content": "ok"}, {}} {
			finish := any(nil)
			if len(delta) == 0 {
				finish = "stop"
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer model.Close()
	saw := func(from int, text string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(bodies[from:], func(b string) bool { return strings.Contains(b, text) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	opts := ACPAgentOptions{Command: command, Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "scripted"}}
	root := t.TempDir()
	f := startACPFixtureAt(t, ctx, opts, root, newRecordingPlatform(), nil)
	first := f.start(t, ctx, "sesn_d", "turn_1", "remember 42")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	_ = f.agent.Close()

	// The same volume: resumed, with the history.
	same := startACPFixtureAt(t, ctx, opts, root, newRecordingPlatform(), nil)
	kept := same.start(t, ctx, "sesn_d", "turn_2", "what was it?", func(p *TurnStartParams) { p.ExecutorSessionRef = first.ExecutorSessionRef })
	if !kept.Resumed || kept.ExecutorSessionRef != first.ExecutorSessionRef {
		t.Fatalf("turn.start on the same volume = %+v, want it resumed", kept)
	}
	assertIdle(t, same.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	_ = same.agent.Close()

	// A new volume: lost.
	mu.Lock()
	from := len(bodies)
	mu.Unlock()
	g := startACPFixtureAt(t, ctx, opts, t.TempDir(), newRecordingPlatform(), nil)
	lost := g.start(t, ctx, "sesn_d", "turn_3", "what was it?", func(p *TurnStartParams) { p.ExecutorSessionRef = first.ExecutorSessionRef })
	if lost.Resumed || lost.ExecutorSessionRef == first.ExecutorSessionRef || !strings.HasPrefix(lost.ExecutorSessionRef, "sesn_d@") {
		t.Fatalf("turn.start with the volume gone = %+v, want resumed false and a new handle (was %s)", lost, first.ExecutorSessionRef)
	}
	assertIdle(t, g.platform.waitIdle(t, "turn_3"), TurnOutcomeCompleted)
	if saw(from, "remember 42") {
		t.Fatal("the new conversation holds the earlier input")
	}
}

// The CLI's transcript is looked for where the CLI itself looks: the last
// HOME of its environment (the host's Env overrides the process's), and
// CLAUDE_CONFIG_DIR over HOME.
func TestClaudeCLIHoldsLooksInTheConfigDirOfTheCLIsEnvironment(t *testing.T) {
	const uuid = "0a0a0a0a-0a0a-5a0a-8a0a-0a0a0a0a0a0a"
	transcript := func(dir string) {
		noErr(t, os.MkdirAll(filepath.Join(dir, "projects", "-work"), 0o755))
		noErr(t, os.WriteFile(filepath.Join(dir, "projects", "-work", uuid+".jsonl"), nil, 0o600))
	}
	t.Setenv("HOME", t.TempDir()) // the process's own HOME has nothing
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, config := t.TempDir(), t.TempDir()
	transcript(filepath.Join(home, ".claude"))
	a := &claudeCLIAdapter{env: BrainEnv{Env: []string{"HOME=" + home}}}
	if !a.holds(uuid) {
		t.Fatal("a transcript under the HOME of the host's Env is not found")
	}
	a = &claudeCLIAdapter{env: BrainEnv{Env: []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + config}}}
	if a.holds(uuid) {
		t.Fatal("CLAUDE_CONFIG_DIR does not take over from HOME")
	}
	transcript(config)
	if !a.holds(uuid) {
		t.Fatal("a transcript under CLAUDE_CONFIG_DIR is not found")
	}
}
