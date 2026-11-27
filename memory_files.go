package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Memory stores of a session (capabilities.session_files.memory, contract
// 2.6.0) are mirrored read-only below <Root>/<agent session id>/memory:
//
//   - <memory>/<slug>/<path> holds each memory of a store the session
//     mounts (path is the memory's stored path, which starts with "/"), 0444
//     in 0755 directories, through os.Root like uploads. The platform wins:
//     anything else below <memory> is deleted, a file whose sha256 differs
//     is downloaded again, and a store the platform did not materialize
//     (over the session's cap) has no directory.
//   - <memory>/.bma-memory.json (0444) lists the mirrored stores
//     ({"stores":[{slug, memory_store_id}]}). It is outside every store
//     directory (slugs have no dot). A store whose entry is missing or
//     names another store, or a missing or damaged marker, is distrusted:
//     its directory is rebuilt from scratch.
//
// The mirror is refreshed with the uploads before every process of the
// session starts and on session_files.sync, which the platform sends before
// each turn of a session with memory (an on-machine brain starts no process,
// and a read tool call is no process either). So a bash change to the copy
// lasts until the next turn or process at most, and an update made through
// the API or another session shows up at the next one. The content route
// serves the memory's current head: when it changed after the manifest was
// listed, the bytes are checked against the digest the route sends and that
// version is written (the store wins).
const (
	sessionMemoryDir    = "memory"
	sessionMemoryMarker = ".bma-memory.json"
	// The session's cap, the platform's: it admits whole stores up to it and
	// marks the rest not materialized. Here it only guards a bad manifest.
	maxSessionMemoryFiles = 10000
	maxSessionMemoryBytes = 64 << 20
	maxMemoryPathSegment  = 255
)

// EnvMemoryDir names <Root>/<agent session id>/memory in the environment of
// the processes of a session that mounts memory stores.
const EnvMemoryDir = "BMA_MEMORY_DIR"

var memorySlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,127}$`)

type sessionMemoryStore struct {
	MemoryStoreID string              `json:"memory_store_id"`
	Slug          string              `json:"slug"`
	Access        string              `json:"access"`
	MountPath     string              `json:"mount_path"`
	Materialized  bool                `json:"materialized"`
	Files         []sessionMemoryFile `json:"files"`
}

type sessionMemoryFile struct {
	MemoryID  string `json:"memory_id"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Digest    string `json:"digest"`
}

// memoryMirror is what the executor wrote for one store: files by stored
// path ("/a/b.md"), each with the digest it wrote (the base of a later
// write-back).
type memoryMirror struct {
	slug, access string
	files        map[string]mountedFile
}

type memoryMarker struct {
	Stores []memoryMarkerStore `json:"stores"`
}

type memoryMarkerStore struct {
	Slug          string `json:"slug"`
	MemoryStoreID string `json:"memory_store_id"`
}

// MemoryDir is <Root>/<sessionID>/memory, where the session's memory stores
// are mirrored (one directory per store slug), for example for a Launch hook
// that lets a sandbox read it.
func (f *SessionFiles) MemoryDir(sessionID string) string {
	return filepath.Join(f.root, sessionID, sessionMemoryDir)
}

// syncMemory refreshes the memory mirror of sessionID with the manifest
// anchored on token (a process anchor of that session), for
// session_files.sync.
func (f *SessionFiles) syncMemory(ctx context.Context, client *Client, sessionID, token string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("agent session id %q is not a safe directory name", sessionID)
	}
	if token == "" {
		return errors.New("session_files.sync needs a download_token")
	}
	ctx, cancel := context.WithTimeout(ctx, sessionFilesPrepareTimeout)
	defer cancel()
	var manifest sessionFilesManifest
	if err := client.daemonJSON(ctx, "/processes/"+url.PathEscape(token)+"/session-files", maxSessionManifestBytes, &manifest); err != nil {
		return fmt.Errorf("load session files: %w", err)
	}
	if manifest.SessionID != sessionID {
		return fmt.Errorf("session files belong to %q, not %q", manifest.SessionID, sessionID)
	}
	session := f.session(sessionID)
	session.mu.Lock()
	defer session.mu.Unlock()
	session.lastActive = time.Now()
	if f.opts.Mounter != nil {
		session.setAnchor(client, token)
		if session.mountedAt.IsZero() {
			// Not mounted yet (no process of the session ran here): the
			// uploads are mounted too, from this manifest.
			if err := f.sessionDirs(session); err != nil {
				return err
			}
			if err := session.mountLocked(ctx, f, manifest, false); err != nil {
				return err
			}
			session.mountedAt = time.Now()
			return nil
		}
		return session.mountLocked(ctx, f, manifest, true)
	}
	if len(manifest.MemoryStores) > 0 || len(session.memory) > 0 {
		if err := f.sessionDirs(session); err != nil {
			return err
		}
	}
	return session.syncMemoryLocked(ctx, client, token, manifest.MemoryStores, f.log)
}

