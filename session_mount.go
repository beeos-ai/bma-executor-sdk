package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Session files mounted instead of copied (capabilities.session_files.mount,
// contract 2.7.0, ADR-0111). With SessionFilesOptions.Mounter the executor
// places a session's uploads and memory stores as file systems at the paths
// the copy would use: <Root>/<session>/uploads (read-only) and one mount per
// memory store <Root>/<session>/memory/<slug> (read-only for read_only
// stores; a read_write store is written through at close, ADR-0111 item 7,
// with the outcomes queued like the copy's writeback reports).
// <Root>/<session>/memory is a 0555 directory with the marker and the mount
// points. The mounts follow the manifest at the copy's points (before
// every process of the session, session_files.sync); outputs stay a scanned
// local directory. A mount that fails fails the start or the sync: there is
// no copy fallback. While a session is active the executor renews its anchor
// every sessionAnchorRenewal so a long turn or a background process keeps
// reading; a session idle for sessionFilesIdle is unmounted.
const sessionAnchorRenewal = 10 * time.Minute

// SessionMounter mounts sessions' files (sessionmount.Mounter on Linux).
type SessionMounter interface {
	// Mount makes m visible: it mounts what is not mounted yet, replaces the
	// listings of what is, and detaches stores m no longer has. It returns
	// once every mount answers.
	Mount(ctx context.Context, m SessionMount) error
	// Unmount detaches every mount of the session.
	Unmount(sessionID string) error
	// Stats are the cumulative counters reported with executor.heartbeat.
	Stats() SessionFilesStats
}

// SessionMount is what one session shows.
type SessionMount struct {
	SessionID string
	// Uploads and Memory are <Root>/<session>/uploads and <Root>/<session>/memory.
	Uploads, Memory string
	// Owner is the OS user the session's processes run as: the mount roots
	// are its (0700) and only it (and root) is answered.
	Owner MountOwner
	Files []MountedFile
	// Stores are the materialized memory stores, in resource order.
	Stores []MountedStore
	// Source reads content through the session's current anchor.
	Source SessionSource
}

// MountOwner is a uid and gid.
type MountOwner struct{ UID, GID uint32 }

// MountedFile is one file of a mount: its path relative to the mount root,
// size and sha256 ("sha256:<hex>"), and Key, what Source needs to read it
// (the resource id of an upload, the stored path of a memory).
type MountedFile struct {
	Path, Key, Digest string
	Size              int64
}

// MountedStore is one memory store.
type MountedStore struct {
	Slug, StoreID string
	// ReadOnly: the store is read_only; otherwise writes go through
	// SessionSource.WriteMemory and DeleteMemory.
	ReadOnly bool
	Files    []MountedFile
}

// SessionSource reads a session's content from the platform.
type SessionSource interface {
	// Upload reads length bytes (< 0: to the end) of the upload resource
	// from offset.
	Upload(ctx context.Context, resourceID string, offset, length int64) (io.ReadCloser, error)
	// Memory reads the current content of a memory of a store and answers
	// its digest and size (it may be newer than the listing).
	Memory(ctx context.Context, storeID, storePath string) (body io.ReadCloser, digest string, size int64, err error)
	// WriteMemory stores content at storePath of a read_write store against
	// base (the digest "sha256:<hex>" the writer started from, "" for a new
	// memory); DeleteMemory deletes it against base. A refusal by a memory
	// rule is a *DaemonHTTPError; any other error means nothing was saved.
	WriteMemory(ctx context.Context, storeID, storePath, base string, content []byte) (MemoryWrite, error)
	DeleteMemory(ctx context.Context, storeID, storePath, base string) (MemoryWrite, error)
	// Report queues a writeback outcome for the model (delivered with the
	// next process.read or session_files.sync result).
	Report(SessionMemoryReport)
}

// MemoryWrite is the store's answer to a write or delete: Status written,
// unchanged or conflict; on a conflict Content is the store's version (nil
// when the store has none) and Digest its "sha256:<hex>"; otherwise Digest is
// what the store now holds at the path ("" after a delete).
type MemoryWrite struct {
	Status, Digest string
	Content        *string
}

// anchoredSource reads through a session's latest anchor.
type anchoredSource struct{ s *fileSession }

func (a anchoredSource) current() (*Client, string) {
	a.s.anchorMu.Lock()
	defer a.s.anchorMu.Unlock()
	return a.s.client, a.s.anchor
}

func (a anchoredSource) Upload(ctx context.Context, resourceID string, offset, length int64) (io.ReadCloser, error) {
	client, anchor := a.current()
	header := http.Header{"Accept": {"application/octet-stream"}}
	if offset > 0 || length >= 0 {
		last := ""
		if length >= 0 {
			last = strconv.FormatInt(offset+length-1, 10)
		}
		header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-"+last)
	}
	response, err := client.daemonDo(ctx, http.MethodGet, "/processes/"+url.PathEscape(anchor)+"/session-files/"+
		url.PathEscape(resourceID)+"/content", nil, 0, header)
	if err != nil {
		return nil, err
	}
	if (offset > 0 || length >= 0) && response.StatusCode != http.StatusPartialContent {
		_ = response.Body.Close()
		return nil, fmt.Errorf("session file range answered %s", response.Status)
	}
	return response.Body, nil
}

func (a anchoredSource) WriteMemory(ctx context.Context, storeID, storePath, base string, content []byte) (MemoryWrite, error) {
	return a.write(ctx, http.MethodPut, storeID, storePath, base, content)
}

func (a anchoredSource) DeleteMemory(ctx context.Context, storeID, storePath, base string) (MemoryWrite, error) {
	return a.write(ctx, http.MethodDelete, storeID, storePath, base, nil)
}

func (a anchoredSource) write(ctx context.Context, method, storeID, storePath, base string, content []byte) (MemoryWrite, error) {
	client, anchor := a.current()
	answer, err := sendMemoryWrite(ctx, client, anchor, storeID, method, storePath, strings.TrimPrefix(base, "sha256:"), content)
	if err != nil {
		return MemoryWrite{}, err
	}
	w := MemoryWrite{Status: answer.Status, Content: answer.Content}
	if answer.ContentSHA256 != "" {
		w.Digest = "sha256:" + answer.ContentSHA256
	}
	return w, nil
}

func (a anchoredSource) Report(r SessionMemoryReport) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.s.memoryReports = append(a.s.memoryReports, r)
}

