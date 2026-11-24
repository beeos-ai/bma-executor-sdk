//go:build darwin

package bmaexecutor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalBashInASandboxThatDeniesTheOutputDir: BeeWork Local runs the
// brain's bash under a Seatbelt profile that denies reading and writing its
// data directory, where CoreOptions.ToolOutputDir is. Programs that inspect a
// regular-file stdout (raw cat, dd) then failed with "stdout: Operation not
// permitted"; with a pipe they run as anywhere else.
func TestLocalBashInASandboxThatDeniesTheOutputDir(t *testing.T) {
	outputs, err := filepath.EvalSymlinks(t.TempDir())
	noErr(t, err)
	outputs = filepath.Join(outputs, "brain-output")
	profile := `(version 1)(allow default)(deny file-read* file-write* (subpath "` + outputs + `"))`
	rig := newBashRigWith(t, CoreOptions{ToolOutputDir: outputs, Launch: func(_ context.Context, req LaunchRequest) (LaunchSpec, error) {
		return LaunchSpec{Argv: []string{"/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", req.Argv[len(req.Argv)-1]}}, nil
	}})
	for _, tc := range []struct{ command, want string }{
		{`printf 'a\n' > in.txt && cat in.txt`, "a\n"},
		{`printf 'b\n' > in.txt && cat in.txt >&2`, "b\n"},
		{`printf 'c\n' | cat`, "c\n"},
		{`printf 'd\n' > in.txt && dd if=in.txt status=none`, "d\n"},
		{`printf 'e\n' > in.txt && cp in.txt copy.txt && cat copy.txt`, "e\n"},
		{`printf 'f\n' | tee tee.txt && cat tee.txt`, "f\nf\n"},
		{`printf 'g\n' > in.txt && cat < in.txt > redirect.txt && cat redirect.txt`, "g\n"},
	} {
		got := rig.route.Call(context.Background(), RouteCall{ID: "c-" + tc.command, Name: "bash", Input: map[string]any{"command": tc.command}})
		wantExecuted(t, got)
		if want := tc.want + "\nexit_code: 0"; got.IsError || got.Text != want || strings.Contains(got.Text, "Operation not permitted") {
			t.Errorf("%s: text = %q, want %q", tc.command, got.Text, want)
		}
	}
}
