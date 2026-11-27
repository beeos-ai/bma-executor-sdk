//go:build !windows

package bmaexecutor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sessionUsers gives each test session its own unprivileged identity.
var sessionUsers = map[string]RunAs{
	"sess_a": {UID: 61001, GID: 61001, Home: "/nonexistent-a", User: "bmas-a"},
	"sess_b": {UID: 61002, GID: 61002, Home: "/nonexistent-b", User: "bmas-b"},
}

func sessionUser(id string) (*RunAs, error) {
	runAs, ok := sessionUsers[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &runAs, nil
}

// TestSessionRunAsKeepsSessionsApart: with SessionRunAs every session's bash,
// processes and file tools run as the session's own identity, its workspace
// is that identity's (0700) below an executor-owned 0711 brain directory, so
// session B can neither list nor read session A's workspace. Needs root.
func TestSessionRunAsKeepsSessionsApart(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another uid needs root")
	}
	ctx := testContext(t)
	root, err := os.MkdirTemp("", "session-runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	machine := workloadUser
	alias := filepath.Join(root, "machine-home")
	if err := os.Mkdir(alias, 0o755); err != nil {
		t.Fatal(err)
	}
	core := newTestCore(t, CoreOptions{WorkRoot: root, WorkRootAliases: []string{alias}, RunAs: &machine,
		SessionRunAs: func(_ string, p SessionAssignParams) (*RunAs, error) { return sessionUser(p.SessionID) }})
	if err := core.Register(NewProcessCapability(core, ProcessOptions{PackageInstaller: identityInstaller{}})); err != nil {
		t.Fatal(err)
	}
	b := attachBrain(t, core, "local")
	a := readySession(t, b, SessionAssignParams{SessionID: "sess_a", Lease: Lease{Token: "lease-a"}})
	bs := readySession(t, b, SessionAssignParams{SessionID: "sess_b", Lease: Lease{Token: "lease-b"}})

	if got := bashOut(t, a, `printf '%s|%s|%s' "$(id -u)" "$HOME" "$USER"; printf secret > a.txt`); got != "61001|/nonexistent-a|bmas-a" {
		t.Fatalf("session a bash identity = %q", got)
	}
	var state ProcessState
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: bs.ID(), ProcessID: "id", Argv: []string{"id", "-u"}, WaitMS: 5000,
	}, &state); err != nil || state.State != ProcessExited {
		t.Fatalf("process = %+v err %v", state, err)
	}
	if got := strings.TrimSpace(readAll(t, b.executor, bs.ID(), "id")); got != "61002" {
		t.Fatalf("session b process uid = %q", got)
	}
	// A cwd equal to a work root alias (the machine's configured cwd) is the session's workspace.
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: bs.ID(), ProcessID: "pwd", Argv: []string{"pwd"}, Cwd: alias, WaitMS: 5000,
	}, &state); err != nil || state.State != ProcessExited {
		t.Fatalf("pwd = %+v err %v", state, err)
	}
	if got := strings.TrimSpace(readAll(t, b.executor, bs.ID(), "pwd")); got != core.workspace("local", "sess_b") {
		t.Fatalf("cwd alias resolves to %q", got)
	}
	// A user-level install runs as the session with the session's HOME.
	if err := b.executor.Call(ctx, MethodProcessStart, ProcessStartParams{
		SessionID: bs.ID(), ProcessID: "pip", WaitMS: 5000,
		PackageInstall: &ProcessPackageInstall{Manager: "pip", Packages: []string{"requests"}},
	}, &state); err != nil || state.State != ProcessExited {
		t.Fatalf("install = %+v err %v", state, err)
	}
	if got := strings.TrimSpace(readAll(t, b.executor, bs.ID(), "pip")); got != "61002|/nonexistent-b|bmas-b" {
		t.Fatalf("session b install identity = %q", got)
	}
	brainDir := filepath.Join(root, "local")
	for dir, want := range map[string]string{brainDir: "0:0 drwx--x--x", core.workspace("local", "sess_a"): "61001:61001 drwx------",
		core.workspace("local", "sess_b"): "61002:61002 drwx------"} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileOwner(info) + " " + info.Mode().String(); got != want {
			t.Fatalf("%s = %s, want %s", dir, got, want)
		}
	}
	secret := filepath.Join(core.workspace("local", "sess_a"), "a.txt")
	for _, probe := range []string{"cat " + secret, "ls " + brainDir, "ls " + core.workspace("local", "sess_a")} {
		if got := bashOut(t, bs, probe+" >/dev/null 2>&1 && echo LEAK || echo denied"); got != "denied" {
			t.Fatalf("session b %q: %s", probe, got)
		}
	}
	// The file tools of b use b's identity: an absolute path into a's
	// workspace is refused before it is opened.
	read, err := bs.CallTool(ctx, ToolCallParams{Tool: ToolNameRead, Input: NewToolInput(ToolInput{Path: secret})})
	if err == nil && !read.IsError {
		t.Fatalf("session b read a's file: %q", textOf(read))
	}
	// A refused identity refuses the assign.
	if _, err := b.executor.AssignSession(ctx, SessionAssignParams{SessionID: "sess_c", Lease: Lease{Token: "lease-c"}}); err == nil {
		t.Fatal("an assign without a session identity was accepted")
	}
}

