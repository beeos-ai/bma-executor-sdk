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
	"strconv"
	"strings"
	"text/template"
)

// The user service that runs `bma run`: a LaunchAgent on macOS (it runs in
// the owner's login session, so it reads the login keychain and the harness's
// login, and has the desktop), a systemd user unit on Linux (lingering so it
// starts at boot). Both restart a failed run and leave a run that ended with
// status 0 (the platform revoked this computer) stopped.

const serviceLabel = "ai.beeos.bma"

var plist = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array><string>{{.Exe}}</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>{{.Log}}</string>
  <key>StandardErrorPath</key><string>{{.Log}}</string>
</dict>
</plist>
`))

var unit = template.Must(template.New("unit").Parse(`[Unit]
Description=BMA — this computer's executor (bma run)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{.Exe}} run
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`))

type serviceFiles struct {
	Label, Exe, Log string
}

func homeDir() string {
	dir, _ := os.UserHomeDir()
	return dir
}

func plistPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", serviceLabel+".plist")
}

func unitPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user", "bma.service")
	}
	return filepath.Join(homeDir(), ".config", "systemd", "user", "bma.service")
}

func logPath() string { return filepath.Join(homeDir(), "Library", "Logs", "bma", "bma.log") }

func render(t *template.Template, f serviceFiles) ([]byte, error) {
	var b bytes.Buffer
	err := t.Execute(&b, f)
	return b.Bytes(), err
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

var errNoServiceManager = errors.New("no user service manager (launchd or systemd --user): run `bma run` yourself")

// installService writes the service for the bma executable and (re)starts it.
func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	f := serviceFiles{Label: serviceLabel, Exe: exe, Log: logPath()}
	switch runtime.GOOS {
	case "darwin":
		b, err := render(plist, f)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(f.Log), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(plistPath(), b, 0o644); err != nil {
			return err
		}
		_ = run("launchctl", "bootout", guiDomain()+"/"+serviceLabel)
		return run("launchctl", "bootstrap", guiDomain(), plistPath())
	case "linux":
		if _, err := exec.LookPath("systemctl"); err != nil {
			return errNoServiceManager
		}
		b, err := render(unit, f)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(unitPath(), b, 0o644); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "enable", "bma"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "restart", "bma"); err != nil {
			return err
		}
		if user := os.Getenv("USER"); user != "" {
			if err := run("loginctl", "enable-linger", user); err != nil {
				fmt.Printf("  ! could not keep the service running without a login (%v);\n"+
					"    an administrator can run: loginctl enable-linger %s\n", err, user)
			}
		}
		return nil
	}
	return errNoServiceManager
}

// serviceInstalled reports whether the service file exists.
func serviceInstalled() bool {
	p := unitPath()
	if runtime.GOOS == "darwin" {
		p = plistPath()
	}
	_, err := os.Stat(p)
	return err == nil
}

// serviceRunning reports whether the service manager runs the service.
func serviceRunning() bool {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("launchctl", "print", guiDomain()+"/"+serviceLabel).Output()
		return err == nil && bytes.Contains(out, []byte("state = running"))
	case "linux":
		return exec.Command("systemctl", "--user", "is-active", "--quiet", "bma").Run() == nil
	}
	return false
}

func restartIfInstalled() error {
	if !serviceInstalled() {
		return nil
	}
	return installService()
}

// removeService stops the service and deletes its file.
func removeService() error {
	switch runtime.GOOS {
	case "darwin":
		_ = run("launchctl", "bootout", guiDomain()+"/"+serviceLabel)
		if err := os.Remove(plistPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	case "linux":
		_ = run("systemctl", "--user", "disable", "--now", "bma")
		if err := os.Remove(unitPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_ = run("systemctl", "--user", "daemon-reload")
	}
	return nil
}

// autoLoginUser is the macOS account that logs in at boot ("" = none): a
// LaunchAgent only runs once its user is logged in.
func autoLoginUser() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("defaults", "read", "/Library/Preferences/com.apple.loginwindow", "autoLoginUser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
