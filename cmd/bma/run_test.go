//go:build darwin || linux

package main

import (
	"path/filepath"
	"testing"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// The BYO executor declares session files and memory like BeeWork Local, with
// the process capability that syncs a session's uploads and memory.
func TestNewCoreDeclaresSessionFiles(t *testing.T) {
	t.Setenv("BMA_HOME", t.TempDir())
	c := config{WorkRoot: filepath.Join(t.TempDir(), "work")}
	core, files, err := newCore(c, func(bmaexecutor.LogEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	caps := core.Capabilities()
	want := filepath.Join(home(), "sessions")
	if caps.SessionFiles == nil || caps.SessionFiles.Root != want || !caps.SessionFiles.Memory {
		t.Fatalf("session_files = %+v, want root %s with memory", caps.SessionFiles, want)
	}
	if files == nil || files.Root() != want {
		t.Fatalf("files = %v", files)
	}
	if caps.Process == nil || !caps.Process.Enabled {
		t.Fatalf("process capability missing: %+v", caps.Process)
	}
}
