package bmaexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBmaFile serves a skills view like bma-file: POST /list (paged), GET
// /read (at most chunk bytes per answer, 206 with Content-Range, 416 past the
// end, X-Bma-File-Digest), bearer tokens.
type fakeBmaFile struct {
	mu      sync.Mutex
	files   map[string][]byte // view path -> content
	exec    map[string]bool
	tokens  map[string]bool
	chunk   int
	page    int
	fail    func(op string) int // a status to answer instead, 0: none
	reads   atomic.Int32
	lists   atomic.Int32
	slow    chan struct{} // reads wait on it when set
	srv     *httptest.Server
	lastTok atomic.Value
}

func newFakeBmaFile(t *testing.T, files map[string]string) *fakeBmaFile {
	f := &fakeBmaFile{files: map[string][]byte{}, exec: map[string]bool{}, tokens: map[string]bool{"tok-1": true, "tok-2": true}, chunk: 5, page: 2}
	for p, c := range files {
		f.files[p] = []byte(c)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBmaFile) url() string { return f.srv.URL + "/v1/machines/mch_1/fs" }

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (f *fakeBmaFile) entries(dir string) []skillEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]skillEntry{}
	for p, c := range f.files {
		if !strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") {
			continue
		}
		rest := strings.TrimPrefix(p, strings.TrimSuffix(dir, "/")+"/")
		name, _, nested := strings.Cut(rest, "/")
		child := path.Join(dir, name)
		if nested {
			seen[name] = skillEntry{Path: child, Name: name, Type: "dir", Mode: 0o555}
			continue
		}
		mode := uint32(0o444)
		if f.exec[p] {
			mode = 0o555
		}
		seen[name] = skillEntry{Path: child, Name: name, Type: "file", Size: int64(len(c)), Mode: mode, SHA256: digest(c)}
	}
	var out []skillEntry
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *fakeBmaFile) serve(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/machines/mch_1/fs/")
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.lastTok.Store(tok)
	f.mu.Lock()
	ok := f.tokens[tok]
	fail := f.fail
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"token_expired","message":"expired"}}`))
		return
	}
	if fail != nil {
		if status := fail(op); status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"boom","message":"no"}}`))
			return
		}
	}
	switch op {
	case "list":
		f.lists.Add(1)
		var req struct {
			View, Path, After string
			Limit             int
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.View != "skills" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		all := f.entries(req.Path)
		start := 0
		for i, e := range all {
			if req.After != "" && e.Name == req.After {
				start = i + 1
			}
		}
		end := min(start+f.page, len(all))
		out := map[string]any{"data": all[start:end], "has_more": end < len(all)}
		if end > start {
			out["last_id"] = all[end-1].Name
		}
		_ = json.NewEncoder(w).Encode(out)
	case "read":
		f.reads.Add(1)
		if f.slow != nil {
			<-f.slow
		}
		p := r.URL.Query().Get("path")
		f.mu.Lock()
		c, ok := f.files[p]
		f.mu.Unlock()
		if !ok || r.URL.Query().Get("view") != "skills" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		off := 0
		if rng := r.Header.Get("Range"); rng != "" {
			off, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
		}
		w.Header().Set("X-Bma-File-Digest", "sha256:"+digest(c))
		if off >= len(c) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		end := min(off+f.chunk, len(c))
		if off != 0 || end != len(c) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, end-1, len(c)))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(c[off:end])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

var testSkills = map[string]string{
	"/cu-finder/SKILL.md":         "---\nname: cu-finder\ndescription: Finder tips\n---\nOpen Finder with open_app.",
	"/cu-finder/scripts/check.sh": "#!/bin/sh\necho ok\n",
	"/cu-lark/SKILL.md":           "---\nname: cu-lark\n---\nPaste non-ASCII text.",
}

func skillViewsFor(t *testing.T, opts SkillViewsOptions) (*SkillViews, string, func(method string, params any) (any, error)) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { chmodWritable(root) })
	opts.Root = root
	v, err := NewSkillViews(opts)
	if err != nil {
		t.Fatal(err)
	}
	c := v.Capability()
	call := func(method string, params any) (any, error) {
		raw, _ := json.Marshal(params)
		return c.Methods[method](context.Background(), Request{Method: method, Params: raw})
	}
	return v, root, call
}

func mountParams(f *fakeBmaFile, root, session, token string) FsMountParams {
	return FsMountParams{SessionID: session, ServiceURL: f.url(),
		Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, session, "skills"), Readonly: true, Token: token}}}
}

func wantMounted(t *testing.T, got any, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := got.(FsMountResult); !ok || r.State != "mounted" {
		t.Fatalf("fs.mount = %+v", got)
	}
}

