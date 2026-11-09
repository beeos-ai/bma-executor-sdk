//go:build !windows

package bmaexecutor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func platformCall(t *testing.T, route toolRoute, ctx context.Context, id string) *acpToolResult {
	t.Helper()
	return route.call(ctx, routedCall{SessionID: rigSession, CallID: id, ToolUseID: "evt_1", Name: "bash", Input: wireInput(bashInput("true"))})
}

// What the route puts on the wire: the handshake once, the credential, the
// _meta keys of ADR-0005.
func TestPlatformToolRouteWire(t *testing.T) {
	gw := newStubGateway(t, map[string]string{"bash": "ask"})
	route := newPlatformToolRoute(platformToolRouteOptions{Endpoint: gw.server.URL, Token: "tok-1", TurnID: rigTurn, SyncWait: 2 * time.Second,
		ApprovalFor: func(string) string { return "appr_x" }})
	platformCall(t, route, context.Background(), "c1")
	platformCall(t, route, context.Background(), "c2")
	var methods []string
	for _, r := range gw.recorded() {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Fatalf("%s: Authorization = %q", r.Method, r.Header.Get("Authorization"))
		}
	}
	if got := strings.Join(methods, ","); got != "initialize,notifications/initialized,tools/call,tools/call" {
		t.Fatalf("requests = %s", got)
	}
	last := gw.recorded()[3]
	if last.Header.Get("Mcp-Session-Id") != "mcp-session-1" {
		t.Fatalf("session header = %q", last.Header.Get("Mcp-Session-Id"))
	}
	params := string(last.Params)
	if !regexp.MustCompile(`"ai.beeos.bma/idempotency_key":"bma-tool-[0-9a-f]{32}"`).MatchString(params) ||
		!strings.Contains(params, `"ai.beeos.bma/approval":"appr_x"`) || !strings.Contains(params, `"ai.beeos.bma/sync_wait_ms":2000`) ||
		!strings.Contains(params, `"ai.beeos.bma/tool_use_id":"evt_1"`) {
		t.Fatalf("params = %s", params)
	}
}

func TestPlatformToolRouteReadsServerSentEvents(t *testing.T) {
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	gw.sse = true
	route := newPlatformToolRoute(platformToolRouteOptions{Endpoint: gw.server.URL, Token: "tok-1", TurnID: rigTurn})
	wantExecuted(t, platformCall(t, route, context.Background(), "c1"))
}

// A gateway that cannot be reached did not run the call; one that dies after
// the request was sent leaves it unknown; both are errors for the brain.
func TestPlatformToolRouteTransportFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	noErr(t, err)
	addr := listener.Addr().String()
	noErr(t, listener.Close())
	down := newPlatformToolRoute(platformToolRouteOptions{Endpoint: "http://" + addr, Token: "tok-1", TurnID: rigTurn})
	wantRefused(t, platformCall(t, down, context.Background(), "c1"), "tools_unavailable")

	dying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > 0 {
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			if strings.Contains(string(buf), `"tools/call"`) {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close() // the call was sent; no answer
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(dying.Close)
	route := newPlatformToolRoute(platformToolRouteOptions{Endpoint: dying.URL, Token: "tok-1", TurnID: rigTurn})
	got := platformCall(t, route, context.Background(), "c1")
	if got.Outcome != acpToolUnknown || got.Code != "transport_error" || !got.IsError {
		t.Fatalf("answer = %+v", got)
	}
}

// A brain that gives up on a call (its turn ended) gets an answer at once;
// the hands' process is the platform's to end, not the connection's.
func TestPlatformToolRouteAbandonedCall(t *testing.T) {
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	route := newPlatformToolRoute(platformToolRouteOptions{Endpoint: gw.server.URL, Token: "tok-1", TurnID: rigTurn})
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan *acpToolResult, 1)
	go func() {
		answer <- route.call(ctx, routedCall{SessionID: rigSession, CallID: "c1", Name: "bash", Input: wireInput(bashInput("sleep 20 & echo $! > child.pid; wait"))})
	}()
	pid := waitForPID(t, gw.workspace+"/child.pid")
	cancel()
	select {
	case got := <-answer:
		if got.Outcome != acpToolUnknown || got.Code != "canceled" {
			t.Fatalf("answer = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the abandoned call did not return")
	}
	if !processAlive(pid) {
		t.Fatal("the brain's disconnect killed the hands' process")
	}
	gw.cancelTurn() // the platform's cancel does
}

// The tool credential is in memory only and in no text the route produces.
func TestPlatformToolRouteKeepsItsTokenToItself(t *testing.T) {
	const token = "tok-secret-value"
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	route := newPlatformToolRoute(platformToolRouteOptions{Endpoint: gw.server.URL, Token: token, TurnID: rigTurn})
	got := platformCall(t, route, context.Background(), "c1") // refused: not the gateway's token
	wantRefused(t, got, "stale_turn")
	for _, text := range []string{got.Text, fmt.Sprintf("%v %+v %#v %s", route, route, route, route)} {
		if strings.Contains(text, token) {
			t.Fatalf("the token leaked into %q", text)
		}
	}
}
