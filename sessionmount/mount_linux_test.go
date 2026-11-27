//go:build linux

package sessionmount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// These tests mount for real: they need root and /dev/fuse (CI runs them with
// sudo; locally in a privileged Linux container) and skip otherwise.
func needFUSE(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("needs /dev/fuse")
	}
}

// fakeSource serves uploads by resource id and memories by store path, and
// takes memory writes as BMA does (the store wins a stale base).
type fakeSource struct {
	mu       sync.Mutex
	uploads  map[string][]byte
	memories map[string][]byte
	calls    []string
	writes   []string // "PUT|DELETE <path> base=<digest>"
	reports  []bmaexecutor.SessionMemoryReport
	down     bool
	full     bool // refuse writes as a full store
}

func (s *fakeSource) WriteMemory(_ context.Context, _, p, base string, b []byte) (bmaexecutor.MemoryWrite, error) {
	return s.write("PUT", p, base, b)
}

func (s *fakeSource) DeleteMemory(_ context.Context, _, p, base string) (bmaexecutor.MemoryWrite, error) {
	return s.write("DELETE", p, base, nil)
}

func (s *fakeSource) write(method, p, base string, b []byte) (bmaexecutor.MemoryWrite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, method+" "+p+" base="+base)
	switch {
	case s.down:
		return bmaexecutor.MemoryWrite{}, errors.New("network down")
	case s.full && method == "PUT":
		return bmaexecutor.MemoryWrite{}, &bmaexecutor.DaemonHTTPError{StatusCode: 409, Code: "memory_store_full",
			Message: "the memory store is full"}
	}
	current, found := s.memories[p]
	if found != (base != "") || found && digestOf(current) != base {
		w := bmaexecutor.MemoryWrite{Status: "conflict"}
		if found {
			c := string(current)
			w.Content, w.Digest = &c, digestOf(current)
		}
		return w, nil
	}
	if method == "DELETE" {
		delete(s.memories, p)
		return bmaexecutor.MemoryWrite{Status: "written"}, nil
	}
	s.memories[p] = append([]byte(nil), b...)
	return bmaexecutor.MemoryWrite{Status: "written", Digest: digestOf(b)}, nil
}

func (s *fakeSource) Report(r bmaexecutor.SessionMemoryReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
}

func (s *fakeSource) state() (map[string]string, []string, []bmaexecutor.SessionMemoryReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	memories := map[string]string{}
	for p, b := range s.memories {
		memories[p] = string(b)
	}
	return memories, append([]string(nil), s.writes...), append([]bmaexecutor.SessionMemoryReport(nil), s.reports...)
}

func (s *fakeSource) Upload(_ context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf("%s@%d+%d", id, offset, length))
	if s.down {
		return nil, errors.New("network down")
	}
	b := s.uploads[id]
	end := int64(len(b))
	if length >= 0 {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(b[offset:end])), nil
}

func (s *fakeSource) Memory(_ context.Context, _, p string) (io.ReadCloser, string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, "", 0, errors.New("network down")
	}
	b := s.memories[p]
	return io.NopCloser(bytes.NewReader(b)), digestOf(b), int64(len(b)), nil
}

func (s *fakeSource) callCount() int { return len(s.callList()) }

func (s *fakeSource) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func file(p, key string, b []byte) bmaexecutor.MountedFile {
	return bmaexecutor.MountedFile{Path: p, Key: key, Digest: digestOf(b), Size: int64(len(b))}
}

func newTestMounter(t *testing.T) (*Mounter, string) {
	t.Helper()
	needFUSE(t)
	base := t.TempDir()
	// The sessions' users must reach the mounts through the test's directories.
	for _, dir := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{CacheDir: filepath.Join(base, "cache")})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "session")
	return m, root
}

