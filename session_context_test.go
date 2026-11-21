package bmaexecutor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDaemonSessions serves the two session context routes of the BMA
// daemon API (GET /api/v1/daemon/processes/{id}/session and
// GET /api/v1/daemon/sessions/{id}) for one machine token.
type fakeDaemonSessions struct {
	token     string
	sessions  map[string]string // session id -> response body
	processes map[string]string // process id -> session id
	hits      atomic.Int32
}

func (f *fakeDaemonSessions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var sessionID string
	switch {
	case r.Method != http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case strings.HasPrefix(r.URL.Path, "/api/v1/daemon/processes/") && strings.HasSuffix(r.URL.Path, "/session"):
		processID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/daemon/processes/"), "/session")
		sessionID = f.processes[processID]
	case strings.HasPrefix(r.URL.Path, "/api/v1/daemon/sessions/"):
		sessionID = strings.TrimPrefix(r.URL.Path, "/api/v1/daemon/sessions/")
	}
	body, ok := f.sessions[sessionID]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","message":"session not bound to this machine"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func newSessionContextClient(t *testing.T, server *httptest.Server, configure func(*Options)) *Client {
	t.Helper()
	options := Options{
		BaseURL: server.URL + "/api/v1", EnvironmentKey: "mtok", Executor: ExecutorInfo{Name: "ctx-test"},
	}
	if configure != nil {
		configure(&options)
	}
	client, err := NewClient(options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSessionContextByProcessAndByIDAreCachedPerSession(t *testing.T) {
	daemon := &fakeDaemonSessions{
		token: "mtok",
		sessions: map[string]string{"sesn_a": `{"id":"sesn_a","environment_id":"mch_1",` +
			`"metadata":{"owner":"u_1","app_ref":"ws_7"},"created_at":"2026-11-07T00:00:00Z"}`},
		processes: map[string]string{"proc_1": "sesn_a", "proc_2": "sesn_a"},
	}
	server := httptest.NewServer(daemon)
	defer server.Close()
	client := newSessionContextClient(t, server, nil)
	ctx := testContext(t)

	got, err := client.SessionContext(ctx, "proc_1")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	if got.ID != "sesn_a" || got.EnvironmentID != "mch_1" || got.Metadata["owner"] != "u_1" ||
		got.Metadata["app_ref"] != "ws_7" || !got.CreatedAt.Equal(want) {
		t.Fatalf("session context = %+v", got)
	}
	// The same process and the same session are served from the cache; a
	// second process of the session is one lookup.
	if _, err := client.SessionContext(ctx, "proc_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SessionContextByID(ctx, "sesn_a"); err != nil {
		t.Fatal(err)
	}
	if hits := daemon.hits.Load(); hits != 1 {
		t.Fatalf("daemon hits = %d, want 1", hits)
	}
	if _, err := client.SessionContext(ctx, "proc_2"); err != nil {
		t.Fatal(err)
	}
	if hits := daemon.hits.Load(); hits != 2 {
		t.Fatalf("daemon hits = %d, want 2", hits)
	}
	// A caller cannot change the cached metadata.
	got.Metadata["owner"] = "u_2"
	again, _ := client.SessionContextByID(ctx, "sesn_a")
	if again.Metadata["owner"] != "u_1" {
		t.Fatal("cached metadata was modified through a returned map")
	}
}

func TestSessionContextNotFoundIsNotCached(t *testing.T) {
	daemon := &fakeDaemonSessions{token: "mtok", sessions: map[string]string{}, processes: map[string]string{}}
	server := httptest.NewServer(daemon)
	defer server.Close()
	client := newSessionContextClient(t, server, nil)
	ctx := testContext(t)
	for _, call := range []func() error{
		func() error { _, err := client.SessionContext(ctx, "proc_other_machine"); return err },
		func() error { _, err := client.SessionContextByID(ctx, "sesn_other"); return err },
	} {
		err := call()
		if !errors.Is(err, ErrSessionContextNotFound) {
			t.Fatalf("error = %v, want ErrSessionContextNotFound", err)
		}
	}
	daemon.sessions["sesn_other"] = `{"id":"sesn_other","environment_id":"mch_1","metadata":{},"created_at":"2026-11-07T00:00:00Z"}`
	if _, err := client.SessionContextByID(ctx, "sesn_other"); err != nil {
		t.Fatalf("after the session was bound: %v", err)
	}
}

func TestSessionContextNeedsAMachineToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for name, configure := range map[string]func(*Options){
		"environment-scoped": func(o *Options) { o.EnvironmentID = "env_1" },
		"token source only":  func(o *Options) { o.EnvironmentKey = ""; o.TokenSource = StaticToken("ws://x", "") },
	} {
		client := newSessionContextClient(t, server, configure)
		if _, err := client.SessionContextByID(testContext(t), "sesn_a"); !errors.Is(err, ErrMachineTokenRequired) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestSessionContextReportsServerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer server.Close()
	client := newSessionContextClient(t, server, nil)
	_, err := client.SessionContextByID(testContext(t), "sesn_a")
	var status *DaemonHTTPError
	if !errors.As(err, &status) || status.StatusCode != http.StatusBadGateway || errors.Is(err, ErrSessionContextNotFound) {
		t.Fatalf("error = %v", err)
	}
	// Path segments are escaped, never interpreted.
	if _, err := client.SessionContextByID(testContext(t), "../processes/x"); err == nil {
		t.Fatal("an unsafe session id was accepted")
	}
}
