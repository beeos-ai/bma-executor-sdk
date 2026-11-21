//go:build darwin || linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// The BYO executor commands. up, status, harness, disconnect and run keep
// their own flag parsing (flag package); upgrade and uninstall manage the
// installation itself.

// byoCommand wraps a command that parses its own arguments.
func byoCommand(use, short string, run func([]string) error) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, a []string) error {
			err := run(a)
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			var silent exitCode
			if err != nil && !errors.As(err, &silent) && strings.HasPrefix(err.Error(), "usage:") {
				return usageError(err.Error())
			}
			return err
		},
	}
}

func byoCmds() []*cobra.Command {
	run := byoCommand("run", "Run the executor in the foreground (the service's entry)", cmdRun)
	run.Hidden = true
	return []*cobra.Command{
		byoCommand("up [--api URL] [--labels a,b] [--harness id,...] [--remote-brain] [--reauth] [--no-browser]",
			"Connect this computer (sign in through the dashboard, prepare harnesses, start the service)", cmdUp),
		byoCommand("status [--json]", "Show the connection, the service and the harnesses", cmdStatus),
		byoCommand("harness list | add <id> | login <id>", "List, enable or sign in to coding harnesses", cmdHarness),
		byoCommand("disconnect", "Stop the service and remove this computer from BMA", cmdDisconnect),
		run, upgradeCmd(), uninstallCmd(),
	}
}

// releaseBase is where the releases of bma are (BMA_RELEASE_BASE overrides
// it, for tests).
func releaseBase() string {
	if b := os.Getenv("BMA_RELEASE_BASE"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://github.com/beeos-ai/bma-executor-sdk/releases/latest/download"
}

// manifest is bma-manifest.json of a release (tools/package_bma.sh).
type manifest struct {
	Version   string            `json:"version"`
	Artifacts map[string]string `json:"artifacts"`
}

func archiveName() string { return "bma-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz" }

// homebrewManaged: the binary lives in a Homebrew keg, which only brew updates.
func homebrewManaged(exe string) bool { return strings.Contains(exe, "/Cellar/") }

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func fetch(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "bma/"+version)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

func latestManifest(ctx context.Context) (manifest, error) {
	var m manifest
	resp, err := fetch(ctx, releaseBase()+"/bma-manifest.json")
	if err != nil {
		return m, err
	}
	defer func() { _ = resp.Body.Close() }()
	return m, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m)
}

// downloadBinary fetches the archive of this platform, checks it against
// the manifest's sha256 and writes its bma binary to dest.
func downloadBinary(ctx context.Context, m manifest, dest string) error {
	want := m.Artifacts[archiveName()]
	if want == "" {
		return fmt.Errorf("release %s has no %s", m.Version, archiveName())
	}
	resp, err := fetch(ctx, releaseBase()+"/"+archiveName())
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	tmp, err := os.CreateTemp("", "bma-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()); _ = tmp.Close() }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s sha256 %s, the manifest says %s: refused", archiveName(), got, want)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(tmp)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s holds no bma binary", archiveName())
		}
		if err != nil {
			return err
		}
		if hdr.Name != "bma" {
			continue
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
}

func upgradeCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace bma with the latest release (verified against its manifest) and restart the service",
		Args:  args(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			m, err := latestManifest(ctx)
			if err != nil {
				return err
			}
			if m.Version == version {
				fmt.Fprintf(os.Stderr, "✓ bma %s is the latest release\n", version)
				return nil
			}
			if check {
				fmt.Fprintf(os.Stderr, "bma %s is available (this is %s): run `bma upgrade`\n", m.Version, version)
				return nil
			}
			exe, err := selfPath()
			if err != nil {
				return err
			}
			if homebrewManaged(exe) {
				return errors.New("bma was installed with Homebrew: run `brew upgrade bma`")
			}
			next := exe + ".new"
			if err := downloadBinary(ctx, m, next); err != nil {
				_ = os.Remove(next)
				return err
			}
			if err := os.Rename(next, exe); err != nil {
				_ = os.Remove(next)
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ bma %s → %s (%s)\n", version, m.Version, exe)
			if err := restartIfInstalled(); err != nil {
				return fmt.Errorf("restarting the service: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether a newer release exists")
	return cmd
}

func uninstallCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Disconnect this computer, remove the service, ~/.bma, the CLI profiles and bma itself",
		Args:  args(0),
		RunE: func(*cobra.Command, []string) error {
			exe, err := selfPath()
			if err != nil {
				return err
			}
			if !yes {
				if !interactive() {
					return usageError("uninstall asks for confirmation: pass --yes")
				}
				if !ask("Disconnect this computer and remove bma, its service, ~/.bma and ~/.config/bma?", false) {
					return exitCode(1)
				}
			}
			c, err := loadConfig()
			if err != nil {
				return err
			}
			if c.MachineID != "" {
				if err := cmdDisconnect(nil); err != nil {
					return fmt.Errorf("%w\n(bma is still installed: fix that, or delete the computer in the dashboard, then run `bma uninstall` again)", err)
				}
			}
			if err := removeService(); err != nil {
				return err
			}
			if useCLIKeychain() {
				if cfg, err := loadCLIConfig(); err == nil {
					for name := range cfg.Profiles {
						_ = deleteCredential(name)
					}
				}
			}
			for _, dir := range []string{home(), cliDir()} {
				if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			if homebrewManaged(exe) {
				fmt.Fprintln(os.Stderr, "✓ Disconnected and cleaned up; remove the binary with `brew uninstall bma`")
				return nil
			}
			// The install.sh layout keeps bma in ~/.bma/bin: removing ~/.bma took it already.
			if err := os.Remove(exe); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ bma removed (%s); delete the PATH line for ~/.bma/bin from your shell profile\n", exe)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}
