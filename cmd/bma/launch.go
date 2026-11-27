//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// `bma sessions launch -f session.yaml`: start a session from a file in one
// command (schema: cmd/bma/schema/session.schema.json).

// launchKeyLabel is the session metadata key that holds the launch key, so
// that a rerun finds the session it launched before.
const launchKeyLabel = "bma_cli_launch_key"

// launchSpec is a session file.
type launchSpec struct {
	// Agent is an agent's name or aprf_ ID, {id|name, version}, or an inline
	// agent definition (with `model`), applied like `bma agents apply`.
	Agent any `yaml:"agent" json:"agent"`
	// Environment is an environment's name or env_ / mch_ ID; MachinePool a
	// machine pool's name or mpo_ / env_ ID (the session runs on a machine
	// of that pool). Exactly one of them.
	Environment string            `yaml:"environment" json:"environment,omitempty"`
	MachinePool string            `yaml:"machine_pool" json:"machine_pool,omitempty"`
	Title       string            `yaml:"title" json:"title,omitempty"`
	Metadata    map[string]string `yaml:"metadata" json:"metadata,omitempty"`
	// Resources are mounted in the session: {path, mount_path} uploads a
	// local file; any other entry is passed to the API as it is (a `file`
	// resource with file_id, a `memory_store` resource).
	Resources []map[string]any `yaml:"resources" json:"resources,omitempty"`
	VaultIDs  []string         `yaml:"vault_ids" json:"vault_ids,omitempty"`
	// Message is the first user message; Files its attachments.
	Message string   `yaml:"message" json:"message,omitempty"`
	Files   []string `yaml:"files" json:"files,omitempty"`
}

// readSpecFile reads a YAML (or JSON) file, "-" for stdin, into out with
// unknown members refused; it answers the directory relative paths start
// from.
func readSpecFile(name string, out any) (string, error) {
	var b []byte
	var err error
	dir := "."
	if name == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(name)
		dir = filepath.Dir(name)
	}
	if err != nil {
		return "", err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return "", usageError(fmt.Sprintf("%s: %v", name, err))
	}
	return dir, nil
}

// localPath resolves a path of the file relative to its directory.
func localPath(dir, p string) string {
	if filepath.IsAbs(p) || dir == "." {
		return p
	}
	return filepath.Join(dir, p)
}

// launchKey is the Idempotency-Key of a launch: a digest of the spec and of
// every local file it uploads, so the same file launches the same session.
func launchKey(spec launchSpec, dir string) (string, error) {
	h := sha256.New()
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	h.Write(b)
	var paths []string
	for _, r := range spec.Resources {
		if p, ok := r["path"].(string); ok {
			paths = append(paths, p)
		}
	}
	paths = append(paths, spec.Files...)
	for _, p := range paths {
		f, err := os.Open(localPath(dir, p))
		if err != nil {
			return "", err
		}
		fh := sha256.New()
		_, err = io.Copy(fh, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "\x00%s\x00%x", p, fh.Sum(nil))
	}
	return "bma-launch-" + hex.EncodeToString(h.Sum(nil))[:40], nil
}

