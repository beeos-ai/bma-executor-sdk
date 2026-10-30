package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// frameLog records control-channel frames so tests can wait for responses and
// requests in any order without racing the client's writes.
type frameLog struct {
	mu     sync.Mutex
	frames []rpcInbound
	used   []bool
	err    error
	notify chan struct{}
}

func newFrameLog() *frameLog {
	return &frameLog{notify: make(chan struct{}, 1)}
}

func (l *frameLog) push(frame rpcInbound) {
	l.mu.Lock()
	l.frames = append(l.frames, frame)
	l.used = append(l.used, false)
	l.mu.Unlock()
	l.signal()
}

func (l *frameLog) fail(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
	l.signal()
}

func (l *frameLog) signal() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *frameLog) await(ctx context.Context, match func(rpcInbound) bool) (rpcInbound, error) {
	for {
		l.mu.Lock()
		for i := range l.frames {
			if !l.used[i] && match(l.frames[i]) {
				l.used[i] = true
				frame := l.frames[i]
				l.mu.Unlock()
				return frame, nil
			}
		}
		err := l.err
		l.mu.Unlock()
		if err != nil {
			return rpcInbound{}, err
		}
		select {
		case <-ctx.Done():
			return rpcInbound{}, ctx.Err()
		case <-l.notify:
		}
	}
}

func (l *frameLog) count(match func(rpcInbound) bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := 0
	for _, frame := range l.frames {
		if match(frame) {
			total++
		}
	}
	return total
}

type testSession struct {
	ws  *websocket.Conn
	log *frameLog
}

func (s *testSession) readLoop(ctx context.Context) {
	for {
		_, data, err := s.ws.Read(ctx)
		if err != nil {
			s.log.fail(err)
			return
		}
		var frame rpcInbound
		if err := json.Unmarshal(data, &frame); err != nil {
			s.log.fail(err)
			return
		}
		s.log.push(frame)
	}
}

func (s *testSession) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, data)
}

func (s *testSession) awaitResponse(ctx context.Context, id string) (rpcInbound, error) {
	return s.log.await(ctx, func(frame rpcInbound) bool {
		return frame.ID != nil && *frame.ID == id
	})
}

func (s *testSession) awaitRequest(ctx context.Context, method string) (rpcInbound, error) {
	return s.log.await(ctx, func(frame rpcInbound) bool {
		return frame.Method == method && frame.ID != nil
	})
}

type testServer struct {
	server *httptest.Server
	done   chan error
}

func startTestServer(t *testing.T, session func(ctx context.Context, s *testSession) error) *testServer {
	t.Helper()
	ts := &testServer{done: make(chan error, 1)}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			if got := r.Header.Get("Authorization"); got != "Bearer env_key" {
				http.Error(w, "authorization", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID:   "exec_test",
				Token:        "jwt_test",
				ExpiresIn:    300,
				WSURL:        "ws://" + r.Host + "/v1/executors/channel",
				ChannelScope: []ChannelScope{ChannelScopeControl, ChannelScopeMcp},
			})
		case "/v1/executors/channel":
			if got := r.Header.Get("Authorization"); got != "Bearer jwt_test" {
				http.Error(w, "authorization", http.StatusUnauthorized)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{WSSubprotocol}})
			if err != nil {
				ts.done <- err
				return
			}
			defer ws.CloseNow()
			ws.SetReadLimit(maxFrameBytes)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s := &testSession{ws: ws, log: newFrameLog()}
			go s.readLoop(ctx)
			ts.done <- session(ctx, s)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *testServer) wait(t *testing.T) {
	t.Helper()
	select {
	case err := <-ts.done:
		if err != nil {
			t.Fatalf("server session: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server session did not finish")
	}
}

func registerSession(ctx context.Context, s *testSession, heartbeatMS int64) error {
	frame, err := s.awaitRequest(ctx, MethodExecutorRegister)
	if err != nil {
		return err
	}
	return s.write(rpcSuccessEnvelope{
		JSONRPC: "2.0",
		ID:      *frame.ID,
		Result: ExecutorRegisterResult{
			ExecutorID:           "exec_test",
			ConnectionID:         "conn_test",
			ServerTime:           "2026-10-31T00:00:00Z",
			HeartbeatIntervalMS:  heartbeatMS,
			LeaseTTLMS:           60000,
			MaxControlFrameBytes: 65536,
			MaxMCPFrameBytes:     1 << 20,
		},
	})
}

func newTestClient(t *testing.T, baseURL string, mutate func(*Options)) *Client {
	t.Helper()
	options := Options{
		EnvironmentKey: "env_key",
		EnvironmentID:  "env_test",
		Executor:       ExecutorInfo{Name: "test", OS: "linux", Arch: "arm64", Version: "test"},
		BaseURL:        baseURL,
		RequestTimeout: 2 * time.Second,
	}
	if mutate != nil {
		mutate(&options)
	}
	client, err := NewClient(options)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func startTestClient(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx, "test")
	})
}

