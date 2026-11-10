//go:build !windows

package bmaexecutor_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
	"github.com/beeos-ai/bma-executor-sdk/conformance"
	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
)

// The tool route conformance suite (ADR-0029 §5, T1–T11) against both routes
// of the SDK. The suite lives in package conformance so that a brain or hands
// of your own can run the same cases (see examples/third-party-brain and
// examples/third-party-hand).

func TestToolRouteConformance(t *testing.T) {
	t.Run("local", func(t *testing.T) { conformance.RunToolRouteConformance(t, newLocalRig) })
	t.Run("platform", func(t *testing.T) { conformance.RunToolRouteConformance(t, conformance.PlatformFactory(nil)) })
}

// newLocalRig is the local route: a Core, and the turn state under a state
// directory that outlives the simulated restarts.
func newLocalRig(t *testing.T, toolPolicy map[string]string) *conformance.Rig {
	t.Helper()
	return localFactory(nil)(t, toolPolicy)
}

// localFactory builds local routes whose Cores have the Launch hook launch.
func localFactory(launch bmaexecutor.LaunchFunc) conformance.Factory {
	return func(t *testing.T, toolPolicy map[string]string) *conformance.Rig {
		t.Helper()
		return localRig(t, toolPolicy, launch)
	}
}

func localRig(t *testing.T, toolPolicy map[string]string, launch bmaexecutor.LaunchFunc) *conformance.Rig {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()
	workspace := filepath.Join(root, "acp", conformance.SessionID)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		core   *bmaexecutor.Core
		route  *bmaexecutor.LocalToolRoute
		cancel context.CancelFunc
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		cancel()
		core.Close()
	})
	// start begins turn in a Core of "a process" (a new Core when fresh): nothing
	// in memory is shared with the Core before.
	start := func(turn string, fresh bool) {
		if fresh {
			if core != nil {
				core.Close()
			}
			var err error
			if core, err = bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: root, Launch: launch}); err != nil {
				t.Fatal(err)
			}
		}
		if cancel != nil {
			cancel()
		}
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		var err error
		route, err = bmaexecutor.NewLocalToolRoute(bmaexecutor.LocalToolRouteOptions{Core: core, StateDir: stateDir,
			SessionID: conformance.SessionID, TurnID: turn, Workspace: workspace, ToolPolicy: toolPolicy, Context: ctx})
		if err != nil {
			t.Fatal(err)
		}
	}
	start(conformance.TurnID, true)
	return &conformance.Rig{
		Workspace: workspace,
		Route:     func() bmaexecutor.ToolRoute { mu.Lock(); defer mu.Unlock(); return route },
		Approve: func(callID, tool string, input map[string]any, decision string) {
			mu.Lock()
			defer mu.Unlock()
			id := bmaexecutor.ApprovalID(conformance.SessionID, conformance.TurnID, callID)
			request := bmaexecutor.InteractionRequestParams{InteractionID: id, ToolName: tool, ToolInput: input}
			answer := bmaexecutor.InteractionResponseParams{InteractionID: id, Outcome: decision}
			if err := route.RecordApproval(callID, request, answer); err != nil {
				t.Fatal(err)
			}
		},
		MarkStarted: func(callID string) {
			mu.Lock()
			defer mu.Unlock()
			if err := bmaexecutor.MarkLocalCallStarted(route, callID); err != nil {
				t.Fatal(err)
			}
		},
		Restart:    func() { mu.Lock(); defer mu.Unlock(); start(conformance.TurnID, true) },
		Supersede:  func() { mu.Lock(); defer mu.Unlock(); start("turn_2", false) },
		CancelTurn: func() { mu.Lock(); defer mu.Unlock(); cancel() },
	}
}

// The hands' refusal text (T8) is the same on both routes.
func TestToolRouteHandsRefusalTextIsTheSame(t *testing.T) {
	var texts []string
	for _, f := range []conformance.Factory{newLocalRig, conformance.PlatformFactory(nil)} {
		rig := f(t, map[string]string{"write": "allow"})
		ctx := t.Context()
		got := rig.Route().Call(ctx, bmaexecutor.RouteCall{ID: "c1", Name: "write", Input: map[string]any{"file_path": "../escape.txt", "content": "x"}})
		texts = append(texts, got.Text)
	}
	if texts[0] != texts[1] {
		t.Fatalf("local %q, platform %q", texts[0], texts[1])
	}
}

// platformLaunchFactory is the platform route in front of hands whose Core has
// the Launch hook launch.
func platformLaunchFactory(launch bmaexecutor.LaunchFunc) conformance.Factory {
	return conformance.PlatformFactory(func(t *testing.T, turn context.Context) (gatewaystub.Hands, string) {
		t.Helper()
		root := t.TempDir()
		core, err := bmaexecutor.NewCore(bmaexecutor.CoreOptions{WorkRoot: root, Launch: launch})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(core.Close)
		workspace := filepath.Join(root, "hands")
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		return conformance.CoreHands(t, turn, core, workspace), workspace
	})
}

// launchRecorder records the requests of a Launch hook and answers with launch.
type launchRecorder struct {
	mu       sync.Mutex
	requests []bmaexecutor.LaunchRequest
	launch   func(bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error)
}