func launchCmd() *cobra.Command {
	var filename, message, key string
	var files []string
	var wait, stream, interactive bool
	cmd := &cobra.Command{
		Use:   "launch -f <session.yaml>",
		Short: "Start a session from a YAML file: agent, environment or machine pool, files, first message",
		Long: `Start a session from a YAML file in one step (schema:
https://github.com/beeos-ai/bma-executor-sdk/blob/main/cmd/bma/schema/session.schema.json).

The agent, the environment, the machine pool and memory stores may be named
by name or ID; a name must match exactly one item of the list (an ambiguous
or unknown name is an error that lists the candidates). An inline agent (a
definition with "model") is created, or updated to a new version only when
its definition changed, as "bma agents apply" does. "machine_pool" runs the
session on a machine of that pool. Local "resources" paths and "files"
(attachments of the first message) are uploaded and mounted in the session
as <name> in its uploads directory, and the message lists the mount paths the
session shows; relative paths start at the file's directory.

Launching is idempotent: the launch key, a digest of the file and of every
local file it uploads (or --idempotency-key), is stored in the session's
metadata as bma_cli_launch_key. Running the same launch again finds that
session instead of creating another one, and does not send the message
again. Change the file or pass another --idempotency-key to launch a new
session.

Without --wait, --stream or --interactive, print the session. --wait follows
the first turn and prints the agent's reply and the deliverables; --stream
prints the turn's events as JSON lines; the exit status is then the turn's:
0 end_turn, 7 waiting for input (requires_action), 8 failed. --interactive
continues as "bma sessions chat".

POST /v1/sessions`,
		Args: args(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if filename == "" {
				return usageError("-f <session.yaml> is required")
			}
			if n := countTrue(wait, stream, interactive); n > 1 {
				return usageError("--wait, --stream and --interactive exclude each other")
			}
			var spec launchSpec
			dir, err := readSpecFile(filename, &spec)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("message") {
				spec.Message = message
			}
			spec.Files = append(spec.Files, files...)
			if spec.Message == "" && len(spec.Files) > 0 {
				return usageError("files are attachments of the first message: set message (or --message)")
			}
			if spec.Message == "" && (wait || stream) {
				return usageError("--wait and --stream follow the first turn: set message (or --message)")
			}
			if key == "" {
				if key, err = launchKey(spec, dir); err != nil {
					return err
				}
			}
			s, err := apiSession()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			l := &launcher{s: s, spec: spec, dir: dir, key: key}
			if err := l.run(ctx); err != nil {
				return err
			}
			switch {
			case interactive:
				return chat(ctx, s, l.sid, l.firstTurn(ctx))
			case wait || stream:
				var onEvent func(map[string]any) error
				if stream {
					onEvent = func(v map[string]any) error { return printEvent(v) }
				}
				var res turnResult
				if first := l.firstTurn(ctx); first != nil || l.session["status"] == "running" {
					res, err = followTurn(ctx, s, l.sid, first, onEvent, false)
				} else {
					res, err = lastTurn(ctx, s, l.sid)
				}
				if err != nil {
					return err
				}
				if stream {
					return turnStatus(res)
				}
				return printTurn(res)
			}
			if l.message != nil {
				if err := sendEvents(ctx, s, l.sid, l.key+"-message", l.message); err != nil {
					return err
				}
			}
			return printValue(l.session, false)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&filename, "filename", "f", "", "the session file (YAML or JSON), - for stdin (required)")
	f.StringVar(&message, "message", "", "the first user message (replaces the file's message)")
	f.StringArrayVar(&files, "file", nil, "attach a local file to the first message (repeatable)")
	f.BoolVar(&wait, "wait", false, "follow the first turn and print the agent's reply and the deliverables")
	f.BoolVar(&stream, "stream", false, "print the first turn's events as JSON lines")
	f.BoolVar(&interactive, "interactive", false, "continue in `bma sessions chat` (answer questions and tool approvals)")
	f.StringVar(&key, "idempotency-key", "", "launch key (default: a digest of the file and of the files it uploads)")
	return cmd
}

func countTrue(v ...bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}

// launcher finds or creates the session of a launch.
type launcher struct {
	s       *session
	spec    launchSpec
	dir     string
	key     string
	sid     string
	session map[string]any
	// message is the first message still to send (nil: none, or sent).
	message map[string]any
}

// firstTurn sends the first message when it is still to be sent.
func (l *launcher) firstTurn(ctx context.Context) func() error {
	if l.message == nil {
		return nil
	}
	msg := l.message
	l.message = nil
	return func() error { return sendEvents(ctx, l.s, l.sid, l.key+"-message", msg) }
}

func (l *launcher) run(ctx context.Context) error {
	found, err := l.existing(ctx)
	if err != nil {
		return err
	}
	att, err := attachments(l.spec.Files)
	if err != nil {
		return err
	}
	if found != nil {
		l.session, l.sid = found, found["id"].(string)
		text := withAttachments(l.spec.Message, shownMounts(found, att))
		fmt.Fprintf(os.Stderr, "✓ session %s was launched from this file before (%s %s); not creating another\n",
			l.sid, launchKeyLabel, l.key)
		if text != "" {
			// A launch that stopped between creating the session and
			// sending its message sends it now (same Idempotency-Key).
			if m, err := latestEvent(ctx, l.s, l.sid, "user.message"); err != nil {
				return err
			} else if m == nil {
				l.message = userMessage(text)
			}
		}
		return nil
	}
	body, err := l.body(ctx, att)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	v, err := l.s.json(ctx, "POST", "v1/sessions", nil, payload, option.WithHeader("Idempotency-Key", l.key))
	if err != nil {
		return err
	}
	l.session, _ = v.(map[string]any)
	l.sid, _ = l.session["id"].(string)
	fmt.Fprintf(os.Stderr, "✓ session %s created\n", l.sid)
	if text := withAttachments(l.spec.Message, shownMounts(l.session, att)); text != "" {
		l.message = userMessage(text)
	}
	return nil
}

// existing is the session an earlier run of this launch created, nil when
// there is none.
func (l *launcher) existing(ctx context.Context) (map[string]any, error) {
	q := url.Values{"metadata": {launchKeyLabel + ":" + l.key}, "include_archived": {"true"}, "limit": {"1"}}
	v, err := l.s.json(ctx, "GET", "v1/sessions", q, nil)
	if err != nil {
		return nil, err
	}
	page, _ := v.(map[string]any)
	data, _ := page["data"].([]any)
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			if md, _ := m["metadata"].(map[string]any); md[launchKeyLabel] == l.key {
				return m, nil
			}
		}
	}
	return nil, nil
}

