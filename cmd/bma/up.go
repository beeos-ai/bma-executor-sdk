//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	bmaexecutor "github.com/beeos-ai/bma-executor-sdk"
)

// cmdUp connects this computer; running it again keeps what is set and only
// does what is missing (sign-in when the machine token no longer works,
// harness releases, the service).
func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	api := fs.String("api", "", "console origin to sign in through (default: the one used last, else "+defaultAPI+")")
	labels := fs.String("labels", "", "comma separated labels (a-z 0-9 . _ -), replacing the current ones")
	harness := fs.String("harness", "", "comma separated harness ids to enable without asking (see `bma harness list`)")
	remoteBrain := fs.Bool("remote-brain", false, "let a brain on another machine use this computer as its hands")
	reauth := fs.Bool("reauth", false, "sign in again even if this computer's machine token still works")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in link instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	ctx := context.Background()
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if *api != "" {
		if *api != c.APIURL {
			c.MachineID = "" // another deployment: this computer joins it anew
		}
		c.APIURL = strings.TrimRight(*api, "/")
	}
	if set["labels"] {
		c.Labels = splitList(*labels)
	}
	if err := chooseHarnesses(&c, splitList(*harness), set["harness"]); err != nil {
		return err
	}
	if set["remote-brain"] {
		c.RemoteBrain = *remoteBrain
	} else if interactive() && c.MachineID == "" {
		c.RemoteBrain = ask("Let brains on your other machines use this computer as their hands (remote brain)?", false)
	}
	host, _ := os.Hostname()
	fmt.Printf("✓ This computer: %s/%s, %s\n", runtime.GOOS, runtime.GOARCH, host)

	harnesses, err := enabled(c)
	if err != nil {
		return err
	}
	// The harness downloads run while the owner approves in the browser.
	downloaded := make(chan error, 1)
	go func() { downloaded <- installAll(ctx, harnesses) }()

	if *reauth || !tokenWorks(ctx, c) {
		if err := signIn(ctx, &c, host, *noBrowser, set["labels"]); err != nil {
			return err
		}
	} else {
		fmt.Printf("✓ Already connected as %s (%s)\n", c.DisplayName, c.MachineID)
	}
	if err := c.save(); err != nil {
		return err
	}
	if err := <-downloaded; err != nil {
		return err
	}
	for _, h := range harnesses {
		if err := prepareHarness(ctx, h); err != nil {
			return err
		}
	}
	if err := installService(); err != nil {
		if errors.Is(err, errNoServiceManager) {
			fmt.Println("!", err)
			return nil
		}
		return err
	}
	fmt.Println("✓ Service installed; connecting…")
	if err := waitConnected(45 * time.Second); err != nil {
		return err
	}
	fmt.Printf("✓ Connected. Open %s → Computers to start a session on %s.\n", c.APIURL, c.DisplayName)
	if runtime.GOOS == "darwin" && autoLoginUser() == "" {
		fmt.Println("  Note: the service runs while you are logged in. For an unattended Mac, turn on automatic login " +
			"(System Settings → Users & Groups).")
	}
	return nil
}

// chooseHarnesses enables the named harnesses, or asks for each one of the
// catalog not enabled yet on an interactive terminal.
func chooseHarnesses(c *config, named []string, given bool) error {
	if given {
		for _, id := range named {
			h, ok := bmaexecutor.HarnessByID(id)
			if !ok {
				return fmt.Errorf("unknown harness %q: see `bma harness list`", id)
			}
			if _, on := c.Harnesses[id]; !on {
				c.Harnesses[id] = h.Latest().Version
			}
		}
		return nil
	}
	for _, h := range bmaexecutor.Harnesses() {
		if _, on := c.Harnesses[h.ID]; on {
			continue
		}
		if !interactive() || ask(fmt.Sprintf("Use %s on this computer (your own %s account)?", h.DisplayName, h.DisplayName), true) {
			c.Harnesses[h.ID] = h.Latest().Version
		}
	}
	if len(c.Harnesses) == 0 {
		return errors.New("no harness enabled: run `bma up` again and enable one")
	}
	return nil
}

func installAll(ctx context.Context, harnesses []enabledHarness) error {
	for _, h := range harnesses {
		if _, err := installHarness(ctx, h.kind, h.release); err != nil {
			return err
		}
	}
	return nil
}

// tokenWorks exchanges the stored machine token once (the executor's own
// first step): a refusal means this computer must sign in again.
func tokenWorks(ctx context.Context, c config) bool {
	token, err := loadToken(c.MachineID)
	if err != nil || c.ExecutorURL == "" {
		return false
	}
	body, _ := json.Marshal(map[string]any{"executor": map[string]string{"name": "bma", "os": runtime.GOOS,
		"arch": runtime.GOARCH, "version": version}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.ExecutorURL, "/")+"/v1/machines/"+c.MachineID+"/token", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// signIn runs the device authorization and stores what it grants.
func signIn(ctx context.Context, c *config, host string, noBrowser, labelsSet bool) error {
	codes, err := startDevice(ctx, c.APIURL, host)
	if err != nil {
		return err
	}
	link := codes.VerificationURIComplete
	fmt.Printf("→ Approve this computer in your browser:\n    %s\n  Code: %s (check it matches the page)\n", link, codes.UserCode)
	if !noBrowser && os.Getenv("SSH_CONNECTION") == "" {
		openBrowser(link)
	}
	g, err := pollDevice(ctx, c.APIURL, codes)
	if err != nil {
		return err
	}
	where, err := saveToken(g.MachineID, g.AccessToken)
	if err != nil {
		return err
	}
	if c.MachineID != "" && c.MachineID != g.MachineID {
		_ = deleteToken(c.MachineID)
	}
	c.MachineID, c.WorkspaceID, c.DisplayName, c.ExecutorURL = g.MachineID, g.WorkspaceID, g.DisplayName, g.ExecutorURL
	if !labelsSet && len(g.Labels) > 0 {
		c.Labels = g.Labels
	}
	fmt.Printf("✓ Approved: %s (%s), workspace %s; machine token kept in %s\n", g.DisplayName, g.MachineID, g.WorkspaceID, where)
	return nil
}

// waitConnected waits for the service to report a ready connection.
func waitConnected(limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if s, err := loadState(); err == nil && time.Since(s.UpdatedAt) < limit {
			switch s.Connection {
			case bmaexecutor.ConnectionStateReady:
				return nil
			case "revoked":
				return errors.New("BMA rejected this computer's machine token: run `bma up --reauth`")
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the service has not connected after %s: see `bma status` and the log %s", limit, logHint())
}

func logHint() string {
	if runtime.GOOS == "darwin" {
		return logPath()
	}
	return "(journalctl --user -u bma)"
}

func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func interactive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var stdin = bufio.NewReader(os.Stdin)

// ask asks a yes/no question; an empty answer is def.
func ask(q string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Printf("? %s %s ", q, hint)
	line, _ := stdin.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, url).Start()
}
