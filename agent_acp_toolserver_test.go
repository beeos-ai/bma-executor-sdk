package bmaexecutor

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// blockingRoute answers a call only once release is closed.
type blockingRoute struct{ release chan struct{} }

func (b blockingRoute) Call(ctx context.Context, _ RouteCall) *RouteResult {
	select {
	case <-b.release:
		return &RouteResult{Outcome: RouteOutcomeExecuted, Text: "ran"}
	case <-ctx.Done():
		return failure(RouteOutcomeUnknown, "canceled", "abandoned").result
	}
}

// A tools/call can outlast Claude Code's first-byte budget for an HTTP MCP
// request (60 s; an approval, a brain reconnecting, a long command): the tool
// server sends the response headers at once and the result as the one event
// of an SSE stream (MCP Streamable HTTP).
func TestACPToolServerStreamsALongToolCall(t *testing.T) {
	a := &ACPAgent{sessions: map[string]*acpSession{}}
	srv, err := startACPToolServer(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.close)
	route := blockingRoute{release: make(chan struct{})}
	turnCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.sessions["ses"] = &acpSession{id: "ses", placeholder: "ph-1", agentMode: "default",
		turn: &acpTurn{ctx: turnCtx, toolRoute: route, tools: map[string]string{}}}

	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"bash","arguments":{"command":"true"},"_meta":{"claudecode/toolUseId":"toolu_1"}}}`
	req, _ := http.NewRequest(http.MethodPost, srv.url+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ph-1")
	req.Header.Set("Accept", "application/json, text/event-stream")
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		got <- answer{resp, err}
	}()
	var resp *http.Response
	select {
	case a := <-got:
		if a.err != nil {
			t.Fatal(a.err)
		}
		resp = a.resp
	case <-time.After(3 * time.Second):
		t.Fatal("no response headers while the call is still running")
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("status %d content-type %q, want 200 text/event-stream", resp.StatusCode, ct)
	}
	close(route.release)
	sc := bufio.NewScanner(resp.Body)
	var data string
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			data = d
			break
		}
	}
	var msg struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &msg); err != nil {
		t.Fatalf("event data %q: %v", data, err)
	}
	if msg.ID != 7 || msg.Result.IsError || len(msg.Result.Content) != 1 || msg.Result.Content[0].Text != "ran" {
		t.Fatalf("event = %+v", msg)
	}
}
