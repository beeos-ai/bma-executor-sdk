package bmaexecutor

import (
	"bufio"
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
	p = filepath.Clean(p)
	roots := []string{workspace}
	if real, err := filepath.EvalSymlinks(workspace); err == nil && real != workspace {
		roots = append(roots, real)
	}
	for _, root := range roots {
		if rel, err := filepath.Rel(root, p); err == nil && filepath.IsLocal(rel) {
			return rel, nil
		}
	}
	return "", outside
}

// openRoot opens the workspace as a root; the caller closes it.
func (s *CoreSession) openRoot() (*os.Root, error) {
	return os.OpenRoot(s.workspaceDir())
}

// withRoot runs fn with the workspace root and the workspace-relative form of
// path.
func (s *CoreSession) withRoot(p string, fn func(root *os.Root, rel string) error) error {
	rel, err := s.relPath(p)
	if err != nil {
		return err
	}
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root, rel)
}

func (c *Core) read(session *CoreSession, input ToolInput) (ToolResultParams, error) {
	var result ToolResultParams
	err := session.withRoot(input.Path, func(root *os.Root, rel string) error {
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
	err := session.withRoot(input.Path, func(root *os.Root, rel string) error {
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
	err = session.withRoot(input.Path, func(root *os.Root, rel string) error {
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
	base, err := session.relPath(input.Path)
	if err != nil {
		return ToolResultParams{}, err
	}
	pattern := filepath.Join(base, input.Pattern)
	if !filepath.IsLocal(pattern) {
		return ToolResultParams{}, fmt.Errorf("path %q is outside the session workspace", filepath.Join(input.Path, input.Pattern))
	}
	var matches []string
	err = session.withRoot(".", func(root *os.Root, _ string) error {
		found, err := fs.Glob(root.FS(), filepath.ToSlash(pattern))
		for _, match := range found {
			matches = append(matches, filepath.FromSlash(match))
		}
		return err
	})
	if err != nil {
		return ToolResultParams{}, err
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
	var matches []string
	err = session.withRoot(input.Path, func(root *os.Root, start string) error {
		fsys := root.FS()
		return fs.WalkDir(fsys, filepath.ToSlash(start), func(p string, entry fs.DirEntry, err error) error {
			if err != nil && p == filepath.ToSlash(start) {
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
			// Opening goes through the root: a link out of the workspace fails.
			file, err := fsys.Open(p)
			if err != nil {
				return nil
			}
			defer file.Close()
			rel := filepath.FromSlash(p)
			scanner := bufio.NewScanner(file)
			for line := 1; scanner.Scan() && len(matches) < maxGrepMatches; line++ {
				if expression.Match(scanner.Bytes()) {
					matches = append(matches, fmt.Sprintf("%s:%d:%s", rel, line, scanner.Text()))
				}
			}
			return nil
		})
	})
	if err != nil {
		return ToolResultParams{}, err
	}
	return textResult(false, strings.Join(matches, "\n")), nil
}
