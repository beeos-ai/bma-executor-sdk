//go:build !windows

package bmaexecutor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The file tools stay inside the session workspace: a link, a ".." or an
// absolute path that leaves it is refused by the os.Root they work through.

type fileRig struct {
	route     *LocalToolRoute
	core      *Core
	workspace string
	outside   string
	n         int
}

func newFileRig(t *testing.T, coreOpts CoreOptions, policy map[string]string) *fileRig {
	t.Helper()
	root := t.TempDir()
	coreOpts.WorkRoot = filepath.Join(root, "work")
	core, err := NewCore(coreOpts)
	noErr(t, err)
	t.Cleanup(core.Close)
	workspace := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	noErr(t, os.MkdirAll(workspace, 0o755))
	noErr(t, os.MkdirAll(outside, 0o755))
	noErr(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644))
	route, err := NewLocalToolRoute(LocalToolRouteOptions{Core: core, StateDir: t.TempDir(), SessionID: rigSession, TurnID: rigTurn,
		Workspace: workspace, PermissionMode: "accept_edits", ToolPolicy: policy})
	noErr(t, err)
	return &fileRig{route: route, core: core, workspace: workspace, outside: outside}
}

func (r *fileRig) call(name string, input map[string]any) *RouteResult {
	r.n++
	return r.route.Call(context.Background(), RouteCall{ID: fmt.Sprintf("c%d", r.n), Name: name, Input: input})
}

func (r *fileRig) callID(id, name string, input map[string]any) *RouteResult {
	return r.route.Call(context.Background(), RouteCall{ID: id, Name: name, Input: input})
}

func wantToolError(t *testing.T, got *RouteResult, what string) {
	t.Helper()
	if !got.IsError {
		t.Fatalf("%s succeeded: %+v", what, got)
	}
}

func TestFileToolsRefuseLinksOutOfTheWorkspace(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	noErr(t, os.Symlink(filepath.Join(r.outside, "secret.txt"), filepath.Join(r.workspace, "flink")))
	noErr(t, os.Symlink(r.outside, filepath.Join(r.workspace, "dlink")))
	noErr(t, os.Symlink("../outside/secret.txt", filepath.Join(r.workspace, "rlink")))

	for _, p := range []string{"flink", "rlink", "dlink/secret.txt", filepath.Join(r.workspace, "flink")} {
		got := r.call("read", map[string]any{"file_path": p})
		wantToolError(t, got, "read "+p)
		if strings.HasPrefix(got.Text, "secret") {
			t.Fatalf("read %s leaked: %q", p, got.Text)
		}
		wantToolError(t, r.call("write", map[string]any{"file_path": p, "content": "pwned"}), "write "+p)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "secret", "new_string": "pwned"}), "edit "+p)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "secret", "new_string": "pwned", "replace_all": true}), "edit all "+p)
	}
	// Creating a file or a directory through a link to a directory outside.
	wantToolError(t, r.call("write", map[string]any{"file_path": "dlink/new.txt", "content": "x"}), "write new through dlink")
	wantToolError(t, r.call("write", map[string]any{"file_path": "dlink/sub/new.txt", "content": "x"}), "mkdir through dlink")
	if raw, _ := os.ReadFile(filepath.Join(r.outside, "secret.txt")); string(raw) != "secret\n" {
		t.Fatalf("outside file changed: %q", raw)
	}
	entries, _ := os.ReadDir(r.outside)
	if len(entries) != 1 {
		t.Fatalf("outside directory has %d entries", len(entries))
	}
}

