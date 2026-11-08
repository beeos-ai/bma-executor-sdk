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
	outputs   []fakeOutput
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
		_ = json.NewEncoder(w).Encode(manifest)
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
