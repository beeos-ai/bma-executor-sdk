package bmaexecutor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// effortOf is the reasoning effort the fake agent saw for the turn's prompt.
func effortOf(t *testing.T, f *acpFixture, turnID string) string {
	t.Helper()
	for _, e := range eventsOf(f.platform.snapshot(), turnID) {
		if e.Type == AgentEventTypeAgentMessage {
			if _, rest, ok := strings.Cut(fmt.Sprint(eventJSON(t, e)["content"]), "effort="); ok {
				value, _, _ := strings.Cut(rest, " ")
				return "effort=" + value
			}
		}
	}
	t.Fatalf("turn %s: no effort message", turnID)
	return ""
}

func (f *acpFixture) upstreamModels() []string {
	f.upstream.mu.Lock()
	defer f.upstream.mu.Unlock()
	return slices.Clone(f.upstream.models)
}

var turnConfigModels = []AgentModel{
	{ID: "m-fast", DisplayName: "Fast", ThinkingLevels: []string{"none", "low", "high"}, DefaultThinkingLevel: "low"},
	{ID: "m-plain"},
}

func TestACPAgentTurnConfigChoosesAnOfferedModelAndThinkingLevel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Models: turnConfigModels})
	if c := f.agent.Capability(); fmt.Sprint(c.Models) != fmt.Sprint(turnConfigModels) {
		t.Fatalf("capability models = %+v", c.Models)
	}
	steps := []struct {
		turn, model, level         string
		wantModel, wantLevel, seen string
	}{
		{"turn_1", "m-fast", "high", "m-fast", "high", "effort=high"},
		{"turn_2", "m-plain", "", "m-plain", "", "effort=<nil>"},
		{"turn_3", "m-fast", "", "m-fast", "low", "effort=low"},      // the model's default level
		{"turn_4", "m-plain", "high", "m-plain", "", "effort=<nil>"}, // a model without levels ignores it
	}
	for _, s := range steps {
		res := f.start(t, ctx, "sesn_tc", s.turn, "effort", func(p *TurnStartParams) { p.Config.Model, p.Config.ThinkingLevel = s.model, s.level })
		if res.Effective.Model != s.wantModel || res.Effective.ThinkingLevel != s.wantLevel {
			t.Fatalf("%s effective = %+v", s.turn, res.Effective)
		}
		assertIdle(t, f.platform.waitIdle(t, s.turn), TurnOutcomeCompleted)
		if got := effortOf(t, f, s.turn); got != s.seen {
			t.Fatalf("%s: agent saw %q, want %q", s.turn, got, s.seen)
		}
	}
	// Each turn's requests go upstream with that turn's model, not Upstream.Model.
	if got := f.upstreamModels(); !slices.Equal(got, []string{"m-fast", "m-plain", "m-fast", "m-plain"}) {
		t.Fatalf("upstream models = %v", got)
	}
	for _, refused := range []struct{ model, level string }{{"m-other", ""}, {"m-fast", "xhigh"}, {"m-fast", "turbo"}} {
		_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_tc2", TurnID: "turn_x" + refused.model + refused.level, InputID: "in_x",
			Input:  []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "effort"}},
			Config: &AgentTurnConfig{System: "Be brief.", Model: refused.model, ThinkingLevel: refused.level}})
		var rpc *RPCError
		if !asRPC(err, &rpc) || rpc.Code != CodeInvalidParams {
			t.Fatalf("model %q level %q: %v, want -32602", refused.model, refused.level, err)
		}
	}
	if got := len(f.upstreamModels()); got != 4 {
		t.Fatalf("a refused turn reached the upstream: %d requests", got)
	}
}

func TestACPAgentTurnConfigWithoutModelsIgnoresModelAndLevel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	res := f.start(t, ctx, "sesn_none", "turn_1", "effort", func(p *TurnStartParams) { p.Config.Model, p.Config.ThinkingLevel = "m-any", "high" })
	if res.Effective.Model != "upstream-model" || res.Effective.ThinkingLevel != "" {
		t.Fatalf("effective = %+v, want the host's model and no level", res.Effective)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := effortOf(t, f, "turn_1"); got != "effort=<nil>" {
		t.Fatalf("agent saw %q", got)
	}
	if got := f.upstreamModels(); !slices.Equal(got, []string{"upstream-model"}) {
		t.Fatalf("upstream models = %v", got)
	}
}

