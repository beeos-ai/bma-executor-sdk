//go:build linux

// The cache backs the Linux mounts only: it renames the checked file while
// it is open, which Windows refuses.

package sessionmount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeFetch serves content and records each request ("off+len", -1 whole).
type fakeFetch struct {
	content []byte
	mu      sync.Mutex
	calls   []string
	fail    atomic.Bool
}

func (f *fakeFetch) fetch(_ context.Context, offset, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%d+%d", offset, length))
	f.mu.Unlock()
	if f.fail.Load() {
		return nil, errors.New("network down")
	}
	end := int64(len(f.content))
	if length >= 0 {
		end = offset + length
	}
	return io.NopCloser(bytes.NewReader(f.content[offset:end])), nil
}

func newTestCache(t *testing.T, limit int64) (*cache, *counters) {
	t.Helper()
	stats := &counters{}
	c, err := newCache(filepath.Join(t.TempDir(), "cache"), limit, stats)
	if err != nil {
		t.Fatal(err)
	}
	return c, stats
}

// A small file is fetched whole once, checked, and renamed to its digest; a
// second reference reads the complete content without fetching.
func TestCacheSmallFileFetchedWholeAndChecked(t *testing.T) {
	c, _ := newTestCache(t, 0)
	src := &fakeFetch{content: []byte("hello world")}
	o, complete, err := c.acquire(digestOf(src.content), int64(len(src.content)), src.fetch)
	if err != nil || complete {
		t.Fatalf("acquire: %v %v", complete, err)
	}
	buf := make([]byte, 5)
	n, err := o.readAt(t.Context(), buf, 6, c.stats)
	if err != nil || string(buf[:n]) != "world" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	if fmt.Sprint(src.calls) != "[0+-1]" {
		t.Fatalf("calls %v", src.calls)
	}
	if _, err := os.Stat(filepath.Join(c.dir, digestOf(src.content)[7:])); err != nil {
		t.Fatalf("checked content not under its digest: %v", err)
	}
	c.release(o)
	o2, complete, err := c.acquire(digestOf(src.content), int64(len(src.content)), src.fetch)
	if err != nil || !complete || o2 != o {
		t.Fatalf("second acquire: %v %v", complete, err)
	}
	c.release(o2)
}

// Bytes that are not the digest are never returned: the read fails and so
// does every later one.
func TestCacheRefusesContentThatIsNotItsDigest(t *testing.T) {
	c, _ := newTestCache(t, 0)
	src := &fakeFetch{content: []byte("tampered")}
	o, _, err := c.acquire(digestOf([]byte("original")), 8, src.fetch)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	for range 2 {
		if _, err := o.readAt(t.Context(), buf, 0, c.stats); !errors.Is(err, ErrDigest) {
			t.Fatalf("read = %v, want ErrDigest", err)
		}
	}
	c.release(o)
	if _, ok := c.objects[digestOf([]byte("original"))[7:]]; ok {
		t.Fatal("unusable content kept")
	}
}

// A large file is read lazily: a read fetches only the ranges it covers;
// once every range is there the file is checked.
func TestCacheLargeFileReadLazilyByRange(t *testing.T) {
	c, _ := newTestCache(t, 0)
	content := bytes.Repeat([]byte("0123456789abcdef"), (3*ChunkBytes+100)/16+1)[:3*ChunkBytes+100]
	src := &fakeFetch{content: content}
	o, _, err := c.acquire(digestOf(content), int64(len(content)), src.fetch)
	if err != nil {
		t.Fatal(err)
	}
	defer c.release(o)
	buf := make([]byte, 4)
	off := int64(2*ChunkBytes + 16)
	if n, err := o.readAt(t.Context(), buf, off, c.stats); err != nil || !bytes.Equal(buf[:n], content[off:off+4]) {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	if want := fmt.Sprintf("[%d+%d]", 2*ChunkBytes, ChunkBytes); fmt.Sprint(src.calls) != want {
		t.Fatalf("calls %v, want %s", src.calls, want)
	}
	all := make([]byte, len(content))
	if n, err := o.readAt(t.Context(), all, 0, c.stats); err != nil || n != len(content) || !bytes.Equal(all, content) {
		t.Fatalf("whole read %d %v", n, err)
	}
	if len(src.calls) != 4 || o.have != nil {
		t.Fatalf("calls %v, have %v", src.calls, o.have != nil)
	}
}

// Without the network a fetch fails (EIO for the reader) and is retried by
// the next read; what is cached keeps being read.
func TestCacheNetworkLossFailsUncachedReadsOnly(t *testing.T) {
	c, stats := newTestCache(t, 0)
	cached := &fakeFetch{content: []byte("cached")}
	o, _, _ := c.acquire(digestOf(cached.content), 6, cached.fetch)
	buf := make([]byte, 6)
	if _, err := o.readAt(t.Context(), buf, 0, stats); err != nil {
		t.Fatal(err)
	}
	cached.fail.Store(true)
	if n, err := o.readAt(t.Context(), buf, 0, stats); err != nil || string(buf[:n]) != "cached" {
		t.Fatalf("cached read without the network: %q %v", buf[:n], err)
	}
	c.release(o)

	other := &fakeFetch{content: []byte("other")}
	other.fail.Store(true)
	o, _, _ = c.acquire(digestOf(other.content), 5, other.fetch)
	if _, err := o.readAt(t.Context(), buf, 0, stats); err == nil {
		t.Fatal("uncached read without the network succeeded")
	}
	if stats.fetchErrors.Load() != 1 {
		t.Fatalf("fetch errors %d", stats.fetchErrors.Load())
	}
	other.fail.Store(false)
	if n, err := o.readAt(t.Context(), buf, 0, stats); err != nil || string(buf[:n]) != "other" {
		t.Fatalf("retry: %q %v", buf[:n], err)
	}
	c.release(o)
}

// Concurrent readers of one range wait for one fetch.
func TestCacheConcurrentReadersFetchOnce(t *testing.T) {
	c, _ := newTestCache(t, 0)
	src := &fakeFetch{content: []byte("shared content")}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			o, _, err := c.acquire(digestOf(src.content), int64(len(src.content)), src.fetch)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.release(o)
			buf := make([]byte, 6)
			if n, err := o.readAt(t.Context(), buf, 0, c.stats); err != nil || string(buf[:n]) != "shared" {
				t.Errorf("read %q %v", buf[:n], err)
			}
		})
	}
	wg.Wait()
	if len(src.calls) != 1 {
		t.Fatalf("calls %v", src.calls)
	}
}

// Unreferenced content goes, oldest first, when the cache would exceed its
// limit; content still open stays.
func TestCacheEvictsIdleContentOldestFirst(t *testing.T) {
	c, _ := newTestCache(t, 10)
	get := func(s string) *object {
		src := &fakeFetch{content: []byte(s)}
		o, _, err := c.acquire(digestOf(src.content), int64(len(s)), src.fetch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.readAt(t.Context(), make([]byte, len(s)), 0, c.stats); err != nil {
			t.Fatal(err)
		}
		return o
	}
	a := get("aaaa")
	c.release(a)
	b := get("bbbb") // open
	get("cccc")      // 12 > 10: a (idle) goes, b (open) stays
	if _, ok := c.objects[digestOf([]byte("aaaa"))[7:]]; ok {
		t.Fatal("idle content kept over the limit")
	}
	if _, ok := c.objects[digestOf([]byte("bbbb"))[7:]]; !ok {
		t.Fatal("open content evicted")
	}
	c.release(b)
}
