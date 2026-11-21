// Command third-party-hand is the smallest "hands" a third party can write
// against the SDK and the executor contract alone (ADR-0029): this computer
// registers with BMA, and whichever brain a session binds - on the platform, on
// another computer, or in a sandbox - uses it to run bash and file tools
// (read, write, edit, glob, grep) and processes in the session's workspace.
//
// The SDK's Core is the hands: it runs the tools, keeps one workspace per
// session under -work-root, enforces the file roots and ends a session's
// processes when it stops. The machine's owner decides what may use it:
//
//   - -labels narrow where BMA may place sessions (accepted as given; BMA never
//     bases an authorization on them);
//
//   - -remote-brain lets a brain that is NOT on this computer use it as its
//     hands. It is off unless given, the platform cannot turn it on, and a
//     platform-hosted brain does not need it.
//
//     BMA_MACHINE_TOKEN=<machine token> go run ./examples/third-party-hand \
//     -base-url https://executor.example -machine-id mch_... \
//     -work-root ~/bma-work -labels team-a,gpu -remote-brain
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func main() {
	baseURL := flag.String("base-url", "", "BMA executor base URL")
	machineID := flag.String("machine-id", "", "this machine's id (mch_...)")
	workRoot := flag.String("work-root", "bma-work", "directory holding session workspaces")
	labels := flag.String("labels", "", "comma separated executor labels (a-z 0-9 . _ -)")
	remoteBrain := flag.Bool("remote-brain", false, "allow a brain on another machine to use this machine as its hands")
	flag.Parse()
	token := os.Getenv("BMA_MACHINE_TOKEN")
	if *baseURL == "" || *machineID == "" || token == "" {
		log.Fatal("-base-url, -machine-id and BMA_MACHINE_TOKEN are required")
	}
	hand, err := newHand(bmaexecutor.Options{
		BaseURL: *baseURL, EnvironmentKey: token, MachineID: *machineID,
		Labels: splitLabels(*labels), RemoteBrain: *remoteBrain,
		OnLog: func(e bmaexecutor.LogEvent) { log.Printf("%s: %s %v", e.Level, e.Message, e.Err) },
	}, *workRoot)
	if err != nil {
		log.Fatal(err)
	}
	defer hand.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	registered, err := hand.client.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("registered as %s; waiting for sessions (Ctrl-C to stop)", registered.ExecutorID)
	select {
	case <-ctx.Done():
	case <-hand.client.Done():
		log.Printf("connection ended: %v", hand.client.Err())
	}
	_ = hand.client.Stop(context.Background(), "shutdown")
}

type hand struct {
	core   *bmaexecutor.Core
	client *bmaexecutor.Client
}

func (h *hand) close() { h.core.Close() }

// newHand builds the hands: a Core with the process capability, attached to a
// client that advertises what the Core can do. Register capabilities before
// Attach, and Attach before Start.
func newHand(options bmaexecutor.Options, workRoot string) (*hand, error) {
	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: workRoot})
	if err != nil {
		return nil, err
	}
	if err := core.Register(bmaexecutor.NewProcessCapability(core, bmaexecutor.ProcessOptions{})); err != nil {
		core.Close()
		return nil, err
	}
	options.Executor = bmaexecutor.ExecutorInfo{Name: "third-party-hand", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"}
	options.Capabilities = core.Capabilities()
	client, err := bmaexecutor.NewClient(options)
	if err != nil {
		core.Close()
		return nil, err
	}
	if err := core.Attach("cloud", client); err != nil {
		core.Close()
		return nil, err
	}
	return &hand{core: core, client: client}, nil
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
