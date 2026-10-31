// Command cloud-hand runs this machine as an executor ("hand") for the BMA
// cloud harness: the cloud assigns sessions and sends tool calls; the SDK
// execution core runs bash/file tools and the stdio MCP servers named in
// session.assign, and reports results back over the same connection.
//
//	BMA_BASE_URL=https://api.<env>.bma.beeos.ai \
//	BMA_ENVIRONMENT_ID=env_... BMA_ENVIRONMENT_KEY=... \
//	go run ./examples/cloud-hand -work-root ~/bma-work
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime"

	bmaexecutor "github.com/beeos-ai/bma-sdks/go/executor"
)

func main() {
	workRoot := flag.String("work-root", "bma-work", "directory holding session workspaces")
	flag.Parse()

	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: *workRoot})
	if err != nil {
		log.Fatal(err)
	}
	defer core.Close()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL:        os.Getenv("BMA_BASE_URL"),
		EnvironmentID:  os.Getenv("BMA_ENVIRONMENT_ID"),
		EnvironmentKey: os.Getenv("BMA_ENVIRONMENT_KEY"),
		Executor:       bmaexecutor.ExecutorInfo{Name: "cloud-hand-example", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: "0.1.0"},
		Capabilities:   core.Capabilities(),
		OnLog:          func(e bmaexecutor.LogEvent) { log.Printf("%s: %s %v", e.Level, e.Message, e.Err) },
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := core.Attach("cloud", client); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	registered, err := client.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("registered as %s; waiting for the cloud harness (Ctrl-C to stop)", registered.ExecutorID)
	<-ctx.Done()
	_ = client.Stop(context.Background(), "shutdown")
}
