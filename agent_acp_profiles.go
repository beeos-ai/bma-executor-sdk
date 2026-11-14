package bmaexecutor

// NewACPAgent: a BrainHost with the adapter of a profile. This is the
// one place that maps a brain's name to its adapter; the host never names one.

import (
	"context"
	"fmt"
)

// ACP profiles: which brain NewACPAgent hosts.
const (
	// ACPProfileDSH is bma-dsh-acp (ADR-0027), the default.
	ACPProfileDSH = "dsh"
	// ACPProfileClaudeCode is the official claude CLI, unmodified, driven
	// directly (brain_claude_cli.go); Command[0] is the pinned binary. It uses
	// only the node owner's own Claude credentials and never the platform's
	// model endpoint. A host whose Command is the owner's launcher of that
	// binary uses NewBrainHost with NewClaudeCLIAdapter(ClaudeCLIOptions{Executable}).
	ACPProfileClaudeCode = "claude-code"
	// ACPProfileCodex is the official codex CLI, unmodified, driven through
	// its app server (brain_codex_app_server.go); Command[0] is the pinned
	// binary and ACPAgentOptions.Env sets CODEX_HOME to the BMA Codex home
	// the owner signed in to. It uses only the owner's own credentials.
	ACPProfileCodex = "codex"
)

// NewACPAgent starts the brain of opts.Profile, checks it and returns its
// host. Serve it with client.ServeAgent(agent, agent.Capability()).
func NewACPAgent(ctx context.Context, opts ACPAgentOptions) (*ACPAgent, error) {
	var adapter BrainAdapter
	switch opts.Profile {
	case "", ACPProfileDSH:
		adapter = newDSHAdapter()
	case ACPProfileClaudeCode:
		adapter = NewClaudeCLIAdapter(ClaudeCLIOptions{})
	case ACPProfileCodex:
		adapter = NewCodexAppServerAdapter()
	default:
		return nil, fmt.Errorf("bmaexecutor: unknown ACPAgentOptions.Profile %q", opts.Profile)
	}
	return NewBrainHost(ctx, adapter, opts)
}
