//go:build darwin || linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/spf13/cobra"
)

// `bma agents apply -f agent.yaml`: make the agent named in the file match
// it (schema: cmd/bma/schema/agent.schema.json, the body of an agent create).

// Members an update replaces as a whole; a file that omits one declares it
// empty, as a create stores it.
var (
	agentClearedText  = []string{"description", "system"}
	agentClearedLists = []string{"tools", "mcp_servers", "skills"}
)

// applyAgent creates the agent named spec.name, or updates it to spec; an
// update stores a new version only when the definition changed. It answers
// the agent and "created", "updated" or "unchanged".
func applyAgent(ctx context.Context, s *session, spec map[string]any) (map[string]any, string, error) {
	name, _ := spec["name"].(string)
	if name == "" {
		return nil, "", usageError("agent: name is required")
	}
	if _, ok := spec["model"]; !ok {
		return nil, "", usageError("agent: model is required")
	}
	if _, ok := spec["version"]; ok {
		return nil, "", usageError("agent: version is not part of a definition")
	}
	items, err := listAll(ctx, s, "v1/agents", nil)
	if err != nil {
		return nil, "", err
	}
	var matches []map[string]any
	for _, it := range items {
		if it["name"] == name {
			matches = append(matches, it)
		}
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return nil, "", err
	}
	switch len(matches) {
	case 0:
		sum := sha256.Sum256(payload)
		v, err := s.json(ctx, "POST", "v1/agents", nil, payload,
			option.WithHeader("Idempotency-Key", "bma-apply-"+hex.EncodeToString(sum[:])[:40]))
		if err != nil {
			return nil, "", err
		}
		m, _ := v.(map[string]any)
		return m, "created", nil
	case 1:
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, fmt.Sprint(m["id"]))
		}
		return nil, "", usageError(fmt.Sprintf("%d agents are named %q (%s): rename or archive the others",
			len(matches), name, strings.Join(ids, ", ")))
	}
	cur := matches[0]
	body := map[string]any{}
	for k, v := range spec {
		body[k] = v
	}
	for _, k := range agentClearedText {
		if _, ok := body[k]; !ok {
			body[k] = ""
		}
	}
	for _, k := range agentClearedLists {
		if _, ok := body[k]; !ok {
			body[k] = []any{}
		}
	}
	// metadata is merged by key: delete the keys the file no longer has.
	md := map[string]any{}
	if want, ok := spec["metadata"].(map[string]any); ok {
		for k, v := range want {
			md[k] = v
		}
	}
	if have, ok := cur["metadata"].(map[string]any); ok {
		for k := range have {
			if _, keep := md[k]; !keep {
				md[k] = nil
			}
		}
	}
	body["metadata"] = md
	body["version"] = cur["version"] // fails with 409 when the agent changed meanwhile
	if payload, err = json.Marshal(body); err != nil {
		return nil, "", err
	}
	id, _ := cur["id"].(string)
	v, err := s.json(ctx, "POST", "v1/agents/"+url.PathEscape(id), nil, payload)
	if err != nil {
		return nil, "", err
	}
	m, _ := v.(map[string]any)
	action := "updated"
	if fmt.Sprint(m["version"]) == fmt.Sprint(cur["version"]) {
		action = "unchanged"
	}
	return m, action, nil
}

func applyCmd() *cobra.Command {
	var filename string
	cmd := &cobra.Command{
		Use:   "apply -f <agent.yaml>",
		Short: "Create the agent of a YAML file, or update it to a new version only when the file changed",
		Long: `Make the agent named in the file match it (the file is the body of an
agent create; schema:
https://github.com/beeos-ai/bma-executor-sdk/blob/main/cmd/bma/schema/agent.schema.json).

No agent of that name: create it. One agent of that name: update it to the
file; members the file omits are cleared (description, system, tools,
mcp_servers, skills; metadata keys it does not list are deleted). An
update that changes nothing stores no version, so applying the same file
again is a no-op. Several agents of that name: an error listing their IDs.

Prints the agent; stderr says created, updated (new version) or unchanged.

POST /v1/agents
POST /v1/agents/{agent_id}`,
		Args: args(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if filename == "" {
				return usageError("-f <agent.yaml> is required")
			}
			var spec map[string]any
			if _, err := readSpecFile(filename, &spec); err != nil {
				return err
			}
			s, err := apiSession()
			if err != nil {
				return err
			}
			ag, action, err := applyAgent(cmd.Context(), s, spec)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "✓ agent %s %s (%s, version %v)\n", ag["name"], action, ag["id"], ag["version"])
			return printValue(ag, false)
		},
	}
	cmd.Flags().StringVarP(&filename, "filename", "f", "", "the agent file (YAML or JSON), - for stdin (required)")
	return cmd
}
