//go:build !windows

package bmaexecutor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

type fakeMount struct {
	resourceID, mountPath string
	content               []byte
}

type fakeOutput struct {
	sessionID, processID, path, digest string
	content                            string
}

// fakeDaemonFiles serves the BMA daemon session file routes anchored on a
// process: the manifest, file content and session-outputs uploads.
type fakeDaemonFiles struct {
	mu        sync.Mutex
	processes map[string]string // process id -> agent session id
	mounts    map[string][]fakeMount
	outputs   []fakeOutput
}

func digestOf(content []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(content)) }

func (f *fakeDaemonFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mtok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/daemon/processes/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	processID, route, _ := strings.Cut(rest, "/")
	sessionID, ok := f.processes[processID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet && route == "session-files":
		manifest := sessionFilesManifest{SessionID: sessionID, Files: []sessionFile{}}
		for _, m := range f.mounts[sessionID] {
			manifest.Files = append(manifest.Files, sessionFile{
				ResourceID: m.resourceID, FileID: "file_" + m.resourceID, MountPath: m.mountPath,
				SizeBytes: int64(len(m.content)), Digest: digestOf(m.content),
			})
		}
		_ = json.NewEncoder(w).Encode(manifest)
	case r.Method == http.MethodGet && strings.HasPrefix(route, "session-files/") && strings.HasSuffix(route, "/content"):
		resourceID := strings.TrimSuffix(strings.TrimPrefix(route, "session-files/"), "/content")
		for _, m := range f.mounts[sessionID] {
			if m.resourceID == resourceID {
				_, _ = w.Write(m.content)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && route == "session-outputs":
		body, _ := io.ReadAll(r.Body)
		digest := digestOf(body)
		if r.Header.Get("X-BMA-File-Digest") != digest {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.outputs = append(f.outputs, fakeOutput{sessionID: sessionID, processID: processID,
			path: r.URL.Query().Get("path"), digest: digest, content: string(body)})
		_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "file_out", "digest": digest, "size_bytes": len(body)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeDaemonFiles) uploaded() []fakeOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := append([]fakeOutput(nil), f.outputs...)
	sort.Slice(result, func(i, j int) bool { return result[i].sessionID+result[i].path < result[j].sessionID+result[j].path })
	return result
}

func newSessionFilesHarness(t *testing.T, daemon *fakeDaemonFiles) (processHarness, *SessionFiles, string) {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := NewSessionFiles(SessionFilesOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	core := newTestCore(t, CoreOptions{})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{SessionFiles: files})); err != nil {
		t.Fatal(err)
	}
	if capabilities := core.Capabilities(); capabilities.SessionFiles == nil || capabilities.SessionFiles.Root != root {
		t.Fatalf("session_files capability = %+v", capabilities.SessionFiles)
	}
	b, _ := attachInProcess(t, core, "cloud", func(o *Options) {
		o.BaseURL, o.EnvironmentKey = server.URL, "mtok"
	})
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_files", Lease: Lease{Token: "lease"}})
	return processHarness{executor: b.executor, session: session.ID()}, files, root
}

func TestSessionFilesPerSessionLayoutConcurrentSessions(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_a": "sesn_a", "proc_b": "sesn_b"},
		mounts: map[string][]fakeMount{
			"sesn_a": {{resourceID: "res_a", mountPath: "/mnt/session/uploads/data/a.txt", content: []byte("alpha")}},
			"sesn_b": {{resourceID: "res_b", mountPath: "/mnt/session/uploads/b.txt", content: []byte("bravo")}},
		},
	}
	h, files, root := newSessionFilesHarness(t, daemon)
	uploadsA, outputsA := files.Dirs("sesn_a")
	uploadsB, outputsB := files.Dirs("sesn_b")
	if uploadsA != filepath.Join(root, "sesn_a", "uploads") || outputsB != filepath.Join(root, "sesn_b", "outputs") {
		t.Fatalf("layout = %s %s", uploadsA, outputsB)
	}

	// Both sessions' processes run at the same time on one executor; each
	// sees its own uploads and writes its own outputs (plus a symlink to a
	// secret that must not be uploaded).
	script := func(uploads, outputs, mount, name string) []string {
		return []string{"sh", "-c", fmt.Sprintf(
			`cat %q; echo; mkdir -p %q/sub && printf deliverable-%s > %q/sub/report.txt && ln -s %q %q/leak && sleep 0.3`,
			filepath.Join(uploads, mount), outputs, name, outputs, secret, outputs)}
	}
	var wg sync.WaitGroup
	for _, run := range []struct{ process, session, uploads, outputs, mount, name string }{
		{"proc_a", "sesn_a", uploadsA, outputsA, "data/a.txt", "a"},
		{"proc_b", "sesn_b", uploadsB, outputsB, "b.txt", "b"},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.mustStart(t, ProcessStartParams{ProcessID: run.process, Argv: script(run.uploads, run.outputs, run.mount, run.name),
				Metadata: map[string]string{MetadataAgentSessionID: run.session}, WaitMS: 5000})
		}()
	}
	wg.Wait()
	if got := readAll(t, h.executor, h.session, "proc_a"); got != "alpha\n" {
		t.Fatalf("session a saw %q", got)
	}
	if got := readAll(t, h.executor, h.session, "proc_b"); got != "bravo\n" {
		t.Fatalf("session b saw %q", got)
	}
	info, err := os.Stat(filepath.Join(uploadsA, "data", "a.txt"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("mounted file mode = %v (%v)", info, err)
	}
	// The read that reports the ended process uploaded its outputs.
	got := daemon.uploaded()
	want := []fakeOutput{
		{sessionID: "sesn_a", processID: "proc_a", path: "sub/report.txt", digest: digestOf([]byte("deliverable-a")), content: "deliverable-a"},
		{sessionID: "sesn_b", processID: "proc_b", path: "sub/report.txt", digest: digestOf([]byte("deliverable-b")), content: "deliverable-b"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("uploaded outputs = %+v, want %+v", got, want)
	}
}

func TestSessionFilesResyncRemovesStaleAndForeignEntries(t *testing.T) {
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_1": "sesn_a", "proc_2": "sesn_a"},
		mounts:    map[string][]fakeMount{"sesn_a": {{resourceID: "res_1", mountPath: "/mnt/session/uploads/one.txt", content: []byte("one")}}},
	}
	h, files, _ := newSessionFilesHarness(t, daemon)
	uploads, _ := files.Dirs("sesn_a")
	h.mustStart(t, ProcessStartParams{ProcessID: "proc_1", Argv: []string{"true"}, WaitMS: 5000,
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}})
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(uploads, "link")); err != nil {
		t.Fatal(err)
	}
	daemon.mu.Lock()
	daemon.mounts["sesn_a"] = []fakeMount{{resourceID: "res_2", mountPath: "/mnt/session/uploads/two.txt", content: []byte("two")}}
	daemon.mu.Unlock()
	h.mustStart(t, ProcessStartParams{ProcessID: "proc_2", Argv: []string{"true"}, WaitMS: 5000,
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}})
	entries, err := os.ReadDir(uploads)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "two.txt" {
		t.Fatalf("uploads after resync = %v", entries)
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "keep" {
		t.Fatalf("symlink target was touched: %q %v", content, err)
	}
}

