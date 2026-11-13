package bmaexecutor

// NewACPAgent: a BrainHost with the adapter of an ACP profile. This is the
// one place that maps a brain's name to its adapter; the host never names one.

import (
	"context"
	"fmt"
)

// ACP profiles: which brain NewACPAgent hosts.
const (
	// ACPProfileDSH is bma-dsh-acp (ADR-0027), the default.
	ACPProfileDSH = "dsh"
	// ACPProfileClaudeCode is the official claude-agent-acp, unmodified (see
	// agent_acp_claude.go). It uses only the node owner's own Claude
	// credentials and never the platform's model endpoint.
	ACPProfileClaudeCode = "claude-code"
	// ACPProfileClaudeCodeCLI is the official claude CLI driven directly
	// (brain_claude_cli.go), harness id claude-code; Command[0] is the pinned
	// binary. A host whose Command is the owner's launcher of that binary uses
	// NewBrainHost with NewClaudeCLIAdapter(ClaudeCLIOptions{Executable}).
	ACPProfileClaudeCodeCLI = "claude-code-cli"
)

// NewACPAgent starts the brain of opts.Profile, checks it and returns its
// host. Serve it with client.ServeAgent(agent, agent.Capability()).
func NewACPAgent(ctx context.Context, opts ACPAgentOptions) (*ACPAgent, error) {
	var adapter BrainAdapter
	switch opts.Profile {
	case "", ACPProfileDSH:
		adapter = newDSHAdapter()
	case ACPProfileClaudeCode:
		adapter = newClaudeACPAdapter()
	case ACPProfileClaudeCodeCLI:
		adapter = NewClaudeCLIAdapter(ClaudeCLIOptions{})
	default:
		return nil, fmt.Errorf("bmaexecutor: unknown ACPAgentOptions.Profile %q", opts.Profile)
	}
	return NewBrainHost(ctx, adapter, opts)
}
