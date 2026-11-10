package bmaexecutor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// The file tools (read, write, edit, glob, grep) reach files only through an
// os.Root opened on the session workspace: a ".." that leaves it, an absolute
// path outside it and a symbolic link that points out of it are refused by
// the operating system call itself, also when the path was swapped for a link
// between a check and the use. A link that stays inside the workspace works.

// workspaceDir is the session's workspace (openLocal may re-root it).
func (s *CoreSession) workspaceDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workspace
}

// relUnder is p as a clean path below one of roots, in OS form ("." for the
// root itself). It is lexical; os.Root enforces the rest when a file is used.
func relUnder(p string, roots ...string) (string, bool) {
	p = filepath.Clean(p)
	for _, root := range roots {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsLocal(rel) {
			return rel, true
		}
	}
	return "", false
}

// dirAndReal returns dir and, when it differs, dir with its symbolic links
// resolved: an absolute path may name either.
func dirAndReal(dir string) []string {
	roots := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		roots = append(roots, real)
	}
	return roots
}

// relPath is path as a clean, workspace-relative path in OS form ("." for the
// workspace itself), or an error when it is outside the workspace. It is a
// lexical check; os.Root enforces the rest when the file is used.
func (s *CoreSession) relPath(p string) (string, error) {
	workspace := s.workspaceDir()
	if p == "" {
		return ".", nil
	}
	outside := fmt.Errorf("path %q is outside the session workspace", p)
	if !filepath.IsAbs(p) {
		rel := filepath.Clean(p)
		if !filepath.IsLocal(rel) {
			return "", outside
		}
		return rel, nil
	}
	if rel, ok := relUnder(p, dirAndReal(workspace)...); ok {
		return rel, nil
	}
	return "", outside
}

// fileTarget is where a file tool's path points: the workspace (mount nil) or
// one of the session's mounted directories (core_mounts.go). rel is relative
// to that directory.
type fileTarget struct {
	mount *sessionMount
	rel   string
}

// target resolves a tool's path. The workspace comes first; an absolute path
// below one of the session's own mounts is the second choice, and a write
// there is refused unless the mount is writable. Anything else is outside.
func (s *CoreSession) target(p string, write bool) (fileTarget, error) {
	rel, err := s.relPath(p)
	if err == nil {
		return fileTarget{rel: rel}, nil
	}
	if !filepath.IsAbs(p) {
		return fileTarget{}, err
	}
	for _, m := range s.sessionMounts() {
		rel, ok := m.rel(p)
		switch {
		case !ok:
		case write && !m.writable:
			return fileTarget{}, fmt.Errorf("path %q is read-only (the session's %s)", p, m.name)
		default:
			return fileTarget{mount: m, rel: rel}, nil
		}
	}
	return fileTarget{}, err
}

// open opens the directory the target is relative to as a root; the caller
// closes it.
func (s *CoreSession) open(t fileTarget) (*os.Root, error) {
	if t.mount != nil {
		return t.mount.open()
	}
	return os.OpenRoot(s.workspaceDir())
}

// show is a path below the target's directory as the tools report it:
// relative to the workspace, absolute for a mount (the model passes it back).
func (t fileTarget) show(rel string) string {
	if t.mount != nil {
		return filepath.Join(t.mount.dir, rel)
	}
	return rel
}

// withRoot runs fn with the root of the directory path is in and the
// root-relative form of path. write says the call changes the file.
func (s *CoreSession) withRoot(p string, write bool, fn func(root *os.Root, rel string) error) error {
	t, err := s.target(p, write)
	if err != nil {
		return err
	}
	root, err := s.open(t)
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root, t.rel)
}

func (c *Core) read(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	var result ToolResultParams
	err := session.withRoot(input.Path, false, func(root *os.Root, rel string) error {
		file, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer file.Close()
		result, err = c.readFrom(file, input)
		return err
	})
	return result, err
}

// readPath reads a file outside the workspace by its path (a session's own
// output file, see LocalToolRoute) with the read tool's offset and limit.
func (c *Core) readPath(p string, input ToolInput) (ToolResultParams, error) {
	file, err := os.Open(p)
	if err != nil {
		return ToolResultParams{}, err
	}
	defer file.Close()
	return c.readFrom(file, input)
}

