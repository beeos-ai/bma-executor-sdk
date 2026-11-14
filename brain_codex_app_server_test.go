//go:build !windows

package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
)

// codexBinary is the pinned official codex binary: BMA_CODEX_CLI, or
// testdata/codex-cli/codex (tools/fetch_codex_cli.sh 0.169.0 testdata/codex-cli/codex).
func codexBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("BMA_CODEX_CLI")
	if bin == "" {
		bin = "testdata/codex-cli/codex"
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		if os.Getenv("BMA_REQUIRE_CODEX_CLI") != "" {
			t.Fatalf("the codex CLI is not installed: %v", err)
		}
		t.Skipf("the codex CLI is not installed (tools/fetch_codex_cli.sh): %v", err)
	}
	return bin
}

// startCodexFixture hosts the real, pinned codex CLI with an empty Codex home
// and a scripted Responses endpoint (no credential of anyone).
func startCodexFixture(t *testing.T, ctx context.Context, model *scriptedResponses, opts ACPAgentOptions, root string) *acpFixture {
	t.Helper()
	home := filepath.Join(root, "codex-home")
	noErr(t, os.MkdirAll(home, 0o700))
	opts.Profile = ACPProfileCodex
	opts.Command = append([]string{codexBinary(t)}, codexProviderSettings(model.server.URL)...)
	opts.Env = append(opts.Env, "CODEX_HOME="+home, "BMA_CODEX_TEST_KEY="+codexTestAPIKey)
	opts.CancelGrace = 10 * time.Second
	return startACPFixtureAt(t, ctx, opts, root, newRecordingPlatform(), nil)
}

func TestCodexRunsAnApprovedPlatformTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "printf hi > out.txt; cat out.txt"})
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())

	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.Harnesses) != 1 || capability.Harnesses[0].ID != "codex" || capability.Harnesses[0].Protocol != "codex-app-server/json-rpc" ||
		!capability.Harnesses[0].ImageInputs || !slices.ContainsFunc(codexPins, func(p codexPin) bool { return p.Version == capability.Harnesses[0].Version }) {
		t.Fatalf("capability = %+v, %v", capability, err)
	}
	if len(capability.PlatformCredentials) != 0 || !capability.TurnResume || capability.Harnesses[0].ModelCredentials != BrainModelOwner {
		t.Fatalf("capability: no platform credentials, turn_resume, owner models: %+v", capability)
	}

	// A model of codex's catalog, as an owner's turn names one: its catalog
	// entry would bring code mode and tools of codex's own.
	catalogModel := capability.Harnesses[0].Models[0].ID
	result := f.start(t, ctx, "sesn_x", "turn_1", "run it", func(p *TurnStartParams) { p.Config.Model = catalogModel })
	if result.ExecutorSessionRef != "sesn_x" {
		t.Fatalf("executor_session_ref = %q", result.ExecutorSessionRef)
	}
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	if request.InteractionID != ApprovalID("sesn_x", "turn_1", "call_01") || request.ToolName != "bash" || request.ToolInput["command"] == nil ||
		!slices.Equal(request.Scopes, []string{InteractionScopeOnce, InteractionScopeSession}) {
		t.Fatalf("interaction.request = %+v", request)
	}
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_x", InteractionID: request.InteractionID, Outcome: InteractionOutcomeAllow, Scope: InteractionScopeSession}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	want := []string{AgentEventTypeSessionStatusRunning, AgentEventTypeAgentToolUse, AgentEventTypeAgentToolResult, AgentEventTypeAgentMessage, AgentEventTypeSessionStatusIdle}
	if got := claudeEvents(t, f, "turn_1"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	events := eventsOf(f.platform.snapshot(), "turn_1")
	use, res, message := eventJSON(t, events[1]), eventJSON(t, events[2]), eventJSON(t, events[3])
	if use["name"] != "bash" || use["evaluated_permission"].(map[string]any)["type"] != "ask" {
		t.Fatalf("tool_use = %v", use)
	}
	if text := fmt.Sprint(res["content"]); res["tool_use_id"] != events[1].EventID || !strings.Contains(text, "hi") {
		t.Fatalf("tool_result = %v", res)
	}
	if fmt.Sprint(message["content"]) != "[map[text:done type:text]]" {
		t.Fatalf("agent.message = %v", message)
	}
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_x", "out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("out.txt = %q, %v", b, err)
	}
	// Fail closed: the model is offered the platform tools, under their
	// names, and nothing of codex's own that could reach the machine.
	wantTools := slices.Sorted(func(yield func(string) bool) {
		for name := range coreTools {
			if !yield(name) {
				return
			}
		}
	})
	if got := model.offeredTools(); !slices.Equal(got, wantTools) {
		t.Fatalf("the model was offered %v, want exactly %v", got, wantTools)
	}
	if model.requests[0].APIKey != codexTestAPIKey || model.requests[0].Model != catalogModel || f.upstream.requests != 0 {
		t.Fatalf("model api key %q, model %q, BMA upstream requests %d", model.requests[0].APIKey, model.requests[0].Model, f.upstream.requests)
	}
}

