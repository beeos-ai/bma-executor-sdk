//go:build darwin || linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// runCLI runs bma with args against a fresh config directory and answers
// stdout and the exit code.
func runCLI(t *testing.T, env map[string]string, a ...string) (string, int) {
	t.Helper()
	t.Setenv("BMA_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	t.Setenv("BMA_API_KEY", "")
	for k, v := range env {
		t.Setenv(k, v)
	}
	opts = globals{}
	var out bytes.Buffer
	stdout = &out
	defer func() { stdout = os.Stdout }()
	root := newRoot()
	root.SetArgs(a)
	err := root.ExecuteContext(context.Background())
	code := 0
	if err != nil {
		code = exitCodeOf(err)
	}
	return out.String(), code
}

// Every operation of the document has exactly one command, and every
// generated command one operation (ADR-0102).
func TestEveryOperationHasExactlyOneCommand(t *testing.T) {
	served := map[string]int{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, id := range strings.Split(c.Annotations["operation"], ",") {
			if id != "" {
				served[id]++
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(newRoot())
	ids := map[string]bool{}
	for _, op := range apiOperations {
		ids[op.ID] = true
		if served[op.ID] != 1 {
			t.Errorf("operation %s (%s %s) has %d commands, want 1", op.ID, op.Method, op.Path, served[op.ID])
		}
	}
	for id := range served {
		if !ids[id] {
			t.Errorf("command names operation %s, which the document does not have", id)
		}
	}
}

// api_gen.go is what tools/cligen makes of the document.
func TestGeneratedCommandsMatchTheDocument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "api_gen.go")
	cmd := exec.Command("go", "run", "./tools/cligen", "-spec", "contracts/bma-api/bma.openapi.yaml", "-out", out)
	cmd.Dir = "../.."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cligen: %v\n%s", err, b)
	}
	want, _ := os.ReadFile(out)
	got, _ := os.ReadFile("api_gen.go")
	if !bytes.Equal(want, got) {
		t.Fatal("cmd/bma/api_gen.go is stale: go run ./tools/cligen -spec contracts/bma-api/bma.openapi.yaml -out cmd/bma/api_gen.go")
	}
}

func TestBuildRequestFillsPathQueryAndBody(t *testing.T) {
	op := &apiOp{Method: "POST", Path: "/v1/things/{thing_id}", Body: true, Params: []apiParam{
		{Name: "thing_id", In: "path"},
		{Name: "dry_run", Flag: "dry-run", In: "query", Type: "boolean"},
		{Name: "count", Flag: "count", In: "body", Type: "integer"},
		{Name: "labels", Flag: "labels", In: "body", Type: "json"},
		{Name: "mode", Flag: "mode", In: "body", Type: "string", Enum: []string{"a", "b"}},
	}}
	set := map[string]string{"dry-run": "true", "count": "3", "labels": "[x, y]"}
	path, q, body, err := buildRequest(op, []string{"a b"}, `{"name": "n", "count": 1}`, func(f string) (string, bool) {
		v, ok := set[f]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "v1/things/a%20b" || q.Get("dry_run") != "true" {
		t.Fatalf("path %q query %v", path, q)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["name"] != "n" || m["count"] != float64(3) || fmt.Sprint(m["labels"]) != "[x y]" {
		t.Fatalf("body %s", body)
	}
	// An array query parameter is sent once per value.
	arr := &apiOp{Method: "GET", Path: "/v1/report", Params: []apiParam{
		{Name: "group_by[]", Flag: "group-by", In: "query", Type: "json"}}}
	_, q, _, err = buildRequest(arr, nil, "", func(string) (string, bool) { return "[workspace_id, description]", true })
	if err != nil || q.Encode() != "group_by%5B%5D=workspace_id&group_by%5B%5D=description" {
		t.Fatalf("array query %v, %v", q, err)
	}
	set["mode"] = "c"
	if _, _, _, err := buildRequest(op, []string{"x"}, "", func(f string) (string, bool) { v, ok := set[f]; return v, ok }); exitCodeOf(err) != 2 {
		t.Fatalf("an enum outside its values is a usage error: %v", err)
	}
}

// fakeAPI answers the requests of the tests.
func fakeAPI(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestListAllFollowsPagesAndQueryFilters(t *testing.T) {
	var auth, ws string
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth, ws = r.Header.Get("Authorization"), r.Header.Get("bma-workspace-id")
		if r.URL.Path != "/v1/machines" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "" {
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}],"next_page":"p2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m2"}],"next_page":null}`)
	})
	out, code := runCLI(t, map[string]string{"BMA_API_KEY": "bma_org_v1_k", "BMA_BASE_URL": url, "BMA_WORKSPACE_ID": "ws1"},
		"machines", "list", "--all", "--query", ".[].id")
	if code != 0 || out != "m1\nm2\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	if auth != "Bearer bma_org_v1_k" || ws != "ws1" {
		t.Fatalf("credential %q workspace %q", auth, ws)
	}
}

func TestExitCodesFollowTheAPIAnswer(t *testing.T) {
	for status, want := range map[int]int{401: 3, 403: 3, 404: 4, 409: 5, 400: 1} {
		url := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"no"}}`)
		})
		if _, code := runCLI(t, map[string]string{"BMA_API_KEY": "k", "BMA_BASE_URL": url}, "machines", "get", "m1"); code != want {
			t.Errorf("status %d: exit %d, want %d", status, code, want)
		}
	}
	if _, code := runCLI(t, nil, "machines", "get"); code != 2 {
		t.Errorf("missing argument: exit %d, want 2", code)
	}
	if _, code := runCLI(t, nil, "machines", "get", "m1"); code != 3 {
		t.Errorf("not signed in: exit %d, want 3", code)
	}
	if _, code := runCLI(t, nil, "no-such-command"); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
}