func writeToolCall(s *testSession, id string, params ToolCallParams) error {
	return s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  MethodToolCall,
		"params":  params,
	})
}

func TestToolCancelCancelsHandlerAndReportsCanceled(t *testing.T) {
	var calls int32
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		if err := writeToolCall(s, "srv_tool", ToolCallParams{
			SessionID:  "sess_test",
			ToolCallID: "toolu_cancel",
			Tool:       ToolNameBash,
			Input:      ToolInput{Command: stringPtr("sleep 60")},
		}); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_tool"); err != nil {
			return err
		}

		if err := s.write(map[string]any{
			"jsonrpc": "2.0",
			"id":      "srv_cancel",
			"method":  MethodToolCancel,
			"params": ToolCancelParams{
				SessionID:  "sess_test",
				ToolCallID: "toolu_cancel",
				Reason:     "user_interrupt",
			},
		}); err != nil {
			return err
		}
		cancelFrame, err := s.awaitResponse(ctx, "srv_cancel")
		if err != nil {
			return err
		}
		var cancelResult ToolCancelResult
		if err := json.Unmarshal(cancelFrame.Result, &cancelResult); err != nil {
			return err
		}
		if !cancelResult.Canceled {
			return fmt.Errorf("tool.cancel was not acknowledged")
		}

		resultFrame, err := s.awaitRequest(ctx, MethodToolResult)
		if err != nil {
			return err
		}
		var result ToolResultParams
		if err := json.Unmarshal(resultFrame.Params, &result); err != nil {
			return err
		}
		if result.Outcome != ToolOutcomeCanceled {
			return fmt.Errorf("tool.result outcome = %s, want canceled", result.Outcome)
		}
		if result.IsError {
			return fmt.Errorf("canceled tool result must not set is_error")
		}
		return s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *resultFrame.ID, Result: Ack{Accepted: true}})
	})

	client := newTestClient(t, ts.server.URL, nil)
	client.OnToolCall(func(ctx context.Context, _ ToolCallParams) (ToolResultParams, error) {
		atomic.AddInt32(&calls, 1)
		<-ctx.Done()
		return ToolResultParams{}, ctx.Err()
	})
	startTestClient(t, client)
	ts.wait(t)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("tool handler calls = %d, want 1", got)
	}
}

func TestToolResultRetriesOnBusy(t *testing.T) {
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		if err := writeToolCall(s, "srv_tool", ToolCallParams{
			SessionID:  "sess_test",
			ToolCallID: "toolu_retry",
			Tool:       ToolNameBash,
			Input:      ToolInput{Command: stringPtr("true")},
		}); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_tool"); err != nil {
			return err
		}

		first, err := s.awaitRequest(ctx, MethodToolResult)
		if err != nil {
			return err
		}
		if err := s.write(rpcErrorEnvelope{
			JSONRPC: "2.0",
			ID:      *first.ID,
			Error:   JSONRPCError{Code: JSONRPCErrorCodeExecutorBusy, Message: "executor_busy"},
		}); err != nil {
			return err
		}

		second, err := s.awaitRequest(ctx, MethodToolResult)
		if err != nil {
			return err
		}
		var firstParams, secondParams ToolResultParams
		if err := json.Unmarshal(first.Params, &firstParams); err != nil {
			return err
		}
		if err := json.Unmarshal(second.Params, &secondParams); err != nil {
			return err
		}
		if firstParams.ToolCallID != secondParams.ToolCallID {
			return fmt.Errorf("retry changed tool_call_id: %q != %q", firstParams.ToolCallID, secondParams.ToolCallID)
		}
		return s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *second.ID, Result: Ack{Accepted: true}})
	})

	lost := make(chan error, 1)
	client := newTestClient(t, ts.server.URL, func(options *Options) {
		options.ToolResultRetryAttempts = 3
		options.ToolResultRetryBaseDelay = time.Millisecond
		options.OnToolResultLost = func(_ ToolResultParams, err error) { lost <- err }
	})
	client.OnToolCall(func(_ context.Context, params ToolCallParams) (ToolResultParams, error) {
		return ToolResultParams{
			SessionID:  params.SessionID,
			ToolCallID: params.ToolCallID,
			Outcome:    ToolOutcomeSucceeded,
			Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: stringPtr("ok")}},
		}, nil
	})
	startTestClient(t, client)
	ts.wait(t)
	select {
	case err := <-lost:
		t.Fatalf("tool result reported lost after retry succeeded: %v", err)
	default:
	}
}

