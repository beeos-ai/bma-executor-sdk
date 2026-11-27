//go:build !windows

package bmaexecutor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// run starts a process of session that runs script and returns the read that
// reports its end (the one that writes memory back and reports).
func run(t *testing.T, h processHarness, process, session, script string) ProcessReadResult {
	t.Helper()
	h.mustStart(t, ProcessStartParams{ProcessID: process, Argv: []string{"sh", "-c", script},
		Metadata: map[string]string{MetadataAgentSessionID: session}})
	return endRead(t, h, process)
}

func rwStore(id, slug string, files map[string]string) *fakeMemoryStore {
	s := &fakeMemoryStore{id: id, slug: slug, access: "read_write", files: map[string][]byte{}}
	for p, c := range files {
		s.files[p] = []byte(c)
	}
	return s
}

func bare(c string) string { return strings.TrimPrefix(digestOf([]byte(c)), "sha256:") }

// A created and a changed file of a read_write store reach the store when
// the process ends, each against the base the mirror wrote; read_write files
// are writable, the result reports nothing (everything landed).
func TestMemoryWritebackAtProcessEnd(t *testing.T) {
	store := rwStore("memstore_a", "prefs", map[string]string{"/lang.md": "zh"})
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a", "p2": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	read := run(t, h, "p1", "sesn_a", fmt.Sprintf(`printf zh-CN > %q && mkdir -p %q && printf new > %q`,
		filepath.Join(dir, "lang.md"), filepath.Join(dir, "n"), filepath.Join(dir, "n", "new.md")))
	if len(read.SessionMemory) != 0 {
		t.Fatalf("session_memory = %+v", read.SessionMemory)
	}
	got := daemon.writes()
	want := map[string]bool{"PUT memstore_a/lang.md base=" + bare("zh"): true, "PUT memstore_a/n/new.md base=": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Fatalf("writes = %v", got)
	}
	if string(store.files["/lang.md"]) != "zh-CN" || string(store.files["/n/new.md"]) != "new" {
		t.Fatalf("store = %q", store.files)
	}
	// The next process start refreshes the mirror without sending again.
	run(t, h, "p2", "sesn_a", "true")
	if len(daemon.writes()) != 2 {
		t.Fatalf("sent again: %v", daemon.writes())
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "n", "new.md")); string(c) != "new" {
		t.Fatalf("new.md after the refresh = %q", c)
	}
}

// The store wins a conflict: another session changed the memory since this
// one read it; its copy is replaced by the store's version and the agent is
// told in the process end.
func TestMemoryWritebackConflictStoreWinsAndIsReported(t *testing.T) {
	store := rwStore("memstore_a", "prefs", map[string]string{"/plan.md": "v0"})
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a", "p2": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	file := filepath.Join(files.MemoryDir("sesn_a"), "prefs", "plan.md")
	run(t, h, "p1", "sesn_a", "true")
	// Another session writes while this session's process runs (after its
	// refresh, before its write).
	h.mustStart(t, ProcessStartParams{ProcessID: "p2", Argv: []string{"sh", "-c",
		fmt.Sprintf(`sleep 0.3; printf mine > %q`, file)}, Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}})
	daemon.update(func() { store.files["/plan.md"] = []byte("other session") })
	read := endRead(t, h, "p2")
	if len(read.SessionMemory) != 1 || read.SessionMemory[0].Outcome != "conflict" ||
		read.SessionMemory[0].Path != "/plan.md" || read.SessionMemory[0].MemoryStoreID != "memstore_a" {
		t.Fatalf("session_memory = %+v", read.SessionMemory)
	}
	if c, _ := os.ReadFile(file); string(c) != "other session" {
		t.Fatalf("local copy = %q, want the store's", c)
	}
	if string(store.files["/plan.md"]) != "other session" {
		t.Fatalf("store = %q", store.files["/plan.md"])
	}
}

// A memory rule's refusal is reported with the platform's message, the file
// stays, and it is not sent again until it changes.
func TestMemoryWritebackRejectionIsReportedOnce(t *testing.T) {
	store := rwStore("memstore_a", "prefs", nil)
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a", "p2": "sesn_a", "p0": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	run(t, h, "p0", "sesn_a", "true")
	file := filepath.Join(files.MemoryDir("sesn_a"), "prefs", "big.md")
	read := run(t, h, "p1", "sesn_a", fmt.Sprintf(`head -c 150000 /dev/zero > %q`, file))
	if len(read.SessionMemory) != 1 || read.SessionMemory[0].Outcome != "rejected" ||
		!strings.Contains(read.SessionMemory[0].Message, "split") {
		t.Fatalf("session_memory = %+v", read.SessionMemory)
	}
	if info, err := os.Stat(file); err != nil || info.Size() != 150000 {
		t.Fatalf("big.md is gone: %v %v", info, err)
	}
	read = run(t, h, "p2", "sesn_a", "true")
	if n := len(daemon.writes()); n != 1 || len(read.SessionMemory) != 0 {
		t.Fatalf("sent again: %v %+v", daemon.writes(), read.SessionMemory)
	}
}

