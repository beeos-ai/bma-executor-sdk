// Package sessionmount mounts a session's uploads and memory stores as file
// systems for bmaexecutor.SessionFiles (capabilities.session_files.mount,
// ADR-0111). It runs inside a root executor on Linux (go-fuse, direct
// mount(2), no fusermount): every mount is answered by this process, reads
// content through the session's anchor (bmaexecutor.SessionSource) and caches
// it by sha256. On other systems New fails and the executor copies.
package sessionmount

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// ErrUnsupported: session files are mounted on Linux only; elsewhere New
// and Probe answer it and the executor copies.
var ErrUnsupported = errors.New("sessionmount: mounting session files needs Linux")

// FSName is the file system type of the mounts ("fuse." + FSName in
// /proc/mounts).
const FSName = "bma-session"

// Request deadlines (ADR-0111 item 8): nothing waits on the platform longer.
const (
	listTimeout   = 10 * time.Second
	memoryTimeout = 15 * time.Second
)

// memoryCacheBytes bounds the memories kept in this process (a session copies
// at most 64 MiB of memory).
const memoryCacheBytes = 256 << 20

// Options configures New.
type Options struct {
	// CacheDir holds the uploads content cache (root-only, emptied by New).
	CacheDir string
	// CacheBytes bounds it (DefaultCacheBytes when 0).
	CacheBytes int64
}

// counters are the cumulative SessionFilesStats.
type counters struct {
	mounts, mountFailures, readsCached, readsFetched, fetchErrors atomic.Int64
	mountNanos                                                    atomic.Int64
}

func (c *counters) stats() bmaexecutor.SessionFilesStats {
	return bmaexecutor.SessionFilesStats{Mounts: c.mounts.Load(), MountFailures: c.mountFailures.Load(),
		MountSeconds: time.Duration(c.mountNanos.Load()).Seconds(), ReadsCached: c.readsCached.Load(),
		ReadsFetched: c.readsFetched.Load(), FetchErrors: c.fetchErrors.Load()}
}

// tree is one mount's listing: files by path relative to the mount root and
// the names in each directory ("" is the root).
type tree struct {
	files map[string]bmaexecutor.MountedFile
	dirs  map[string][]string
}

func newTree(files []bmaexecutor.MountedFile) (*tree, error) {
	t := &tree{files: map[string]bmaexecutor.MountedFile{}, dirs: map[string][]string{"": nil}}
	names := map[string]map[string]bool{"": {}}
	for _, f := range files {
		if f.Path == "" || path.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, "/") || f.Path == ".." ||
			strings.HasPrefix(f.Path, "../") {
			return nil, fmt.Errorf("sessionmount: %q is not a clean relative path", f.Path)
		}
		t.files[f.Path] = f
		for p := f.Path; p != "."; p = path.Dir(p) {
			dir := path.Dir(p)
			if dir == "." {
				dir = ""
			}
			if names[dir] == nil {
				names[dir] = map[string]bool{}
			}
			names[dir][path.Base(p)] = true
		}
	}
	for dir, set := range names {
		if _, isFile := t.files[dir]; isFile && dir != "" {
			return nil, fmt.Errorf("sessionmount: %q is both a file and a directory", dir)
		}
		list := make([]string, 0, len(set))
		for name := range set {
			list = append(list, name)
		}
		sort.Strings(list)
		t.dirs[dir] = list
	}
	return t, nil
}

// view is one mount: a session's uploads or one memory store.
type view struct {
	m        *Mounter
	dir      string // the mount point
	store    string // memory store id; "" for uploads
	readOnly bool
	mounted  time.Time

	mu     sync.RWMutex
	tree   *tree
	owner  bmaexecutor.MountOwner
	source bmaexecutor.SessionSource
}

func (v *view) snapshot() (*tree, bmaexecutor.MountOwner, bmaexecutor.SessionSource) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.tree, v.owner, v.source
}

func (v *view) replace(t *tree, owner bmaexecutor.MountOwner, source bmaexecutor.SessionSource) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tree, v.owner, v.source = t, owner, source
}

// allowed reports whether uid may use the view: its session's user or root.
func (v *view) allowed(uid uint32) bool {
	_, owner, _ := v.snapshot()
	return uid == 0 || uid == owner.UID
}

// memoryCache keeps memories (at most 100 kB each) in this process by digest.
type memoryCache struct {
	mu    sync.Mutex
	items map[string][]byte
	order []string
	bytes int
}

func (c *memoryCache) get(digest string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.items[digest]
	return b, ok
}