// mirrorsMemory reports whether sessionID mirrors at least one store.
func (f *SessionFiles) mirrorsMemory(sessionID string) bool {
	f.mu.Lock()
	session := f.sessions[sessionID]
	f.mu.Unlock()
	if session == nil {
		return false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.memory) > 0 || session.mountedStores > 0
}

// syncMemoryLocked makes <session>/memory hold exactly stores, downloading
// through the anchor. Without stores and without a directory nothing is
// touched, so a session without memory sees no change.
func (s *fileSession) syncMemoryLocked(ctx context.Context, client *Client, anchor string, stores []sessionMemoryStore,
	log func(LogLevel, string, error)) error {
	sessionDir := filepath.Dir(s.uploads)
	if len(stores) == 0 && len(s.memory) == 0 {
		if _, err := os.Lstat(filepath.Join(sessionDir, sessionMemoryDir)); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	want, skipped, err := memoryPlan(stores)
	if err != nil {
		return err
	}
	for _, name := range skipped {
		log(LogWarn, "memory "+name+" of "+s.id+" not mirrored: the file system cannot hold its name", nil)
	}
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return err
	}
	top, err := os.OpenRoot(sessionDir)
	if err != nil {
		return err
	}
	defer func() { _ = top.Close() }()
	if info, err := top.Lstat(sessionMemoryDir); err == nil && !info.IsDir() {
		if err := removeInRoot(top, sessionMemoryDir); err != nil {
			return err
		}
	}
	if err := top.MkdirAll(sessionMemoryDir, 0o755); err != nil {
		return err
	}
	root, err := openChildRoot(top, sessionMemoryDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	trusted := readMemoryMarker(root)
	bySlug := map[string]sessionMemoryStore{}
	for _, store := range stores {
		if store.Materialized {
			bySlug[store.Slug] = store
		}
	}
	// Top-level entries other than the materialized stores' directories
	// (and the marker) go; so does a distrusted store's directory.
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		store, keep := bySlug[name]
		if name == sessionMemoryMarker || (keep && entry.IsDir() && trusted[name] == store.MemoryStoreID) {
			continue
		}
		if err := removeTreeInRoot(root, name); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	mirrors := map[string]*memoryMirror{}
	for _, store := range stores {
		if !store.Materialized {
			continue
		}
		mirror := s.memory[store.MemoryStoreID]
		if mirror == nil || trusted[store.Slug] != store.MemoryStoreID {
			mirror = &memoryMirror{files: map[string]mountedFile{}}
		}
		mirror.slug, mirror.access = store.Slug, store.Access
		if err := mirrorStore(ctx, client, anchor, root, store, want[store.MemoryStoreID], mirror); err != nil {
			return fmt.Errorf("memory store %s: %w", store.Slug, err)
		}
		mirrors[store.MemoryStoreID] = mirror
	}
	s.memory = mirrors
	if err := writeMemoryMarker(root, stores); err != nil {
		return fmt.Errorf("write %s: %w", sessionMemoryMarker, err)
	}
	return nil
}

// memoryPlan validates stores and returns, per materialized store, its files
// by path relative to the store directory. Files the file system cannot hold
// (a segment longer than it allows, a name that differs from another only in
// case) are left out and logged by the caller's log; they do not fail the sync.
func memoryPlan(stores []sessionMemoryStore) (want map[string]map[string]sessionMemoryFile, skipped []string, err error) {
	want = map[string]map[string]sessionMemoryFile{}
	slugs := map[string]bool{}
	files, total := 0, int64(0)
	for _, store := range stores {
		if !memorySlugPattern.MatchString(store.Slug) || store.MemoryStoreID == "" || slugs[store.Slug] || want[store.MemoryStoreID] != nil {
			return nil, nil, fmt.Errorf("memory store %q has an invalid description", store.Slug)
		}
		slugs[store.Slug] = true
		byRel := map[string]sessionMemoryFile{}
		want[store.MemoryStoreID] = byRel
		if !store.Materialized {
			continue
		}
		folded := map[string]bool{}
		for _, file := range store.Files {
			rel, err := memoryRel(file.Path)
			if err != nil {
				return nil, nil, err
			}
			if !validDigest(file.Digest) || file.SizeBytes < 0 {
				return nil, nil, fmt.Errorf("memory %s has an invalid description", file.Path)
			}
			files, total = files+1, total+file.SizeBytes
			if files > maxSessionMemoryFiles || total > maxSessionMemoryBytes {
				return nil, nil, fmt.Errorf("memory stores exceed %d files or %d bytes", maxSessionMemoryFiles, maxSessionMemoryBytes)
			}
			if longSegment(rel) || folded[strings.ToLower(rel)] {
				skipped = append(skipped, store.Slug+file.Path)
				continue
			}
			folded[strings.ToLower(rel)] = true
			byRel[rel] = file
		}
	}
	return want, skipped, nil
}

// memoryRel maps a stored memory path ("/a/b.md") to a clean relative path.
func memoryRel(p string) (string, error) {
	rel, ok := strings.CutPrefix(p, "/")
	if !ok || rel == "" || strings.ContainsAny(rel, "\x00\\") || path.Clean(rel) != rel ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("memory path %q is not a clean absolute path", p)
	}
	return rel, nil
}

