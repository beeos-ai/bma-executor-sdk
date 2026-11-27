package sessionmount

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Content of mounted uploads is cached on the machine's disk by sha256
// (ADR-0111): a file of at most ChunkBytes is fetched whole and checked
// before its first byte is returned; a larger one is fetched in ChunkBytes
// ranges as it is read and checked once every range is there. A file whose
// bytes do not match its digest is removed and every read of it fails.
// Objects nobody has open are evicted oldest first when the cache would
// exceed its limit. The cache is emptied when it is created: content is only
// trusted once this process checked it.
const (
	// ChunkBytes is the range a large file is fetched in.
	ChunkBytes = 8 << 20
	// DefaultCacheBytes bounds the cache.
	DefaultCacheBytes = 2 << 30
	// FetchTimeout bounds fetching one range (or one small file).
	FetchTimeout = 30 * time.Second
)

// ErrDigest reports content whose bytes are not its digest.
var ErrDigest = errors.New("sessionmount: content does not match its digest")

// fetchFunc reads length bytes (< 0: to the end) from offset.
type fetchFunc func(ctx context.Context, offset, length int64) (io.ReadCloser, error)

type cache struct {
	dir   string
	limit int64
	stats *counters

	mu      sync.Mutex
	objects map[string]*object // by hex digest
	idle    *list.List         // objects with no reference, least recently used first
	total   int64
}

// object is one file's content in the cache.
type object struct {
	hex   string
	size  int64
	fetch fetchFunc // replaced by every acquire (the latest anchor)

	mu       sync.Mutex
	path     string // the file's name: <hex>.part until checked, then <hex>
	file     *os.File
	have     []bool // per chunk; nil once complete and checked
	inflight map[int]chan struct{}
	err      error // ErrDigest: unusable

	refs int           // guarded by cache.mu
	elem *list.Element // in cache.idle while refs == 0
}

