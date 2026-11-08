package bmaexecutor

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"time"
)

// runTestHelper is the test binary acting as a command the process tests
// start (BMA_TEST_HELPER=<mode>), so they need no shell tools.
//
//	sleep      sleeps (a grandchild).
//	tree       starts a "sleep" grandchild, writes its pid to BMA_TEST_PIDFILE, sleeps.
//	orphan     like tree, then exits at once.
//	interrupt  prints "ready", waits for an interrupt, prints "interrupted", exits 0.
//	isatty     prints tty=true when stdout is a terminal.
//	rawinput   (Windows) reads one byte from a raw console input, prints "got=<byte>".
//	echo       copies a line from stdin to stdout, prefixed "echo:".
func runTestHelper(mode string) {
	switch mode {
	case "sleep":
		time.Sleep(10 * time.Minute)
	case "tree", "orphan":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "BMA_TEST_HELPER=sleep")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		pidfile := os.Getenv("BMA_TEST_PIDFILE")
		if err := os.WriteFile(pidfile+".tmp", []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(2)
		}
		if err := os.Rename(pidfile+".tmp", pidfile); err != nil {
			os.Exit(2)
		}
		if mode == "tree" {
			time.Sleep(10 * time.Minute)
		}
	case "interrupt":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		fmt.Println("ready")
		<-ch
		fmt.Println("interrupted")
	case "rawinput":
		if helperRawInput == nil {
			os.Exit(2)
		}
		helperRawInput()
	case "isatty":
		info, err := os.Stdout.Stat()
		fmt.Printf("tty=%v\n", err == nil && info.Mode()&os.ModeCharDevice != 0)
	case "echo":
		var line string
		_, _ = fmt.Scanln(&line)
		fmt.Println("echo:" + line)
	}
}

// helperRawInput is set on Windows (helper_windows_test.go).
var helperRawInput func()