func TestFileToolsRefuseDotDotAndOutsideAbsolutePaths(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	outsideFile := filepath.Join(r.outside, "secret.txt")
	for _, p := range []string{"../outside/secret.txt", "a/../../outside/secret.txt", outsideFile} {
		wantToolError(t, r.call("read", map[string]any{"file_path": p}), "read "+p)
		wantToolError(t, r.call("write", map[string]any{"file_path": p, "content": "pwned"}), "write "+p)
		wantToolError(t, r.call("edit", map[string]any{"file_path": p, "old_string": "secret", "new_string": "pwned", "replace_all": true}), "edit "+p)
		wantToolError(t, r.call("grep", map[string]any{"path": p, "pattern": "secret"}), "grep "+p)
		wantToolError(t, r.call("glob", map[string]any{"path": p, "pattern": "*"}), "glob "+p)
	}
	wantToolError(t, r.call("glob", map[string]any{"pattern": "../outside/*"}), "glob ../outside/*")
	if raw, _ := os.ReadFile(outsideFile); string(raw) != "secret\n" {
		t.Fatalf("outside file changed: %q", raw)
	}
}

func TestFileToolsWorkInsideTheWorkspace(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	wantExecuted(t, r.call("write", map[string]any{"file_path": "a/b/one.txt", "content": "alpha\nbeta\n"}))
	wantExecuted(t, r.call("write", map[string]any{"file_path": filepath.Join(r.workspace, "two.txt"), "content": "gamma\n"}))
	if got := r.call("read", map[string]any{"file_path": "a/b/one.txt", "offset": 2.0}); got.IsError || got.Text != "beta\n" {
		t.Fatalf("read = %+v", got)
	}
	if got := r.call("edit", map[string]any{"file_path": "a/b/one.txt", "old_string": "alpha", "new_string": "ALPHA"}); got.IsError {
		t.Fatalf("edit = %+v", got)
	}
	if got := r.call("edit", map[string]any{"file_path": "two.txt", "old_string": "gamma", "new_string": "g", "replace_all": true}); got.IsError || !strings.Contains(got.Text, "1 replacements") {
		t.Fatalf("edit all = %+v", got)
	}
	if got := r.call("glob", map[string]any{"pattern": "a/*/*.txt"}); got.IsError || got.Text != filepath.Join("a", "b", "one.txt") {
		t.Fatalf("glob = %+v", got)
	}
	if got := r.call("glob", map[string]any{"path": "a", "pattern": "b/*"}); got.IsError || got.Text != filepath.Join("a", "b", "one.txt") {
		t.Fatalf("glob in a = %+v", got)
	}
	if got := r.call("grep", map[string]any{"pattern": "ALPHA|^g$"}); got.IsError ||
		got.Text != "a/b/one.txt:1:ALPHA\ntwo.txt:1:g" {
		t.Fatalf("grep = %+v", got)
	}
	// A link that stays inside the workspace works, for files and directories.
	noErr(t, os.Symlink("a/b", filepath.Join(r.workspace, "inner")))
	noErr(t, os.Symlink("two.txt", filepath.Join(r.workspace, "two-link")))
	if got := r.call("read", map[string]any{"file_path": "inner/one.txt"}); got.IsError || !strings.HasPrefix(got.Text, "ALPHA") {
		t.Fatalf("read through an inner link = %+v", got)
	}
	if got := r.call("read", map[string]any{"file_path": "two-link"}); got.IsError || got.Text != "g\n" {
		t.Fatalf("read of an inner file link = %+v", got)
	}
	wantExecuted(t, r.call("write", map[string]any{"file_path": "inner/three.txt", "content": "x"}))
	if !fileExists(filepath.Join(r.workspace, "a", "b"), "three.txt") {
		t.Fatal("write through an inner link did not land")
	}
}

// A workspace that is itself a link (or has one in its path, as macOS temp
// directories do) is the root; absolute paths may name it by its real path.
func TestFileToolsWorkspaceBehindALink(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	base, err := filepath.EvalSymlinks(filepath.Dir(r.workspace))
	noErr(t, err)
	real := filepath.Join(base, "real")
	noErr(t, os.MkdirAll(real, 0o755))
	noErr(t, os.WriteFile(filepath.Join(real, "f.txt"), []byte("hi\n"), 0o644))
	linked := filepath.Join(filepath.Dir(r.workspace), "linked")
	noErr(t, os.Symlink(real, linked))
	noErr(t, r.core.openLocal(acpBrain, rigSession, linked))
	if got := r.call("read", map[string]any{"file_path": "f.txt"}); got.IsError || got.Text != "hi\n" {
		t.Fatalf("read = %+v", got)
	}
	if got := r.call("read", map[string]any{"file_path": filepath.Join(real, "f.txt")}); got.IsError || got.Text != "hi\n" {
		t.Fatalf("read by real path = %+v", got)
	}
}

