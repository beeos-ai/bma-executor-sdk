package bmaexecutor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ACPAgent in a sandbox (bmad, BMA S6): the platform hands the model
// credential per turn and refreshes it, the agent runs as the workload user.

// refreshingPlatform answers model_token.refresh with refresh.
type refreshingPlatform struct {
	*recordingPlatform
	mu       sync.Mutex
	calls    []ModelTokenRefreshParams
	response func(ModelTokenRefreshParams) (ModelTokenRefreshResult, error)
}

func (p *refreshingPlatform) RefreshModelToken(_ context.Context, _ *Executor, params ModelTokenRefreshParams) (ModelTokenRefreshResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, params)
	p.mu.Unlock()
	return p.response(params)
}

func (p *refreshingPlatform) refreshes() []ModelTokenRefreshParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func platformModel(base, token string, expiresAt time.Time) func(*TurnStartParams) {
	return func(params *TurnStartParams) {
		params.Metadata = map[string]string{"model_base_url": base, "model_token": token}
		if !expiresAt.IsZero() {
			params.Metadata["model_token_expires_at"] = expiresAt.UTC().Format(time.RFC3339)
		}
	}
}

func (u *fakeUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.auth)
}

func messagesOf(t *testing.T, events []AgentEventRecord, turnID string) string {
	t.Helper()
	var out []string
	for _, e := range eventsOf(events, turnID) {
		if e.Type == AgentEventTypeAgentMessage {
			out = append(out, fmt.Sprint(eventJSON(t, e)["content"]))
		}
	}
	return strings.Join(out, "\n")
}

func TestACPAgentRefreshesThePlatformModelTokenBeforeItExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform := &refreshingPlatform{recordingPlatform: newRecordingPlatform()}
	platform.response = func(ModelTokenRefreshParams) (ModelTokenRefreshResult, error) {
		return ModelTokenRefreshResult{ModelToken: "tok_new", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, nil
	}
	f := startACPFixtureOn(t, ctx, ACPAgentOptions{}, platform)
	base := f.upstream.server.URL + "/v1"

	// 5 minutes left (within the 10-minute window): refreshed once, before the first request.
	f.start(t, ctx, "sesn_r", "turn_1", "model-twice", platformModel(base, "tok_old", time.Now().Add(5*time.Minute)))
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got := f.upstream.seen(); !slices.Equal(got, []string{"Bearer tok_new", "Bearer tok_new"}) {
		t.Fatalf("upstream auth = %v, want the refreshed token twice", got)
	}
	if calls := platform.refreshes(); len(calls) != 1 || calls[0].SessionID != "sesn_r" || calls[0].TurnID != "turn_1" {
		t.Fatalf("model_token.refresh calls = %+v", calls)
	}

	// An hour left: used as delivered, no refresh.
	f.start(t, ctx, "sesn_r", "turn_2", "model-twice", platformModel(base, "tok_far", time.Now().Add(time.Hour)))
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if got := f.upstream.seen()[2:]; !slices.Equal(got, []string{"Bearer tok_far", "Bearer tok_far"}) {
		t.Fatalf("upstream auth = %v", got)
	}
	if n := len(platform.refreshes()); n != 1 {
		t.Fatalf("model_token.refresh calls = %d, want still 1", n)
	}
}

func TestACPAgentFailedRefreshForwardsTheCurrentTokenAndBacksOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform := &refreshingPlatform{recordingPlatform: newRecordingPlatform()}
	platform.response = func(ModelTokenRefreshParams) (ModelTokenRefreshResult, error) {
		return ModelTokenRefreshResult{}, &RPCError{Code: CodeResourceUnavailable, Message: "model_token_unavailable"}
	}
	f := startACPFixtureOn(t, ctx, ACPAgentOptions{}, platform)
	f.start(t, ctx, "sesn_f", "turn_1", "model-twice", platformModel(f.upstream.server.URL+"/v1", "tok_old", time.Now().Add(time.Minute)))
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	// Never a silent substitute: the delivered token goes out (the gateway answers 401 once it expires).
	if got := f.upstream.seen(); !slices.Equal(got, []string{"Bearer tok_old", "Bearer tok_old"}) {
		t.Fatalf("upstream auth = %v", got)
	}
	// The second request is within the backoff: not asked again.
	if n := len(platform.refreshes()); n != 1 {
		t.Fatalf("model_token.refresh calls = %d, want 1", n)
	}
}

