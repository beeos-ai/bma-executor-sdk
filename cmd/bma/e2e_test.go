//go:build e2e && (darwin || linux)

// The bma command line against a deployment, every operation of the document
// and every hand-written command. Run (CI: .github/workflows/cli-e2e.yaml):
//
//	BMA_API_KEY=… BMA_BASE_URL=https://api.staging.beeos.ai BMA_WORKSPACE_ID=… \
//	  go test -tags e2e -run TestE2E -timeout 40m -v ./cmd/bma
//
// BMA_API_KEY is an organization key of an acceptance organization (admin
// role). Optional:
//
//	BMA_E2E_BIN                 the bma binary to test (default: built from this tree)
//	BMA_E2E_REPORT              write the coverage table (Markdown) here
//	BMA_E2E_AUTH_URL            dashboard API origin of `bma login`, with
//	BMA_E2E_USER_EMAIL/PASSWORD a dashboard user of the organization (signs in
//	                            the device grant and runs the /v1/me operations)
//	BMA_E2E_MODEL               model of the test agent (default deepseek-flash)
//	BMA_E2E_MACHINES=1          create a cloud machine (desktop, terminal, pause/resume)
//	BMA_E2E_BYO=1               Linux with systemd --user: bma up/status/harness/
//	                            upgrade/disconnect/uninstall on a BYO machine
//
// Everything the run creates is named cli-e2e-<stamp> and removed at the end;
// destructive operations only touch what the run created. An operation is
// "pass" when its success path ran, "error-path" when only its refusal could
// run (the reason says why), "skip" with a reason, and "fail" otherwise; the
// test fails when any operation fails or none of the above covers it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type e2eResult struct{ status, note string }

var rank = map[string]int{"skip": 0, "error-path": 1, "known-gap": 2, "pass": 3, "fail": 4}

// knownGaps are operations BMA_E2E_KNOWN_GAPS (comma separated) declares
// unserved by this deployment: their "no route" (404 route_not_found) or 501
// answer is recorded as known-gap, listed in the report, and does not fail
// the run; once served they record pass like any other operation.
func knownGaps() map[string]bool {
	m := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("BMA_E2E_KNOWN_GAPS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			m[id] = true
		}
	}
	return m
}

func unserved(code int, errOut string) bool {
	return (code == 4 && strings.Contains(errOut, "route_not_found")) || (code == 6 && strings.Contains(errOut, "unimplemented"))
}

type e2e struct {
	bin, stamp, dir string
	env             []string // the organization key's environment
	mu              sync.Mutex
	ops             map[string]e2eResult
	features        []string
	featureResults  map[string]e2eResult
}

func (e *e2e) record(m map[string]e2eResult, id, status, note string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := m[id]; ok && rank[old.status] >= rank[status] {
		return
	}
	m[id] = e2eResult{status, note}
}

// exec runs bma with env; stdin may be nil.
func (e *e2e) exec(env []string, stdin io.Reader, a ...string) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, a...)
	cmd.Env = env
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		}
	}
	return out.String(), errb.String(), code
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// op runs one operation's command and records it; want is the exit code.
func (e *e2e) op(t *testing.T, id string, want int, a ...string) (string, bool) {
	t.Helper()
	out, errOut, code := e.exec(e.env, nil, a...)
	if code != want && knownGaps()[id] && unserved(code, errOut) {
		e.record(e.ops, id, "known-gap", "declared unserved here: "+short(errOut))
		return out, false
	}
	if code != want {
		e.record(e.ops, id, "fail", fmt.Sprintf("`bma %s` exit %d, want %d: %s", strings.Join(a, " "), code, want, short(errOut)))
		t.Errorf("%s: bma %s: exit %d, want %d\n%s", id, strings.Join(a, " "), code, want, errOut)
		return out, false
	}
	e.record(e.ops, id, "pass", "")
	return out, true
}

// must is op with exit 0 whose failure ends the subtest (later steps need it).
func (e *e2e) must(t *testing.T, id string, a ...string) string {
	t.Helper()
	out, ok := e.op(t, id, 0, a...)
	if !ok {
		t.FailNow()
	}
	return strings.TrimSpace(out)
}

// errorPath runs a command whose success path the run cannot reach (why)
// and checks that the refusal is the expected exit code.
func (e *e2e) errorPath(t *testing.T, id string, want int, why string, a ...string) {
	t.Helper()
	_, errOut, code := e.exec(e.env, nil, a...)
	if code != want && knownGaps()[id] && unserved(code, errOut) {
		e.record(e.ops, id, "known-gap", "declared unserved here: "+short(errOut))
		return
	}
	if code != want {
		e.record(e.ops, id, "fail", fmt.Sprintf("`bma %s` exit %d, want %d: %s", strings.Join(a, " "), code, want, short(errOut)))
		t.Errorf("%s: bma %s: exit %d, want %d\n%s", id, strings.Join(a, " "), code, want, errOut)
		return
	}
	e.record(e.ops, id, "error-path", why)
}

func (e *e2e) skip(id, why string) { e.record(e.ops, id, "skip", why) }

func (e *e2e) feature(t *testing.T, name string, ok bool, note string) {
	t.Helper()
	e.mu.Lock()
	if _, seen := e.featureResults[name]; !seen {
		e.features = append(e.features, name)
	}
	e.mu.Unlock()
	if !ok {
		t.Errorf("feature %s: %s", name, note)
		e.record(e.featureResults, name, "fail", note)
		return
	}
	e.record(e.featureResults, name, "pass", note)
}

// cleanup removes what the run created, whatever happened (errors ignored).
func (e *e2e) cleanup(t *testing.T, a ...string) {
	t.Cleanup(func() { e.exec(e.env, nil, a...) })
}

func (e *e2e) name(s string) string { return e.stamp + "-" + s }

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func sha(p string) string {
	b, _ := os.ReadFile(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func envWith(base []string, kv ...string) []string {
	out := []string{}
	drop := map[string]bool{}
	for i := 0; i < len(kv); i += 2 {
		drop[kv[i]] = true
	}
	for _, v := range base {
		if k, _, _ := strings.Cut(v, "="); !drop[k] {
			out = append(out, v)
		}
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] != "" {
			out = append(out, kv[i]+"="+kv[i+1])
		}
	}
	return out
}

func TestE2E(t *testing.T) {
	for _, k := range []string{"BMA_API_KEY", "BMA_BASE_URL", "BMA_WORKSPACE_ID"} {
		if os.Getenv(k) == "" {
			t.Skipf("%s is not set", k)
		}
	}
	dir := t.TempDir()
	e := &e2e{bin: os.Getenv("BMA_E2E_BIN"), stamp: fmt.Sprintf("cli-e2e-%d", time.Now().Unix()), dir: dir,
		ops: map[string]e2eResult{}, featureResults: map[string]e2eResult{}}
	if e.bin == "" {
		e.bin = filepath.Join(dir, "bma")
		if b, err := exec.Command("go", "build", "-o", e.bin, ".").CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, b)
		}
	}
	e.env = envWith(os.Environ(), "BMA_CONFIG_DIR", filepath.Join(dir, "cfg"), "BMA_PROFILE", "",
		"BMA_HOME", filepath.Join(dir, "bmahome"))

	t.Run("cli", e.testCLI)
	t.Run("organization", e.testOrganization)
	t.Run("workspaces", e.testWorkspaces)
	t.Run("webhooks", e.testWebhooks)
	t.Run("environments", e.testEnvironments)
	t.Run("files", e.testFiles)
	t.Run("skills", e.testSkills)
	t.Run("turns", e.testTurns)
	t.Run("memory-stores", e.testMemoryStores)
	t.Run("vaults", e.testVaults)
	t.Run("user-profiles", e.testUserProfiles)
	t.Run("model-providers", e.testModelProviders)
	t.Run("machine-pools", e.testMachinePools)
	t.Run("machines", e.testMachines)
	t.Run("user", e.testUser)
	t.Run("byo", e.testBYO)

	report := e.report()
	if p := os.Getenv("BMA_E2E_REPORT"); p != "" {
		if err := os.WriteFile(p, []byte(report), 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Log("\n" + report)
	for _, op := range apiOperations {
		if _, ok := e.ops[op.ID]; !ok {
			t.Errorf("operation %s (%s %s) is not covered by the e2e", op.ID, op.Method, op.Path)
		}
	}
}

func (e *e2e) report() string {
	var b strings.Builder
	count := map[string]int{}
	byOp := map[string]apiOp{}
	ids := []string{}
	for _, op := range apiOperations {
		byOp[op.ID] = op
		ids = append(ids, op.ID)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, c := byOp[ids[i]], byOp[ids[j]]
		return strings.Join(a.Group, " ")+" "+a.Verb < strings.Join(c.Group, " ")+" "+c.Verb
	})
	rows := []string{}
	for _, id := range ids {
		op := byOp[id]
		r, ok := e.ops[id]
		if !ok {
			r = e2eResult{"fail", "not covered"}
		}
		count[r.status]++
		cmd := strings.Join(op.Group, " ") + " " + op.Verb
		for _, c := range handWrittenCommands {
			if c.op == id {
				cmd = c.cmd
			}
		}
		rows = append(rows, fmt.Sprintf("| `bma %s` | %s | %s %s | %s | %s |", cmd, id, op.Method, op.Path, r.status,
			strings.ReplaceAll(r.note, "|", "\\|")))
	}
	fmt.Fprintf(&b, "# bma CLI e2e coverage (%s)\n\nOperations: %d — pass %d, error-path %d, known-gap %d, skip %d, fail %d\n\n",
		e.stamp, len(ids), count["pass"], count["error-path"], count["known-gap"], count["skip"], count["fail"])
	b.WriteString("| command | operation | route | result | note |\n|---|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n") + "\n\n## Hand-written features\n\n| feature | result | note |\n|---|---|---|\n")
	for _, f := range e.features {
		r := e.featureResults[f]
		fmt.Fprintf(&b, "| %s | %s | %s |\n", f, r.status, strings.ReplaceAll(r.note, "|", "\\|"))
	}
	return b.String()
}

