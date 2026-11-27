//go:build !windows

package bmaexecutor

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMounter records what SessionFiles asks it to show.
type fakeMounter struct {
	mu        sync.Mutex
	mounts    []SessionMount
	unmounted []string
	fail      error
}

func (m *fakeMounter) Mount(_ context.Context, s SessionMount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.mounts = append(m.mounts, s)
	return nil
}

func (m *fakeMounter) Unmount(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmounted = append(m.unmounted, sessionID)
	return nil
}

func (m *fakeMounter) Stats() SessionFilesStats {
	return SessionFilesStats{Mounts: int64(len(m.mounts))}
}

func (m *fakeMounter) last(t *testing.T) SessionMount {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.mounts) == 0 {
		t.Fatal("nothing mounted")
	}
	return m.mounts[len(m.mounts)-1]
}

func newMountHarness(t *testing.T, daemon *fakeDaemonFiles, mounter *fakeMounter) (processHarness, *SessionFiles, string) {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	root, err := filepath.EvalSymlinks(t.TempDir())
	noErr(t, err)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "sesn_a", "memory"), 0o755) })
	files, err := NewSessionFiles(SessionFilesOptions{Root: root, Mounter: mounter})
	noErr(t, err)
	core := newTestCore(t, CoreOptions{})
	noErr(t, core.Register(NewProcessCapability(core, ProcessOptions{SessionFiles: files})))
	if c := core.Capabilities().SessionFiles; c == nil || !c.Mount || !c.Memory || c.Root != root {
		t.Fatalf("session_files capability = %+v", c)
	}
	b, _ := attachInProcess(t, core, "cloud", func(o *Options) { o.BaseURL, o.EnvironmentKey = server.URL, "mtok" })
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_files", Lease: Lease{Token: "lease"}})
	return processHarness{executor: b.executor, session: session.ID()}, files, root
}

func mountDaemon() *fakeDaemonFiles {
	return &fakeDaemonFiles{
		processes: map[string]string{"proc_a": "sesn_a"},
		mounts: map[string][]fakeMount{"sesn_a": {{resourceID: "res_a", mountPath: "/mnt/session/uploads/data/a.txt",
			content: []byte("alphabet")}}},
		memory: map[string][]*fakeMemoryStore{"sesn_a": {{id: "memstore_a", slug: "prefs",
			files: map[string][]byte{"/lang.md": []byte("zh-CN")}}}},
	}
}

// With a mounter the executor declares session_files.mount and, before a
// process of a session starts, hands the mounter the session's listing
// instead of copying: the uploads and each store at the copy's paths, the
// owner, nothing copied; the memory directory is 0555 with the marker and
// BMA_MEMORY_DIR names it. The source reads through the anchor, an upload by
// byte range (ADR-0111).
func TestSessionFilesMountModeMountsInsteadOfCopying(t *testing.T) {
	daemon, mounter := mountDaemon(), &fakeMounter{}
	h, files, root := newMountHarness(t, daemon, mounter)
	got := startIn(t, h, "proc_a", "sesn_a", "sh", "-c", `printf '%s' "$BMA_MEMORY_DIR"`)
	memory := filepath.Join(root, "sesn_a", "memory")
	if got != memory {
		t.Fatalf("BMA_MEMORY_DIR = %q", got)
	}
	m := mounter.last(t)
	uploads, outputs := files.Dirs("sesn_a")
	if m.SessionID != "sesn_a" || m.Uploads != uploads || m.Memory != memory || m.Owner != (MountOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}) {
		t.Fatalf("mount = %+v", m)
	}
	if len(m.Files) != 1 || m.Files[0] != (MountedFile{Path: "data/a.txt", Key: "res_a", Digest: digestOf([]byte("alphabet")), Size: 8}) {
		t.Fatalf("files = %+v", m.Files)
	}
	if len(m.Stores) != 1 || m.Stores[0].Slug != "prefs" || m.Stores[0].StoreID != "memstore_a" || !m.Stores[0].ReadOnly ||
		len(m.Stores[0].Files) != 1 || m.Stores[0].Files[0] != (MountedFile{Path: "lang.md", Key: "/lang.md",
		Digest: digestOf([]byte("zh-CN")), Size: 5}) {
		t.Fatalf("stores = %+v", m.Stores)
	}
	if _, err := os.Stat(filepath.Join(uploads, "data", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("an upload was copied: %v", err)
	}
	if info, err := os.Stat(outputs); err != nil || !info.IsDir() {
		t.Fatalf("outputs: %v %v", info, err)
	}
	if info, err := os.Stat(memory); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("memory directory: %v %v", info, err)
	}
	assertMode(t, filepath.Join(memory, sessionMemoryMarker), 0o444)

	body, err := m.Source.Upload(testContext(t), "res_a", 2, 3)
	noErr(t, err)
	b, _ := io.ReadAll(body)
	_ = body.Close()
	if string(b) != "pha" || strings.Join(daemon.ranges, ",") != "res_a:2-4" {
		t.Fatalf("range read %q, ranges %v", b, daemon.ranges)
	}
	body, digest, size, err := m.Source.Memory(testContext(t), "memstore_a", "/lang.md")
	noErr(t, err)
	b, _ = io.ReadAll(body)
	_ = body.Close()
	if string(b) != "zh-CN" || digest != digestOf(b) || size != 5 {
		t.Fatalf("memory read %q %s %d", b, digest, size)
	}
	if stats := files.Stats(); stats == nil || stats.Mounts != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

