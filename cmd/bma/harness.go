package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// Harnesses come from the SDK catalog (bmaexecutor.Harnesses): every one is
// installed, checked and logged in by the same code below, from the data of
// its catalog entry; nothing here names a harness.

// harnessPath is where a pinned release's binary lives:
// ~/.bma/harnesses/<id>/<version>/<binary>.
func harnessPath(h bmaexecutor.HarnessKind, r bmaexecutor.HarnessRelease) string {
	return filepath.Join(home(), "harnesses", h.ID, r.Version, r.Binary)
}

// installHarness makes sure the release's binary is present and matches its
// pinned sha256; a missing one is downloaded and verified before it is
// moved into place.
func installHarness(ctx context.Context, h bmaexecutor.HarnessKind, r bmaexecutor.HarnessRelease) (string, error) {
	art, err := r.Artifact()
	if err != nil {
		return "", err
	}
	dest := harnessPath(h, r)
	if sum, err := fileSHA256(dest); err == nil && sum == art.SHA256 {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	part := dest + ".part"
	defer func() { _ = os.Remove(part) }()
	if err := download(ctx, art.URL, part); err != nil {
		return "", fmt.Errorf("%s %s: %w", h.DisplayName, r.Version, err)
	}
	sum, err := fileSHA256(part)
	if err != nil {
		return "", err
	}
	if sum != art.SHA256 {
		return "", fmt.Errorf("%s %s: downloaded sha256 %s, pinned %s: refused", h.DisplayName, r.Version, sum, art.SHA256)
	}
	if err := os.Chmod(part, 0o755); err != nil {
		return "", err
	}
	return dest, os.Rename(part, dest)
}

func download(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// enabledHarness is an enabled harness with its configured release.
type enabledHarness struct {
	kind    bmaexecutor.HarnessKind
	release bmaexecutor.HarnessRelease
}

// enabled resolves the configured harnesses; a version no longer pinned
// moves to the newest pinned release.
func enabled(c config) ([]enabledHarness, error) {
	var out []enabledHarness
	for _, h := range bmaexecutor.Harnesses() {
		v, on := c.Harnesses[h.ID]
		if !on {
			continue
		}
		r, ok := h.Release(v)
		if !ok {
			r = h.Latest()
		}
		out = append(out, enabledHarness{kind: h, release: r})
	}
	for id := range c.Harnesses {
		if _, ok := bmaexecutor.HarnessByID(id); !ok {
			return nil, fmt.Errorf("harness %q is not known to this bma (%s): run `bma harness list`", id, version)
		}
	}
	return out, nil
}

// signedIn runs the harness's own login check (its exit status only; no
// credential is read).
func signedIn(ctx context.Context, h bmaexecutor.HarnessKind, exe string) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, h.AuthStatus...)
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}

// login runs the harness's interactive sign-in on this terminal, with the
// owner's own account.
func login(ctx context.Context, h bmaexecutor.HarnessKind, exe string) error {
	cmd := exec.CommandContext(ctx, exe, h.Login...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// prepareHarness installs the release and makes sure the owner is signed
// in, offering the sign-in on an interactive terminal.
func prepareHarness(ctx context.Context, e enabledHarness) error {
	exe, err := installHarness(ctx, e.kind, e.release)
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ %s %s ready (%s)\n", e.kind.DisplayName, e.release.Version, exe)
	if signedIn(ctx, e.kind, exe) {
		fmt.Printf("  ✓ %s signed in\n", e.kind.DisplayName)
		return nil
	}
	cmdline := exe + " " + strings.Join(e.kind.Login, " ")
	if !interactive() || !ask(fmt.Sprintf("  %s is not signed in. Sign in now with your own account?", e.kind.DisplayName), true) {
		fmt.Printf("  ! %s is not signed in: run `bma harness login %s` (or %s)\n", e.kind.DisplayName, e.kind.ID, cmdline)
		return nil
	}
	if err := login(ctx, e.kind, exe); err != nil {
		return fmt.Errorf("%s sign-in: %w", e.kind.DisplayName, err)
	}
	if !signedIn(ctx, e.kind, exe) {
		return errors.New(e.kind.DisplayName + " is still not signed in")
	}
	fmt.Printf("  ✓ %s signed in\n", e.kind.DisplayName)
	return nil
}

func cmdHarness(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bma harness list | add <id> | login <id>")
	}
	ctx := context.Background()
	c, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		for _, h := range bmaexecutor.Harnesses() {
			state := "off"
			if v, on := c.Harnesses[h.ID]; on {
				state = "on " + v
			}
			var versions []string
			for _, r := range h.Releases {
				versions = append(versions, r.Version)
			}
			fmt.Printf("%-14s %-14s %-8s pinned %s\n", h.ID, h.DisplayName, state, strings.Join(versions, ", "))
		}
		return nil
	case "add", "login":
		if len(args) != 2 {
			return fmt.Errorf("usage: bma harness %s <id>", args[0])
		}
		h, ok := bmaexecutor.HarnessByID(args[1])
		if !ok {
			return fmt.Errorf("unknown harness %q: see `bma harness list`", args[1])
		}
		r, ok := h.Release(c.Harnesses[h.ID])
		if !ok {
			r = h.Latest()
		}
		if args[0] == "login" {
			exe, err := installHarness(ctx, h, r)
			if err != nil {
				return err
			}
			return login(ctx, h, exe)
		}
		if err := prepareHarness(ctx, enabledHarness{kind: h, release: r}); err != nil {
			return err
		}
		c.Harnesses[h.ID] = r.Version
		if err := c.save(); err != nil {
			return err
		}
		return restartIfInstalled()
	}
	return fmt.Errorf("unknown harness command %q", args[0])
}
