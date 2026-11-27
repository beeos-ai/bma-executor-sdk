//go:build linux

package sessionmount

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// Writes to a read_write store (ADR-0111 item 7) go through the store at
// close: a file opened for writing is staged in this process and its close
// (or fsync) stores it against the digest it had when opened; unlink deletes
// against the digest it has; a rename writes the target and deletes the
// source. The store wins a conflict: the entry shows the store's version (or
// is gone) and the call fails with EIO. A memory rule's refusal fails it
// with the matching errno (EFBIG, ENOSPC, EROFS, EINVAL), nothing reaching
// the platform (not_saved) with EIO, and the entry keeps the store's last
// content. Scratch names (hidden, "~", ".tmp" and the other outputs scratch
// forms) stay in this process and are never sent, so writing a temporary
// file and renaming it onto a memory saves one version. Every outcome but
// "written" is also a SessionMemoryReport for the model.
const (
	// maxMemoryBytes is the memory content rule (100 kB): a longer write
	// fails with EFBIG at once.
	maxMemoryBytes = 102400
	// maxScratchBytes bounds one scratch file kept in this process.
	maxScratchBytes = 16 << 20
	// bulkDeleteLimit is how many memories of a store may be deleted between
	// two refreshes (ADR-0110 item 4).
	bulkDeleteLimit = 50
)

// scratchSuffixes are the outputs rule's temporary-file suffixes.
var scratchSuffixes = []string{".tmp", ".temp", ".swp", ".swo", ".part", ".crdownload", ".pyc"}

// scratch reports a name the outputs scratch rule refuses: it stays local.
func scratch(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") || seg == "__pycache__" {
			return true
		}
	}
	name := strings.ToLower(path.Base(p))
	if strings.HasPrefix(name, "~$") || strings.HasSuffix(name, "~") {
		return true
	}
	for _, s := range scratchSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// content is the current bytes of the file p: local ones, or the store's.
func (v *view) content(ctx context.Context, p string) ([]byte, string, syscall.Errno) {
	v.mu.RLock()
	f, ok := v.tree.files[p]
	b, isLocal := v.local[p]
	v.mu.RUnlock()
	switch {
	case isLocal:
		return append([]byte(nil), b...), "", 0
	case !ok:
		return nil, "", syscall.ENOENT
	}
	b, digest, err := v.memory(ctx, f)
	if err != nil {
		return nil, "", syscall.EIO
	}
	return append([]byte(nil), b...), digest, 0
}

// setLocal shows p with bytes b that are not in the store.
func (v *view) setLocal(p string, b []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.local == nil {
		v.local = map[string][]byte{}
	}
	v.local[p] = b
	t := v.tree.clone()
	t.put(bmaexecutor.MountedFile{Path: p, Size: int64(len(b))})
	v.tree = t
}

// stored shows p as the store holds it: digest and b, or gone when b is nil.
func (v *view) stored(p, digest string, b []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.local, p)
	t := v.tree.clone()
	if b == nil {
		t.drop(p)
	} else {
		t.put(bmaexecutor.MountedFile{Path: p, Key: "/" + p, Digest: digest, Size: int64(len(b))})
		v.m.memories.put(digest, b)
	}
	v.tree = t
}

// forget drops what p has locally; a stored p keeps the store's content.
func (v *view) forget(p string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.local[p]; !ok {
		return
	}
	delete(v.local, p)
	t := v.tree.clone()
	t.drop(p)
	v.tree = t
}

// base is the store digest of p ("" when p is not in the store).
func (v *view) base(p string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if _, isLocal := v.local[p]; isLocal {
		return ""
	}
	return v.tree.files[p].Digest
}

// commit stores b at p against base. Callers hold v.writes.
func (v *view) commit(ctx context.Context, p, base string, b []byte) syscall.Errno {
	if scratch(p) {
		v.setLocal(p, b)
		return 0
	}
	_, _, source := v.snapshot()
	ctx, cancel := context.WithTimeout(ctx, memoryTimeout) // a FUSE interrupt cancels it
	defer cancel()
	w, err := source.WriteMemory(ctx, v.store, "/"+p, base, b)
	if errno := v.failed(p, err); errno != 0 {
		return errno
	}
	if w.Status == "conflict" {
		return v.storeWins(p, w, "/"+p+" changed in the store since this session read it (another session or the "+
			"API wrote it first); the store's version replaced your copy. Read it again and re-apply your change "+
			"if it is still needed.")
	}
	v.m.stats.commits.Add(1)
	v.stored(p, digestOf(b), append([]byte{}, b...)) // never nil: an empty memory is not a deletion
	return 0
}

