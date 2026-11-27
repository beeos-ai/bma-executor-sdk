package bmaexecutor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type fakeMount struct {
	resourceID, mountPath string
	content               []byte
}

// fakeMemoryStore is a memory store a session mounts: files by stored path;
// head overrides what the content route serves (a newer version), vanished
// paths are listed but answer 404 (deleted in between).
type fakeMemoryStore struct {
	id, slug       string
	access         string // "" = read_only
	unmaterialized bool
	files          map[string][]byte
	head           map[string][]byte
	vanished       map[string]bool
}

type fakeOutput struct {
	sessionID, processID, path, digest string
	content                            string
}

// fakeDaemonFiles serves the BMA daemon session file routes anchored on a
// process: the manifest, file content and session-outputs uploads.
type fakeDaemonFiles struct {
	mu        sync.Mutex
	processes map[string]string // process id -> agent session id
	mounts    map[string][]fakeMount
	memory    map[string][]*fakeMemoryStore
	// memoryReads counts memory content downloads by store slug + path.
	memoryReads map[string]int
	outputs     []fakeOutput
	// memoryWrites records the memory writebacks: "PUT|DELETE <store><path> base=<sha>".
	memoryWrites []string
	// failMemoryWrites answers that many memory writebacks 503.
	failMemoryWrites int
	// failUploads answers that many session-outputs uploads 503 (a
	// transient failure) before storing any; -1 fails every one.
	failUploads int
	// ranges are the Range reads of uploads ("<resource>:<first>-<last>");
	// renewals counts session-anchors renewals.
	ranges   []string
	renewals int
}

func digestOf(content []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(content)) }

