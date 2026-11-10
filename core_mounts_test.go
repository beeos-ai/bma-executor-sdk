//go:build !windows

package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A local brain's file tools reach the session's own uploads, skills and
// outputs directories below the executor's session files root (ADR-0021) and
// nothing else: another session's directory, a link out of a directory and a
// write to a read-only directory are refused.

type mountRig struct {
	*fileRig
	root string // session files root
	ours string // <root>/<rigSession>
}

func newMountRig(t *testing.T, coreOpts CoreOptions) *mountRig {
	t.Helper()
	root := filepath.Join(t.TempDir(), "session")
	for _, p := range []string{
		filepath.Join(rigSession, "uploads", "docs"), filepath.Join(rigSession, "skills", "pdf"), filepath.Join(rigSession, "outputs"),
		filepath.Join("sesn_other", "uploads"), filepath.Join("sesn_other", "skills"), filepath.Join("sesn_other", "outputs"),
	} {
		noErr(t, os.MkdirAll(filepath.Join(root, p), 0o755))
	}
	for p, content := range map[string]string{
		filepath.Join(rigSession, "uploads", "docs", "a.txt"):  "alpha upload\n",
		filepath.Join(rigSession, "skills", "pdf", "SKILL.md"): "# pdf skill\nuse it\n",
		filepath.Join("sesn_other", "uploads", "secret.txt"):   "other session secret\n",
		filepath.Join("sesn_other", "skills", "secret.md"):     "other skill secret\n",
	} {
		noErr(t, os.WriteFile(filepath.Join(root, p), []byte(content), 0o444))
	}
	r := newFileRigAt(t, coreOpts, root)
	return &mountRig{fileRig: r, root: root, ours: filepath.Join(root, rigSession)}
}

// newFileRigAt is newFileRig with the route given the session files root.
func newFileRigAt(t *testing.T, coreOpts CoreOptions, filesRoot string) *fileRig {
	t.Helper()
	dir := t.TempDir()
	coreOpts.WorkRoot = filepath.Join(dir, "work")
	core, err := NewCore(coreOpts)
	noErr(t, err)
	t.Cleanup(core.Close)
	workspace := filepath.Join(dir, "ws")
	noErr(t, os.MkdirAll(workspace, 0o755))
	outside := filepath.Join(dir, "outside")
	noErr(t, os.MkdirAll(outside, 0o755))
	noErr(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside secret\n"), 0o644))
	route, err := NewLocalToolRoute(LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: rigSession, TurnID: rigTurn,
		Workspace: workspace, PermissionMode: "accept_edits", SessionFilesRoot: filesRoot})
	noErr(t, err)
	return &fileRig{route: route, core: core, workspace: workspace, outside: outside}
}

func (r *mountRig) path(parts ...string) string {
	return filepath.Join(append([]string{r.ours}, parts...)...)
}

func wantText(t *testing.T, got *RouteResult, what string, substrings ...string) {
	t.Helper()
	if got.IsError {
		t.Fatalf("%s failed: %+v", what, got)
	}
	for _, s := range substrings {
		if !strings.Contains(got.Text, s) {
			t.Fatalf("%s = %q, want %q in it", what, got.Text, s)
		}
	}
}

func wantNoLeak(t *testing.T, got *RouteResult, what string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(got.Text, s) {
			t.Fatalf("%s leaked %q: %+v", what, s, got)
		}
	}
}

func TestSessionMountsAreReadable(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	wantText(t, r.call("read", map[string]any{"file_path": r.path("uploads", "docs", "a.txt")}), "read upload", "alpha upload")
	wantText(t, r.call("read", map[string]any{"file_path": r.path("skills", "pdf", "SKILL.md"), "offset": 2, "limit": 1}), "read skill", "use it")
	// glob and grep report absolute paths the model can pass back to read.
	wantText(t, r.call("glob", map[string]any{"path": r.path("skills"), "pattern": "*/SKILL.md"}), "glob skills", r.path("skills", "pdf", "SKILL.md"))
	wantText(t, r.call("glob", map[string]any{"path": r.path("uploads"), "pattern": "docs/*.txt"}), "glob uploads", r.path("uploads", "docs", "a.txt"))
	wantText(t, r.call("grep", map[string]any{"path": r.path("uploads"), "pattern": "alpha"}), "grep uploads", r.path("uploads", "docs", "a.txt")+":1:alpha upload")
	// The workspace is as before: relative paths, relative results.
	noErr(t, os.WriteFile(filepath.Join(r.workspace, "w.txt"), []byte("work\n"), 0o644))
	wantText(t, r.call("glob", map[string]any{"pattern": "*.txt"}), "glob workspace", "w.txt")
	if got := r.call("glob", map[string]any{"pattern": "*.txt"}); strings.Contains(got.Text, r.workspace) {
		t.Fatalf("workspace glob is absolute now: %q", got.Text)
	}
	// An absolute path of the session's own directory, in the form the OS resolves it, works too.
	real, err := filepath.EvalSymlinks(r.path("uploads", "docs", "a.txt"))
	noErr(t, err)
	wantText(t, r.call("read", map[string]any{"file_path": real}), "read by real path", "alpha upload")
}

