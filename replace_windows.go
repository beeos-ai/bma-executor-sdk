//go:build windows

package bmaexecutor

import (
	"errors"
	"io/fs"
	"os"
)

// On Windows renaming over a read-only file fails (the read-only attribute is
// the 0444 of a mounted session file), unlike POSIX, where only the directory's
// permissions matter. The destination is made writable first; the renamed file
// carries its own mode.

func replaceInRoot(root *os.Root, src, dst string) error {
	err := root.Rename(src, dst)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if chmodErr := root.Chmod(dst, 0o666); chmodErr != nil {
		return err
	}
	return root.Rename(src, dst)
}

func replacePath(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if chmodErr := os.Chmod(dst, 0o666); chmodErr != nil {
		return err
	}
	return os.Rename(src, dst)
}

// removeInRoot removes name inside root, clearing a read-only attribute that
// would refuse the delete (os.Remove does the same for plain paths).
func removeInRoot(root *os.Root, name string) error {
	err := root.Remove(name)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if chmodErr := root.Chmod(name, 0o666); chmodErr != nil {
		return err
	}
	return root.Remove(name)
}
