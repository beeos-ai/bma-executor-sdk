package bmaexecutor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func tokenServer(t *testing.T, path *string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*path = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
			ExecutorID: "exec_test", Token: "jwt", ExpiresIn: 300, WSURL: "ws://" + r.Host + "/channel",
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// The token exchange hangs off the executor's own resource (ADR-0023 1.3):
// POST /v1/machines/{machine_id}/token for a machine daemon token and
// POST /v1/environments/{environment_id}/token for an environment key. A
// machine token without a machine id goes to the machine token alias
// POST /api/v1/executors/token: POST /v1/executors/token is the environment
// key alias, which answers a machine token 401 (ADR-0023 3.3).
func TestTokenExchangePathFollowsTheResource(t *testing.T) {
	for name, tc := range map[string]struct {
		environmentID string
		machineID     string
		env           string
		want          string
	}{
		"machine option":       {machineID: "mach_1", want: "/v1/machines/mach_1/token"},
		"machine from env":     {env: "mach_env", want: "/v1/machines/mach_env/token"},
		"machine id escaped":   {machineID: "a/b", want: "/v1/machines/a%2Fb/token"},
		"environment":          {environmentID: "env_test", want: "/v1/environments/env_test/token"},
		"environment wins env": {environmentID: "env_test", env: "mach_env", want: "/v1/environments/env_test/token"},
		"legacy machine":       {want: "/api/v1/executors/token"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(MachineIDEnv, tc.env)
			var got string
			server := tokenServer(t, &got)
			client, err := NewClient(Options{
				BaseURL: server.URL, EnvironmentKey: "k", EnvironmentID: tc.environmentID, MachineID: tc.machineID, Executor: ExecutorInfo{Name: "t"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.fetchToken(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("token request path = %q, want %q", got, tc.want)
			}
		})
	}
}

// EnvironmentID and MachineID are alternatives; BMA_MACHINE_ID in the
// environment never conflicts with an explicit EnvironmentID.
func TestEnvironmentIDAndMachineIDAreExclusive(t *testing.T) {
	_, err := NewClient(Options{EnvironmentKey: "k", EnvironmentID: "env_1", MachineID: "mach_1", Executor: ExecutorInfo{Name: "t"}})
	if err == nil || !strings.Contains(err.Error(), "EnvironmentID and MachineID") {
		t.Fatalf("NewClient with both ids: err = %v", err)
	}
}
