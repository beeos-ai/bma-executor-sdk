package bmaexecutor

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
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
//	cua        a fake cua-driver: `call <tool> <json> [--screenshot-out-file f]`.
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
	case "cua":
		runFakeCua()
	case "echo":
		var line string
		_, _ = fmt.Scanln(&line)
		fmt.Println("echo:" + line)
	}
}

// onePixelPNG is a valid 1x1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// runFakeCua answers like cua-driver: it logs each call's tool and JSON to
// BMA_TEST_CUA_LOG and prints a result shaped like the real driver's.
func runFakeCua() {
	args := os.Args[1:]
	if os.Getenv("BMA_TEST_CUA_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "daemon is not running")
		os.Exit(1)
	}
	if len(args) < 3 || args[0] != "call" {
		os.Exit(64)
	}
	tool, payload := args[1], args[2]
	screenshot := ""
	for i, arg := range args {
		if arg == "--screenshot-out-file" && i+1 < len(args) {
			screenshot = args[i+1]
		}
	}
	if logPath := os.Getenv("BMA_TEST_CUA_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(file, "%s %s telemetry=%s\n", tool, payload, os.Getenv("CUA_TELEMETRY"))
			_ = file.Close()
		}
	}
	writePNG := func() {
		raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
		_ = os.WriteFile(screenshot, raw, 0o600)
	}
	var result any
	switch tool {
	case "get_screen_size":
		result = map[string]any{"width": 1024, "height": 768}
	case "get_desktop_state":
		writePNG()
		result = map[string]any{"screenshot_width": 1024, "screenshot_height": 768}
	case "get_window_state":
		// The bundled macOS 0.34.0 driver returns tree_markdown by default,
		// but rejects the newer tree_format input field.
		var input map[string]any
		_ = json.Unmarshal([]byte(payload), &input)
		if os.Getenv("BMA_TEST_CUA_STRICT_WINDOW") == "1" {
			if _, present := input["tree_format"]; present {
				fmt.Fprintln(os.Stderr, "get_window_state: unknown argument tree_format")
				os.Exit(1)
			}
		}
		writePNG()
		result = map[string]any{"screenshot_width": 754, "screenshot_height": 512,
			"tree_markdown": "- Window \"Untitled - Notepad\"\n  - [0] Edit \"Text Editor\""}
	case "list_windows":
		result = map[string]any{"windows": []any{
			map[string]any{"app_name": "notepad.exe", "pid": 1208, "window_id": 983170, "title": "Untitled - Notepad",
				"is_on_screen": true, "bounds": map[string]any{"x": 111, "y": 104, "width": 754, "height": 512}},
			map[string]any{"app_name": "hidden.exe", "pid": 1, "window_id": 2, "title": "gone", "is_on_screen": false},
		}}
	default:
		result = map[string]any{"summary": "did " + tool + " " + strings.TrimSpace(payload)}
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

// helperRawInput is set on Windows (helper_windows_test.go).
var helperRawInput func()