var handWrittenCommands = []struct{ op, cmd string }{
	{"files.upload", "files upload"}, {"createSkill", "skills push"}, {"createSkillVersion", "skills push --skill"},
	{"sessions.events.stream", "sessions events stream"},
}

// --- the command line itself ---------------------------------------------------------------

func (e *e2e) testCLI(t *testing.T) {
	out, _, code := e.exec(e.env, nil, "version")
	e.feature(t, "version", code == 0 && strings.TrimSpace(out) != "", out)

	// Output formats and --query over one list.
	for _, f := range []string{"json", "jsonl", "yaml", "table"} {
		out, errOut, code := e.exec(e.env, nil, "models", "list", "--format", f)
		ok := code == 0 && out != ""
		switch f {
		case "json":
			ok = ok && json.Valid([]byte(out))
		case "jsonl":
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				ok = ok && json.Valid([]byte(l))
			}
		case "yaml":
			ok = ok && strings.Contains(out, "id: ")
		case "table":
			ok = ok && strings.HasPrefix(out, "ID")
		}
		e.feature(t, "--format "+f, ok, short(errOut+out))
	}
	out, _, code = e.exec(e.env, nil, "models", "list", "--query", ".data[0].id")
	e.feature(t, "--query (raw string)", code == 0 && !strings.Contains(out, `"`) && strings.TrimSpace(out) != "", out)
	out, _, code = e.exec(e.env, nil, "models", "list", "--json", "--query", "{n: (.data|length)}")
	e.feature(t, "--json with --query", code == 0 && json.Valid([]byte(out)), out)
	_, _, code = e.exec(e.env, nil, "models", "list", "--json", "--format", "yaml")
	e.feature(t, "--json/--format conflict → exit 2", code == 2, fmt.Sprint(code))
	_, _, code = e.exec(e.env, nil, "models", "list", "--query", ".[")
	e.feature(t, "bad --query → exit 2", code == 2, fmt.Sprint(code))

	// Exit codes.
	_, _, code = e.exec(e.env, nil, "machines", "get")
	e.feature(t, "missing argument → exit 2", code == 2, fmt.Sprint(code))
	_, _, code = e.exec(e.env, nil, "no-such-command")
	e.feature(t, "unknown command → exit 2", code == 2, fmt.Sprint(code))
	_, _, code = e.exec(envWith(e.env, "BMA_API_KEY", "bma_org_v1_invalid"), nil, "machines", "list")
	e.feature(t, "rejected key → exit 3", code == 3, fmt.Sprint(code))
	_, _, code = e.exec(envWith(e.env, "BMA_API_KEY", ""), nil, "machines", "list")
	e.feature(t, "no credential → exit 3", code == 3, fmt.Sprint(code))
	_, errOut, code := e.exec(e.env, nil, "machines", "get", "mch_00000000000000000000000000")
	e.feature(t, "not found → exit 4", code == 4 && strings.Contains(errOut, "request_id"), short(errOut))
	_, errOut, code = e.exec(e.env, nil, "machines", "get", "mch_00000000000000000000000000", "--json")
	e.feature(t, "--json error → API envelope on stderr", code == 4 && json.Valid([]byte(strings.TrimSpace(errOut))), short(errOut))

	// Shell completion: each script parses in its shell.
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out, errOut, code := e.exec(e.env, nil, "completion", sh)
		ok := code == 0 && strings.Contains(out, "bma")
		note := ""
		if p, err := exec.LookPath(sh); err == nil && ok {
			f := filepath.Join(e.dir, "completion."+sh)
			_ = os.WriteFile(f, []byte(out), 0o644)
			if b, err := exec.Command(p, "-n", f).CombinedOutput(); err != nil {
				ok, note = false, sh+" -n: "+short(string(b))
			} else {
				note = sh + " -n OK"
			}
		} else if ok {
			note = sh + " not installed: script generated, not parsed"
		}
		e.feature(t, "completion "+sh, ok, note+short(errOut))
	}
	// The completion of a resource command answers its subcommands.
	out, _, code = e.exec(e.env, nil, "__complete", "sessions", "")
	e.feature(t, "completion candidates (sessions …)", code == 0 && strings.Contains(out, "send") && strings.Contains(out, "events"), short(out))

	// docs: one page per command.
	docs := filepath.Join(e.dir, "docs")
	_, errOut, code = e.exec(e.env, nil, "docs", docs)
	pages, _ := filepath.Glob(filepath.Join(docs, "*.md"))
	_, sendErr := os.Stat(filepath.Join(docs, "bma_sessions_send.md"))
	e.feature(t, "docs", code == 0 && len(pages) > len(apiOperations) && sendErr == nil,
		fmt.Sprintf("%d pages %s", len(pages), short(errOut)))
}

// --- organization (Admin API) ---------------------------------------------------------------

