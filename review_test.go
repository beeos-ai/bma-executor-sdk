package bmaexecutor

// Regressions of research/typed-sdk-review-p12.md and typed-sdk-review-1.0.md
// (EM1, EM2 and the adopted suggestions); mirrors
// typescript/test/review.test.ts and python/tests/test_review.py.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func capabilitiesOf(t *testing.T, executor *Executor) Capabilities {
	t.Helper()
	var caps Capabilities
	if err := executor.Capabilities(&caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func bashOK(ctx context.Context, in BashInput, tc ToolContext) (ToolOutput, error) {
	return BashResult{Stdout: in.Command}.Output(), nil
}

func TestExplicitlyDisabledCapabilityWinsOverAHandler(t *testing.T) {
	executor, _ := typedPair(t, func(c *Client) {
		c.Tools.Bash.Handle(bashOK)
		c.Tools.Read.Handle(func(ctx context.Context, in ReadInput, tc ToolContext) (ToolOutput, error) {
			return TextOutput("x"), nil
		})
	}, Capabilities{Shell: &ShellCapability{Enabled: false}, Files: &FilesCapability{Enabled: false, ReadRoots: []string{"/w"}}})
	caps := capabilitiesOf(t, executor)
	if caps.Shell == nil || caps.Shell.Enabled || caps.Files == nil || caps.Files.Enabled || caps.Files.ReadRoots[0] != "/w" {
		t.Fatalf("shell = %+v files = %+v", caps.Shell, caps.Files)
	}
}

func TestHandlerRegisteredAfterStartUpdatesCapabilities(t *testing.T) {
	var client *Client
	executor, _ := typedPair(t, func(c *Client) { client = c }, Capabilities{})
	if caps := capabilitiesOf(t, executor); caps.Shell != nil && caps.Shell.Enabled {
		t.Fatalf("shell before = %+v", caps.Shell)
	}
	client.Tools.Bash.Handle(bashOK)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if caps := capabilitiesOf(t, executor); caps.Shell != nil && caps.Shell.Enabled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no executor.capabilities.update after Handle")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnregisteredToolIsRejectedBeforeTheAck(t *testing.T) {
	_, session := typedPair(t, func(c *Client) { c.Tools.Bash.Handle(bashOK) }, Capabilities{})
	_, err := session.Tools.Write.Call(testContext(t), WriteInput{Path: "x", Content: "y"}, CallToolOptions{})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeCapabilityUnavailable || rpcErr.Data == nil || rpcErr.Data.Capability != "tool:write" {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingRequiredInputFailsBeforeTheHandler(t *testing.T) {
	called := false
	_, session := typedPair(t, func(c *Client) {
		c.Tools.Bash.Handle(func(ctx context.Context, in BashInput, tc ToolContext) (ToolOutput, error) {
			called = true
			return TextOutput("ran"), nil
		})
	}, Capabilities{})
	result, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameBash, Input: json.RawMessage(`{"cwd":"x"}`)})
	if err != nil || called || result.Outcome != ToolOutcomeFailed || textOf(result) != "bash requires command" {
		t.Fatalf("result = %+v err = %v called = %v", result, err, called)
	}
}

func TestLargeBashOutputIsTruncatedLikeCore(t *testing.T) {
	stdout := strings.Repeat("o", 100_000)
	out := BashResult{Stdout: stdout, Stderr: strings.Repeat("e", 30_000), ExitCode: 1}.Output()
	inline := DefaultCoreInlineResultBytes
	if len(out.Content) != 3 || len(out.Content[0].Text) != inline/2 || len(out.Content[1].Text) != inline/4 ||
		out.Content[2].Text != "exit_code: 1 (output truncated to fit the result; 130000 bytes total)" {
		t.Fatalf("content = %d blocks, %d/%d bytes, %q", len(out.Content), len(out.Content[0].Text), len(out.Content[1].Text), out.Content[len(out.Content)-1].Text)
	}
	marked := BashResult{Stdout: "head", ExitCode: 0, Truncated: true}.Output()
	if marked.Content[len(marked.Content)-1].Text != "exit_code: 0 (output truncated to fit the result; 4 bytes total)" {
		t.Fatalf("marked = %+v", marked.Content)
	}
}

func TestComputerToolSlotsAreNotExposed(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Tools{}), reflect.TypeOf(HarnessTools{})} {
		for _, name := range []string{"ComputerObserve", "ComputerAction"} {
			if _, ok := typ.FieldByName(name); ok {
				t.Fatalf("%s.%s is exposed before the contract defines its input", typ.Name(), name)
			}
		}
	}
}

func TestTypedRequestCallAndNotify(t *testing.T) {
	statuses := make(chan SessionStatusParams, 1)
	var client *Client
	harness := NewHarness(HarnessOptions{OnSessionStatus: func(_ *Executor, p SessionStatusParams) { statuses <- p }})
	ctx := testContext(t)
	client, err := NewClient(Options{
		EnvironmentID: "local", Executor: ExecutorInfo{Name: "typed", OS: "test", Arch: "test", Version: "0"},
		TokenSource: StaticToken(startHarness(t, harness), ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	Requests.ProcessList.Handle(client, func(ctx context.Context, p ProcessListParams) (ProcessListResult, error) {
		return ProcessListResult{Processes: []ProcessState{{ProcessID: "p_" + p.SessionID}}}, nil
	})
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed ProcessListResult
	if err := executor.Call(ctx, MethodProcessList, ProcessListParams{SessionID: "s1"}, &listed); err != nil || len(listed.Processes) != 1 || listed.Processes[0].ProcessID != "p_s1" {
		t.Fatalf("listed = %+v %v", listed, err)
	}
	if err := Notifications.SessionStatus.Notify(ctx, client, SessionStatusParams{SessionID: "s1", Status: SessionStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if got := <-statuses; got.SessionID != "s1" {
		t.Fatalf("status = %+v", got)
	}
}

func TestRequiredInputTableMatchesTheContract(t *testing.T) {
	raw, err := os.ReadFile("contracts/bma/executor/schemas/tool.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Required []string `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{ToolNameBash: "BashInput", ToolNameRead: "ReadInput", ToolNameWrite: "WriteInput", ToolNameEdit: "EditInput",
		ToolNameGlob: "GlobInput", ToolNameGrep: "GrepInput", ToolNameListFiles: "ListFilesInput"}
	for tool, def := range inputs {
		want := schema.Defs[def].Required
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(requiredInput[tool], want) {
			t.Errorf("%s required = %v, contract %v", tool, requiredInput[tool], want)
		}
	}
}
