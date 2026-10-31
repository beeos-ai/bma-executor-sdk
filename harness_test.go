package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startHarness serves h on a loopback listener and returns its ws:// URL.
func startHarness(t *testing.T, h *Harness) string {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func newLoopbackClient(t *testing.T, wsURL, token string) *Client {
	t.Helper()
	client, err := NewClient(Options{
		EnvironmentID: "local",
		Executor:      ExecutorInfo{Name: "test-hand", OS: "test", Arch: "test", Version: "0"},
		Capabilities:  ExecutorCapabilities{Shell: &ShellCapability{Enabled: true}},
		TokenSource:   StaticToken(wsURL, token),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(context.Background(), "test done") })
	return client
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func textOf(result ToolResultParams) string {
	var b strings.Builder
	for _, block := range result.Content {
		if block.Text != nil {
			b.WriteString(*block.Text)
		}
	}
	return b.String()
}

func TestHarnessDrivesExecutorClient(t *testing.T) {
	ctx := testContext(t)
	harness := NewHarness(HarnessOptions{})
	wsURL := startHarness(t, harness)
	client := newLoopbackClient(t, wsURL, "")

	var runs atomic.Int32
	release := make(chan struct{})
	client.OnToolCall(func(ctx context.Context, call ToolCallParams) (ToolResultParams, error) {
		runs.Add(1)
		if call.Tool == "block" {
			select {
			case <-ctx.Done():
			case <-release:
			}
			return ToolResultParams{Outcome: ToolOutcomeSucceeded}, nil
		}
		text := "ran " + *call.Input.Command
		return ToolResultParams{Outcome: ToolOutcomeSucceeded, Content: []ContentBlock{{Type: ContentBlockTypeText, Text: &text}}}, nil
	})
	client.OnSessionAssign(func(ctx context.Context, assign SessionAssignParams) (SessionAssignResult, error) {
		go func() {
			_, _ = client.SessionReady(context.Background(), SessionReadyParams{
				SessionID: assign.SessionID, WorkID: assign.WorkID, LeaseToken: assign.Lease.Token,
			})
		}()
		return SessionAssignResult{Accepted: true, SessionID: assign.SessionID}, nil
	})
	if _, err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if executor.Info().Name != "test-hand" || executor.ID() != client.ExecutorID() {
		t.Fatalf("executor = %+v id %q, client id %q", executor.Info(), executor.ID(), client.ExecutorID())
	}
	session, err := executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	command := "echo hi"
	call := ToolCallParams{ToolCallID: "call_1", Tool: ToolNameBash, Input: ToolInput{Command: &command}}
	result, err := session.CallTool(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ToolOutcomeSucceeded || textOf(result) != "ran echo hi" || result.SessionID != "sess_1" {
		t.Fatalf("result = %+v", result)
	}
	// Idempotent by tool_call_id: the cached result comes back and the
	// executor does not run the tool again.
	again, err := session.CallTool(ctx, call)
	if err != nil || textOf(again) != "ran echo hi" || runs.Load() != 1 {
		t.Fatalf("repeat = %+v err %v runs %d", again, err, runs.Load())
	}

	// tool.cancel aborts a running call and the result reports canceled.
	blocked := make(chan ToolResultParams, 1)
	go func() {
		result, _ := session.CallTool(ctx, ToolCallParams{ToolCallID: "call_block", Tool: "block"})
		blocked <- result
	}()
	waitFor(t, func() bool { return runs.Load() == 2 })
	canceled, err := session.CancelTool(ctx, "call_block", "user")
	if err != nil || !canceled {
		t.Fatalf("cancel = %v err %v", canceled, err)
	}
	if got := <-blocked; got.Outcome != ToolOutcomeCanceled {
		t.Fatalf("canceled result = %+v", got)
	}

	if err := session.Stop(ctx, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("session not done after stop")
	}
	if _, err := session.CallTool(ctx, ToolCallParams{ToolCallID: "call_late", Tool: ToolNameBash, Input: ToolInput{Command: &command}}); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("call after stop err = %v", err)
	}
}

func TestHarnessAuthenticateRejectsUpgrade(t *testing.T) {
	harness := NewHarness(HarnessOptions{
		Authenticate: func(r *http.Request) (Principal, error) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return Principal{}, errors.New("bad token")
			}
			return Principal{ExecutorID: "exe_pinned"}, nil
		},
	})
	wsURL := startHarness(t, harness)
	ctx := testContext(t)

	bad := newLoopbackClient(t, wsURL, "bad")
	if _, err := bad.Start(ctx); err == nil {
		t.Fatal("bad token registered")
	}
	good := newLoopbackClient(t, wsURL, "good")
	if _, err := good.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if good.ExecutorID() != "exe_pinned" {
		t.Fatalf("executor id = %q, want the authenticated identity", good.ExecutorID())
	}
}

