package bmaexecutor

import (
	"strings"
)

// windowsCommandLine returns the raw command line to run argv with, or false
// when Go's own argv quoting is right.
//
// Go quotes each argument by the MSVC rules (a quote becomes \"). cmd.exe does
// not know that rule: for `cmd.exe /d /s /c <command>` the quoted form changes
// what cmd runs, and a program started from it (PowerShell -Command "...$x...")
// sees mangled quotes. The os/exec documentation says to pass cmd.exe's command
// through SysProcAttr.CmdLine verbatim. With /s cmd strips exactly the first and
// last quote of the line, so `cmd.exe /d /s /c "<command>"` runs <command>
// unchanged whatever quotes it contains. Only cmd.exe needs this; every other
// program keeps Go's quoting.
func windowsCommandLine(argv []string) (string, bool) {
	if len(argv) < 3 || !isCmdExe(argv[0]) {
		return "", false
	}
	flagAt := -1
	hasS := false
	for i, arg := range argv[1:] {
		switch strings.ToLower(arg) {
		case "/s":
			hasS = true
		case "/c", "/k", "/r":
			flagAt = i + 1
		}
		if flagAt >= 0 {
			break
		}
	}
	if flagAt < 0 || flagAt == len(argv)-1 {
		return "", false
	}
	var line strings.Builder
	line.WriteString(escapeWindowsArg(argv[0]))
	for _, flag := range argv[1 : flagAt+1] {
		line.WriteByte(' ')
		line.WriteString(escapeWindowsArg(flag))
	}
	if !hasS {
		// Insert /s before the /c-style flag.
		return windowsCommandLine(append(append(append([]string{}, argv[:flagAt]...), "/s"), argv[flagAt:]...))
	}
	line.WriteString(` "`)
	line.WriteString(strings.Join(argv[flagAt+1:], " "))
	line.WriteByte('"')
	return line.String(), true
}

func isCmdExe(name string) bool {
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	return name == "cmd" || name == "cmd.exe"
}

// escapeWindowsArg quotes one argument by the rules CommandLineToArgvW and
// the MSVC runtime parse (the same algorithm as syscall.EscapeArg).
func escapeWindowsArg(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == ' ' || c == '\t' || c == '"' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			backslashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, backslashes+1))
			backslashes = 0
		default:
			backslashes = 0
		}
		b.WriteByte(s[i])
	}
	b.WriteString(strings.Repeat(`\`, backslashes))
	b.WriteByte('"')
	return b.String()
}
