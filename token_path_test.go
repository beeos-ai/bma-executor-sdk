package bmaexecutor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The token exchange of a machine-scoped executor (machine daemon token, no
// environment id) is bma-api's POST /api/v1/executors/token; an environment
// key goes to the managed-agents POST /v1/executors/token (protocol
// reference §2.1). The managed-agents facade answers a machine token 404.
func TestTokenExchangePathFollowsTheCredential(t *testing.T) {
	for name, tc := range map[string]struct {
		environmentID string
		want          string
	}{
		"machine":     {"", "/api/v1/executors/token"},
		"environment": {"env_test", "/v1/executors/token"},
	} {
		t.Run(name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(ExecutorTokenResponse{
					ExecutorID: "exec_test", Token: "jwt", ExpiresIn: 300, WSURL: "ws://" + r.Host + "/channel",
				})
			}))
			defer server.Close()
			client, err := NewClient(Options{
				BaseURL: server.URL, EnvironmentKey: "k", EnvironmentID: tc.environmentID, Executor: ExecutorInfo{Name: "t"},
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
