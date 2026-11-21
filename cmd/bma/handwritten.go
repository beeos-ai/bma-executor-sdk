//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// The hand-written API commands: what a generated command cannot do (event
// streams, multipart uploads) and conveniences over generated ones. A
// command that serves operations names them in its "operation" annotation
// (comma separated); the parity test holds every operation to exactly one
// command.

const (
	filesPath         = "/v1/files"
	fileContentPath   = "/v1/files/{file_id}/content"
	skillsPath        = "/v1/skills"
	skillVersionsPath = "/v1/skills/{skill_id}/versions"
)

// operationOf is the id of the operation at method and path, "" when the
// document has none.
func operationOf(method, path string) string {
	for _, op := range apiOperations {
		if op.Method == method && op.Path == path {
			return op.ID
		}
	}
	return ""
}

func joinOperations(ids ...string) string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return strings.Join(out, ",")
}

func addHandWritten(root *cobra.Command) {
	sessions := group(root, []string{"sessions"})
	sessions.AddCommand(sendCmd())
	group(root, []string{"sessions", "events"}).AddCommand(streamCmd())
	files := group(root, []string{"files"})
	files.AddCommand(uploadCmd())
	// `bma files download` is the generated file content command.
	if id := operationOf("GET", fileContentPath); id != "" {
		for _, c := range files.Commands() {
			if c.Annotations["operation"] == id {
				c.Aliases = append(c.Aliases, "download")
			}
		}
	}
	group(root, []string{"skills"}).AddCommand(pushCmd())
}

func uploadCmd() *cobra.Command {
	var expires int64
	cmd := &cobra.Command{
		Use:         "upload <path>",
		Short:       "Upload a file",
		Long:        "Upload a file and print its metadata.\n\nPOST " + filesPath,
		Args:        args(1),
		Annotations: map[string]string{"operation": operationOf("POST", filesPath)},
		RunE: func(cmd *cobra.Command, a []string) error {
			s, err := apiSession()
			if err != nil {
				return err
			}
			f, err := os.Open(a[0])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			params := anthropic.BetaFileUploadParams{File: anthropic.File(f, filepath.Base(a[0]), "")}
			if expires > 0 {
				params.ExpiresInSeconds = anthropic.Int(expires)
			}
			meta, err := s.client.Beta.Files.Upload(cmd.Context(), params)
			if err != nil {
				return err
			}
			return printRaw(meta.RawJSON())
		},
	}
	cmd.Flags().Int64Var(&expires, "expires-in-seconds", 0, "seconds until the file expires (3600–7776000)")
	return cmd
}

func printRaw(raw string) error {
	v, err := decodeJSON([]byte(raw))
	if err != nil {
		return err
	}
	return printValue(v, false)
}

func pushCmd() *cobra.Command {
	var skillID, displayName string
	cmd := &cobra.Command{
		Use:   "push <dir>",
		Short: "Upload a skill directory (with SKILL.md at its root) as a new skill or, with --skill, a new version",
		Long: "Upload every file under <dir> (SKILL.md at its root) as a new skill, or with --skill as a new " +
			"version of that skill; print the skill or the version.\n\nPOST " + skillsPath + "\nPOST " + skillVersionsPath,
		Args: args(1),
		Annotations: map[string]string{"operation": joinOperations(operationOf("POST", skillsPath),
			operationOf("POST", skillVersionsPath))},
		RunE: func(cmd *cobra.Command, a []string) error {
			files, closeAll, err := skillFiles(a[0])
			if err != nil {
				return err
			}
			defer closeAll()
			s, err := apiSession()
			if err != nil {
				return err
			}
			if skillID != "" {
				v, err := s.client.Beta.Skills.Versions.New(cmd.Context(), skillID,
					anthropic.BetaSkillVersionNewParams{Files: files})
				if err != nil {
					return err
				}
				return printRaw(v.RawJSON())
			}
			params := anthropic.BetaSkillNewParams{Files: files}
			if displayName != "" {
				params.DisplayName = anthropic.String(displayName)
			}
			sk, err := s.client.Beta.Skills.New(cmd.Context(), params)
			if err != nil {
				return err
			}
			return printRaw(sk.RawJSON())
		},
	}
	cmd.Flags().StringVar(&skillID, "skill", "", "upload a new version of this skill")
	cmd.Flags().StringVar(&displayName, "display-name", "", "the new skill's name (default: SKILL.md's name)")
	return cmd
}

// skillFiles opens every regular file under dir, named <dir name>/<path>
// (the API wants one top-level directory with SKILL.md in it).
func skillFiles(dir string) ([]io.Reader, func(), error) {
	dir = filepath.Clean(dir)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return nil, nil, usageError(dir + " has no SKILL.md at its root")
	}
	top := filepath.Base(dir)
	var files []io.Reader
	var opened []*os.File
	closeAll := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		opened = append(opened, f)
		files = append(files, anthropic.File(f, filepath.ToSlash(filepath.Join(top, rel)), ""))
		return nil
	})
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return files, closeAll, nil
}

func docsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docs <dir>",
		Short: "Write the command reference as Markdown, one file per command",
		Args:  args(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			if err := os.MkdirAll(a[0], 0o755); err != nil {
				return err
			}
			root := cmd.Root()
			root.DisableAutoGenTag = true
			if err := doc.GenMarkdownTree(root, a[0]); err != nil {
				return errors.Join(fmt.Errorf("writing the reference to %s", a[0]), err)
			}
			return nil
		},
	}
}