// rawExecutor is a hand-written executor peer for wire-level assertions.
type rawExecutor struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialRaw(t *testing.T, wsURL, query string) *rawExecutor {
	t.Helper()
	ctx := testContext(t)
	if query != "" {
		wsURL += "?" + query
	}
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{WSSubprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &rawExecutor{t: t, ws: ws}
}

func (r *rawExecutor) send(frame map[string]any) {
	r.t.Helper()
	frame["jsonrpc"] = "2.0"
	raw, _ := json.Marshal(frame)
	if err := r.ws.Write(testContext(r.t), websocket.MessageText, raw); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rawExecutor) read() rpcInbound {
	r.t.Helper()
	_, raw, err := r.ws.Read(testContext(r.t))
	if err != nil {
		r.t.Fatal(err)
	}
	var message rpcInbound
	if err := json.Unmarshal(raw, &message); err != nil {
		r.t.Fatal(err)
	}
	return message
}

// readResponse skips harness requests until the response to id arrives.
func (r *rawExecutor) readResponse(id string) rpcInbound {
	r.t.Helper()
	for {
		message := r.read()
		if message.Method == "" && message.ID != nil && *message.ID == id {
			return message
		}
	}
}

func (r *rawExecutor) register(resume map[string]any) ExecutorRegisterResult {
	params := map[string]any{
		"protocol": ProtocolVersion, "environment_id": "local",
		"executor": map[string]any{"name": "raw", "os": "test", "arch": "test", "version": "0"}, "capabilities": map[string]any{},
	}
	if resume != nil {
		params["resume"] = resume
	}
	r.send(map[string]any{"id": "reg", "method": MethodExecutorRegister, "params": params})
	response := r.readResponse("reg")
	if response.Error != nil {
		r.t.Fatalf("register error %+v", response.Error)
	}
	var result ExecutorRegisterResult
	_ = json.Unmarshal(response.Result, &result)
	return result
}

// acceptAssign answers the next session.assign and returns its params.
func (r *rawExecutor) acceptAssign() SessionAssignParams {
	r.t.Helper()
	request := r.read()
	if request.Method != MethodSessionAssign {
		r.t.Fatalf("got %s, want session.assign", request.Method)
	}
	var params SessionAssignParams
	_ = json.Unmarshal(request.Params, &params)
	r.send(map[string]any{"id": *request.ID, "result": map[string]any{"accepted": true, "session_id": params.SessionID}})
	return params
}

func TestHarnessToolResultSemantics(t *testing.T) {
	ctx := testContext(t)
	harness := NewHarness(HarnessOptions{})
	wsURL := startHarness(t, harness)
	raw := dialRaw(t, wsURL, "")
	raw.register(nil)
	executor, err := harness.WaitExecutor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assigned := make(chan *Session, 1)
	go func() {
		session, err := executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_a"})
		if err != nil {
			t.Error(err)
		}
		assigned <- session
	}()
	raw.acceptAssign()
	<-assigned

	// A result for a call the harness never dispatched is not accepted.
	raw.send(map[string]any{"id": "r1", "method": MethodToolResult, "params": map[string]any{
		"session_id": "sess_a", "tool_call_id": "never", "outcome": "succeeded", "is_error": false, "content": []any{},
	}})
	if response := raw.readResponse("r1"); response.Error == nil || response.Error.Code != JSONRPCErrorCodeNotFound {
		t.Fatalf("unknown call response = %+v", response)
	}
	// A result for a session this executor does not hold is session_not_active.
	raw.send(map[string]any{"id": "r2", "method": MethodToolResult, "params": map[string]any{
		"session_id": "sess_other", "tool_call_id": "x", "outcome": "succeeded", "is_error": false, "content": []any{},
	}})
	if response := raw.readResponse("r2"); response.Error == nil || response.Error.Code != JSONRPCErrorCodeSessionNotActive {
		t.Fatalf("foreign session response = %+v", response)
	}
}

func TestHarnessResumeDeliversPendingResultAndLeaseExpiryIsUnknown(t *testing.T) {
	ctx := testContext(t)
	harness := NewHarness(HarnessOptions{LeaseTTL: 300 * time.Millisecond})
	wsURL := startHarness(t, harness)

	first := dialRaw(t, wsURL, "")
	registered := first.register(nil)
	executor, _ := harness.WaitExecutor(ctx)
	sessionCh := make(chan *Session, 1)
	go func() {
		session, _ := executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_r"})
		sessionCh <- session
	}()
	first.acceptAssign()
	session := <-sessionCh

	resultCh := make(chan error, 2)
	var got [2]ToolResultParams
	for i, id := range []string{"call_kept", "call_lost"} {
		go func() {
			result, err := session.CallTool(ctx, ToolCallParams{ToolCallID: id, Tool: ToolNameBash})
			got[i] = result
			resultCh <- err
		}()
		request := first.read()
		if request.Method != MethodToolCall {
			t.Fatalf("got %s", request.Method)
		}
		first.send(map[string]any{"id": *request.ID, "result": map[string]any{"accepted": true}})
	}
	_ = first.ws.Close(websocket.StatusGoingAway, "network blip")

	// Reconnect within the lease: same executor, session still active, the
	// late result is delivered to the waiting caller.
	second := dialRaw(t, wsURL, "")
	again := second.register(map[string]any{
		"executor_id": registered.ExecutorID, "last_connection_id": registered.ConnectionID, "active_sessions": []string{"sess_r"},
	})
	if again.ExecutorID != registered.ExecutorID || again.ConnectionID == registered.ConnectionID {
		t.Fatalf("resume register = %+v, first %+v", again, registered)
	}
	second.send(map[string]any{"id": "res", "method": MethodToolResult, "params": map[string]any{
		"session_id": "sess_r", "tool_call_id": "call_kept", "outcome": "succeeded", "is_error": false, "content": []any{},
	}})
	if response := second.readResponse("res"); response.Error != nil {
		t.Fatalf("resumed result rejected: %+v", response.Error)
	}
	if err := <-resultCh; err != nil {
		t.Fatalf("kept call err = %v", err)
	}

	// Lease expiry without another reconnect: the remaining call's outcome is
	// unknown and the session ends.
	_ = second.ws.Close(websocket.StatusGoingAway, "gone")
	if err := <-resultCh; !errors.Is(err, ErrToolOutcomeUnknown) {
		t.Fatalf("lost call err = %v", err)
	}
	select {
	case <-session.Done():
	case <-ctx.Done():
		t.Fatal("session did not end after lease expiry")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