func TestSessionFilesRejectsUnsafeSessionAndMountPaths(t *testing.T) {
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_bad_mount": "sesn_a", "proc_bad_session": "../escape"},
		mounts:    map[string][]fakeMount{"sesn_a": {{resourceID: "res_x", mountPath: "/mnt/session/uploads/../../etc/x", content: []byte("x")}}},
	}
	h, _, root := newSessionFilesHarness(t, daemon)
	for _, start := range []ProcessStartParams{
		{ProcessID: "proc_bad_session", Argv: []string{"true"}, Metadata: map[string]string{MetadataAgentSessionID: "../escape"}},
		{ProcessID: "proc_bad_mount", Argv: []string{"true"}, Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}},
	} {
		_, err := h.start(t, start)
		if rpcErr := requireCode(t, err, CodeInternalError); rpcErr.Data == nil || rpcErr.Data.Reason != ReasonSpawnFailed {
			t.Fatalf("%s: error = %+v", start.ProcessID, rpcErr)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); !os.IsNotExist(err) {
		t.Fatalf("a directory was created outside the root: %v", err)
	}
	// A process without an agent session does not touch session files.
	h.mustStart(t, ProcessStartParams{ProcessID: "proc_plain", Argv: []string{"true"}, WaitMS: 5000})
}

func TestNewSessionFilesNeedsAnAbsoluteRoot(t *testing.T) {
	for _, root := range []string{"", "relative/dir", "~/sessions"} {
		if _, err := NewSessionFiles(SessionFilesOptions{Root: root}); err == nil {
			t.Fatalf("root %q accepted", root)
		}
	}
}
