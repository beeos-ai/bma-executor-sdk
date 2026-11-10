//go:build !windows

package bmaexecutor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func platformCall(t *testing.T, route ToolRoute, ctx context.Context, id string) *RouteResult {
	t.Helper()
	return route.Call(ctx, RouteCall{ID: id, ToolUseID: "evt_1", Name: "bash", Input: wireInput(bashInput("true"))})
}

// What the route puts on the wire: the handshake once, the credential, the
// _meta keys of ADR-0005.
func TestPlatformToolRouteWire(t *testing.T) {
	gw := newGateway(t, map[string]string{"bash": "ask"})
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: gw.URL(), Token: "tok-1", TurnID: rigTurn, SyncWait: 2 * time.Second,
		ApprovalFor: func(string) string { return "appr_x" }})
	platformCall(t, route, context.Background(), "c1")
	platformCall(t, route, context.Background(), "c2")
	var methods []string
	for _, r := range gw.Recorded() {
		methods = append(methods, r.Method)
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Fatalf("%s: Authorization = %q", r.Method, r.Header.Get("Authorization"))
		}
	}
	if got := strings.Join(methods, ","); got != "initialize,notifications/initialized,tools/call,tools/call" {
		t.Fatalf("requests = %s", got)
	}
	last := gw.Recorded()[3]
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
	gw := newGateway(t, map[string]string{"bash": "allow"})
	gw.SetSSE(true)
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: gw.URL(), Token: "tok-1", TurnID: rigTurn})
	wantExecuted(t, platformCall(t, route, context.Background(), "c1"))
}

// A gateway that cannot be reached did not run the call; one that dies after
// the request was sent leaves it unknown; both are errors for the brain.
func TestPlatformToolRouteTransportFailures(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	noErr(t, err)
	addr := listener.Addr().String()
	noErr(t, listener.Close())
	down := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: "http://" + addr, Token: "tok-1", TurnID: rigTurn})
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
	shortRetries(t, 300*time.Millisecond)
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: dying.URL, Token: "tok-1", TurnID: rigTurn})
	got := platformCall(t, route, context.Background(), "c1")
	if got.Outcome != RouteOutcomeUnknown || got.Code != "transport_error" || !got.IsError {
		t.Fatalf("answer = %+v", got)
	}
}

func shortRetries(t *testing.T, window time.Duration) {
	t.Helper()
	w, b, m := platformRetryWindow, platformRetryBase, platformRetryMax
	platformRetryWindow, platformRetryBase, platformRetryMax = window, 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { platformRetryWindow, platformRetryBase, platformRetryMax = w, b, m })
}

