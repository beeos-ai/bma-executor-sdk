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
package main

import (
	"context"
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
	logf := func(e bmaexecutor.LogEvent) { log.Printf("%v %s %v", e.Level, e.Message, e.Err) }

	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: *workRoot, OnLog: logf})
	check(err)
	defer core.Close()
	brain, err := bmaexecutor.NewACPAgent(ctx, bmaexecutor.ACPAgentOptions{
		Command: command, StateDir: *state, Core: core, OnLog: logf,
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
	fmt.Printf("registered %s; harness %s %s\n", registered.ExecutorID, capability.Harnesses[0].ID, capability.Harnesses[0].Version)
	select {
	case <-ctx.Done():
	case <-client.Done():
		log.Printf("connection ended: %v", client.Err())
	}
	_ = client.Stop(context.Background(), "shutdown")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
