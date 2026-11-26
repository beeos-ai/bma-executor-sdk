//go:build windows

package bmaexecutor

import (
	"testing"
)

// The default shell (COMSPEC, cmd.exe) on a pseudo console: input, output,
// resize, exit code, and a close that ends a running terminal.
func TestTerminalConPTY(t *testing.T) {
	s, h := testTerminals(t, TerminalOptions{}), newTermHost(t)
	spec := TerminalOpenParams{TerminalID: "trm_1", Kind: "human", Rows: 24, Cols: 80}
	res, err := termCall(t, s.open, MethodTerminalOpen, spec)
	if err != nil {
		t.Fatal(err)
	}
	if opened, _ := res.(TerminalOpenResult); opened.PID == 0 || opened.Epoch != 1 {
		t.Fatalf("open %+v", res)
	}
	a, _ := termAttach(t, s, h, 1, 0)
	a.until(func(f map[string]any) bool { return f["type"] == "attached" })
	a.send(termInput("set /a 6*7+1000\r", 1))
	a.waitText("1042")
	a.send(map[string]any{"type": "resize", "rows": 30, "cols": 100})
	if r := a.until(func(f map[string]any) bool { return f["type"] == "resized" }); r["cols"] != float64(100) {
		t.Fatalf("resized %v", r)
	}
	a.send(termInput("mode con\r", 1))
	a.waitText("100")
	a.send(termInput("exit 7\r", 1))
	if ex := a.until(func(f map[string]any) bool { return f["type"] == "exit" }); ex["exit_code"] != float64(7) {
		t.Fatalf("exit %v", ex)
	}

	other := spec
	other.TerminalID = "trm_2"
	if _, err := termCall(t, s.open, MethodTerminalOpen, other); err != nil {
		t.Fatal(err)
	}
	res, err = termCall(t, s.close, MethodTerminalClose, TerminalCloseParams{TerminalID: "trm_2", Reason: "user"})
	if r, _ := res.(TerminalCloseResult); err != nil || !r.Closed {
		t.Fatalf("close %v %v", res, err)
	}
}