func TestACPAgentPlatformModelAsksForTheHarnessModelCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform := newRecordingPlatform()
	f := startACPFixtureOn(t, ctx, ACPAgentOptions{PlatformModel: true}, platform)
	capability, err := f.executor.AgentCapability()
	if err != nil || !slices.Equal(capability.PlatformCredentials, []string{PlatformCredentialHarnessModel}) {
		t.Fatalf("platform_credentials = %+v (%v)", capability, err)
	}
	// No node-owner upstream: a turn without the credential fails visibly.
	f.start(t, ctx, "sesn_p", "turn_1", "model")
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeFailed)
	var message string
	for _, e := range eventsOf(platform.snapshot(), "turn_1") {
		if e.Type == AgentEventTypeSessionError {
			message = eventJSON(t, e)["error"].(map[string]any)["message"].(string)
		}
	}
	if !strings.Contains(message, "model_token") {
		t.Fatalf("session.error = %q", message)
	}
	f.start(t, ctx, "sesn_p", "turn_2", "model", platformModel(f.upstream.server.URL+"/v1", "tok_turn", time.Time{}))
	assertIdle(t, platform.waitIdle(t, "turn_2"), TurnOutcomeCompleted)
	if got := f.upstream.seen(); !slices.Equal(got, []string{"Bearer tok_turn"}) {
		t.Fatalf("upstream auth = %v", got)
	}
	if !strings.Contains(messagesOf(t, platform.snapshot(), "turn_2"), "ok:200:upstream-ok") {
		t.Fatalf("messages = %s", messagesOf(t, platform.snapshot(), "turn_2"))
	}
}

func TestACPAgentWithoutPlatformModelDeclaresNoCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := startACPFixture(t, ctx, ACPAgentOptions{})
	capability, err := f.executor.AgentCapability()
	if err != nil || len(capability.PlatformCredentials) != 0 {
		t.Fatalf("platform_credentials = %+v (%v)", capability, err)
	}
	core, err := NewCore(CoreOptions{WorkRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	exe, _ := os.Executable()
	if _, err := NewACPAgent(ctx, ACPAgentOptions{Command: []string{exe}, Core: core, StateDir: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "PlatformModel") {
		t.Fatalf("neither Upstream nor PlatformModel: %v", err)
	}
}

// The agent process runs as the Core's workload identity, like the tools it calls.
func TestACPAgentRunsTheAgentAsTheCoreRunAs(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux (run in a container)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// t.TempDir's parent is private to root: the workload must reach its files.
	root, err := os.MkdirTemp("", "acp-runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The test binary's build directory is private to root: run a copy.
	exe, _ := os.Executable()
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	agentBin := filepath.Join(root, "agent")
	if err := os.WriteFile(agentBin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(home, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	core, err := NewCore(CoreOptions{WorkRoot: work, RunAs: &RunAs{UID: 65534, GID: 65534, Home: home, User: "nobody"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(core.Close)
	platform := newRecordingPlatform()
	f := startACPFixtureAtCore(t, ctx, ACPAgentOptions{Command: []string{agentBin}, Core: core}, root, platform, nil)
	f.start(t, ctx, "sesn_u", "turn_1", "whoami")
	assertIdle(t, platform.waitIdle(t, "turn_1"), TurnOutcomeCompleted)
	if got, want := messagesOf(t, platform.snapshot(), "turn_1"), "uid=65534 home="+home+" user=nobody state=writable"; !strings.Contains(got, want) {
		t.Fatalf("agent identity = %s, want %s", got, want)
	}
}
