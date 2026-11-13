package bmaexecutor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// claudeCLIBinary is the pinned official claude binary: the one the Agent SDK
// platform package of testdata/claude-agent-acp installs (npm ci).
func claudeCLIBinary(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("BMA_CLAUDE_AGENT_ACP_DIR")
	if dir == "" {
		dir = "testdata/claude-agent-acp"
	}
	bin, err := filepath.Abs(filepath.Join(dir, "node_modules/@anthropic-ai", "claude-agent-sdk-"+runtime.GOOS+"-"+strings.Replace(runtime.GOARCH, "amd64", "x64", 1), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		if os.Getenv("BMA_REQUIRE_CLAUDE_CLI") != "" {
			t.Fatalf("the claude CLI is not installed: %v", err)
		}
		t.Skipf("the claude CLI is not installed (npm ci in testdata/claude-agent-acp): %v", err)
	}
	return bin
}

// startClaudeCLIFixture hosts the real, pinned claude CLI with an empty HOME
// and a scripted Anthropic endpoint (no credential of anyone).
func startClaudeCLIFixture(t *testing.T, ctx context.Context, model *scriptedModel, opts ACPAgentOptions, root string) *acpFixture {
	t.Helper()
	home := filepath.Join(root, "home")
	noErr(t, os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	opts.Profile = ACPProfileClaudeCodeCLI
	opts.Command = []string{claudeCLIBinary(t)}
	opts.Env = append(opts.Env, "ANTHROPIC_BASE_URL="+model.server.URL, "ANTHROPIC_API_KEY="+claudeTestAPIKey,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_TELEMETRY=1", "DISABLE_AUTOUPDATER=1")
	opts.CancelGrace = 10 * time.Second
	return startACPFixtureAt(t, ctx, opts, root, newRecordingPlatform(), nil)
}

func TestClaudeCLIRunsAnApprovedPlatformTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "printf hi > out.txt; cat out.txt"})
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())

	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.Harnesses) != 1 || capability.Harnesses[0].ID != "claude-code" || capability.Harnesses[0].Protocol != "claude-cli/stream-json" ||
		!capability.Harnesses[0].ImageInputs || capability.Harnesses[0].Version != "2.1.318" {
		t.Fatalf("capability = %+v, %v", capability, err)
	}
	if len(capability.PlatformCredentials) != 0 || capability.TurnResume || len(capability.Models) == 0 {
		t.Fatalf("capability: no platform credentials, no turn_resume, the CLI's models: %+v", capability)
	}

	result := f.start(t, ctx, "sesn_c", "turn_1", "run it")
	if result.ExecutorSessionRef != claudeSessionUUID("sesn_c") {
		t.Fatalf("executor_session_ref = %q, want the session's uuid %s", result.ExecutorSessionRef, claudeSessionUUID("sesn_c"))
	}
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	request := f.platform.interactions[0]
	if request.InteractionID != ApprovalID("sesn_c", "turn_1", "toolu_01") || request.ToolName != "bash" || request.ToolInput["command"] == nil {
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
	if b, err := os.ReadFile(filepath.Join(f.root, "acp", "sesn_c", "out.txt")); err != nil || string(b) != "hi" {
		t.Fatalf("out.txt = %q, %v", b, err)
	}
	// The CLI has no tool of its own; it offers the model its deferred-loading
	// placeholder besides the injected tools (not in its init tool list).
	for _, name := range model.offeredTools() {
		if !strings.HasPrefix(name, claudeToolPrefix) && name != "DeferredToolPlaceholder" {
			t.Fatalf("the model was offered %q: %v", name, model.offeredTools())
		}
	}
	if model.requests[0].APIKey != claudeTestAPIKey || f.upstream.requests != 0 {
		t.Fatalf("model api key %q, BMA upstream requests %d", model.requests[0].APIKey, f.upstream.requests)
	}
}