func sessionMount(root, id string, owner uint32, files []bmaexecutor.MountedFile, stores []bmaexecutor.MountedStore,
	src bmaexecutor.SessionSource) bmaexecutor.SessionMount {
	dir := filepath.Join(root, id)
	return bmaexecutor.SessionMount{SessionID: id, Uploads: filepath.Join(dir, "uploads"), Memory: filepath.Join(dir, "memory"),
		Owner: bmaexecutor.MountOwner{UID: owner, GID: owner}, Files: files, Stores: stores, Source: src}
}

// runAs runs argv as uid and answers its output and error.
func runAs(uid uint32, argv ...string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Two sessions on one machine: each sees its own uploads and memory store,
// the mounts are fuse.bma-session, read-only in the kernel (a write, create,
// chmod all fail with EROFS), and a mount answers only its session's user
// and root.
func TestMountTwoSessionsReadOnlyAndOwned(t *testing.T) {
	m, root := newTestMounter(t)
	a := &fakeSource{uploads: map[string][]byte{"res_a": []byte("alpha")}, memories: map[string][]byte{"/lang.md": []byte("zh")}}
	b := &fakeSource{uploads: map[string][]byte{"res_b": []byte("bravo")}}
	if err := os.MkdirAll(filepath.Join(root, "sesn_a", "memory"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_a", 1001, []bmaexecutor.MountedFile{file("data/a.txt", "res_a", []byte("alpha"))},
		[]bmaexecutor.MountedStore{{Slug: "prefs", StoreID: "memstore_a", ReadOnly: true,
			Files: []bmaexecutor.MountedFile{file("lang.md", "/lang.md", []byte("zh"))}}}, a)); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_b", 1002, []bmaexecutor.MountedFile{file("b.txt", "res_b", []byte("bravo"))}, nil, b)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Unmount("sesn_a"); _ = m.Unmount("sesn_b") })
	aFile := filepath.Join(root, "sesn_a", "uploads", "data", "a.txt")
	if got, err := runAs(1001, "cat", aFile, filepath.Join(root, "sesn_a", "memory", "prefs", "lang.md")); err != nil || got != "alphazh" {
		t.Fatalf("owner read %q %v", got, err)
	}
	if got, err := runAs(1002, "cat", aFile); err == nil || !strings.Contains(got, "ermission denied") {
		t.Fatalf("session b's user read session a: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "sesn_b", "uploads", "b.txt")); err != nil || string(got) != "bravo" {
		t.Fatalf("root read %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "sesn_b", "uploads", "data")); !os.IsNotExist(err) {
		t.Fatalf("session b sees session a's directory: %v", err)
	}
	for name, try := range map[string]func() error{
		"write":  func() error { return os.WriteFile(aFile, []byte("x"), 0o644) },
		"create": func() error { return os.WriteFile(filepath.Join(root, "sesn_a", "uploads", "new"), nil, 0o644) },
		"chmod":  func() error { return os.Chmod(aFile, 0o644) },
		"remove": func() error { return os.Remove(aFile) },
	} {
		if err := try(); !errors.Is(err, syscall.EROFS) {
			t.Fatalf("%s: %v, want EROFS", name, err)
		}
	}
	mounts, _ := os.ReadFile("/proc/self/mounts")
	if n := strings.Count(string(mounts), "fuse."+FSName); n != 3 {
		t.Fatalf("%d fuse.%s mounts in /proc/self/mounts:\n%s", n, FSName, mounts)
	}
	info, err := os.Stat(filepath.Join(root, "sesn_a", "uploads"))
	if err != nil || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != 1001 {
		t.Fatalf("uploads root %v %v", info, err)
	}
}

// A large upload is read lazily: opening it and reading a few bytes fetches
// one range, not the file; a whole read gets every range once.
func TestMountLargeUploadReadLazily(t *testing.T) {
	m, root := newTestMounter(t)
	content := bytes.Repeat([]byte("0123456789abcdef"), (3*ChunkBytes)/16)
	src := &fakeSource{uploads: map[string][]byte{"res_big": content}}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_l", 0, []bmaexecutor.MountedFile{file("big.bin", "res_big", content)}, nil, src)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Unmount("sesn_l") })
	f, err := os.Open(filepath.Join(root, "sesn_l", "uploads", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 2*ChunkBytes+3); err != nil || string(buf) != "3456" {
		t.Fatalf("read %q %v", buf, err)
	}
	if want := fmt.Sprintf("res_big@%d+%d", 2*ChunkBytes, ChunkBytes); strings.Join(src.callList(), ",") != want {
		t.Fatalf("calls %v, want %s", src.callList(), want)
	}
	all, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(all, content) {
		t.Fatalf("whole read: %d bytes %v", len(all), err)
	}
	if src.callCount() != 3 {
		t.Fatalf("calls %v", src.callList())
	}
}

// Without the platform: what is cached is read, what is not fails with EIO
// at once (no hang).
func TestMountNetworkLoss(t *testing.T) {
	m, root := newTestMounter(t)
	src := &fakeSource{uploads: map[string][]byte{"res_1": []byte("one"), "res_2": []byte("two")}}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_n", 0, []bmaexecutor.MountedFile{
		file("one", "res_1", []byte("one")), file("two", "res_2", []byte("two"))}, nil, src)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Unmount("sesn_n") })
	dir := filepath.Join(root, "sesn_n", "uploads")
	if got, err := os.ReadFile(filepath.Join(dir, "one")); err != nil || string(got) != "one" {
		t.Fatalf("read %q %v", got, err)
	}
	src.mu.Lock()
	src.down = true
	src.mu.Unlock()
	began := time.Now()
	if _, err := os.ReadFile(filepath.Join(dir, "two")); !errors.Is(err, syscall.EIO) {
		t.Fatalf("uncached read without the network: %v, want EIO", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatalf("EIO took %v", time.Since(began))
	}
	if got, err := os.ReadFile(filepath.Join(dir, "one")); err != nil || string(got) != "one" {
		t.Fatalf("cached read without the network %q %v", got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("listing without the network: %v %v", entries, err)
	}
}

// A refresh shows the new listing: a removed file is gone, a changed one
// has its new content, a removed store is detached, an added one mounted.
func TestMountRefreshFollowsTheManifest(t *testing.T) {
	m, root := newTestMounter(t)
	src := &fakeSource{uploads: map[string][]byte{"res_1": []byte("v1"), "res_2": []byte("v2!"), "res_x": []byte("gone")}}
	memory := filepath.Join(root, "sesn_r", "memory")
	if err := os.MkdirAll(memory, 0o555); err != nil {
		t.Fatal(err)
	}
	store := func(slug string) bmaexecutor.MountedStore {
		return bmaexecutor.MountedStore{Slug: slug, StoreID: "memstore_" + slug, ReadOnly: true}
	}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_r", 0, []bmaexecutor.MountedFile{
		file("f", "res_1", []byte("v1")), file("x", "res_x", []byte("gone"))}, []bmaexecutor.MountedStore{store("old")}, src)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Unmount("sesn_r") })
	dir := filepath.Join(root, "sesn_r", "uploads")
	if got, _ := os.ReadFile(filepath.Join(dir, "f")); string(got) != "v1" {
		t.Fatalf("f = %q", got)
	}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_r", 0, []bmaexecutor.MountedFile{
		file("f", "res_2", []byte("v2!"))}, []bmaexecutor.MountedStore{store("new")}, src)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(kernelTimeout + 200*time.Millisecond)
	if got, err := os.ReadFile(filepath.Join(dir, "f")); err != nil || string(got) != "v2!" {
		t.Fatalf("f after refresh %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Fatalf("removed upload still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(memory, "old")); !os.IsNotExist(err) {
		t.Fatalf("removed store still mounted: %v", err)
	}
	mounts, _ := os.ReadFile("/proc/self/mounts")
	if !strings.Contains(string(mounts), filepath.Join(memory, "new")) {
		t.Fatalf("added store not mounted:\n%s", mounts)
	}
}