func TestToolResultReportsLostAfterRetries(t *testing.T) {
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		if err := writeToolCall(s, "srv_tool", ToolCallParams{
			SessionID:  "sess_test",
			ToolCallID: "toolu_lost",
			Tool:       ToolNameBash,
			Input:      ToolInput{Command: stringPtr("true")},
		}); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_tool"); err != nil {
			return err
		}
		for attempt := 0; attempt < 2; attempt++ {
			frame, err := s.awaitRequest(ctx, MethodToolResult)
			if err != nil {
				return err
			}
			if err := s.write(rpcErrorEnvelope{
				JSONRPC: "2.0",
				ID:      *frame.ID,
				Error:   JSONRPCError{Code: JSONRPCErrorCodeExecutorBusy, Message: "executor_busy"},
			}); err != nil {
				return err
			}
		}
		return nil
	})

	type lostResult struct {
		params ToolResultParams
		err    error
	}
	lost := make(chan lostResult, 1)
	client := newTestClient(t, ts.server.URL, func(options *Options) {
		options.ToolResultRetryAttempts = 2
		options.ToolResultRetryBaseDelay = time.Millisecond
		options.OnToolResultLost = func(params ToolResultParams, err error) {
			lost <- lostResult{params: params, err: err}
		}
	})
	client.OnToolCall(func(_ context.Context, params ToolCallParams) (ToolResultParams, error) {
		return ToolResultParams{
			SessionID:  params.SessionID,
			ToolCallID: params.ToolCallID,
			Outcome:    ToolOutcomeSucceeded,
			Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: stringPtr("ok")}},
		}, nil
	})
	startTestClient(t, client)

	select {
	case result := <-lost:
		if result.params.ToolCallID != "toolu_lost" {
			t.Fatalf("lost tool_call_id = %q", result.params.ToolCallID)
		}
		var rpcErr *RPCError
		if !errors.As(result.err, &rpcErr) || rpcErr.Code != JSONRPCErrorCodeExecutorBusy {
			t.Fatalf("lost error = %v, want -32009", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnToolResultLost was not called")
	}
}

func TestSessionReadyAndEnded(t *testing.T) {
	assigned := make(chan struct{})
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		if err := s.write(map[string]any{
			"jsonrpc": "2.0",
			"id":      "srv_assign",
			"method":  MethodSessionAssign,
			"params": SessionAssignParams{
				SessionID:     "sess_test",
				WorkID:        "work_test",
				AgentID:       "agent_test",
				EnvironmentID: "env_test",
				Lease:         Lease{Token: "lease_test", FencingToken: 1},
			},
		}); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_assign"); err != nil {
			return err
		}
		close(assigned)

		ready, err := s.awaitRequest(ctx, MethodSessionReady)
		if err != nil {
			return err
		}
		var readyParams SessionReadyParams
		if err := json.Unmarshal(ready.Params, &readyParams); err != nil {
			return err
		}
		if readyParams.SessionID != "sess_test" || readyParams.LeaseToken != "lease_test" {
			return fmt.Errorf("unexpected session.ready params: %+v", readyParams)
		}
		if err := s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *ready.ID, Result: Ack{Accepted: true}}); err != nil {
			return err
		}

		ended, err := s.awaitRequest(ctx, MethodSessionEnded)
		if err != nil {
			return err
		}
		var endedParams SessionEndedParams
		if err := json.Unmarshal(ended.Params, &endedParams); err != nil {
			return err
		}
		return s.write(rpcSuccessEnvelope{
			JSONRPC: "2.0",
			ID:      *ended.ID,
			Result:  SessionEndedResult{Accepted: true},
		})
	})

	client := newTestClient(t, ts.server.URL, nil)
	startTestClient(t, client)
	select {
	case <-assigned:
	case <-time.After(5 * time.Second):
		t.Fatal("session.assign was not handled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ack, err := client.SessionReady(ctx, SessionReadyParams{
		SessionID:  "sess_test",
		WorkID:     "work_test",
		LeaseToken: "lease_test",
	})
	if err != nil || !ack.Accepted {
		t.Fatalf("SessionReady = %+v, %v", ack, err)
	}
	ended, err := client.SessionEnded(ctx, SessionEndedParams{SessionID: "sess_test", Reason: "completed"})
	if err != nil || !ended.Accepted {
		t.Fatalf("SessionEnded = %+v, %v", ended, err)
	}
	if active := client.ActiveSessions(); len(active) != 0 {
		t.Fatalf("active sessions after ended = %v", active)
	}
	ts.wait(t)
}