func newCache(dir string, limit int64, stats *counters) (*cache, error) {
	if limit <= 0 {
		limit = DefaultCacheBytes
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &cache{dir: dir, limit: limit, stats: stats, objects: map[string]*object{}, idle: list.New()}, nil
}

// acquire references the content of digest ("sha256:<hex>"), size bytes,
// fetched by fetch. It answers whether the content was already complete.
func (c *cache) acquire(digest string, size int64, fetch fetchFunc) (*object, bool, error) {
	sum, ok := strings.CutPrefix(digest, "sha256:")
	if _, err := hex.DecodeString(sum); !ok || err != nil || len(sum) != 64 || size < 0 {
		return nil, false, fmt.Errorf("sessionmount: invalid digest %q", digest)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if o := c.objects[sum]; o != nil {
		if o.refs == 0 && o.elem != nil {
			c.idle.Remove(o.elem)
			o.elem = nil
		}
		o.refs++
		o.mu.Lock()
		o.fetch = fetch
		complete := o.have == nil && o.err == nil
		o.mu.Unlock()
		return o, complete, nil
	}
	c.evictLocked(size)
	part := filepath.Join(c.dir, sum+".part")
	file, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return nil, false, err
	}
	chunks := max(1, int((size+ChunkBytes-1)/ChunkBytes))
	o := &object{hex: sum, size: size, fetch: fetch, path: part, file: file, have: make([]bool, chunks),
		inflight: map[int]chan struct{}{}, refs: 1}
	if size == 0 {
		// Nothing to fetch; the digest still has to be the empty content's.
		o.have[0] = true
		o.verifyLocked()
	}
	c.objects[sum] = o
	c.total += size
	return o, false, nil
}

// release drops a reference; an unreferenced object becomes evictable, an
// unusable one goes at once.
func (c *cache) release(o *object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o.refs--; o.refs > 0 {
		return
	}
	o.mu.Lock()
	broken := o.err != nil
	o.mu.Unlock()
	if broken {
		c.dropLocked(o)
		return
	}
	o.elem = c.idle.PushBack(o)
	c.evictLocked(0)
}

// evictLocked drops idle objects, oldest first, until adding more bytes
// stays within the limit (or nothing idle is left).
func (c *cache) evictLocked(more int64) {
	for c.total+more > c.limit && c.idle.Len() > 0 {
		c.dropLocked(c.idle.Front().Value.(*object))
	}
}

func (c *cache) dropLocked(o *object) {
	if o.elem != nil {
		c.idle.Remove(o.elem)
		o.elem = nil
	}
	if c.objects[o.hex] == o {
		delete(c.objects, o.hex)
		c.total -= o.size
	}
	o.mu.Lock()
	name := o.path
	_ = o.file.Close()
	o.mu.Unlock()
	_ = os.Remove(name)
}

// readAt reads into dest from off, fetching the chunks it covers.
func (o *object) readAt(ctx context.Context, dest []byte, off int64, stats *counters) (int, error) {
	if off >= o.size || len(dest) == 0 {
		return 0, nil
	}
	end := min(o.size, off+int64(len(dest)))
	for chunk := int(off / ChunkBytes); int64(chunk)*ChunkBytes < end; chunk++ {
		if err := o.ensure(ctx, chunk, stats); err != nil {
			return 0, err
		}
	}
	// A file of one chunk is only read once checked; ensure did that.
	n, err := o.file.ReadAt(dest[:end-off], off)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// ensure makes chunk present, fetching it once however many readers wait.
func (o *object) ensure(ctx context.Context, chunk int, stats *counters) error {
	for {
		o.mu.Lock()
		switch {
		case o.err != nil:
			o.mu.Unlock()
			return o.err
		case o.have == nil || o.have[chunk]:
			o.mu.Unlock()
			return nil
		}
		wait, busy := o.inflight[chunk]
		if !busy {
			wait = make(chan struct{})
			o.inflight[chunk] = wait
			fetch := o.fetch
			o.mu.Unlock()
			err := o.fill(ctx, chunk, fetch)
			o.mu.Lock()
			delete(o.inflight, chunk)
			close(wait)
			if err != nil {
				o.mu.Unlock()
				stats.fetchErrors.Add(1)
				return err
			}
			o.have[chunk] = true
			if !slicesAll(o.have) {
				o.mu.Unlock()
				return nil
			}
			o.verifyLocked()
			err = o.err
			o.mu.Unlock()
			return err
		}
		o.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fill fetches chunk into the file. A file of one chunk is fetched whole
// (no Range), so an empty or small file is one plain request.
func (o *object) fill(ctx context.Context, chunk int, fetch fetchFunc) error {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	offset := int64(chunk) * ChunkBytes
	length := min(ChunkBytes, o.size-offset)
	whole := len(o.have) == 1
	var body io.ReadCloser
	var err error
	if whole {
		body, err = fetch(ctx, 0, -1)
	} else {
		body, err = fetch(ctx, offset, length)
	}
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	n, err := io.Copy(io.NewOffsetWriter(o.file, offset), io.LimitReader(body, length+1))
	if err != nil {
		return err
	}
	if n != length {
		if whole {
			return fmt.Errorf("%w: %d bytes, want %d", ErrDigest, n, length)
		}
		return fmt.Errorf("sessionmount: range of %d bytes answered %d", length, n)
	}
	return nil
}

// verifyLocked checks the complete file against its digest and, when it
// matches, renames it to its digest; otherwise the object is unusable.
func (o *object) verifyLocked() {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(o.file, 0, o.size)); err != nil {
		o.err = err
		return
	}
	if hex.EncodeToString(h.Sum(nil)) != o.hex {
		o.err = ErrDigest
		return
	}
	done := strings.TrimSuffix(o.path, ".part")
	if err := os.Rename(o.path, done); err != nil {
		o.err = err
		return
	}
	o.path, o.have = done, nil
}

func slicesAll(have []bool) bool {
	for _, h := range have {
		if !h {
			return false
		}
	}
	return true
}