// remove deletes p from the store. Callers hold v.writes.
func (v *view) remove(ctx context.Context, p string) syscall.Errno {
	base := v.base(p)
	if base == "" {
		v.forget(p)
		return 0
	}
	v.mu.Lock()
	refuse, first := v.deletes >= bulkDeleteLimit, !v.deleteRefused
	if refuse {
		v.deleteRefused = true
	}
	v.mu.Unlock()
	_, _, source := v.snapshot()
	if refuse {
		if first {
			source.Report(bmaexecutor.SessionMemoryReport{MemoryStoreID: v.store, Outcome: "delete_refused",
				Message: "More than 50 memories of this store were deleted in one turn; a turn never deletes more. " +
					"Nothing more is deleted until the next turn. Delete memories a few at a time, or through the API."})
		}
		return syscall.EPERM
	}
	ctx, cancel := context.WithTimeout(ctx, memoryTimeout) // a FUSE interrupt cancels it
	defer cancel()
	w, err := source.DeleteMemory(ctx, v.store, "/"+p, base)
	if errno := v.failed(p, err); errno != 0 {
		return errno
	}
	if w.Status == "conflict" {
		return v.storeWins(p, w, "/"+p+" changed in the store since this session read it, so it was not deleted; "+
			"the store's version was restored.")
	}
	v.mu.Lock()
	v.deletes++
	v.mu.Unlock()
	v.m.stats.commits.Add(1)
	v.stored(p, "", nil)
	return 0
}

// storeWins shows the store's version of p after a conflict and reports it.
func (v *view) storeWins(p string, w bmaexecutor.MemoryWrite, message string) syscall.Errno {
	v.m.stats.conflicts.Add(1)
	if w.Content == nil {
		v.stored(p, "", nil)
		message = "/" + p + " was deleted from the store since this session read it, so your change was not kept. " +
			"Write it again if it is still needed."
	} else {
		b := append([]byte{}, *w.Content...)
		v.stored(p, digestOf(b), b)
	}
	_, _, source := v.snapshot()
	source.Report(bmaexecutor.SessionMemoryReport{MemoryStoreID: v.store, Path: "/" + p, Outcome: "conflict", Message: message})
	return syscall.EIO
}

// failed maps a write or delete that did not reach an answer: a memory rule's
// refusal is reported with its errno; anything else (not_saved) is EIO. The
// entry keeps the store's content.
func (v *view) failed(p string, err error) syscall.Errno {
	if err == nil {
		return 0
	}
	v.m.stats.commitFailures.Add(1)
	v.forget(p)
	var refusal *bmaexecutor.DaemonHTTPError
	if !errors.As(err, &refusal) || refusal.StatusCode >= 500 || refusal.StatusCode == http.StatusNotFound ||
		refusal.StatusCode == http.StatusTooManyRequests {
		return syscall.EIO
	}
	_, _, source := v.snapshot()
	source.Report(bmaexecutor.SessionMemoryReport{MemoryStoreID: v.store, Path: "/" + p, Outcome: "rejected",
		Message: refusal.Message})
	switch {
	case refusal.Code == "memory_store_full":
		return syscall.ENOSPC
	case refusal.Code == "memory_store_archived" || refusal.StatusCode == http.StatusForbidden:
		return syscall.EROFS
	case refusal.StatusCode == http.StatusRequestEntityTooLarge || strings.Contains(refusal.Message, "102400"):
		return syscall.EFBIG
	}
	return syscall.EINVAL
}

// writeHandle is a file of a read_write store open for writing.
type writeHandle struct {
	n *node
	p string

	mu    sync.Mutex
	base  string // the store digest the file had when opened ("" new)
	b     []byte
	dirty bool // written since the last commit: committed at the next close
	// touched: created or truncated and not written since; committed when
	// the last descriptor closes (a shell redirection closes a duplicate
	// before it writes, which must not save an empty version).
	touched bool
	// refused is a write's errno: the close fails with it and commits
	// nothing (the store keeps its content; a new file goes).
	refused syscall.Errno
}

var (
	_ fs.FileReader   = (*writeHandle)(nil)
	_ fs.FileWriter   = (*writeHandle)(nil)
	_ fs.FileFlusher  = (*writeHandle)(nil)
	_ fs.FileFsyncer  = (*writeHandle)(nil)
	_ fs.FileReleaser = (*writeHandle)(nil)
)

