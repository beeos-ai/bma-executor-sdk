//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBMA is the part of the public API launch, apply, send, chat, wait and
// interrupt use, with the documented semantics they depend on: Idempotency-
// Key replays, the metadata filter of the session list, an agent update that
// changes nothing storing no version, and a turn per user.message on the
// event stream.
type fakeBMA struct {
	t  *testing.T
	mu sync.Mutex
	// replays by Idempotency-Key: the body hash and the answer.
	replays  map[string][2]string
	agents   []map[string]any
	versions map[string]int // agent id -> stored versions
	sessions []map[string]any
	events   map[string][]map[string]any
	files    []map[string]any
	machines []map[string]any
	wake     chan struct{}
	creates  map[string]int // path -> creations
	n        int
}

func newFakeBMA(t *testing.T) (*fakeBMA, string) {
	f := &fakeBMA{t: t, replays: map[string][2]string{}, versions: map[string]int{}, events: map[string][]map[string]any{},
		wake: make(chan struct{}), creates: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeBMA) id(prefix string) string {
	f.n++
	return fmt.Sprintf("%s%026d", prefix, f.n)
}

func writeJSONAnswer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fakeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSONAnswer(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func page(items []map[string]any) map[string]any {
	data := make([]any, len(items))
	for i, it := range items {
		data[i] = it
	}
	return map[string]any{"data": data, "next_page": nil}
}

// idempotent replays the answer of a repeated key, refuses a key reused
// with another body, else runs create and remembers its answer.
func (f *fakeBMA) idempotent(w http.ResponseWriter, r *http.Request, body string, create func() any) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" {
		if prev, ok := f.replays[r.URL.Path+" "+key]; ok {
			if prev[0] != body {
				fakeError(w, 400, "invalid_request_error", "idempotency_key_reused")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, prev[1])
			return
		}
	}
	f.creates[r.URL.Path]++
	b, _ := json.Marshal(create())
	if key != "" {
		f.replays[r.URL.Path+" "+key] = [2]string{body, string(b)}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (f *fakeBMA) appendEvents(sid string, evs ...map[string]any) {
	for _, ev := range evs {
		f.n++
		if ev["id"] == nil {
			ev["id"] = fmt.Sprintf("sevt_%d", f.n)
		}
		ev["bma_sequence"] = f.n
		f.events[sid] = append(f.events[sid], ev)
		switch ev["type"] {
		case "session.status_running":
			f.session(sid)["status"] = "running"
		case "session.status_idle":
			f.session(sid)["status"] = "idle"
		}
	}
	close(f.wake)
	f.wake = make(chan struct{})
}

func (f *fakeBMA) session(id string) map[string]any {
	for _, s := range f.sessions {
		if s["id"] == id {
			return s
		}
	}
	return nil
}

func idle(reason string, ids ...string) map[string]any {
	sr := map[string]any{"type": reason}
	if len(ids) > 0 {
		sr["event_ids"] = ids
	}
	return map[string]any{"type": "session.status_idle", "stop_reason": sr}
}

func agentText(text string) map[string]any {
	return map[string]any{"type": "agent.message", "content": []any{map[string]any{"type": "text", "text": text}}}
}

// turn runs the agent's answer to the events sent: a message containing
// "approval" asks to run a tool first; "fail" ends with retries_exhausted;
// "write" writes and delivers report.md.
func (f *fakeBMA) turn(sid string, sent []map[string]any) {
	for _, ev := range sent {
		switch ev["type"] {
		case "user.message":
			text := messageText(ev)
			f.appendEvents(sid, map[string]any{"type": "session.status_running"})
			switch {
			case strings.Contains(text, "approval"):
				f.appendEvents(sid, map[string]any{"type": "agent.tool_use", "id": "sevt_tool", "name": "bash",
					"input": map[string]any{"command": "ls"}}, idle(stopRequiresAction, "sevt_tool"))
			case strings.Contains(text, "fail"):
				f.appendEvents(sid, map[string]any{"type": "session.error", "error": map[string]any{"message": "model unavailable"}},
					idle("retries_exhausted"))
			case strings.Contains(text, "slow"):
				// stays running until interrupted
			default:
				if strings.Contains(text, "write") {
					f.files = append(f.files, map[string]any{"id": "file_out", "filename": "report.md", "scope": sid,
						"bma_delivered": true})
				}
				f.appendEvents(sid, agentText("reply: "+strings.SplitN(text, "\n", 2)[0]), idle(stopEndTurn))
			}
		case "user.tool_confirmation":
			f.appendEvents(sid, map[string]any{"type": "session.status_running"},
				agentText("tool "+fmt.Sprint(ev["result"])), idle(stopEndTurn))
		case "user.interrupt":
			if f.session(sid)["status"] == "running" {
				f.appendEvents(sid, idle(stopEndTurn))
			}
		}
	}
}

func (f *fakeBMA) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/events/stream") {
		f.stream(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	var in map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && len(b) > 0 {
		if err := json.Unmarshal(b, &in); err != nil {
			fakeError(w, 400, "invalid_request_error", "bad json")
			return
		}
	}
	q := r.URL.Query()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/agents":
		writeJSONAnswer(w, 200, page(f.agents))
	case r.Method == "POST" && r.URL.Path == "/v1/agents":
		f.idempotent(w, r, string(b), func() any {
			a := map[string]any{"type": "agent", "id": f.id("aprf_"), "version": 1}
			for k, v := range in {
				a[k] = v
			}
			f.agents = append(f.agents, a)
			return a
		})
	case r.Method == "POST" && len(parts) == 2 && parts[0] == "agents":
		for _, a := range f.agents {
			if a["id"] != parts[1] {
				continue
			}
			if fmt.Sprint(in["version"]) != fmt.Sprint(a["version"]) {
				fakeError(w, 409, "conflict_error", "version")
				return
			}
			changed := false
			for k, v := range in {
				if k == "version" {
					continue
				}
				if k == "metadata" {
					md, _ := a["metadata"].(map[string]any)
					if md == nil {
						md = map[string]any{}
					}
					for mk, mv := range v.(map[string]any) {
						if mv == nil {
							if _, ok := md[mk]; ok {
								delete(md, mk)
								changed = true
							}
						} else if md[mk] != mv {
							md[mk], changed = mv, true
						}
					}
					a["metadata"] = md
					continue
				}
				old, _ := json.Marshal(a[k])
				nw, _ := json.Marshal(v)
				if a[k] == nil && (string(nw) == `""` || string(nw) == `[]`) {
					continue // stored empty
				}
				if string(old) != string(nw) {
					a[k], changed = v, true
				}
			}
			if changed {
				a["version"] = a["version"].(int) + 1
			}
			writeJSONAnswer(w, 200, a)
			return
		}
		fakeError(w, 404, "not_found_error", "agent")
	case r.Method == "GET" && r.URL.Path == "/v1/environments":
		writeJSONAnswer(w, 200, page([]map[string]any{{"id": "env_python", "name": "python"},
			{"id": "env_dup1", "name": "dup"}, {"id": "env_dup2", "name": "dup"}}))
	case r.Method == "GET" && r.URL.Path == "/v1/machine_pools":
		writeJSONAnswer(w, 200, page([]map[string]any{{"id": "mpo_gpu", "name": "gpu"}}))
	case r.Method == "POST" && r.URL.Path == "/v1/machines":
		f.idempotent(w, r, string(b), func() any {
			m := map[string]any{"id": f.id("mch_"), "machine_pool_id": in["machine_pool_id"]}
			f.machines = append(f.machines, m)
			return m
		})
	case r.Method == "POST" && r.URL.Path == "/v1/files":
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(strings.NewReader(string(b)), params["boundary"])
		p, err := mr.NextPart()
		if err != nil {
			fakeError(w, 400, "invalid_request_error", "no file")
			return
		}
		content, _ := io.ReadAll(p)
		f.idempotent(w, r, p.FileName(), func() any {
			file := map[string]any{"type": "file", "id": f.id("file_"), "filename": p.FileName(), "content": string(content),
				"created_at": "2026-11-27T00:00:00Z", "mime_type": "text/plain", "size_bytes": len(content)}
			f.files = append(f.files, file)
			return file
		})
	case r.Method == "GET" && r.URL.Path == "/v1/files":
		var out []map[string]any
		for _, file := range f.files {
			if file["scope"] == q.Get("scope_id") {
				out = append(out, file)
			}
		}
		writeJSONAnswer(w, 200, page(out))
	case r.Method == "GET" && r.URL.Path == "/v1/sessions":
		var out []map[string]any
		statuses := map[string]bool{}
		for _, st := range q["statuses"] {
			statuses[st] = true
		}
		for _, s := range f.sessions {
			md, _ := s["metadata"].(map[string]any)
			ok := len(statuses) == 0 || statuses[fmt.Sprint(s["status"])]
			for _, kv := range q["metadata"] {
				k, v, _ := strings.Cut(kv, ":")
				ok = ok && md[k] == v
			}
			if ok {
				out = append(out, s)
			}
		}
		writeJSONAnswer(w, 200, page(out))
	case r.Method == "POST" && r.URL.Path == "/v1/sessions":
		if in["agent"] == nil || in["environment_id"] == nil {
			fakeError(w, 400, "invalid_request_error", "agent and environment_id are required")
			return
		}
		f.idempotent(w, r, string(b), func() any {
			s := map[string]any{"type": "session", "id": f.id("ses_"), "status": "idle"}
			for k, v := range in {
				s[k] = v
			}
			f.sessions = append(f.sessions, s)
			return s
		})
	case len(parts) == 2 && parts[0] == "sessions" && r.Method == "GET":
		if s := f.session(parts[1]); s != nil {
			writeJSONAnswer(w, 200, s)
			return
		}
		fakeError(w, 404, "not_found_error", "session")
	case len(parts) == 3 && parts[2] == "resources" && r.Method == "POST":
		s := f.session(parts[1])
		res, _ := s["resources"].([]any)
		s["resources"] = append(res, map[string]any{"type": "file", "file_id": in["file_id"], "mount_path": in["mount_path"]})
		writeJSONAnswer(w, 200, map[string]any{"type": "file", "id": "sesrsc_1"})
	case len(parts) == 3 && parts[2] == "events" && r.Method == "POST":
		sid := parts[1]
		f.idempotent(w, r, string(b), func() any {
			var sent []map[string]any
			for _, e := range in["events"].([]any) {
				ev := e.(map[string]any)
				sent = append(sent, ev)
			}
			f.appendEvents(sid, sent...)
			f.turn(sid, sent)
			return map[string]any{"data": in["events"]}
		})
	case len(parts) == 3 && parts[2] == "events" && r.Method == "GET":
		evs := append([]map[string]any(nil), f.events[parts[1]]...)
		if t := q.Get("types"); t != "" {
			var keep []map[string]any
			for _, e := range evs {
				if e["type"] == t {
					keep = append(keep, e)
				}
			}
			evs = keep
		}
		if q.Get("order") == "desc" {
			sort.SliceStable(evs, func(i, j int) bool { return evs[i]["bma_sequence"].(int) > evs[j]["bma_sequence"].(int) })
		}
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 && len(evs) > n {
			evs = evs[:n]
		}
		writeJSONAnswer(w, 200, page(evs))
	default:
		fakeError(w, 404, "not_found_error", r.Method+" "+r.URL.Path)
	}
}

// stream sends the events appended after it opened.
func (f *fakeBMA) stream(w http.ResponseWriter, r *http.Request) {
	sid := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")[0]
	f.mu.Lock()
	next := len(f.events[sid])
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, ": ok\n\n")
	w.(http.Flusher).Flush()
	deltas := r.URL.Query().Get("event_deltas") != ""
	for {
		f.mu.Lock()
		evs := f.events[sid][next:]
		next = len(f.events[sid])
		wake := f.wake
		f.mu.Unlock()
		for _, ev := range evs {
			if deltas && ev["type"] == "agent.message" {
				d, _ := json.Marshal(map[string]any{"type": "event_delta", "event_id": ev["id"],
					"delta": map[string]any{"type": "content_delta", "index": 0, "content": map[string]any{"type": "text", "text": messageText(ev)}}})
				_, _ = fmt.Fprintf(w, "event: event_delta\ndata: %s\n\n", d)
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "id: %v\nevent: %v\ndata: %s\n\n", ev["id"], ev["type"], b)
		}
		w.(http.Flusher).Flush()
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		}
	}
}

func putFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func apiEnv(url string) map[string]string {
	return map[string]string{"BMA_API_KEY": "bma_org_v1_k", "BMA_BASE_URL": url}
}

const inlineSession = `agent:
  name: researcher
  model: claude-sonnet-4-5
  system: You research.
environment: python
title: Weekly report
metadata: {team: data}
resources:
  - path: data.csv
    mount_path: /mnt/session/uploads/input/data.csv
message: Summarize data.csv
`

// A launch resolves names, uploads and mounts the files, sends the first
// message once, waits for the reply; running it again creates nothing.
func TestLaunchWaitPrintsTheReplyAndARerunCreatesNoSecondSession(t *testing.T) {
	f, url := newFakeBMA(t)
	dir := t.TempDir()
	spec := putFile(t, dir, "session.yaml", inlineSession)
	putFile(t, dir, "data.csv", "a,b\n1,2\n")
	putFile(t, dir, "notes.txt", "remember")

	out, code := runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec, "--file", filepath.Join(dir, "notes.txt"), "--wait")
	if code != 0 || out != "reply: Summarize data.csv\n" {
		t.Fatalf("launch: code %d out %q", code, out)
	}
	if len(f.sessions) != 1 || len(f.agents) != 1 || len(f.files) != 2 {
		t.Fatalf("sessions %d agents %d files %d, want 1 1 2", len(f.sessions), len(f.agents), len(f.files))
	}
	s := f.sessions[0]
	agent := s["agent"].(map[string]any)
	if agent["id"] != f.agents[0]["id"] || s["environment_id"] != "env_python" || s["title"] != "Weekly report" {
		t.Fatalf("session %v", s)
	}
	md := s["metadata"].(map[string]any)
	if md["team"] != "data" || !strings.HasPrefix(fmt.Sprint(md[launchKeyLabel]), "bma-launch-") {
		t.Fatalf("metadata %v", md)
	}
	mounts := fmt.Sprint(s["resources"])
	if !strings.Contains(mounts, "/mnt/session/uploads/input/data.csv") || !strings.Contains(mounts, "/mnt/session/uploads/notes.txt") {
		t.Fatalf("resources %s", mounts)
	}
	msg := f.events[fmt.Sprint(s["id"])][0]
	if !strings.Contains(messageText(msg), "Attached files:\n- /mnt/session/uploads/notes.txt") {
		t.Fatalf("first message %q", messageText(msg))
	}

	out, code = runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec, "--file", filepath.Join(dir, "notes.txt"), "--wait", "--json")
	if code != 0 {
		t.Fatalf("rerun: code %d", code)
	}
	var res turnResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.SessionID != s["id"] || res.Text != "reply: Summarize data.csv" ||
		res.StopReason != stopEndTurn {
		t.Fatalf("rerun out %q (%v)", out, err)
	}
	users := 0
	for _, ev := range f.events[res.SessionID] {
		if ev["type"] == "user.message" {
			users++
		}
	}
	if len(f.sessions) != 1 || len(f.agents) != 1 || f.agents[0]["version"] != 1 || len(f.files) != 2 || users != 1 {
		t.Fatalf("rerun created: sessions %d agents %d (v%v) files %d messages %d", len(f.sessions), len(f.agents),
			f.agents[0]["version"], len(f.files), users)
	}

	// Another --idempotency-key is another launch.
	if _, code = runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec, "--idempotency-key", "run-2"); code != 0 || len(f.sessions) != 2 {
		t.Fatalf("new key: code %d sessions %d", code, len(f.sessions))
	}
}

