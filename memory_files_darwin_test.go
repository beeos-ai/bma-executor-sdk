//go:build darwin

package bmaexecutor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A macOS sandbox-exec (Seatbelt) profile that confines a process to its own
// session reads the session's memory mirror: the mirror is in place before
// the Launch hook runs, and the profile names files.MemoryDir. The other
// session's memory stays unreadable.
func TestSessionMemoryReadableInsideSeatbelt(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}
	daemon := &fakeDaemonFiles{
		processes: map[string]string{"proc_a": "sesn_a", "proc_b": "sesn_b"},
		memory: map[string][]*fakeMemoryStore{
			"sesn_a": {{id: "memstore_a", slug: "prefs", files: map[string][]byte{"/lang.md": []byte("zh-CN")}}},
			"sesn_b": {{id: "memstore_b", slug: "other", files: map[string][]byte{"/b.md": []byte("bravo")}}},
		},
	}
	const profile = `(version 1)
(allow default)
(deny file-write*)
(allow file-write* (literal "/dev/null"))
(deny file-read* (subpath (param "ROOT")))
(allow file-read* (subpath (param "MEMORY")))`
	h, _, root := newSessionFilesHarness(t, daemon, func(files *SessionFiles) LaunchFunc {
		return func(_ context.Context, req LaunchRequest) (LaunchSpec, error) {
			if _, err := os.Stat(filepath.Join(files.MemoryDir(req.AgentSessionID), sessionMemoryMarker)); err != nil {
				t.Errorf("memory not mirrored before Launch: %v", err)
			}
			return LaunchSpec{Argv: append([]string{"/usr/bin/sandbox-exec", "-D", "ROOT=" + files.Root(),
				"-D", "MEMORY=" + files.MemoryDir(req.AgentSessionID), "-p", profile}, req.Argv...)}, nil
		}
	})
	startIn(t, h, "proc_b", "sesn_b", "true")
	got := startIn(t, h, "proc_a", "sesn_a", "/bin/sh", "-c",
		`cat "$BMA_MEMORY_DIR/prefs/lang.md"; echo; cat "$1" 2>/dev/null || echo denied`, "sh",
		filepath.Join(root, "sesn_b", "memory", "other", "b.md"))
	if got != "zh-CN\ndenied\n" {
		t.Fatalf("sandboxed process saw %q", got)
	}
	if strings.Contains(got, "bravo") {
		t.Fatal("another session's memory was readable")
	}
}