// TestSessionFilesBelongToTheSession: with SessionFilesOptions.RunAs the
// session directory is the executor's with the session's group (0750) and
// outputs the session's (0700); another session's identity reaches neither,
// and a link in place of the session directory is refused. Needs root.
func TestSessionFilesBelongToTheSession(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root")
	}
	root, err := os.MkdirTemp("", "session-files-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	files, err := NewSessionFiles(SessionFilesOptions{Root: root, RunAs: sessionUser})
	if err != nil {
		t.Fatal(err)
	}
	a := files.session("sess_a")
	if err := files.sessionDirs(a); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.uploads, "up.txt"), []byte("upload"), 0o444); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{filepath.Dir(a.uploads): "0:61001 drwxr-x---", a.outputs: "61001:61001 drwx------"} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileOwner(info) + " " + info.Mode().String(); got != want {
			t.Fatalf("%s = %s, want %s", dir, got, want)
		}
	}
	for id, want := range map[string]string{"sess_a": "upload", "sess_b": ""} {
		runAs, _ := sessionUser(id)
		cmd := exec.Command("cat", filepath.Join(a.uploads, "up.txt"))
		setCredential(cmd, runAs)
		out, _ := cmd.Output()
		if string(out) != want {
			t.Fatalf("%s reads a's upload: %q, want %q", id, out, want)
		}
	}
	b := files.session("sess_b")
	if err := os.Symlink(filepath.Dir(a.uploads), filepath.Dir(b.uploads)); err != nil {
		t.Fatal(err)
	}
	if err := files.sessionDirs(b); err == nil {
		t.Fatal("a link in place of the session directory was accepted")
	}
}

// identityInstaller's user-level install prints who it runs as.
type identityInstaller struct{}

func (identityInstaller) PackageInstallCommand(string, ProcessPackageInstall) (PackageInstallCommand, error) {
	return PackageInstallCommand{Argv: []string{"/bin/sh", "-c", `printf '%s|%s|%s' "$(id -u)" "$HOME" "$USER"`}, Dir: "/",
		Env: []string{"PATH=/usr/bin:/bin", "HOME=/machine-home"}}, nil
}

func bashOut(t *testing.T, s *Session, command string) string {
	t.Helper()
	res, err := s.CallTool(testContext(t), ToolCallParams{Tool: ToolNameBash, Input: NewToolInput(ToolInput{Command: command})})
	if err != nil || res.IsError {
		t.Fatalf("bash %q = %q err %v", command, textOf(res), err)
	}
	out, _, _ := strings.Cut(textOf(res), "exit_code:")
	return strings.TrimSpace(out)
}