func TestSessionOutputsAreWritable(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	out := r.path("outputs", "report", "result.txt")
	wantText(t, r.call("write", map[string]any{"file_path": out, "content": "deliverable one\n"}), "write output", "wrote")
	if raw, err := os.ReadFile(out); err != nil || string(raw) != "deliverable one\n" {
		t.Fatalf("output = %q, %v", raw, err)
	}
	wantText(t, r.call("edit", map[string]any{"file_path": out, "old_string": "one", "new_string": "two"}), "edit output", "edited")
	wantText(t, r.call("edit", map[string]any{"file_path": out, "old_string": "deliverable", "new_string": "result", "replace_all": true}), "edit all output", "edited")
	wantText(t, r.call("read", map[string]any{"file_path": out}), "read output", "result two")
	wantText(t, r.call("grep", map[string]any{"path": r.path("outputs"), "pattern": "result"}), "grep outputs", out+":1:result two")
}

func TestSessionUploadsAndSkillsAreReadOnly(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	for _, p := range []string{r.path("uploads", "docs", "a.txt"), r.path("uploads", "new.txt"), r.path("skills", "pdf", "SKILL.md"), r.path("skills", "evil", "SKILL.md")} {
		wantToolError(t, r.call("write", map[string]any{"file_path": p, "content": "pwned"}), "write "+p)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "alpha", "new_string": "pwned"}), "edit "+p)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "alpha", "new_string": "pwned", "replace_all": true}), "edit all "+p)
	}
	if raw, _ := os.ReadFile(r.path("uploads", "docs", "a.txt")); string(raw) != "alpha upload\n" {
		t.Fatalf("upload changed: %q", raw)
	}
	for _, p := range []string{r.path("uploads", "new.txt"), r.path("skills", "evil")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s was created", p)
		}
	}
}

// Only this session's three directories: not the session directory, not the
// root, not another session's, not a ".." into one.
func TestSessionMountsRefuseOtherPaths(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	secret := filepath.Join(r.root, "sesn_other", "uploads", "secret.txt")
	for name, p := range map[string]string{
		"another session":           secret,
		"another session's skills":  filepath.Join(r.root, "sesn_other", "skills", "secret.md"),
		"another session's outputs": filepath.Join(r.root, "sesn_other", "outputs", "x"),
		"dotdot into another":       r.path("uploads", "..", "..", "sesn_other", "uploads", "secret.txt"),
		"dotdot out of uploads":     r.path("uploads", "..", "..", "..", "outside.txt"),
		"our directory":             r.path("hidden", "x"),
		"our session directory":     r.ours,
		"the root":                  r.root,
		"a sibling of uploads":      r.ours + "-evil/uploads/x",
		"a prefix of uploads":       r.path("uploads-evil", "x"),
		"the outside":               filepath.Join(r.outside, "secret.txt"),
	} {
		got := r.call("read", map[string]any{"file_path": p})
		wantToolError(t, got, "read "+name)
		wantNoLeak(t, got, "read "+name, "other session secret", "outside secret")
		for _, tool := range []string{"glob", "grep"} {
			input := map[string]any{"path": p, "pattern": "*"}
			if tool == "grep" {
				input["pattern"] = "secret"
			}
			got := r.call(tool, input)
			if !got.IsError {
				wantNoLeak(t, got, tool+" "+name, "outside secret", "other session secret", "other skill secret")
				if strings.TrimSpace(got.Text) != "" {
					t.Fatalf("%s %s found %q", tool, name, got.Text)
				}
			}
		}
		wantToolError(t, r.call("write", map[string]any{"file_path": p, "content": "pwned"}), "write "+name)
	}
	// A glob pattern cannot climb out either.
	got := r.call("glob", map[string]any{"path": r.path("uploads"), "pattern": "../../sesn_other/uploads/*"})
	wantToolError(t, got, "glob pattern with ..")
	if raw, _ := os.ReadFile(secret); string(raw) != "other session secret\n" {
		t.Fatalf("other session's file changed: %q", raw)
	}
	// A relative path is the workspace's, never a mount's.
	wantToolError(t, r.call("read", map[string]any{"file_path": "uploads/docs/a.txt"}), "read relative uploads path")
}

