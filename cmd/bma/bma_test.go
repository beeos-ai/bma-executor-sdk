package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func useHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BMA_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg"))
	return dir
}

func TestConfigAndTokenStayWithTheOwner(t *testing.T) {
	useHome(t)
	c, err := loadConfig()
	if err != nil || c.APIURL != defaultAPI || !c.PlatformTools || c.RemoteBrain {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c.MachineID = "mch_1"
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(configPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode())
	}
	if _, err := loadToken("mch_1"); err == nil {
		t.Fatal("no token yet")
	}
	where, err := saveToken("mch_1", "secret-token")
	if err != nil || where != tokenFile() {
		t.Fatalf("saved in %q: %v", where, err)
	}
	if fi, _ := os.Stat(tokenFile()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", fi.Mode())
	}
	b, _ := os.ReadFile(configPath())
	if strings.Contains(string(b), "secret-token") {
		t.Fatal("the token is not in the configuration")
	}
	if tok, err := loadToken("mch_1"); err != nil || tok != "secret-token" {
		t.Fatalf("load: %q %v", tok, err)
	}
	if err := deleteToken("mch_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken("mch_1"); err == nil {
		t.Fatal("deleted")
	}
}

func TestServiceFilesRestartFailuresOnly(t *testing.T) {
	f := serviceFiles{Label: serviceLabel, Exe: "/Users/me/.bma/bin/bma", Log: "/Users/me/Library/Logs/bma/bma.log"}
	p, err := render(plist, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>/Users/me/.bma/bin/bma</string><string>run</string>", "<key>RunAtLoad</key><true/>",
		"<key>SuccessfulExit</key><false/>", "<string>ai.beeos.bma</string>"} {
		if !strings.Contains(string(p), want) {
			t.Errorf("plist lacks %s", want)
		}
	}
	u, err := render(unit, f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/Users/me/.bma/bin/bma run", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(string(u), want) {
			t.Errorf("unit lacks %s", want)
		}
	}
}

func testHarness(url, sum string) bmaexecutor.HarnessKind {
	return bmaexecutor.HarnessKind{ID: "fake", DisplayName: "Fake", Releases: []bmaexecutor.HarnessRelease{{
		Version: "1.0.0", Binary: "fake",
		Platforms: map[string]bmaexecutor.HarnessArtifact{runtime.GOOS + "/" + runtime.GOARCH: {URL: url, SHA256: sum}},
	}}}
}

func TestInstallHarnessVerifiesThePinnedDigest(t *testing.T) {
	useHome(t)
	body := []byte("#!/bin/sh\necho fake\n")
	sum := sha256.Sum256(body)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	bad := testHarness(srv.URL, strings.Repeat("0", 64))
	if _, err := installHarness(context.Background(), bad, bad.Latest()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a digest mismatch is refused: %v", err)
	}
	if _, err := os.Stat(harnessPath(bad, bad.Latest())); err == nil {
		t.Fatal("a refused download is not installed")
	}
	good := testHarness(srv.URL, hex.EncodeToString(sum[:]))
	exe, err := installHarness(context.Background(), good, good.Latest())
	if err != nil {
		t.Fatal(err)
	}
	if exe != filepath.Join(home(), "harnesses", "fake", "1.0.0", "fake") {
		t.Fatalf("path %s", exe)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("executable")
	}
	n := gets.Load()
	if _, err := installHarness(context.Background(), good, good.Latest()); err != nil || gets.Load() != n {
		t.Fatal("a verified install is reused")
	}
}

func TestDevicePollFollowsTheGrant(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/v1/auth/device_authorizations":
			m, _ := in["bma_machine"].(map[string]any)
			if in["client_id"] != clientID || m["os"] != runtime.GOOS {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "BCDF-GHJK",
				"verification_uri": "https://d/device", "verification_uri_complete": "https://d/device?user_code=BCDF-GHJK",
				"expires_in": 60, "interval": 0})
		case "/v1/auth/device_authorizations/token":
			if polls.Add(1) < 3 {
				w.WriteHeader(http.StatusBadRequest)
				code := "authorization_pending"
				if polls.Load() == 2 {
					code = "slow_down"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{
					"type": "invalid_request_error", "code": code, "message": "wait"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "bma_machine",
				"bma_machine_id": "mch_1", "bma_workspace_id": "proj_1", "bma_display_name": "mini",
				"bma_labels": []string{"home"}, "bma_executor_url": "https://executor.example"})
		}
	}))
	defer srv.Close()
	codes, err := startDevice(context.Background(), srv.URL, "mini")
	if err != nil {
		t.Fatal(err)
	}
	codes.Interval = 0
	g, err := pollDevice(context.Background(), srv.URL, codes)
	if err != nil {
		t.Fatal(err)
	}
	if g.AccessToken != "tok" || g.MachineID != "mch_1" || g.ExecutorURL != "https://executor.example" || polls.Load() != 3 {
		t.Fatalf("grant %+v after %d polls", g, polls.Load())
	}
}

func TestHarnessChoiceUsesTheCatalog(t *testing.T) {
	c := defaultConfig()
	if err := chooseHarnesses(&c, []string{"claude-code"}, true); err != nil {
		t.Fatal(err)
	}
	h, _ := bmaexecutor.HarnessByID("claude-code")
	if c.Harnesses["claude-code"] != h.Latest().Version {
		t.Fatalf("enabled %v", c.Harnesses)
	}
	if err := chooseHarnesses(&c, []string{"nope"}, true); err == nil {
		t.Fatal("unknown harness")
	}
	c.Harnesses["claude-code"] = "0.0.1" // no longer pinned: the newest pin runs
	got, err := enabled(c)
	if err != nil || len(got) != 1 || got[0].release.Version != h.Latest().Version {
		t.Fatalf("enabled %+v %v", got, err)
	}
}
