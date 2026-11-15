package bmaexecutor

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// withFileIdentity runs fn on a dedicated OS thread whose file system
// identity (fsuid, fsgid, supplementary groups) is the workload's. These
// are per-thread syscalls (unlike syscall.Setuid), so the rest of the executor keeps
// root. The thread is never unlocked: Go discards it when the goroutine ends
// instead of reusing a thread with changed credentials.
func withFileIdentity(workload *RunAs, fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		groups := make([]int, len(workload.Groups))
		for i, id := range workload.Groups {
			groups[i] = int(id)
		}
		if err := unix.Setgroups(groups); err != nil {
			result <- fmt.Errorf("set workload groups: %w", err)
			return
		}
		_ = unix.Setfsgid(int(workload.GID))
		_ = unix.Setfsuid(int(workload.UID))
		// setfs[ug]id reports the previous id, not failures: read them back
		// (an invalid id changes nothing and returns the current one).
		gid, _ := unix.SetfsgidRetGid(-1)
		uid, _ := unix.SetfsuidRetUid(-1)
		if uid != int(workload.UID) || gid != int(workload.GID) {
			result <- fmt.Errorf("switch file identity to %d:%d: got %d:%d", workload.UID, workload.GID, uid, gid)
			return
		}
		result <- fn()
	}()
	return <-result
}