// body resolves the spec into the session create request, uploading the
// local files.
func (l *launcher) body(ctx context.Context, att []attachment) (map[string]any, error) {
	spec := l.spec
	agent, err := l.agent(ctx)
	if err != nil {
		return nil, err
	}
	env, err := l.environment(ctx)
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{}
	for k, v := range spec.Metadata {
		metadata[k] = v
	}
	metadata[launchKeyLabel] = l.key
	resources := []any{}
	for i, r := range spec.Resources {
		p, local := r["path"].(string)
		if !local {
			out := map[string]any{}
			for k, v := range r {
				out[k] = v
			}
			if out["type"] == "memory_store" {
				ref, _ := out["memory_store_id"].(string)
				if out["memory_store_id"], err = resolve(ctx, l.s, memoryStoreKind, ref); err != nil {
					return nil, err
				}
			}
			resources = append(resources, out)
			continue
		}
		for k := range r {
			if k != "path" && k != "mount_path" {
				return nil, usageError(fmt.Sprintf("resources[%d]: a local file takes path and mount_path, not %s", i, k))
			}
		}
		id, err := upload(ctx, l.s, localPath(l.dir, p), fmt.Sprintf("%s-r%d", l.key, i))
		if err != nil {
			return nil, err
		}
		res := map[string]any{"type": "file", "file_id": id}
		if mp, ok := r["mount_path"].(string); ok {
			res["mount_path"] = mp
		}
		resources = append(resources, res)
	}
	for i, a := range att {
		id, err := upload(ctx, l.s, localPath(l.dir, a.path), fmt.Sprintf("%s-f%d", l.key, i))
		if err != nil {
			return nil, err
		}
		resources = append(resources, map[string]any{"type": "file", "file_id": id, "mount_path": a.mount})
	}
	body := map[string]any{"agent": agent, "environment_id": env, "metadata": metadata}
	if len(resources) > 0 {
		body["resources"] = resources
	}
	if spec.Title != "" {
		body["title"] = spec.Title
	}
	if len(spec.VaultIDs) > 0 {
		body["vault_ids"] = spec.VaultIDs
	}
	return body, nil
}

// agent is the session's agent member: an ID, or {type: agent, id, version}.
func (l *launcher) agent(ctx context.Context) (any, error) {
	switch a := l.spec.Agent.(type) {
	case nil:
		return nil, usageError("agent is required: an agent's name or ID, or an inline definition")
	case string:
		return resolve(ctx, l.s, agentKind, a)
	case map[string]any:
		if _, inline := a["model"]; inline {
			ag, action, err := applyAgent(ctx, l.s, a)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "✓ agent %s %s (%s, version %v)\n", ag["name"], action, ag["id"], ag["version"])
			return map[string]any{"type": "agent", "id": ag["id"], "version": ag["version"]}, nil
		}
		for k := range a {
			if k != "id" && k != "name" && k != "version" {
				return nil, usageError("agent: a reference takes id or name and version; an inline agent needs model (unknown member " + k + ")")
			}
		}
		ref, _ := a["id"].(string)
		if ref == "" {
			ref, _ = a["name"].(string)
		}
		id, err := resolve(ctx, l.s, agentKind, ref)
		if err != nil {
			return nil, err
		}
		if v, ok := a["version"]; ok {
			return map[string]any{"type": "agent", "id": id, "version": v}, nil
		}
		return id, nil
	}
	return nil, usageError("agent must be a name, an ID or an object")
}

