package bmaexecutor

// Skill views by copy: an executor that cannot mount a FUSE file system
// (macOS and Windows local services, ADR-0021 §9) declares
// capabilities.fs.copy_views ["skills"]; the platform then sends it the same
// fs.mount / fs.unmount it sends a FUSE executor, with the session's skills
// view at <session_files.root>/<agent_session_id>/skills and a read-only
// token for bma-file. SkillViews lands that view by copying it: it lists the
// view through bma-file's list op, reads every file through its read op,
// checks each file's size and sha256, writes the tree to a temporary
// directory next to the target and renames it into place, read-only. Skill
// versions are frozen when the session is created, so the copy and a mount
// show the same files.
//
// The token lives in memory only. The platform waits at most 30 seconds for
// fs.mount: a copy that takes longer goes on in the background, and the next
// fs.mount of the same session and path picks it up and answers mounted when
// it is done. A repeated fs.mount (token renewal, reconnect) replaces the
// token and does not copy again; a view already on disk (an executor restart)
// is checked against the listing and kept when every file matches.
// fs.unmount removes the session's skills directory, and the session's
// directory when that leaves it empty. Sessions never touch each other.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SkillViewsOptions configures NewSkillViews.
type SkillViewsOptions struct {
	// Root is the executor's capabilities.session_files.root (absolute). It
	// is advertised as such when nothing else did.
	Root string
	// HTTPClient talks to bma-file (default: a client with a 30 s response
	// header timeout that honours the proxy environment).
	HTTPClient *http.Client
	// MountWait bounds how long fs.mount waits for a copy before it answers
	// failed and lets the copy go on (default 25 s, below the platform's 30 s).
	MountWait time.Duration
	// MaxBytes and MaxFiles bound one session's view (default 256 MiB and
	// 10 000 files).
	MaxBytes int64
	MaxFiles int
	OnLog    func(LogEvent)
}

// SkillViews lands the skills view of each session by copying (see the
// package comment above). Register its Capability with a Core.
type SkillViews struct {
	opts SkillViewsOptions
	root string

	mu   sync.Mutex
	jobs map[string]*skillJob // by session id
}

// skillJob is one session's copy.
type skillJob struct {
	session, path string
	service       string
	token         atomic.Pointer[string]
	cancel        context.CancelFunc
	done          chan struct{}
	err           error // set before done is closed
}

// The view this executor copies.
const skillsView = "skills"

// fs.mount failure codes (the ones bmad answers).
const (
	fsErrorUnavailable = "fs_unavailable"
	fsErrorMountFailed = "mount_failed"
)

// NewSkillViews checks the root and creates it (0700) if needed.
func NewSkillViews(opts SkillViewsOptions) (*SkillViews, error) {
	if !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("bmaexecutor: SkillViewsOptions.Root must be an absolute path, got %q", opts.Root)
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return nil, fmt.Errorf("bmaexecutor: skill views root: %w", err)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}}
	}
	if opts.MountWait <= 0 {
		opts.MountWait = 25 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 256 << 20
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 10000
	}
	return &SkillViews{opts: opts, root: filepath.Clean(opts.Root), jobs: map[string]*skillJob{}}, nil
}

// Capability advertises capabilities.fs.copy_views ["skills"] (and
// session_files.root when unset) and serves fs.mount and fs.unmount.
func (v *SkillViews) Capability() Capability {
	return Capability{
		Advertise: func(c *Capabilities) {
			if c.Fs == nil {
				c.Fs = &FsCapability{}
			}
			if !slices.Contains(c.Fs.CopyViews, skillsView) {
				c.Fs.CopyViews = append(c.Fs.CopyViews, skillsView)
			}
			if c.SessionFiles == nil {
				c.SessionFiles = &SessionFilesCapability{Root: v.root}
			} else if filepath.Clean(c.SessionFiles.Root) != v.root {
				v.log(LogWarn, fmt.Sprintf("session_files.root %s is not the skill views root %s: skills land where the platform cannot expect them", c.SessionFiles.Root, v.root), nil)
			}
		},
		Methods: map[string]Handler{MethodFsMount: v.mount, MethodFsUnmount: v.unmount},
	}
}

// Mounted reports whether the session's skills view is copied (or being
// copied).
func (v *SkillViews) Mounted(sessionID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.jobs[sessionID] != nil
}

// Remove stops the session's copy and removes its skills directory (and the
// session's directory when that leaves it empty). A host that reaps session
// directories calls it first: the view is read-only, so a plain RemoveAll
// of the session directory fails.
func (v *SkillViews) Remove(sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("session id %q is not a safe directory name", sessionID)
	}
	v.mu.Lock()
	job := v.jobs[sessionID]
	delete(v.jobs, sessionID)
	v.mu.Unlock()
	if job != nil {
		job.cancel()
		<-job.done
	}
	return v.removeView(sessionID)
}

