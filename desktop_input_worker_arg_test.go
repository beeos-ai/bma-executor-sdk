package bmaexecutor

import "testing"

func TestNativeInputWorkerArgumentIsNeutral(t *testing.T) {
	cmd := newNativeInputCommand("/bin/executor")
	if len(cmd.Args) != 2 || cmd.Args[1] != "--native-input-worker" {
		t.Fatalf("worker argv = %q", cmd.Args)
	}
}
