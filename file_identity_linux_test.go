package bmaexecutor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreRunAsWritesAndEditsAsWorkload(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another uid needs root")
	}
	base, err := os.MkdirTemp("", "file-identity-")
	noErr(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	noErr(t, os.Chmod(base, 0o755))
	workspace := filepath.Join(base, "workspace")
	runAs := workloadUser
	core := newTestCore(t, CoreOptions{WorkRoot: base, Workspace: func(string, string) string { return workspace }, RunAs: &runAs})
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_files", Lease: Lease{Token: "lease"}})
	call := func(tool string, input ToolInput) ToolResultParams {
		t.Helper()
		result, err := session.CallTool(testContext(t), ToolCallParams{Tool: tool, Input: NewToolInput(input)})
		noErr(t, err)
		return result
	}
	input := ToolInput{Path: "nested/file", Content: "before"}
	if result := call(ToolNameWrite, input); result.IsError {
		t.Fatalf("write: %s", textOf(result))
	}
	input.OldString, input.NewString = "before", "after"
	if result := call(ToolNameEdit, input); result.IsError {
		t.Fatalf("edit: %s", textOf(result))
	}
	path := filepath.Join(workspace, input.Path)
	raw, err := os.ReadFile(path)
	noErr(t, err)
	if string(raw) != "after" {
		t.Fatalf("edited content = %q", raw)
	}
	info, err := os.Stat(path)
	noErr(t, err)
	if owner := fileOwner(info); owner != "65534:65534" {
		t.Fatalf("file owner = %s", owner)
	}
	// Even inside the allowed workspace, the executor's private file is not
	// writable by the workload. The parent keeps its own file identity.
	private := filepath.Join(workspace, "private")
	noErr(t, os.WriteFile(private, []byte("executor-private"), 0o600))
	for _, tool := range []string{ToolNameWrite, ToolNameEdit} {
		result := call(tool, ToolInput{Path: "private", Content: "changed", OldString: "executor-private", NewString: "changed"})
		if !result.IsError || !strings.Contains(textOf(result), "permission denied") {
			t.Fatalf("%s private file: %s", tool, textOf(result))
		}
	}
	raw, err = os.ReadFile(private)
	noErr(t, err)
	if string(raw) != "executor-private" {
		t.Fatalf("private content changed: %q", raw)
	}
	// Supplementary groups are the workload's, not the executor's.
	groupFile := filepath.Join(workspace, "group-file")
	noErr(t, os.WriteFile(groupFile, []byte("before"), 0o660))
	noErr(t, os.Chmod(groupFile, 0o660))
	noErr(t, os.Chown(groupFile, 0, 65533))
	if result := call(ToolNameEdit, ToolInput{Path: "group-file", OldString: "before", NewString: "after"}); result.IsError {
		t.Fatalf("supplementary group edit: %s", textOf(result))
	}
	// Existing workspace containment still rejects a symlink outside it.
	noErr(t, os.Symlink(base, filepath.Join(workspace, "outside")))
	if result := call(ToolNameWrite, ToolInput{Path: "outside/new-file", Content: "x"}); !result.IsError {
		t.Fatal("write escaped the workspace")
	}
	// Exercise the local route's replace_all path with the same identity.
	route, _ := newLocalRoute(t, func(o *LocalToolRouteOptions) {
		o.Core, o.Workspace = core, workspace
	})
	input = ToolInput{Path: "nested/file", OldString: "after", NewString: "all"}
	if text, failed := route.replaceAll(context.Background(), "call_r", input); failed {
		t.Fatalf("replace_all: %s", text)
	}
	if text, failed := route.replaceAll(context.Background(), "call_r", ToolInput{Path: "private", OldString: "executor-private", NewString: "x"}); !failed || !strings.Contains(text, "permission denied") {
		t.Fatalf("replace_all private: %s", text)
	}
	t.Run("concurrent", func(t *testing.T) {
		for i := range 8 {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				input := ToolInput{Path: fmt.Sprintf("parallel-%d", i), Content: "written"}
				result, err := session.CallTool(testContext(t), ToolCallParams{Tool: ToolNameWrite, Input: NewToolInput(input)})
				noErr(t, err)
				if result.IsError {
					t.Fatal(textOf(result))
				}
				info, err := os.Stat(filepath.Join(workspace, input.Path))
				noErr(t, err)
				if owner := fileOwner(info); owner != "65534:65534" {
					t.Fatalf("concurrent file owner = %s", owner)
				}
				// These unrelated goroutines must retain the executor's access.
				raw, err := os.ReadFile(private)
				noErr(t, err)
				if string(raw) != "executor-private" {
					t.Fatalf("private content = %q", raw)
				}
			})
		}
	})
}

