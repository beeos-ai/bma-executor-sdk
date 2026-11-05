package bmaexecutor

// Typed tool handlers (client.Tools.<Name>.Handle) and typed harness calls
// (session.Tools.<Name>.Call), ADR 0012 §4; mirrors
// typescript/test/typed-tools.test.ts.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func typedPair(t *testing.T, register func(*Client), capabilities Capabilities) (*Executor, *Session) {
	t.Helper()
	ctx := testContext(t)
	harness := NewHarness(HarnessOptions{})
	client, err := NewClient(Options{
		EnvironmentID: "local",
		Executor:      ExecutorInfo{Name: "typed", OS: "test", Arch: "test", Version: "0"},
		Capabilities:  capabilities,
		TokenSource:   StaticToken(startHarness(t, harness), ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	client.OnSessionAssign(func(ctx context.Context, assign SessionAssignParams) (SessionAssignResult, error) {
		go func() {
			_, _ = client.SessionReady(context.Background(), SessionReadyParams{SessionID: assign.SessionID, WorkID: assign.WorkID, LeaseToken: assign.Lease.Token})
		}()
		return SessionAssignResult{Accepted: true, SessionID: assign.SessionID}, nil
	})
	register(client)
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, err := executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_typed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return executor, session
}

func TestTypedBashResultRendersLikeCore(t *testing.T) {
	var seen []BashInput
	executor, session := typedPair(t, func(c *Client) {
		c.Tools.Bash.Handle(func(ctx context.Context, in BashInput, tc ToolContext) (ToolOutput, error) {
			seen = append(seen, in)
			if tc.SessionID != "sess_typed" || tc.ToolCallID == "" {
				t.Errorf("context = %+v", tc)
			}
			return BashResult{Stdout: "out\n", Stderr: "warn\n", ExitCode: 3}.Output(), nil
		})
	}, Capabilities{})
	var caps Capabilities
	if err := executor.Capabilities(&caps); err != nil || caps.Shell == nil || !caps.Shell.Enabled {
		t.Fatalf("shell capability = %+v %v", caps.Shell, err)
	}
	ctx := testContext(t)
	result, err := session.Tools.Bash.Call(ctx, BashInput{Command: "false", Cwd: "sub"}, CallToolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, []BashInput{{Command: "false", Cwd: "sub"}}) {
		t.Fatalf("seen = %+v", seen)
	}
	want := []ContentBlock{{Type: ContentBlockTypeText, Text: "out\n"}, {Type: ContentBlockTypeText, Text: "warn\n"}, {Type: ContentBlockTypeText, Text: "exit_code: 3"}}
	if result.Outcome != ToolOutcomeSucceeded || result.IsError || !reflect.DeepEqual(result.Content, want) {
		t.Fatalf("result = %+v", result)
	}
	quiet, err := session.Tools.Bash.Call(ctx, BashInput{Command: "true"}, CallToolOptions{ToolCallID: "call_quiet"})
	if err != nil || quiet.ToolCallID != "call_quiet" {
		t.Fatalf("quiet = %+v %v", quiet, err)
	}
}

func TestTypedTextToolErrorAndFailures(t *testing.T) {
	_, session := typedPair(t, func(c *Client) {
		c.Tools.Read.Handle(func(ctx context.Context, in ReadInput, tc ToolContext) (ToolOutput, error) {
			switch in.Path {
			case "missing":
				return ToolOutput{}, NewToolError("no such file: " + in.Path)
			case "boom":
				return ToolOutput{}, errors.New("disk on fire")
			}
			return TextOutput("contents of " + in.Path), nil
		})
	}, Capabilities{})
	ctx := testContext(t)
	ok, err := session.Tools.Read.Call(ctx, ReadInput{Path: "a.txt"}, CallToolOptions{})
	if err != nil || ok.Outcome != ToolOutcomeSucceeded || ok.IsError || textOf(ok) != "contents of a.txt" {
		t.Fatalf("ok = %+v %v", ok, err)
	}
	missing, _ := session.Tools.Read.Call(ctx, ReadInput{Path: "missing"}, CallToolOptions{})
	if missing.Outcome != ToolOutcomeSucceeded || !missing.IsError || textOf(missing) != "no such file: missing" {
		t.Fatalf("missing = %+v", missing)
	}
	boom, _ := session.Tools.Read.Call(ctx, ReadInput{Path: "boom"}, CallToolOptions{})
	if boom.Outcome != ToolOutcomeFailed || !boom.IsError || textOf(boom) != "disk on fire" {
		t.Fatalf("boom = %+v", boom)
	}
	unhandled, _ := session.Tools.Write.Call(ctx, WriteInput{Path: "x", Content: "y"}, CallToolOptions{})
	if unhandled.Outcome != ToolOutcomeFailed || !strings.Contains(textOf(unhandled), "no handler registered for tool write") {
		t.Fatalf("unhandled = %+v", unhandled)
	}
}

func TestTypedCapabilitiesFollowHandlers(t *testing.T) {
	executor, _ := typedPair(t, func(c *Client) {
		c.Tools.Read.Handle(func(ctx context.Context, in ReadInput, tc ToolContext) (ToolOutput, error) { return TextOutput("x"), nil })
	}, Capabilities{Shell: &ShellCapability{Enabled: true}, Files: &FilesCapability{ReadRoots: []string{"/w"}}})
	var caps Capabilities
	if err := executor.Capabilities(&caps); err != nil {
		t.Fatal(err)
	}
	if caps.Shell == nil || caps.Shell.Enabled || caps.Files == nil || !caps.Files.Enabled || caps.Files.ReadRoots[0] != "/w" || caps.ComputerUse {
		t.Fatalf("capabilities = %+v %+v", caps.Shell, caps.Files)
	}
}
