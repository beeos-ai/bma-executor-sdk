package bmaexecutor

import (
	"strings"
	"testing"
)

// The catalog's Claude Code releases are exactly the adapter's pins, so a
// node manager downloads only binaries the adapter will run.
func TestHarnessCatalogClaudeReleasesAreThePins(t *testing.T) {
	h, ok := HarnessByID("claude-code")
	if !ok || h.Adapter != "claude-cli" || h.newAdapter == nil {
		t.Fatalf("claude-code: %+v", h)
	}
	if len(h.Releases) != len(claudeCLIPins) {
		t.Fatalf("releases %d, pins %d", len(h.Releases), len(claudeCLIPins))
	}
	for i, pin := range claudeCLIPins {
		r := h.Releases[i]
		if r.Version != pin.Version || r.Binary != "claude" {
			t.Fatalf("release %d: %+v", i, r)
		}
		for platform, sum := range pin.SHA256 {
			a := r.Platforms[platform]
			if a.SHA256 != sum || !strings.HasPrefix(a.URL, "https://downloads.claude.ai/claude-code-releases/"+pin.Version+"/") {
				t.Fatalf("%s %s: %+v", pin.Version, platform, a)
			}
		}
	}
	if h.Latest().Version != claudeCLIPins[len(claudeCLIPins)-1].Version {
		t.Fatal("latest is the newest pin")
	}
	if _, ok := HarnessByID("nope"); ok {
		t.Fatal("unknown harness")
	}
}

func TestHarnessOwnerEnvironKeepsOnlyTheOwnersVariables(t *testing.T) {
	h, _ := HarnessByID("claude-code")
	got := h.OwnerEnviron([]string{"ANTHROPIC_API_KEY=x", "BMA_MACHINE_TOKEN=secret", "CLAUDE_CODE_USE_BEDROCK=1", "PATH=/bin"})
	if strings.Join(got, ",") != "ANTHROPIC_API_KEY=x,CLAUDE_CODE_USE_BEDROCK=1" {
		t.Fatalf("got %v", got)
	}
}