// Unmount removes the mount points; a mount left by a dead executor is
// detached by DetachStale; the probe mounts and unmounts.
func TestMountUnmountDetachStaleAndProbe(t *testing.T) {
	m, root := newTestMounter(t)
	src := &fakeSource{uploads: map[string][]byte{}}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_u", 0, nil, nil, src)); err != nil {
		t.Fatal(err)
	}
	if err := m.Unmount("sesn_u"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "sesn_u", "uploads")); !os.IsNotExist(err) {
		t.Fatalf("mount point left: %v", err)
	}
	// A "dead executor": mounted by another Mounter that is never unmounted.
	other, _ := New(Options{CacheDir: filepath.Join(t.TempDir(), "c2")})
	if err := other.Mount(t.Context(), sessionMount(root, "sesn_s", 0, nil, nil, src)); err != nil {
		t.Fatal(err)
	}
	if err := DetachStale(root); err != nil {
		t.Fatal(err)
	}
	if mounts, _ := os.ReadFile("/proc/self/mounts"); strings.Contains(string(mounts), filepath.Join(root, "sesn_s")) {
		t.Fatalf("stale mount left:\n%s", mounts)
	}
	if err := Probe(t.TempDir()); err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// mountStore mounts one read_write store of session sesn_w owned by uid 1001
