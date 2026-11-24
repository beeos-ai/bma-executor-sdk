//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `bma search <query>` and `bma fetch <url>` post the operation's body (the
// argument as query / url, the other fields as flags) with an
// Idempotency-Key, and print the answer.
func TestSearchAndFetchCommands(t *testing.T) {
	type call struct{ path, body, key string }
	var calls []call
	url := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.URL.Path, string(b), r.Header.Get("Idempotency-Key")})
		_, _ = w.Write([]byte(`{"id":"wsrch_1","type":"web_search"}`))
	})
	run := func(a ...string) (int, string) {
		t.Helper()
		t.Setenv("BMA_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
		t.Setenv("BMA_API_KEY", "bma_org_v1_k")
		t.Setenv("BMA_BASE_URL", url)
		opts = globals{}
		var out bytes.Buffer
		stdout = &out
		defer func() { stdout = os.Stdout }()
		root := newRoot()
		root.SetArgs(a)
		if err := root.ExecuteContext(context.Background()); err != nil {
			return exitCodeOf(err), out.String()
		}
		return 0, out.String()
	}
	if code, out := run("search", "model context protocol", "--num-results", "3", "--recency", "year"); code != 0 ||
		!strings.Contains(out, "wsrch_1") {
		t.Fatalf("search: exit %d, %q", code, out)
	}
	if code, _ := run("fetch", "https://example.com/", "--format", "text", "--idempotency-key", "mine"); code != 0 {
		t.Fatalf("fetch: exit %d", code)
	}
	if code, _ := run("search", "q", "--query", "other"); code == 0 {
		t.Fatal("argument and --query together must be a usage error")
	}
	if len(calls) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	if c := calls[0]; c.path != "/v1/web_searches" || c.body != `{"num_results":3,"query":"model context protocol","recency":"year"}` ||
		!strings.HasPrefix(c.key, "bma-cli-") {
		t.Fatalf("search call %+v", c)
	}
	if c := calls[1]; c.path != "/v1/web_fetches" || c.body != `{"format":"text","url":"https://example.com/"}` || c.key != "mine" {
		t.Fatalf("fetch call %+v", c)
	}
}