func (f *fakeDaemonFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mtok" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/daemon/processes/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	processID, route, _ := strings.Cut(rest, "/")
	sessionID, ok := f.processes[processID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodGet && route == "session-files":
		manifest := sessionFilesManifest{SessionID: sessionID, Files: []sessionFile{}}
		for _, m := range f.mounts[sessionID] {
			manifest.Files = append(manifest.Files, sessionFile{
				ResourceID: m.resourceID, FileID: "file_" + m.resourceID, MountPath: m.mountPath,
				SizeBytes: int64(len(m.content)), Digest: digestOf(m.content),
			})
		}
		for _, store := range f.memory[sessionID] {
			access := store.access
			if access == "" {
				access = "read_only"
			}
			item := sessionMemoryStore{MemoryStoreID: store.id, Slug: store.slug, Access: access,
				MountPath: "/mnt/session/" + sessionID + "/memory/" + store.slug, Materialized: !store.unmaterialized,
				Files: []sessionMemoryFile{}}
			if !store.unmaterialized {
				paths := make([]string, 0, len(store.files))
				for p := range store.files {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				for _, p := range paths {
					item.Files = append(item.Files, sessionMemoryFile{MemoryID: "mem_" + p, Path: p,
						SizeBytes: int64(len(store.files[p])), Digest: digestOf(store.files[p])})
				}
			}
			manifest.MemoryStores = append(manifest.MemoryStores, item)
		}
		_ = json.NewEncoder(w).Encode(manifest)
	case r.Method == http.MethodGet && strings.HasPrefix(route, "session-memory/") && strings.HasSuffix(route, "/content"):
		storeID := strings.TrimSuffix(strings.TrimPrefix(route, "session-memory/"), "/content")
		p := r.URL.Query().Get("path")
		for _, store := range f.memory[sessionID] {
			if store.id != storeID || store.unmaterialized || store.vanished[p] {
				continue
			}
			content, ok := store.head[p]
			if !ok {
				content, ok = store.files[p]
			}
			if !ok {
				break
			}
			if f.memoryReads == nil {
				f.memoryReads = map[string]int{}
			}
			f.memoryReads[store.slug+p]++
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Bma-File-Digest", digestOf(content))
			_, _ = w.Write(content)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.HasPrefix(route, "session-memory/") &&
		strings.HasSuffix(route, "/memories"):
		f.memoryWrite(w, r, sessionID, strings.TrimSuffix(strings.TrimPrefix(route, "session-memory/"), "/memories"))
	case r.Method == http.MethodGet && strings.HasPrefix(route, "session-files/") && strings.HasSuffix(route, "/content"):
		resourceID := strings.TrimSuffix(strings.TrimPrefix(route, "session-files/"), "/content")
		for _, m := range f.mounts[sessionID] {
			if m.resourceID != resourceID {
				continue
			}
			if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
				first, last, _ := strings.Cut(spec, "-")
				from, _ := strconv.Atoi(first)
				to := len(m.content) - 1
				if last != "" {
					to, _ = strconv.Atoi(last)
				}
				to = min(to, len(m.content)-1)
				f.ranges = append(f.ranges, fmt.Sprintf("%s:%d-%d", resourceID, from, to))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(m.content)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(m.content[from : to+1])
				return
			}
			_, _ = w.Write(m.content)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && route == "session-anchors":
		f.renewals++
		renewed := fmt.Sprintf("prc_renewed%d", f.renewals)
		f.processes[renewed] = sessionID
		_ = json.NewEncoder(w).Encode(map[string]string{"process_id": renewed})
	case r.Method == http.MethodPost && route == "session-outputs":
		body, _ := io.ReadAll(r.Body)
		if f.failUploads != 0 {
			if f.failUploads > 0 {
				f.failUploads--
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		digest := digestOf(body)
		if r.Header.Get("X-BMA-File-Digest") != digest {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.outputs = append(f.outputs, fakeOutput{sessionID: sessionID, processID: processID,
			path: r.URL.Query().Get("path"), digest: digest, content: string(body)})
		_ = json.NewEncoder(w).Encode(map[string]any{"file_id": "file_out", "digest": digest, "size_bytes": len(body)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// memoryWrite answers a writeback as BMA does (ADR-0110): the store wins a
// stale base, a retry is unchanged, a rule refuses an oversized memory.
func (f *fakeDaemonFiles) memoryWrite(w http.ResponseWriter, r *http.Request, sessionID, storeID string) {
	p, base := r.URL.Query().Get("path"), r.Header.Get("X-BMA-Memory-Base-SHA256")
	body, _ := io.ReadAll(r.Body)
	f.memoryWrites = append(f.memoryWrites, r.Method+" "+storeID+p+" base="+base)
	if f.failMemoryWrites > 0 {
		f.failMemoryWrites--
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var store *fakeMemoryStore
	for _, s := range f.memory[sessionID] {
		if s.id == storeID {
			store = s
		}
	}
	if store == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if store.access != "read_write" {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "memory_read_only", "message": "read-only"})
		return
	}
	bare := func(c []byte) string { return strings.TrimPrefix(digestOf(c), "sha256:") }
	current, found := store.files[p]
	answer := func(status string, withContent bool) {
		out := map[string]any{"status": status, "content_sha256": "", "content": nil}
		if c, ok := store.files[p]; ok {
			out["content_sha256"] = bare(c)
			if withContent {
				out["content"] = string(c)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	if r.Method == http.MethodDelete {
		switch {
		case !found:
			answer("unchanged", false)
		case bare(current) != base:
			answer("conflict", true)
		default:
			delete(store.files, p)
			answer("written", false)
		}
		return
	}
	switch {
	case len(body) > 102400:
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "invalid_field",
			"message": "content must be at most 102400 bytes (100 kB): split it into smaller memories."})
	case found && bare(current) == bare(body):
		answer("unchanged", false)
	case found != (base != "") || found && bare(current) != base:
		answer("conflict", true)
	default:
		store.files[p] = body
		answer("written", false)
	}
}

func (f *fakeDaemonFiles) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.memoryWrites...)
}

func (f *fakeDaemonFiles) uploaded() []fakeOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := append([]fakeOutput(nil), f.outputs...)
	sort.Slice(result, func(i, j int) bool { return result[i].sessionID+result[i].path < result[j].sessionID+result[j].path })
	return result
}

func (f *fakeDaemonFiles) reads(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memoryReads[key]
}

func (f *fakeDaemonFiles) update(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}