func (a anchoredSource) Memory(ctx context.Context, storeID, storePath string) (io.ReadCloser, string, int64, error) {
	client, anchor := a.current()
	response, err := client.daemonDo(ctx, http.MethodGet, "/processes/"+url.PathEscape(anchor)+"/session-memory/"+
		url.PathEscape(storeID)+"/content?path="+url.QueryEscape(storePath), nil, 0,
		http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, "", 0, err
	}
	size, digest, err := memoryContentExpect(response)
	if err != nil {
		_ = response.Body.Close()
		return nil, "", 0, err
	}
	return response.Body, digest, size, nil
}

// mountLocked shows the manifest through the mounter; memoryOnly keeps the
// uploads listing (session_files.sync touches memory only).
func (s *fileSession) mountLocked(ctx context.Context, f *SessionFiles, manifest sessionFilesManifest, memoryOnly bool) error {
	owner, err := f.owner(s.id)
	if err != nil {
		return err
	}
	m := SessionMount{SessionID: s.id, Uploads: s.uploads, Memory: filepath.Join(filepath.Dir(s.uploads), sessionMemoryDir),
		Owner: owner, Source: anchoredSource{s}}
	if memoryOnly {
		m.Files = s.mountedFiles
	} else {
		files := make([]MountedFile, 0, len(manifest.Files))
		for _, file := range manifest.Files {
			rel, err := uploadsPath(file.MountPath)
			if err != nil {
				return err
			}
			if !validDigest(file.Digest) || file.SizeBytes < 0 || file.SizeBytes > maxSessionFileBytes || file.ResourceID == "" {
				return fmt.Errorf("session file %s has an invalid description", file.MountPath)
			}
			files = append(files, MountedFile{Path: rel, Key: file.ResourceID, Digest: file.Digest, Size: file.SizeBytes})
		}
		m.Files = files
	}
	want, skipped, err := memoryPlan(manifest.MemoryStores)
	if err != nil {
		return err
	}
	for _, name := range skipped {
		f.log(LogWarn, "memory "+name+" of "+s.id+" not mounted: the file system cannot hold its name", nil)
	}
	for _, store := range manifest.MemoryStores {
		if !store.Materialized {
			continue
		}
		mounted := MountedStore{Slug: store.Slug, StoreID: store.MemoryStoreID, ReadOnly: store.Access != memoryAccessReadWrite}
		for rel, file := range want[store.MemoryStoreID] {
			mounted.Files = append(mounted.Files, MountedFile{Path: rel, Key: file.Path, Digest: file.Digest, Size: file.SizeBytes})
		}
		m.Stores = append(m.Stores, mounted)
	}
	if err := prepareMemoryDir(m.Memory, manifest.MemoryStores); err != nil {
		return fmt.Errorf("memory directory: %w", err)
	}
	if err := f.opts.Mounter.Mount(ctx, m); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	s.mountedFiles, s.mountedStores = m.Files, len(m.Stores)
	return nil
}