// with files, and answers its directory.
func mountStore(t *testing.T, files map[string]string) (string, *fakeSource) {
	t.Helper()
	m, root := newTestMounter(t)
	src := &fakeSource{uploads: map[string][]byte{}, memories: map[string][]byte{}}
	var listed []bmaexecutor.MountedFile
	for p, c := range files {
		src.memories["/"+p] = []byte(c)
		listed = append(listed, file(p, "/"+p, []byte(c)))
	}
	if err := os.MkdirAll(filepath.Join(root, "sesn_w", "memory"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount(t.Context(), sessionMount(root, "sesn_w", 1001, nil,
		[]bmaexecutor.MountedStore{{Slug: "notes", StoreID: "memstore_w", Files: listed}}, src)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Unmount("sesn_w") })
	return filepath.Join(root, "sesn_w", "memory", "notes"), src
}

func digestText(c string) string { return digestOf([]byte(c)) }

// A read_write store is written through at close: a change against the
// digest it was opened with, a new file against none, an unlink as a delete;
// write-temporary-then-rename saves one version and the scratch name is never
// sent; the session's user may write, another may not; commits are fast.
func TestMountReadWriteStoreCommitsAtClose(t *testing.T) {
	dir, src := mountStore(t, map[string]string{"todo.md": "v1", "old.md": "bye"})
	todo := filepath.Join(dir, "todo.md")
	began := time.Now()
	if err := os.WriteFile(todo, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 400*time.Millisecond {
		t.Fatalf("commit took %v", took)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "new.md"), []byte("x"), 0o644); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file in a missing directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "new.md"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".todo.md.swp"), []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, ".todo.md.swp"), todo); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "old.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := runAs(1001, "sh", "-c", "printf mine > "+filepath.Join(dir, "own.md")+" && cat "+todo); err != nil || got != "v3" {
		t.Fatalf("the session's user: %q %v", got, err)
	}
	if got, err := runAs(1002, "sh", "-c", "printf x > "+filepath.Join(dir, "other.md")); err == nil {
		t.Fatalf("another user wrote: %q", got)
	}
	memories, writes, reports := src.state()
	want := map[string]string{"/todo.md": "v3", "/sub/new.md": "fresh", "/own.md": "mine"}
	if fmt.Sprint(memories) != fmt.Sprint(want) {
		t.Fatalf("store = %v", memories)
	}
	wantWrites := []string{"PUT /todo.md base=" + digestText("v1"), "PUT /sub/new.md base=",
		"PUT /todo.md base=" + digestText("v2"), "DELETE /old.md base=" + digestText("bye"), "PUT /own.md base="}
	if strings.Join(writes, "\n") != strings.Join(wantWrites, "\n") {
		t.Fatalf("writes:\n%s", strings.Join(writes, "\n"))
	}
	if len(reports) != 0 {
		t.Fatalf("reports %+v", reports)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "own.md,sub,todo.md" {
		t.Fatalf("listing %v", names)
	}
	info, err := os.Stat(todo)
	if err != nil || info.Mode().Perm() != 0o644 || info.Sys().(*syscall.Stat_t).Uid != 1001 || info.Size() != 2 {
		t.Fatalf("todo.md %v %v", info, err)
	}
}

