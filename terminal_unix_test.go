//go:build !windows

package bmaexecutor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var shSpec = TerminalOpenParams{TerminalID: "trm_1", Kind: "human", Argv: []string{"/bin/sh"}, Rows: 24, Cols: 80}

func TestTerminalAttachInputResizeAndResume(t *testing.T) {
	s, h := testTerminals(t, TerminalOptions{}), newTermHost(t)
	res, err := termCall(t, s.open, MethodTerminalOpen, shSpec)
	if err != nil {
		t.Fatal(err)
	}
	opened, _ := res.(TerminalOpenResult)
	if opened.Epoch != 1 || opened.PID == 0 || opened.Rows != 24 {
		t.Fatalf("open %+v", opened)
	}
	again, _ := termCall(t, s.open, MethodTerminalOpen, shSpec)
	if r, _ := again.(TerminalOpenResult); r.PID != opened.PID {
		t.Fatal("open is idempotent by terminal_id")
	}
	other := shSpec
	other.Argv = []string{"/bin/bash"}
	if _, err := termCall(t, s.open, MethodTerminalOpen, other); termRPCCode(err) == "" {
		t.Fatalf("a different spec under a live terminal_id must conflict: %v", err)
	}

	a, ar := termAttach(t, s, h, 1, 0)
	att := a.until(func(f map[string]any) bool { return f["type"] == "attached" })
	if att["writer_epoch"] != float64(1) || att["can_write"] != true || att["pid"] != float64(opened.PID) ||
		ar.WriterEpoch != 1 {
		t.Fatalf("attached %v / %+v", att, ar)
	}
	a.send(termInput("echo CT_$((6*7))\n", 1))
	a.waitText("CT_42")
	a.send(termInput("x\n", 9))
	if e := a.until(func(f map[string]any) bool { return f["type"] == "error" }); e["code"] != terminalCodeStaleWriter {
		t.Fatalf("stale writer: %v", e)
	}
	a.send(map[string]any{"type": "resize", "rows": 30, "cols": 100})
	if r := a.until(func(f map[string]any) bool { return f["type"] == "resized" }); r["rows"] != float64(30) {
		t.Fatalf("resized %v", r)
	}
	a.send(termInput("stty size\n", 1))
	a.waitText("30 100")
	a.send(map[string]any{"type": "bogus"})
	if e := a.until(func(f map[string]any) bool { return f["type"] == "error" }); e["code"] != terminalCodeInvalidFrame {
		t.Fatalf("unknown frame: %v", e)
	}

	// One attachment at a time.
	if _, err := termCall(t, s.attach, MethodTerminalAttach, TerminalAttachParams{TerminalID: "trm_1", Epoch: 1,
		CanWrite: true, Host: h.host()}); termRPCCode(err) != terminalCodeWriterBusy {
		t.Fatalf("second attach: %v", err)
	}

	// Detach, output continues, resume from a byte offset in the middle.
	a.send(termInput("sleep 0.3; echo CT_REPLAY\n", 1))
	seq := a.next
	_ = a.c.Close(websocket.StatusNormalClosure, "drop")
	termWaitFor(t, termDetached(s))
	time.Sleep(500 * time.Millisecond)
	b, br := termAttach(t, s, h, 1, seq)
	if br.WriterEpoch != 2 {
		t.Fatalf("writer epoch %d after reattach", br.WriterEpoch)
	}
	b.until(func(f map[string]any) bool { return f["type"] == "attached" })
	b.waitText("CT_REPLAY")

	// Exit: the exit frame, the socket closes, open recreates under epoch 2.
	b.send(termInput("exit 7\n", 2))
	ex := b.until(func(f map[string]any) bool { return f["type"] == "exit" })
	if end, _ := ex["end_seq"].(float64); ex["exit_code"] != float64(7) || ex["exited"] != true || int64(end) != b.next {
		t.Fatalf("exit %v (next %d)", ex, b.next)
	}
	b.until(func(f map[string]any) bool { return f["type"] == "_closed" })
	re, _ := termCall(t, s.open, MethodTerminalOpen, shSpec)
	if r, _ := re.(TerminalOpenResult); r.Epoch != 2 {
		t.Fatalf("recreated %+v", re)
	}
	if _, err := termCall(t, s.attach, MethodTerminalAttach, TerminalAttachParams{TerminalID: "trm_1", Epoch: 1,
		CanWrite: true, Host: h.host()}); err == nil {
		t.Fatal("attach with the old epoch must be fenced")
	}
}