func (v *SkillViews) log(level LogLevel, message string, err error) {
	if v.opts.OnLog != nil {
		v.opts.OnLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

func (v *SkillViews) mount(ctx context.Context, req Request) (any, error) {
	var params FsMountParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if !sessionIDPattern.MatchString(params.SessionID) {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: session_id is not a safe directory name"}
	}
	want := filepath.Join(v.root, params.SessionID, skillsView)
	if len(params.Mounts) == 0 {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: no mounts"}
	}
	for _, m := range params.Mounts {
		if m.View != skillsView {
			return FsMountResult{State: "failed", ErrorCode: fsErrorMountFailed, Error: "this executor copies only the skills view, not " + m.View}, nil
		}
		if filepath.Clean(m.Path) != want || m.Path != filepath.Clean(m.Path) {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: the skills view must be at " + want}
		}
		if m.Token == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: no token"}
		}
	}
	service := strings.TrimSuffix(params.ServiceURL, "/")
	if u, err := url.Parse(service); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: service_url is not an http(s) URL"}
	}
	token := params.Mounts[len(params.Mounts)-1].Token

	v.mu.Lock()
	job := v.jobs[params.SessionID]
	if job != nil {
		job.token.Store(&token)
		select {
		case <-job.done:
			if job.err != nil || !v.viewPresent(params.SessionID) {
				job = nil // failed or removed meanwhile: copy again
			}
		default:
		}
	}
	if job == nil {
		job = v.start(params.SessionID, want, service, token)
		v.jobs[params.SessionID] = job
	}
	v.mu.Unlock()

	wait := time.NewTimer(v.opts.MountWait)
	defer wait.Stop()
	select {
	case <-job.done:
		if job.err != nil {
			return failedMount(job.err), nil
		}
		return FsMountResult{State: "mounted"}, nil
	case <-wait.C:
		return FsMountResult{State: "failed", ErrorCode: fsErrorMountFailed, Error: "the skills are still being copied; the next fs.mount picks the copy up"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (v *SkillViews) unmount(_ context.Context, req Request) (any, error) {
	var params FsUnmountParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if !sessionIDPattern.MatchString(params.SessionID) {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: session_id is not a safe directory name"}
	}
	if err := v.Remove(params.SessionID); err != nil {
		return FsUnmountResult{State: "failed", ErrorCode: fsErrorMountFailed, Error: err.Error()}, nil
	}
	return FsUnmountResult{State: "unmounted"}, nil
}

// start runs a session's copy in the background.
func (v *SkillViews) start(session, target, service, token string) *skillJob {
	ctx, cancel := context.WithCancel(context.Background())
	job := &skillJob{session: session, path: target, service: service, cancel: cancel, done: make(chan struct{})}
	job.token.Store(&token)
	go func() {
		defer close(job.done)
		defer cancel()
		started := time.Now()
		files, bytes, err := v.copyView(ctx, job)
		job.err = err
		if err != nil {
			v.log(LogWarn, "skills of session "+session+" were not copied", err)
			return
		}
		v.log(LogInfo, fmt.Sprintf("skills of session %s copied: %d file(s), %d byte(s) in %s", session, files, bytes, time.Since(started).Round(time.Millisecond)), nil)
	}()
	return job
}

// skillFsError is a copy failure with the fs.mount code it answers.
type skillFsError struct {
	code string
	err  error
}

func (e *skillFsError) Error() string { return e.err.Error() }
func (e *skillFsError) Unwrap() error { return e.err }

func failedMount(err error) FsMountResult {
	code := fsErrorMountFailed
	var fe *skillFsError
	if errors.As(err, &fe) {
		code = fe.code
	}
	return FsMountResult{State: "failed", ErrorCode: code, Error: err.Error()}
}

func mountFailed(format string, args ...any) error {
	return &skillFsError{code: fsErrorMountFailed, err: fmt.Errorf(format, args...)}
}

// skillEntry is a node of the view as bma-file lists it.
type skillEntry struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// copyView lists the view, keeps a copy already on disk that matches it, or
// copies it to a temporary directory and renames that into place.
func (v *SkillViews) copyView(ctx context.Context, job *skillJob) (files int, bytes int64, err error) {
	entries, err := v.listAll(ctx, job)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.Type == "file" {
			files++
			bytes += e.Size
		}
	}
	if files > v.opts.MaxFiles || bytes > v.opts.MaxBytes {
		return 0, 0, mountFailed("the skills view has %d file(s) and %d byte(s), more than this executor keeps (%d, %d)", files, bytes, v.opts.MaxFiles, v.opts.MaxBytes)
	}
	session, err := v.openSession(job.session)
	if err != nil {
		return 0, 0, mountFailed("session directory: %v", err)
	}
	defer func() { _ = session.Close() }()
	if matchesView(session, skillsView, entries) {
		return files, bytes, nil
	}
	tmp := ".skills-" + randomHex(8)
	if err := session.Mkdir(tmp, 0o755); err != nil {
		return 0, 0, mountFailed("temporary directory: %v", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = removeReadOnlyTree(session, tmp)
		}
	}()
	for _, e := range entries {
		rel := tmp + e.Path
		switch e.Type {
		case "dir":
			if err := session.Mkdir(rel, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return 0, 0, mountFailed("%s: %v", e.Path, err)
			}
		case "file":
			if err := v.download(ctx, job, session, rel, e); err != nil {
				return 0, 0, err
			}
		}
	}
	// Read-only from the leaves up, then into place.
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "dir" {
			if err := session.Chmod(tmp+entries[i].Path, 0o555); err != nil {
				return 0, 0, mountFailed("%s: %v", entries[i].Path, err)
			}
		}
	}
	if err := session.Chmod(tmp, 0o555); err != nil {
		return 0, 0, mountFailed("chmod: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, mountFailed("the copy was stopped")
	}
	if err := removeReadOnlyTree(session, skillsView); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, 0, mountFailed("previous skills directory: %v", err)
	}
	if err := session.Rename(tmp, skillsView); err != nil {
		return 0, 0, mountFailed("rename: %v", err)
	}
	keep = true
	return files, bytes, nil
}

// listAll lists the view from "/" down, directories before their children.
func (v *SkillViews) listAll(ctx context.Context, job *skillJob) ([]skillEntry, error) {
	var all []skillEntry
	queue := []string{"/"}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		after := ""
		for {
			var out struct {
				Data    []skillEntry `json:"data"`
				HasMore bool         `json:"has_more"`
				LastID  string       `json:"last_id"`
			}
			body, _ := json.Marshal(map[string]any{"view": skillsView, "path": dir, "limit": 1000, "after": after})
			reply, err := v.call(ctx, job, http.MethodPost, "/list", body, "")
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(reply.body, &out); err != nil {
				return nil, mountFailed("list %s: undecodable answer", dir)
			}
			for _, e := range out.Data {
				if err := checkSkillEntry(dir, e); err != nil {
					return nil, err
				}
				all = append(all, e)
				if len(all) > v.opts.MaxFiles*2 {
					return nil, mountFailed("the skills view has more than %d entries", v.opts.MaxFiles*2)
				}
				if e.Type == "dir" {
					queue = append(queue, e.Path)
				}
			}
			if !out.HasMore {
				break
			}
			if out.LastID == "" || out.LastID == after {
				return nil, mountFailed("list %s: no next page", dir)
			}
			after = out.LastID
		}
	}
	return all, nil
}

