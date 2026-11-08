package bmaexecutor

import "testing"

func TestWindowsCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
		ok   bool
	}{
		{"platform argv keeps quotes and dollars verbatim",
			[]string{"cmd.exe", "/d", "/s", "/c", `powershell -NoProfile -Command "$x = 'a'; Write-Output \"$x b\""`},
			`cmd.exe /d /s /c "powershell -NoProfile -Command "$x = 'a'; Write-Output \"$x b\"""`, true},
		{"adds /s so cmd strips only the outer quotes",
			[]string{`C:\Windows\System32\CMD.EXE`, "/c", `echo "a b"`},
			`C:\Windows\System32\CMD.EXE /s /c "echo "a b""`, true},
		{"switches before /c keep Go quoting",
			[]string{"cmd", "/d", "/q", "/s", "/c", "dir"},
			`cmd /d /q /s /c "dir"`, true},
		{"command split into several arguments is joined", []string{"cmd", "/s", "/c", "echo", "hi"}, `cmd /s /c "echo hi"`, true},
		{"cmd without a command", []string{"cmd", "/c"}, "", false},
		{"cmd without /c", []string{"cmd", "/d", "x"}, "", false},
		{"other programs keep Go quoting", []string{"powershell.exe", "-Command", `echo "a"`}, "", false},
		{"cmd-like name", []string{"mycmd.exe", "/c", "x"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := windowsCommandLine(tc.argv)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("windowsCommandLine(%q) = %q, %v; want %q, %v", tc.argv, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestEscapeWindowsArg(t *testing.T) {
	for in, want := range map[string]string{
		"":           `""`,
		"plain":      "plain",
		"a b":        `"a b"`,
		`say "hi"`:   `"say \"hi\""`,
		`C:\dir\`:    `C:\dir\`,
		`C:\my dir\`: `"C:\my dir\\"`,
		`a\"b`:       `"a\\\"b"`,
	} {
		if got := escapeWindowsArg(in); got != want {
			t.Errorf("escapeWindowsArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProcessBaseEnvWindowsHasSystemVariables(t *testing.T) {
	posix := processBaseEnv("linux")
	if len(posix) != 5 {
		t.Fatalf("posix base env = %v", posix)
	}
	windows := map[string]bool{}
	for _, name := range processBaseEnv("windows") {
		windows[name] = true
	}
	for _, name := range []string{"PATH", "SystemRoot", "windir", "ComSpec", "PATHEXT", "TEMP", "TMP",
		"USERPROFILE", "USERNAME", "APPDATA", "LOCALAPPDATA", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if !windows[name] {
			t.Errorf("windows base env lacks %s", name)
		}
	}
}

func TestSetEnvValueFoldsCaseOnlyWhenAsked(t *testing.T) {
	folded := map[string]string{"PATH": "a"}
	setEnvValue(folded, "Path", "b", true)
	if len(folded) != 1 || folded["Path"] != "b" {
		t.Fatalf("folded = %v", folded)
	}
	exact := map[string]string{"PATH": "a"}
	setEnvValue(exact, "Path", "b", false)
	if len(exact) != 2 {
		t.Fatalf("exact = %v", exact)
	}
}
