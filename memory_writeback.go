package bmaexecutor

import (
	"bytes"
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
	"strings"
	"time"
)

// Memory writeback (capabilities.session_files.memory_writeback, ADR-0110):
// what the agent creates, changes or deletes in the directory of a
// read_write memory store is written back to the store, path by path,
// against the digest the mirror wrote there (the base):
//
//   - The 2 s walk sends a file once its size and mtime were the same in two
//     walks; the end of a process of the session (process.read) and every
//     mirror refresh (process start, session_files.sync) send what changed
//     at once, before the refresh, so a refresh never drops an unsent change.
//   - A file whose base is gone is deleted from the store once it is absent
//     in two walks (one when forced). More than memoryBulkDeleteLimit
//     deletions of a store in one pass are refused: none is sent and the next
//     refresh restores the files.
//   - The store wins a conflict (the base no longer is the store's version,
//     or a create finds the path taken): the store's version replaces the
//     file, or the file goes when the store deleted it.
//   - A memory rule that refuses a file (size, count, path, archived) leaves
//     it on the machine and it is not sent again until it changes.
//   - A store directory that disappeared, or whose marker entry is missing,
//     is not written back from (it would look like "delete everything"); the
//     next refresh rebuilds it.
//   - A session the walk forgets after sessionFilesIdle parks its bases
//     (memory_park.go); a write after that goes back with the next refresh.
//     A pass that sends something keeps the session on the walk.
//
// Every outcome that is not "written" is queued as a SessionMemoryReport and
// delivered once: with the next process end (ProcessReadResult.
// session_memory) or session_files.sync result. Writeback never fails a
// process, a sync or a turn: what cannot reach the platform is retried by
// the next walk.
const (
	memoryBaseHeader      = "X-BMA-Memory-Base-SHA256"
	memoryBulkDeleteLimit = 50
	maxMemoryContentBytes = 102400
	maxMemoryWriteAnswer  = 2*maxMemoryContentBytes + 4096
	memoryAccessReadWrite = "read_write"
	memoryTemporaryPrefix = ".bma-upload-"
)

// memoryWriteAnswer is the platform's answer to one writeback.
type memoryWriteAnswer struct {
	Status        string  `json:"status"` // written | unchanged | conflict
	ContentSHA256 string  `json:"content_sha256"`
	Content       *string `json:"content"`
}

// memoryWriteState is the writeback's own state of one mirrored store.
type memoryWriteState struct {
	seen     map[string]fileObservation // a change seen once, by stored path
	absent   map[string]bool            // a base file seen missing once
	rejected map[string]string          // stored path -> digest a rule refused
	refused  bool                       // a bulk delete was reported since the last refresh
	reset    bool                       // a distrusted directory was reported since the last refresh
}

func newMemoryWriteState() *memoryWriteState {
	return &memoryWriteState{seen: map[string]fileObservation{}, absent: map[string]bool{}, rejected: map[string]string{}}
}

// writeBackMemory runs one writeback pass of the session (the walk, or a
// forced one at the end of a process), bounded by sessionFilesFlushTimeout.
func (s *fileSession) writeBackMemory(ctx context.Context, force bool, log func(LogLevel, string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeBackMemoryLocked(ctx, s.memoryClient, s.memoryAnchor, force, log)
}

// takeMemoryReports returns the queued reports and forgets them.
func (s *fileSession) takeMemoryReports() []SessionMemoryReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.memoryReports
	s.memoryReports = nil
	return out
}

func (s *fileSession) writeBackMemoryLocked(ctx context.Context, client *Client, anchor string, force bool,
	log func(LogLevel, string, error)) {
	if client == nil || anchor == "" || !s.writesMemory() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionFilesFlushTimeout)
	defer cancel()
	root, err := os.OpenRoot(filepath.Join(filepath.Dir(s.uploads), sessionMemoryDir))
	if err != nil {
		s.distrustAll()
		return
	}
	defer func() { _ = root.Close() }()
	trusted := readMemoryMarker(root)
	for id, mirror := range s.memory {
		if mirror.access != memoryAccessReadWrite || ctx.Err() != nil {
			continue
		}
		if mirror.write == nil {
			mirror.write = newMemoryWriteState()
		}
		info, err := root.Lstat(mirror.slug)
		if trusted[mirror.slug] != id || err != nil || !info.IsDir() {
			s.distrust(id, mirror)
			continue
		}
		dir, err := openChildRoot(root, mirror.slug)
		if err != nil {
			s.distrust(id, mirror)
			continue
		}
		w := storeWriter{session: s, client: client, anchor: anchor, id: id, mirror: mirror, dir: dir, log: log}
		w.pass(ctx, force)
		_ = dir.Close()
	}
}