// checkSkillEntry accepts an entry whose path is a clean child of dir with
// safe names, a known type, a sane size and, for a file, a sha256.
func checkSkillEntry(dir string, e skillEntry) error {
	if e.Path != path.Clean(e.Path) || !strings.HasPrefix(e.Path, "/") || path.Dir(e.Path) != dir || e.Name != path.Base(e.Path) ||
		e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "\\\x00") {
		return mountFailed("list %s: unusable entry %q", dir, e.Path)
	}
	switch e.Type {
	case "dir":
	case "file":
		if e.Size < 0 || !validDigest("sha256:"+e.SHA256) {
			return mountFailed("list %s: entry %q has no size or sha256", dir, e.Path)
		}
	default:
		return mountFailed("list %s: entry %q is neither a file nor a directory", dir, e.Path)
	}
	return nil
}

// download reads a file in ranges (bma-file answers at most 8 MiB at a time),
// checks size and sha256 and writes it read-only.
func (v *SkillViews) download(ctx context.Context, job *skillJob, session *os.Root, rel string, e skillEntry) error {
	f, err := session.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return mountFailed("%s: %v", e.Path, err)
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	var off int64
	for off < e.Size {
		q := url.Values{"view": {skillsView}, "path": {e.Path}}
		reply, err := v.call(ctx, job, http.MethodGet, "/read?"+q.Encode(), nil, "bytes="+strconv.FormatInt(off, 10)+"-")
		if err != nil {
			return err
		}
		if reply.status == http.StatusRequestedRangeNotSatisfiable {
			break
		}
		if got := reply.header.Get("X-Bma-File-Digest"); got != "sha256:"+e.SHA256 {
			return mountFailed("%s: the file changed since it was listed", e.Path)
		}
		if len(reply.body) == 0 {
			break
		}
		if _, err := f.Write(reply.body); err != nil {
			return mountFailed("%s: %v", e.Path, err)
		}
		sum.Write(reply.body)
		off += int64(len(reply.body))
	}
	if off != e.Size || hex.EncodeToString(sum.Sum(nil)) != e.SHA256 {
		return mountFailed("%s: content does not match its size and sha256", e.Path)
	}
	if err := f.Close(); err != nil {
		return mountFailed("%s: %v", e.Path, err)
	}
	mode := fs.FileMode(0o444)
	if e.Mode&0o111 != 0 {
		mode = 0o555
	}
	if err := session.Chmod(rel, mode); err != nil {
		return mountFailed("%s: %v", e.Path, err)
	}
	return nil
}