func TestGlobAndGrepDoNotCrossLinksOutOfTheWorkspace(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	noErr(t, os.WriteFile(filepath.Join(r.workspace, "own.txt"), []byte("secret in the workspace\n"), 0o644))
	noErr(t, os.Symlink(r.outside, filepath.Join(r.workspace, "dlink")))
	noErr(t, os.Symlink(filepath.Join(r.outside, "secret.txt"), filepath.Join(r.workspace, "flink.txt")))

	got := r.call("grep", map[string]any{"pattern": "secret"})
	if got.IsError || got.Text != "own.txt:1:secret in the workspace" {
		t.Fatalf("grep = %+v", got)
	}
	for _, pattern := range []string{"dlink/*", "dlink/secret.txt", "*/secret.txt"} {
		if got := r.call("glob", map[string]any{"pattern": pattern}); got.Text != "" {
			t.Fatalf("glob %s = %+v", pattern, got)
		}
	}
	wantToolError(t, r.call("grep", map[string]any{"path": "dlink", "pattern": "secret"}), "grep dlink")
}

// The directory a path goes through is swapped for a link to the outside while
// the tools run: whatever the timing, nothing lands outside.
func TestFileToolsSurviveALinkSwappedIn(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	dir := filepath.Join(r.workspace, "d")
	swap := func(link bool) {
		_ = os.RemoveAll(dir)
		if link {
			_ = os.Symlink(r.outside, dir)
		} else {
			_ = os.MkdirAll(dir, 0o755)
		}
	}
	swap(false)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			swap(i%2 == 1)
		}
	}()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		r.call("write", map[string]any{"file_path": "d/x.txt", "content": "pwned"})
		r.call("edit", map[string]any{"file_path": "d/secret.txt", "old_string": "secret", "new_string": "pwned", "replace_all": true})
		r.call("read", map[string]any{"file_path": "d/secret.txt"})
	}
	close(stop)
	wg.Wait()
	entries, _ := os.ReadDir(r.outside)
	if len(entries) != 1 {
		t.Fatalf("outside directory has %d entries", len(entries))
	}
	if raw, _ := os.ReadFile(filepath.Join(r.outside, "secret.txt")); string(raw) != "secret\n" {
		t.Fatalf("outside file changed: %q", raw)
	}
}

// CoreOptions.FileAccess: the host's say before a file tool runs.

type accessLog struct {
	mu   sync.Mutex
	reqs []FileAccessRequest
	fn   func(FileAccessRequest) error
}

func (l *accessLog) hook(_ context.Context, req FileAccessRequest) error {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	fn := l.fn
	l.mu.Unlock()
	if fn != nil {
		return fn(req)
	}
	return nil
}

func (l *accessLog) setFn(fn func(FileAccessRequest) error) {
	l.mu.Lock()
	l.fn = fn
	l.mu.Unlock()
}

func (l *accessLog) calls() []FileAccessRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]FileAccessRequest(nil), l.reqs...)
}

func readOnlyHook(req FileAccessRequest) error {
	if req.Operation == FileOperationWrite {
		return NewReasonError(CodeForbidden, ReasonWorkspaceReadOnly, "this session is read-only")
	}
	return nil
}

