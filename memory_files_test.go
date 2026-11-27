//go:build !windows

package bmaexecutor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startIn(t *testing.T, h processHarness, process, session string, argv ...string) string {
	t.Helper()
	h.mustStart(t, ProcessStartParams{ProcessID: process, Argv: argv, WaitMS: 5000,
		Metadata: map[string]string{MetadataAgentSessionID: session}})
	return readAll(t, h.executor, h.session, process)
}

func assertMode(t *testing.T, file string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(file)
	if err != nil || info.Mode().Perm() != want || !info.Mode().IsRegular() {
		t.Fatalf("%s: mode %v, want %v (%v)", file, info, want, err)
	}
}

// Two sessions on one executor: each process sees exactly its own stores
// below its own memory directory, read-only, named by BMA_MEMORY_DIR, and
// the marker lists that session's stores only.
func TestSessionMemoryMirroredPerSessionBeforeProcessStart(t *testing.T) {
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_a": "sesn_a", "proc_b": "sesn_b"},
		memory: map[string][]*fakeMemoryStore{
			"sesn_a": {{id: "memstore_a", slug: "prefs", files: map[string][]byte{"/lang.md": []byte("zh-CN"), "/deep/dir/note.md": []byte("alpha")}}},
			"sesn_b": {{id: "memstore_b", slug: "team-notes", files: map[string][]byte{"/b.md": []byte("bravo")}}},
		},
	}
	h, files, root := newSessionFilesHarness(t, daemon)
	got := startIn(t, h, "proc_a", "sesn_a", "sh", "-c", `printf '%s|' "$BMA_MEMORY_DIR"; cat "$BMA_MEMORY_DIR/prefs/lang.md" "$BMA_MEMORY_DIR/prefs/deep/dir/note.md"; ls "$BMA_MEMORY_DIR"`)
	memA := filepath.Join(root, "sesn_a", "memory")
	if want := memA + "|zh-CNalphaprefs\n"; got != want {
		t.Fatalf("session a saw %q, want %q", got, want)
	}
	if files.MemoryDir("sesn_a") != memA {
		t.Fatalf("MemoryDir = %s", files.MemoryDir("sesn_a"))
	}
	got = startIn(t, h, "proc_b", "sesn_b", "sh", "-c", `cat "$BMA_MEMORY_DIR/team-notes/b.md"; ls "$BMA_MEMORY_DIR"`)
	if got != "bravoteam-notes\n" {
		t.Fatalf("session b saw %q", got)
	}
	assertMode(t, filepath.Join(memA, "prefs", "lang.md"), 0o444)
	assertMode(t, filepath.Join(memA, sessionMemoryMarker), 0o444)
	raw, err := os.ReadFile(filepath.Join(memA, sessionMemoryMarker))
	if err != nil || string(raw) != `{"stores":[{"slug":"prefs","memory_store_id":"memstore_a"}]}` {
		t.Fatalf("marker = %s (%v)", raw, err)
	}
	if _, err := os.Stat(filepath.Join(memA, "team-notes")); !os.IsNotExist(err) {
		t.Fatalf("session a has session b's store: %v", err)
	}
}