func (s *fileSession) writesMemory() bool {
	for _, mirror := range s.memory {
		if mirror.access == memoryAccessReadWrite {
			return true
		}
	}
	return false
}

func (s *fileSession) distrustAll() {
	for id, mirror := range s.memory {
		if mirror.access == memoryAccessReadWrite {
			s.distrust(id, mirror)
		}
	}
}

func (s *fileSession) distrust(id string, mirror *memoryMirror) {
	if mirror.write == nil {
		mirror.write = newMemoryWriteState()
	}
	if mirror.write.reset {
		return
	}
	mirror.write.reset = true
	s.report(id, "", "reset", "The directory of this memory store was missing or damaged, so nothing in it was "+
		"written back. It is restored from the store at the next turn; re-apply changes made since the last sync.")
}

func (s *fileSession) report(storeID, memoryPath, outcome, message string) {
	s.memoryReports = append(s.memoryReports,
		SessionMemoryReport{MemoryStoreID: storeID, Path: memoryPath, Outcome: outcome, Message: message})
}

// storeWriter is one writeback pass over one store's directory.
type storeWriter struct {
	session *fileSession
	client  *Client
	anchor  string
	id      string
	mirror  *memoryMirror
	dir     *os.Root
	log     func(LogLevel, string, error)
}

func (w storeWriter) pass(ctx context.Context, force bool) {
	local := map[string]fileObservation{} // by stored path
	_ = fs.WalkDir(w.dir.FS(), ".", func(rel string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || strings.HasPrefix(path.Base(rel), memoryTemporaryPrefix) {
			return nil //nolint:nilerr // a vanished entry is seen again next pass
		}
		if info, err := entry.Info(); err == nil {
			local["/"+rel] = observe(info)
		}
		return nil
	})
	for p, observed := range local {
		if ctx.Err() != nil {
			return
		}
		w.changed(ctx, p, observed, force)
	}
	w.deleted(ctx, local, force)
}

// changed sends p when it differs from its base and has settled.
func (w storeWriter) changed(ctx context.Context, p string, observed fileObservation, force bool) {
	state := w.mirror.write
	delete(state.absent, p)
	known, tracked := w.mirror.files[p]
	if tracked && known.observed == observed {
		delete(state.seen, p)
		return
	}
	if !force && state.seen[p] != observed {
		state.seen[p] = observed
		return
	}
	delete(state.seen, p)
	content, digest, err := readMemoryFile(w.dir, strings.TrimPrefix(p, "/"), observed)
	if err != nil {
		return // changed while read: next pass
	}
	if tracked && digest == known.digest {
		w.mirror.files[p] = mountedFile{digest: digest, observed: observed}
		return
	}
	if state.rejected[p] == digest {
		return
	}
	base := ""
	if tracked {
		base = strings.TrimPrefix(known.digest, "sha256:")
	}
	answer, err := w.send(ctx, http.MethodPut, p, base, content)
	if w.refusal(p, digest, err) {
		return
	}
	switch answer.Status {
	case "written", "unchanged":
		w.mirror.files[p] = mountedFile{digest: digest, observed: observed}
		delete(state.rejected, p)
	case "conflict":
		w.storeWins(p, answer)
		if answer.Content == nil {
			w.session.report(w.id, p, "conflict", p+" was deleted from the store since this session read it, so "+
				"your change was not kept. Write it again if it is still needed.")
			return
		}
		w.session.report(w.id, p, "conflict", p+" changed in the store since this session read it (another "+
			"session or the API wrote it first); the store's version replaced your copy. Read it again and "+
			"re-apply your change if it is still needed.")
	}
}

// deleted sends the deletion of base files that are gone, unless too many.
func (w storeWriter) deleted(ctx context.Context, local map[string]fileObservation, force bool) {
	state := w.mirror.write
	var gone []string
	for p := range w.mirror.files {
		if _, ok := local[p]; ok {
			continue
		}
		if state.absent[p] || force {
			gone = append(gone, p)
		}
		state.absent[p] = true
	}
	if len(gone) > memoryBulkDeleteLimit {
		if !state.refused {
			state.refused = true
			w.session.report(w.id, "", "delete_refused", fmt.Sprintf("%d memories of this store were removed at "+
				"once; a sync never deletes more than %d. Nothing was deleted and the files are restored at the "+
				"next turn. Delete memories a few at a time, or through the API.", len(gone), memoryBulkDeleteLimit))
		}
		return
	}
	for _, p := range gone {
		if ctx.Err() != nil {
			return
		}
		answer, err := w.send(ctx, http.MethodDelete, p, strings.TrimPrefix(w.mirror.files[p].digest, "sha256:"), nil)
		if w.refusal(p, "", err) {
			continue
		}
		delete(state.absent, p)
		if answer.Status != "conflict" {
			delete(w.mirror.files, p)
			continue
		}
		w.storeWins(p, answer)
		w.session.report(w.id, p, "conflict", p+" changed in the store since this session read it, so it was "+
			"not deleted; the store's version was restored.")
	}
}