func longSegment(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if len(segment) > maxMemoryPathSegment {
			return true
		}
	}
	return false
}

// mirrorStore makes the store's directory hold exactly files.
func mirrorStore(ctx context.Context, client *Client, anchor string, root *os.Root,
	store sessionMemoryStore, files map[string]sessionMemoryFile, mirror *memoryMirror) error {
	if err := root.MkdirAll(store.Slug, 0o755); err != nil {
		return err
	}
	dir, err := openChildRoot(root, store.Slug)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	known := map[string]mountedFile{} // by relative path
	for p, mount := range mirror.files {
		known[strings.TrimPrefix(p, "/")] = mount
	}
	err = pruneRoot(dir, func(rel string) bool { _, ok := files[rel]; return ok }, func(rel string) { delete(known, rel) })
	if err != nil {
		return err
	}
	mirror.files = map[string]mountedFile{}
	for rel, file := range files {
		if currentIn(dir, rel, file.SizeBytes, file.Digest, known) {
			mirror.files[file.Path] = known[rel]
			continue
		}
		route := "/processes/" + url.PathEscape(anchor) + "/session-memory/" + url.PathEscape(store.MemoryStoreID) +
			"/content?path=" + url.QueryEscape(file.Path)
		if info, err := dir.Lstat(rel); err == nil && info.IsDir() {
			if err := removeTreeInRoot(dir, rel); err != nil {
				return err
			}
		}
		digest, err := fetchInto(ctx, client, dir, rel, route, memoryContentExpect)
		var status *DaemonHTTPError
		switch {
		case errors.As(err, &status) && status.StatusCode == http.StatusNotFound:
			_ = removeInRoot(dir, rel) // deleted since the listing: the store wins
			continue
		case err != nil:
			return fmt.Errorf("materialize %s: %w", file.Path, err)
		}
		info, err := dir.Lstat(rel)
		if err != nil {
			return err
		}
		mirror.files[file.Path] = mountedFile{digest: digest, observed: observe(info)}
	}
	return nil
}

// memoryContentExpect takes the length and digest of the version the content
// route sent (the current head, which may be newer than the manifest).
func memoryContentExpect(response *http.Response) (int64, string, error) {
	digest := response.Header.Get(fileDigestHeader)
	if !validDigest(digest) || response.ContentLength < 0 || response.ContentLength > maxSessionMemoryBytes {
		return 0, "", fmt.Errorf("memory content without a valid %s and Content-Length", fileDigestHeader)
	}
	return response.ContentLength, digest, nil
}

// readMemoryMarker returns the marker's stores, slug to memory store id;
// empty when the marker is missing or damaged (every store distrusted).
func readMemoryMarker(root *os.Root) map[string]string {
	trusted := map[string]string{}
	info, err := root.Lstat(sessionMemoryMarker)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSessionUploadResponse*16 {
		return trusted
	}
	raw, err := root.ReadFile(sessionMemoryMarker)
	if err != nil {
		return trusted
	}
	var marker memoryMarker
	if json.Unmarshal(raw, &marker) != nil {
		return trusted
	}
	for _, store := range marker.Stores {
		trusted[store.Slug] = store.MemoryStoreID
	}
	return trusted
}

func writeMemoryMarker(root *os.Root, stores []sessionMemoryStore) error {
	marker := memoryMarker{Stores: []memoryMarkerStore{}}
	for _, store := range stores {
		if store.Materialized {
			marker.Stores = append(marker.Stores, memoryMarkerStore{Slug: store.Slug, MemoryStoreID: store.MemoryStoreID})
		}
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if current, err := root.ReadFile(sessionMemoryMarker); err == nil && string(current) == string(raw) {
		if info, err := root.Lstat(sessionMemoryMarker); err == nil && info.Mode().IsRegular() {
			return nil
		}
	}
	temporary := fmt.Sprintf(".bma-marker-%d", time.Now().UnixNano())
	if err := root.WriteFile(temporary, raw, 0o444); err != nil {
		return err
	}
	if err := replaceInRoot(root, temporary, sessionMemoryMarker); err != nil {
		_ = removeInRoot(root, temporary)
		return err
	}
	return nil
}

// removeTreeInRoot removes name (a file, link or directory tree) from root
// without following links, clearing read-only attributes on the way.
func removeTreeInRoot(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return removeInRoot(root, name)
	}
	sub, err := openChildRoot(root, name)
	if err != nil {
		return err
	}
	err = pruneRoot(sub, func(string) bool { return false }, func(string) {})
	_ = sub.Close()
	if err != nil {
		return err
	}
	return root.Remove(name)
}