// prepareMemoryDir makes <session>/memory a root-owned 0555 directory with
// the marker when the session has memory stores; without stores and without
// the directory nothing is touched.
func prepareMemoryDir(dir string, stores []sessionMemoryStore) error {
	if len(stores) == 0 {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil { // the marker is replaced through a temporary file
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := writeMemoryMarker(root, stores); err != nil {
		return err
	}
	return os.Chmod(dir, 0o555)
}

// owner is the OS user sessionID's processes run as: SessionFilesOptions.RunAs's
// identity, or the executor's own.
func (f *SessionFiles) owner(sessionID string) (MountOwner, error) {
	own := MountOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())} //nolint:gosec // ids fit in uint32
	if f.runAs == nil {
		return own, nil
	}
	runAs, err := f.runAs(sessionID)
	if err != nil || runAs == nil {
		return own, err
	}
	return MountOwner{UID: runAs.UID, GID: runAs.GID}, nil
}

// renewAnchors renews the anchor of every mounted session whose anchor is
// older than sessionAnchorRenewal. A refused renewal (the anchor expired)
// keeps the anchor; the session's next process or sync brings a fresh one.
func (f *SessionFiles) renewAnchors(ctx context.Context, sessions []*fileSession) {
	for _, s := range sessions {
		s.mu.Lock()
		client, anchor, due := s.client, s.anchor, !s.mountedAt.IsZero() && time.Since(s.anchoredAt) >= sessionAnchorRenewal
		s.mu.Unlock()
		if !due || client == nil {
			continue
		}
		renewCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var answer struct {
			ProcessID string `json:"process_id"`
		}
		err := client.daemonPostJSON(renewCtx, "/processes/"+url.PathEscape(anchor)+"/session-anchors", &answer)
		cancel()
		if err != nil || !sessionIDPattern.MatchString(answer.ProcessID) {
			f.log(LogWarn, "renew the session files anchor of "+s.id, err)
			continue
		}
		s.mu.Lock()
		if s.anchor == anchor { // no process or sync brought a newer one meanwhile
			s.setAnchor(client, answer.ProcessID)
		}
		s.mu.Unlock()
	}
}

// setAnchor records the anchor content requests name from now on.
func (s *fileSession) setAnchor(client *Client, anchor string) {
	s.anchorMu.Lock()
	defer s.anchorMu.Unlock()
	s.client, s.anchor, s.anchoredAt = client, anchor, time.Now()
}

// Stats are the mounter's counters, or the probe's error.
func (f *SessionFiles) Stats() *SessionFilesStats {
	switch {
	case f.opts.Mounter != nil:
		stats := f.opts.Mounter.Stats()
		return &stats
	case f.opts.MountUnavailable != "":
		return &SessionFilesStats{MountUnavailable: f.opts.MountUnavailable}
	}
	return nil
}
