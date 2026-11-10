package bmaexecutor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The directories of a session's files (ADR-0021) are <root>/<session id>/
// {uploads,skills,outputs} below the executor's capabilities.session_files.root.
// A local brain's file tools may reach them besides the workspace, and only
// these three of this one session: uploads and skills read-only, outputs
// readable and writable. Each is opened as an os.Root of its own, so a "..",
// an absolute path elsewhere (another session's directory included) and a
// symbolic link that leaves the directory are refused by the operating system
// call, and the directory itself must be a real directory, not a link.

// The mounted directories of a session.
const (
	SessionMountUploads = "uploads"
	SessionMountSkills  = "skills"
	SessionMountOutputs = "outputs"
)

// sessionMount is one mounted directory of a session.
type sessionMount struct {
	name     string
	root     string // the executor's session files root
	session  string
	dir      string // <root>/<session>/<name>
	writable bool
}

// sessionMountsFor returns the directories of sessionID below root: uploads
// and skills read-only, outputs writable. The session id must be a single safe
// directory name.
func sessionMountsFor(root, sessionID string) ([]*sessionMount, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("session files root %q is not an absolute path", root)
	}
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, fmt.Errorf("session id %q is not a safe directory name", sessionID)
	}
	root = filepath.Clean(root)
	var mounts []*sessionMount
	for _, m := range []struct {
		name     string
		writable bool
	}{{SessionMountUploads, false}, {SessionMountSkills, false}, {SessionMountOutputs, true}} {
		mounts = append(mounts, &sessionMount{name: m.name, root: root, session: sessionID,
			dir: filepath.Join(root, sessionID, m.name), writable: m.writable})
	}
	return mounts, nil
}

// setLocalMounts lets the file tools of a local session reach the session's
// directories below root; an empty root clears them.
func (c *Core) setLocalMounts(sessionID, root string) error {
	session := c.session(coreKey{acpBrain, sessionID})
	if session == nil {
		return errors.New("the session is not open")
	}
	var mounts []*sessionMount
	if root != "" {
		var err error
		if mounts, err = sessionMountsFor(root, sessionID); err != nil {
			return err
		}
	}
	session.mu.Lock()
	session.mounts = mounts
	session.mu.Unlock()
	return nil
}

func (s *CoreSession) sessionMounts() []*sessionMount {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mounts
}

// rel is the absolute path p as a path below the mount's directory.
func (m *sessionMount) rel(p string) (string, bool) {
	return relUnder(p, dirAndReal(m.dir)...)
}

// open opens the mount's directory as a root. The session directory and the
// directory below it must be real directories, not links, and are the same
// ones when opened as when looked at (a swap in between fails).
func (m *sessionMount) open() (*os.Root, error) {
	top, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = top.Close() }()
	session, err := openChildRoot(top, m.session)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	return openChildRoot(session, m.name)
}

// openChildRoot opens the directory name of parent as a root, refusing a link.
func openChildRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (links are not followed)", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if after, err := child.Stat("."); err != nil || !os.SameFile(before, after) {
		_ = child.Close()
		return nil, fmt.Errorf("%s changed while it was opened", name)
	}
	return child, nil
}
