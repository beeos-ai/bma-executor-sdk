package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

func testPolicy(t *testing.T) (policy, string) {
	t.Helper()
	folder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(folder, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return policy{owner: "u_42", folders: map[string]string{"ws_7": folder}}, folder
}

func session(owner, ref string) bmaexecutor.SessionContext {
	return bmaexecutor.SessionContext{ID: "sesn_1", Metadata: map[string]string{metadataOwner: owner, metadataWorkspaceRef: ref}}
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var rpcErr *bmaexecutor.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Data == nil || rpcErr.Data.Reason != reason {
		t.Fatalf("error = %v, want reason %s", err, reason)
	}
}

func TestLaunchPolicyRefusals(t *testing.T) {
	p, _ := testPolicy(t)
	req := bmaexecutor.LaunchRequest{Argv: []string{"true"}}
	_, err := p.launch(session("u_other", "ws_7"), req)
	requireReason(t, err, bmaexecutor.ReasonSessionRejected)
	_, err = p.launch(session("u_42", "ws_unknown"), req)
	requireReason(t, err, bmaexecutor.ReasonWorkspaceUnavailable)
	for _, cwd := range []string{"..", "/etc"} {
		req.Cwd = cwd
		_, err = p.launch(session("u_42", "ws_7"), req)
		requireReason(t, err, bmaexecutor.ReasonWorkspaceBoundary)
	}
}

func initiator(value string) bmaexecutor.ProcessContext {
	return bmaexecutor.ProcessContext{Metadata: map[string]string{metadataInitiator: value}}
}

func platformRequest() bmaexecutor.LaunchRequest {
	return bmaexecutor.LaunchRequest{ProcessContext: initiator(initiatorPlatform), Argv: []string{"true"}}
}

// A process with neither agent_session_id nor initiator has no session to
// check the owner or the workspace against: it is refused instead of falling
// back to a default folder.
func TestAdmitRefusesAProcessWithoutASession(t *testing.T) {
	p, _ := testPolicy(t)
	_, err := p.admit(bmaexecutor.LaunchRequest{Argv: []string{"true"}})
	requireReason(t, err, bmaexecutor.ReasonSessionRejected)
	// initiator is an open set; only "platform" is defined.
	_, err = p.admit(bmaexecutor.LaunchRequest{ProcessContext: initiator("other"), Argv: []string{"true"}})
	requireReason(t, err, bmaexecutor.ReasonSessionRejected)
	req := bmaexecutor.LaunchRequest{Argv: []string{"true"}}
	req.AgentSessionID = "sesn_1"
	platform, err := p.admit(req)
	if err != nil || platform {
		t.Fatalf("a command of a session = platform %v, %v", platform, err)
	}
}

// A platform process (the machine desktop agent) has no session. An executor
// that does not host platform processes refuses it like any start without a
// session; one that does runs it by its own policy, never in a session folder.
func TestAdmitPlatformProcesses(t *testing.T) {
	p, _ := testPolicy(t)
	_, err := p.admit(platformRequest())
	requireReason(t, err, bmaexecutor.ReasonSessionRejected)

	p.platformDir = t.TempDir()
	platform, err := p.admit(platformRequest())
	if err != nil || !platform {
		t.Fatalf("platform process = platform %v, %v", platform, err)
	}
	install := platformRequest()
	install.PackageInstall = &bmaexecutor.ProcessPackageInstall{Manager: bmaexecutor.PackageManagerApt, Packages: []string{"jq"}}
	_, err = p.admit(install)
	requireReason(t, err, bmaexecutor.ReasonLocalConfirmationDenied)
}

func TestLaunchPlatformStaysInThePlatformDir(t *testing.T) {
	p, folder := testPolicy(t)
	p.platformDir = t.TempDir()
	spec, err := p.launchPlatform(platformRequest())
	if err != nil || spec.Dir != p.platformDir {
		t.Fatalf("spec = %+v err = %v", spec, err)
	}
	for _, cwd := range []string{"..", folder} {
		req := platformRequest()
		req.Cwd = cwd
		_, err = p.launchPlatform(req)
		requireReason(t, err, bmaexecutor.ReasonWorkspaceBoundary)
	}
}

// A package install writes outside the folder; the example offers no local
// confirmation, so it refuses.
func TestAdmitRefusesPackageInstalls(t *testing.T) {
	p, _ := testPolicy(t)
	req := bmaexecutor.LaunchRequest{Argv: []string{"apt-get", "install", "jq"},
		PackageInstall: &bmaexecutor.ProcessPackageInstall{Manager: bmaexecutor.PackageManagerApt, Packages: []string{"jq"}}}
	req.AgentSessionID = "sesn_1"
	_, err := p.admit(req)
	requireReason(t, err, bmaexecutor.ReasonLocalConfirmationDenied)
}

func TestLaunchPolicyChoosesTheFolder(t *testing.T) {
	p, folder := testPolicy(t)
	spec, err := p.launch(session("u_42", "ws_7"), bmaexecutor.LaunchRequest{Argv: []string{"true"}, Cwd: "src"})
	if err != nil || spec.Dir != filepath.Join(folder, "src") || spec.Argv != nil {
		t.Fatalf("spec = %+v err = %v", spec, err)
	}
}

// The wrapped command may write in the folder but not next to it.
func TestSandboxExecConfinesWrites(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec is macOS only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	p, folder := testPolicy(t)
	p.wrap = true
	// Temporary directories are allowed by the profile; the denied write
	// goes to the home directory.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	denied := filepath.Join(home, ".bma-sandbox-example-denied")
	t.Cleanup(func() { _ = os.Remove(denied) })
	spec, err := p.launch(session("u_42", "ws_7"), bmaexecutor.LaunchRequest{
		Argv: []string{"sh", "-c", `touch ok && touch "$1"`, "sh", denied}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("write outside the folder succeeded: %s", output)
	}
	if _, err := os.Stat(filepath.Join(folder, "ok")); err != nil {
		t.Fatalf("write inside the folder failed: %v (%s)", err, output)
	}
	if _, err := os.Stat(denied); err == nil {
		t.Fatal("the denied file exists")
	}
}
