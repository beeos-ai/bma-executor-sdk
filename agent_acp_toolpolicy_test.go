package bmaexecutor

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ACPAgentOptions.ToolPolicy is the host's ceiling: the platform's tool_policy
// and the permission mode can only make a tool stricter.

func TestACPCapPolicyTakesTheStricter(t *testing.T) {
	levels := []string{"allow", "ask", "deny"}
	for i, turn := range levels {
		for j, ceiling := range levels {
			want := levels[max(i, j)]
			got := acpCapPolicy(map[string]string{"bash": turn}, map[string]string{"bash": ceiling})
			if got["bash"] != want {
				t.Errorf("turn %s, ceiling %s: %s, want %s", turn, ceiling, got["bash"], want)
			}
		}
	}
	// A ceiling over a tool the turn does not name applies; other tools are not capped.
	got := acpCapPolicy(map[string]string{"read": "allow", "bash": "ask"}, map[string]string{"write": "deny", "bash": "allow"})
	if got["write"] != "deny" || got["bash"] != "ask" || got["read"] != "allow" {
		t.Fatalf("policy = %v", got)
	}
	// No ceiling changes nothing.
	if got := acpCapPolicy(map[string]string{"bash": "allow"}, nil); got["bash"] != "allow" {
		t.Fatalf("policy = %v", got)
	}
}

// The ceiling sits on top of the mode's defaults and the platform's overrides.
func TestACPCapPolicyOverModeAndOverrides(t *testing.T) {
	ceiling := map[string]string{"write": "ask", "edit": "deny", "read": "ask", "bash": "allow"}
	got := acpCapPolicy(acpEffectivePolicy("accept_edits", map[string]string{"bash": "deny", "grep": "ask"}), ceiling)
	want := map[string]string{"write": "ask", "edit": "deny", "read": "ask", "bash": "deny", "glob": "allow", "grep": "ask"}
	for tool, permission := range want {
		if got[tool] != permission {
			t.Errorf("%s = %q, want %q", tool, got[tool], permission)
		}
	}
}

func TestNewACPAgentChecksToolPolicy(t *testing.T) {
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	noErr(t, err)
	defer core.Close()
	for name, policy := range map[string]map[string]string{
		"value":    {"bash": "maybe"},
		"empty":    {"bash": ""},
		"no tool":  {"": "deny"},
		"one good": {"read": "allow", "write": "Deny"},
	} {
		_, err := NewACPAgent(context.Background(), ACPAgentOptions{Command: []string{"/bin/false"}, Core: core, StateDir: t.TempDir(),
			Upstream: ACPModelUpstream{BaseURL: "http://127.0.0.1:1/v1"}, ToolPolicy: policy})
		if err == nil || !strings.Contains(err.Error(), "ToolPolicy") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A tool the platform allows is refused by the executor when the host denied
// it, and one the host only asks about still needs the user's approval.
func TestACPAgentHostToolPolicyCapsThePlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripted bash command is POSIX shell")
	}
	for ceiling, wantCode := range map[string]string{"deny": "permission_denied", "ask": "approval_required"} {
		t.Run(ceiling, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			f := startACPFixture(t, ctx, ACPAgentOptions{ToolPolicy: map[string]string{"bash": ceiling}})
			f.start(t, ctx, "sesn_hp", "turn_1", "bypass", func(p *TurnStartParams) { p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`} })
			got := reported(t, f, "turn_1")
			if len(got) != 1 || !strings.HasPrefix(got[0], wantCode+": ") || !strings.HasSuffix(got[0], "|true|not_forwarded|"+wantCode) {
				t.Fatalf("answer = %q", got)
			}
			assertNotRun(t, f, "sesn_hp", "pwned.txt")
		})
	}
}