func TestSkillViewsCopyTheView(t *testing.T) {
	f := newFakeBmaFile(t, testSkills)
	f.exec["/cu-finder/scripts/check.sh"] = true
	v, root, call := skillViewsFor(t, SkillViewsOptions{})
	caps := &Capabilities{}
	v.Capability().Advertise(caps)
	if caps.Fs == nil || caps.Fs.Fuse || len(caps.Fs.CopyViews) != 1 || caps.Fs.CopyViews[0] != "skills" || caps.SessionFiles == nil || caps.SessionFiles.Root != root {
		t.Fatalf("capabilities = %+v %+v", caps.Fs, caps.SessionFiles)
	}
	got, err := call(MethodFsMount, mountParams(f, root, "ses_a", "tok-1"))
	wantMounted(t, got, err)
	dir := filepath.Join(root, "ses_a", "skills")
	for p, want := range testSkills {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", p, b, err)
		}
	}
	for p, mode := range map[string]fs.FileMode{"cu-finder/SKILL.md": 0o444, "cu-finder/scripts/check.sh": 0o555, "cu-finder": 0o555, ".": 0o555} {
		info, _ := os.Stat(filepath.Join(dir, p))
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %v, want %v", p, info.Mode().Perm(), mode)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cu-lark", "SKILL.md"), []byte("x"), 0o644); err == nil {
		t.Error("the view is writable")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "ses_a"))
	if len(entries) != 1 || entries[0].Name() != "skills" {
		t.Fatalf("session directory = %v (a temporary directory was left)", entries)
	}
	if !v.Mounted("ses_a") {
		t.Fatal("not reported mounted")
	}

	// A repeated fs.mount with a renewed token copies nothing again.
	reads, lists := f.reads.Load(), f.lists.Load()
	got, err = call(MethodFsMount, mountParams(f, root, "ses_a", "tok-2"))
	wantMounted(t, got, err)
	if f.reads.Load() != reads || f.lists.Load() != lists {
		t.Fatalf("a repeated fs.mount went to bma-file (reads %d→%d, lists %d→%d)", reads, f.reads.Load(), lists, f.lists.Load())
	}

	// Another session does not touch this one, and fs.unmount removes only its own.
	got, err = call(MethodFsMount, mountParams(f, root, "ses_b", "tok-1"))
	wantMounted(t, got, err)
	_ = os.MkdirAll(filepath.Join(root, "ses_b", "uploads"), 0o700)
	res, err := call(MethodFsUnmount, FsUnmountParams{SessionID: "ses_b", Flush: true})
	if err != nil || res.(FsUnmountResult).State != "unmounted" || res.(FsUnmountResult).PendingBytes != 0 {
		t.Fatalf("fs.unmount = %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "ses_b", "skills")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("skills of ses_b stayed")
	}
	if _, err := os.Stat(filepath.Join(root, "ses_b", "uploads")); err != nil {
		t.Fatal("fs.unmount removed uploads")
	}
	if _, err := os.Stat(filepath.Join(dir, "cu-lark", "SKILL.md")); err != nil {
		t.Fatal("fs.unmount of ses_b removed ses_a's skills")
	}
	// Unmount of ses_a also removes its then empty session directory; a second is a no-op.
	for range 2 {
		if res, err := call(MethodFsUnmount, FsUnmountParams{SessionID: "ses_a"}); err != nil || res.(FsUnmountResult).State != "unmounted" {
			t.Fatalf("fs.unmount = %+v %v", res, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ses_a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the empty session directory stayed")
	}
	if v.Mounted("ses_a") {
		t.Fatal("still reported mounted")
	}
}

// A view already on disk (an executor restart) is kept when it matches the
// listing, and copied again when a file differs.
func TestSkillViewsKeepAMatchingCopy(t *testing.T) {
	f := newFakeBmaFile(t, testSkills)
	_, root, call := skillViewsFor(t, SkillViewsOptions{})
	got, err := call(MethodFsMount, mountParams(f, root, "ses_a", "tok-1"))
	wantMounted(t, got, err)
	// A new executor process: nothing in memory.
	_, _, call2 := func() (*SkillViews, string, func(string, any) (any, error)) {
		v, err := NewSkillViews(SkillViewsOptions{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		c := v.Capability()
		return v, root, func(m string, p any) (any, error) {
			raw, _ := json.Marshal(p)
			return c.Methods[m](context.Background(), Request{Params: raw})
		}
	}()
	reads := f.reads.Load()
	got, err = call2(MethodFsMount, mountParams(f, root, "ses_a", "tok-2"))
	wantMounted(t, got, err)
	if f.reads.Load() != reads {
		t.Fatal("a matching copy was downloaded again")
	}
	f.mu.Lock()
	f.files["/cu-lark/SKILL.md"] = []byte("changed")
	f.mu.Unlock()
	_, _, call3 := func() (*SkillViews, string, func(string, any) (any, error)) {
		v, _ := NewSkillViews(SkillViewsOptions{Root: root})
		c := v.Capability()
		return v, root, func(m string, p any) (any, error) {
			raw, _ := json.Marshal(p)
			return c.Methods[m](context.Background(), Request{Params: raw})
		}
	}()
	got, err = call3(MethodFsMount, mountParams(f, root, "ses_a", "tok-2"))
	wantMounted(t, got, err)
	if b, _ := os.ReadFile(filepath.Join(root, "ses_a", "skills", "cu-lark", "SKILL.md")); string(b) != "changed" {
		t.Fatalf("a differing copy was kept: %q", b)
	}
}

func TestSkillViewsRefusals(t *testing.T) {
	f := newFakeBmaFile(t, testSkills)
	_, root, call := skillViewsFor(t, SkillViewsOptions{})
	for name, p := range map[string]FsMountParams{
		"path elsewhere":  {SessionID: "ses_a", ServiceURL: f.url(), Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, "ses_b", "skills"), Token: "tok-1"}}},
		"path outside":    {SessionID: "ses_a", ServiceURL: f.url(), Mounts: []FsMount{{View: "skills", Path: "/tmp/skills", Token: "tok-1"}}},
		"unsafe session":  {SessionID: "../x", ServiceURL: f.url(), Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, "../x", "skills"), Token: "tok-1"}}},
		"no token":        {SessionID: "ses_a", ServiceURL: f.url(), Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, "ses_a", "skills")}}},
		"bad service url": {SessionID: "ses_a", ServiceURL: "file:///x", Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, "ses_a", "skills"), Token: "tok-1"}}},
		"no mounts":       {SessionID: "ses_a", ServiceURL: f.url()},
		"unclean path":    {SessionID: "ses_a", ServiceURL: f.url(), Mounts: []FsMount{{View: "skills", Path: filepath.Join(root, "ses_a", "skills") + "/", Token: "tok-1"}}},
	} {
		_, err := call(MethodFsMount, p)
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	got, err := call(MethodFsMount, FsMountParams{SessionID: "ses_a", ServiceURL: f.url(), Mounts: []FsMount{{View: "uploads", Path: filepath.Join(root, "ses_a", "uploads"), Token: "tok-1"}}})
	if err != nil || got.(FsMountResult).State != "failed" || got.(FsMountResult).ErrorCode != "mount_failed" {
		t.Fatalf("uploads = %+v %v", got, err)
	}
}