// refusal handles a failed send: a rule's refusal is reported once per
// content; anything else (offline, 5xx, the anchor gone) is retried next
// pass. It reports whether the send failed.
func (w storeWriter) refusal(p, digest string, err error) bool {
	if err == nil {
		return false
	}
	var status *DaemonHTTPError
	if !errors.As(err, &status) || status.StatusCode >= 500 || status.StatusCode == http.StatusNotFound ||
		status.StatusCode == http.StatusTooManyRequests {
		w.log(LogWarn, "memory writeback of "+w.session.id+p+" will be retried", err)
		return true
	}
	if digest != "" {
		w.mirror.write.rejected[p] = digest
	}
	w.session.report(w.id, p, "rejected", status.Message)
	return true
}

func (w storeWriter) send(ctx context.Context, method, p, base string, content []byte) (memoryWriteAnswer, error) {
	answer, err := sendMemoryWrite(ctx, w.client, w.anchor, w.id, method, p, base, content)
	if err == nil {
		w.session.lastActive = time.Now() // an agent still writing keeps its session on the walk
	}
	return answer, err
}

// sendMemoryWrite writes (PUT) or deletes (DELETE) the memory at p of a store
// through anchor, against base (the bare sha256 the writer started from,
// empty for a new memory). A refusal is a *DaemonHTTPError.
func sendMemoryWrite(ctx context.Context, client *Client, anchor, storeID, method, p, base string,
	content []byte) (memoryWriteAnswer, error) {
	route := "/processes/" + url.PathEscape(anchor) + "/session-memory/" + url.PathEscape(storeID) +
		"/memories?path=" + url.QueryEscape(p)
	header := http.Header{memoryBaseHeader: {base}, "Accept": {"application/json"}}
	var body io.Reader
	if method == http.MethodPut {
		body = bytes.NewReader(content)
	}
	response, err := client.daemonDo(ctx, method, route, body, int64(len(content)), header)
	if err != nil {
		return memoryWriteAnswer{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var answer memoryWriteAnswer
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxMemoryWriteAnswer))
	if err == nil {
		err = json.Unmarshal(raw, &answer)
	}
	return answer, err
}

// storeWins puts the store's version of p in place (or removes p when the
// store has none) and makes it the base.
func (w storeWriter) storeWins(p string, answer memoryWriteAnswer) {
	rel := strings.TrimPrefix(p, "/")
	delete(w.mirror.write.rejected, p)
	if answer.Content == nil {
		_ = removeInRoot(w.dir, rel)
		delete(w.mirror.files, p)
		return
	}
	if err := writeMemoryFile(w.dir, rel, []byte(*answer.Content)); err != nil {
		w.log(LogWarn, "memory conflict of "+w.session.id+p+": the store's version could not be written", err)
		delete(w.mirror.files, p) // the next refresh downloads it
		return
	}
	if err := ownInRoot(w.dir, rel, w.mirror.owner); err != nil {
		w.log(LogWarn, "memory conflict of "+w.session.id+p, err)
	}
	if info, err := w.dir.Lstat(rel); err == nil {
		w.mirror.files[p] = mountedFile{digest: "sha256:" + answer.ContentSHA256, observed: observe(info)}
	}
}

// readMemoryFile reads a settled memory file (one byte past the memory
// limit at most: the platform's rule refuses it) and its digest.
func readMemoryFile(root *os.Root, rel string, observed fileObservation) ([]byte, string, error) {
	handle, err := root.OpenFile(rel, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || observe(info) != observed {
		return nil, "", errors.New("memory file changed while read")
	}
	limit := min(observed.size, maxMemoryContentBytes+1)
	content := make([]byte, limit)
	if _, err := io.ReadFull(handle, content); err != nil {
		return nil, "", err
	}
	if observed.size > limit { // hash what the store would hold: the whole file
		hash := sha256.New()
		_, _ = hash.Write(content)
		if _, err := io.Copy(hash, handle); err != nil {
			return nil, "", err
		}
		return content, fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
	}
	return content, fmt.Sprintf("sha256:%x", sha256.Sum256(content)), nil
}

// writeMemoryFile replaces rel with content (0644) through a temporary file.
func writeMemoryFile(root *os.Root, rel string, content []byte) error {
	if err := root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(rel), fmt.Sprintf("%s%d", memoryTemporaryPrefix, time.Now().UnixNano()))
	if err := root.WriteFile(temporary, content, 0o644); err != nil {
		return err
	}
	if err := replaceInRoot(root, temporary, rel); err != nil {
		_ = removeInRoot(root, temporary)
		return err
	}
	return nil
}
