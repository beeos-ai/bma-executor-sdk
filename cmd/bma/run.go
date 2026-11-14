package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// cmdRun is the executor: this computer's hands (Core: processes, files,
// tools in the turn's working directory) and the enabled harness as its
// brain, on one connection to BMA. It ends with status 0 when the platform
// rejects the machine token (the computer was removed: the service stays
// stopped), with an error otherwise (the service restarts it).
func cmdRun(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: bma run")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := loadToken(c.MachineID)
	if err != nil {
		return err
	}
	harnesses, err := enabled(c)
	if err != nil {
		return err
	}
	if len(harnesses) != 1 {
		// One executor connection hosts one brain; a node with several
		// harnesses needs the SDK's multi-brain host.
		return fmt.Errorf("enable exactly one harness (enabled: %d): `bma harness list`", len(harnesses))
	}
	h := harnesses[0]
	exe := harnessPath(h.kind, h.release)
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("%s %s is not installed: run `bma up`", h.kind.DisplayName, h.release.Version)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(e bmaexecutor.LogEvent) {
		if e.Err != nil {
			log.Printf("%s %s: %v", e.Level, e.Message, e.Err)
			return
		}
		log.Printf("%s %s", e.Level, e.Message)
	}
	if err := os.MkdirAll(c.WorkRoot, 0o700); err != nil {
		return err
	}
	core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: c.WorkRoot, OnLog: logf})
	if err != nil {
		return err
	}
	defer core.Close()
	opts := bmaexecutor.ACPAgentOptions{
		StateDir: filepath.Join(home(), "state", h.kind.ID), Core: core, OnLog: logf,
		PlatformTools: c.PlatformTools,
	}
	if len(c.AllowedRoots) > 0 {
		opts.AllowedRoots = append([]string{c.WorkRoot}, c.AllowedRoots...)
	}
	brain, err := h.kind.NewBrain(ctx, exe, opts)
	if err != nil {
		return err
	}
	defer brain.Close()
	client, err := bmaexecutor.NewClient(bmaexecutor.Options{
		BaseURL: c.ExecutorURL, EnvironmentKey: token, MachineID: c.MachineID,
		Executor:     bmaexecutor.ExecutorInfo{Name: "bma", OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version},
		Capabilities: core.Capabilities(), OnLog: logf, RemoteBrain: c.RemoteBrain, Labels: c.Labels,
	})
	if err != nil {
		return err
	}
	client.ServeAgent(brain, brain.Capability())
	if err := core.Attach("cloud", client); err != nil {
		return err
	}
	if _, err := client.Start(ctx); err != nil {
		return finish(err)
	}
	go reportState(ctx, client)
	select {
	case <-ctx.Done():
		_ = client.Stop(context.Background(), "shutdown")
		return nil
	case <-client.Done():
		return finish(client.Err())
	}
}

// finish records how the connection ended: a rejected machine token stops
// the service for good.
func finish(err error) error {
	s := state{Connection: "disconnected", UpdatedAt: time.Now(), PID: os.Getpid()}
	if err != nil {
		s.Error = err.Error()
	}
	if errors.Is(err, bmaexecutor.ErrCredentialRejected) {
		s.Connection = "revoked"
		_ = writeJSON(statePath(), s)
		log.Printf("this computer's machine token was rejected (removed from BMA?): run `bma up --reauth`")
		return nil
	}
	_ = writeJSON(statePath(), s)
	return err
}

// reportState writes the connection state for `bma status` and `bma up`.
func reportState(ctx context.Context, client *bmaexecutor.Client) {
	var s state
	for {
		cur := string(client.State())
		if cur != s.Connection {
			s.Connection, s.Error = cur, ""
			if cur == bmaexecutor.ConnectionStateReady {
				s.ConnectedAt = time.Now()
			}
			s.UpdatedAt, s.PID = time.Now(), os.Getpid()
			_ = writeJSON(statePath(), s)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
