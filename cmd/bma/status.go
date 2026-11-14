package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

type statusReport struct {
	Version       string            `json:"version"`
	API           string            `json:"api_url"`
	MachineID     string            `json:"machine_id"`
	DisplayName   string            `json:"display_name"`
	WorkspaceID   string            `json:"workspace_id"`
	TokenStore    string            `json:"token_store"`
	Service       string            `json:"service"` // running | installed | not installed
	Connection    string            `json:"connection"`
	ConnectedAt   *time.Time        `json:"connected_at,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	Labels        []string          `json:"labels"`
	RemoteBrain   bool              `json:"remote_brain"`
	PlatformTools bool              `json:"platform_tools"`
	Harnesses     map[string]string `json:"harnesses"` // id -> version
	SignedIn      map[string]bool   `json:"signed_in"` // id -> the harness's own login check
	AutoLogin     *bool             `json:"auto_login,omitempty"`
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	r := statusReport{Version: version, API: c.APIURL, MachineID: c.MachineID, DisplayName: c.DisplayName,
		WorkspaceID: c.WorkspaceID, TokenStore: tokenStore(c.MachineID), Labels: c.Labels, RemoteBrain: c.RemoteBrain,
		PlatformTools: c.PlatformTools, Harnesses: c.Harnesses, SignedIn: map[string]bool{}, Service: "not installed",
		Connection: "unknown"}
	switch {
	case serviceRunning():
		r.Service = "running"
	case serviceInstalled():
		r.Service = "installed"
	}
	if s, err := loadState(); err == nil {
		r.Connection, r.LastError = s.Connection, s.Error
		if !s.ConnectedAt.IsZero() {
			r.ConnectedAt = &s.ConnectedAt
		}
	}
	harnesses, err := enabled(c)
	if err != nil {
		return err
	}
	for _, h := range harnesses {
		exe := harnessPath(h.kind, h.release)
		r.SignedIn[h.kind.ID] = fileExists(exe) && signedIn(context.Background(), h.kind, exe)
	}
	if runtime.GOOS == "darwin" {
		on := autoLoginUser() != ""
		r.AutoLogin = &on
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Printf("bma %s\n", r.Version)
	if r.MachineID == "" {
		fmt.Println("Not connected: run `bma up`.")
		return nil
	}
	fmt.Printf("Computer:    %s (%s), workspace %s, %s\n", r.DisplayName, r.MachineID, r.WorkspaceID, r.API)
	fmt.Printf("Service:     %s\n", r.Service)
	conn := r.Connection
	if r.ConnectedAt != nil {
		conn += " since " + r.ConnectedAt.Local().Format(time.RFC3339)
	}
	if r.Connection == "revoked" {
		conn += " — BMA removed this computer: run `bma up --reauth`"
	}
	fmt.Printf("Connection:  %s\n", conn)
	if r.LastError != "" {
		fmt.Printf("Last error:  %s\n", r.LastError)
	}
	for _, h := range harnesses {
		login := "signed in"
		if !r.SignedIn[h.kind.ID] {
			login = "NOT signed in — run `bma harness login " + h.kind.ID + "`"
		}
		fmt.Printf("Harness:     %s %s, %s\n", h.kind.DisplayName, h.release.Version, login)
	}
	fmt.Printf("Labels:      %s\n", strings.Join(r.Labels, ","))
	fmt.Printf("Remote brain %v, platform tools %v; machine token in %s\n", r.RemoteBrain, r.PlatformTools, r.TokenStore)
	if r.AutoLogin != nil && !*r.AutoLogin {
		fmt.Println("Note: automatic login is off; the service runs only while you are logged in.")
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// cmdDisconnect stops the service and removes this computer from BMA with
// its own machine token (a machine token can delete only its own machine),
// then forgets the token. The harness releases stay for a later `bma up`.
func cmdDisconnect(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: bma disconnect")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	token, err := loadToken(c.MachineID)
	if err != nil {
		return err
	}
	if err := removeService(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
		strings.TrimRight(c.ExecutorURL, "/")+"/v1/machines/"+c.MachineID, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("removing %s from BMA: %w (the service is stopped; run `bma disconnect` again)", c.MachineID, err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusUnauthorized, http.StatusNotFound:
		// 401/404: the platform no longer knows this machine or its token.
	default:
		return fmt.Errorf("removing %s from BMA: %s (the service is stopped; delete it in the console)", c.MachineID,
			resp.Status)
	}
	if err := deleteToken(c.MachineID); err != nil {
		return err
	}
	fmt.Printf("✓ %s (%s) disconnected and removed from BMA\n", c.DisplayName, c.MachineID)
	c.MachineID, c.WorkspaceID, c.DisplayName, c.ExecutorURL = "", "", "", ""
	_ = os.Remove(statePath())
	return c.save()
}
