//go:build !windows

package bmaexecutor

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// brainFilesRig is an executor with session files whose Core also runs a
// local brain's tools (LocalToolRoute, bash allowed), attached to daemon.
type brainFilesRig struct {
	files    *SessionFiles
	executor *Executor
	route    *LocalToolRoute
}

func newBrainFilesRig(t *testing.T, daemon *fakeDaemonFiles) *brainFilesRig {
	t.Helper()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	root, err := filepath.EvalSymlinks(t.TempDir())
	noErr(t, err)
	files, err := NewSessionFiles(SessionFilesOptions{Root: filepath.Join(root, "session")})
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: filepath.Join(root, "work"),
		// A plain sh, not the developer's login profile (as the bash rig).
		Launch: func(_ context.Context, req LaunchRequest) (LaunchSpec, error) {
			return LaunchSpec{Argv: []string{"/bin/sh", "-c", req.Argv[len(req.Argv)-1]}}, nil
		}})
	noErr(t, err)
	t.Cleanup(core.Close)
	noErr(t, core.Register(NewProcessCapability(core, ProcessOptions{SessionFiles: files})))
	b, _ := attachInProcess(t, core, "cloud", func(o *Options) { o.BaseURL, o.EnvironmentKey = server.URL, "mtok" })
	workspace := filepath.Join(root, "work", "acp", rigSession)
	noErr(t, os.MkdirAll(workspace, 0o755))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	route, err := NewLocalToolRoute(LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: rigSession, TurnID: rigTurn,
		Workspace: workspace, ToolPolicy: map[string]string{"bash": "allow"}, Context: ctx, SessionFilesRoot: files.Root()})
	noErr(t, err)
	return &brainFilesRig{files: files, executor: b.executor, route: route}
}

func (r *brainFilesRig) sync(t *testing.T) SessionFilesSyncResult {
	t.Helper()
	var result SessionFilesSyncResult
	noErr(t, r.executor.Call(testContext(t), MethodSessionFilesSync,
		SessionFilesSyncParams{AgentSessionID: rigSession, DownloadToken: "prc_turn"}, &result))
	if result.State != "synced" {
		t.Fatalf("sync = %+v", result)
	}
	return result
}

func (r *brainFilesRig) bash(command string) *RouteResult {
	return r.route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: map[string]any{"command": command}})
}

// A brain on the machine (an ACP brain whose tools the LocalToolRoute runs)
// starts no process: the session_files.sync the platform sends before each of
// its turns shows the session's uploads and memory stores, its bash commands
// get BMA_MEMORY_DIR as the session's processes do, and the sync's
// download_token anchors the outputs they write, so they reach the platform.
func TestSessionFilesSyncServesABrainOnTheMachine(t *testing.T) {
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"prc_turn": rigSession},
		mounts: map[string][]fakeMount{rigSession: {
			{resourceID: "res_1", mountPath: "/mnt/session/uploads/in.txt", content: []byte("upload one")}}},
		memory: map[string][]*fakeMemoryStore{rigSession: {
			{id: "memstore_a", slug: "prefs", files: map[string][]byte{"/a.md": []byte("memory one")}}}},
	}
	r := newBrainFilesRig(t, daemon)
	files := r.files
	r.sync(t)
	uploads, outputs := files.Dirs(rigSession)
	memory := files.MemoryDir(rigSession)
	got := r.bash(`cat "` + filepath.Join(uploads, "in.txt") + `"; echo; ` +
		`printf '[%s]' "$BMA_MEMORY_DIR"; cat "$BMA_MEMORY_DIR/prefs/a.md"; echo; ` +
		`printf deliverable > "` + filepath.Join(outputs, "report.txt") + `"`)
	wantText(t, got, "bash of the brain", "upload one", "["+memory+"]memory one")

	files.session(rigSession).scan(testContext(t), files, true)
	out := daemon.uploaded()
	if len(out) != 1 || out[0].processID != "prc_turn" || out[0].path != "report.txt" || out[0].content != "deliverable" {
		t.Fatalf("outputs uploaded = %+v", out)
	}
	assertMode(t, filepath.Join(uploads, "in.txt"), 0o444)

	// The next turn's sync is the platform's view again: a bash change to an
	// upload is undone, a detached upload goes.
	noErr(t, os.Chmod(filepath.Join(uploads, "in.txt"), 0o644))
	noErr(t, os.WriteFile(filepath.Join(uploads, "in.txt"), []byte("bash"), 0o644))
	r.sync(t)
	if raw, _ := os.ReadFile(filepath.Join(uploads, "in.txt")); string(raw) != "upload one" {
		t.Fatalf("in.txt = %q after the next sync", raw)
	}
	daemon.update(func() { delete(daemon.mounts, rigSession) })
	r.sync(t)
	if _, err := os.Lstat(filepath.Join(uploads, "in.txt")); !os.IsNotExist(err) {
		t.Fatalf("detached upload still there: %v", err)
	}
}

// A session without memory stores gets no BMA_MEMORY_DIR in its bash
// commands, as in its processes.
func TestBrainBashWithoutMemoryHasNoMemoryDir(t *testing.T) {
	daemon := &fakeDaemonFiles{processes: map[string]string{"prc_turn": rigSession}}
	r := newBrainFilesRig(t, daemon)
	r.sync(t)
	wantText(t, r.bash(`printf '[%s]' "${BMA_MEMORY_DIR-unset}"`), "bash", "[unset]")
	if _, err := os.Stat(r.files.MemoryDir(rigSession)); !os.IsNotExist(err) {
		t.Fatalf("memory directory of a session without stores: %v", err)
	}
}