func (c *memoryCache) put(digest string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[digest]; ok {
		return
	}
	c.items[digest] = b
	c.order = append(c.order, digest)
	c.bytes += len(b)
	for c.bytes > memoryCacheBytes && len(c.order) > 1 {
		c.bytes -= len(c.items[c.order[0]])
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
}

// memory answers a memory's bytes: from this process when its digest is
// known, else the store's current content, checked against the digest the
// route sent. It answers the digest of what it returns (newer than the
// listing when the store changed since).
func (v *view) memory(ctx context.Context, f bmaexecutor.MountedFile) ([]byte, string, error) {
	if b, ok := v.m.memories.get(f.Digest); ok {
		v.m.stats.readsCached.Add(1)
		return b, f.Digest, nil
	}
	v.m.stats.readsFetched.Add(1)
	_, _, source := v.snapshot()
	ctx, cancel := context.WithTimeout(ctx, memoryTimeout)
	defer cancel()
	body, digest, size, err := source.Memory(ctx, v.store, f.Key)
	if err != nil {
		v.m.stats.fetchErrors.Add(1)
		return nil, "", err
	}
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		v.m.stats.fetchErrors.Add(1)
		return nil, "", err
	}
	if sum := sha256.Sum256(b); int64(len(b)) != size || "sha256:"+hex.EncodeToString(sum[:]) != digest {
		v.m.stats.fetchErrors.Add(1)
		return nil, "", ErrDigest
	}
	v.m.memories.put(digest, b)
	return b, digest, nil
}

// upload references a mounted upload's content in the disk cache.
func (v *view) upload(f bmaexecutor.MountedFile) (*object, error) {
	_, _, source := v.snapshot()
	key := f.Key
	o, complete, err := v.m.cache.acquire(f.Digest, f.Size, func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return source.Upload(ctx, key, offset, length)
	})
	if err != nil {
		return nil, err
	}
	if complete {
		v.m.stats.readsCached.Add(1)
	} else {
		v.m.stats.readsFetched.Add(1)
	}
	return o, nil
}

// Mounter keeps the mounts of every session of this executor.
type Mounter struct {
	cache    *cache
	memories *memoryCache
	stats    *counters

	mu       sync.Mutex
	sessions map[string]map[string]*mounted // session -> mount point -> mount
}

var _ bmaexecutor.SessionMounter = (*Mounter)(nil)

// Stats implements bmaexecutor.SessionMounter.
func (m *Mounter) Stats() bmaexecutor.SessionFilesStats { return m.stats.stats() }

// Mount implements bmaexecutor.SessionMounter.
func (m *Mounter) Mount(_ context.Context, s bmaexecutor.SessionMount) error {
	want := map[string]wantMount{}
	t, err := newTree(s.Files)
	if err != nil {
		return err
	}
	want[s.Uploads] = wantMount{tree: t, readOnly: true}
	for _, store := range s.Stores {
		t, err := newTree(store.Files)
		if err != nil {
			return fmt.Errorf("memory store %s: %w", store.Slug, err)
		}
		want[path.Join(s.Memory, store.Slug)] = wantMount{tree: t, store: store.StoreID, readOnly: store.ReadOnly}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[s.SessionID]
	if current == nil {
		current = map[string]*mounted{}
		m.sessions[s.SessionID] = current
	}
	for dir, mt := range current {
		if w, ok := want[dir]; !ok || w.store != mt.view.store || w.readOnly != mt.view.readOnly {
			if err := mt.unmount(); err != nil {
				return fmt.Errorf("unmount %s: %w", dir, err)
			}
			delete(current, dir)
		}
	}
	for dir, w := range want {
		if mt := current[dir]; mt != nil {
			mt.view.replace(w.tree, s.Owner, s.Source)
			continue
		}
		v := &view{m: m, dir: dir, store: w.store, readOnly: w.readOnly, mounted: time.Now(), tree: w.tree,
			owner: s.Owner, source: s.Source}
		began := time.Now()
		mt, err := mount(v)
		m.stats.mountNanos.Add(int64(time.Since(began)))
		if err != nil {
			m.stats.mountFailures.Add(1)
			return fmt.Errorf("mount %s: %w", dir, err)
		}
		m.stats.mounts.Add(1)
		current[dir] = mt
	}
	return nil
}

type wantMount struct {
	tree     *tree
	store    string
	readOnly bool
}

// Unmount implements bmaexecutor.SessionMounter.
func (m *Mounter) Unmount(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var first error
	for dir, mt := range m.sessions[sessionID] {
		if err := mt.unmount(); err != nil && first == nil {
			first = fmt.Errorf("unmount %s: %w", dir, err)
		}
	}
	delete(m.sessions, sessionID)
	return first
}
