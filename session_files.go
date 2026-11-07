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
//     reports an ended process uploads its session's outputs at once.
//
// Every operation goes through os.Root, so ".." and symlinks that leave the
// directory fail instead of being followed; only regular files are uploaded.
// The routes are BMA's machine-token daemon routes anchored on the session's
// latest process (/daemon/processes/{id}/session-files,
// .../session-files/{resource_id}/content, .../session-outputs?path=).
const (
	sessionFilesScanInterval = 2 * time.Second
	sessionFilesFlushTimeout = 10 * time.Second
	// sessionFilesPrepareTimeout bounds syncing mounts before a process
	// starts (a 1 GiB mount included).
	sessionFilesPrepareTimeout = 10 * time.Minute
	// sessionFilesIdle forgets a session no process started for this long
	// (its anchor process is no longer accepted by BMA either).
	sessionFilesIdle         = 15 * time.Minute
	maxSessionFileBytes      = 1 << 30
	maxSessionOutputPath     = 512
	maxSessionOutputFiles    = 10000
	maxSessionManifestBytes  = 8 * 1024 * 1024
	maxSessionUploadResponse = 64 * 1024
	sessionUploadsMountRoot  = "/mnt/session/uploads"
	fileDigestHeader         = "X-BMA-File-Digest"
)

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
	OnLog          func(LogEvent)
}

// SessionFiles syncs the session files of an executor that declares
// capabilities.session_files. Pass it as ProcessOptions.SessionFiles (the
// process capability then advertises the root and syncs before each process
// of an agent session) and run Run for the output uploads. It needs a
// machine-scoped executor (ErrMachineTokenRequired).
type SessionFiles struct {
	root           string
	maxOutputBytes int64
	onLog          func(LogEvent)

	mu       sync.Mutex
	sessions map[string]*fileSession
	// started holds the process ids whose start already synced, so a
	// replayed process.start does not sync again.
	started sync.Map
}