func TestSkillViewsFailures(t *testing.T) {
	for name, c := range map[string]struct {
		token  string
		fail   func(op string) int
		change bool
		code   string
		text   string
	}{
		"expired token": {token: "tok-x", code: "mount_failed", text: "HTTP 401 token_expired"},
		"bma-file down": {token: "tok-1", fail: func(string) int { return 503 }, code: "fs_unavailable"},
		"view not granted": {token: "tok-1", fail: func(op string) int {
			if op == "read" {
				return 403
			}
			return 0
		}, code: "mount_failed", text: "HTTP 403"},
		"changed content": {token: "tok-1", change: true, code: "mount_failed", text: "changed since it was listed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeBmaFile(t, testSkills)
			f.fail = c.fail
			_, root, call := skillViewsFor(t, SkillViewsOptions{})
			if c.change {
				f.fail = func(op string) int {
					if op == "read" {
						f.mu.Lock()
						f.files["/cu-finder/SKILL.md"] = []byte("other")
						f.mu.Unlock()
					}
					return 0
				}
			}
			got, err := call(MethodFsMount, mountParams(f, root, "ses_a", c.token))
			r, ok := got.(FsMountResult)
			if err != nil || !ok || r.State != "failed" || r.ErrorCode != c.code || !strings.Contains(r.Error, c.text) {
				t.Fatalf("fs.mount = %+v %v", got, err)
			}
			if strings.Contains(r.Error, c.token) && c.token != "" {
				t.Fatal("the token is in the error")
			}
			entries, _ := os.ReadDir(filepath.Join(root, "ses_a"))
			if len(entries) != 0 {
				t.Fatalf("a failed copy left %v", entries)
			}
			// The next fs.mount (fixed bma-file, new token) copies again.
			f.fail = nil
			got, err = call(MethodFsMount, mountParams(f, root, "ses_a", "tok-2"))
			wantMounted(t, got, err)
		})
	}
}

// A copy longer than MountWait goes on; the next fs.mount picks it up and
// answers mounted when it is done, with the new token in use.
func TestSkillViewsSlowCopy(t *testing.T) {
	f := newFakeBmaFile(t, testSkills)
	f.slow = make(chan struct{})
	_, root, call := skillViewsFor(t, SkillViewsOptions{MountWait: 50 * time.Millisecond})
	got, err := call(MethodFsMount, mountParams(f, root, "ses_a", "tok-1"))
	if err != nil || got.(FsMountResult).State != "failed" || !strings.Contains(got.(FsMountResult).Error, "still being copied") {
		t.Fatalf("first fs.mount = %+v %v", got, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err = call(MethodFsMount, mountParams(f, root, "ses_a", "tok-2"))
	}()
	time.Sleep(20 * time.Millisecond)
	close(f.slow)
	<-done
	wantMounted(t, got, err)
	if tok, _ := f.lastTok.Load().(string); tok != "tok-2" {
		t.Fatalf("the copy kept the old token: %q", tok)
	}
	if n := f.lists.Load(); n > 4 {
		t.Fatalf("the second fs.mount started another copy (%d lists)", n)
	}
}

// chmodWritable makes the read-only views below root removable for TempDir.
func chmodWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}
