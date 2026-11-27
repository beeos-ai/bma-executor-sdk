package bmaexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// A session the walk forgets (no process or session_files.sync of it for
// sessionFilesIdle, while an ACP turn may still be editing its memory with
// its own tools) parks the bases of its read_write stores in its session
// directory. The next refresh of the session (process start or
// session_files.sync) takes them back before it writes back, so a change made
// while it was forgotten is sent against its base instead of being
// overwritten by the refresh. The bases are only a starting point: the
// marker still decides which directories are trusted, and the platform
// checks every base (a wrong one is a conflict the store wins).
const sessionMemoryBases = ".bma-memory-bases.json"

type parkedMemory struct {
	Stores map[string]parkedStore `json:"stores"` // by memory store id
}

type parkedStore struct {
	Slug  string            `json:"slug"`
	Files map[string]string `json:"files"` // stored path -> digest
}

// parkMemoryLocked writes the bases of s's read_write stores (after the
// final writeback of a forgotten session).
func (s *fileSession) parkMemoryLocked() error {
	parked := parkedMemory{Stores: map[string]parkedStore{}}
	for id, mirror := range s.memory {
		if mirror.access != memoryAccessReadWrite {
			continue
		}
		files := map[string]string{}
		for p, file := range mirror.files {
			files[p] = file.digest
		}
		parked.Stores[id] = parkedStore{Slug: mirror.slug, Files: files}
	}
	if len(parked.Stores) == 0 {
		return nil
	}
	raw, err := json.Marshal(parked)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(s.uploads))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	temporary := fmt.Sprintf(".bma-bases-%d", time.Now().UnixNano())
	if err := root.WriteFile(temporary, raw, 0o444); err != nil {
		return err
	}
	if err := replaceInRoot(root, temporary, sessionMemoryBases); err != nil {
		_ = removeInRoot(root, temporary)
		return err
	}
	return nil
}

// unparkMemoryLocked takes parked bases back into a session that has no
// mirror yet, and removes them.
func (s *fileSession) unparkMemoryLocked() error {
	if s.memory != nil {
		return nil
	}
	root, err := os.OpenRoot(filepath.Dir(s.uploads))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(sessionMemoryBases)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4*maxSessionMemoryBytes {
		return removeInRoot(root, sessionMemoryBases)
	}
	raw, err := root.ReadFile(sessionMemoryBases)
	if err != nil {
		return err
	}
	var parked parkedMemory
	if json.Unmarshal(raw, &parked) == nil {
		s.memory = map[string]*memoryMirror{}
		for id, store := range parked.Stores {
			mirror := &memoryMirror{slug: store.Slug, access: memoryAccessReadWrite, files: map[string]mountedFile{}}
			for p, digest := range store.Files {
				if validDigest(digest) {
					mirror.files[p] = mountedFile{digest: digest}
				}
			}
			s.memory[id] = mirror
		}
	}
	return removeInRoot(root, sessionMemoryBases)
}
