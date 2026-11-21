//go:build windows

package bmaexecutor

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func pipeName(t *testing.T) string {
	return fmt.Sprintf("bma-go-test-%d-%d", time.Now().UnixNano(), len(t.Name()))
}

func startPipeExecutor(t *testing.T, dial Dialer, tokenURL string) {
	t.Helper()
	core := newTestCore(t, CoreOptions{})
	client, err := NewClient(Options{
		EnvironmentID: "local", Executor: ExecutorInfo{Name: "pipe-hand"}, Capabilities: core.Capabilities(),
		TokenSource: StaticToken(tokenURL, "pipe-token"), Dial: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Attach("pipe", client); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		_, err := client.Start(context.Background())
		started <- err
	}()
	t.Cleanup(func() { _ = client.Stop(context.Background(), "done") })
	t.Cleanup(func() { <-started })
}

// The brain serves Harness on a named pipe and the executor dials it by the
// default dialer: the cloud-shaped direction.
func TestExecutorDialsAHarnessOnANamedPipe(t *testing.T) {
	name := pipeName(t)
	listener, err := NamedPipeListener(name)
	if err != nil {
		t.Fatal(err)
	}
	harness := NewHarness(HarnessOptions{})
	server := &http.Server{Handler: harness}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	startPipeExecutor(t, nil, "npipe:////./pipe/"+name)
	ctx := testContext(t)
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := readySession(t, brain{harness: harness, executor: executor}, SessionAssignParams{SessionID: "sess_pipe", Lease: Lease{Token: "lease"}})
	if session == nil {
		t.Fatal("no session")
	}
}

// The executor listens on a named pipe and a local brain dials it
// (Harness.DialExecutor), also by the \\.\pipe\ path and the npipe:// URL.
func TestListenNamedPipeLetsALocalBrainDialTheExecutor(t *testing.T) {
	name := pipeName(t)
	dial, closeListener, err := ListenNamedPipe(name, "pipe-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeListener() })
	startPipeExecutor(t, dial, "ws://local/")

	ctx := testContext(t)
	harness := NewHarness(HarnessOptions{})
	if err := harness.DialExecutor(ctx, `\\.\pipe\`+name, "wrong"); err == nil {
		t.Fatal("a brain with the wrong token connected")
	}
	if err := harness.DialExecutor(ctx, "npipe:////./pipe/"+name, "pipe-token"); err != nil {
		t.Fatal(err)
	}
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	readySession(t, brain{harness: harness, executor: executor}, SessionAssignParams{SessionID: "sess_pipe", Lease: Lease{Token: "lease"}})
}
