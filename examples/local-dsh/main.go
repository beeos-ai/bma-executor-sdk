// Command local-dsh runs a local DSH brain on this computer for BMA: one
// executor connection (a BYO machine) that hosts bma-dsh-acp over ACP and
// runs its tools here, in the turn's working directory.
//
//	BMA_MACHINE_TOKEN=<machine token> BMA_LOCAL_MODEL_KEY=<your key> go run ./examples/local-dsh \
//	  -base-url https://executor.example -machine-id mch_... -agent /path/to/bma-dsh-acp \
//	  -model-base-url https://api.example/v1 -model my-model -work-root ~/bma-work
//
// Events, approvals, cancel and turn ends go through BMA; the model is the
// node owner's endpoint (-model-base-url with BMA_LOCAL_MODEL_KEY), or the
// platform's per-turn model endpoint when turn.start carries one.
//
// Claude Code instead of DSH: -profile claude-code with -agent set to the
// pinned official claude-agent-acp (node dist/index.js, see -node). It uses
// only your own Claude credentials, which are never sent to BMA: your Claude
// Code login (HOME), or ANTHROPIC_* / CLAUDE_CODE_* / AWS_* / GOOGLE_*
// variables of this process, which are handed to the agent as they are.
// -model-base-url and BMA_LOCAL_MODEL_KEY are not used.
//
// -labels (comma separated) and -remote-brain are this machine owner's local
// choices: labels narrow where BMA may place sessions; -remote-brain lets a
// brain on another machine use this one as its hands (off unless given).
// -platform-tools lets this machine be the brain of a session whose hands are
// on another machine (platform credential brain_tools).
//
// -cua-driver names the Cua Driver executable (https://github.com/trycua/cua,
// MIT): when its daemon runs in this user's desktop session, the local brain
// gets computer_observe and computer_action (each call approved on the
// platform by default); otherwise they are left out and a warning is logged.
// -cua-socket names the daemon a host app started for itself
// (`cua-driver serve --embedded --socket <path>`).
// -ask-user-question offers ask_user_question: a question is a custom tool
// call of the session, answered on any client (user.custom_tool_result), and
// the same turn continues. Give it only when the clients answer custom tools.
//
// Output is one JSON object per line on stderr (a supervisor such as a desktop
// app reads it): "executor registered" with executor_id, connection log lines,
// and "interaction requested" / "interaction ended" with the platform
// interaction id and its kind (permission, or question for ask_user_question
// with the custom tool call's event id), so a local UI can show the same card
// as every other client of the session and drop it when it is answered anywhere.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func main() {
	baseURL := flag.String("base-url", "", "BMA executor base URL")
	machineID := flag.String("machine-id", "", "BYO machine id (mch_...); empty uses the legacy token path")
	agent := flag.String("agent", "", "absolute path of the bma-dsh-acp executable, or of its .mjs bundle (with -node)")
	node := flag.String("node", "", "absolute path of node, to run a .mjs bundle")
	modelBase := flag.String("model-base-url", "", "OpenAI-compatible model endpoint ending in /v1")
	model := flag.String("model", "", "model id sent upstream")
	workRoot := flag.String("work-root", filepath.Join(os.TempDir(), "bma-local-dsh"), "session workspaces and allowed cwd root")
	state := flag.String("state", "", "agent state directory (default <work-root>/.state)")
	profile := flag.String("profile", bmaexecutor.ACPProfileDSH, "agent profile: dsh or claude-code")
	labels := flag.String("labels", "", "comma separated executor labels (a-z 0-9 . _ -)")
	remoteBrain := flag.Bool("remote-brain", false, "allow a brain on another machine to use this machine as its hands")
	platformTools := flag.Bool("platform-tools", false, "may be the brain of a session whose hands are on another machine")
	askUser := flag.Bool("ask-user-question", false, "offer ask_user_question (the session's clients answer custom tool calls)")
	cuaDriver := flag.String("cua-driver", "", "absolute path of the cua-driver executable (computer use when its daemon runs)")
	cuaSocket := flag.String("cua-socket", "", "the cua-driver daemon's socket (a host that runs `cua-driver serve --embedded --socket` passes it); default the driver's own")
	lifeline := flag.Bool("stdin-lifeline", false, "exit when stdin closes (a supervising app passes a pipe, so its exit or crash stops this executor)")
	flag.Parse()
	token, key := os.Getenv("BMA_MACHINE_TOKEN"), os.Getenv("BMA_LOCAL_MODEL_KEY")
	claude := *profile == bmaexecutor.ACPProfileClaudeCode
	if *baseURL == "" || *agent == "" || (*modelBase == "" && !claude) || token == "" {
		log.Fatal("-base-url, -agent, -model-base-url (not with -profile claude-code) and BMA_MACHINE_TOKEN are required")
	}
	if *state == "" {
		*state = filepath.Join(*workRoot, ".state")
	}
	command := []string{*agent}
	if strings.HasSuffix(*agent, ".mjs") {
		command = []string{*node, *agent}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *lifeline {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			emit(map[string]any{"level": "info", "msg": "stdin closed; stopping"})
			stop()
		}()
	}
	logf := func(e bmaexecutor.LogEvent) {
		line := map[string]any{"level": e.Level, "msg": e.Message}
		if e.Err != nil {
			line["error"] = e.Err.Error()
		}
		emit(line)
	}
	onInteraction := func(e bmaexecutor.ACPInteractionEvent) {
		emit(map[string]any{"level": "info", "msg": "interaction " + e.Phase, "interaction_id": e.InteractionID, "kind": e.Kind,
			"session_id": e.SessionID, "tool_name": e.ToolName, "tool_input": e.ToolInput, "outcome": e.Outcome})
	}

	coreOptions := bmaexecutor.CoreOptions{WorkRoot: *workRoot, OnLog: logf}
	if *cuaDriver != "" {
		if !filepath.IsAbs(*cuaDriver) {
			log.Fatal("-cua-driver must be an absolute path")
		}
		if driver, err := bmaexecutor.NewCuaDriver(ctx, bmaexecutor.CuaDriverOptions{Path: *cuaDriver, Socket: *cuaSocket}); err != nil {
			emit(map[string]any{"level": "warn", "msg": "computer use unavailable", "error": err.Error()})
		} else {
			coreOptions.ComputerUse = driver
			emit(map[string]any{"level": "info", "msg": "computer use available"})
		}
	}
	core, err := bmaexecutor.NewCore(coreOptions)
	check(err)
	defer core.Close()
	agentOptions := bmaexecutor.ACPAgentOptions{
		Profile: *profile, Command: command, StateDir: *state, Core: core, OnLog: logf, OnInteraction: onInteraction,
		PlatformTools: *platformTools, AskUserQuestion: *askUser,
	}
	if claude {
		for _, kv := range os.Environ() { // the owner's own credentials, for Claude Code only
			for _, prefix := range []string{"ANTHROPIC_", "CLAUDE_CODE_", "AWS_", "GOOGLE_", "CLOUD_ML_"} {
				if strings.HasPrefix(kv, prefix) {
					agentOptions.Env = append(agentOptions.Env, kv)
				}
			}
		}
	} else {
		agentOptions.Upstream = bmaexecutor.ACPModelUpstream{BaseURL: *modelBase, Model: *model,
			Header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }}
	}
	brain, err := bmaexecutor.NewACPAgent(ctx, agentOptions)
	check(err)
	defer brain.Close()

	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL: *baseURL, EnvironmentKey: token, MachineID: *machineID,
		Executor:     bmaexecutor.ExecutorInfo{Name: "local-dsh", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"},
		Capabilities: core.Capabilities(), OnLog: logf, RemoteBrain: *remoteBrain,
		Labels: splitLabels(*labels),
	})
	check(err)
	capability := brain.Capability()
	client.ServeAgent(brain, capability)
	check(core.Attach("cloud", client))
	registered, err := client.Start(ctx)
	check(err)
	emit(map[string]any{"level": "info", "msg": "executor registered", "executor_id": registered.ExecutorID,
		"harness": capability.Harnesses[0].ID, "harness_version": capability.Harnesses[0].Version})
	select {
	case <-ctx.Done():
	case <-client.Done():
		emit(map[string]any{"level": "error", "msg": "connection ended", "error": fmt.Sprint(client.Err())})
	}
	_ = client.Stop(context.Background(), "shutdown")
}

var out = json.NewEncoder(os.Stderr)

func emit(line map[string]any) {
	line["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	_ = out.Encode(line)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func splitLabels(s string) []string {
	var labels []string
	for _, label := range strings.Split(s, ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
}