// opened registers h as open for writing.
func (h *writeHandle) opened() *writeHandle {
	v := h.n.v
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.writers == nil {
		v.writers = map[string][]any{}
	}
	v.writers[h.p] = append(v.writers[h.p], h)
	return h
}

// Release commits a file created or truncated and never written (touch,
// ": > f"), then forgets h.
func (h *writeHandle) Release(ctx context.Context) syscall.Errno {
	h.mu.Lock()
	touched := h.touched && h.refused == 0
	h.mu.Unlock()
	if touched {
		h.dirty = true
		_ = h.Flush(ctx)
	}
	v := h.n.v
	v.mu.Lock()
	defer v.mu.Unlock()
	list := v.writers[h.p]
	for i, other := range list {
		if other == h {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(v.writers, h.p)
	} else {
		v.writers[h.p] = list
	}
	return 0
}

// writer is the latest file open for writing at p, or nil.
func (v *view) writer(p string) *writeHandle {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if list := v.writers[p]; len(list) > 0 {
		return list[len(list)-1].(*writeHandle)
	}
	return nil
}

func (h *writeHandle) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if off >= int64(len(h.b)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(append([]byte(nil), h.b[off:min(int64(len(h.b)), off+int64(len(dest)))]...)), 0
}

func (h *writeHandle) limit() int64 {
	if scratch(h.p) {
		return maxScratchBytes
	}
	return maxMemoryBytes
}

func (h *writeHandle) Write(_ context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	end := off + int64(len(data))
	if off < 0 || end > h.limit() {
		h.refused = syscall.EFBIG
		return 0, syscall.EFBIG
	}
	if end > int64(len(h.b)) {
		h.b = append(h.b, make([]byte, end-int64(len(h.b)))...)
	}
	copy(h.b[off:], data)
	h.dirty, h.touched = true, false
	return uint32(len(data)), 0 //nolint:gosec // a write is at most the kernel's buffer
}

// truncate sets the staged size.
func (h *writeHandle) truncate(size uint64) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if int64(size) > h.limit() { //nolint:gosec // bounded just above
		return syscall.EFBIG
	}
	if s := int(size); s <= len(h.b) {
		h.b = h.b[:s]
	} else {
		h.b = append(h.b, make([]byte, s-len(h.b))...)
	}
	if !h.dirty {
		h.touched = true
	}
	return 0
}

// Flush commits a changed file at close.
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno {
	h.n.v.writes.Lock()
	defer h.n.v.writes.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refused != 0 {
		h.n.v.forget(h.p)
		return h.refused
	}
	if !h.dirty {
		return 0
	}
	h.dirty, h.touched = false, false
	errno := h.n.v.commit(ctx, h.p, h.base, h.b)
	if errno == 0 {
		h.base = h.n.v.base(h.p)
	}
	return errno
}

func (h *writeHandle) Fsync(ctx context.Context, _ uint32) syscall.Errno { return h.Flush(ctx) }

// openWrite opens p of a read_write store for writing: its current content
// (none with O_TRUNC), committed at close.
func (n *node) openWrite(ctx context.Context, p string, flags uint32) (*writeHandle, syscall.Errno) {
	h := &writeHandle{n: n, p: p, base: n.v.base(p)}
	if flags&syscall.O_TRUNC != 0 {
		h.touched = true
		return h.opened(), 0
	}
	b, _, errno := n.v.content(ctx, p)
	if errno != 0 {
		return nil, errno
	}
	h.b = b
	return h.opened(), 0
}

var (
	_ fs.NodeCreater    = (*node)(nil)
	_ fs.NodeUnlinker   = (*node)(nil)
	_ fs.NodeRenamer    = (*node)(nil)
	_ fs.NodeMkdirer    = (*node)(nil)
	_ fs.NodeRmdirer    = (*node)(nil)
	_ fs.NodeSetattrer  = (*node)(nil)
	_ fs.NodeSymlinker  = (*node)(nil)
	_ fs.NodeLinker     = (*node)(nil)
	_ fs.NodeSetxattrer = (*node)(nil)
)

// writable answers 0 for a call the session may make on a read_write store.
func (n *node) writable(ctx context.Context) syscall.Errno {
	switch {
	case n.denied(ctx):
		return syscall.EACCES
	case n.v.readOnly || n.v.store == "":
		return syscall.EROFS
	}
	return 0
}

