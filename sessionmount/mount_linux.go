//go:build linux

package sessionmount

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// kernelTimeout is how long the kernel keeps an entry or attribute; a
// changed file is a new inode (its number hashes path and digest), so a
// refresh is visible within it and an open file keeps its content.
const kernelTimeout = time.Second

// New returns a Mounter. The process must be root and /dev/fuse usable.
func New(opts Options) (*Mounter, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("sessionmount: mounting needs root")
	}
	stats := &counters{}
	c, err := newCache(opts.CacheDir, opts.CacheBytes, stats)
	if err != nil {
		return nil, fmt.Errorf("sessionmount: cache: %w", err)
	}
	return &Mounter{cache: c, memories: &memoryCache{items: map[string][]byte{}}, stats: stats,
		sessions: map[string]map[string]*mounted{}}, nil
}

// Probe mounts an empty read-only file system below dir, lists it and
// unmounts it: what the executor checks before it declares
// capabilities.session_files.mount.
func Probe(dir string) error {
	point := filepath.Join(dir, ".bma-mount-probe")
	m := &Mounter{stats: &counters{}}
	v := &view{m: m, dir: point, readOnly: true, mounted: time.Now(), tree: &tree{files: map[string]bmaexecutor.MountedFile{},
		dirs: map[string][]string{"": nil}}}
	mt, err := mount(v)
	if err != nil {
		return err
	}
	_, listErr := os.ReadDir(point)
	if err := mt.unmount(); err != nil {
		return err
	}
	return listErr
}

