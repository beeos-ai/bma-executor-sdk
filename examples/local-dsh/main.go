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
// Output is one JSON object per line on stderr (a supervisor such as a desktop
// app reads it): "executor registered" with executor_id, connection log lines,
// and "interaction requested" / "interaction ended" with the platform
// interaction id, so a local UI can show the same approval card as every other
// client of the session and drop it when it is answered anywhere.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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
	agent := flag.String("agent", "", "bma-dsh-acp executable, or a .mjs bundle run with node")
	modelBase := flag.String("model-base-url", "", "OpenAI-compatible model endpoint ending in /v1")
	model := flag.String("model", "", "model id sent upstream")
	workRoot := flag.String("work-root", filepath.Join(os.TempDir(), "bma-local-dsh"), "session workspaces and allowed cwd root")
	state := flag.String("state", "", "agent state directory (default <work-root>/.state)")
	flag.Parse()
	token, key := os.Getenv("BMA_MACHINE_TOKEN"), os.Getenv("BMA_LOCAL_MODEL_KEY")
	if *baseURL == "" || *agent == "" || *modelBase == "" || token == "" {
		log.Fatal("-base-url, -agent, -model-base-url and BMA_MACHINE_TOKEN are required")
	}
	if *state == "" {
		*state = filepath.Join(*workRoot, ".state")
	}
	command := []string{*agent}
	if strings.HasSuffix(*agent, ".mjs") {
		command = []string{"node", *agent}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(e bmaexecutor.LogEvent) {
		line := map[string]any{"level": e.Level, "msg": e.Message}
		if e.Err != nil {
			line["error"] = e.Err.Error()
		}
		emit(line)
	}
	onInteraction := func(e bmaexecutor.ACPInteractionEvent) {
		emit(map[string]any{"level": "info", "msg": "interaction " + e.Phase, "interaction_id": e.InteractionID,
			"session_id": e.SessionID, "tool_name": e.ToolName, "tool_input": e.ToolInput, "outcome": e.Outcome})
	}

	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: *workRoot, OnLog: logf})
	check(err)
	defer core.Close()
	brain, err := bmaexecutor.NewACPAgent(ctx, bmaexecutor.ACPAgentOptions{
		Command: command, StateDir: *state, Core: core, OnLog: logf, OnInteraction: onInteraction,
		Upstream: bmaexecutor.ACPModelUpstream{BaseURL: *modelBase, Model: *model,
			Header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }},
	})
	check(err)
	defer brain.Close()

	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL: *baseURL, EnvironmentKey: token, MachineID: *machineID,
		Executor:     bmaexecutor.ExecutorInfo{Name: "local-dsh", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"},
		Capabilities: core.Capabilities(), OnLog: logf,
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
