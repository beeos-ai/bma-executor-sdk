//go:build windows

package bmaexecutor

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	helperRawInput = func() {
		stdin := windows.Handle(os.Stdin.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(stdin, &mode); err != nil {
			fmt.Println("no console input:", err)
			os.Exit(3)
		}
		// Raw, VT input: no line editing, no processed ^C.
		if err := windows.SetConsoleMode(stdin, windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
			fmt.Println("set mode:", err)
			os.Exit(3)
		}
		fmt.Println("ready")
		buf := make([]byte, 1)
		n, err := os.Stdin.Read(buf)
		if err != nil || n != 1 {
			fmt.Println("read:", n, err)
			os.Exit(4)
		}
		fmt.Printf("got=%d\n", buf[0])
	}
}