func TestLaunchResolvesNamesAndRefusesAmbiguousOrUnknownOnes(t *testing.T) {
	f, url := newFakeBMA(t)
	f.agents = []map[string]any{{"id": "aprf_a", "name": "writer", "version": 3}}
	dir := t.TempDir()
	for _, c := range []struct {
		spec string
		code int
		want string
	}{
		{"agent: writer\nenvironment: dup\n", 2, `2 environments are named "dup" (env_dup1, env_dup2)`},
		{"agent: nobody\nenvironment: python\n", 4, `no agent is named "nobody" (names: writer)`},
		{"agent: writer\n", 2, "environment or machine_pool is required"},
		{"agent: writer\nenvironment: python\nmachine_pool: gpu\n", 2, "not both"},
		{"agent: writer\nenvironment: python\nextra: 1\n", 2, "field extra not found"},
	} {
		spec := putFile(t, dir, "s.yaml", c.spec)
		if _, code := runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec); code != c.code {
			t.Errorf("%q: exit %d, want %d", c.spec, code, c.code)
		}
	}
	spec := putFile(t, dir, "s.yaml", "agent: {name: writer, version: 2}\nmachine_pool: gpu\n")
	if _, code := runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec); code != 0 {
		t.Fatalf("pool launch: exit %d", code)
	}
	s := f.sessions[0]
	if fmt.Sprint(s["agent"]) != "map[id:aprf_a type:agent version:2]" || len(f.machines) != 1 ||
		s["environment_id"] != f.machines[0]["id"] || f.machines[0]["machine_pool_id"] != "mpo_gpu" {
		t.Fatalf("session %v machines %v", s, f.machines)
	}
}

