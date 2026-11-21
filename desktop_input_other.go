//go:build !darwin || !cgo

package bmaexecutor

import "errors"

func newNativeDesktopInput() (NativeDesktopInput, error) {
	return nil, errors.New("native_desktop_input_unavailable")
}
