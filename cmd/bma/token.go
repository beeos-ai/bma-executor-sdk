package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The machine token: the login keychain on macOS (the security command,
// value passed on stdin, never in argv or the environment), else a file
// readable by the owner only. A keychain that refuses (a locked session over
// SSH) falls back to the file, as Claude Code does with its own login.

const keychainService = "ai.beeos.bma"

// tokenFile is where the token is kept without a keychain.
func tokenFile() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home(), "machine-token")
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "bma", "machine-token")
	}
	dir, _ := os.UserHomeDir()
	return filepath.Join(dir, ".config", "bma", "machine-token")
}

func useKeychain() bool { return runtime.GOOS == "darwin" && os.Getenv("BMA_HOME") == "" }

// saveToken stores the token of machine and answers where.
func saveToken(machine, token string) (string, error) {
	if useKeychain() {
		cmd := exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %q\n",
			keychainService, machine, token))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil && stderr.Len() == 0 {
			_ = os.Remove(tokenFile())
			return "keychain", nil
		}
	}
	if err := writeFile(tokenFile(), []byte(token)); err != nil {
		return "", err
	}
	return tokenFile(), nil
}

var errNoToken = errors.New("this computer is not connected: run `bma up`")

// loadToken reads the token of machine.
func loadToken(machine string) (string, error) {
	if machine == "" {
		return "", errNoToken
	}
	if b, err := os.ReadFile(tokenFile()); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if useKeychain() {
		out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", keychainService, "-a", machine,
			"-w").Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", errNoToken
}

// tokenStore says where the token of machine is.
func tokenStore(machine string) string {
	if _, err := os.Stat(tokenFile()); err == nil {
		return tokenFile()
	}
	if useKeychain() && exec.Command("/usr/bin/security", "find-generic-password", "-s", keychainService, "-a",
		machine).Run() == nil {
		return "keychain"
	}
	return "none"
}

// deleteToken removes the token of machine wherever it is.
func deleteToken(machine string) error {
	if useKeychain() {
		_ = exec.Command("/usr/bin/security", "delete-generic-password", "-s", keychainService, "-a", machine).Run()
	}
	if err := os.Remove(tokenFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