func (n *node) Create(ctx context.Context, name string, _, _ uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle,
	uint32, syscall.Errno) {
	if errno := n.writable(ctx); errno != 0 {
		return nil, nil, 0, errno
	}
	p := path.Join(n.rel(), name)
	n.v.writes.Lock()
	defer n.v.writes.Unlock()
	// The kernel creates only a name it did not find: a new file, committed
	// at close.
	h := (&writeHandle{n: n, p: p, touched: true}).opened()
	n.v.setLocal(p, nil)
	child := &node{v: n.v, path: p}
	inode := n.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG, Ino: inode("f", p, "")})
	child.fill(&out.Attr)
	return inode, h, 0, 0
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	if errno := n.writable(ctx); errno != 0 {
		return errno
	}
	n.v.writes.Lock()
	defer n.v.writes.Unlock()
	return n.v.remove(ctx, path.Join(n.rel(), name))
}

// Rename moves a file inside the store: the target is written with the
// source's content, then the source deleted. Directories and other mounts
// are EXDEV (mv copies then).
func (n *node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if errno := n.writable(ctx); errno != 0 {
		return errno
	}
	to, ok := newParent.(*node)
	if !ok || to.v != n.v || flags != 0 {
		return syscall.EXDEV
	}
	src, dst := path.Join(n.rel(), name), path.Join(to.rel(), newName)
	n.v.writes.Lock()
	defer n.v.writes.Unlock()
	v := n.v
	v.mu.RLock()
	_, isFile := v.tree.files[src]
	_, isDir := v.tree.dirs[src]
	v.mu.RUnlock()
	switch {
	case isDir:
		return syscall.EXDEV
	case !isFile:
		return syscall.ENOENT
	case src == dst:
		return 0
	}
	b, _, errno := v.content(ctx, src)
	if errno != 0 {
		return errno
	}
	if errno := v.commit(ctx, dst, v.base(dst), b); errno != 0 {
		return errno
	}
	return v.remove(ctx, src)
}

func (n *node) Mkdir(ctx context.Context, name string, _ uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if errno := n.writable(ctx); errno != 0 {
		return nil, errno
	}
	p := path.Join(n.rel(), name)
	v := n.v
	v.mu.Lock()
	t := v.tree.clone()
	t.mkdir(p)
	v.tree = t
	v.mu.Unlock()
	child := &node{v: v, path: p}
	child.fill(&out.Attr)
	return n.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: inode("d", p, "")}), 0
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if errno := n.writable(ctx); errno != 0 {
		return errno
	}
	p := path.Join(n.rel(), name)
	v := n.v
	v.mu.Lock()
	defer v.mu.Unlock()
	names, ok := v.tree.dirs[p]
	switch {
	case !ok:
		return syscall.ENOENT
	case len(names) > 0:
		return syscall.ENOTEMPTY
	}
	t := v.tree.clone()
	t.rmdir(p)
	v.tree = t
	return 0
}

// Setattr truncates (through the open handle, or as an open, truncate and
// close of the path) and accepts times; modes and owners are EPERM.
func (n *node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if errno := n.writable(ctx); errno != 0 {
		return errno
	}
	_, mode := in.GetMode()
	_, uid := in.GetUID()
	_, gid := in.GetGID()
	if mode || uid || gid {
		return syscall.EPERM
	}
	if size, ok := in.GetSize(); ok {
		if _, isWrite := f.(*writeHandle); !isWrite {
			if h := n.v.writer(n.rel()); h != nil {
				f = h // open(O_TRUNC) truncates without its handle
			}
		}
		if h, isWrite := f.(*writeHandle); isWrite {
			if errno := h.truncate(size); errno != 0 {
				return errno
			}
		} else {
			p := n.rel()
			n.v.writes.Lock()
			h, errno := n.openWrite(ctx, p, 0)
			if errno == 0 {
				errno = h.truncate(size)
			}
			if errno == 0 {
				h.touched = false // committed here, not at its release
				errno = n.v.commit(ctx, p, h.base, h.b)
			}
			if h != nil {
				_ = h.Release(ctx)
			}
			n.v.writes.Unlock()
			if errno != 0 {
				return errno
			}
		}
	}
	n.fill(&out.Attr)
	if h, isWrite := f.(*writeHandle); isWrite {
		h.mu.Lock()
		out.Size = uint64(len(h.b))
		h.mu.Unlock()
	}
	return 0
}

func (n *node) Symlink(context.Context, string, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (n *node) Link(context.Context, fs.InodeEmbedder, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EPERM
}

func (n *node) Setxattr(context.Context, string, []byte, uint32) syscall.Errno { return syscall.EPERM }