func TestSessionMountsRefuseLinksOut(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	other := filepath.Join(r.root, "sesn_other")
	// Links the agent could have made inside its own directories.
	noErr(t, os.Symlink(filepath.Join(r.outside, "secret.txt"), r.path("outputs", "to-file")))
	noErr(t, os.Symlink(r.outside, r.path("outputs", "to-dir")))
	noErr(t, os.Symlink(filepath.Join(other, "uploads", "secret.txt"), r.path("outputs", "to-other-file")))
	noErr(t, os.Symlink(filepath.Join(other, "uploads"), r.path("outputs", "to-other-dir")))
	noErr(t, os.Symlink("../../sesn_other/uploads/secret.txt", r.path("outputs", "relative")))
	for _, name := range []string{"to-file", "to-other-file", "relative", "to-dir/secret.txt", "to-other-dir/secret.txt"} {
		p := r.path("outputs", name)
		got := r.call("read", map[string]any{"file_path": p})
		wantToolError(t, got, "read "+name)
		wantNoLeak(t, got, "read "+name, "outside secret", "other session secret")
		wantToolError(t, r.call("write", map[string]any{"file_path": p, "content": "pwned"}), "write "+name)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "secret", "new_string": "pwned"}), "edit "+name)
	}
	wantToolError(t, r.call("write", map[string]any{"file_path": r.path("outputs", "to-dir", "new.txt"), "content": "x"}), "create through a directory link")
	wantToolError(t, r.call("write", map[string]any{"file_path": r.path("outputs", "to-other-dir", "sub", "new.txt"), "content": "x"}), "mkdir through a directory link")
	for _, tool := range []string{"glob", "grep"} {
		input := map[string]any{"path": r.path("outputs"), "pattern": "secret"}
		if tool == "glob" {
			input["pattern"] = "to-*/*"
		}
		got := r.call(tool, input)
		wantNoLeak(t, got, tool+" through links", "outside secret", "other session secret")
		if tool == "glob" && strings.Contains(got.Text, "secret.txt") {
			t.Fatalf("glob walked a link: %q", got.Text)
		}
	}
	for _, p := range []string{filepath.Join(r.outside, "new.txt"), filepath.Join(other, "uploads", "new.txt")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s was created through a link", p)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(r.outside, "secret.txt")); string(raw) != "outside secret\n" {
		t.Fatalf("outside file changed: %q", raw)
	}
}

// The directories themselves must be real directories: a session directory or
// a mount that is a link to another one (the link is not followed).
func TestSessionMountDirectoryThatIsALinkIsRefused(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	other := filepath.Join(r.root, "sesn_other")
	noErr(t, os.RemoveAll(r.path("uploads")))
	noErr(t, os.Symlink(filepath.Join(other, "uploads"), r.path("uploads")))
	got := r.call("read", map[string]any{"file_path": r.path("uploads", "secret.txt")})
	wantToolError(t, got, "read through a linked uploads")
	wantNoLeak(t, got, "read through a linked uploads", "other session secret")
	wantNoLeak(t, r.call("grep", map[string]any{"path": r.path("uploads"), "pattern": "secret"}), "grep through a linked uploads", "other session secret")

	// A linked session directory, made before the route looks at it.
	r2 := newMountRig(t, CoreOptions{})
	noErr(t, os.RemoveAll(r2.ours))
	noErr(t, os.Symlink(filepath.Join(r2.root, "sesn_other"), r2.ours))
	got = r2.call("read", map[string]any{"file_path": r2.path("uploads", "secret.txt")})
	wantToolError(t, got, "read through a linked session directory")
	wantNoLeak(t, got, "read through a linked session directory", "other session secret")
	wantToolError(t, r2.call("write", map[string]any{"file_path": r2.path("outputs", "x.txt"), "content": "x"}), "write through a linked session directory")
	if _, err := os.Lstat(filepath.Join(r2.root, "sesn_other", "outputs", "x.txt")); err == nil {
		t.Fatal("wrote into another session through a linked session directory")
	}
}

// Without the root the directories are not reachable at all, as before.
func TestSessionMountsNeedTheRoot(t *testing.T) {
	r := newMountRig(t, CoreOptions{})
	plain := newFileRigAt(t, CoreOptions{}, "")
	for _, call := range []struct {
		tool  string
		input map[string]any
	}{
		{"read", map[string]any{"file_path": r.path("uploads", "docs", "a.txt")}},
		{"glob", map[string]any{"path": r.path("skills"), "pattern": "*/*"}},
		{"grep", map[string]any{"path": r.path("uploads"), "pattern": "alpha"}},
		{"write", map[string]any{"file_path": r.path("outputs", "x"), "content": "x"}},
	} {
		wantToolError(t, plain.call(call.tool, call.input), call.tool+" without the root")
	}
	// A root that is not absolute, and a session id that is no directory name.
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	ws := t.TempDir()
	if _, err := NewLocalToolRoute(LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: rigSession, TurnID: rigTurn, Workspace: ws, SessionFilesRoot: "session"}); err == nil {
		t.Fatal("a relative root was accepted")
	}
	if _, err := NewLocalToolRoute(LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: "../sesn", TurnID: rigTurn, Workspace: ws, SessionFilesRoot: r.root}); err == nil {
		t.Fatal("an unsafe session id was accepted")
	}
}

