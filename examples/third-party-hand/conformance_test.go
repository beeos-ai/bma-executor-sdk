//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
	"github.com/beeos-ai/bma-executor-sdk/conformance"
	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
)

// The hand run against the tool route conformance suite (T1–T11), behind the
// contract double of the platform's tool gateway: whatever brain, wherever it
// runs, a call that passed the gateway is run by this hand's Core as the
// gateway expects (the same results, refusals and process handling as the SDK's
// own local route).
func TestHandPassesTheToolRouteConformanceSuite(t *testing.T) {
	conformance.RunToolRouteConformance(t, conformance.PlatformFactory(func(t *testing.T, turn context.Context) (gatewaystub.Hands, string) {
		workRoot := t.TempDir()
		h, err := newHand(bmaexecutor.Options{EnvironmentID: "env_local", TokenSource: bmaexecutor.StaticToken("ws://127.0.0.1:1", "")}, workRoot)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.close)
		workspace := filepath.Join(workRoot, "sessions", strings.ReplaceAll(t.Name(), "/", "_"))
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		return conformance.CoreHands(t, turn, h.core, workspace), workspace
	}))
}
