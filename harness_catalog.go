package bmaexecutor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
)

// HarnessKind is a coding harness a node runs as its brain with the owner's
// own credentials (ADR-0066: each harness has one BrainAdapter; callers never
// branch on the harness). The catalog is the single description a node
// manager (the bma CLI, a desktop app) needs: where a pinned release comes
// from and how it is verified, how the owner's login is checked and started,
// which of this process's variables carry the owner's credentials, and the
// adapter that runs it.
type HarnessKind struct {
	// ID is the declared harness id (agent.harnesses[].id), e.g. claude-code.
	ID string
	// DisplayName is the name shown to the owner.
	DisplayName string
	// Adapter names the adapter family (claude-cli, codex-app-server, acp).
	Adapter string
	// Releases are the pinned releases, newest last; only these run.
	Releases []HarnessRelease
	// AuthStatus are the arguments that exit 0 when the owner is signed in
	// (the harness's own check; no credential is read by the SDK).
	AuthStatus []string
	// Login are the arguments that start the owner's interactive sign-in.
	Login []string
	// OwnerEnv are the prefixes of this process's variables handed to the
	// harness as they are (the owner's own credentials and settings).
	OwnerEnv []string

	newAdapter func(executable string) BrainAdapter
}

// HarnessRelease is one pinned release: its version, and per GOOS/GOARCH the
// URL of the binary and its sha256.
type HarnessRelease struct {
	Version   string
	Binary    string // file name of the executable inside the release directory
	Platforms map[string]HarnessArtifact
}

// HarnessArtifact is the binary of one release for one platform.
type HarnessArtifact struct {
	URL    string
	SHA256 string
}

// Harnesses is the catalog, in the order a node manager offers them.
func Harnesses() []HarnessKind { return slices.Clone(harnessCatalog) }

// HarnessByID finds a harness of the catalog.
func HarnessByID(id string) (HarnessKind, bool) {
	i := slices.IndexFunc(harnessCatalog, func(h HarnessKind) bool { return h.ID == id })
	if i < 0 {
		return HarnessKind{}, false
	}
	return harnessCatalog[i], true
}

// Latest is the newest pinned release.
func (h HarnessKind) Latest() HarnessRelease { return h.Releases[len(h.Releases)-1] }

// Release finds a pinned release.
func (h HarnessKind) Release(version string) (HarnessRelease, bool) {
	i := slices.IndexFunc(h.Releases, func(r HarnessRelease) bool { return r.Version == version })
	if i < 0 {
		return HarnessRelease{}, false
	}
	return h.Releases[i], true
}

// Artifact is the release's binary for this platform.
func (r HarnessRelease) Artifact() (HarnessArtifact, error) {
	a, ok := r.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return HarnessArtifact{}, fmt.Errorf("bmaexecutor: release %s has no binary for %s/%s", r.Version,
			runtime.GOOS, runtime.GOARCH)
	}
	return a, nil
}

// OwnerEnviron is the part of environ handed to the harness.
func (h HarnessKind) OwnerEnviron(environ []string) []string {
	var out []string
	for _, kv := range environ {
		for _, prefix := range h.OwnerEnv {
			if strings.HasPrefix(kv, prefix) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// NewBrain starts the harness at executable (a pinned release's binary) as
// the brain of opts.Core: the harness id is declared, the owner's variables
// of this process are handed over, and the adapter checks the binary against
// the pins before anything runs (fail-closed).
func (h HarnessKind) NewBrain(ctx context.Context, executable string, opts ACPAgentOptions) (*BrainHost, error) {
	if h.newAdapter == nil {
		return nil, errors.New("bmaexecutor: harness " + h.ID + " has no adapter")
	}
	opts.Command = []string{executable}
	opts.HarnessID = h.ID
	opts.Env = append(opts.Env, h.OwnerEnviron(os.Environ())...)
	return NewBrainHost(ctx, h.newAdapter(executable), opts)
}

var harnessCatalog = []HarnessKind{claudeCodeHarness()}

// claudeCodeHarness is the official claude CLI; its releases are the pins
// the adapter verifies (claudeCLIPins), downloaded from Anthropic's release
// bucket (the URLs the official installer uses).
func claudeCodeHarness() HarnessKind {
	h := HarnessKind{
		ID: "claude-code", DisplayName: "Claude Code", Adapter: "claude-cli",
		AuthStatus: []string{"auth", "status", "--json"},
		Login:      []string{"auth", "login"},
		OwnerEnv:   []string{"ANTHROPIC_", "CLAUDE_CODE_", "AWS_", "GOOGLE_", "CLOUD_ML_"},
		newAdapter: func(executable string) BrainAdapter {
			return NewClaudeCLIAdapter(ClaudeCLIOptions{Executable: executable})
		},
	}
	bucket := map[string]string{"darwin/arm64": "darwin-arm64", "darwin/amd64": "darwin-x64",
		"linux/arm64": "linux-arm64", "linux/amd64": "linux-x64"}
	for _, pin := range claudeCLIPins {
		r := HarnessRelease{Version: pin.Version, Binary: "claude", Platforms: map[string]HarnessArtifact{}}
		for platform, sum := range pin.SHA256 {
			r.Platforms[platform] = HarnessArtifact{SHA256: sum,
				URL: "https://downloads.claude.ai/claude-code-releases/" + pin.Version + "/" + bucket[platform] + "/claude"}
		}
		h.Releases = append(h.Releases, r)
	}
	return h
}
