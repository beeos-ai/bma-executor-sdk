// Command windows-hand is the machine executor for a Windows machine ("hand"):
// the SDK execution core (cmd.exe bash tool, file tools, stdio MCP), the
// process capability (Job Object process trees, ConPTY terminals, interrupts,
// file transfers), session files and, when Cua Driver runs in the desktop
// session, computer use. It is meant to run in the interactive session of an
// auto-logged-on user (a scheduled task at logon; see packaging/windows).
//
// Configuration comes from the process environment, after the optional env
// file (KEY=VALUE lines):
//
//	BMA_BASE_URL        deployment origin, e.g. https://executor.<env>.bma.beeos.ai
//	BMA_MACHINE_TOKEN   machine daemon token (never logged)
//	BMA_MACHINE_ID      machine id (optional; selects /v1/machines/{id}/token)
//	BMA_WORK_ROOT       session workspaces (default <dir of the env file>\work)
//	BMA_FILES_ROOT      session files root (default <dir of the env file>\files)
//	BMA_CUA_DRIVER      cua-driver executable, or "off" (default: cua-driver on PATH)
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// Set by the build: -ldflags "-X main.version=1.2.0 -X main.commit=<sha>".
var version, commit = "dev", "unknown"

func main() {
	envFile := flag.String("env-file", `C:\bma\executor.env`, "KEY=VALUE file; the process environment wins over it")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("windows-hand %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
		return
	}
	if err := loadEnvFile(*envFile); err != nil {
		log.Fatal(err)
	}
	base := filepath.Dir(*envFile)
	workRoot := envOr("BMA_WORK_ROOT", filepath.Join(base, "work"))
	filesRoot := envOr("BMA_FILES_ROOT", filepath.Join(base, "files"))
	token := os.Getenv("BMA_MACHINE_TOKEN")
	if os.Getenv("BMA_BASE_URL") == "" || token == "" {
		log.Fatal("BMA_BASE_URL and BMA_MACHINE_TOKEN are required (environment or " + *envFile + ")")
	}
	logf := func(e bmaexecutor.LogEvent) { log.Printf("%s: %s %v", e.Level, e.Message, e.Err) }
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	options := bmaexecutor.CoreOptions{
		WorkRoot: workRoot,
		// The executor's own credentials never reach a command.
		DenyEnv: func(name string) bool { return strings.HasPrefix(strings.ToUpper(name), "BMA_MACHINE_") },
	}
	switch driver := os.Getenv("BMA_CUA_DRIVER"); driver {
	case "off":
	default:
		if backend := waitForCuaDriver(ctx, driver); backend != nil {
			options.ComputerUse = backend
		}
	}
	core, err := bmaexecutor.NewCore(options)
	if err != nil {
		log.Fatal(err)
	}
	defer core.Close()
	files, err := bmaexecutor.NewSessionFiles(bmaexecutor.SessionFilesOptions{Root: filesRoot, OnLog: logf})
	if err != nil {
		log.Fatal(err)
	}
	if err := core.Register(bmaexecutor.NewProcessCapability(core, bmaexecutor.ProcessOptions{
		SessionFiles: files, FileTransfer: bmaexecutor.NewFileTransferer(),
	})); err != nil {
		log.Fatal(err)
	}
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL: os.Getenv("BMA_BASE_URL"), EnvironmentKey: token,
		Executor:     bmaexecutor.ExecutorInfo{Name: "windows-hand", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version},
		Capabilities: core.Capabilities(), OnLog: logf,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := core.Attach("cloud", client); err != nil {
		log.Fatal(err)
	}
	go files.Run(ctx)
	start := time.Now()
	registered, err := client.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if err := client.WaitReady(ctx); err != nil {
		log.Fatal(err)
	}
	log.Printf("registered as %s, ready %.1fs after start; work root %s, files root %s, computer use %v",
		registered.ExecutorID, time.Since(start).Seconds(), workRoot, filesRoot, options.ComputerUse != nil)
	<-ctx.Done()
	_ = client.Stop(context.Background(), "shutdown")
}

// waitForCuaDriver connects to Cua Driver. Its daemon starts at logon like the
// executor does, so it gets a minute to come up; without it computer use stays
// off (the executor then does not advertise computer_use).
func waitForCuaDriver(ctx context.Context, path string) bmaexecutor.ComputerUseBackend {
	deadline := time.Now().Add(time.Minute)
	for {
		backend, err := bmaexecutor.NewCuaDriver(ctx, bmaexecutor.CuaDriverOptions{Path: path})
		if err == nil {
			return backend
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			log.Printf("computer use is off: %v", err)
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// loadEnvFile sets the variables of path that the environment does not set. A
// missing file is fine (the environment may hold everything).
func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, set := os.LookupEnv(strings.TrimSpace(name)); !set {
			_ = os.Setenv(strings.TrimSpace(name), strings.Trim(strings.TrimSpace(value), `"`))
		}
	}
	return scanner.Err()
}
