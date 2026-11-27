package bmaexecutor

import (
	"context"
	"crypto/sha256"
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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session files on an executor that declares capabilities.session_files
// (the reference implementation of BMA's machine executor, bmad, adapted to
// one directory per session so concurrent sessions do not share files):
//
//   - <Root>/<agent session id>/uploads holds the session's mounted files,
//     read-only (0444). Before each process of the session starts, the
//     session's mount manifest is fetched and synced: a file whose sha256
//     already matches is kept, a changed one is replaced, a removed mount and
//     anything else (a symlink, a file the agent added) is deleted. Downloads
//     land in a temporary file and are renamed into place only after the
//     length and sha256 match.
//   - <Root>/<agent session id>/outputs is where the agent writes
//     deliverables. Run walks it every 2 s without following symlinks and
//     uploads each regular file whose size and mtime stayed the same for one
//     interval and changed since its last upload; the process.read that
//     reports an ended process uploads its session's outputs at once,
//     retrying failed uploads for up to sessionFilesFlushTimeout, and lists
//     them with whether each went through (ProcessReadResult.session_outputs).
//
// Every operation goes through os.Root, so ".." and symlinks that leave the
// directory fail instead of being followed; only regular files are uploaded.
// The routes are BMA's machine-token daemon routes anchored on the session's
// latest process (/daemon/processes/{id}/session-files,
// .../session-files/{resource_id}/content, .../session-outputs?path=).
const (
	sessionFilesScanInterval = 2 * time.Second
	// sessionFilesFlushRetry is the pause before a flush retries the uploads
	// that failed.
	sessionFilesFlushRetry = 500 * time.Millisecond
	// sessionFilesPrepareTimeout bounds syncing mounts before a process
	// starts (a 1 GiB mount included).
	sessionFilesPrepareTimeout = 10 * time.Minute
	// sessionFilesIdle forgets a session no process started for this long
	// (its anchor process is no longer accepted by BMA either).
	sessionFilesIdle         = 15 * time.Minute
	maxSessionFileBytes      = 1 << 30
	maxSessionOutputPath     = 512
	maxSessionOutputFiles    = 10000
	maxSessionManifestBytes  = 32 * 1024 * 1024 // 10 000 memories with 1 KiB paths included
	maxSessionUploadResponse = 64 * 1024
	sessionUploadsMountRoot  = "/mnt/session/uploads"
	fileDigestHeader         = "X-BMA-File-Digest"
)

// sessionFilesFlushTimeout bounds one flush of a session's outputs (a
// variable for tests).
var sessionFilesFlushTimeout = 10 * time.Second

// sessionIDPattern is one safe directory name.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// SessionFilesOptions configures NewSessionFiles.
type SessionFilesOptions struct {
	// Root is the absolute, already-expanded directory that holds every
	// session's files; it is advertised as capabilities.session_files.root
	// and BMA shows the agent <Root>/<session id>/{uploads,outputs}. Keep it
	// apart from the executor's own state and IPC directories.
	Root string
	// MaxOutputBytes is the largest output uploaded (default and maximum
	// 1 GiB); larger files are skipped and logged.
	MaxOutputBytes int64
	// RunAs, when set, is the OS identity of an agent session's processes
	// (the one CoreOptions.SessionRunAs gives the session): <Root>/<session>
	// is then the executor's, group RunAs.GID, 0750, so only that identity
	// reaches the session's files, and outputs is RunAs's, 0700; mounts
	// answer only that identity (and root). Unix only.
	RunAs func(sessionID string) (*RunAs, error)
	OnLog func(LogEvent)
	// Mounter, when set, mounts each session's uploads and memory stores
	// instead of copying them (capabilities.session_files.mount, see
	// SessionMounter).
	Mounter SessionMounter
	// MountUnavailable is why the host could not mount (its probe's error),
	// reported with executor.heartbeat; the session files are copied.
	MountUnavailable string
}

// SessionFiles syncs the session files of an executor that declares
// capabilities.session_files. Pass it as ProcessOptions.SessionFiles (the
// process capability then advertises the root and syncs before each process
// of an agent session) and run Run for the output uploads. It needs a
// machine-scoped executor (ErrMachineTokenRequired).
type SessionFiles struct {
	root           string
	maxOutputBytes int64
	runAs          func(sessionID string) (*RunAs, error)
	onLog          func(LogEvent)
	opts           SessionFilesOptions

	mu       sync.Mutex
	sessions map[string]*fileSession
	// running counts the running processes of each agent session: a mounted
	// session with one is never idle.
	running map[string]int
	// started holds the process ids whose start already synced, so a
	// replayed process.start does not sync again.
	started sync.Map
}

type fileSession struct {
	id               string
	uploads, outputs string
	// memoryReports are memory writeback reports not delivered yet.
	memoryReports []SessionMemoryReport
	// memoryClient and memoryAnchor authorize memory writeback: the anchor of
	// the last refresh (a process start, or session_files.sync's
	// download_token).
	memoryClient *Client
	memoryAnchor string

	// mu serializes syncing and scanning this session.
	mu sync.Mutex
	// client and anchor (the session's latest process or session_files.sync
	// download_token) authorize requests.
	// Writers hold mu and anchorMu; a mount's reads take only anchorMu.
	client     *Client
	anchor     string
	anchorMu   sync.Mutex
	anchoredAt time.Time
	lastActive time.Time
	// mountedAt is when the session was first mounted (zero: copied);
	// mountedFiles and mountedStores are what its mounts show.
	mountedAt     time.Time
	mountedFiles  []MountedFile
	mountedStores int
	mounts        map[string]mountedFile // by uploads-relative path
	outputFile    map[string]*outputFile
	memory        map[string]*memoryMirror // by memory store id
}

type mountedFile struct {
	digest   string
	observed fileObservation
}

// outputFile tracks one output path. An observation is uploaded once it is
// seen twice in a row and differs from the last upload (or rejection).
type outputFile struct {
	seen, uploaded fileObservation
	digest         string
}

type fileObservation struct {
	size    int64
	modTime time.Time
}

type sessionFilesManifest struct {
	SessionID    string               `json:"session_id"`
	Files        []sessionFile        `json:"files"`
	MemoryStores []sessionMemoryStore `json:"memory_stores,omitempty"`
}

type sessionFile struct {
	ResourceID string `json:"resource_id"`
	FileID     string `json:"file_id"`
	MountPath  string `json:"mount_path"`
	SizeBytes  int64  `json:"size_bytes"`
	Digest     string `json:"digest"`
}

type sessionOutputResult struct {
	FileID    string `json:"file_id"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

// NewSessionFiles validates the root and creates it (0700) if needed.
func NewSessionFiles(opts SessionFilesOptions) (*SessionFiles, error) {
	if !filepath.IsAbs(opts.Root) {
		return nil, fmt.Errorf("bmaexecutor: SessionFilesOptions.Root must be an absolute, expanded path, got %q", opts.Root)
	}
	if opts.MaxOutputBytes <= 0 || opts.MaxOutputBytes > maxSessionFileBytes {
		opts.MaxOutputBytes = maxSessionFileBytes
	}
	root := filepath.Clean(opts.Root)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("bmaexecutor: create session files root: %w", err)
	}
	return &SessionFiles{root: root, maxOutputBytes: opts.MaxOutputBytes, runAs: opts.RunAs, onLog: opts.OnLog, opts: opts,
		sessions: map[string]*fileSession{}, running: map[string]int{}}, nil
}

// Root is the advertised capabilities.session_files.root.
func (f *SessionFiles) Root() string { return f.root }

// Dirs returns the uploads and outputs directories of an agent session, for
// example for a Launch hook that confines a sandbox to them.
func (f *SessionFiles) Dirs(sessionID string) (uploads, outputs string) {
	dir := filepath.Join(f.root, sessionID)
	return filepath.Join(dir, "uploads"), filepath.Join(dir, "outputs")
}

// Run uploads changed outputs until ctx ends, then uploads what is left.
func (f *SessionFiles) Run(ctx context.Context) {
	ticker := time.NewTicker(sessionFilesScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			for _, session := range f.active(time.Time{}) {
				session.scan(flush, f, true)
				session.writeBackMemory(flush, true, f.log)
				f.unmount(session)
			}
			cancel()
			return
		case <-ticker.C:
			sessions := f.active(time.Now().Add(-sessionFilesIdle))
			for _, session := range sessions {
				session.scan(ctx, f, false)
				session.writeBackMemory(ctx, false, f.log)
			}
			if f.opts.Mounter != nil {
				f.renewAnchors(ctx, sessions)
			}
		}
	}
}

// active returns the known sessions; sessions idle since before cutoff are
// flushed and forgotten.
func (f *SessionFiles) active(cutoff time.Time) []*fileSession {
	f.mu.Lock()
	sessions := make([]*fileSession, 0, len(f.sessions))
	var idle []*fileSession
	for id, session := range f.sessions {
		session.mu.Lock()
		last := session.lastActive
		busy := f.running[id] > 0 || (session.client != nil && slices.Contains(session.client.ActiveSessions(), id))
		if busy && !session.mountedAt.IsZero() {
			session.lastActive = time.Now() // a mounted session in use stays mounted and renewed
		}
		session.mu.Unlock()
		if last.Before(cutoff) && !(busy && !session.mountedAt.IsZero()) {
			delete(f.sessions, id)
			idle = append(idle, session)
			continue
		}
		sessions = append(sessions, session)
	}
	f.mu.Unlock()
	for _, session := range idle {
		flush, cancel := context.WithTimeout(context.Background(), sessionFilesFlushTimeout)
		session.scan(flush, f, true)
		session.writeBackMemory(flush, true, f.log)
		cancel()
		f.unmount(session)
		session.mu.Lock()
		if err := session.parkMemoryLocked(); err != nil {
			f.log(LogWarn, "memory bases of "+session.id+" not parked", err)
		}
		session.mu.Unlock()
	}
	return sessions
}

// unmount detaches a forgotten or closing session's mounts.
func (f *SessionFiles) unmount(session *fileSession) {
	if f.opts.Mounter == nil || session.mountedAt.IsZero() {
		return
	}
	if err := f.opts.Mounter.Unmount(session.id); err != nil {
		f.log(LogWarn, "unmount the session files of "+session.id, err)
	}
}

// processStarted and processEnded count the running processes of an agent
// session.
func (f *SessionFiles) processStarted(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[sessionID]++
}

func (f *SessionFiles) processEnded(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running[sessionID]--; f.running[sessionID] <= 0 {
		delete(f.running, sessionID)
	}
}

func (f *SessionFiles) session(id string) *fileSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	session, ok := f.sessions[id]
	if !ok {
		uploads, outputs := f.Dirs(id)
		session = &fileSession{id: id, uploads: uploads, outputs: outputs,
			mounts: map[string]mountedFile{}, outputFile: map[string]*outputFile{}}
		f.sessions[id] = session
	}
	return session
}

func (f *SessionFiles) log(level LogLevel, message string, err error) {
	if f.onLog != nil {
		f.onLog(LogEvent{Level: level, Message: message, Err: err})
	}
}

// prepare shows sessionID's files before processID starts. A replayed start
// does not sync again. A failure fails the start: the agent would otherwise
// run without the files it was given.
func (f *SessionFiles) prepare(ctx context.Context, client *Client, sessionID, processID string) error {
	if _, replayed := f.started.Load(processID); replayed {
		return nil
	}
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("agent session id %q is not a safe directory name", sessionID)
	}
	ctx, cancel := context.WithTimeout(ctx, sessionFilesPrepareTimeout)
	defer cancel()
	session := f.session(sessionID)
	session.mu.Lock()
	defer session.mu.Unlock()
	session.setAnchor(client, processID)
	session.lastActive = time.Now()
	if err := session.unparkMemoryLocked(); err != nil {
		f.log(LogWarn, "parked memory bases of "+sessionID+" not read", err)
	}
	session.writeBackMemoryLocked(ctx, client, processID, true, f.log) // before the refresh lists the manifest
	var manifest sessionFilesManifest
	if err := client.daemonJSON(ctx, "/processes/"+url.PathEscape(processID)+"/session-files", maxSessionManifestBytes, &manifest); err != nil {
		return fmt.Errorf("load session files: %w", err)
	}
	if manifest.SessionID != sessionID {
		return fmt.Errorf("session files belong to %q, not %q", manifest.SessionID, sessionID)
	}
	if err := f.sessionDirs(session); err != nil {
		return err
	}
	if f.opts.Mounter != nil {
		if err := session.mountLocked(ctx, f, manifest, false); err != nil {
			return err
		}
		if session.mountedAt.IsZero() {
			session.mountedAt = time.Now()
		}
		f.started.Store(processID, struct{}{})
		return nil
	}
	if err := session.syncLocked(ctx, manifest.Files); err != nil {
		return err
	}
	owner, err := f.memoryOwner(sessionID)
	if err != nil {
		return err
	}
	if err := session.syncMemoryLocked(ctx, client, processID, manifest.MemoryStores, owner, f.log); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	f.started.Store(processID, struct{}{})
	return nil
}

// sessionDirs creates the session's directory with its uploads and outputs
// and, with SessionFilesOptions.RunAs, gives them to the session's identity
// (again on every call, so a changed mode or owner does not last): the
// session directory stays the executor's (it writes into it, so a link put in
// its place must not be followed) with the session's group, 0750; outputs is
// the session's, 0700. Links in place of either are refused.
func (f *SessionFiles) sessionDirs(s *fileSession) error {
	for _, dir := range []string{s.uploads, s.outputs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if f.runAs == nil {
		return nil
	}
	runAs, err := f.runAs(s.id)
	if err != nil || runAs == nil {
		return err
	}
	sessionDir := filepath.Dir(s.uploads)
	for _, dir := range []string{sessionDir, s.outputs} {
		if info, err := os.Lstat(dir); err != nil {
			return err
		} else if !info.IsDir() {
			return fmt.Errorf("session files: %s is not a directory", filepath.Base(dir))
		}
	}
	if err := os.Lchown(sessionDir, os.Geteuid(), int(runAs.GID)); err != nil {
		return fmt.Errorf("session files: hand the session directory to the session's group: %w", err)
	}
	if err := os.Chmod(sessionDir, 0o750); err != nil {
		return err
	}
	if err := os.Lchown(s.outputs, int(runAs.UID), int(runAs.GID)); err != nil {
		return fmt.Errorf("session files: hand outputs to the session's user: %w", err)
	}
	return os.Chmod(s.outputs, 0o700)
}

// flushMemory writes back sessionID's memory now (a process of it ended) and
// returns the reports not delivered yet.
func (f *SessionFiles) flushMemory(ctx context.Context, sessionID string) []SessionMemoryReport {
	f.mu.Lock()
	session := f.sessions[sessionID]
	f.mu.Unlock()
	if session == nil {
		return nil
	}
	session.writeBackMemory(ctx, true, f.log)
	return session.takeMemoryReports()
}

// memoryReports returns sessionID's undelivered memory writeback reports.
func (f *SessionFiles) memoryReports(sessionID string) []SessionMemoryReport {
	f.mu.Lock()
	session := f.sessions[sessionID]
	f.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.takeMemoryReports()
}

// flush uploads the outputs of sessionID now (a process of it ended) and
// reports every output it found changed since its last upload: its digest on
// the machine and whether the upload went through. A failed upload is retried
// until it succeeds or sessionFilesFlushTimeout passes; what is still
// pending then is reported uploaded=false (the periodic scan keeps trying).
func (f *SessionFiles) flush(ctx context.Context, sessionID string) []SessionOutputReport {
	f.mu.Lock()
	session := f.sessions[sessionID]
	f.mu.Unlock()
	if session == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionFilesFlushTimeout)
	defer cancel()
	reports := map[string]outputAttempt{}
	var order []string
	for {
		for _, a := range session.scan(ctx, f, true) {
			if _, seen := reports[a.rel]; !seen {
				order = append(order, a.rel)
			}
			reports[a.rel] = a
		}
		if !session.pending(reports) {
			break
		}
		retry := time.NewTimer(sessionFilesFlushRetry)
		select {
		case <-ctx.Done():
			retry.Stop()
		case <-retry.C:
			continue
		}
		break
	}
	out := make([]SessionOutputReport, 0, len(order))
	for _, rel := range order {
		if a := reports[rel]; a.digest != "" {
			out = append(out, SessionOutputReport{Path: rel, Digest: a.digest, Uploaded: a.done})
		}
	}
	return out
}

// outputAttempt is one output a scan tried to upload: digest is the content
// it read ("" when the file changed while it was read), done that the
// platform stored it or refused it for good.
type outputAttempt struct {
	rel, digest string
	done        bool
}

// pending reports whether an attempt still waits for its upload, settling
// those the periodic scan uploaded meanwhile and dropping those whose file
// is gone.
func (s *fileSession) pending(reports map[string]outputAttempt) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting := false
	for rel, a := range reports {
		if a.done {
			continue
		}
		state := s.outputFile[rel]
		switch {
		case state == nil:
			continue // deleted: nothing left to upload
		case a.digest != "" && state.digest == a.digest:
			a.done = true
			reports[rel] = a
		default:
			waiting = true
		}
	}
	return waiting
}

// uploadsPath maps a server mount path to a clean path relative to uploads.
func uploadsPath(mountPath string) (string, error) {
	rel, ok := strings.CutPrefix(mountPath, sessionUploadsMountRoot+"/")
	if !ok || rel == "" || strings.ContainsRune(rel, 0) || path.Clean(rel) != rel ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("mount path %q is not below %s", mountPath, sessionUploadsMountRoot)
	}
	return rel, nil
}

func validDigest(digest string) bool {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 2*sha256.Size {
		return false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// syncLocked makes uploads hold exactly the manifest's files.
func (s *fileSession) syncLocked(ctx context.Context, files []sessionFile) error {
	want := make(map[string]sessionFile, len(files))
	for _, file := range files {
		rel, err := uploadsPath(file.MountPath)
		if err != nil {
			return err
		}
		if !validDigest(file.Digest) || file.SizeBytes < 0 || file.SizeBytes > maxSessionFileBytes || file.ResourceID == "" {
			return fmt.Errorf("session file %s has an invalid description", file.MountPath)
		}
		want[rel] = file
	}
	root, err := os.OpenRoot(s.uploads)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	err = pruneRoot(root, func(rel string) bool { _, ok := want[rel]; return ok }, func(rel string) { delete(s.mounts, rel) })
	if err != nil {
		return fmt.Errorf("remove unmounted files: %w", err)
	}
	for rel, file := range want {
		if currentIn(root, rel, file.SizeBytes, file.Digest, s.mounts) {
			continue
		}
		delete(s.mounts, rel)
		route := "/processes/" + url.PathEscape(s.anchor) + "/session-files/" + url.PathEscape(file.ResourceID) + "/content"
		if _, err := fetchInto(ctx, s.client, root, rel, route, func(*http.Response) (int64, string, error) {
			return file.SizeBytes, file.Digest, nil
		}); err != nil {
			return fmt.Errorf("materialize %s: %w", file.MountPath, err)
		}
		info, err := root.Lstat(rel)
		if err != nil {
			return err
		}
		s.mounts[rel] = mountedFile{digest: file.Digest, observed: observe(info)}
	}
	return nil
}

// pruneRoot deletes every entry of root that is not a regular file keep
// accepts, then the directories left empty; removed is told each deleted
// file. WalkDir does not descend symlinks.
func pruneRoot(root *os.Root, keep func(rel string) bool, removed func(rel string)) error {
	var stale, dirs []string
	err := fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case rel == ".":
		case entry.IsDir():
			dirs = append(dirs, rel)
		default:
			if !entry.Type().IsRegular() || !keep(rel) {
				stale = append(stale, rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, rel := range stale {
		if err := removeInRoot(root, rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed(rel)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Remove(dirs[i]) // fails while the directory still has files
	}
	return nil
}

// currentIn reports whether rel in root already holds size bytes of digest,
// recording a match in known.
func currentIn(root *os.Root, rel string, size int64, digest string, known map[string]mountedFile) bool {
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}
	if mount, ok := known[rel]; ok && mount.digest == digest && mount.observed == observe(info) {
		return true
	}
	handle, err := root.OpenFile(rel, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return false
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != digest {
		return false
	}
	known[rel] = mountedFile{digest: digest, observed: observe(info)}
	return true
}

func observe(info fs.FileInfo) fileObservation {
	return fileObservation{size: info.Size(), modTime: info.ModTime()}
}

// fetchInto downloads route into rel of root (0444): into a temporary file, renamed
// into place only once its length and sha256 are what expect says of the
// response. It returns the digest written.
func fetchInto(ctx context.Context, client *Client, root *os.Root, rel, route string,
	expect func(*http.Response) (size int64, digest string, err error)) (string, error) {
	dir := path.Dir(rel)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create parent directory: %w", err)
	}
	temporary := path.Join(dir, fmt.Sprintf(".bma-upload-%d", time.Now().UnixNano()))
	handle, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create temporary file: %w", err)
	}
	committed := false
	defer func() {
		_ = handle.Close()
		if !committed {
			_ = root.Remove(temporary)
		}
	}()
	response, err := client.daemonDo(ctx, http.MethodGet, route, nil, 0, http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	size, want, err := expect(response)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(handle, hash), io.LimitReader(response.Body, size+1))
	if err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	if digest := fmt.Sprintf("sha256:%x", hash.Sum(nil)); written != size || digest != want {
		return "", fmt.Errorf("download digest mismatch: got %d bytes %s, want %d bytes %s", written, digest, size, want)
	}
	if err := handle.Chmod(0o444); err != nil {
		return "", err
	}
	if err := handle.Close(); err != nil {
		return "", err
	}
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}
	if err := replaceInRoot(root, temporary, rel); err != nil {
		return "", fmt.Errorf("place file: %w", err)
	}
	committed = true
	return want, nil
}

// scan uploads changed outputs; force uploads files not yet seen stable. It
// returns the outputs it tried to upload.
func (s *fileSession) scan(ctx context.Context, f *SessionFiles, force bool) []outputAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil
	}
	root, err := os.OpenRoot(s.outputs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			f.log(LogWarn, "session outputs unavailable: "+s.id, err)
		}
		return nil
	}
	defer func() { _ = root.Close() }()
	present := map[string]bool{}
	var attempts []outputAttempt
	// fs.WalkDir reports symlinks as entries and does not descend them. A
	// vanished or unreadable entry is skipped and retried next scan.
	walkErr := fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || !entry.Type().IsRegular() {
			return nil //nolint:nilerr // see above
		}
		if len(present) >= maxSessionOutputFiles {
			return fs.SkipAll
		}
		if info, err := entry.Info(); err == nil {
			present[rel] = true
			if a, tried := s.observeOutput(ctx, f, root, rel, observe(info), force); tried {
				attempts = append(attempts, a)
			}
		}
		return nil
	})
	if walkErr != nil {
		f.log(LogWarn, "scan session outputs: "+s.id, walkErr)
	}
	for rel := range s.outputFile {
		if !present[rel] {
			delete(s.outputFile, rel)
		}
	}
	return attempts
}

// observeOutput uploads rel when it changed (and, without force, stayed the
// same since the previous scan); tried reports an upload attempt.
func (s *fileSession) observeOutput(ctx context.Context, f *SessionFiles, root *os.Root, rel string,
	observed fileObservation, force bool) (attempt outputAttempt, tried bool) {
	state := s.outputFile[rel]
	if state == nil {
		state = &outputFile{}
		s.outputFile[rel] = state
	}
	if state.uploaded == observed {
		return outputAttempt{}, false
	}
	stable := state.seen == observed
	state.seen = observed
	if !stable && !force {
		return outputAttempt{}, false
	}
	switch {
	case len(rel) > maxSessionOutputPath:
		f.log(LogError, fmt.Sprintf("session output %s of %s not uploaded: path exceeds %d bytes", rel, s.id, maxSessionOutputPath), nil)
		state.uploaded = observed
		return outputAttempt{}, false
	case observed.size == 0:
		return outputAttempt{}, false // an empty file is not a deliverable yet
	case observed.size > f.maxOutputBytes:
		f.log(LogError, fmt.Sprintf("session output %s of %s not uploaded: %d bytes exceed %d", rel, s.id, observed.size, f.maxOutputBytes), nil)
		state.uploaded = observed
		return outputAttempt{}, false
	}
	digest, err := s.uploadOutput(ctx, root, rel, observed, state.digest)
	attempt = outputAttempt{rel: rel, digest: digest}
	var status *DaemonHTTPError
	switch {
	case errors.As(err, &status) && status.permanent():
		f.log(LogError, fmt.Sprintf("session output %s of %s rejected; retried once it changes", rel, s.id), err)
		state.uploaded = observed
		attempt.done = true
	case err != nil:
		f.log(LogWarn, fmt.Sprintf("session output %s of %s upload failed; retrying", rel, s.id), err)
		state.seen = fileObservation{}
	default:
		state.uploaded, state.digest = observed, digest
		attempt.done = true
	}
	return attempt, true
}

func (s *fileSession) uploadOutput(ctx context.Context, root *os.Root, rel string, observed fileObservation, previousDigest string) (string, error) {
	handle, err := root.OpenFile(rel, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != observed.size || !info.ModTime().Equal(observed.modTime) {
		return "", errors.New("file changed while uploading")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(handle, 0, observed.size)); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("sha256:%x", hash.Sum(nil))
	if digest == previousDigest {
		return digest, nil // touched, not changed
	}
	// From here on the digest is the content read, whatever the upload's fate.
	response, err := s.client.daemonDo(ctx, http.MethodPost,
		"/processes/"+url.PathEscape(s.anchor)+"/session-outputs?path="+url.QueryEscape(rel),
		io.NewSectionReader(handle, 0, observed.size), observed.size,
		http.Header{fileDigestHeader: {digest}, "Accept": {"application/json"}})
	if err != nil {
		return digest, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionUploadResponse))
	if err != nil {
		return digest, err
	}
	var result sessionOutputResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return digest, fmt.Errorf("decode session output response: %w", err)
	}
	if result.Digest != digest {
		return digest, fmt.Errorf("session output digest mismatch: sent %s, stored %s", digest, result.Digest)
	}
	return digest, nil
}
