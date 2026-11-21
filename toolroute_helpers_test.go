//go:build !windows

package bmaexecutor

import (
	"encoding/json"
	"github.com/beeos-ai/bma-executor-sdk/conformance/gatewaystub"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Helpers of the tests that drive a route directly. The conformance cases
// themselves live in package conformance (run by toolroute_conformance_test.go).

const (
	rigSession = "sesn_t1"
	rigTurn    = "turn_1"
)

// wireInput is the input as the brain's JSON decoder hands it over (numbers are float64).
func wireInput(input map[string]any) map[string]any {
	raw, _ := json.Marshal(input)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func bashInput(command string) map[string]any { return map[string]any{"command": command} }

func fileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func wantRefused(t *testing.T, got *RouteResult, code string) {
	t.Helper()
	if got.Outcome != RouteOutcomeNotForwarded || got.Code != code || !got.IsError || !strings.HasPrefix(got.Text, code+": ") {
		t.Fatalf("answer = %+v, want not_forwarded %s", got, code)
	}
}

func wantExecuted(t *testing.T, got *RouteResult) {
	t.Helper()
	if got.Outcome != RouteOutcomeExecuted || got.Code != "" || got.IsError {
		t.Fatalf("answer = %+v, want executed without error", got)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newGateway(t *testing.T, policy map[string]string) *gatewaystub.Gateway {
	t.Helper()
	return gatewaystub.New(t, gatewaystub.Options{Policy: policy})
}
