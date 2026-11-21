//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// The CLI's profiles (~/.config/bma/config.yaml, no secrets) and their
// credentials (the login keychain on macOS, else credentials.json 0600).

const (
	defaultAPIURL  = "https://api.beeos.ai"
	defaultAuthURL = "https://dashboard-api.beeos.ai"
	cliKeychain    = "ai.beeos.bma.cli"
)

// profile is one deployment and scope the CLI talks to.
type profile struct {
	APIURL       string `yaml:"api_url,omitempty" json:"api_url,omitempty"`
	AuthURL      string `yaml:"auth_url,omitempty" json:"auth_url,omitempty"`
	Organization string `yaml:"organization,omitempty" json:"organization,omitempty"`
	Workspace    string `yaml:"workspace,omitempty" json:"workspace,omitempty"`
}

type cliConfig struct {
	Current  string             `yaml:"current,omitempty"`
	Profiles map[string]profile `yaml:"profiles"`
}

func cliDir() string {
	if d := os.Getenv("BMA_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "bma")
	}
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".config", "bma")
}

func cliConfigPath() string { return filepath.Join(cliDir(), "config.yaml") }
func credentialsPath() string {
	return filepath.Join(cliDir(), "credentials.json")
}

func loadCLIConfig() (cliConfig, error) {
	c := cliConfig{Profiles: map[string]profile{}}
	b, err := os.ReadFile(cliConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", cliConfigPath(), err)
	}
	if c.Profiles == nil {
		c.Profiles = map[string]profile{}
	}
	return c, nil
}

func (c cliConfig) save() error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFile(cliConfigPath(), b)
}

// profileName is the selected profile: --profile, BMA_PROFILE, the current
// one, else "default".
func profileName(c cliConfig) string {
	return firstNonEmpty(opts.Profile, os.Getenv("BMA_PROFILE"), c.Current, "default")
}

func currentProfile() (string, profile, error) {
	c, err := loadCLIConfig()
	if err != nil {
		return "", profile{}, err
	}
	name := profileName(c)
	return name, c.Profiles[name], nil
}

var errNotSignedIn = errors.New("not signed in: run `bma login` (or set BMA_API_KEY)")

func useCLIKeychain() bool { return runtime.GOOS == "darwin" && os.Getenv("BMA_CONFIG_DIR") == "" }

func loadCredentialFile() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(credentialsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	return m, json.Unmarshal(b, &m)
}

// saveCredential stores the credential of a profile and answers where.
func saveCredential(name, secret string) (string, error) {
	if useCLIKeychain() {
		cmd := exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %q\n", cliKeychain, name, secret))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil && stderr.Len() == 0 {
			return "keychain", nil
		}
	}
	m, err := loadCredentialFile()
	if err != nil {
		return "", err
	}
	m[name] = secret
	if err := writeJSON(credentialsPath(), m); err != nil {
		return "", err
	}
	return credentialsPath(), nil
}

// loadCredential reads the credential of a profile.
func loadCredential(name string) (string, error) {
	m, err := loadCredentialFile()
	if err != nil {
		return "", err
	}
	if s := m[name]; s != "" {
		return s, nil
	}
	if useCLIKeychain() {
		out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", cliKeychain, "-a", name, "-w").Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", errNotSignedIn
}

// credentialSource says where the credential of the command comes from.
func credentialSource(name string) string {
	switch {
	case opts.APIKey != "":
		return "--api-key"
	case os.Getenv("BMA_API_KEY") != "":
		return "BMA_API_KEY"
	}
	if m, _ := loadCredentialFile(); m[name] != "" {
		return credentialsPath()
	}
	if useCLIKeychain() && exec.Command("/usr/bin/security", "find-generic-password", "-s", cliKeychain, "-a",
		name).Run() == nil {
		return "keychain"
	}
	return "none"
}

func deleteCredential(name string) error {
	if useCLIKeychain() {
		_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", cliKeychain, "-a", name).Run()
	}
	m, err := loadCredentialFile()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	return writeJSON(credentialsPath(), m)
}

func profilesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profiles", Short: "List, select, set and delete profiles", Args: args(0),
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List the profiles", Args: args(0),
		RunE: func(*cobra.Command, []string) error {
			c, err := loadCLIConfig()
			if err != nil {
				return err
			}
			cur := profileName(c)
			names := make([]string, 0, len(c.Profiles))
			for n := range c.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			items := []any{}
			for _, n := range names {
				p := c.Profiles[n]
				items = append(items, map[string]any{"name": n, "current": n == cur,
					"api_url": firstNonEmpty(p.APIURL, defaultAPIURL), "organization": p.Organization,
					"workspace": p.Workspace})
			}
			if format(true) == "table" {
				for _, it := range items {
					m := it.(map[string]any)
					mark := " "
					if m["current"].(bool) {
						mark = "*"
					}
					fmt.Fprintf(stdout, "%s %-16s %s  org %s  workspace %s\n", mark, m["name"], m["api_url"],
						orDash(m["organization"].(string)), orDash(m["workspace"].(string)))
				}
				return nil
			}
			return printValue(items, true)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "use <name>", Short: "Make a profile the current one", Args: args(1),
		RunE: func(_ *cobra.Command, a []string) error {
			c, err := loadCLIConfig()
			if err != nil {
				return err
			}
			if _, ok := c.Profiles[a[0]]; !ok {
				return fmt.Errorf("no profile %q (see `bma profiles list`)", a[0])
			}
			c.Current = a[0]
			return c.save()
		},
	})
	var set profile
	setCmd := &cobra.Command{
		Use: "set <name>", Short: "Create or change a profile (only the flags given)", Args: args(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			c, err := loadCLIConfig()
			if err != nil {
				return err
			}
			p := c.Profiles[a[0]]
			f := cmd.Flags()
			if f.Changed("api") {
				p.APIURL = strings.TrimRight(set.APIURL, "/")
			}
			if f.Changed("auth") {
				p.AuthURL = strings.TrimRight(set.AuthURL, "/")
			}
			if f.Changed("organization") {
				p.Organization = set.Organization
			}
			if f.Changed("workspace-id") {
				p.Workspace = set.Workspace
			}
			c.Profiles[a[0]] = p
			if c.Current == "" {
				c.Current = a[0]
			}
			return c.save()
		},
	}
	setCmd.Flags().StringVar(&set.APIURL, "api", "", "API origin, e.g. "+defaultAPIURL)
	setCmd.Flags().StringVar(&set.AuthURL, "auth", "", "dashboard API origin `bma login` signs in through, e.g. "+defaultAuthURL)
	setCmd.Flags().StringVar(&set.Organization, "organization", "", "organization id (org_...)")
	setCmd.Flags().StringVar(&set.Workspace, "workspace-id", "", "workspace id")
	cmd.AddCommand(setCmd)
	cmd.AddCommand(&cobra.Command{
		Use: "delete <name>", Short: "Delete a profile and its credential", Args: args(1),
		RunE: func(_ *cobra.Command, a []string) error {
			c, err := loadCLIConfig()
			if err != nil {
				return err
			}
			delete(c.Profiles, a[0])
			if c.Current == a[0] {
				c.Current = ""
			}
			if err := deleteCredential(a[0]); err != nil {
				return err
			}
			return c.save()
		},
	})
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