// A deletion is written back against its base; emptying a store of more
// than 50 memories deletes none, is reported and restored at the next refresh.
func TestMemoryWritebackDeletesAndRefusesBulkDelete(t *testing.T) {
	many := map[string]string{"/one.md": "1"}
	for i := range 51 {
		many[fmt.Sprintf("/m/%02d.md", i)] = "x"
	}
	store := rwStore("memstore_a", "prefs", many)
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a", "p2": "sesn_a", "p3": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	run(t, h, "p1", "sesn_a", fmt.Sprintf(`rm %q`, filepath.Join(dir, "one.md")))
	if _, ok := store.files["/one.md"]; ok || daemon.writes()[0] != "DELETE memstore_a/one.md base="+bare("1") {
		t.Fatalf("one.md not deleted: %v", daemon.writes())
	}
	read := run(t, h, "p2", "sesn_a", fmt.Sprintf(`rm -rf %q`, filepath.Join(dir, "m")))
	if len(read.SessionMemory) != 1 || read.SessionMemory[0].Outcome != "delete_refused" {
		t.Fatalf("session_memory = %+v", read.SessionMemory)
	}
	if len(store.files) != 51 || len(daemon.writes()) != 1 {
		t.Fatalf("store has %d, writes %v", len(store.files), daemon.writes())
	}
	run(t, h, "p3", "sesn_a", "true")
	if entries, _ := os.ReadDir(filepath.Join(dir, "m")); len(entries) != 51 {
		t.Fatalf("restored %d files", len(entries))
	}
}

// A store directory whose marker entry is gone is not written back from
// (it would read as "delete everything"); the reset is reported and the next
// refresh rebuilds it from the store.
func TestMemoryWritebackDistrustedDirectoryIsReset(t *testing.T) {
	store := rwStore("memstore_a", "prefs", map[string]string{"/a.md": "a", "/b.md": "b"})
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a", "p2": "sesn_a", "p3": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	mem := files.MemoryDir("sesn_a")
	run(t, h, "p1", "sesn_a", "true")
	read := run(t, h, "p2", "sesn_a", fmt.Sprintf(`rm -rf %q`, filepath.Join(mem, "prefs")))
	if len(read.SessionMemory) != 1 || read.SessionMemory[0].Outcome != "reset" {
		t.Fatalf("session_memory = %+v", read.SessionMemory)
	}
	if len(daemon.writes()) != 0 || len(store.files) != 2 {
		t.Fatalf("writes %v, store %v", daemon.writes(), store.files)
	}
	run(t, h, "p3", "sesn_a", "true")
	if c, _ := os.ReadFile(filepath.Join(mem, "prefs", "a.md")); string(c) != "a" {
		t.Fatalf("a.md after the reset = %q", c)
	}
}