// The store wins a conflict (EIO, the store's version shown, reported); a
// rule's refusal is its errno; without the platform the close is EIO and the
// store's content stays; links, modes and moves to other mounts are refused.
func TestMountReadWriteStoreFailures(t *testing.T) {
	dir, src := mountStore(t, map[string]string{"a.md": "base"})
	a := filepath.Join(dir, "a.md")
	if b, err := os.ReadFile(a); err != nil || string(b) != "base" {
		t.Fatalf("read %q %v", b, err)
	}
	src.mu.Lock()
	src.memories["/a.md"] = []byte("theirs")
	src.mu.Unlock()
	if err := os.WriteFile(a, []byte("mine"), 0o644); !errors.Is(err, syscall.EIO) {
		t.Fatalf("stale write: %v, want EIO", err)
	}
	if b, err := os.ReadFile(a); err != nil || string(b) != "theirs" {
		t.Fatalf("after the conflict %q %v", b, err)
	}
	if err := os.WriteFile(a, bytes.Repeat([]byte("x"), maxMemoryBytes+1), 0o644); !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("oversized: %v, want EFBIG", err)
	}
	src.mu.Lock()
	src.full = true
	src.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("b"), 0o644); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("store full: %v, want ENOSPC", err)
	}
	src.mu.Lock()
	src.full, src.down = false, true
	src.mu.Unlock()
	began := time.Now()
	if err := os.WriteFile(a, []byte("offline"), 0o644); !errors.Is(err, syscall.EIO) {
		t.Fatalf("offline: %v, want EIO", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatalf("not_saved took %v", time.Since(began))
	}
	if b, err := os.ReadFile(a); err != nil || string(b) != "theirs" {
		t.Fatalf("after not_saved %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.md")); !os.IsNotExist(err) {
		t.Fatalf("a refused new file stays: %v", err)
	}
	for name, err := range map[string]error{
		"symlink": os.Symlink("a.md", filepath.Join(dir, "l.md")),
		"link":    os.Link(a, filepath.Join(dir, "h.md")),
		"chmod":   os.Chmod(a, 0o600),
	} {
		if !errors.Is(err, syscall.EPERM) {
			t.Fatalf("%s: %v, want EPERM", name, err)
		}
	}
	_, _, reports := src.state()
	var outcomes []string
	for _, r := range reports {
		outcomes = append(outcomes, r.Outcome+" "+r.Path)
	}
	if strings.Join(outcomes, ",") != "conflict /a.md,rejected /b.md" {
		t.Fatalf("reports %v", outcomes)
	}
}

// More than bulkDeleteLimit deletions between two refreshes are refused
// (EPERM, reported once).
func TestMountReadWriteStoreRefusesBulkDelete(t *testing.T) {
	files := map[string]string{}
	for i := range bulkDeleteLimit + 2 {
		files[fmt.Sprintf("m%02d.md", i)] = fmt.Sprint(i)
	}
	dir, src := mountStore(t, files)
	var refused int
	for i := range bulkDeleteLimit + 2 {
		if err := os.Remove(filepath.Join(dir, fmt.Sprintf("m%02d.md", i))); errors.Is(err, syscall.EPERM) {
			refused++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	memories, _, reports := src.state()
	if refused != 2 || len(memories) != 2 || len(reports) != 1 || reports[0].Outcome != "delete_refused" {
		t.Fatalf("refused %d, left %d, reports %+v", refused, len(memories), reports)
	}
}
