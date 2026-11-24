//go:build !windows

package bmaexecutor

import (
	"context"
	"strings"
	"testing"
)

// TestLocalBashCommandWritesToAPipe: the command's stdout and stderr are one
// pipe, not the output file, so a sandbox that denies the output file's path
// does not reach the command (see toolroute_local_bash_darwin_test.go), and
// both streams keep their order.
func TestLocalBashCommandWritesToAPipe(t *testing.T) {
	rig := newBashRig(t)
	got := rig.route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: map[string]any{
		"command": `[ -p /dev/stdout ] && echo stdout-pipe; [ -p /dev/stderr ] && echo stderr-pipe >&2; echo one; echo two >&2; echo three`,
	}})
	wantExecuted(t, got)
	want := "stdout-pipe\nstderr-pipe\none\ntwo\nthree\n\nexit_code: 0"
	if got.IsError || got.Text != want {
		t.Fatalf("text = %q, want %q", got.Text, want)
	}
}

// TestLocalBashFinishedAnswerHasAllOutput: a command that ends within the wait
// answers with everything it wrote, also more than a pipe buffer holds.
func TestLocalBashFinishedAnswerHasAllOutput(t *testing.T) {
	rig := newBashRig(t)
	got := rig.route.Call(context.Background(), RouteCall{ID: "c1", Name: "bash", Input: map[string]any{
		"command": `i=0; while [ $i -lt 20000 ]; do echo line-$i; i=$((i+1)); done; echo end >&2`,
	}})
	wantExecuted(t, got)
	if got.IsError || !strings.Contains(got.Text, "line-0\n") || !strings.HasSuffix(got.Text, "line-19999\nend\n\nexit_code: 0") {
		t.Fatalf("text has %d bytes, tail %q", len(got.Text), got.Text[max(0, len(got.Text)-80):])
	}
}