// A writeback that cannot reach the platform fails nothing: the process ends
// normally, and the next pass sends it.
func TestMemoryWritebackTransientFailureIsRetried(t *testing.T) {
	store := rwStore("memstore_a", "prefs", nil)
	daemon := &fakeDaemonFiles{processes: map[string]string{"p0": "sesn_a", "p1": "sesn_a", "p2": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	run(t, h, "p0", "sesn_a", "true")
	daemon.update(func() { daemon.failMemoryWrites = 1 })
	read := run(t, h, "p1", "sesn_a", fmt.Sprintf(`printf x > %q`, filepath.Join(files.MemoryDir("sesn_a"), "prefs", "x.md")))
	if read.State != ProcessExited || len(read.SessionMemory) != 0 || len(store.files) != 0 {
		t.Fatalf("read %+v store %v", read, store.files)
	}
	run(t, h, "p2", "sesn_a", "true")
	if string(store.files["/x.md"]) != "x" {
		t.Fatalf("not retried: %v", daemon.writes())
	}
}

// read_only stores are never written back, whatever bash does to the copy.
func TestMemoryWritebackSkipsReadOnlyStores(t *testing.T) {
	store := &fakeMemoryStore{id: "memstore_r", slug: "ref", files: map[string][]byte{"/r.md": []byte("r")}}
	daemon := &fakeDaemonFiles{processes: map[string]string{"p1": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	file := filepath.Join(files.MemoryDir("sesn_a"), "ref", "r.md")
	run(t, h, "p1", "sesn_a", fmt.Sprintf(`chmod u+w %q; printf changed > %q; printf n > %q`, file, file,
		filepath.Join(filepath.Dir(file), "n.md")))
	if len(daemon.writes()) != 0 {
		t.Fatalf("read_only store written back: %v", daemon.writes())
	}
}

// A turn that starts no process (an on-machine brain editing with its own
// tools): the walk writes the change back once it settled, and the next
// session_files.sync writes back first, then refreshes, and carries the
// reports.
func TestMemoryWritebackWithoutProcesses(t *testing.T) {
	store := rwStore("memstore_a", "prefs", map[string]string{"/a.md": "a"})
	daemon := &fakeDaemonFiles{processes: map[string]string{"anchor": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	go files.Run(ctx)
	sync := func() SessionFilesSyncResult {
		var result SessionFilesSyncResult
		noErr(t, h.executor.Call(testContext(t), MethodSessionFilesSync,
			SessionFilesSyncParams{AgentSessionID: "sesn_a", DownloadToken: "anchor"}, &result))
		return result
	}
	sync()
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	noErr(t, os.WriteFile(filepath.Join(dir, "walk.md"), []byte("walk"), 0o644))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && daemonFile(daemon, store, "/walk.md") != "walk" {
		time.Sleep(200 * time.Millisecond)
	}
	if daemonFile(daemon, store, "/walk.md") != "walk" {
		t.Fatalf("the walk did not write back: %v", daemon.writes())
	}
	// Written just before the next turn: sync sends it before it refreshes.
	noErr(t, os.WriteFile(filepath.Join(dir, "late.md"), []byte("late"), 0o644))
	daemon.update(func() { store.files["/a.md"] = []byte("other") })
	noErr(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("mine"), 0o644))
	r := sync()
	if daemonFile(daemon, store, "/late.md") != "late" {
		t.Fatalf("late.md lost: %v", daemon.writes())
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "late.md")); string(c) != "late" {
		t.Fatalf("late.md after the refresh = %q", c)
	}
	if r.State != "synced" || len(r.SessionMemory) != 1 || r.SessionMemory[0].Path != "/a.md" {
		t.Fatalf("sync = %+v", r)
	}
}

func daemonFile(d *fakeDaemonFiles, s *fakeMemoryStore, p string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(s.files[p])
}

// A session the walk forgot (no process or sync for sessionFilesIdle, e.g. a
// long ACP turn running its own tools) keeps its bases: what the agent
// changes, creates or deletes afterwards is written back against them by the
// next sync instead of being overwritten by the refresh.
func TestMemoryWritebackAfterTheSessionWasForgotten(t *testing.T) {
	store := rwStore("memstore_a", "prefs", map[string]string{"/a.md": "a", "/c.md": "c"})
	daemon := &fakeDaemonFiles{processes: map[string]string{"anchor": "sesn_a"},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {store}}}
	h, files, _ := newSessionFilesHarness(t, daemon)
	sync := func() SessionFilesSyncResult {
		var result SessionFilesSyncResult
		noErr(t, h.executor.Call(testContext(t), MethodSessionFilesSync,
			SessionFilesSyncParams{AgentSessionID: "sesn_a", DownloadToken: "anchor"}, &result))
		return result
	}
	sync()
	files.active(time.Now().Add(time.Hour)) // idle: flushed and forgotten
	dir := filepath.Join(files.MemoryDir("sesn_a"), "prefs")
	noErr(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("mine"), 0o644))
	noErr(t, os.WriteFile(filepath.Join(dir, "b.md"), []byte("new"), 0o644))
	noErr(t, os.Remove(filepath.Join(dir, "c.md")))
	if r := sync(); r.State != "synced" || len(r.SessionMemory) != 0 {
		t.Fatalf("sync = %+v", r)
	}
	if daemonFile(daemon, store, "/a.md") != "mine" || daemonFile(daemon, store, "/b.md") != "new" ||
		daemonFile(daemon, store, "/c.md") != "" {
		t.Fatalf("store = %q, writes %v", store.files, daemon.writes())
	}
	want := map[string]bool{"PUT memstore_a/a.md base=" + bare("a"): true, "PUT memstore_a/b.md base=": true,
		"DELETE memstore_a/c.md base=" + bare("c"): true}
	if got := daemon.writes(); len(got) != 3 || !want[got[0]] || !want[got[1]] || !want[got[2]] {
		t.Fatalf("writes = %v", got)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "a.md")); string(c) != "mine" {
		t.Fatalf("a.md after the refresh = %q", c)
	}
}
