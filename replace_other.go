//go:build !windows

package bmaexecutor

import "os"

// replaceInRoot renames src over dst inside root; rename replaces a read-only
// destination on POSIX.
func replaceInRoot(root *os.Root, src, dst string) error { return root.Rename(src, dst) }

// replacePath renames src over dst.
func replacePath(src, dst string) error { return os.Rename(src, dst) }

// removeInRoot removes name inside root.
func removeInRoot(root *os.Root, name string) error { return root.Remove(name) }