func (r *launchRecorder) hook(_ context.Context, req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return r.launch(req)
}

func (r *launchRecorder) calls() []bmaexecutor.LaunchRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func (r *launchRecorder) set(launch func(bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launch = launch
}

// A Launch hook that keeps every request (an empty LaunchSpec) changes none
// of T1–T11, on either route.
func TestToolRouteConformanceWithAPassThroughLaunch(t *testing.T) {
	pass := func(context.Context, bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
		return bmaexecutor.LaunchSpec{}, nil
	}
	t.Run("local", func(t *testing.T) { conformance.RunToolRouteConformance(t, localFactory(pass)) })
	t.Run("platform", func(t *testing.T) { conformance.RunToolRouteConformance(t, platformLaunchFactory(pass)) })
}

// CoreOptions.Launch on both routes: a bash command starts with the argv the
// hook returns, the hook sees the shell argv; a refused command does not run,
// is not_forwarded with the hook's reason, and the same call id is answered
// with that again; file tools do not call the hook.
func TestToolRouteBashGoesThroughLaunch(t *testing.T) {
	for _, route := range []struct {
		name    string
		factory func(bmaexecutor.LaunchFunc) conformance.Factory
	}{{"local", localFactory}, {"platform", platformLaunchFactory}} {
		t.Run(route.name, func(t *testing.T) {
			recorder := &launchRecorder{}
			wrap := func(req bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
				// Wrap the command the way a sandbox wrapper (sandbox-exec, bwrap) does.
				argv := append([]string{"sh", "-c", `touch launched; exec "$@"`, "wrapper"}, req.Argv...)
				return bmaexecutor.LaunchSpec{Argv: argv}, nil
			}
			recorder.set(wrap)
			rig := route.factory(recorder.hook)(t, map[string]string{"bash": "allow", "write": "allow"})
			call := func(id, name string, input map[string]any) *bmaexecutor.RouteResult {
				t.Helper()
				return rig.Route().Call(t.Context(), bmaexecutor.RouteCall{ID: id, Name: name, Input: input})
			}
			launched := filepath.Join(rig.Workspace, "launched")

			got := call("c1", "bash", map[string]any{"command": "printf hi"})
			if got.Outcome != bmaexecutor.RouteOutcomeExecuted || got.IsError || got.Text != "hi\nexit_code: 0" {
				t.Fatalf("bash = %+v", got)
			}
			if _, err := os.Stat(launched); err != nil {
				t.Fatalf("the wrapper did not run: %v", err)
			}
			calls := recorder.calls()
			if len(calls) != 1 || calls[0].Tool != "bash" || calls[0].ToolCallID == "" ||
				!slices.Equal(calls[0].Argv, []string{"bash", "-lc", "printf hi"}) && !slices.Equal(calls[0].Argv, []string{"sh", "-lc", "printf hi"}) {
				t.Fatalf("launch calls = %+v", calls)
			}

			for i, c := range []bmaexecutor.RouteCall{
				{ID: "c2", Name: "write", Input: map[string]any{"file_path": "a.txt", "content": "x"}},
				{ID: "c3", Name: "read", Input: map[string]any{"file_path": "a.txt"}},
			} {
				if got := call(c.ID, c.Name, c.Input); got.Outcome != bmaexecutor.RouteOutcomeExecuted || got.IsError {
					t.Fatalf("file tool %d = %+v", i, got)
				}
			}
			if n := len(recorder.calls()); n != 1 {
				t.Fatalf("file tools called Launch: %d calls", n)
			}

			_ = os.Remove(launched)
			recorder.set(func(bmaexecutor.LaunchRequest) (bmaexecutor.LaunchSpec, error) {
				return bmaexecutor.LaunchSpec{}, bmaexecutor.NewReasonError(bmaexecutor.CodeForbidden,
					bmaexecutor.ReasonWorkspaceBoundary, "outside the granted folder")
			})
			refused := call("c4", "bash", map[string]any{"command": "touch ran"})
			if refused.Outcome != bmaexecutor.RouteOutcomeNotForwarded || refused.Code != "workspace_boundary" || !refused.IsError ||
				!strings.HasPrefix(refused.Text, "workspace_boundary: ") {
				t.Fatalf("refused bash = %+v", refused)
			}
			if _, err := os.Stat(filepath.Join(rig.Workspace, "ran")); err == nil {
				t.Fatal("a refused command ran")
			}
			// The refusal is the hands' answer: the same call id gets it again,
			// a new call runs once the hook allows it.
			recorder.set(wrap)
			if again := call("c4", "bash", map[string]any{"command": "touch ran"}); again.Outcome != bmaexecutor.RouteOutcomeNotForwarded ||
				again.Code != "workspace_boundary" || !again.Replayed {
				t.Fatalf("repeated refused call = %+v", again)
			}
			if got := call("c5", "bash", map[string]any{"command": "touch ran"}); got.Outcome != bmaexecutor.RouteOutcomeExecuted || got.IsError {
				t.Fatalf("bash after the hook allows it = %+v", got)
			}
			if _, err := os.Stat(filepath.Join(rig.Workspace, "ran")); err != nil {
				t.Fatalf("the allowed command did not run: %v", err)
			}
		})
	}
}