func (c *Core) readFrom(file io.Reader, input ToolInput) (ToolResultParams, error) {
	offset, limit := int64(1), int64(-1)
	if input.Offset > 0 {
		offset = input.Offset
	}
	if input.Limit > 0 {
		limit = input.Limit
	}
	out := &cappedBuffer{limit: c.opts.InlineResultBytes}
	reader := bufio.NewReader(file)
	for line := int64(1); limit != 0; line++ {
		text, err := reader.ReadString('\n')
		if line >= offset && text != "" {
			_, _ = out.Write([]byte(text))
			if limit > 0 {
				limit--
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ToolResultParams{}, err
		}
	}
	return textResult(false, out.String()), nil
}

func (c *Core) write(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	err := session.withRoot(input.Path, true, func(root *os.Root, rel string) error {
		if dir := filepath.Dir(rel); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		return root.WriteFile(rel, []byte(input.Content), 0o600)
	})
	if err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, fmt.Sprintf("wrote %d bytes", len(input.Content))), nil
}

// editFile replaces OldString by NewString in the file: the only match, or
// with all every match. It reports how many matches there were; nothing is
// written unless that count is one (or any, with all).
func (c *Core) editFile(session *CoreSession, input ToolInput, all bool) (matched int, err error) {
	if input.OldString == "" {
		return 0, errors.New("edit requires old_string and new_string")
	}
	err = session.withRoot(input.Path, true, func(root *os.Root, rel string) error {
		raw, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		text := string(raw)
		matched = strings.Count(text, input.OldString)
		if matched == 0 || (matched != 1 && !all) {
			return nil
		}
		return root.WriteFile(rel, []byte(strings.ReplaceAll(text, input.OldString, input.NewString)), 0o600)
	})
	return matched, err
}

func (c *Core) edit(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	matched, err := c.editFile(session, input, false)
	if err != nil {
		return ToolResultParams{}, err
	}
	if matched != 1 {
		return textResult(true, fmt.Sprintf("old_string must match exactly once; matched %d times", matched)), nil
	}
	return textResult(false, "edited "+filepath.Base(input.Path)), nil
}

func (c *Core) glob(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == "" {
		return ToolResultParams{}, errors.New("glob requires pattern")
	}
	t, err := session.target(input.Path, false)
	if err != nil {
		return ToolResultParams{}, err
	}
	pattern := filepath.Join(t.rel, input.Pattern)
	if !filepath.IsLocal(pattern) {
		return ToolResultParams{}, fmt.Errorf("path %q is outside the session workspace", filepath.Join(input.Path, input.Pattern))
	}
	root, err := session.open(t)
	if err != nil {
		return ToolResultParams{}, err
	}
	defer func() { _ = root.Close() }()
	found, err := fs.Glob(root.FS(), filepath.ToSlash(pattern))
	if err != nil {
		return ToolResultParams{}, err
	}
	matches := make([]string, len(found))
	for i, match := range found {
		matches[i] = t.show(filepath.FromSlash(match))
	}
	return textResult(false, strings.Join(matches, "\n")), nil
}

func (c *Core) grep(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	if input.Pattern == "" {
		return ToolResultParams{}, errors.New("grep requires pattern")
	}
	expression, err := regexp.Compile(input.Pattern)
	if err != nil {
		return ToolResultParams{}, err
	}
	t, err := session.target(input.Path, false)
	if err != nil {
		return ToolResultParams{}, err
	}
	root, err := session.open(t)
	if err != nil {
		return ToolResultParams{}, err
	}
	defer func() { _ = root.Close() }()
	var matches []string
	fsys, start := root.FS(), filepath.ToSlash(t.rel)
	err = fs.WalkDir(fsys, start, func(p string, entry fs.DirEntry, err error) error {
		if err != nil && p == start {
			return err // the search root itself cannot be used
		}
		if err != nil || entry.IsDir() || len(matches) >= maxGrepMatches {
			return nil
		}
		if input.Glob != "" {
			if ok, _ := path.Match(input.Glob, entry.Name()); !ok {
				return nil
			}
		}
		// Opening goes through the root: a link out of it fails.
		file, err := fsys.Open(p)
		if err != nil {
			return nil
		}
		defer file.Close()
		name := t.show(filepath.FromSlash(p))
		scanner := bufio.NewScanner(file)
		for line := 1; scanner.Scan() && len(matches) < maxGrepMatches; line++ {
			if expression.Match(scanner.Bytes()) {
				matches = append(matches, fmt.Sprintf("%s:%d:%s", name, line, scanner.Text()))
			}
		}
		return nil
	})
	if err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, strings.Join(matches, "\n")), nil
}

