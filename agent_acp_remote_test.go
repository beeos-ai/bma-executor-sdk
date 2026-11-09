//go:build !windows

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A turn whose turn.start metadata carries tools_mcp_* runs its tools on the
// platform's gateway (the hands are elsewhere); any other turn runs them
// here. The turn never falls back to this machine.

func remoteTurn(url, token string, extra ...func(*TurnStartParams)) func(*TurnStartParams) {
	return func(p *TurnStartParams) {
		p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`}
		if url != "" {
			p.Metadata[metaToolsURL] = url
		}
		if token != "" {
			p.Metadata[metaToolsToken] = token
		}
		p.Config.Cwd = "/hands/work"
		p.Config.EnvironmentInfo = &EnvironmentInfo{OS: "linux", Arch: "amd64", Shell: "bash", Cwd: "/hands/work"}
		for _, e := range extra {
			e(p)
		}
	}
}

func lastMessage(t *testing.T, f *acpFixture, turn string) string {
	t.Helper()
	var text string
	for _, e := range eventsOf(f.platform.snapshot(), turn) {
		if m := eventJSON(t, e); m["type"] == AgentEventTypeAgentMessage {
			text = m["content"].([]any)[0].(map[string]any)["text"].(string)
		}
	}
	return text
}

func TestACPAgentRunsAPlatformToolTurnOnTheGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	f := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true})
	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.PlatformCredentials) != 1 || capability.PlatformCredentials[0] != PlatformCredentialBrainTools {
		t.Fatalf("platform_credentials = %+v, %v", capability, err)
	}
	f.start(t, ctx, "sesn_r", "turn_1", "bypass", remoteTurn(gw.server.URL, "tok-1"))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := lastMessage(t, f, "turn_1"); !strings.HasPrefix(got, "exit_code: 0|false|executed|") {
		t.Fatalf("answer = %q", got)
	}
	if _, err := os.Stat(filepath.Join(gw.workspace, "pwned.txt")); err != nil {
		t.Fatalf("the tool did not run on the gateway's machine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_r", "pwned.txt")); err == nil {
		t.Fatal("the tool also ran on this machine")
	}
	// The hands' working directory is not created here, and is told to the model.
	if _, err := os.Stat("/hands/work"); err == nil {
		t.Fatal("the hands' directory exists on this machine")
	}
}

func TestACPAgentRemoteTurnNeverFallsBackToThisMachine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gw := newStubGateway(t, map[string]string{"bash": "allow"})
	f := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true})

	// A refused credential: stale_turn, nothing ran anywhere.
	f.start(t, ctx, "sesn_r", "turn_1", "bypass", remoteTurn(gw.server.URL, "tok-unknown"))
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := lastMessage(t, f, "turn_1"); !strings.Contains(got, "|true|not_forwarded|stale_turn") {
		t.Fatalf("answer = %q", got)
	}
	// A gateway that cannot be reached: not_forwarded, not run here.
	gw.server.Close()
	f.start(t, ctx, "sesn_r", "turn_2", "bypass", remoteTurn(gw.server.URL, "tok-1"))
	assertIdle(t, f.platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if got := lastMessage(t, f, "turn_2"); !strings.Contains(got, "|true|not_forwarded|tools_unavailable") {
		t.Fatalf("answer = %q", got)
	}
	for _, dir := range []string{filepath.Join(f.root, "acp", "sesn_r"), gw.workspace} {
		if _, err := os.Stat(filepath.Join(dir, "pwned.txt")); err == nil {
			t.Fatalf("a refused call ran in %s", dir)
		}
	}
}

func TestACPAgentRemoteTurnNeedsUrlAndToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true})
	for i, c := range [][2]string{{"http://127.0.0.1:1/mcp", ""}, {"", "tok"}} {
		turn := "turn_" + string(rune('a'+i))
		f.start(t, ctx, "sesn_r", turn, "bypass", remoteTurn(c[0], c[1]))
		assertIdle(t, f.platform.waitIdle(t, turn), TurnOutcomeFailed)
	}
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_r", "pwned.txt")); err == nil {
		t.Fatal("a turn with half a credential ran a tool here")
	}
}

func TestACPAgentLocalTurnRunsHere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{PlatformTools: true})
	f.start(t, ctx, "sesn_l", "turn_1", "bypass", func(p *TurnStartParams) { p.Metadata = map[string]string{"tool_policy": `{"bash":"allow"}`} })
	assertIdle(t, f.platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if _, err := os.Stat(filepath.Join(f.root, "acp", "sesn_l", "pwned.txt")); err != nil {
		t.Fatalf("the local turn did not run here: %v", err)
	}
}
