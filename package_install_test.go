//go:build !windows

package bmaexecutor

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// fakeInstaller records the installs it was asked for and runs a command
// that echoes the packages, standing in for a real package manager.
type fakeInstaller struct {
	mu         sync.Mutex
	installs   []ProcessPackageInstall
	privileged bool
}

func (f *fakeInstaller) PackageInstallCommand(_ string, install ProcessPackageInstall) (PackageInstallCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = append(f.installs, install)
	return PackageInstallCommand{
		Argv:       append([]string{"sh", "-c", `printf '%s:%s' "$(id -u)" "$*"`, "sh", install.Manager}, install.Packages...),
		Env:        []string{"PATH=" + os.Getenv("PATH")},
		Privileged: f.privileged,
	}, nil
}

func (f *fakeInstaller) calls() []ProcessPackageInstall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ProcessPackageInstall(nil), f.installs...)
}

// TestPackageInstallRunsTheInstallerCommandAsAProcess: a process.start
// package_install runs the command the installer builds, with its output as
// the process output, and is advertised as process.package_install.
func TestPackageInstallRunsTheInstallerCommandAsAProcess(t *testing.T) {
	installer := &fakeInstaller{}
	core := newTestCore(t, CoreOptions{})
	capability := NewProcessCapability(core, ProcessOptions{PackageInstaller: installer})
	var advertised Capabilities
	capability.Advertise(&advertised)
	if advertised.Process == nil || !advertised.Process.PackageInstall {
		t.Fatalf("process capability = %+v, want package_install", advertised.Process)
	}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{PackageInstaller: installer})
	state := h.mustStart(t, ProcessStartParams{
		ProcessID: "pkg", Stderr: StderrMerge, WaitMS: 5000,
		PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: []string{"jq", "libssl3", "g++", "python3.11"}},
	})
	if state.State != ProcessExited || *state.ExitCode != 0 {
		t.Fatalf("package install = %+v", state)
	}
	want := strings.TrimSpace(must(runID())) + ":apt jq libssl3 g++ python3.11"
	if got := readAll(t, h.executor, h.session, "pkg"); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	// A repeated start is idempotent: the installer runs once.
	h.mustStart(t, ProcessStartParams{
		ProcessID: "pkg", Stderr: StderrMerge,
		PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: []string{"jq", "libssl3", "g++", "python3.11"}},
	})
	if _, err := h.start(t, ProcessStartParams{
		ProcessID: "pkg", Stderr: StderrMerge,
		PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: []string{"curl"}},
	}); err == nil {
		t.Fatal("a different install under the same process_id was accepted")
	}
	if calls := installer.calls(); len(calls) != 1 {
		t.Fatalf("installer calls = %+v, want one", calls)
	}
}

// TestPackageInstallRejectsInjection: package names are plain names, the
// manager is one of the contract's, and package_install excludes every other
// way to run something. Nothing reaches the installer.
func TestPackageInstallRejectsInjection(t *testing.T) {
	installer := &fakeInstaller{}
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{PackageInstaller: installer, GitCredentials: nil})
	apt := func(packages ...string) *ProcessPackageInstall {
		return &ProcessPackageInstall{Manager: PackageManagerApt, Packages: packages}
	}
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = "p"
	}
	for name, params := range map[string]ProcessStartParams{
		"option":         {PackageInstall: apt("-oDebug::pkgProblemResolver=1")},
		"long option":    {PackageInstall: apt("jq", "--allow-unauthenticated")},
		"shell":          {PackageInstall: apt("jq;id")},
		"substitution":   {PackageInstall: apt("$(id)")},
		"space":          {PackageInstall: apt("jq curl")},
		"path":           {PackageInstall: apt("./evil.deb")},
		"url":            {PackageInstall: apt("https://evil.test/x.deb")},
		"version pin":    {PackageInstall: apt("jq=1.6")},
		"release":        {PackageInstall: apt("jq/sid")},
		"empty name":     {PackageInstall: apt("")},
		"no packages":    {PackageInstall: apt()},
		"too many":       {PackageInstall: apt(tooMany...)},
		"too long":       {PackageInstall: apt(strings.Repeat("a", 129))},
		"newline":        {PackageInstall: apt("jq\ncurl")},
		"unknown":        {PackageInstall: &ProcessPackageInstall{Manager: "brew", Packages: []string{"jq"}}},
		"with argv":      {PackageInstall: apt("jq"), Argv: []string{"sh"}},
		"with pty":       {PackageInstall: apt("jq"), IOMode: IOModePTY},
		"with git":       {PackageInstall: apt("jq"), GitCredentials: true},
		"with transfer":  {PackageInstall: apt("jq"), FileTransfer: &ProcessFileTransfer{Direction: FileTransferUpload, LocalPath: "/x"}},
		"scoped npm":     {PackageInstall: &ProcessPackageInstall{Manager: PackageManagerNpm, Packages: []string{"@evil/pkg"}}},
		"pip extra spec": {PackageInstall: &ProcessPackageInstall{Manager: PackageManagerPip, Packages: []string{"requests[socks]"}}},
	} {
		params.ProcessID = "inject-" + strings.ReplaceAll(name, " ", "-")
		if _, err := h.start(t, params); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			requireCode(t, err, CodeInvalidParams)
		}
	}
	if calls := installer.calls(); len(calls) != 0 {
		t.Fatalf("installer reached with %+v", calls)
	}
}

// TestPackageInstallWithoutInstallerIsUnavailable: an executor without a
// PackageInstaller answers capability_unavailable naming the capability.
func TestPackageInstallWithoutInstallerIsUnavailable(t *testing.T) {
	h := newProcessHarness(t, CoreOptions{}, ProcessOptions{})
	_, err := h.start(t, ProcessStartParams{
		ProcessID: "pkg", PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: []string{"jq"}},
	})
	if rpcErr := requireCode(t, err, CodeCapabilityUnavailable); rpcErr.Data == nil || rpcErr.Data.Capability != "process.package_install" {
		t.Fatalf("error = %+v", rpcErr)
	}
}

// TestPrivilegedPackageInstallKeepsTheExecutorIdentity: with RunAs, a
// privileged install (system packages) runs as the executor while an
// unprivileged one (user-level packages) runs as the workload user. Changing
// uid needs root.
func TestPrivilegedPackageInstallKeepsTheExecutorIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another uid needs root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runAs := workloadUser
	for _, test := range []struct {
		privileged bool
		want       string
	}{{true, "0:apt jq"}, {false, "65534:apt jq"}} {
		installer := &fakeInstaller{privileged: test.privileged}
		h := newProcessHarness(t, CoreOptions{WorkRoot: root, RunAs: &runAs}, ProcessOptions{PackageInstaller: installer})
		h.mustStart(t, ProcessStartParams{
			ProcessID: "pkg", Stderr: StderrMerge, WaitMS: 5000,
			PackageInstall: &ProcessPackageInstall{Manager: PackageManagerApt, Packages: []string{"jq"}},
		})
		if got := readAll(t, h.executor, h.session, "pkg"); got != test.want {
			t.Fatalf("privileged=%t: output = %q, want %q", test.privileged, got, test.want)
		}
	}
}

func runID() (string, error) {
	out, err := exec.Command("id", "-u").Output()
	return string(out), err
}