// File operations a FileAccessFunc is asked about.
const (
	FileOperationRead  = "read"
	FileOperationWrite = "write"
)

// FileAccessRequest is what CoreOptions.FileAccess is asked before a file
// tool runs.
type FileAccessRequest struct {
	// SessionID is the executor's session id. AgentSessionID is the agent
	// session: for a LocalToolRoute the same id, for a tool.call the
	// session.assign metadata agent_session_id (empty when it has none).
	SessionID, AgentSessionID string
	// Tool is read, write, edit, glob or grep; ToolCallID the call.
	Tool, ToolCallID string
	// Operation is FileOperationRead or FileOperationWrite. read, glob and
	// grep read; write writes; edit reads and writes, so the hook is called
	// twice for it, read first. glob and grep are asked about their search
	// directory, not about each file under it.
	Operation string
	// Path is the file or directory, relative to the session workspace,
	// cleaned and slash-separated ("." is the workspace itself); when Mount
	// is set it is relative to that mount's directory instead. A call whose
	// path is outside the workspace and the session's mounts fails without
	// asking the hook.
	Path string
	// Mount names the session's mounted directory Path is in: "uploads",
	// "skills" or "outputs" (see ACPAgentOptions.SessionFilesRoot). Empty for
	// the workspace. A hook that guards the workspace only should treat a
	// request with a Mount as outside its scope or check it explicitly.
	Mount string
}

// FileAccessFunc is CoreOptions.FileAccess.
type FileAccessFunc func(ctx context.Context, req FileAccessRequest) error

type fileAccessCheckedKey struct{}

// fileAccessChecked marks a ctx whose call already passed FileAccess (the
// LocalToolRoute asks before it records the call as started).
func fileAccessChecked(ctx context.Context) context.Context {
	return context.WithValue(ctx, fileAccessCheckedKey{}, true)
}

// checkFileAccess asks CoreOptions.FileAccess about a file tool call and
// returns its first refusal. It does nothing without a hook, for another
// tool, for a ctx already checked or for a path outside the session's
// directories.
func (c *Core) checkFileAccess(ctx context.Context, session *CoreSession, agentSessionID, callID, tool string, input ToolInput) error {
	if c.opts.FileAccess == nil || ctx.Value(fileAccessCheckedKey{}) != nil {
		return nil
	}
	var operations []string
	switch tool {
	case ToolNameRead, ToolNameGlob, ToolNameGrep:
		operations = []string{FileOperationRead}
	case ToolNameWrite:
		operations = []string{FileOperationWrite}
	case ToolNameEdit:
		operations = []string{FileOperationRead, FileOperationWrite}
	default:
		return nil
	}
	for _, operation := range operations {
		t, err := session.target(input.Path, operation == FileOperationWrite)
		if err != nil {
			return nil // outside the session's directories (or read-only): the tool itself refuses
		}
		req := FileAccessRequest{SessionID: session.ID(), AgentSessionID: agentSessionID, Tool: tool, ToolCallID: callID,
			Operation: operation, Path: filepath.ToSlash(t.rel)}
		if t.mount != nil {
			req.Mount = t.mount.name
		}
		if err := c.opts.FileAccess(ctx, req); err != nil {
			return &fileAccessError{err}
		}
	}
	return nil
}

// fileAccessError is a FileAccess hook's refusal.
type fileAccessError struct{ err error }

func (e *fileAccessError) Error() string { return e.err.Error() }
func (e *fileAccessError) Unwrap() error { return e.err }

// fileAccessRefusal reports whether err is a FileAccess refusal, with the
// code (data.reason, else permission_denied) and detail a tool result carries.
func fileAccessRefusal(err error) (code, detail string, ok bool) {
	var refusal *fileAccessError
	if !errors.As(err, &refusal) {
		return "", "", false
	}
	code, detail = "permission_denied", refusal.err.Error()
	var rpcErr *RPCError
	if errors.As(refusal.err, &rpcErr) {
		detail = rpcErr.Message
		if rpcErr.Data != nil && rpcErr.Data.Reason != "" {
			code = rpcErr.Data.Reason
		}
	}
	return code, detail, true
}

// refusalError turns a FileAccess refusal into the "<code>: <message>" error
// a failed tool result carries.
func refusalError(err error) error {
	if code, detail, ok := fileAccessRefusal(err); ok {
		return fmt.Errorf("%s: %s", code, detail)
	}
	return err
}