func (e *e2e) testOrganization(t *testing.T) {
	org := jsonOf(t, e.must(t, "organization.retrieve", "organizations", "me", "get"))
	if org["id"] == nil {
		t.Error("organizations me get: no id")
	}
	users := e.must(t, "organization.users.list", "organizations", "users", "list", "--limit", "20", "--query",
		`.data[] | select(.role != "owner") | .id + " " + .role`)
	first := strings.Fields(strings.SplitN(users, "\n", 2)[0])
	if len(first) == 2 {
		e.must(t, "organization.users.retrieve", "organizations", "users", "get", first[0])
	} else {
		e.skip("organization.users.retrieve", "the organization has no member besides the owner")
	}
	e.errorPath(t, "organization.users.update", 4, "changing a real member's role is not reversible if the run stops midway, and the run cannot create a member (that needs a second person accepting an invite): unknown user → 404",
		"organizations", "users", "update", "user_00000000000000000000000000", "--role", "user")
	e.errorPath(t, "organization.users.remove", 4, "removing a real member is irreversible and the run cannot create one (that needs a second person accepting an invite): unknown user → 404",
		"organizations", "users", "delete", "user_00000000000000000000000000")
	since := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T00:00:00Z")
	e.must(t, "getCostReport", "organizations", "cost-report", "list", "--starting-at", since, "--group-by", "[workspace_id]")
	e.must(t, "getMessagesUsageReport", "organizations", "usage-report", "messages", "list", "--starting-at", since)

	// API keys: created inactive-able only (the API has no delete); the run deactivates its key.
	key := e.must(t, "createOrganizationAPIKey", "organizations", "api-keys", "create", "--name", e.name("key"),
		"--workspace-id", os.Getenv("BMA_WORKSPACE_ID"), "--query", ".id")
	e.cleanup(t, "organizations", "api-keys", "update", key, "--status", "archived")
	e.must(t, "organization.api_keys.retrieve", "organizations", "api-keys", "get", key)
	e.must(t, "organization.api_keys.update", "organizations", "api-keys", "update", key, "--name", e.name("key-2"))
	if n := e.must(t, "organization.api_keys.list", "organizations", "api-keys", "list", "--all", "--query",
		fmt.Sprintf(`[.[] | select(.id == %q)] | length`, key)); n != "1" {
		t.Errorf("api-keys list --all does not find %s (%s)", key, n)
	}
	e.must(t, "organization.api_keys.update", "organizations", "api-keys", "update", key, "--status", "archived")

	// Invites to an address nobody reads; deleted by the run.
	inv := e.must(t, "organization.invites.create", "organizations", "invites", "create", "--email", e.stamp+"@example.com",
		"--role", "user", "--query", ".id")
	e.cleanup(t, "organizations", "invites", "delete", inv)
	e.must(t, "organization.invites.retrieve", "organizations", "invites", "get", inv)
	e.must(t, "organization.invites.list", "organizations", "invites", "list", "--email", e.stamp+"@example.com")
	e.must(t, "organization.invites.delete", "organizations", "invites", "delete", inv)

	// Models.
	model := firstNonEmpty(os.Getenv("BMA_E2E_MODEL"), "deepseek-flash")
	e.must(t, "models.list", "models", "list")
	e.must(t, "models.retrieve", "models", "get", model)
}

func (e *e2e) testWorkspaces(t *testing.T) {
	e.must(t, "organization.workspaces.list", "organizations", "workspaces", "list", "--limit", "5")
	ws := e.must(t, "organization.workspaces.create", "organizations", "workspaces", "create", "--name", e.name("ws"), "--query", ".id")
	e.cleanup(t, "organizations", "workspaces", "archive", ws)
	e.must(t, "organization.workspaces.retrieve", "organizations", "workspaces", "get", ws)
	e.must(t, "organization.workspaces.update", "organizations", "workspaces", "update", ws, "--name", e.name("ws-2"))
	user := e.must(t, "", "organizations", "users", "list", "--query", `[.data[] | select(.role != "owner")][0].id // .data[0].id`)
	e.must(t, "organization.workspaces.members.add", "organizations", "workspaces", "members", "create", ws, "--user-id", user,
		"--workspace-role", "workspace_developer")
	e.must(t, "organization.workspaces.members.list", "organizations", "workspaces", "members", "list", ws)
	e.must(t, "organization.workspaces.members.retrieve", "organizations", "workspaces", "members", "get", ws, user)
	e.must(t, "organization.workspaces.members.update", "organizations", "workspaces", "members", "update", ws, user,
		"--workspace-role", "workspace_user")
	e.must(t, "organization.workspaces.members.remove", "organizations", "workspaces", "members", "delete", ws, user)

	e.must(t, "organization.workspaces.archive", "organizations", "workspaces", "archive", ws)
}

func (e *e2e) testWebhooks(t *testing.T) {
	id := "whep_00000000000000000000000000"
	if out, ok := e.op(t, "createWebhookEndpoint", 0, "organizations", "webhooks", "create", "--url", "https://example.com/"+e.stamp,
		"--enabled-events", "[session.status_idle]", "--query", ".id"); ok {
		id = strings.TrimSpace(out)
		e.cleanup(t, "organizations", "webhooks", "delete", id)
	}
	e.op(t, "listWebhookEndpoints", 0, "organizations", "webhooks", "list")
	e.op(t, "getWebhookEndpoint", 0, "organizations", "webhooks", "get", id)
	e.op(t, "updateWebhookEndpoint", 0, "organizations", "webhooks", "update", id, "--url", "https://example.com/"+e.stamp+"-2")
	e.op(t, "regenerateWebhookEndpointSigningSecret", 0, "organizations", "webhooks", "regenerate-signing-secret", id)
	e.op(t, "deleteWebhookEndpoint", 0, "organizations", "webhooks", "delete", id)
}

// --- managed agents resources -------------------------------------------------------------

func (e *e2e) environment(t *testing.T, id string) string {
	env := e.must(t, id, "environments", "create", "--name", e.name("env"), "--config",
		"{type: cloud, networking: {type: unrestricted}}", "--query", ".id")
	e.cleanup(t, "environments", "delete", env)
	return env
}

func (e *e2e) testEnvironments(t *testing.T) {
	env := e.environment(t, "environments.create")
	e.must(t, "environments.retrieve", "environments", "get", env)
	e.must(t, "environments.update", "environments", "update", env, "--description", "updated by "+e.stamp)
	e.must(t, "environments.list", "environments", "list", "--limit", "5")
	e.must(t, "environments.archive", "environments", "archive", env)
	e.must(t, "environments.delete", "environments", "delete", env)
}

func writeRandom(t *testing.T, p string, n int) {
	t.Helper()
	b := make([]byte, n)
	_, _ = rand.Read(b)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *e2e) testFiles(t *testing.T) {
	small := filepath.Join(e.dir, "hello.txt")
	_ = os.WriteFile(small, []byte("hello from "+e.stamp+"\n"), 0o644)
	big := filepath.Join(e.dir, "big.bin")
	writeRandom(t, big, 24<<20) // 24 MiB of binary
	for _, p := range []string{small, big} {
		start := time.Now()
		f := jsonOf(t, e.must(t, "files.upload", "files", "upload", p))
		id, _ := f["id"].(string)
		e.cleanup(t, "files", "delete", id)
		e.must(t, "files.retrieve_metadata", "files", "get", id)
		got := p + ".down"
		e.must(t, "files.download", "files", "download", id, "-o", got)
		size, _ := f["size_bytes"].(float64)
		e.feature(t, "files upload/download round trip "+filepath.Base(p), sha(got) == sha(p),
			fmt.Sprintf("%.0f bytes, sha256 equal=%v, %.1fs", size, sha(got) == sha(p), time.Since(start).Seconds()))
		if p == small {
			out, _, code := e.exec(e.env, nil, "files", "content", id)
			e.feature(t, "files content to stdout", code == 0 && strings.Contains(out, e.stamp), short(out))
		}
	}
	e.must(t, "files.list", "files", "list", "--limit", "5")
	id := e.must(t, "", "files", "upload", small, "--query", ".id")
	e.must(t, "files.delete", "files", "delete", id)
	_, _, code := e.exec(e.env, nil, "files", "upload", filepath.Join(e.dir, "missing"))
	e.feature(t, "files upload of a missing path fails", code != 0, fmt.Sprint(code))
}