func TestClaudeCLIDenyRunsNothingPolicyAllowAsksNobodyAndResumes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "touch ran.txt"})
	root := t.TempDir()
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, root)
	f.start(t, ctx, "sesn_c", "turn_1", "run it")
	f.platform.waitFor(t, "permission", func() bool { return len(f.platform.interactions) == 1 })
	if _, err := f.executor.RespondInteraction(ctx, InteractionResponseParams{SessionID: "sesn_c", InteractionID: f.platform.interactions[0].InteractionID, Outcome: InteractionOutcomeDeny}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	ran := filepath.Join(f.root, "acp", "sesn_c", "ran.txt")
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("a denied tool ran")
	}
	// The platform's policy allows the tool: no interaction, it runs.
	f.start(t, ctx, "sesn_c", "turn_2", "run it", func(p *TurnStartParams) {
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
	before := len(model.requests)
	g := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, root)
	g.start(t, ctx, "sesn_c", "turn_3", "say hello", func(p *TurnStartParams) { p.ExecutorSessionRef = claudeSessionUUID("sesn_c") })
	assertIdle(t, g.platform.waitIdle(t, "turn_3"), TurnOutcomeCompleted)
	if !model.sawPromptAfter(before, "run it") {
		t.Fatal("the resumed conversation lost its history")
	}
	// An unknown conversation is not created anew.
	g.start(t, ctx, "sesn_other", "turn_1", "say hello", func(p *TurnStartParams) { p.ExecutorSessionRef = claudeSessionUUID("sesn_other") })
	idle := g.platform.waitIdle(t, "turn_1")
	assertIdle(t, idle, TurnOutcomeFailed)
}

func TestClaudeCLICancelEndsTheTurnAndKillsTheTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "sleep 45 && touch late.txt"})
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{}, t.TempDir())
	f.start(t, ctx, "sesn_c", "turn_1", "run it", func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
	})
	f.platform.waitFor(t, "tool use", func() bool {
		return strings.Contains(strings.Join(eventTypes(eventsOf(f.platform.events, "turn_1")), ","), AgentEventTypeAgentToolUse)
	})
	time.Sleep(time.Second)
	began := time.Now()
	if _, err := f.executor.CancelTurn(ctx, TurnCancelParams{SessionID: "sesn_c", TurnID: "turn_1"}); err != nil {
		t.Fatal(err)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCanceled)
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("cancel took %s", took)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_c", "late.txt")); err == nil {
		t.Fatal("the canceled command finished")
	}
}