// skillReply is a bma-file answer read whole.
type skillReply struct {
	status int
	header http.Header
	body   []byte
}

// call sends one bma-file request with the job's current token, retrying
// network errors, 429 and 5xx twice (100 ms, 400 ms); other 4xx are final.
func (v *SkillViews) call(ctx context.Context, job *skillJob, method, op string, body []byte, rng string) (skillReply, error) {
	backoff := []time.Duration{100 * time.Millisecond, 400 * time.Millisecond}
	var last error
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = strings.NewReader(string(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, job.service+op, reader)
		if err != nil {
			return skillReply{}, mountFailed("%s: %v", op, err)
		}
		req.Header.Set("Authorization", "Bearer "+*job.token.Load())
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		reply, status, err := v.once(req)
		if err == nil {
			return reply, nil
		}
		if status != 0 && status != http.StatusTooManyRequests && status < 500 {
			return skillReply{}, mountFailed("bma-file %s: HTTP %d %s", strings.SplitN(op, "?", 2)[0], status, err)
		}
		last = err
		if attempt >= len(backoff) || ctx.Err() != nil {
			return skillReply{}, &skillFsError{code: fsErrorUnavailable, err: fmt.Errorf("bma-file %s: %w", strings.SplitN(op, "?", 2)[0], last)}
		}
		select {
		case <-time.After(backoff[attempt]):
		case <-ctx.Done():
			return skillReply{}, &skillFsError{code: fsErrorUnavailable, err: ctx.Err()}
		}
	}
}

// once sends one request; an HTTP error comes back with its status and the
// error code of its body (never the body itself).
func (v *SkillViews) once(req *http.Request) (skillReply, int, error) {
	resp, err := v.opts.HTTPClient.Do(req)
	if err != nil {
		return skillReply{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 9<<20))
	if err != nil {
		return skillReply{}, 0, err
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return skillReply{}, resp.StatusCode, errors.New(e.Error.Code)
	}
	return skillReply{status: resp.StatusCode, header: resp.Header, body: data}, 0, nil
}

// openSession opens <root>/<session> as a root, creating it (0700) when
// needed; a link in its place is refused.
func (v *SkillViews) openSession(session string) (*os.Root, error) {
	top, err := os.OpenRoot(v.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = top.Close() }()
	if err := top.Mkdir(session, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return openChildRoot(top, session)
}

// viewPresent reports whether the session's skills directory exists.
func (v *SkillViews) viewPresent(session string) bool {
	info, err := os.Lstat(filepath.Join(v.root, session, skillsView))
	return err == nil && info.IsDir()
}

// removeView removes <root>/<session>/skills, and <root>/<session> when that
// leaves it empty.
func (v *SkillViews) removeView(session string) error {
	top, err := os.OpenRoot(v.root)
	if err != nil {
		return err
	}
	defer func() { _ = top.Close() }()
	root, err := openChildRoot(top, session)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = removeReadOnlyTree(root, skillsView)
	_ = root.Close()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = top.Remove(session) // only when empty
	return nil
}

// removeReadOnlyTree makes the directories of name writable again and
// removes it (a link is removed, not followed).
func removeReadOnlyTree(root *os.Root, name string) error {
	if _, err := root.Lstat(name); err != nil {
		return err
	}
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = root.Chmod(p, 0o755)
		}
		return nil
	})
	return root.RemoveAll(name)
}

// matchesView reports whether name holds exactly the listed tree with every
// file's size and sha256.
func matchesView(root *os.Root, name string, entries []skillEntry) bool {
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		return false
	}
	want := map[string]skillEntry{}
	for _, e := range entries {
		want[strings.TrimPrefix(e.Path, "/")] = e
	}
	seen := 0
	ok := true
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !ok {
			ok = false
			return fs.SkipAll
		}
		if p == name {
			return nil
		}
		rel := strings.TrimPrefix(p, name+"/")
		e, listed := want[rel]
		switch {
		case !listed, d.Type()&fs.ModeSymlink != 0, d.IsDir() != (e.Type == "dir"):
			ok = false
			return fs.SkipAll
		case e.Type == "file" && !fileMatches(root, p, e):
			ok = false
			return fs.SkipAll
		}
		seen++
		return nil
	})
	return ok && seen == len(entries)
}

func fileMatches(root *os.Root, name string, e skillEntry) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	return err == nil && n == e.Size && hex.EncodeToString(sum.Sum(nil)) == e.SHA256
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