func (e *e2e) testSkills(t *testing.T) {
	dir := filepath.Join(e.dir, e.stamp) // the API wants SKILL.md's name to be the top directory
	_ = os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	write := func(desc string) {
		_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+e.stamp+"\ndescription: "+desc+"\n---\n\nSay hello.\n"), 0o644)
	}
	write("e2e skill v1")
	_ = os.WriteFile(filepath.Join(dir, "scripts", "hello.sh"), []byte("#!/bin/sh\necho hello\n"), 0o755)
	writeRandom(t, filepath.Join(dir, "scripts", "blob.bin"), 4096)
	_ = os.WriteFile(filepath.Join(dir, ".hidden"), []byte("not uploaded"), 0o644)
	sk := jsonOf(t, e.must(t, "createSkill", "skills", "push", dir, "--display-name", e.name("skill")))
	id, _ := sk["id"].(string)
	e.cleanup(t, "skills", "delete", id)
	write("e2e skill v2")
	e.must(t, "createSkillVersion", "skills", "push", dir, "--skill", id)
	vs := strings.Fields(e.must(t, "listSkillVersions", "skills", "versions", "list", id, "--query", `[.data[] | [.created_at, .id]] | sort | map(.[1]) | join(" ")`))
	if len(vs) != 2 {
		t.Fatalf("skill %s versions %v, want 2", id, vs)
	}
	v1, v2 := vs[0], vs[1]
	e.must(t, "getSkill", "skills", "get", id)
	e.must(t, "listSkills", "skills", "list", "--limit", "5")
	e.must(t, "getSkillVersion", "skills", "versions", "get", id, v2)
	zip := filepath.Join(e.dir, "skill.zip")
	e.must(t, "getSkillVersionContent", "skills", "versions", "content", id, v2, "-o", zip)
	b, _ := os.ReadFile(zip)
	e.feature(t, "skills push → content holds the files (no dotfiles)", bytes.HasPrefix(b, []byte("PK")) &&
		bytes.Contains(b, []byte("scripts/hello.sh")) && bytes.Contains(b, []byte("scripts/blob.bin")) && !bytes.Contains(b, []byte(".hidden")),
		fmt.Sprintf("%d bytes zip, v1 %s v2 %s", len(b), v1, v2))
	_, _, code := e.exec(e.env, nil, "skills", "push", e.dir+"/nothing")
	e.feature(t, "skills push without SKILL.md → exit 2", code == 2, fmt.Sprint(code))
	e.must(t, "deleteSkillVersion", "skills", "versions", "delete", id, v1) // the latest one goes with the skill
	e.must(t, "deleteSkill", "skills", "delete", id)
}

// testTurns: agent, session, events (send, stream with a forced disconnect, wait), resources.
func (e *e2e) testTurns(t *testing.T) {
	env := e.environment(t, "")
	model := firstNonEmpty(os.Getenv("BMA_E2E_MODEL"), "deepseek-flash")
	agent := jsonOf(t, e.must(t, "agents.create", "agents", "create", "--name", e.name("agent"), "--model", model,
		"--system", "You answer in as few words as possible."))
	aid, _ := agent["id"].(string)
	e.cleanup(t, "agents", "archive", aid)
	e.must(t, "agents.retrieve", "agents", "get", aid)
	e.must(t, "agents.update", "agents", "update", aid, "--description", "e2e", "--version", fmt.Sprint(agent["version"]))
	e.must(t, "agents.list", "agents", "list", "--limit", "5")
	e.must(t, "listAgentVersions", "agents", "versions", "list", aid)

	sid := e.must(t, "sessions.create", "sessions", "create", "--agent", aid, "--environment-id", env, "--title", e.name("session"), "--query", ".id")
	e.cleanup(t, "sessions", "delete", sid)
	e.must(t, "sessions.retrieve", "sessions", "get", sid)
	e.must(t, "sessions.update", "sessions", "update", sid, "--title", e.name("session-2"))
	e.must(t, "sessions.list", "sessions", "list", "--limit", "5")

	// Turn 1: the generated events create, then wait for idle with the stream.
	e.must(t, "sessions.events.send", "sessions", "events", "create", sid, "--events",
		`[{type: user.message, content: [{type: text, text: "Reply with exactly the word alpha."}]}]`)
	e.waitIdle(t, sid)

	// Turn 2: send --wait on a session with history.
	out, errOut, code := e.exec(e.env, nil, "sessions", "send", sid, "Reply with exactly the word bravo.", "--wait")
	e.feature(t, "sessions send --wait (second turn of a session)", code == 0 && strings.Contains(strings.ToLower(out), "bravo"), short(out+errOut))

	// Turn 3: send --stream prints JSON lines up to the idle.
	out, errOut, code = e.exec(e.env, nil, "sessions", "send", sid, "Reply with exactly the word charlie.", "--stream")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := map[string]any{}
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	e.feature(t, "sessions send --stream", code == 0 && last["type"] == eventIdle && strings.Contains(strings.ToLower(out), "charlie"),
		fmt.Sprintf("%d events, last %v %s", len(lines), last["type"], short(errOut)))

	// Turn 4: the stream through a proxy that cuts the first connection after two events.
	e.streamResume(t, sid)

	// Turn 5: send from stdin.
	out, _, code = e.exec(e.env, strings.NewReader("Reply with exactly the word echo."), "sessions", "send", sid, "-", "--wait", "--json")
	e.feature(t, "sessions send - (stdin) --wait --json", code == 0 && strings.Contains(strings.ToLower(out), "echo") && json.Valid([]byte(out)), short(out))
	_, _, code = e.exec(e.env, nil, "sessions", "send", sid, "x", "--wait", "--stream")
	e.feature(t, "sessions send --wait --stream → exit 2", code == 2, fmt.Sprint(code))

	if n := e.must(t, "sessions.events.list", "sessions", "events", "list", sid, "--all", "--query",
		`[.[] | select(.type == "agent.message")] | length`); n == "0" || n == "1" || n == "2" || n == "3" {
		t.Errorf("events list: %s agent messages, want >= 4", n)
	}

	// Resources: a file mounted into the session.
	f := filepath.Join(e.dir, "resource.txt")
	_ = os.WriteFile(f, []byte("resource "+e.stamp), 0o644)
	fid := e.must(t, "", "files", "upload", f, "--query", ".id")
	e.cleanup(t, "files", "delete", fid)
	rid := e.must(t, "sessions.resources.add", "sessions", "resources", "create", sid, "--type", "file", "--file-id", fid, "--query", ".id")
	e.must(t, "sessions.resources.list", "sessions", "resources", "list", sid)
	e.must(t, "sessions.resources.retrieve", "sessions", "resources", "get", sid, rid)
	e.errorPath(t, "sessions.resources.update", 1, "only github_repository resources rotate a token; a GitHub repository resource needs a real repository token (external party): a file resource → 400",
		"sessions", "resources", "update", sid, rid, "--authorization-token", "x")
	e.must(t, "sessions.resources.delete", "sessions", "resources", "delete", sid, rid)

	e.must(t, "listSessionToolGrants", "sessions", "tool-grants", "list", sid)
	e.errorPath(t, "revokeSessionToolGrant", 4, "a tool grant exists only after a user approves a tool confirmation in a session of an agent with an always_ask MCP tool (needs an external MCP server): unknown grant → 404",
		"sessions", "tool-grants", "revoke", sid, "tgr_00000000000000000000000000")
	e.must(t, "sessions.archive", "sessions", "archive", sid)
	if _, errOut, code := e.exec(e.env, nil, "sessions", "bma-executor-revocation", sid); code == 0 || code == 5 {
		e.record(e.ops, "revokeSessionExecutor", "pass", map[int]string{0: "", 5: "409 executor_revocation_pending (retryable, documented)"}[code])
	} else {
		e.record(e.ops, "revokeSessionExecutor", "fail", fmt.Sprintf("exit %d: %s", code, short(errOut)))
		t.Errorf("bma-executor-revocation: exit %d %s", code, errOut)
	}
	e.must(t, "sessions.delete", "sessions", "delete", sid)
	e.must(t, "agents.archive", "agents", "archive", aid)
}