func TestFileAccessHookSeesToolOperationAndRelativePath(t *testing.T) {
	log := &accessLog{}
	r := newFileRig(t, CoreOptions{FileAccess: log.hook}, nil)
	wantExecuted(t, r.call("write", map[string]any{"file_path": filepath.Join(r.workspace, "a", "..", "a", "f.txt"), "content": "one\n"}))
	wantExecuted(t, r.call("read", map[string]any{"file_path": "a/f.txt"}))
	wantExecuted(t, r.call("edit", map[string]any{"file_path": "./a/f.txt", "old_string": "one", "new_string": "two"}))
	wantExecuted(t, r.call("edit", map[string]any{"file_path": "a/f.txt", "old_string": "two", "new_string": "three", "replace_all": true}))
	wantExecuted(t, r.call("glob", map[string]any{"pattern": "a/*"}))
	wantExecuted(t, r.call("grep", map[string]any{"path": "a", "pattern": "three"}))
	// A path outside the workspace fails by itself, without asking.
	wantToolError(t, r.call("read", map[string]any{"file_path": "../outside/secret.txt"}), "read outside")
	want := []FileAccessRequest{
		{Tool: "write", Operation: "write", Path: "a/f.txt"},
		{Tool: "read", Operation: "read", Path: "a/f.txt"},
		{Tool: "edit", Operation: "read", Path: "a/f.txt"},
		{Tool: "edit", Operation: "write", Path: "a/f.txt"},
		{Tool: "edit", Operation: "read", Path: "a/f.txt"},
		{Tool: "edit", Operation: "write", Path: "a/f.txt"},
		{Tool: "glob", Operation: "read", Path: "."},
		{Tool: "grep", Operation: "read", Path: "a"},
	}
	got := log.calls()
	if len(got) != len(want) {
		t.Fatalf("hook calls = %+v", got)
	}
	for i, req := range got {
		if req.SessionID != rigSession || req.AgentSessionID != rigSession || req.ToolCallID == "" ||
			req.Tool != want[i].Tool || req.Operation != want[i].Operation || req.Path != want[i].Path {
			t.Fatalf("hook call %d = %+v, want %+v", i, req, want[i])
		}
	}
}