func TestTerminalGapAndClose(t *testing.T) {
	s, h := testTerminals(t, TerminalOptions{}), newTermHost(t)
	if _, err := termCall(t, s.open, MethodTerminalOpen, shSpec); err != nil {
		t.Fatal(err)
	}
	a, _ := termAttach(t, s, h, 1, 0)
	a.until(func(f map[string]any) bool { return f["type"] == "attached" })
	a.send(termInput("head -c 1300000 /dev/zero | tr '\\0' a; echo; echo GAP_$((1+1))\n", 1))
	a.waitText("GAP_2")
	_ = a.c.Close(websocket.StatusNormalClosure, "drop")
	termWaitFor(t, termDetached(s))

	b, _ := termAttach(t, s, h, 1, 0)
	att := b.until(func(f map[string]any) bool { return f["type"] == "attached" })
	gap := b.until(func(f map[string]any) bool { return f["type"] == "gap" })
	earliestF, _ := gap["earliest_seq"].(float64)
	latestF, _ := gap["latest_seq"].(float64)
	earliest := int64(earliestF)
	if earliest <= 0 || gap["truncated"] != true || att["earliest_seq"] != gap["earliest_seq"] ||
		int64(latestF)-earliest != terminalRingBytes {
		t.Fatalf("attached %v gap %v", att, gap)
	}
	b.next = earliest
	b.waitText("GAP_2")

	res, err := termCall(t, s.close, MethodTerminalClose, TerminalCloseParams{TerminalID: "trm_1", Reason: "user"})
	if r, _ := res.(TerminalCloseResult); err != nil || !r.Closed {
		t.Fatalf("close %v %v", res, err)
	}
	if c := b.until(func(f map[string]any) bool { return f["type"] == "closed" }); c["reason"] != "user" {
		t.Fatalf("closed %v", c)
	}
	again, _ := termCall(t, s.close, MethodTerminalClose, TerminalCloseParams{TerminalID: "trm_1"})
	if r, _ := again.(TerminalCloseResult); !r.Closed {
		t.Fatal("close is idempotent")
	}
}

// Without argv the shell starts in TerminalOptions.Dir and names it in PWD;
// a service-started executor has no locale, yet the shell is in a UTF-8 one
// and a Chinese file name round-trips through the terminal.
func TestTerminalStartsInDirWithUTF8(t *testing.T) {
	dir := t.TempDir()
	base := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "LC_") || strings.HasPrefix(kv, "LANG=") || strings.HasPrefix(kv, "PWD=")
	})
	s := testTerminals(t, TerminalOptions{Env: append(base, "PS1=$ ", "PWD=/nonexistent-executor-dir", "SHELL=/bin/sh"), Dir: dir})
	h := newTermHost(t)
	spec := shSpec
	spec.Argv = nil
	if _, err := termCall(t, s.open, MethodTerminalOpen, spec); err != nil {
		t.Fatal(err)
	}
	a, _ := termAttach(t, s, h, 1, 0)
	a.send(termInput("echo \"DIR_IS_$(pwd -P)_${PWD}_$LANG\"; touch 中文.txt; echo OK_$(ls)\n", 1))
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.waitText("OK_中文.txt")
	if want := "DIR_IS_" + resolved + "_" + dir + "_C.UTF-8"; !strings.Contains(a.text, want) {
		t.Fatalf("terminal output lacks %q: %q", want, a.text)
	}
}

// The Launch hook sees the command, directory and environment the terminal
// would get and may replace them; its *RPCError refuses the open as is.
func TestTerminalLaunchHook(t *testing.T) {
	dir := t.TempDir()
	var seen TerminalLaunchRequest
	refuse := false
	s := testTerminals(t, TerminalOptions{
		DenyEnv: func(name string) bool { return name == "SECRET" },
		Launch: func(_ context.Context, req TerminalLaunchRequest) (LaunchSpec, error) {
			seen = req
			if refuse {
				return LaunchSpec{}, NewReasonError(CodeForbidden, "terminal_refused", "not here")
			}
			return LaunchSpec{Dir: dir, Argv: append([]string{"/usr/bin/env", "WRAPPED=1"}, req.Argv...),
				Env: append(req.Env, "HOOKED=yes")}, nil
		},
	})
	spec := shSpec
	spec.Env = map[string]string{"SECRET": "x", "MINE": "y"}
	if _, err := termCall(t, s.open, MethodTerminalOpen, spec); err != nil {
		t.Fatal(err)
	}
	if seen.TerminalID != "trm_1" || seen.Kind != "human" || !slices.Equal(seen.Argv, []string{"/bin/sh"}) ||
		!slices.Contains(seen.Env, "MINE=y") || slices.Contains(seen.Env, "SECRET=x") ||
		!slices.Contains(seen.Env, "TERM=xterm-256color") {
		t.Fatalf("launch request %+v", seen)
	}
	a, _ := termAttach(t, s, newTermHost(t), 1, 0)
	a.send(termInput("echo \"R_${WRAPPED}_${HOOKED}_$(pwd -P)\"\n", 1))
	resolved, _ := filepath.EvalSymlinks(dir)
	a.waitText("R_1_yes_" + resolved)

	refuse = true
	other := shSpec
	other.TerminalID = "trm_2"
	_, err := termCall(t, s.open, MethodTerminalOpen, other)
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeForbidden || rpc.Data == nil || rpc.Data.Reason != "terminal_refused" {
		t.Fatalf("refused open: %v", err)
	}
}