// A session without memory stores gets no memory directory and no
// BMA_MEMORY_DIR: nothing changes for it.
func TestSessionMemoryAbsentLeavesNoTrace(t *testing.T) {
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_1": "sesn_a"},
		mounts:    map[string][]fakeMount{"sesn_a": {{resourceID: "res_1", mountPath: "/mnt/session/uploads/one.txt", content: []byte("one")}}},
	}
	h, _, root := newSessionFilesHarness(t, daemon)
	if got := startIn(t, h, "proc_1", "sesn_a", "sh", "-c", `printf '[%s]' "${BMA_MEMORY_DIR-unset}"`); got != "[unset]" {
		t.Fatalf("env = %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(root, "sesn_a"))
	if err != nil || len(entries) != 2 || entries[0].Name() != "outputs" || entries[1].Name() != "uploads" {
		t.Fatalf("session dir = %v (%v)", entries, err)
	}
}

// The platform wins at every sync: an unchanged file is not downloaded
// again, a changed or tampered one is, a removed memory, a file the agent
// added and a symlink go (the link's target untouched), and a store the
// platform no longer materializes loses its directory.
func TestSessionMemoryIncrementalSyncRestoresPlatformState(t *testing.T) {
	store := &fakeMemoryStore{id: "memstore_a", slug: "prefs", files: map[string][]byte{
		"/keep.md": []byte("keep"), "/change.md": []byte("v1"), "/gone.md": []byte("gone"), "/tamper.md": []byte("orig")}}
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_1": "sesn_a", "proc_2": "sesn_a", "proc_3": "sesn_a"},
		memory:    map[string][]*fakeMemoryStore{"sesn_a": {store}},
	}
	h, files, _ := newSessionFilesHarness(t, daemon)
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	startIn(t, h, "proc_1", "sesn_a", "true")

	outside := filepath.Join(t.TempDir(), "secret")
	noErr(t, os.WriteFile(outside, []byte("secret"), 0o600))
	noErr(t, os.Chmod(filepath.Join(dir, "tamper.md"), 0o644))
	noErr(t, os.WriteFile(filepath.Join(dir, "tamper.md"), []byte("bash wrote this"), 0o644))
	noErr(t, os.WriteFile(filepath.Join(dir, "added.md"), []byte("x"), 0o644))
	noErr(t, os.Symlink(outside, filepath.Join(dir, "link.md")))
	daemon.update(func() {
		store.files["/change.md"] = []byte("v2")
		delete(store.files, "/gone.md")
	})
	startIn(t, h, "proc_2", "sesn_a", "true")

	entries, err := os.ReadDir(dir)
	noErr(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "change.md,keep.md,tamper.md" {
		t.Fatalf("store dir = %v", names)
	}
	for name, want := range map[string]string{"change.md": "v2", "keep.md": "keep", "tamper.md": "orig"} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		assertMode(t, filepath.Join(dir, name), 0o444)
	}
	if got, _ := os.ReadFile(outside); string(got) != "secret" {
		t.Fatalf("symlink target touched: %q", got)
	}
	if n := daemon.reads("prefs/keep.md"); n != 1 {
		t.Fatalf("unchanged file downloaded %d times", n)
	}
	if n := daemon.reads("prefs/tamper.md"); n != 2 {
		t.Fatalf("tampered file downloaded %d times", n)
	}

	daemon.update(func() { store.unmaterialized = true })
	startIn(t, h, "proc_3", "sesn_a", "true")
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("unmaterialized store still has a directory: %v", err)
	}
}

// A missing marker, or one naming another store for the slug, distrusts the
// store: its directory is rebuilt from the platform.
func TestSessionMemoryDistrustedMarkerRebuildsStore(t *testing.T) {
	store := &fakeMemoryStore{id: "memstore_a", slug: "prefs", files: map[string][]byte{"/a.md": []byte("a")}}
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_1": "sesn_a", "proc_2": "sesn_a", "proc_3": "sesn_a"},
		memory:    map[string][]*fakeMemoryStore{"sesn_a": {store}},
	}
	h, files, _ := newSessionFilesHarness(t, daemon)
	memory := files.MemoryDir("sesn_a")
	startIn(t, h, "proc_1", "sesn_a", "true")
	noErr(t, os.Remove(filepath.Join(memory, sessionMemoryMarker)))
	startIn(t, h, "proc_2", "sesn_a", "true")
	if n := daemon.reads("prefs/a.md"); n != 2 {
		t.Fatalf("store not rebuilt after the marker was lost: %d downloads", n)
	}
	noErr(t, os.Remove(filepath.Join(memory, sessionMemoryMarker)))
	noErr(t, os.WriteFile(filepath.Join(memory, sessionMemoryMarker), []byte(`{"stores":[{"slug":"prefs","memory_store_id":"memstore_other"}]}`), 0o644))
	startIn(t, h, "proc_3", "sesn_a", "true")
	if n := daemon.reads("prefs/a.md"); n != 3 {
		t.Fatalf("store not rebuilt after the marker named another store: %d downloads", n)
	}
	assertMode(t, filepath.Join(memory, sessionMemoryMarker), 0o444)
}