// A launch whose message send was lost sends it on the rerun.
func TestLaunchRerunSendsAMessageThatWasNeverSent(t *testing.T) {
	f, url := newFakeBMA(t)
	f.agents = []map[string]any{{"id": "aprf_a", "name": "writer", "version": 1}}
	spec := putFile(t, t.TempDir(), "s.yaml", "agent: writer\nenvironment: env_python\nmessage: hello\n")
	f.sessions = nil
	var l launchSpec
	dir, _ := readSpecFile(spec, &l)
	key, _ := launchKey(l, dir)
	f.sessions = append(f.sessions, map[string]any{"id": "ses_old", "status": "idle",
		"metadata": map[string]any{launchKeyLabel: key}})
	out, code := runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec, "--wait")
	if code != 0 || out != "reply: hello\n" || len(f.sessions) != 1 {
		t.Fatalf("code %d out %q sessions %d", code, out, len(f.sessions))
	}
}

func TestLaunchExitStatusFollowsTheTurn(t *testing.T) {
	f, url := newFakeBMA(t)
	f.agents = []map[string]any{{"id": "aprf_a", "name": "writer", "version": 1}}
	dir := t.TempDir()
	for msg, want := range map[string]int{"needs approval": exitRequiresAction, "please fail": exitTurnFailed,
		"write it": 0} {
		spec := putFile(t, dir, "s.yaml", "agent: writer\nenvironment: python\nmessage: "+msg+"\n")
		out, code := runCLI(t, apiEnv(url), "sessions", "launch", "-f", spec, "--wait")
		if code != want {
			t.Errorf("%q: exit %d, want %d (out %q)", msg, code, want, out)
		}
		if msg == "write it" && !strings.Contains(out, "Deliverables:\n  file_out  report.md  (bma files download file_out -o report.md)") {
			t.Errorf("deliverables missing: %q", out)
		}
	}
}