// A dropped stream resumes with Last-Event-ID and repeats nothing.
func TestEventStreamResumesAfterADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	var resumedFrom string
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions/s1/events/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "id: e1\ndata: {\"type\":\"session.status_running\"}\n\n")
			return // dropped
		}
		resumedFrom = r.Header.Get("Last-Event-ID")
		_, _ = io.WriteString(w, ": keep-alive\n\nid: e2\ndata: {\"type\":\"agent.message\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}\n\nid: e3\ndata: {\"type\":\"session.status_idle\"}\n\n")
	})
	out, code := runCLI(t, map[string]string{"BMA_API_KEY": "k", "BMA_BASE_URL": url},
		"sessions", "events", "stream", "s1", "--until-idle", "--query", ".type")
	if code != 0 || resumedFrom != "e1" || out != "session.status_running\nagent.message\nsession.status_idle\n" {
		t.Fatalf("code %d resumed from %q out %q", code, resumedFrom, out)
	}
}

func TestSendWaitPrintsTheAgentTextOfTheTurn(t *testing.T) {
	var sent atomic.Bool
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/sessions/s1/events":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"text":"hello"`) {
				http.Error(w, "bad body", 400)
				return
			}
			sent.Store(true)
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.URL.Path == "/v1/sessions/s1/events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			for !sent.Load() {
				if r.Context().Err() != nil {
					return
				}
			}
			_, _ = io.WriteString(w, "id: 1\ndata: {\"type\":\"session.status_running\"}\n\nid: 2\ndata: {\"type\":\"agent.message\",\"content\":[{\"type\":\"text\",\"text\":\"hi there\"}]}\n\nid: 3\ndata: {\"type\":\"session.status_idle\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	out, code := runCLI(t, map[string]string{"BMA_API_KEY": "k", "BMA_BASE_URL": url}, "sessions", "send", "s1", "hello", "--wait")
	if code != 0 || out != "hi there\n" {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestLoginWithTheDeviceGrantStoresTheTokenAndScope(t *testing.T) {
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["client_id"] != clientCLI {
			http.Error(w, "client", 400)
			return
		}
		switch r.URL.Path {
		case "/v1/auth/device_authorizations":
			_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri":"u","verification_uri_complete":"u?c","expires_in":60,"interval":0}`)
		case "/v1/auth/device_authorizations/token":
			_, _ = io.WriteString(w, `{"access_token":"bma_pat_v1_x","token_type":"bearer","bma_organization_id":"org_1","bma_workspace_id":"proj_1"}`)
		}
	})
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("BMA_CONFIG_DIR", dir)
	opts = globals{}
	root := newRoot()
	root.SetArgs([]string{"login", "--auth", url, "--api", "https://api.example", "--no-browser"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	c, _ := loadCLIConfig()
	p := c.Profiles["default"]
	if c.Current != "default" || p.Organization != "org_1" || p.Workspace != "proj_1" || p.APIURL != "https://api.example" {
		t.Fatalf("profile %+v", c)
	}
	if key, err := loadCredential("default"); err != nil || key != "bma_pat_v1_x" {
		t.Fatalf("credential %q %v", key, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.yaml")); strings.Contains(string(b), "bma_pat") {
		t.Fatal("the configuration holds no secret")
	}
	if fi, _ := os.Stat(credentialsPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v", fi.Mode())
	}
}

func TestUpgradeVerifiesTheArchiveAgainstTheManifest(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "bma", Mode: 0o755, Size: 4})
	_, _ = tw.Write([]byte("next"))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(archive.Bytes())
	good := hex.EncodeToString(sum[:])
	for _, tc := range []struct{ sum, want string }{{good, "next"}, {strings.Repeat("0", 64), ""}} {
		url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "bma-manifest.json") {
				_ = json.NewEncoder(w).Encode(manifest{Version: "9.9.9", Artifacts: map[string]string{archiveName(): tc.sum}})
				return
			}
			_, _ = w.Write(archive.Bytes())
		})
		t.Setenv("BMA_RELEASE_BASE", url)
		m, err := latestManifest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "bma")
		err = downloadBinary(context.Background(), m, dest)
		got, _ := os.ReadFile(dest)
		if string(got) != tc.want || (tc.want == "") == (err == nil) {
			t.Fatalf("sum %s: got %q err %v", tc.sum[:8], got, err)
		}
	}
}

func TestExitCodeClasses(t *testing.T) {
	if exitCodeOf(errors.New("x")) != 1 || exitCodeOf(usageError("u")) != 2 || exitCodeOf(context.Canceled) != 130 {
		t.Fatal("exit codes")
	}
}

func TestStoredExecutorURLOnARetiredHostMovesToTheExecutorHost(t *testing.T) {
	useHome(t)
	c := defaultConfig()
	c.ExecutorURL, c.MachineID = "https://api.bma.beeos.ai/", "mch_1"
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig()
	if err != nil || got.ExecutorURL != "https://executor.beeos.ai" {
		t.Fatalf("loaded %q %v", got.ExecutorURL, err)
	}
	b, _ := os.ReadFile(configPath())
	if !strings.Contains(string(b), `"executor_url": "https://executor.beeos.ai"`) {
		t.Fatalf("not persisted: %s", b)
	}
}