// DetachStale detaches every mount of this package below root (left by an
// executor that ended without unmounting).
func DetachStale(root string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var points []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		for i, field := range fields {
			if field == "-" && i+1 < len(fields) && fields[i+1] == "fuse."+FSName && len(fields) > 4 {
				if p := fields[4]; p == root || strings.HasPrefix(p, root+"/") {
					points = append(points, p)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for i := len(points) - 1; i >= 0; i-- {
		if err := unix.Unmount(points[i], unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("detach %s: %w", points[i], err)
		}
		_ = os.Remove(points[i])
	}
	return nil
}

// mounted is one live mount.
type mounted struct {
	view   *view
	server *fuse.Server
}

// mount creates the mount point (root-owned, 0555) and mounts v on it.
func mount(v *view) (*mounted, error) {
	if err := os.MkdirAll(v.dir, 0o555); err != nil {
		return nil, err
	}
	if err := os.Chown(v.dir, 0, 0); err != nil {
		return nil, err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	options := []string{"default_permissions"}
	if v.readOnly {
		flags |= syscall.MS_RDONLY
		options = append(options, "ro")
	}
	t := kernelTimeout
	server, err := fs.Mount(v.dir, &node{v: v}, &fs.Options{
		EntryTimeout: &t, AttrTimeout: &t, NegativeTimeout: &t,
		MountOptions: fuse.MountOptions{
			FsName: FSName, Name: FSName, AllowOther: true, Options: options,
			DirectMount: true, DirectMountStrict: true, DirectMountFlags: flags,
			MaxReadAhead: ChunkBytes / 64,
		},
	})
	if err != nil {
		_ = os.Remove(v.dir)
		return nil, err
	}
	return &mounted{view: v, server: server}, nil
}

// unmount detaches the mount (lazily when busy) and removes its mount point.
func (mt *mounted) unmount() error {
	if err := mt.server.Unmount(); err != nil {
		if derr := unix.Unmount(mt.view.dir, unix.MNT_DETACH); derr != nil && !errors.Is(derr, unix.EINVAL) {
			return errors.Join(err, derr)
		}
	}
	_ = os.Remove(mt.view.dir)
	return nil
}

// node is one directory or file of a view. A file node is immutable: a
// lookup that finds other content makes a new node.
type node struct {
	fs.Inode
	v    *view
	path string // "" is the root
	file *bmaexecutor.MountedFile
}

var (
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
)

// denied answers EACCES to anyone but the session's user and root.
func (n *node) denied(ctx context.Context) bool {
	caller, ok := fuse.FromContext(ctx)
	return !ok || !n.v.allowed(caller.Uid)
}

func (n *node) fill(a *fuse.Attr) {
	_, owner, _ := n.v.snapshot()
	mtime := n.v.mounted
	switch {
	case n.file != nil:
		a.Mode = syscall.S_IFREG | 0o444
		a.Nlink = 1
		a.Size = uint64(n.file.Size) //nolint:gosec // sizes are not negative
		a.Blocks = (a.Size + 511) / 512
	case n.path == "":
		a.Mode = syscall.S_IFDIR | 0o700
		a.Nlink = 2
		a.Uid, a.Gid = owner.UID, owner.GID
	default:
		a.Mode = syscall.S_IFDIR | 0o555
		a.Nlink = 2
	}
	a.Blksize = 4096
	a.SetTimes(&mtime, &mtime, &mtime)
}

func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if n.denied(ctx) {
		return syscall.EACCES
	}
	n.fill(&out.Attr)
	return 0
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.denied(ctx) {
		return nil, syscall.EACCES
	}
	t, _, _ := n.v.snapshot()
	p := path.Join(n.path, name)
	child := &node{v: n.v, path: p}
	var ino uint64
	mode := uint32(syscall.S_IFDIR)
	if f, ok := t.files[p]; ok {
		child.file = &f
		ino, mode = inode("f", p, f.Digest), syscall.S_IFREG
	} else if _, ok := t.dirs[p]; ok {
		ino = inode("d", p, "")
	} else {
		return nil, syscall.ENOENT
	}
	child.fill(&out.Attr)
	return n.NewInode(ctx, child, fs.StableAttr{Mode: mode, Ino: ino}), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if n.denied(ctx) {
		return nil, syscall.EACCES
	}
	t, _, _ := n.v.snapshot()
	names := t.dirs[n.path]
	entries := make([]fuse.DirEntry, 0, len(names))
	for _, name := range names {
		p := path.Join(n.path, name)
		if f, ok := t.files[p]; ok {
			entries = append(entries, fuse.DirEntry{Name: name, Mode: syscall.S_IFREG, Ino: inode("f", p, f.Digest)})
		} else {
			entries = append(entries, fuse.DirEntry{Name: name, Mode: syscall.S_IFDIR, Ino: inode("d", p, "")})
		}
	}
	return fs.NewListDirStream(entries), 0
}

// Open refuses writes (the views are read-only) and opens the content: an
// upload through the disk cache, a memory from this process or the store.
func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.denied(ctx) {
		return nil, 0, syscall.EACCES
	}
	if n.file == nil {
		return nil, 0, syscall.EISDIR
	}
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
		return nil, 0, syscall.EROFS
	}
	if n.v.store != "" {
		b, digest, err := n.v.memory(ctx, *n.file)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		if digest != n.file.Digest {
			// The store changed since the listing: its content wins; the
			// size the kernel holds is stale, so this handle is direct.
			return &memoryHandle{b: b}, fuse.FOPEN_DIRECT_IO, 0
		}
		return &memoryHandle{b: b}, fuse.FOPEN_KEEP_CACHE, 0
	}
	o, err := n.v.upload(*n.file)
	if err != nil {
		return nil, 0, syscall.EIO
	}
	return &uploadHandle{v: n.v, o: o}, fuse.FOPEN_KEEP_CACHE, 0
}

func inode(kind, p, digest string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(kind + "\x00" + p + "\x00" + digest))
	return h.Sum64() | 1<<63 // never 1 (the root)
}

type memoryHandle struct{ b []byte }

var _ fs.FileReader = (*memoryHandle)(nil)

func (h *memoryHandle) Read(_ context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(h.b)) {
		return fuse.ReadResultData(nil), 0
	}
	return fuse.ReadResultData(h.b[off:min(int64(len(h.b)), off+int64(len(dest)))]), 0
}

type uploadHandle struct {
	v *view
	o *object
}

var (
	_ fs.FileReader   = (*uploadHandle)(nil)
	_ fs.FileReleaser = (*uploadHandle)(nil)
)

func (h *uploadHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	n, err := h.o.readAt(ctx, dest, off, h.v.m.stats)
	if err != nil {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *uploadHandle) Release(context.Context) syscall.Errno {
	h.v.m.cache.release(h.o)
	return 0
}