func TestAgentsApplyCreatesThenUpdatesOnlyOnChange(t *testing.T) {
	f, url := newFakeBMA(t)
	dir := t.TempDir()
	spec := putFile(t, dir, "agent.yaml", "name: helper\nmodel: claude-sonnet-4-5\nsystem: v1\nmetadata: {owner: ops, tier: a}\n")
	for i, want := range []int{1, 1} {
		if out, code := runCLI(t, apiEnv(url), "agents", "apply", "-f", spec, "--query", ".version"); code != 0 ||
			out != fmt.Sprintf("%d\n", want) {
			t.Fatalf("apply %d: code %d out %q", i, code, out)
		}
	}
	spec = putFile(t, dir, "agent.yaml", "name: helper\nmodel: claude-sonnet-4-5\nsystem: v2\nmetadata: {owner: ops}\n")
	if out, code := runCLI(t, apiEnv(url), "agents", "apply", "-f", spec, "--query", ".version"); code != 0 || out != "2\n" {
		t.Fatalf("changed: code %d out %q", code, out)
	}
	if md := f.agents[0]["metadata"].(map[string]any); len(f.agents) != 1 || md["tier"] != nil || md["owner"] != "ops" {
		t.Fatalf("agents %v", f.agents)
	}
	if _, code := runCLI(t, apiEnv(url), "agents", "apply", "-f", spec); code != 0 || f.agents[0]["version"] != 2 {
		t.Fatalf("reapply: code %d version %v", code, f.agents[0]["version"])
	}
	f.agents = append(f.agents, map[string]any{"id": "aprf_x", "name": "helper", "version": 1})
	if _, code := runCLI(t, apiEnv(url), "agents", "apply", "-f", spec); code != 2 {
		t.Fatalf("two agents named helper: exit %d, want 2", code)
	}
}

