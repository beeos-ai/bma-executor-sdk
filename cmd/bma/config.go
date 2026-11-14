//go:build darwin || linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// defaultAPI is the console origin `bma up` signs in through.
const defaultAPI = "https://dashboard.beeos.ai"

// config is ~/.bma/config.json: this computer's choices and its machine. It
// holds no secret.
type config struct {
	APIURL      string   `json:"api_url"`
	ExecutorURL string   `json:"executor_url,omitempty"`
	MachineID   string   `json:"machine_id,omitempty"`
	WorkspaceID string   `json:"workspace_id,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Labels      []string `json:"labels"`
	// RemoteBrain lets a brain on another machine use this computer as its
	// hands; only the owner turns it on, here.
	RemoteBrain bool `json:"remote_brain"`
	// PlatformTools lets this computer's brain drive hands on another machine.
	PlatformTools bool     `json:"platform_tools"`
	WorkRoot      string   `json:"work_root"`
	AllowedRoots  []string `json:"allowed_roots"`
	// Harnesses are the enabled harnesses by id, each at a pinned version.
	Harnesses map[string]string `json:"harnesses"`
}

// home is the bma directory (BMA_HOME overrides ~/.bma, for tests).
func home() string {
	if h := os.Getenv("BMA_HOME"); h != "" {
		return h
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		panic(err) // no home directory: nothing of bma can work
	}
	return filepath.Join(dir, ".bma")
}

func configPath() string { return filepath.Join(home(), "config.json") }
func statePath() string  { return filepath.Join(home(), "state.json") }

func defaultConfig() config {
	dir, _ := os.UserHomeDir()
	return config{APIURL: defaultAPI, PlatformTools: true, WorkRoot: filepath.Join(dir, "bma-work"),
		Labels: []string{}, AllowedRoots: []string{}, Harnesses: map[string]string{}}
}

// loadConfig reads the configuration; a computer never connected has the defaults.
func loadConfig() (config, error) {
	c := defaultConfig()
	b, err := os.ReadFile(configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", configPath(), err)
	}
	if c.Harnesses == nil {
		c.Harnesses = map[string]string{}
	}
	return c, nil
}

func (c config) save() error { return writeJSON(configPath(), c) }

// writeJSON writes v atomically, readable by the owner only.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// state is ~/.bma/state.json, written by the service.
type state struct {
	Connection  string    `json:"connection"` // the executor connection state, or "revoked"
	ConnectedAt time.Time `json:"connected_at,omitzero"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	PID         int       `json:"pid"`
}

func loadState() (state, error) {
	var s state
	b, err := os.ReadFile(statePath())
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}