// A call whose answer was lost after it was sent (the brain's network
// dropped) is sent again with the same idempotency key, through a moment the
// gateway cannot be reached and an in_progress reply, until its saved reply
// comes back (ADR-0029 §4.3, E4): one execution, replayed=true.
func TestPlatformToolRouteRetriesALostCallWithItsKey(t *testing.T) {
	shortRetries(t, 5*time.Second)
	var calls []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, buf)
		body := string(buf)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(body, `"tools/call"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		key := regexp.MustCompile(`bma-tool-[0-9a-f]{32}`).FindString(body)
		calls = append(calls, key)
		id := regexp.MustCompile(`"id":(\d+)`).FindStringSubmatch(body)[1]
		switch len(calls) {
		case 1: // sent, then the connection drops
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		case 3:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"isError":true,"content":[{"type":"text","text":"in_progress: still running"}],"structuredContent":{"outcome":"not_forwarded","error_code":"in_progress"}}}`, id)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ran\nexit_code: 0"}],"structuredContent":{"outcome":"executed","replayed":true}}}`, id)
		}
	}))
	t.Cleanup(srv.Close)
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: srv.URL, Token: "tok-1", TurnID: rigTurn})
	got := platformCall(t, route, context.Background(), "c1")
	if got.Outcome != RouteOutcomeExecuted || got.IsError || !got.Replayed || got.Text != "ran\nexit_code: 0" {
		t.Fatalf("answer = %+v", got)
	}
	if len(calls) != 4 || calls[0] == "" || calls[0] != calls[1] || calls[1] != calls[2] || calls[2] != calls[3] {
		t.Fatalf("tools/call keys = %v, want 4 sends of one key", calls)
	}

	// A refusal is final: no retry.
	calls = nil
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, buf)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(buf), `"tools/call"`) {
			calls = append(calls, "x")
			id := regexp.MustCompile(`"id":(\d+)`).FindStringSubmatch(string(buf))[1]
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"isError":true,"content":[{"type":"text","text":"denied"}],"structuredContent":{"outcome":"not_forwarded","error_code":"permission_denied"}}}`, id)
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(refusing.Close)
	route = newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: refusing.URL, Token: "tok-1", TurnID: rigTurn})
	if got := platformCall(t, route, context.Background(), "c2"); got.Code != "permission_denied" || len(calls) != 1 {
		t.Fatalf("answer = %+v after %d sends", got, len(calls))
	}
}

// A brain that gives up on a call (its turn ended) gets an answer at once;
// the hands' process is the platform's to end, not the connection's.
func TestPlatformToolRouteAbandonedCall(t *testing.T) {
	gw := newGateway(t, map[string]string{"bash": "allow"})
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: gw.URL(), Token: "tok-1", TurnID: rigTurn})
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan *RouteResult, 1)
	go func() {
		answer <- route.Call(ctx, RouteCall{ID: "c1", Name: "bash", Input: wireInput(bashInput("sleep 20 & echo $! > child.pid; wait"))})
	}()
	pid := waitForPID(t, gw.Workspace()+"/child.pid")
	cancel()
	select {
	case got := <-answer:
		if got.Outcome != RouteOutcomeUnknown || got.Code != "canceled" {
			t.Fatalf("answer = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the abandoned call did not return")
	}
	if !processAlive(pid) {
		t.Fatal("the brain's disconnect killed the hands' process")
	}
	gw.CancelTurn() // the platform's cancel does
}

// The tool credential is in memory only and in no text the route produces.
func TestPlatformToolRouteKeepsItsTokenToItself(t *testing.T) {
	const token = "tok-secret-value"
	gw := newGateway(t, map[string]string{"bash": "allow"})
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: gw.URL(), Token: token, TurnID: rigTurn})
	got := platformCall(t, route, context.Background(), "c1") // refused: not the gateway's token
	wantRefused(t, got, "stale_turn")
	for _, text := range []string{got.Text, fmt.Sprintf("%v %+v %#v %s", route, route, route, route)} {
		if strings.Contains(text, token) {
			t.Fatalf("the token leaked into %q", text)
		}
	}
}

// The executor reconnected while a call was in flight: the session moved to a
// new epoch, so the gateway refuses the turn's old credential (401). The route
// gets a fresh one through tools_token.refresh and sends the same call (same
// key) again, which returns the saved reply (E4); without a fresh credential
// the refusal stands.
func TestPlatformToolRouteRenewsAStaleCredentialAndRetries(t *testing.T) {
	shortRetries(t, 5*time.Second)
	var keys, tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = io.ReadFull(r.Body, buf)
		body := string(buf)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer tok-2" && len(keys) > 0 {
			w.WriteHeader(http.StatusUnauthorized) // the old epoch's token after the reconnect
			return
		}
		if !strings.Contains(body, `"tools/call"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		keys = append(keys, regexp.MustCompile(`bma-tool-[0-9a-f]{32}`).FindString(body))
		tokens = append(tokens, r.Header.Get("Authorization"))
		id := regexp.MustCompile(`"id":(\d+)`).FindStringSubmatch(body)[1]
		if len(keys) == 1 { // sent, then the brain's network drops
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ran"}],"structuredContent":{"outcome":"executed","replayed":true}}}`, id)
	}))
	t.Cleanup(srv.Close)
	refreshes := 0
	route := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: srv.URL, Token: "tok-1", TurnID: rigTurn,
		ExpiresAt: time.Now().Add(time.Hour),
		Refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			refreshes++
			return ToolsTokenRefreshResult{ToolsMCPURL: srv.URL, ToolsMCPToken: "tok-2",
				ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, nil
		}})
	got := platformCall(t, route, context.Background(), "c1")
	if got.Outcome != RouteOutcomeExecuted || !got.Replayed || refreshes != 1 {
		t.Fatalf("answer = %+v after %d refreshes", got, refreshes)
	}
	if len(keys) != 2 || keys[0] != keys[1] || tokens[1] != "Bearer tok-2" {
		t.Fatalf("tools/call keys %v tokens %v, want the same key twice, the second with the fresh token", keys, tokens)
	}

	// Refresh refused (the turn is over): the refusal stands, nothing ran.
	refusing := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: srv.URL, Token: "tok-1", TurnID: rigTurn,
		ExpiresAt: time.Now().Add(time.Hour),
		Refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			return ToolsTokenRefreshResult{}, fmt.Errorf("turn_inactive")
		}})
	wantRefused(t, platformCall(t, refusing, context.Background(), "c2"), "stale_turn")

	// The refusal arrives while the executor channel is still reconnecting
	// (staging E4: refresh 0.4 s before the channel was back): the refresh
	// waits for the channel, then the same call (same key) goes again.
	keys, tokens = nil, nil
	waiting := 0
	reconnecting := newPlatformToolRoute(PlatformToolRouteOptions{SessionID: rigSession, Endpoint: srv.URL, Token: "tok-1", TurnID: rigTurn,
		ExpiresAt: time.Now().Add(time.Hour),
		Refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			if waiting++; waiting < 3 {
				return ToolsTokenRefreshResult{}, fmt.Errorf("refresh: %w", ErrNotConnected)
			}
			return ToolsTokenRefreshResult{ToolsMCPURL: srv.URL, ToolsMCPToken: "tok-2",
				ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, nil
		}})
	got = platformCall(t, reconnecting, context.Background(), "c3")
	if got.Outcome != RouteOutcomeExecuted || waiting != 3 {
		t.Fatalf("answer = %+v after %d refreshes, want executed after the channel came back", got, waiting)
	}
	if n := len(keys); n == 0 || tokens[n-1] != "Bearer tok-2" {
		t.Fatalf("tools/call tokens %v, want the last with the fresh token", tokens)
	}
}
