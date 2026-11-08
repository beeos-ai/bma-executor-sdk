package bmaexecutor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCwdMapsTheWorkRootToTheSessionWorkspace (G7): a machine's
// configured cwd is the executor work root, which is no workspace.
func TestResolveCwdMapsTheWorkRootToTheSessionWorkspace(t *testing.T) {
	core := newTestCore(t, CoreOptions{})
	readySession(t, attachBrain(t, core, "local"), SessionAssignParams{SessionID: "sess_cwd", Lease: Lease{Token: "lease"}})
	session := core.session(coreKey{"local", "sess_cwd"})
	workspace, err := filepath.EvalSymlinks(session.Workspace())
	if err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"", core.opts.WorkRoot, core.opts.WorkRoot + string(filepath.Separator)} {
		if got, err := session.ResolveCwd(cwd); err != nil || got != workspace {
			t.Errorf("ResolveCwd(%q) = %q, %v; want %q", cwd, got, err, workspace)
		}
	}
	// Other directories stay confined.
	other := filepath.Join(core.opts.WorkRoot, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{other, filepath.Dir(core.opts.WorkRoot)} {
		if _, err := session.ResolveCwd(cwd); err == nil || !strings.Contains(err.Error(), "under the executor work root") {
			t.Errorf("ResolveCwd(%q) error = %v", cwd, err)
		}
	}
}