// session_files.sync mirrors through its download token without a process
// (an on-machine brain's turn) and undoes a bash change; a bad token fails
// visibly.
func TestSessionFilesSyncRefreshesMemory(t *testing.T) {
	store := &fakeMemoryStore{id: "memstore_a", slug: "prefs", files: map[string][]byte{"/a.md": []byte("a1")}}
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"prc_anchor": "sesn_a"},
		memory:    map[string][]*fakeMemoryStore{"sesn_a": {store}},
	}
	h, files, _ := newSessionFilesHarness(t, daemon)
	sync := func(session, token string) SessionFilesSyncResult {
		t.Helper()
		var result SessionFilesSyncResult
		noErr(t, h.executor.Call(testContext(t), MethodSessionFilesSync,
			SessionFilesSyncParams{AgentSessionID: session, DownloadToken: token}, &result))
		return result
	}
	if r := sync("sesn_a", "prc_anchor"); r.State != "synced" {
		t.Fatalf("sync = %+v", r)
	}
	file := filepath.Join(files.MemoryDir("sesn_a"), "prefs", "a.md")
	noErr(t, os.Chmod(file, 0o644))
	noErr(t, os.WriteFile(file, []byte("bash"), 0o644))
	daemon.update(func() { store.files["/b.md"] = []byte("b") })
	if r := sync("sesn_a", "prc_anchor"); r.State != "synced" {
		t.Fatalf("sync = %+v", r)
	}
	if got, _ := os.ReadFile(file); string(got) != "a1" {
		t.Fatalf("a.md = %q after sync", got)
	}
	if got, _ := os.ReadFile(filepath.Join(filepath.Dir(file), "b.md")); string(got) != "b" {
		t.Fatalf("b.md = %q after sync", got)
	}
	for _, bad := range [][2]string{{"sesn_a", "prc_unknown"}, {"sesn_other", "prc_anchor"}, {"../x", "prc_anchor"}, {"sesn_a", ""}} {
		if r := sync(bad[0], bad[1]); r.State != "failed" || r.Error == "" {
			t.Fatalf("sync %v = %+v", bad, r)
		}
	}
}

// The content route serves the current head; a version newer than the
// listing is written when its bytes match the digest the route sends, and a
// memory deleted in between is dropped.
func TestSessionMemoryTakesTheHeadTheRouteServes(t *testing.T) {
	store := &fakeMemoryStore{id: "memstore_a", slug: "prefs",
		files:    map[string][]byte{"/a.md": []byte("listed"), "/gone.md": []byte("x")},
		head:     map[string][]byte{"/a.md": []byte("newer head")},
		vanished: map[string]bool{"/gone.md": true}}
	daemon := &fakeDaemonFiles{processes: map[string]string{"proc_1": "sesn_a"}, memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	startIn(t, h, "proc_1", "sesn_a", "true")
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	if got, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(got) != "newer head" {
		t.Fatalf("a.md = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone.md")); !os.IsNotExist(err) {
		t.Fatalf("vanished memory written: %v", err)
	}
}

// Unsafe manifests fail the process start before anything is written
// outside the store; names the file system cannot hold are skipped.
func TestSessionMemoryRejectsUnsafeManifests(t *testing.T) {
	for name, store := range map[string]*fakeMemoryStore{
		"dotdot":    {id: "memstore_a", slug: "prefs", files: map[string][]byte{"/../escape.md": []byte("x")}},
		"relative":  {id: "memstore_a", slug: "prefs", files: map[string][]byte{"escape.md": []byte("x")}},
		"backslash": {id: "memstore_a", slug: "prefs", files: map[string][]byte{`/a\..\b.md`: []byte("x")}},
		"nul":       {id: "memstore_a", slug: "prefs", files: map[string][]byte{"/a\x00.md": []byte("x")}},
		"slug":      {id: "memstore_a", slug: "../up", files: map[string][]byte{"/a.md": []byte("x")}},
		"no-id":     {slug: "prefs", files: map[string][]byte{"/a.md": []byte("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			daemon := &fakeDaemonFiles{processes: map[string]string{"proc_1": "sesn_a"}, memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
			h, _, root := newSessionFilesHarness(t, daemon)
			_, err := h.start(t, ProcessStartParams{ProcessID: "proc_1", Argv: []string{"true"},
				Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}})
			if rpcErr := requireCode(t, err, CodeInternalError); rpcErr.Data == nil || rpcErr.Data.Reason != ReasonSpawnFailed {
				t.Fatalf("error = %+v", rpcErr)
			}
			for _, p := range []string{filepath.Join(root, "sesn_a", "escape.md"), filepath.Join(root, "sesn_a", "up"), filepath.Join(root, "escape.md")} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("%s written: %v", p, err)
				}
			}
		})
	}
	long := "/" + strings.Repeat("x", 300) + ".md"
	store := &fakeMemoryStore{id: "memstore_a", slug: "prefs", files: map[string][]byte{long: []byte("x"), "/Case.md": []byte("1"), "/case.md": []byte("2"), "/ok.md": []byte("ok")}}
	daemon := &fakeDaemonFiles{processes: map[string]string{"proc_1": "sesn_a"}, memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	startIn(t, h, "proc_1", "sesn_a", "true")
	entries, err := os.ReadDir(filepath.Join(files.MemoryDir("sesn_a"), "prefs"))
	noErr(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// "/Case.md" sorts first in the manifest and wins the case clash.
	if strings.Join(names, ",") != "Case.md,ok.md" {
		t.Fatalf("store dir = %v", names)
	}
}