// ask_user_question is a host tool on the injected MCP server: a custom tool
// call of the session, answered by any client, in the same turn.
func TestClaudeCLIAskUserQuestionIsACustomToolCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, ToolNameAskUserQuestion, map[string]any{"questions": []any{map[string]any{
		"question": "Which format?", "header": "Format", "options": []any{map[string]any{"label": "Markdown"}, map[string]any{"label": "PDF"}}}}})
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{AskUserQuestion: true}, t.TempDir())
	if capability, _ := f.executor.AgentCapability(); !capability.CustomTools {
		t.Fatalf("custom_tools = false: %+v", capability)
	}
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
	if use.EventID != askUserID("sesn_q", "turn_1", "toolu_01") || len(f.platform.interactions) != 0 {
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

// A user's image reaches the model as an image block; a thinking level is the CLI's effort.
func TestClaudeCLIImageAndThinkingLevel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	model := newScriptedModel(t, "bash", map[string]any{"command": "true"})
	png := testImage(t, "png", 4, 4)
	f := startClaudeCLIFixture(t, ctx, model, ACPAgentOptions{InputFiles: func(context.Context, ACPInputFile) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(png)), nil
	}}, t.TempDir())
	capability, _ := f.executor.AgentCapability()
	var effortModel AgentModel
	for _, m := range capability.Models {
		if len(m.ThinkingLevels) > 0 {
			effortModel = m
		}
	}
	if !capability.FileInputs || effortModel.ID == "" {
		t.Fatalf("file_inputs and a model with thinking levels: %+v", capability)
	}
	result := f.start(t, ctx, "sesn_i", "turn_1", "describe it", func(p *TurnStartParams) {
		p.Input = append(p.Input, fileBlock("file_1", "red.png", "image/png", len(png)))
		p.Config.Model, p.Config.ThinkingLevel = effortModel.ID, effortModel.ThinkingLevels[0]
	})
	if result.Effective.ThinkingLevel != effortModel.ThinkingLevels[0] || result.Effective.Model != effortModel.ID {
		t.Fatalf("effective = %+v", result.Effective)
	}
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	images := 0
	for _, r := range model.requests {
		images = max(images, r.Images)
	}
	if images != 1 {
		t.Fatalf("image blocks in the model requests = %d, want 1", images)
	}
	// An unknown level is refused, never replaced.
	_, err := f.executor.StartTurn(ctx, TurnStartParams{SessionID: "sesn_i", TurnID: "turn_2", InputID: "in_2",
		Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "x"}}, Config: &AgentTurnConfig{Model: effortModel.ID, ThinkingLevel: "turbo"}})
	if err == nil || !strings.Contains(err.Error(), "thinking_level") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeCLIRefusesAnUnpinnedBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	// The test binary answers --version as claude 9.9.9 (fakeClaudeCLIVersion).
	_, err = NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCodeCLI, Command: []string{exe}, Env: []string{"BMA_FAKE_CLAUDE_CLI=9.9.9"}, StateDir: t.TempDir(), Core: core})
	if err == nil || !strings.Contains(err.Error(), "not a pinned release") {
		t.Fatalf("err = %v", err)
	}
	// The pinned version, but not its binary.
	_, err = NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCodeCLI, Command: []string{exe}, Env: []string{"BMA_FAKE_CLAUDE_CLI=2.1.318"}, StateDir: t.TempDir(), Core: core})
	if err == nil || !strings.Contains(err.Error(), "is not the pinned claude 2.1.318 binary") {
		t.Fatalf("err = %v", err)
	}
	_, err = NewACPAgent(ctx, ACPAgentOptions{Profile: ACPProfileClaudeCodeCLI, Command: []string{exe}, PlatformModel: true, StateDir: t.TempDir(), Core: core})
	if err == nil || !strings.Contains(err.Error(), "PlatformModel") {
		t.Fatalf("err = %v", err)
	}
}

// The process arguments the gates rely on.
func TestClaudeCLIArguments(t *testing.T) {
	a := &claudeCLIAdapter{env: BrainEnv{Command: []string{"/bin/claude"}}, mcp: &cliToolServer{url: "http://127.0.0.1:1"}}
	args := a.args(cliLaunch{placeholder: "ph", uuid: "u", system: "S", model: "sonnet", effort: "low"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--include-partial-messages",
		"--permission-prompt-tool stdio", "--permission-mode default", "--tools  --mcp-config", "--strict-mcp-config",
		"--setting-sources  ", "--session-id u", "--append-system-prompt S", "--model sonnet", "--effort low"} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("arguments %q lack %q", joined, want)
		}
	}
	if args := strings.Join(a.args(cliLaunch{placeholder: "ph", uuid: "u", resume: true}), " "); !strings.Contains(args, "--resume u") || strings.Contains(args, "--session-id") {
		t.Errorf("resume arguments = %q", args)
	}
	if args := strings.Join(a.args(cliLaunch{placeholder: "ph", model: "default"}), " "); strings.Contains(args, "--model") {
		t.Errorf("the alias default is passed as a model: %q", args)
	}
}

// fakeClaudeCLI stands in for a claude binary that answers --version only.
func fakeClaudeCLI(version string) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "--version" {
		fmt.Println(version + " (Claude Code)")
		os.Exit(0)
	}
	os.Exit(2)
}

func TestClaudeSessionUUIDIsStableAndValid(t *testing.T) {
	a, b := claudeSessionUUID("sesn_1"), claudeSessionUUID("sesn_1")
	if a != b || a == claudeSessionUUID("sesn_2") || len(a) != 36 || a[14] != '5' || !strings.ContainsRune("89ab", rune(a[19])) {
		t.Fatalf("uuid = %s / %s", a, b)
	}
}
