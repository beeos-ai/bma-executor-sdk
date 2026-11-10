package bmaexecutor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func (u *fakeUpstream) sessionHeaders() [][]string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.sessions)
}

// Upstream.SessionHeader: a request forwarded to the host's endpoint carries
// the platform session id of its turn. The fake agent sends the same header
// with "sesn_forged" on every request; that value never goes upstream.
func TestACPAgentModelProxySessionHeader(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform := newRecordingPlatform()
	f := startACPFixtureOn(t, ctx, ACPAgentOptions{Upstream: ACPModelUpstream{SessionHeader: fakeSessionHeader}}, platform)
	f.start(t, ctx, "sesn_h", "turn_1", "model")
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := f.upstream.sessionHeaders(); len(got) != 1 || !slices.Equal(got[0], []string{"sesn_h"}) {
		t.Fatalf("upstream session header = %v, want the turn's session id once", got)
	}
	if got := f.upstream.seen(); !slices.Equal(got, []string{"Bearer node-owner-key"}) {
		t.Fatalf("upstream auth = %v", got)
	}

	// The platform's own model endpoint is not the host's: no session header.
	f.start(t, ctx, "sesn_h", "turn_2", "model", platformModel(f.upstream.server.URL+"/v1", "tok_turn", time.Time{}))
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if got := f.upstream.sessionHeaders(); len(got) != 2 || len(got[1]) != 0 {
		t.Fatalf("platform route: upstream session header = %v, want none", got)
	}
	if got := f.upstream.seen(); got[1] != "Bearer tok_turn" {
		t.Fatalf("platform route: upstream auth = %v", got)
	}
}

func TestACPAgentModelProxyWithoutSessionHeaderSendsNone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	f.start(t, ctx, "sesn_n", "turn_1", "model")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	// Not the agent's own header either.
	if got := f.upstream.sessionHeaders(); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("upstream session header = %v, want none", got)
	}
}

// Two sessions with model requests in flight at the same time: each request
// carries the id of its own session. A request is told apart by its turn's model.
func TestACPAgentModelProxySessionHeaderOfConcurrentSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	got := map[string][]string{} // the request's model -> its session header values
	both := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got[body.Model] = append(got[body.Model], r.Header.Values(fakeSessionHeader)...)
		if len(got) == 2 {
			select {
			case <-both:
			default:
				close(both)
			}
		}
		mu.Unlock()
		// Answer only when both sessions' requests have arrived.
		select {
		case <-both:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"upstream-ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	f := startACPFixture(t, ctx, ACPAgentOptions{
		Upstream: ACPModelUpstream{BaseURL: model.URL + "/v1", Model: "m-a", SessionHeader: fakeSessionHeader},
		Models:   []AgentModel{{ID: "m-a"}, {ID: "m-b"}}})
	f.start(t, ctx, "sesn_a", "turn_a", "model", func(p *TurnStartParams) { p.Config.Model = "m-a" })
	f.start(t, ctx, "sesn_b", "turn_b", "model", func(p *TurnStartParams) { p.Config.Model = "m-b" })
	assertIdle(t, f.platform.waitIdle(t, "turn_a"), TurnOutcomeCompleted)
	assertIdle(t, f.platform.waitIdle(t, "turn_b"), TurnOutcomeCompleted)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !slices.Equal(got["m-a"], []string{"sesn_a"}) || !slices.Equal(got["m-b"], []string{"sesn_b"}) {
		t.Fatalf("session header by model = %v, want m-a: sesn_a, m-b: sesn_b", got)
	}
}

func TestACPAgentRefusesAnInvalidSessionHeader(t *testing.T) {
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	exe, err := os.Executable() // absolute on every OS; the options are refused before it would start
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X Usage Session", "X-Usage-Session:", "会话"} {
		_, err := NewACPAgent(context.Background(), ACPAgentOptions{Command: []string{exe}, Core: core, StateDir: t.TempDir(),
			Upstream: ACPModelUpstream{BaseURL: "http://127.0.0.1:1/v1", SessionHeader: name}})
		if err == nil || !strings.Contains(err.Error(), "SessionHeader") {
			t.Fatalf("%q: %v", name, err)
		}
	}
}
