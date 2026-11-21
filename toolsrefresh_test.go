package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestToolsCredentialRefreshesBeforeExpiryAndBacksOffOnFailure(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	var calls int
	var failNext bool
	var logs []string
	cred := &toolsCredential{
		endpoint: "https://x/v1/machines/m/mcp", token: "tok-0", expiresAt: now.Add(time.Hour),
		now: func() time.Time { return now },
		log: func(level LogLevel, message string, err error) {
			logs = append(logs, fmt.Sprintf("%v %s %v", level, message, err))
		},
		refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			calls++
			if failNext {
				return ToolsTokenRefreshResult{}, errors.New("tools_token_unavailable")
			}
			return ToolsTokenRefreshResult{ToolsMCPToken: fmt.Sprintf("tok-%d", calls), ToolsMCPURL: "https://x/v1/machines/m/mcp",
				ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}, nil
		},
	}
	ctx := context.Background()
	// Plenty of time left: no refresh.
	if _, token := cred.current(ctx); token != "tok-0" || calls != 0 {
		t.Fatalf("token %q after %d refreshes", token, calls)
	}
	// 9 minutes left: refreshed.
	now = now.Add(51 * time.Minute)
	if _, token := cred.current(ctx); token != "tok-1" || calls != 1 {
		t.Fatalf("token %q after %d refreshes", token, calls)
	}
	// A failure is visible, keeps the current token, and is not retried before the backoff.
	now = now.Add(51 * time.Minute)
	failNext = true
	if _, token := cred.current(ctx); token != "tok-1" || calls != 2 {
		t.Fatalf("token %q after %d refreshes", token, calls)
	}
	if len(logs) < 2 || !strings.Contains(logs[len(logs)-1], "tools token refresh failed (attempt 1") || !strings.Contains(logs[len(logs)-1], "tools_token_unavailable") {
		t.Fatalf("the failure was not reported: %v", logs)
	}
	cred.current(ctx)
	if calls != 2 {
		t.Fatalf("retried inside the backoff (%d refreshes)", calls)
	}
	now = now.Add(6 * time.Second)
	cred.current(ctx)
	if calls != 3 || !strings.Contains(logs[len(logs)-1], "attempt 2") {
		t.Fatalf("calls %d, logs %v", calls, logs)
	}
	// A refresh that works again resets the backoff.
	failNext = false
	now = now.Add(11 * time.Second)
	if _, token := cred.current(ctx); token != "tok-4" {
		t.Fatalf("token %q", token)
	}
	// The token never shows in the credential's own printing.
	if s := fmt.Sprintf("%v %+v %s %#v", cred, cred, cred, cred); strings.Contains(s, "tok-") {
		t.Fatalf("token printed: %s", s)
	}
}

func TestToolsCredentialRejectsAnUnusableRefreshAndNeverRefreshesWithoutExpiry(t *testing.T) {
	now := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	cred := &toolsCredential{endpoint: "https://x/mcp", token: "tok-0", expiresAt: now.Add(time.Minute), now: func() time.Time { return now },
		refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			return ToolsTokenRefreshResult{ToolsMCPToken: "", ToolsMCPURL: "https://x/mcp", ExpiresAt: "garbage"}, nil
		}}
	if _, token := cred.current(context.Background()); token != "tok-0" || cred.failures != 1 {
		t.Fatalf("token %q failures %d", token, cred.failures)
	}
	unknownExpiry := &toolsCredential{endpoint: "https://x/mcp", token: "tok-0", now: func() time.Time { return now },
		refresh: func(context.Context) (ToolsTokenRefreshResult, error) {
			t.Fatal("refreshed a credential without an expiry")
			return ToolsTokenRefreshResult{}, nil
		}}
	unknownExpiry.current(context.Background())
}

func TestPlatformToolRouteReopensItsSessionWhenTheEndpointMoves(t *testing.T) {
	r := newPlatformToolRoute(PlatformToolRouteOptions{Endpoint: "https://a/mcp", Token: "t", TurnID: "turn"})
	r.initialized, r.mcpSession = true, "s1"
	r.cred.endpoint = "https://b/mcp"
	r.prepare(context.Background())
	if r.initialized || r.mcpSession != "" || r.endpoint != "https://b/mcp" {
		t.Fatalf("route = %+v", r)
	}
}

func TestIdempotencyKeyIsNULSeparated(t *testing.T) {
	// "a" "bc" and "ab" "c" must not collide, and the key is the first 128
	// bits of sha256 over session NUL turn NUL call (ADR-0005).
	if platformIdempotencyKey("a", "bc", "d") == platformIdempotencyKey("ab", "c", "d") {
		t.Fatal("keys collide")
	}
	// printf 'sesn_t1\0turn_1\0c1' | shasum -a 256 | cut -c1-32
	if got := platformIdempotencyKey("sesn_t1", "turn_1", "c1"); got != "bma-tool-360a2952a370d939e4c33ba3a96c579e" {
		t.Fatalf("key = %s", got)
	}
}
