package bmaexecutor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	// failUploads answers that many session-outputs uploads 503 (a
	// transient failure) before storing any; -1 fails every one.
	failUploads int
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
			item := sessionMemoryStore{MemoryStoreID: store.id, Slug: store.slug, Access: "read_write",
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
	case r.Method == http.MethodGet && strings.HasPrefix(route, "session-files/") && strings.HasSuffix(route, "/content"):
		resourceID := strings.TrimSuffix(strings.TrimPrefix(route, "session-files/"), "/content")
		for _, m := range f.mounts[sessionID] {
			if m.resourceID == resourceID {
				_, _ = w.Write(m.content)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
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