// environment is the session's environment_id.
func (l *launcher) environment(ctx context.Context) (string, error) {
	spec := l.spec
	switch {
	case spec.Environment != "" && spec.MachinePool != "":
		return "", usageError("set environment or machine_pool, not both")
	case spec.Environment != "":
		return resolve(ctx, l.s, environmentKind, spec.Environment)
	case spec.MachinePool == "":
		return "", usageError("environment or machine_pool is required")
	}
	pool, err := resolve(ctx, l.s, machinePoolKind, spec.MachinePool)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(pool, "env_") {
		return pool, nil // a cloud environment's own pool: its sessions get machines of it
	}
	payload, err := json.Marshal(map[string]any{"machine_pool_id": pool})
	if err != nil {
		return "", err
	}
	v, err := l.s.json(ctx, "POST", "v1/machines", nil, payload, option.WithHeader("Idempotency-Key", l.key+"-machine"))
	if err != nil {
		return "", fmt.Errorf("starting a machine of pool %s: %w", pool, err)
	}
	m, _ := v.(map[string]any)
	id, _ := m["id"].(string)
	fmt.Fprintf(os.Stderr, "✓ machine %s of pool %s\n", id, pool)
	return id, nil
}

// kind is a resource the session file may name by name or ID.
type kind struct {
	what, path string
	prefixes   []string
	query      url.Values
}

var (
	agentKind       = kind{"agent", "v1/agents", []string{"aprf_", "agent_"}, nil}
	environmentKind = kind{"environment", "v1/environments", []string{"env_", "mch_"}, nil}
	machinePoolKind = kind{"machine pool", "v1/machine_pools", []string{"mpo_", "env_"}, nil}
	memoryStoreKind = kind{"memory store", "v1/memory_stores", []string{"memstore_"}, nil}
)

// resolve turns a name or an ID into an ID: an ID is used as it is; a name
// must match the name of exactly one listed item.
func resolve(ctx context.Context, s *session, k kind, ref string) (string, error) {
	if ref == "" {
		return "", usageError(k.what + ": a name or an ID is required")
	}
	for _, p := range k.prefixes {
		if strings.HasPrefix(ref, p) {
			return ref, nil
		}
	}
	items, err := listAll(ctx, s, k.path, k.query)
	if err != nil {
		return "", fmt.Errorf("looking up %s %q: %w", k.what, ref, err)
	}
	var ids, names []string
	for _, it := range items {
		name, _ := it["name"].(string)
		if name == ref {
			id, _ := it["id"].(string)
			ids = append(ids, id)
		}
		names = append(names, name)
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		sort.Strings(names)
		return "", notFoundError(fmt.Sprintf("no %s is named %q (names: %s)", k.what, ref, strings.Join(names, ", ")))
	}
	return "", usageError(fmt.Sprintf("%d %ss are named %q (%s): use the ID", len(ids), k.what, ref, strings.Join(ids, ", ")))
}

// listAll reads every page of a list.
func listAll(ctx context.Context, s *session, path string, query url.Values) ([]map[string]any, error) {
	q := url.Values{"limit": {"100"}}
	for k, v := range query {
		q[k] = v
	}
	var out []map[string]any
	for {
		v, err := s.json(ctx, "GET", path, q, nil)
		if err != nil {
			return nil, err
		}
		page, _ := v.(map[string]any)
		data, _ := page["data"].([]any)
		for _, d := range data {
			if m, ok := d.(map[string]any); ok {
				out = append(out, m)
			}
		}
		next, _ := page["next_page"].(string)
		if next == "" {
			return out, nil
		}
		q.Set("page", next)
	}
}