// waitIdle follows the stream until the session is idle.
func (e *e2e) waitIdle(t *testing.T, sid string) {
	t.Helper()
	cur := e.must(t, "", "sessions", "get", sid, "--query", ".status")
	if cur == "idle" {
		return
	}
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if e.must(t, "", "sessions", "get", sid, "--query", ".status") == "idle" {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("session %s did not go idle", sid)
}

// streamResume runs `sessions events stream --until-idle` through a proxy
// that drops the first stream connection after two events, sends a message,
// and checks that the stream resumed with Last-Event-ID: every event once,
// up to the idle.
func (e *e2e) streamResume(t *testing.T, sid string) {
	target, _ := url.Parse(os.Getenv("BMA_BASE_URL"))
	var mu sync.Mutex
	var lastIDs []string
	streams := 0
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = target.Host
		if !strings.HasSuffix(r.URL.Path, "/events/stream") {
			rp.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		streams++
		n := streams
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		if n > 1 {
			rp.ServeHTTP(w, r)
			return
		}
		// The first connection: relay two events, then cut it.
		cw := &cutWriter{ResponseWriter: w, events: 2}
		ctx, cancel := context.WithCancel(r.Context())
		cw.cancel = cancel
		rp.ServeHTTP(cw, r.WithContext(ctx))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	env := envWith(e.env, "BMA_BASE_URL", "http://"+ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "sessions", "events", "stream", sid, "--until-idle")
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ { // the stream is open before the message is sent
		mu.Lock()
		n := streams
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second)
	e.must(t, "", "sessions", "send", sid, "Count from one to five in words, one per line.")
	err = cmd.Wait()
	ids := map[string]int{}
	var types []string
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			id, _ := ev["id"].(string)
			ids[id]++
			ty, _ := ev["type"].(string)
			types = append(types, ty)
		}
	}
	dup := 0
	for _, c := range ids {
		if c > 1 {
			dup++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	ok := err == nil && streams >= 2 && len(lastIDs) >= 2 && lastIDs[1] != "" && dup == 0 &&
		len(types) > 2 && types[len(types)-1] == eventIdle && strings.Contains(errb.String(), "stream dropped")
	e.record(e.ops, "sessions.events.stream", map[bool]string{true: "pass", false: "fail"}[ok], "")
	e.feature(t, "sessions events stream: resume with Last-Event-ID after a forced disconnect", ok,
		fmt.Sprintf("connections %d, Last-Event-ID of the 2nd %q, %d events (%d duplicated), last %v, err %v %s",
			streams, lastIDs[min(1, len(lastIDs)-1)], len(types), dup, types, err, short(errb.String())))
}

// cutWriter passes a server-sent event stream through until it has relayed
// events complete events, then ends the connection.
type cutWriter struct {
	http.ResponseWriter
	events int
	seen   int
	buf    []byte
	cancel func()
	done   bool
}

func (c *cutWriter) Write(b []byte) (int, error) {
	if c.done {
		return 0, io.ErrClosedPipe
	}
	c.buf = append(c.buf, b...)
	for {
		i := bytes.Index(c.buf, []byte("\n\n"))
		if i < 0 {
			return len(b), nil
		}
		chunk := c.buf[:i+2]
		c.buf = c.buf[i+2:]
		if _, err := c.ResponseWriter.Write(chunk); err != nil {
			return 0, err
		}
		if f, ok := c.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		if bytes.Contains(chunk, []byte("\ndata:")) || bytes.HasPrefix(chunk, []byte("data:")) || bytes.Contains(chunk, []byte("id:")) {
			c.seen++
		}
		if c.seen >= c.events {
			c.done = true
			if hj, ok := c.ResponseWriter.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			c.cancel()
			return 0, io.ErrClosedPipe
		}
	}
}

func (c *cutWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (e *e2e) testMemoryStores(t *testing.T) {
	ms := e.must(t, "memory_stores.create", "memory-stores", "create", "--name", e.name("memory"), "--query", ".id")
	e.cleanup(t, "memory-stores", "delete", ms)
	e.must(t, "memory_stores.retrieve", "memory-stores", "get", ms)
	e.must(t, "memory_stores.update", "memory-stores", "update", ms, "--description", "e2e")
	e.must(t, "memory_stores.list", "memory-stores", "list", "--limit", "5")
	mem := e.must(t, "memory_stores.memories.create", "memory-stores", "memories", "create", ms, "--path", "/notes/e2e.md",
		"--content", "first", "--query", ".id")
	e.must(t, "memory_stores.memories.retrieve", "memory-stores", "memories", "get", ms, mem)
	e.must(t, "memory_stores.memories.update", "memory-stores", "memories", "update", ms, mem, "--content", "second")
	e.must(t, "memory_stores.memories.list", "memory-stores", "memories", "list", ms)
	// The oldest version is no longer the head: it can be redacted.
	old := e.must(t, "listMemoryVersions", "memory-stores", "memory-versions", "list", ms, "--all", "--query", ".[-1].id")
	e.must(t, "getMemoryVersion", "memory-stores", "memory-versions", "get", ms, old)
	e.must(t, "redactMemoryVersion", "memory-stores", "memory-versions", "redact", ms, old)
	e.must(t, "memory_stores.memories.delete", "memory-stores", "memories", "delete", ms, mem)
	e.must(t, "archiveMemoryStore", "memory-stores", "archive", ms)
	e.must(t, "memory_stores.delete", "memory-stores", "delete", ms)
}

func (e *e2e) testVaults(t *testing.T) {
	v := e.must(t, "createVault", "vaults", "create", "--display-name", e.name("vault"), "--query", ".id")
	e.cleanup(t, "vaults", "delete", v)
	e.must(t, "getVault", "vaults", "get", v)
	e.must(t, "updateVault", "vaults", "update", v, "--display-name", e.name("vault-2"))
	e.must(t, "listVaults", "vaults", "list", "--limit", "5")
	c := e.must(t, "createVaultCredential", "vaults", "credentials", "create", v, "--display-name", "e2e", "--auth",
		"{type: static_bearer, token: e2e-not-a-secret, mcp_server_url: https://mcp.example.com/mcp}", "--query", ".id")
	e.must(t, "getVaultCredential", "vaults", "credentials", "get", v, c)
	e.must(t, "updateVaultCredential", "vaults", "credentials", "update", v, c, "--display-name", "e2e-2")
	e.must(t, "listVaultCredentials", "vaults", "credentials", "list", v)
	e.errorPath(t, "validateVaultCredentialMCPOAuth", 1, "validating needs an mcp_oauth credential from a real OAuth provider (external party): a static_bearer credential → 400",
		"vaults", "credentials", "mcp-oauth-validate", v, c)
	// MCP OAuth authorizations start a third party's OAuth flow; run them against the vault anyway.
	if out, ok := e.op(t, "createMCPOAuthAuthorization", 0, "mcp-oauth-authorizations", "create", "--vault-id", v,
		"--mcp-server-url", "https://mcp.example.com/mcp", "--query", ".id"); ok {
		e.op(t, "getMCPOAuthAuthorization", 0, "mcp-oauth-authorizations", "get", strings.TrimSpace(out))
	} else {
		e.op(t, "getMCPOAuthAuthorization", 4, "mcp-oauth-authorizations", "get", "mcpoa_00000000000000000000000000")
	}
	e.errorPath(t, "completeMCPOAuthAuthorization", 1, "the callback carries the state and code a real OAuth provider returns (external party): unknown state → 400",
		"mcp-oauth", "callback", "create", "--state", "e2e-unknown-state", "--body", "{code: x}")
	e.must(t, "archiveVaultCredential", "vaults", "credentials", "archive", v, c)
	e.must(t, "deleteVaultCredential", "vaults", "credentials", "delete", v, c)
	e.must(t, "archiveVault", "vaults", "archive", v)
	e.must(t, "deleteVault", "vaults", "delete", v)
}

func (e *e2e) testUserProfiles(t *testing.T) {
	p := e.must(t, "createUserProfile", "user-profiles", "create", "--name", e.name("profile"), "--metadata", "{run: e2e}", "--query", ".id")
	e.cleanup(t, "user-profiles", "delete", p)
	e.must(t, "getUserProfile", "user-profiles", "get", p)
	e.must(t, "updateUserProfile", "user-profiles", "update", p, "--name", e.name("profile-2"))
	e.must(t, "listUserProfiles", "user-profiles", "list")
	e.must(t, "deleteUserProfile", "user-profiles", "delete", p)
}

func (e *e2e) testModelProviders(t *testing.T) {
	id := strings.ReplaceAll(e.stamp, "-", "")
	p := e.must(t, "createModelProvider", "model-providers", "create", "--id", id, "--api-format", "openai-chat-completions",
		"--api-key", "e2e-not-a-secret-key", "--base-url", "https://example.com/v1", "--query", ".id")
	e.cleanup(t, "model-providers", "delete", p)
	e.must(t, "getModelProvider", "model-providers", "get", p)
	e.must(t, "updateModelProvider", "model-providers", "update", p, "--base-url", "https://example.com/v2")
	e.must(t, "listModelProviders", "model-providers", "list")
	m := e.must(t, "createModelProviderModel", "model-providers", "models", "create", p, "--id", e.name("model"),
		"--context-window-tokens", "8192", "--query", ".id")
	e.must(t, "getModelProviderModel", "model-providers", "models", "get", p, m)
	e.must(t, "updateModelProviderModel", "model-providers", "models", "update", p, m, "--context-window-tokens", "16384")
	e.must(t, "listModelProviderModels", "model-providers", "models", "list", p)
	// Grants: an organization's own model to a workspace of the run.
	ws := e.must(t, "", "organizations", "workspaces", "create", "--name", e.name("grants"), "--query", ".id")
	e.cleanup(t, "organizations", "workspaces", "archive", ws)
	e.must(t, "createModelGrant", "models", "grants", "create", m, "--workspace-id", ws)
	e.cleanup(t, "models", "grants", "delete", m, ws)
	e.must(t, "listModelGrants", "models", "grants", "list", m)
	e.must(t, "deleteModelGrant", "models", "grants", "delete", m, ws)
	e.must(t, "deleteModelProviderModel", "model-providers", "models", "delete", p, m)
	e.must(t, "deleteModelProvider", "model-providers", "delete", p)
}

func (e *e2e) testMachinePools(t *testing.T) {
	e.must(t, "listMachinePools", "machine-pools", "list", "--limit", "5")
	env := e.environment(t, "")
	// An environment's pool has the environment's id.
	e.must(t, "getMachinePool", "machine-pools", "get", env)
	e.must(t, "listMachinePoolGrants", "machine-pools", "grants", "list", env)
	if g := e.must(t, "", "machine-pools", "grants", "list", env, "--query", ".data[0].id // empty"); g != "" {
		e.must(t, "getMachinePoolGrant", "machine-pools", "grants", "get", env, g)
	} else {
		e.errorPath(t, "getMachinePoolGrant", 4, "the pool has no grant (grants are written by the platform, the API has no grant create): unknown grant → 404",
			"machine-pools", "grants", "get", env, "mpg_00000000000000000000000000")
	}
	// A pool of the provider the environment's pool uses, with its provider settings.
	cfg := e.must(t, "", "machine-pools", "get", env, "--query", "{provider, provider_config, default_machine_provider_options}", "--format", "jsonl")
	body := jsonOf(t, cfg)
	for k, v := range map[string]any{"default_machine_cpu": 1, "default_machine_memory_mb": 1024, "max_total_cpu": 2,
		"max_total_memory_mb": 2048, "max_total_machines": 2, "max_machine_cpu": 2, "max_machine_memory_mb": 4096} {
		body[k] = v
	}
	bb, _ := json.Marshal(body)
	pool, ok := e.op(t, "createMachinePool", 0, "machine-pools", "create", "--body", string(bb), "--name", e.name("pool"), "--query", ".id")
	pool = strings.TrimSpace(pool)
	if !ok {
		pool = "env_00000000000000000000000000"
	} else {
		e.cleanup(t, "machine-pools", "delete", pool)
	}
	e.op(t, "updateMachinePool", 0, "machine-pools", "update", pool, "--description", "e2e")
	e.op(t, "deleteMachinePool", 0, "machine-pools", "delete", pool)
}

func (e *e2e) testMachines(t *testing.T) {
	e.must(t, "listMachines", "machines", "list", "--limit", "5")
	// A BYO machine is a record and an executor token: no compute.
	m := e.must(t, "createMachine", "machines", "create", "--source", "byo", "--display-name", e.name("byo"), "--token-name", "e2e", "--query", ".id")
	e.cleanup(t, "machines", "delete", m)
	e.must(t, "getMachine", "machines", "get", m)
	e.must(t, "updateMachine", "machines", "update", m, "--agent-ids", "[]")
	e.must(t, "getMachineCapabilities", "machines", "capabilities", m)
	tok := e.must(t, "createExecutorToken", "machines", "executor-tokens", "create", m, "--name", "e2e-2", "--query", ".id")
	e.must(t, "listExecutorTokens", "machines", "executor-tokens", "list", m)
	e.must(t, "revokeExecutorToken", "machines", "executor-tokens", "revoke", m, tok)
	e.must(t, "listMachineBackups", "machines", "backups", "list", m)

	if os.Getenv("BMA_E2E_MACHINES") != "1" {
		for _, id := range []string{"pauseMachine", "resumeMachine", "restoreMachine", "createDesktopSession", "getDesktopSession",
			"acquireDesktopControl", "sendDesktopActions", "closeDesktopSession", "handbackDesktopControl", "keepaliveDesktopSession",
			"releaseDesktopControl", "renewDesktopControl", "captureDesktopScreenshot", "createDesktopSessionTicket", "revokeDesktopViewer",
			"listTerminalSessions", "createTerminalSession", "getTerminalSession", "closeTerminalSession", "createTerminalSessionTicket"} {
			e.skip(id, "needs a running cloud machine: set BMA_E2E_MACHINES=1")
		}
		e.must(t, "deleteMachine", "machines", "delete", m)
		return
	}
	e.must(t, "deleteMachine", "machines", "delete", m)
	e.cloudMachine(t)
}

// cloudMachine: a persistent machine of the run's environment pool, its
// desktop and terminal, pause and resume.
func (e *e2e) cloudMachine(t *testing.T) {
	env := e.environment(t, "")
	m := e.must(t, "", "machines", "create", "--machine-pool-id", env, "--persistent", "true", "--template", "base-2c4g-dsh", "--display-name", e.name("cloud"), "--query", ".id")
	e.cleanup(t, "machines", "delete", m)
	deadline := time.Now().Add(5 * time.Minute)
	for e.must(t, "", "machines", "get", m, "--query", `.lifecycle_state + " " + .connection_state`) != "active online" {
		if time.Now().After(deadline) {
			t.Fatalf("machine %s did not run", m)
		}
		time.Sleep(3 * time.Second)
	}

	// Terminal.
	ts := e.must(t, "createTerminalSession", "terminal-sessions", "create", "--machine-id", m, "--cols", "80", "--rows", "24", "--query", ".id")
	e.must(t, "getTerminalSession", "terminal-sessions", "get", ts)
	e.must(t, "listTerminalSessions", "terminal-sessions", "list", "--machine-id", m)
	e.must(t, "createTerminalSessionTicket", "terminal-sessions", "tickets", ts, "--body", "{purpose: connect, ttl_seconds: 60}")
	e.must(t, "closeTerminalSession", "terminal-sessions", "close", ts)

	// Desktop.
	ds := e.must(t, "createDesktopSession", "desktop-sessions", "create", "--machine-id", m, "--query", ".id")
	e.cleanup(t, "desktop-sessions", "close", ds)
	e.must(t, "getDesktopSession", "desktop-sessions", "get", ds)
	e.must(t, "keepaliveDesktopSession", "desktop-sessions", "keepalive", ds)
	viewer := e.stamp
	e.must(t, "createDesktopSessionTicket", "desktop-sessions", "tickets", ds, "--purpose", "watch", "--viewer", viewer, "--ttl-seconds", "60")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	inputKey := base64.RawURLEncoding.EncodeToString(pub)
	lease := e.must(t, "acquireDesktopControl", "desktop-sessions", "acquire-control", ds, "--holder", viewer, "--input-key", inputKey, "--query", ".lease_id")
	e.must(t, "renewDesktopControl", "desktop-sessions", "renew-control", ds, "--holder", viewer, "--lease-id", lease, "--input-key", inputKey)
	// A screen-live desktop (the executor's live media, every cloud machine here) has no desktop
	// agent: actions and screenshots are refused by design (server/realtime/domain ScreenLive).
	why := "the machine's desktop is screen-live (executor media, no desktop agent): actions/screenshots answer 400 by design; no machine type here has a desktop agent"
	e.errorPath(t, "sendDesktopActions", 1, why, "desktop-sessions", "actions", ds, "--seq", "1", "--action", "{kind: pointer_move, x: 10, y: 10}")
	e.errorPath(t, "captureDesktopScreenshot", 1, why, "desktop-sessions", "screenshot", ds)
	e.must(t, "releaseDesktopControl", "desktop-sessions", "release-control", ds, "--holder", viewer, "--lease-id", lease)
	lease = e.must(t, "", "desktop-sessions", "acquire-control", ds, "--holder", viewer, "--input-key", inputKey, "--query", ".lease_id")
	e.must(t, "handbackDesktopControl", "desktop-sessions", "handback-control", ds, "--holder", viewer, "--lease-id", lease)
	e.must(t, "revokeDesktopViewer", "desktop-sessions", "viewers", "revoke", ds, viewer)
	e.must(t, "closeDesktopSession", "desktop-sessions", "close", ds)

	// Power.
	e.must(t, "pauseMachine", "machines", "pause", m)
	e.must(t, "resumeMachine", "machines", "resume", m)
	if b := e.must(t, "", "machines", "backups", "list", m, "--query", ".data[0].id // empty"); b != "" {
		e.must(t, "restoreMachine", "machines", "restore", m, "--backup-id", b)
	} else {
		e.errorPath(t, "restoreMachine", 4, "the machine has no backup yet (backups are taken by the platform): unknown backup → 404",
			"machines", "restore", m, "--backup-id", "mbk_00000000000000000000000000")
	}
	e.must(t, "", "machines", "delete", m)
}

// --- the user: bma login (device grant), profiles, /v1/me ---------------------------------------

func (e *e2e) testUser(t *testing.T) {
	auth, email, password := os.Getenv("BMA_E2E_AUTH_URL"), os.Getenv("BMA_E2E_USER_EMAIL"), os.Getenv("BMA_E2E_USER_PASSWORD")
	if auth == "" || email == "" || password == "" {
		for _, id := range []string{"listPersonalAccessTokens", "createPersonalAccessToken", "revokePersonalAccessToken",
			"listMyInvites", "acceptMyInvite", "declineMyInvite"} {
			e.skip(id, "needs a user credential: set BMA_E2E_AUTH_URL and BMA_E2E_USER_EMAIL/PASSWORD")
		}
		e.feature(t, "bma login (device grant)", true, "skipped: no BMA_E2E_USER_*")
		return
	}
	cfg := filepath.Join(e.dir, "user-cfg")
	uenv := envWith(e.env, "BMA_CONFIG_DIR", cfg, "BMA_API_KEY", "", "BMA_BASE_URL", "", "BMA_WORKSPACE_ID", "")
	base := os.Getenv("BMA_BASE_URL")
	ws := os.Getenv("BMA_WORKSPACE_ID")
	dash := newDashboard(t, auth, email, password)

	// Denied: the CLI says so and stores nothing.
	out, errOut, code := e.login(t, uenv, auth, base, func(code string) error { return dash.decide(code, "deny", "", "") })
	e.feature(t, "bma login: denied in the dashboard → error, nothing stored", code != 0 && strings.Contains(errOut, "denied"), short(out+errOut))

	// Approved: a PAT named after the computer, with the organization and workspace chosen.
	org := jsonOf(t, e.must(t, "", "organizations", "me", "get"))["id"].(string)
	_, errOut, code = e.login(t, uenv, auth, base, func(code string) error { return dash.decide(code, "approve", org, ws) })
	e.feature(t, "bma login: approved in the dashboard → signed in", code == 0 && strings.Contains(errOut, "Signed in"), short(errOut))
	st, _, code := e.exec(uenv, nil, "auth", "status")
	s := map[string]any{}
	_ = json.Unmarshal([]byte(st), &s)
	e.feature(t, "auth status after login", code == 0 && s["signed_in"] == true && s["organization"] == org && s["workspace"] == ws, short(st))
	tok, _, _ := e.exec(uenv, nil, "auth", "token")
	tok = strings.TrimSpace(tok)
	e.feature(t, "auth token prints the PAT", strings.HasPrefix(tok, "bma_pat_"), "prefix "+firstN(tok, 8))

	host, _ := os.Hostname()
	if runtime.GOOS == "darwin" {
		if b, err := exec.Command("/usr/sbin/scutil", "--get", "ComputerName").Output(); err == nil {
			host = strings.TrimSpace(string(b))
		}
	}
	pat := e.userOp(t, uenv, "listPersonalAccessTokens", 0, "me", "personal-access-tokens", "list", "--all", "--query",
		fmt.Sprintf(`[.[] | select(.name == %q and .status == "active")][-1].id`, "bma CLI on "+host))
	e.feature(t, "PAT named \"bma CLI on <computer>\"", strings.HasPrefix(pat, "pat_"), pat)

	// A second PAT, made and revoked with the first.
	p2 := e.userOp(t, uenv, "createPersonalAccessToken", 0, "me", "personal-access-tokens", "create", "--name", e.name("pat"), "--query", ".id")
	e.userOp(t, uenv, "revokePersonalAccessToken", 0, "me", "personal-access-tokens", "revoke", p2)
	e.userOp(t, uenv, "listMyInvites", 0, "me", "invites", "list")
	why := "an invite to this user needs another organization inviting them (the run has one organization): unknown invite → 404"
	if _, _, code := e.exec(uenv, nil, "me", "invites", "accept", "invite_00000000000000000000000000"); code == 4 {
		e.record(e.ops, "acceptMyInvite", "error-path", why)
	} else {
		e.record(e.ops, "acceptMyInvite", "fail", fmt.Sprintf("exit %d", code))
	}
	if _, _, code := e.exec(uenv, nil, "me", "invites", "decline", "invite_00000000000000000000000000"); code == 4 {
		e.record(e.ops, "declineMyInvite", "error-path", why)
	} else {
		e.record(e.ops, "declineMyInvite", "fail", fmt.Sprintf("exit %d", code))
	}

	// Profiles: a second profile holding the organization key (--with-token), switching between them.
	_, errOut, code = e.exec(uenv, strings.NewReader(os.Getenv("BMA_API_KEY")+"\n"), "login", "--with-token", "--profile", "ops",
		"--api", base, "--workspace", ws)
	e.feature(t, "login --with-token (stdin)", code == 0, short(errOut))
	_, _, code = e.exec(uenv, strings.NewReader(""), "login", "--with-token", "--profile", "empty")
	e.feature(t, "login --with-token with empty stdin → exit 2", code == 2, fmt.Sprint(code))
	list, _, _ := e.exec(uenv, nil, "profiles", "list", "--query", `[.[] | .name + (if .current then "*" else "" end)] | join(",")`)
	e.feature(t, "profiles list", strings.TrimSpace(list) == "default*,ops", list)
	_, _, code = e.exec(uenv, nil, "profiles", "use", "ops")
	st, _, _ = e.exec(uenv, nil, "auth", "status", "--query", ".profile + \" \" + .credential")
	e.feature(t, "profiles use → auth status follows", code == 0 && strings.HasPrefix(strings.TrimSpace(st), "ops "), st)
	who, _, _ := e.exec(uenv, nil, "--profile", "default", "auth", "token")
	e.feature(t, "--profile overrides the current one", strings.TrimSpace(who) == tok, "")
	who, _, _ = e.exec(envWith(uenv, "BMA_PROFILE", "default"), nil, "auth", "token")
	e.feature(t, "BMA_PROFILE overrides the current one", strings.TrimSpace(who) == tok, "")
	_, _, code = e.exec(uenv, nil, "profiles", "set", "ops", "--workspace-id", ws, "--api", base)
	_, _, code2 := e.exec(uenv, nil, "sessions", "list", "--limit", "1")
	e.feature(t, "profiles set + use the org key profile", code == 0 && code2 == 0, fmt.Sprint(code, code2))
	_, _, code = e.exec(uenv, nil, "profiles", "use", "nope")
	e.feature(t, "profiles use of an unknown profile fails", code != 0, fmt.Sprint(code))
	_, _, code = e.exec(uenv, nil, "profiles", "delete", "ops")
	list, _, _ = e.exec(uenv, nil, "profiles", "list", "--query", `[.[].name] | join(",")`)
	e.feature(t, "profiles delete", code == 0 && strings.TrimSpace(list) == "default", list)
	_, _, _ = e.exec(uenv, nil, "profiles", "use", "default")

	// Revoking the login PAT ends it; logout forgets it.
	e.userOp(t, uenv, "", 0, "me", "personal-access-tokens", "revoke", pat)
	_, _, code = e.exec(uenv, nil, "sessions", "list", "--limit", "1")
	e.feature(t, "revoked PAT → exit 3", code == 3, fmt.Sprint(code))
	_, _, code = e.exec(uenv, nil, "logout")
	_, _, code2 = e.exec(uenv, nil, "auth", "status")
	e.feature(t, "logout → auth status not signed in (exit 3)", code == 0 && code2 == 3, fmt.Sprint(code, code2))
}

func firstN(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func (e *e2e) userOp(t *testing.T, env []string, id string, want int, a ...string) string {
	t.Helper()
	out, errOut, code := e.exec(env, nil, a...)
	if code != want {
		if id != "" {
			e.record(e.ops, id, "fail", fmt.Sprintf("exit %d: %s", code, short(errOut)))
		}
		t.Errorf("%s: bma %s: exit %d\n%s", id, strings.Join(a, " "), code, errOut)
		return ""
	}
	if id != "" {
		e.record(e.ops, id, "pass", "with the user's PAT from bma login")
	}
	return strings.TrimSpace(out)
}

// login runs `bma login` and decides its code in the dashboard with decide.
func (e *e2e) login(t *testing.T, env []string, auth, api string, decide func(code string) error) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.bin, "login", "--no-browser", "--auth", auth, "--api", api)
	cmd.Env = env
	var out bytes.Buffer
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = &out, pw
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sent := false
		for sc.Scan() {
			stderr.WriteString(sc.Text() + "\n")
			if i := strings.Index(sc.Text(), "user_code="); i >= 0 && !sent {
				sent = true
				got <- sc.Text()[i+len("user_code="):]
			}
		}
	}()
	select {
	case code := <-got:
		if err := decide(strings.TrimSpace(code)); err != nil {
			t.Errorf("deciding %s: %v", code, err)
		}
	case <-ctx.Done():
	}
	err := cmd.Wait()
	_ = pw.Close()
	time.Sleep(100 * time.Millisecond)
	code := 0
	if err != nil {
		code = -1
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		}
	}
	return out.String(), stderr.String(), code
}