// session_files.sync refreshes the stores only: the uploads listing stays as
// the last process start showed it, the new store content is listed.
func TestSessionFilesMountModeSyncRefreshesMemoryOnly(t *testing.T) {
	daemon, mounter := mountDaemon(), &fakeMounter{}
	daemon.processes["prc_sync"] = "sesn_a"
	h, _, _ := newMountHarness(t, daemon, mounter)
	startIn(t, h, "proc_a", "sesn_a", "true")
	daemon.update(func() {
		daemon.mounts["sesn_a"] = nil
		daemon.memory["sesn_a"][0].files["/new.md"] = []byte("n")
	})
	var result SessionFilesSyncResult
	noErr(t, h.executor.Call(testContext(t), MethodSessionFilesSync,
		SessionFilesSyncParams{AgentSessionID: "sesn_a", DownloadToken: "prc_sync"}, &result))
	if result.State != "synced" {
		t.Fatalf("sync = %+v", result)
	}
	m := mounter.last(t)
	if len(m.Files) != 1 || len(m.Stores) != 1 || len(m.Stores[0].Files) != 2 {
		t.Fatalf("after sync: files %+v stores %+v", m.Files, m.Stores)
	}
}

// A mount that fails fails the process start: there is no copy instead.
func TestSessionFilesMountModeFailureFailsTheStart(t *testing.T) {
	daemon, mounter := mountDaemon(), &fakeMounter{fail: errors.New("no /dev/fuse")}
	h, files, _ := newMountHarness(t, daemon, mounter)
	_, err := h.start(t, ProcessStartParams{ProcessID: "proc_a", Argv: []string{"true"}, WaitMS: 5000,
		Metadata: map[string]string{MetadataAgentSessionID: "sesn_a"}})
	if err == nil || !strings.Contains(err.Error(), "mount: no /dev/fuse") {
		t.Fatalf("start = %v", err)
	}
	uploads, _ := files.Dirs("sesn_a")
	if _, err := os.Stat(filepath.Join(uploads, "data", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("copied after a failed mount: %v", err)
	}
}

// A mounted session renews its anchor once it is sessionAnchorRenewal old,
// and content requests name the renewed anchor; a session idle with no
// running process is unmounted, one with a running process is kept.
func TestSessionFilesMountModeRenewsAndUnmountsIdle(t *testing.T) {
	daemon, mounter := mountDaemon(), &fakeMounter{}
	h, files, _ := newMountHarness(t, daemon, mounter)
	startIn(t, h, "proc_a", "sesn_a", "true")
	session := files.session("sesn_a")
	session.mu.Lock()
	session.anchoredAt = time.Now().Add(-sessionAnchorRenewal)
	session.mu.Unlock()
	files.renewAnchors(testContext(t), []*fileSession{session})
	if daemon.renewals != 1 || session.anchor != "prc_renewed1" {
		t.Fatalf("renewals %d, anchor %s", daemon.renewals, session.anchor)
	}
	files.renewAnchors(testContext(t), []*fileSession{session}) // fresh: not renewed again
	if daemon.renewals != 1 {
		t.Fatalf("renewed a fresh anchor")
	}

	files.processStarted("sesn_a")
	files.active(time.Now().Add(time.Hour))
	if len(mounter.unmounted) != 0 {
		t.Fatalf("unmounted a session with a running process")
	}
	files.processEnded("sesn_a")
	files.active(time.Now().Add(time.Hour))
	if strings.Join(mounter.unmounted, ",") != "sesn_a" {
		t.Fatalf("unmounted = %v", mounter.unmounted)
	}
}

// An executor whose probe failed copies and reports why.
func TestSessionFilesReportsMountUnavailable(t *testing.T) {
	files, err := NewSessionFiles(SessionFilesOptions{Root: t.TempDir(), MountUnavailable: "open /dev/fuse: no such file"})
	noErr(t, err)
	if stats := files.Stats(); stats == nil || stats.MountUnavailable != "open /dev/fuse: no such file" {
		t.Fatalf("stats = %+v", stats)
	}
	if stats := mustFiles(t).Stats(); stats != nil {
		t.Fatalf("a copying executor that never tried reports %+v", stats)
	}
}

func mustFiles(t *testing.T) *SessionFiles {
	t.Helper()
	files, err := NewSessionFiles(SessionFilesOptions{Root: t.TempDir()})
	noErr(t, err)
	return files
}

func TestMountOwnerIsTheSessionRunAs(t *testing.T) {
	files, err := NewSessionFiles(SessionFilesOptions{Root: t.TempDir(), Mounter: &fakeMounter{},
		RunAs: func(id string) (*RunAs, error) {
			if id == "sesn_root" {
				return nil, nil
			}
			return &RunAs{UID: 4242, GID: 4343}, nil
		}})
	noErr(t, err)
	if owner, err := files.owner("sesn_a"); err != nil || owner != (MountOwner{UID: 4242, GID: 4343}) {
		t.Fatalf("owner = %+v, %v", owner, err)
	}
	own := MountOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())} //nolint:gosec // ids fit in uint32
	if owner, err := files.owner("sesn_root"); err != nil || owner != own {
		t.Fatalf("owner without an identity = %+v, %v", owner, err)
	}
}
