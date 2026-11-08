package bmaexecutor

import (
	"runtime"
	"strings"
)

// envCaseInsensitive reports whether environment variable names compare
// case-insensitively (Windows): Path and PATH are one variable there.
const envCaseInsensitive = runtime.GOOS == "windows"

// processBaseEnv lists the executor variables a bash tool or process inherits.
//
// On Windows a program started with only PATH/HOME/USER cannot work: without
// SystemRoot Winsock and the crypto providers fail to load, cmd.exe needs
// ComSpec and PATHEXT to resolve commands, and most tools locate their
// temporary and profile directories through TEMP/TMP, USERPROFILE, APPDATA
// and LOCALAPPDATA. The list is the official MCP SDKs' default environment
// (getDefaultEnvironment) plus the profile, temp and locale variables a
// build or script needs. Secrets are not among them; DenyEnv still applies.
func processBaseEnv(goos string) []string {
	posix := []string{"PATH", "HOME", "USER", "LANG", "TMPDIR"}
	if goos != "windows" {
		return posix
	}
	return append(posix,
		"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT",
		"TEMP", "TMP",
		"USERNAME", "USERDOMAIN", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
		"APPDATA", "LOCALAPPDATA", "ProgramData", "ALLUSERSPROFILE", "PUBLIC",
		"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432",
		"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
		"PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS", "OS", "COMPUTERNAME",
		"PSModulePath",
	)
}

// setEnvValue sets values[key]. With fold, an existing entry whose name
// differs only in case is replaced, so a request's "Path" overrides the
// inherited "PATH" instead of producing two entries (a Windows environment
// block with two spellings of one name is ambiguous).
func setEnvValue(values map[string]string, key, value string, fold bool) {
	if fold {
		for existing := range values {
			if existing != key && strings.EqualFold(existing, key) {
				delete(values, existing)
			}
		}
	}
	values[key] = value
}
