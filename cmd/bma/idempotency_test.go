//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An operation that requires an Idempotency-Key gets one: --idempotency-key
// when given, else a new key per run.
func TestRequiredIdempotencyKeyIsSent(t *testing.T) {
	var keys []string
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		_, _ = w.Write([]byte(`{"id":"ses_fork"}`))
	})
	op := &apiOp{ID: "forkThing", Method: "POST", Path: "/v1/things/{thing_id}/fork", Group: []string{"things"},
		Verb: "fork", Summary: "Fork a thing", Body: true, Idempotent: true,
		Params: []apiParam{{Name: "thing_id", In: "path", Type: "string", Required: true}}}
	run := func(a ...string) int {
		t.Helper()
		t.Setenv("BMA_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
		t.Setenv("BMA_API_KEY", "bma_org_v1_k")
		t.Setenv("BMA_BASE_URL", url)
		opts = globals{}
		var out bytes.Buffer
		stdout = &out
		defer func() { stdout = os.Stdout }()
		root := newRoot()
		group(root, op.Group).AddCommand(apiCommand(op))
		root.SetArgs(a)
		if err := root.ExecuteContext(context.Background()); err != nil {
			return exitCodeOf(err)
		}
		return 0
	}
	if code := run("things", "fork", "th_1", "--idempotency-key", "mine"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := run("things", "fork", "th_1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := run("things", "fork", "th_1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(keys) != 3 || keys[0] != "mine" || !strings.HasPrefix(keys[1], "bma-cli-") || keys[1] == keys[2] {
		t.Fatalf("keys %q", keys)
	}
}
