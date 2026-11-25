package bmaexecutor

// The platform's web tools for a brain that runs its tools on this machine
// (contract 2.4.0, AgentCapability.platform_credentials platform_tools):
// turn.start config.platform_tools offers web_search and web_fetch, and
// metadata platform_tools_mcp_* the platform tools endpoint and a token that
// runs only those. The brain offers them like its other tools, under the
// turn's policy and approvals; a call goes to the endpoint (the platform
// searches and fetches with its own providers and SSRF guard, records the
// usage), never to this machine, and is reported as agent.tool_use /
// agent.tool_result. The brains' own web search and fetch stay off (the
// Claude CLI starts without built-in tools, Codex with web_search disabled),
// so the platform's are the only ones.

import (
	"slices"
	"time"
)

// Turn.start metadata keys of the platform tools credential.
const (
	metaPlatformToolsURL       = "platform_tools_mcp_url"
	metaPlatformToolsToken     = "platform_tools_mcp_token"
	metaPlatformToolsExpiresAt = "platform_tools_mcp_token_expires_at"
)

// onMachinePlatformTools are the platform tools a brain that runs its tools
// here is offered (contract 2.4.0).
var onMachinePlatformTools = []string{"web_search", "web_fetch"}

// withPlatformTools adds the turn's platform tools to tools. They come only
// to a brain whose tools run here; each must be one of
// onMachinePlatformTools with a description, an object input schema, a
// permission allow or ask and a name no other tool has.
func (a *BrainHost) withPlatformTools(tools []BrainTool, platform []PlatformTool, remote bool) ([]BrainTool, error) {
	for _, p := range platform {
		if remote || !slices.Contains(onMachinePlatformTools, p.Name) || p.Description == "" ||
			p.InputSchema["type"] != "object" || (p.Permission != "allow" && p.Permission != "ask") || brainToolNames(tools)[p.Name] {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid params: platform_tools entry " + p.Name +
				" is not web_search or web_fetch with a description, an object input_schema and permission allow or ask, once, for a brain whose tools run here"}
		}
		kind := "search"
		if p.Name == "web_fetch" {
			kind = "fetch"
		}
		tools = append(tools, BrainTool{Name: p.Name, Description: p.Description, Parameters: p.InputSchema, ReadOnly: true,
			Kind: kind, Permission: p.Permission, Platform: true})
	}
	return tools, nil
}

// platformToolsRoute is the route of a turn's platform tool calls: the
// platform tools endpoint with the turn's token, refreshed through
// tools_token.refresh, the user's approval of a call presented with it. A
// turn without the credential (or half of it) gets a route that refuses
// every call: a platform tool never runs on this machine.
func platformToolsRoute(a *BrainHost, turn *AgentTurn, ledger *acpLedger) ToolRoute {
	md := turn.Params().Metadata
	url, token := md[metaPlatformToolsURL], md[metaPlatformToolsToken]
	if url == "" || token == "" {
		return refusingRoute{detail: "turn.start metadata platform_tools_mcp_url and platform_tools_mcp_token are missing; the platform tools do not run on this machine"}
	}
	expiresAt, _ := time.Parse(time.RFC3339, md[metaPlatformToolsExpiresAt]) // unparsable: never refreshed
	route, err := NewPlatformToolRoute(PlatformToolRouteOptions{
		Endpoint: url, Token: token, ExpiresAt: expiresAt, SessionID: turn.SessionID(), TurnID: turn.TurnID(),
		Refresh: turn.RefreshToolsToken, OnLog: a.log,
		ApprovalFor: func(callID string) string {
			if call := ledger.call(callID); call.Decision == acpDecisionAllow {
				return call.InteractionID
			}
			return ""
		},
	})
	if err != nil {
		return refusingRoute{detail: "turn.start metadata platform_tools_mcp_url is not usable: " + err.Error()}
	}
	return route
}
