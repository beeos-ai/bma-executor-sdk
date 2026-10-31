package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type conformanceExample struct {
	Name    string `json:"name"`
	Method  string `json:"method"`
	Kind    string `json:"kind"`
	Message *struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	} `json:"message,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func loadConformance(t *testing.T) []conformanceExample {
	t.Helper()
	raw, err := os.ReadFile("../contracts/bma/executor/examples/conformance.json")
	if err != nil {
		t.Fatalf("read conformance examples: %v", err)
	}
	var document struct {
		Protocol string               `json:"protocol"`
		Messages []conformanceExample `json:"messages"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode conformance examples: %v", err)
	}
	return document.Messages
}

// strictDecode fails on any member the generated contract type does not model.
func strictDecode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// TestConformanceExamplesDecodeStrictly checks the generated types model
// every params and result example of the contract snapshot.
func TestConformanceExamplesDecodeStrictly(t *testing.T) {
	for _, example := range loadConformance(t) {
		if _, ok := Methods[example.Method]; !ok {
			t.Errorf("%s: unknown method %s", example.Name, example.Method)
			continue
		}
		var err error
		if example.Kind == "result" {
			err = strictDecode(example.Result, NewResult(example.Method))
		} else {
			err = strictDecode(example.Message.Params, NewParams(example.Method))
		}
		if err != nil {
			t.Errorf("%s: %v", example.Name, err)
		}
	}
}

type conformanceAnswer struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *JSONRPCError   `json:"error,omitempty"`
}