func TestFileAccessRefusalIsRecordedAndReplayed(t *testing.T) {
	log := &accessLog{fn: func(FileAccessRequest) error {
		return NewReasonError(CodeForbidden, ReasonWorkspaceBoundary, "protected path")
	}}
	r := newFileRig(t, CoreOptions{FileAccess: log.hook}, nil)
	noErr(t, os.WriteFile(filepath.Join(r.workspace, "keep.txt"), []byte("old\n"), 0o644))

	write := map[string]any{"file_path": "new.txt", "content": "x"}
	edit := map[string]any{"file_path": "keep.txt", "old_string": "old", "new_string": "new"}
	editAll := map[string]any{"file_path": "keep.txt", "old_string": "old", "new_string": "new", "replace_all": true}
	for id, call := range map[string]struct {
		tool  string
		input map[string]any
	}{"w1": {"write", write}, "e1": {"edit", edit}, "e2": {"edit", editAll}, "r1": {"read", map[string]any{"file_path": "keep.txt"}}} {
		got := r.callID(id, call.tool, call.input)
		if got.Outcome != RouteOutcomeNotForwarded || got.Code != ReasonWorkspaceBoundary || !got.IsError ||
			got.Text != "workspace_boundary: protected path" {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if fileExists(r.workspace, "new.txt") {
		t.Fatal("the refused write created the file")
	}
	if raw, _ := os.ReadFile(filepath.Join(r.workspace, "keep.txt")); string(raw) != "old\n" {
		t.Fatalf("the refused edit changed the file: %q", raw)
	}

	// The same call id gets the same answer, also once the hook would allow it.
	log.setFn(nil)
	before := len(log.calls())
	got := r.callID("w1", "write", write)
	if !got.Replayed || got.Outcome != RouteOutcomeNotForwarded || got.Code != ReasonWorkspaceBoundary || got.Text != "workspace_boundary: protected path" {
		t.Fatalf("replay = %+v", got)
	}
	if len(log.calls()) != before || fileExists(r.workspace, "new.txt") {
		t.Fatal("the replay asked the hook again or wrote the file")
	}
	// A new call id is asked again and runs.
	wantExecuted(t, r.callID("w2", "write", write))
	if !fileExists(r.workspace, "new.txt") {
		t.Fatal("an allowed write did not run")
	}
}

func TestFileAccessPlainErrorIsPermissionDenied(t *testing.T) {
	log := &accessLog{fn: func(FileAccessRequest) error { return os.ErrPermission }}
	r := newFileRig(t, CoreOptions{FileAccess: log.hook}, nil)
	got := r.call("write", map[string]any{"file_path": "x.txt", "content": "x"})
	if got.Outcome != RouteOutcomeNotForwarded || got.Code != "permission_denied" || !strings.HasPrefix(got.Text, "permission_denied: ") {
		t.Fatalf("got %+v", got)
	}
}

func TestFileAccessReadOnlyHook(t *testing.T) {
	log := &accessLog{fn: readOnlyHook}
	r := newFileRig(t, CoreOptions{FileAccess: log.hook}, nil)
	noErr(t, os.WriteFile(filepath.Join(r.workspace, "f.txt"), []byte("hello\n"), 0o644))
	for _, input := range []struct {
		tool string
		in   map[string]any
	}{
		{"write", map[string]any{"file_path": "g.txt", "content": "x"}},
		{"edit", map[string]any{"file_path": "f.txt", "old_string": "hello", "new_string": "bye"}},
		{"edit", map[string]any{"file_path": "f.txt", "old_string": "hello", "new_string": "bye", "replace_all": true}},
	} {
		wantRefused(t, r.call(input.tool, input.in), ReasonWorkspaceReadOnly)
	}
	if fileExists(r.workspace, "g.txt") {
		t.Fatal("write created a file in a read-only session")
	}
	if got := r.call("read", map[string]any{"file_path": "f.txt"}); got.IsError || got.Text != "hello\n" {
		t.Fatalf("read = %+v", got)
	}
	wantExecuted(t, r.call("glob", map[string]any{"pattern": "*"}))
	if got := r.call("grep", map[string]any{"pattern": "hello"}); got.IsError || got.Text != "f.txt:1:hello" {
		t.Fatalf("grep = %+v", got)
	}
}

// A tool.call goes through the same hook; the route's own pass is not repeated.
func TestFileAccessHookForToolCall(t *testing.T) {
	log := &accessLog{fn: readOnlyHook}
	r := newFileRig(t, CoreOptions{FileAccess: log.hook}, nil)
	noErr(t, os.WriteFile(filepath.Join(r.workspace, "f.txt"), []byte("hello\n"), 0o644))
	call := func(tool, input string) ToolResultParams {
		return r.core.toolCall(context.Background(), acpBrain, ToolCallParams{SessionID: rigSession, ToolCallID: "tc", Tool: tool, Input: []byte(input)})
	}
	got := call("write", `{"path":"g.txt","content":"x"}`)
	if !got.IsError && got.Outcome == ToolOutcomeSucceeded || textOf(got) != "workspace_read_only: this session is read-only" {
		t.Fatalf("write = %+v", got)
	}
	if fileExists(r.workspace, "g.txt") {
		t.Fatal("the refused tool.call write created the file")
	}
	if got := call("read", `{"path":"f.txt"}`); got.IsError || textOf(got) != "hello\n" {
		t.Fatalf("read = %+v", got)
	}
	calls := log.calls()
	if len(calls) != 2 || calls[0].Operation != "write" || calls[1].Operation != "read" || calls[1].Path != "f.txt" {
		t.Fatalf("hook calls = %+v", calls)
	}
}

// Without the option nothing is asked and the tools behave as before.
func TestFileToolsWithoutFileAccessRun(t *testing.T) {
	r := newFileRig(t, CoreOptions{}, nil)
	wantExecuted(t, r.call("write", map[string]any{"file_path": "x.txt", "content": "x"}))
}