func TestCodexDenyRunsNothingPolicyAllowAsksNobodyAndResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "touch ran.txt"})
	root := t.TempDir()
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{}, root)
	f.start(t, ctx, "sesn_x", "turn_1", "run it")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_x", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeDeny}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	ran := filepath.Join(f.root, "acp", "sesn_x", "ran.txt")
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a denied tool ran")
	}
	if got := strings.Join(claudeEvents(t, f, "turn_1"), ","); got != "session.status_running,agent.tool_use,agent.tool_result,agent.message,session.status_idle" {
		t.Fatalf("events = %s", got)
	}
	f.start(t, ctx, "sesn_x", "turn_2", "run it", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if len(f.platform.interactions) != 1 {
		t.Fatalf("interactions = %d, want only the first turn's", len(f.platform.interactions))
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("the allowed tool did not run: %v", err)
	}
	// A new host (an executor restart) resumes the same conversation by its ref.
	_ = f.agent.Close()
	before := model.count()
	g := startCodexFixture(t, ctx, model, ACPAgentOptions{}, root)
	g.start(t, ctx, "sesn_x", "turn_3", "say hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_x" })
	assertIdle(t, g.platform.waitIdle(t, "turn_3"), TurnOutcomeCompleted)
	if !model.sawPromptAfter(before, "run it") {
		t.Fatal("the resumed conversation lost its history")
	}
	// An unknown conversation is not created anew.
	g.start(t, ctx, "sesn_other", "turn_1", "say hello", func(p *TurnStartParams) { p.ExecutorSessionRef = "sesn_other" })
	assertIdle(t, g.platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
}

func TestCodexCancelEndsTheTurnAndKillsTheTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "sleep 45 && touch late.txt"})
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())
	f.start(t, ctx, "sesn_x", "turn_1", "run it", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	f.platform.waitFor(t, "tool use", func() bool {
		return strings.Contains(strings.Join(eventTypes(eventsOf(f.platform.events, "turn_1")), ","), AgentEventTypeAgentToolUse)
	})
	time.Sleep(time.Second)
	began := time.Now()
	if _, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_x", TurnID: "turn_1"}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("cancel took %s", took)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_x", "late.txt")); err == nil {
		t.Fatal("the canceled command finished")
	}
	// The conversation takes the next turn.
	model.setTool("none")
	f.start(t, ctx, "sesn_x", "turn_2", "say hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
}

func TestCodexAskUserQuestionIsACustomToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedResponses(t, ToolNameAskUserQuestion, map[string]any{"questions": []any{map[string]any{
		"question": "Which format?", "header": "Format", "options": []any{map[string]any{"label": "Markdown"}, map[string]any{"label": "PDF"}}}}})
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{AskUserQuestion: true}, t.TempDir())
	f.start(t, ctx, "sesn_q", "turn_1", "run it")
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
	if use.EventID != customToolUseID("sesn_q", "turn_1", "call_01") || len(f.platform.interactions) != 0 {
		t.Fatalf("custom tool use %s, interactions %+v", use.EventID, f.platform.interactions)
	}
	if _, err := f.executor.RespondCustomToolResult(ctx, AgentCustomToolResultParams{SessionID: "sesn_q", ToolUseID: use.EventID,
		Content: []ContentBlock{{Type: ContentBlockTypeText, Text: "Markdown"}}}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := strings.Join(claudeEvents(t, f, "turn_1"), ","); got != "session.status_running,agent.custom_tool_use,agent.message,session.status_idle" {
		t.Fatalf("events = %s", got)
	}
}