// dashboard is a signed-in dashboard session (the API the /device page uses).
type dashboard struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func newDashboard(t *testing.T, base, email, password string) *dashboard {
	jar := &cookieJar{m: map[string][]*http.Cookie{}}
	d := &dashboard{t: t, base: strings.TrimRight(base, "/"), client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	var s struct {
		CSRF string `json:"csrf_token"`
	}
	if err := d.do("POST", "/v1/auth/login", nil, map[string]string{"email": email, "password": password}, &s); err != nil {
		t.Fatalf("dashboard sign-in: %v", err)
	}
	d.csrf = s.CSRF
	return d
}

func (d *dashboard) do(method, path string, headers map[string]string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest(method, d.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if d.csrf != "" {
		req.Header.Set("X-CSRF-Token", d.csrf)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, short(string(body)))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// decide approves (with the organization and workspace) or denies a device code.
func (d *dashboard) decide(code, decision, org, ws string) error {
	h := map[string]string{}
	if org != "" {
		h["bma-organization-id"] = org
		h["bma-workspace-id"] = ws
	}
	return d.do("POST", "/v1/auth/device_authorizations/"+url.PathEscape(code)+"/"+decision, h, map[string]any{}, nil)
}

type cookieJar struct {
	mu sync.Mutex
	m  map[string][]*http.Cookie
}

func (j *cookieJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.m[u.Host] = append(j.m[u.Host], cs...)
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.m[u.Host]
}

// --- this computer as a BYO executor ---------------------------------------------------------------

func (e *e2e) testBYO(t *testing.T) {
	if os.Getenv("BMA_E2E_BYO") != "1" || runtime.GOOS != "linux" {
		e.feature(t, "BYO up/status/harness/upgrade/disconnect/uninstall", true,
			"skipped here: runs on Linux with systemd --user and BMA_E2E_BYO=1 (CI job byo)")
		return
	}
	executor := firstNonEmpty(os.Getenv("BMA_E2E_EXECUTOR_URL"), strings.Replace(os.Getenv("BMA_BASE_URL"), "://api.", "://executor.", 1))
	dashURL := firstNonEmpty(os.Getenv("BMA_E2E_DASHBOARD_URL"), strings.Replace(os.Getenv("BMA_BASE_URL"), "://api.", "://dashboard.", 1))
	created := jsonOf(t, e.must(t, "", "machines", "create", "--source", "byo", "--display-name", e.name("byo-up"), "--token-name", "e2e"))
	m, _ := created["id"].(string)
	token, _ := created["token"].(string)
	e.cleanup(t, "machines", "delete", m)

	// An installation in a fresh home: bma copied to ~/.bma/bin, the machine's
	// config and token as `bma up` leaves them after the device grant.
	home := filepath.Join(e.dir, "byo-home")
	bin := filepath.Join(home, ".bma", "bin", "bma")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	src, _ := os.ReadFile(e.bin)
	_ = os.WriteFile(bin, src, 0o755)
	cfg, _ := json.Marshal(map[string]any{"api_url": dashURL, "executor_url": executor, "machine_id": m,
		"workspace_id": os.Getenv("BMA_WORKSPACE_ID"), "display_name": e.name("byo-up"), "labels": []string{"e2e"},
		"platform_tools": true, "work_root": filepath.Join(home, "bma-work"), "allowed_roots": []string{}, "harnesses": map[string]string{}})
	_ = os.WriteFile(filepath.Join(home, ".bma", "config.json"), cfg, 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".config", "bma"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".config", "bma", "machine-token"), []byte(token), 0o600)
	env := envWith(os.Environ(), "HOME", home, "BMA_HOME", "", "BMA_CONFIG_DIR", "", "XDG_CONFIG_HOME", "", "BMA_API_KEY", "")
	run := func(a ...string) (string, string, int) {
		cmd := exec.Command(bin, a...)
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else if err != nil {
			code = -1
		}
		return out.String(), errb.String(), code
	}
	out, errOut, code := run("harness", "list")
	e.feature(t, "harness list", code == 0 && strings.Contains(out, "claude-code"), short(out+errOut))
	out, errOut, code = run("up", "--harness", "codex", "--labels", "e2e,cli")
	e.feature(t, "up (token kept, harness download+verify, systemd user service, connected)", code == 0 && strings.Contains(out, "Connected"), short(out+errOut))
	out, errOut, code = run("status", "--json")
	st := map[string]any{}
	_ = json.Unmarshal([]byte(out), &st)
	e.feature(t, "status --json (service running, connection ready)", code == 0 && st["service"] == "running" && st["connection"] == "ready" && st["machine_id"] == m, short(out+errOut))
	conn := e.must(t, "", "machines", "get", m, "--query", ".connection_state")
	e.feature(t, "the platform sees the BYO machine connected", conn == "connected" || conn == "ready" || conn == "online", conn)
	_, errOut, code = run("upgrade", "--check")
	e.feature(t, "upgrade --check", code == 0 && (strings.Contains(errOut, "latest") || strings.Contains(errOut, "available")), short(errOut))
	_, errOut, code = run("upgrade")
	v, _, _ := run("version")
	out, _, _ = run("status", "--json")
	st = map[string]any{}
	_ = json.Unmarshal([]byte(out), &st)
	e.feature(t, "upgrade (latest release, manifest-verified, service restarted)", code == 0 && st["service"] == "running",
		short(errOut)+" → "+strings.TrimSpace(v))
	out, errOut, code = run("disconnect")
	gone, _, gcode := e.exec(e.env, nil, "machines", "get", m, "--query", ".lifecycle_state")
	e.feature(t, "disconnect (service stopped, machine removed)", code == 0 && (gcode == 4 || strings.Contains(gone, "delet")), short(out+errOut+gone))
	_, errOut, code = run("uninstall", "--yes")
	_, binErr := os.Stat(bin)
	_, homeErr := os.Stat(filepath.Join(home, ".bma"))
	_, unitErr := os.Stat(filepath.Join(home, ".config", "systemd", "user", "bma.service"))
	e.feature(t, "uninstall --yes (binary, ~/.bma, service removed)", code == 0 && os.IsNotExist(binErr) && os.IsNotExist(homeErr) && os.IsNotExist(unitErr), short(errOut))
}