// The host's FileAccess hook is asked about a mount path too, with the mount
// and the path below it, and its refusal holds.
func TestSessionMountsPassTheFileAccessHook(t *testing.T) {
	var asked []FileAccessRequest
	r := newMountRig(t, CoreOptions{FileAccess: func(_ context.Context, req FileAccessRequest) error {
		asked = append(asked, req)
		if req.Mount == SessionMountSkills || strings.HasSuffix(req.Path, "private.txt") {
			return errors.New("not for this session")
		}
		return nil
	}})
	wantText(t, r.call("read", map[string]any{"file_path": r.path("uploads", "docs", "a.txt")}), "read upload", "alpha upload")
	wantToolError(t, r.call("read", map[string]any{"file_path": r.path("skills", "pdf", "SKILL.md")}), "read a skill the hook refuses")
	wantText(t, r.call("write", map[string]any{"file_path": r.path("outputs", "ok.txt"), "content": "ok"}), "write output", "wrote")
	wantToolError(t, r.call("write", map[string]any{"file_path": r.path("outputs", "private.txt"), "content": "x"}), "write a path the hook refuses")
	if fileExists(r.path("outputs"), "private.txt") {
		t.Fatal("the refused write ran")
	}
	want := []string{"read uploads docs/a.txt", "read skills pdf/SKILL.md", "write outputs ok.txt", "write outputs private.txt"}
	var got []string
	for _, req := range asked {
		got = append(got, fmt.Sprintf("%s %s %s", req.Operation, req.Mount, req.Path))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("hook asked %q, want %q", got, want)
	}
	// A write to a read-only directory is refused by the tool without the hook.
	before := len(asked)
	wantToolError(t, r.call("write", map[string]any{"file_path": r.path("uploads", "x.txt"), "content": "x"}), "write upload")
	if len(asked) != before {
		t.Fatalf("hook asked about a read-only write: %v", asked[before:])
	}
}

// ACPAgentOptions.SessionFilesRoot: checked, and the turn gets the mounts and
// the prompt that names them.
func TestACPAgentSessionFilesRoot(t *testing.T) {
	self, err := os.Executable()
	noErr(t, err)
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	t.Cleanup(core.Close)
	_, err = NewACPAgent(context.Background(), ACPAgentOptions{Command: []string{self}, Core: core, StateDir: t.TempDir(),
		Upstream: ACPModelUpstream{BaseURL: "http://127.0.0.1:1/v1"}, SessionFilesRoot: "mnt/session"})
	if err == nil || !strings.Contains(err.Error(), "SessionFilesRoot") {
		t.Fatalf("relative root: err = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := filepath.Join(t.TempDir(), "session")
	f := startACPFixture(t, ctx, ACPAgentOptions{SessionFilesRoot: root})
	f.start(t, ctx, rigSession, "turn_1", "hello")
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	mounts := f.agent.opts.Core.session(coreKey{acpBrain, rigSession}).sessionMounts()
	if len(mounts) != 3 {
		t.Fatalf("mounts = %v", mounts)
	}
	note := f.agent.sessionFilesNote(rigSession, false)
	for _, want := range []string{filepath.Join(root, rigSession, "uploads"), filepath.Join(root, rigSession, "skills"), filepath.Join(root, rigSession, "outputs"), "read-only"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note %q lacks %q", note, want)
		}
	}
	// Hands on another machine, an unsafe id and no root: no note, no mounts.
	if got := f.agent.sessionFilesNote(rigSession, true); got != "" {
		t.Fatalf("remote hands note = %q", got)
	}
	if got := f.agent.sessionFilesNote("../x", false); got != "" {
		t.Fatalf("unsafe id note = %q", got)
	}
	plain := startACPFixture(t, ctx, ACPAgentOptions{})
	plain.start(t, ctx, rigSession, "turn_1", "hello")
	assertIdle(t, plain.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if m := plain.agent.opts.Core.session(coreKey{acpBrain, rigSession}).sessionMounts(); len(m) != 0 {
		t.Fatalf("mounts without a root = %v", m)
	}
}