// TestConformanceCorePeer drives a Core-backed executor over the in-process
// transport with every harness -> executor example of the contract: each
// method must be dispatched and decoded, results must match the contract, and
// every message the executor sends must match it too.
func TestConformanceCorePeer(t *testing.T) {
	examples := loadConformance(t)
	results := map[string]json.RawMessage{}
	for _, example := range examples {
		if example.Kind == "result" && results[example.Method] == nil {
			results[example.Method] = example.Result
		}
	}
	var ordered []conformanceExample
	var stop conformanceExample
	for _, example := range examples {
		if example.Message == nil || Methods[example.Method].Direction != DirectionHarnessToExecutor {
			continue
		}
		switch example.Method {
		case MethodSessionAssign:
			ordered = append([]conformanceExample{example}, ordered...)
		case MethodSessionStop:
			stop = example
		default:
			ordered = append(ordered, example)
		}
	}
	ordered = append(ordered, stop)

	workspace := t.TempDir()
	core, err := NewCore(CoreOptions{WorkRoot: workspace, Workspace: func(string, string) string { return workspace }})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(core.Register(NewProcessCapability(core, ProcessOptions{})))
	skills, err := NewSkillInstallCapability(func(context.Context, SkillInstallParams) SkillInstallResult {
		return SkillInstallResult{State: "ready"}
	})
	must(err)
	must(core.Register(skills))
	must(core.Register(Capability{
		Advertise: func(c *Capabilities) { c.Terminal = &TerminalCapability{Enabled: true, Kinds: []string{"human"}} },
		Methods: map[string]Handler{
			MethodTerminalOpen: func(context.Context, Request) (any, error) {
				return TerminalOpenResult{TerminalID: "term_1", Epoch: 1, Rows: 24, Cols: 80}, nil
			},
			MethodTerminalAttach: func(context.Context, Request) (any, error) { return TerminalAttachResult{Accepted: true}, nil },
			MethodTerminalClose:  func(context.Context, Request) (any, error) { return TerminalCloseResult{Closed: true}, nil },
		},
	}))

	executorEnd, harnessEnd := Pipe(ProtocolV11)
	client, err := NewClient(Options{
		EnvironmentID: "env_test", Executor: ExecutorInfo{Name: "conformance", OS: "linux", Arch: "amd64", Version: "test"},
		Capabilities: core.Capabilities(), TokenSource: StaticToken("pipe://", ""),
		Dial: func(context.Context, ExecutorTokenResponse, string, ChannelTarget) (Conn, error) {
			return executorEnd, nil
		},
	})
	must(err)
	must(core.Attach("cloud", client))
	t.Cleanup(func() {
		_ = client.Stop(context.Background(), "test done")
		core.Close()
	})

	var (
		mu       sync.Mutex
		problems []string
		sent     = map[string]bool{}
		pending  = map[string]chan conformanceAnswer{}
	)
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	write := func(message any) {
		raw, _ := json.Marshal(message)
		_ = harnessEnd.Write(ctx, raw)
	}
	go func() {
		for {
			raw, err := harnessEnd.Read(ctx)
			if err != nil {
				return
			}
			var message conformanceAnswer
			if err := json.Unmarshal(raw, &message); err != nil {
				problem("invalid frame: %s", raw)
				continue
			}
			if message.Method == "" {
				mu.Lock()
				waiter := pending[message.ID]
				mu.Unlock()
				if waiter != nil {
					waiter <- message
				}
				continue
			}
			spec, ok := Methods[message.Method]
			if !ok || spec.Direction == DirectionHarnessToExecutor {
				problem("executor sent %s, which is not an executor -> harness method", message.Method)
				continue
			}
			mu.Lock()
			sent[message.Method] = true
			mu.Unlock()
			if err := strictDecode(message.Params, NewParams(message.Method)); err != nil {
				problem("%s params: %v: %s", message.Method, err, message.Params)
			}
			if !spec.Notification {
				write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": results[message.Method]})
			}
		}
	}()
	if _, err := client.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, example := range ordered {
		waiter := make(chan conformanceAnswer, 1)
		mu.Lock()
		pending[example.Message.ID] = waiter
		mu.Unlock()
		write(example.Message)
		var answer conformanceAnswer
		select {
		case answer = <-waiter:
		case <-time.After(10 * time.Second):
			problem("%s: no answer", example.Name)
			continue
		}
		// agent and workspace.backup are contract methods this executor does
		// not register: they must answer capability_unavailable naming the
		// capability, never method_not_found.
		if capability := Methods[example.Method].Capability; capability == "agent" || capability == "workspace.backup" {
			if answer.Error == nil || answer.Error.Code != CodeCapabilityUnavailable || answer.Error.Data == nil || answer.Error.Data.Capability != capability {
				problem("%s: want capability_unavailable for %s, got %+v", example.Name, capability, answer.Error)
			}
			continue
		}
		if answer.Error != nil {
			decodeFailure := answer.Error.Code == CodeInvalidParams && strings.HasPrefix(answer.Error.Message, "invalid params")
			if answer.Error.Code == CodeMethodNotFound || decodeFailure {
				problem("%s: answered %d %s", example.Name, answer.Error.Code, answer.Error.Message)
			}
			continue
		}
		if err := strictDecode(answer.Result, NewResult(example.Method)); err != nil {
			problem("%s result: %v: %s", example.Name, err, answer.Result)
		}
	}
	// A method outside the contract stays method_not_found.
	unknown := make(chan conformanceAnswer, 1)
	mu.Lock()
	pending["unknown-method"] = unknown
	mu.Unlock()
	write(map[string]any{"jsonrpc": "2.0", "id": "unknown-method", "method": "vendor.unknown", "params": map[string]any{}})
	select {
	case answer := <-unknown:
		if answer.Error == nil || answer.Error.Code != CodeMethodNotFound {
			problem("vendor.unknown: want method_not_found, got %+v", answer.Error)
		}
	case <-time.After(10 * time.Second):
		problem("vendor.unknown: no answer")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ended := sent[MethodSessionEnded]
		mu.Unlock()
		if ended || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range []string{MethodExecutorRegister, MethodSessionReady, MethodToolResult, MethodSessionEnded} {
		if !sent[method] {
			problems = append(problems, "executor never sent "+method)
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}