// A user's image reaches the model as an input image; the models are those
// codex reports (model_credentials owner): a turn's model and thinking level
// are passed on unchanged, another model is refused.
func TestCodexImageAndTheOwnersModels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedResponses(t, "bash", map[string]any{"command": "true"})
	png := testImage(t, "png", 4, 4)
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{InputFiles: func(context.Context, ACPInputFile) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(png)), nil
	}}, t.TempDir())
	capability, _ := f.executor.AgentCapability()
	h := capability.Harnesses[0]
	i := slices.IndexFunc(h.Models, func(m AgentModel) bool { return slices.Contains(m.ThinkingLevels, "high") })
	if !capability.FileInputs || h.ModelCredentials != BrainModelOwner || i < 0 {
		t.Fatalf("file_inputs, owner models: %+v", capability)
	}
	chosen := h.Models[i].ID
	if _, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_i", TurnID: "turn_0", InputID: "in_0",
		Input: []AgentInputBlock{{Type: "text", Text: "x"}}, Config: &AgentTurnConfig{Model: "deepseek-flash"}}); err == nil {
		t.Fatal("a model codex does not report was accepted")
	}
	result := f.start(t, ctx, "sesn_i", "turn_1", "describe it", func(p *TurnStartParams) {
		p.Input = append(p.Input, fileBlock("file_1", "red.png", "image/png", len(png)))
		p.Config.Model, p.Config.ThinkingLevel = chosen, "high"
	})
	if result.Effective.Model != chosen || result.Effective.ThinkingLevel != "high" {
		t.Fatalf("effective = %+v", result.Effective)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	last := model.requests[len(model.requests)-1]
	if last.Images != 1 || last.Model != chosen || last.Effort != "high" {
		t.Fatalf("model request = %+v, want 1 image, model %s, effort high", last, chosen)
	}
}

func TestCodexRunsTheToolsOnTheRemoteHands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	gw := newGateway(t, map[string]string{"bash": "allow"})
	model := newScriptedResponses(t, "bash", map[string]any{"command": "echo pwned > pwned.txt"})
	f := startCodexFixture(t, ctx, model, ACPAgentOptions{PlatformTools: true}, t.TempDir())
	f.start(t, ctx, "sesn_x", "turn_1", "run it", remoteTurn(gw.URL(), "tok-1"))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if _, err := os.Stat(filepath.Join(gw.Workspace(), "pwned.txt")); err != nil {
		t.Fatalf("the tool did not run on the hands machine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_x", "pwned.txt")); err == nil {
		t.Fatal("the tool also ran on the brain's machine")
	}
	var call *gatewaystub.Request
	for _, r := range gw.Recorded() {
		if r.Method == "tools/call" {
			call = &r
		}
	}
	if call == nil || !strings.Contains(string(call.Params), platformIdempotencyKey("sesn_x", "turn_1", "call_01")) {
		t.Fatalf("gateway calls: %+v", gw.Recorded())
	}
}

func TestCodexRefusesWhatWouldOpenTheGates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	exe, err := os.Executable()
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	home := t.TempDir()
	start := func(command []string, env ...string) error {
		_, err := NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileCodex, Command: command, Env: append([]string{"CODEX_HOME=" + home}, env...), StateDir: t.TempDir(), Core: core})
		return err
	}
	// The test binary answers --version as codex-cli <version> (BMA_FAKE_CODEX_CLI).
	if err := start([]string{exe}, "BMA_FAKE_CODEX_CLI=9.9.9"); err == nil || !strings.Contains(err.Error(), "not a pinned release") {
		t.Fatalf("err = %v", err)
	}
	if err := start([]string{exe}, "BMA_FAKE_CODEX_CLI=0.169.0"); err == nil || !strings.Contains(err.Error(), "is not the pinned codex 0.169.0 binary") {
		t.Fatalf("err = %v", err)
	}
	if err := start([]string{exe, "--enable", "shell_tool"}); err == nil || !strings.Contains(err.Error(), "only -c settings of the model provider") {
		t.Fatalf("err = %v", err)
	}
	if err := start([]string{exe, "-c", "features.shell_tool=true"}); err == nil || !strings.Contains(err.Error(), "only -c settings of the model provider") {
		t.Fatalf("err = %v", err)
	}
	_, err = NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileCodex, Command: []string{exe}, StateDir: t.TempDir(), Core: core})
	if err == nil || !strings.Contains(err.Error(), "CODEX_HOME") {
		t.Fatalf("err = %v", err)
	}
	for _, name := range codexHomeFiles {
		noErr(t, os.WriteFile(filepath.Join(home, name), []byte("x"), 0o600))
		if err := start([]string{exe}, "BMA_FAKE_CODEX_CLI=0.169.0"); err == nil || !strings.Contains(err.Error(), "has "+name) {
			t.Fatalf("%s: err = %v", name, err)
		}
		noErr(t, os.Remove(filepath.Join(home, name)))
	}
	_, err = NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileCodex, Command: []string{exe}, Env: []string{"CODEX_HOME=" + home}, PlatformModel: true, StateDir: t.TempDir(), Core: core})
	if err == nil || !strings.Contains(err.Error(), "PlatformModel") {
		t.Fatalf("err = %v", err)
	}
}