func TestSendWithAFileMountsItAndWaitPrintsTheDeliverables(t *testing.T) {
	f, url := newFakeBMA(t)
	f.sessions = []map[string]any{{"id": "ses_1", "status": "idle"}}
	p := putFile(t, t.TempDir(), "draft.md", "# draft")
	out, code := runCLI(t, apiEnv(url), "sessions", "send", "ses_1", "write a review", "--file", p, "--wait")
	if code != 0 || out != "reply: write a review\n\nDeliverables:\n  file_out  report.md  (bma files download file_out -o report.md)\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	if res := fmt.Sprint(f.sessions[0]["resources"]); !strings.Contains(res, "/mnt/session/uploads/draft.md") {
		t.Fatalf("resources %s", res)
	}
}

// chat streams the reply, answers a tool approval and ends at EOF.
func TestChatAnswersAToolApprovalInTheTerminal(t *testing.T) {
	f, url := newFakeBMA(t)
	f.sessions = []map[string]any{{"id": "ses_1", "status": "idle"}}
	stdin = bufio.NewReader(strings.NewReader("needs approval\ny\nthanks\n"))
	defer func() { stdin = bufio.NewReader(os.Stdin) }()
	out, code := runCLI(t, apiEnv(url), "sessions", "chat", "ses_1")
	if code != 0 || out != "tool allow\nreply: thanks\n" {
		t.Fatalf("code %d out %q", code, out)
	}
	var confirmed bool
	for _, ev := range f.events["ses_1"] {
		if ev["type"] == "user.tool_confirmation" && ev["tool_use_id"] == "sevt_tool" && ev["result"] == "allow" {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatalf("no confirmation in %v", f.events["ses_1"])
	}
}

// Ctrl-C during a chat turn interrupts it; the chat goes on.
func TestChatCtrlCInterruptsTheTurn(t *testing.T) {
	f, url := newFakeBMA(t)
	f.sessions = []map[string]any{{"id": "ses_1", "status": "idle"}}
	sig := make(chan os.Signal, 1)
	chatSignals = func() (<-chan os.Signal, func()) { return sig, func() {} }
	defer func() { chatSignals = defaultChatSignals }()
	pr, pw := io.Pipe()
	stdin = bufio.NewReader(pr)
	defer func() { stdin = bufio.NewReader(os.Stdin) }()
	go func() {
		_, _ = io.WriteString(pw, "slow task\n")
		for {
			f.mu.Lock()
			running := f.sessions[0]["status"] == "running"
			f.mu.Unlock()
			if running {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		sig <- os.Interrupt
		_ = pw.Close()
	}()
	if _, code := runCLI(t, apiEnv(url), "sessions", "chat", "ses_1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var interrupted bool
	for _, ev := range f.events["ses_1"] {
		interrupted = interrupted || ev["type"] == "user.interrupt"
	}
	if !interrupted || f.sessions[0]["status"] != "idle" {
		t.Fatalf("interrupted %v status %v", interrupted, f.sessions[0]["status"])
	}
}

func TestWaitInterruptAndListStatus(t *testing.T) {
	f, url := newFakeBMA(t)
	pollEvery = 10 * time.Millisecond
	f.sessions = []map[string]any{{"id": "ses_a", "status": "idle"}, {"id": "ses_b", "status": "idle"},
		{"id": "ses_c", "status": "running"}}
	f.events["ses_b"] = []map[string]any{{"id": "e1", "type": "session.status_idle", "bma_sequence": 1,
		"stop_reason": map[string]any{"type": stopRequiresAction, "event_ids": []any{"x"}}}}
	if out, code := runCLI(t, apiEnv(url), "sessions", "list", "--status", "requires_action", "--query", ".[].id"); code != 0 || out != "ses_b\n" {
		t.Fatalf("list requires_action: code %d out %q", code, out)
	}
	if out, code := runCLI(t, apiEnv(url), "sessions", "list", "--status", "running", "--query", ".data[].id"); code != 0 || out != "ses_c\n" {
		t.Fatalf("list running: code %d out %q", code, out)
	}
	if out, code := runCLI(t, apiEnv(url), "sessions", "wait", "ses_b", "--state", "requires_action", "--query", ".id"); code != 0 || out != "ses_b\n" {
		t.Fatalf("wait: code %d out %q", code, out)
	}
	if _, code := runCLI(t, apiEnv(url), "sessions", "wait", "ses_c", "--timeout", "50ms"); code != exitTimeout {
		t.Fatalf("wait timeout: exit %d", code)
	}
	for i := 0; i < 2; i++ {
		if _, code := runCLI(t, apiEnv(url), "sessions", "interrupt", "ses_c"); code != 0 {
			t.Fatalf("interrupt %d: exit %d", i, code)
		}
	}
	if f.sessions[2]["status"] != "idle" {
		t.Fatalf("ses_c %v", f.sessions[2]["status"])
	}
}

func TestProfilesUseInteractivePicksTheWorkspace(t *testing.T) {
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/organizations/me":
			writeJSONAnswer(w, 200, map[string]any{"id": "org_1", "name": "Ops"})
		case "/v1/organizations/workspaces":
			writeJSONAnswer(w, 200, page([]map[string]any{{"id": "proj_a", "name": "Default"}, {"id": "proj_b", "name": "Research"}}))
		default:
			fakeError(w, 404, "not_found_error", r.URL.Path)
		}
	})
	stdin = bufio.NewReader(strings.NewReader("9\n2\n"))
	defer func() { stdin = bufio.NewReader(os.Stdin) }()
	dir := filepath.Join(t.TempDir(), "cfg")
	env := map[string]string{"BMA_CONFIG_DIR": dir, "BMA_API_KEY": "k", "BMA_BASE_URL": url}
	for k, v := range env {
		t.Setenv(k, v)
	}
	opts = globals{}
	root := newRoot()
	root.SetArgs([]string{"profiles", "set", "work", "--api", url})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	opts = globals{}
	root = newRoot()
	root.SetArgs([]string{"profiles", "use", "--interactive"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	c, err := loadCLIConfig()
	if err != nil || c.Current != "work" || c.Profiles["work"].Workspace != "proj_b" || c.Profiles["work"].Organization != "org_1" {
		t.Fatalf("config %+v (%v)", c, err)
	}
}