func TestACPAgentTurnConfigLevelsNeedAnAgentThatTakesThem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Models: turnConfigModels, Env: []string{"BMA_ACP_FAKE_NO_EFFORTS=1"}})
	c := f.agent.Capability()
	if len(c.Models) != 2 || c.Models[0].ThinkingLevels != nil || c.Models[0].DefaultThinkingLevel != "" {
		t.Fatalf("capability models = %+v, want no levels for an agent without reasoning effort", c.Models)
	}
	res := f.start(t, ctx, "sesn_old", "turn_1", "effort", func(p *TurnStartParams) { p.Config.Model, p.Config.ThinkingLevel = "m-fast", "high" })
	if res.Effective.ThinkingLevel != "" {
		t.Fatalf("effective = %+v", res.Effective)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := effortOf(t, f, "turn_1"); got != "effort=<nil>" {
		t.Fatalf("agent saw %q", got)
	}
}

func TestACPAgentRefusesInvalidModels(t *testing.T) {
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	exe, err := os.Executable() // absolute on every OS; the options are refused before it would start
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		profile string
		models  []AgentModel
	}{
		"empty id":            {"", []AgentModel{{ID: ""}}},
		"duplicate":           {"", []AgentModel{{ID: "m"}, {ID: "m"}}},
		"unknown level":       {"", []AgentModel{{ID: "m", ThinkingLevels: []string{"turbo"}}}},
		"default not offered": {"", []AgentModel{{ID: "m", ThinkingLevels: []string{"low"}, DefaultThinkingLevel: "high"}}},
		"claude code":         {ACPProfileClaudeCode, []AgentModel{{ID: "m"}}},
	} {
		_, err := NewACPAgent(context.Background(), ACPAgentOptions{Profile: c.profile, Command: []string{exe}, Core: core, StateDir: t.TempDir(),
			Upstream: ACPModelUpstream{BaseURL: "http://127.0.0.1:1/v1"}, Models: c.models})
		if err == nil || !strings.Contains(err.Error(), "Models") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestACPAgentWithBmaDshAcpTurnConfig drives the real bma-dsh-acp (>= 0.2.1,
// BMA_DSH_ACP_BIN as for TestACPAgentWithBmaDshAcp): a turn's model and
// thinking level reach the model endpoint as model and reasoning_effort.
func TestACPAgentWithBmaDshAcpTurnConfig(t *testing.T) {
	bin := os.Getenv("BMA_DSH_ACP_BIN")
	if bin == "" {
		t.Skip("BMA_DSH_ACP_BIN is not set")
	}
	command := []string{bin}
	if strings.HasSuffix(bin, ".mjs") {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node not found for the .mjs bundle")
		}
		command = []string{node, bin}
	}
	var mu sync.Mutex
	var bodies []map[string]any
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{Command: command, Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "m-b"},
		Models: []AgentModel{{ID: "m-a", ThinkingLevels: []string{"low", "high"}}, {ID: "m-b"}}})
	if c := f.agent.Capability(); len(c.Models) != 2 || !slices.Equal(c.Models[0].ThinkingLevels, []string{"low", "high"}) {
		t.Fatalf("capability models = %+v (bma-dsh-acp before 0.2.1 declares no levels)", c.Models)
	}
	f.start(t, ctx, "sesn_real_tc", "turn_1", "hi", func(p *TurnStartParams) { p.Config.Model, p.Config.ThinkingLevel = "m-a", "high" })
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	f.start(t, ctx, "sesn_real_tc", "turn_2", "again", func(p *TurnStartParams) { p.Config.Model = "m-b" })
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0]["model"] != "m-a" || bodies[0]["reasoning_effort"] != "high" || bodies[1]["model"] != "m-b" {
		t.Fatalf("model requests = %v", bodies)
	}
	if _, ok := bodies[1]["reasoning_effort"]; ok {
		t.Fatalf("turn 2 named no level but sent %v", bodies[1]["reasoning_effort"])
	}
}