type fileSession struct {
	id               string
	uploads, outputs string

	// mu serializes syncing and scanning this session.
	mu sync.Mutex
	// client and anchor (the session's latest process) authorize requests.
	client     *Client
	anchor     string
	lastActive time.Time
	mounts     map[string]mountedFile // by uploads-relative path
	outputFile map[string]*outputFile
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
	SessionID string        `json:"session_id"`
	Files     []sessionFile `json:"files"`
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
	return &SessionFiles{root: root, maxOutputBytes: opts.MaxOutputBytes, onLog: opts.OnLog, sessions: map[string]*fileSession{}}, nil
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
			}
			cancel()
			return
		case <-ticker.C:
			for _, session := range f.active(time.Now().Add(-sessionFilesIdle)) {
				session.scan(ctx, f, false)
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
		session.mu.Unlock()
		if last.Before(cutoff) {
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
		cancel()
	}
	return sessions
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
	session.client, session.anchor, session.lastActive = client, processID, time.Now()
	var manifest sessionFilesManifest
	if err := client.daemonJSON(ctx, "/processes/"+url.PathEscape(processID)+"/session-files", maxSessionManifestBytes, &manifest); err != nil {
		return fmt.Errorf("load session files: %w", err)
	}
	if manifest.SessionID != sessionID {
		return fmt.Errorf("session files belong to %q, not %q", manifest.SessionID, sessionID)
	}
	for _, dir := range []string{session.uploads, session.outputs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := session.syncLocked(ctx, manifest.Files); err != nil {
		return err
	}
	f.started.Store(processID, struct{}{})
	return nil
}

// flush uploads the outputs of sessionID now (a process of it ended).
func (f *SessionFiles) flush(ctx context.Context, sessionID string) {
	f.mu.Lock()
	session := f.sessions[sessionID]
	f.mu.Unlock()
	if session == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionFilesFlushTimeout)
	defer cancel()
	session.scan(ctx, f, true)
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
	if err := s.removeStale(root, want); err != nil {
		return fmt.Errorf("remove unmounted files: %w", err)
	}
	for rel, file := range want {
		if s.mountCurrent(root, rel, file) {
			continue
		}
		delete(s.mounts, rel)
		if err := s.download(ctx, root, rel, file); err != nil {
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

// removeStale deletes every entry of uploads that is not a wanted regular
// file and the directories left empty. WalkDir does not descend symlinks.
func (s *fileSession) removeStale(root *os.Root, want map[string]sessionFile) error {
	var stale, dirs []string
	err := fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case rel == ".":
		case entry.IsDir():
			dirs = append(dirs, rel)
		default:
			if _, ok := want[rel]; !ok || !entry.Type().IsRegular() {
				stale = append(stale, rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, rel := range stale {
		if err := root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(s.mounts, rel)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = root.Remove(dirs[i]) // fails while the directory still has files
	}
	return nil
}

// mountCurrent reports whether rel already holds the mount's content.
func (s *fileSession) mountCurrent(root *os.Root, rel string, file sessionFile) bool {
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() || info.Size() != file.SizeBytes {
		return false
	}
	if known, ok := s.mounts[rel]; ok && known.digest == file.Digest && known.observed == observe(info) {
		return true
	}
	handle, err := root.OpenFile(rel, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return false
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != file.Digest {
		return false
	}
	s.mounts[rel] = mountedFile{digest: file.Digest, observed: observe(info)}
	return true
}

func observe(info fs.FileInfo) fileObservation {
	return fileObservation{size: info.Size(), modTime: info.ModTime()}
}

func (s *fileSession) download(ctx context.Context, root *os.Root, rel string, file sessionFile) error {
	dir := path.Dir(rel)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	temporary := path.Join(dir, fmt.Sprintf(".bma-upload-%d", time.Now().UnixNano()))
	handle, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	committed := false
	defer func() {
		_ = handle.Close()
		if !committed {
			_ = root.Remove(temporary)
		}
	}()
	response, err := s.client.daemonDo(ctx, http.MethodGet,
		"/processes/"+url.PathEscape(s.anchor)+"/session-files/"+url.PathEscape(file.ResourceID)+"/content",
		nil, 0, http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(handle, hash), io.LimitReader(response.Body, file.SizeBytes+1))
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	if digest := fmt.Sprintf("sha256:%x", hash.Sum(nil)); written != file.SizeBytes || digest != file.Digest {
		return fmt.Errorf("download digest mismatch: got %d bytes %s, want %d bytes %s", written, digest, file.SizeBytes, file.Digest)
	}
	if err := handle.Chmod(0o444); err != nil {
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return fmt.Errorf("%s is a directory", rel)
	}
	if err := root.Rename(temporary, rel); err != nil {
		return fmt.Errorf("place file: %w", err)
	}
	committed = true
	return nil
}

// scan uploads changed outputs; force uploads files not yet seen stable.
func (s *fileSession) scan(ctx context.Context, f *SessionFiles, force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return
	}
	root, err := os.OpenRoot(s.outputs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			f.log(LogWarn, "session outputs unavailable: "+s.id, err)
		}
		return
	}
	defer func() { _ = root.Close() }()
	present := map[string]bool{}
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
			s.observeOutput(ctx, f, root, rel, observe(info), force)
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
}

func (s *fileSession) observeOutput(ctx context.Context, f *SessionFiles, root *os.Root, rel string, observed fileObservation, force bool) {
	state := s.outputFile[rel]
	if state == nil {
		state = &outputFile{}
		s.outputFile[rel] = state
	}
	if state.uploaded == observed {
		return
	}
	stable := state.seen == observed
	state.seen = observed
	if !stable && !force {
		return
	}
	switch {
	case len(rel) > maxSessionOutputPath:
		f.log(LogError, fmt.Sprintf("session output %s of %s not uploaded: path exceeds %d bytes", rel, s.id, maxSessionOutputPath), nil)
		state.uploaded = observed
		return
	case observed.size == 0:
		return // an empty file is not a deliverable yet
	case observed.size > f.maxOutputBytes:
		f.log(LogError, fmt.Sprintf("session output %s of %s not uploaded: %d bytes exceed %d", rel, s.id, observed.size, f.maxOutputBytes), nil)
		state.uploaded = observed
		return
	}
	digest, err := s.uploadOutput(ctx, root, rel, observed, state.digest)
	var status *DaemonHTTPError
	switch {
	case errors.As(err, &status) && status.permanent():
		f.log(LogError, fmt.Sprintf("session output %s of %s rejected; retried once it changes", rel, s.id), err)
		state.uploaded = observed
	case err != nil:
		f.log(LogWarn, fmt.Sprintf("session output %s of %s upload failed; retrying next scan", rel, s.id), err)
		state.seen = fileObservation{}
	default:
		state.uploaded, state.digest = observed, digest
	}
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
	response, err := s.client.daemonDo(ctx, http.MethodPost,
		"/processes/"+url.PathEscape(s.anchor)+"/session-outputs?path="+url.QueryEscape(rel),
		io.NewSectionReader(handle, 0, observed.size), observed.size,
		http.Header{fileDigestHeader: {digest}, "Accept": {"application/json"}})
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionUploadResponse))
	if err != nil {
		return "", err
	}
	var result sessionOutputResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("decode session output response: %w", err)
	}
	if result.Digest != digest {
		return "", fmt.Errorf("session output digest mismatch: sent %s, stored %s", digest, result.Digest)
	}
	return digest, nil
}