// The process arguments the gates rely on.
func TestCodexArguments(t *testing.T) {
	a := &codexAdapter{owner: []string{"-c", `model_provider="x"`}, catalog: "/state/codex-model-catalog.json"}
	joined := strings.Join(a.args(), " ")
	if !strings.HasPrefix(joined, `app-server -c model_provider="x" `) {
		t.Errorf("arguments %q: the owner's provider first", joined)
	}
	for _, f := range []string{"shell_tool", "unified_exec", "apps", "plugins", "hooks", "multi_agent", "view_image", "image_generation", "computer_use", "browser_use"} {
		if !strings.Contains(joined, "--disable "+f+" ") {
			t.Errorf("arguments %q lack --disable %s", joined, f)
		}
	}
	for _, s := range []string{`web_search="disabled"`, "agents.enabled=false", "tools.experimental_request_user_input.enabled=false",
		`model_catalog_json="/state/codex-model-catalog.json"`, "skills.include_instructions=false", "include_environment_context=false", "include_permissions_instructions=false"} {
		if !strings.Contains(joined, "-c "+s) {
			t.Errorf("arguments %q lack -c %s", joined, s)
		}
	}
}

// A built-in tool's item or request ends the turn (fail closed), whatever
// the command line turned off.
func TestCodexBuiltInToolEndsTheTurn(t *testing.T) {
	for _, line := range []string{
		`{"method":"item/started","params":{"turnId":"t1","item":{"type":"commandExecution","id":"x"}}}`,
		`{"method":"item/started","params":{"turnId":"t1","item":{"type":"mcpToolCall","id":"x"}}}`,
		`{"id":7,"method":"item/commandExecution/requestApproval","params":{"turnId":"t1"}}`,
	} {
		r, w := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, r) }()
		p := &codexProc{a: &codexAdapter{env: BrainEnv{CancelGrace: time.Second}}, stdin: w, stderr: &tailBuffer{max: 1024}, done: make(chan struct{}), waiting: map[int64]chan codexResponse{}}
		close(p.done)
		prompt := &codexPrompt{ctx: context.Background(), turnID: "t1", done: make(chan codexTurnEnd, 1), text: map[string]*strings.Builder{}}
		p.setPrompt(prompt)
		p.dispatch([]byte(line))
		deadline := time.Now().Add(2 * time.Second)
		for prompt.failure.Load() == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		f := prompt.failure.Load()
		if f == nil || f.Code != AgentErrorCodeAgentProtocolError || !strings.Contains(f.Message, "tool_catalog_violation") {
			t.Fatalf("%s: failure = %+v", line, f)
		}
		_ = w.Close()
	}
}

func TestCodexErrorClasses(t *testing.T) {
	for raw, code := range map[string]string{
		`"contextWindowExceeded"`: AgentErrorCodeContextTooLarge,
		`"unauthorized"`:          AgentErrorCodeAgentAuthRequired,
		`"usageLimitExceeded"`:    AgentErrorCodeAgentAuthRequired,
		`{"responseStreamDisconnected":{"httpStatusCode":401}}`:    AgentErrorCodeAgentAuthRequired,
		`{"responseTooManyFailedAttempts":{"httpStatusCode":429}}`: AgentErrorCodeAgentError,
		`"other"`: AgentErrorCodeAgentError,
	} {
		if got := codexError(json.RawMessage(raw), "x"); got.Code != code {
			t.Errorf("%s: %s, want %s", raw, got.Code, code)
		}
	}
}