func TestDuplicateToolCallRunsOnce(t *testing.T) {
	var calls int32
	ts := startTestServer(t, func(ctx context.Context, s *testSession) error {
		if err := registerSession(ctx, s, 60000); err != nil {
			return err
		}
		params := ToolCallParams{
			SessionID:  "sess_test",
			ToolCallID: "toolu_once",
			Tool:       ToolNameBash,
			Input:      ToolInput{Command: stringPtr("true")},
		}
		if err := writeToolCall(s, "srv_tool_1", params); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_tool_1"); err != nil {
			return err
		}
		if err := writeToolCall(s, "srv_tool_2", params); err != nil {
			return err
		}
		if _, err := s.awaitResponse(ctx, "srv_tool_2"); err != nil {
			return err
		}
		result, err := s.awaitRequest(ctx, MethodToolResult)
		if err != nil {
			return err
		}
		if err := s.write(rpcSuccessEnvelope{JSONRPC: "2.0", ID: *result.ID, Result: Ack{Accepted: true}}); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		if extra := s.log.count(func(frame rpcInbound) bool { return frame.Method == MethodToolResult }); extra != 1 {
			return fmt.Errorf("tool.result frames = %d, want 1", extra)
		}
		return nil
	})

	client := newTestClient(t, ts.server.URL, nil)
	client.OnToolCall(func(_ context.Context, params ToolCallParams) (ToolResultParams, error) {
		atomic.AddInt32(&calls, 1)
		return ToolResultParams{
			SessionID:  params.SessionID,
			ToolCallID: params.ToolCallID,
			Outcome:    ToolOutcomeSucceeded,
			Content:    []ContentBlock{{Type: ContentBlockTypeText, Text: stringPtr("ok")}},
		}, nil
	})
	startTestClient(t, client)
	ts.wait(t)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("tool handler calls = %d, want 1", got)
	}
}

func TestMCPChannelDialsWithScopedQuery(t *testing.T) {
	handled := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/executors/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
				ExecutorID:   "exec_test",
				Token:        "jwt_mcp",
				ExpiresIn:    300,
				WSURL:        "ws://" + r.Host + "/v1/executors/channel",
				ChannelScope: []ChannelScope{ChannelScopeMcp},
			})
		case "/v1/executors/channel":
			defer func() { handled <- checkMCPRequest(r) }()
			if r.URL.Query().Get("channel") != "mcp" {
				http.Error(w, "channel", http.StatusBadRequest)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{WSSubprotocol}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			_ = ws.Write(ctx, websocket.MessageText, append([]byte("echo:"), data...))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	channel, err := client.OpenMCPChannel(ctx, "sess_test", "chrome")
	if err != nil {
		t.Fatalf("OpenMCPChannel: %v", err)
	}
	defer channel.Close()
	if err := channel.Write(ctx, []byte("ping")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := channel.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "echo:ping" {
		t.Fatalf("Read = %q, want echo:ping", data)
	}
	if err := <-handled; err != nil {
		t.Fatal(err)
	}
}

func checkMCPRequest(r *http.Request) error {
	if got := r.Header.Get("Authorization"); got != "Bearer jwt_mcp" {
		return fmt.Errorf("authorization = %q", got)
	}
	query := r.URL.Query()
	if query.Get("channel") != "mcp" || query.Get("session_id") != "sess_test" || query.Get("server_key") != "chrome" {
		return fmt.Errorf("unexpected mcp query: %s", r.URL.RawQuery)
	}
	return nil
}

func TestMCPChannelURLKeepsExistingQuery(t *testing.T) {
	got, err := mcpChannelURL("wss://example.test/v1/executors/channel?region=eu", "sess_1", "chrome")
	if err != nil {
		t.Fatalf("mcpChannelURL: %v", err)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	query := parsed.Query()
	for key, want := range map[string]string{
		"region":     "eu",
		"channel":    "mcp",
		"session_id": "sess_1",
		"server_key": "chrome",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasPrefix(got, "wss://example.test/v1/executors/channel?") {
		t.Errorf("unexpected URL %q", got)
	}
}

func TestArtifactReferenceHelpers(t *testing.T) {
	ref := NewArtifactRef("art_1", "out.txt", "text/plain", 12)
	if ref.ArtifactID != "art_1" || ref.Name == nil || *ref.Name != "out.txt" {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	if ref.MimeType == nil || *ref.MimeType != "text/plain" || ref.SizeBytes == nil || *ref.SizeBytes != 12 {
		t.Fatalf("unexpected ref: %+v", ref)
	}
	block := NewArtifactContentBlock(ref)
	if block.Type != ContentBlockTypeArtifact || block.ArtifactID == nil || *block.ArtifactID != "art_1" {
		t.Fatalf("unexpected block: %+v", block)
	}
	if block.MimeType == nil || *block.MimeType != "text/plain" {
		t.Fatalf("unexpected block mime: %+v", block)
	}
}