// TestCoreRunAsReadsGlobsAndGrepsAsWorkload: read, glob and grep act with the
// workload's file identity, so they see what bash sees and nothing more.
func TestCoreRunAsReadsGlobsAndGrepsAsWorkload(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("switching to another uid needs root")
	}
	base, err := os.MkdirTemp("", "file-identity-")
	noErr(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	noErr(t, os.Chmod(base, 0o755))
	workspace := filepath.Join(base, "workspace")
	noErr(t, os.MkdirAll(workspace, 0o755))
	runAs := workloadUser
	core := newTestCore(t, CoreOptions{WorkRoot: base, Workspace: func(string, string) string { return workspace }, RunAs: &runAs})
	b := attachBrain(t, core, "local")
	session := readySession(t, b, SessionAssignParams{SessionID: "sess_files", Lease: Lease{Token: "lease"}})
	call := func(tool string, input ToolInput) ToolResultParams {
		t.Helper()
		result, err := session.CallTool(testContext(t), ToolCallParams{Tool: tool, Input: NewToolInput(input)})
		noErr(t, err)
		return result
	}
	// A workload-owned 0600 file is readable by the workload.
	owned := filepath.Join(workspace, "owned")
	noErr(t, os.WriteFile(owned, []byte("needle in owned\n"), 0o600))
	noErr(t, os.Chown(owned, int(runAs.UID), int(runAs.GID)))
	// A file only the executor may read stays unreadable to the workload.
	private := filepath.Join(workspace, "private")
	noErr(t, os.WriteFile(private, []byte("needle in private\n"), 0o600))
	// Supplementary group access is the workload's.
	grouped := filepath.Join(workspace, "grouped")
	noErr(t, os.WriteFile(grouped, []byte("needle in grouped\n"), 0o640))
	noErr(t, os.Chown(grouped, 0, 65533))

	if result := call(ToolNameRead, ToolInput{Path: "owned"}); result.IsError || !strings.Contains(textOf(result), "needle in owned") {
		t.Fatalf("read owned: %+v", textOf(result))
	}
	if result := call(ToolNameRead, ToolInput{Path: "grouped"}); result.IsError || !strings.Contains(textOf(result), "needle in grouped") {
		t.Fatalf("read grouped: %+v", textOf(result))
	}
	if result := call(ToolNameRead, ToolInput{Path: "private"}); !result.IsError || !strings.Contains(textOf(result), "permission denied") {
		t.Fatalf("read private: %+v", textOf(result))
	}
	if result := call(ToolNameGlob, ToolInput{Pattern: "*"}); result.IsError || !strings.Contains(textOf(result), "owned") {
		t.Fatalf("glob: %+v", textOf(result))
	}
	result := call(ToolNameGrep, ToolInput{Pattern: "needle"})
	if result.IsError || !strings.Contains(textOf(result), "owned:1:needle in owned") ||
		!strings.Contains(textOf(result), "grouped:1:") || strings.Contains(textOf(result), "private:") {
		t.Fatalf("grep: %+v", textOf(result))
	}
	// Workspace confinement still rejects a link out of it.
	noErr(t, os.Symlink(private, filepath.Join(workspace, "link")))
	if result := call(ToolNameRead, ToolInput{Path: "link"}); !result.IsError {
		t.Fatal("read followed a link out of the workspace")
	}
}
