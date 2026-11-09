package bmaexecutor

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Turn.start metadata keys of the platform's tools endpoint (brain_tools, contract 1.4.0).
const (
	metaToolsURL       = "tools_mcp_url"
	metaToolsToken     = "tools_mcp_token"
	metaToolsExpiresAt = "tools_mcp_token_expires_at"
)

// handsAreRemote: the turn carries the platform's tools endpoint, so its
// tools run on another machine (and none may run on this one).
func handsAreRemote(metadata map[string]string) bool {
	return metadata[metaToolsURL] != "" || metadata[metaToolsToken] != ""
}

// remoteToolRoute is the platform route of a turn with tools_mcp_*, or nil
// for a turn whose tools run here. One of url and token without the other is
// an error: the turn must not fall back to this machine.
func (a *ACPAgent) remoteToolRoute(turn *AgentTurn) (func(*acpLedger) toolRoute, error) {
	md := turn.Params().Metadata
	if !handsAreRemote(md) {
		return nil, nil
	}
	url, token := md[metaToolsURL], md[metaToolsToken]
	if url == "" || token == "" {
		return nil, errors.New("turn.start metadata tools_mcp_url and tools_mcp_token must come together; the turn's tools are not run on this machine")
	}
	// An unparsable or missing expiry is never refreshed (the platform did not say).
	expiresAt, _ := time.Parse(time.RFC3339, md[metaToolsExpiresAt])
	return func(ledger *acpLedger) toolRoute {
		return newPlatformToolRoute(PlatformToolRouteOptions{
			Endpoint: url, Token: token, ExpiresAt: expiresAt, TurnID: turn.TurnID(),
			Refresh: turn.RefreshToolsToken, OnLog: a.log,
			ApprovalFor: func(callID string) string {
				if call := ledger.call(callID); call.Decision == acpDecisionAllow {
					return call.InteractionID
				}
				return ""
			},
		})
	}, nil
}

// handsDescription tells the model where its tools run.
func handsDescription(h *EnvironmentInfo) string {
	var facts []string
	for _, f := range [][2]string{{"os", h.OS}, {"arch", h.Arch}, {"shell", h.Shell}} {
		if f[1] != "" {
			facts = append(facts, f[0]+" "+f[1])
		}
	}
	text := "Your tools run on another computer, not on the one this agent process runs on"
	if len(facts) > 0 {
		text += " (" + strings.Join(facts, ", ") + ")"
	}
	text += "."
	if h.SessionFilesRoot != "" {
		text += fmt.Sprintf(" The session's files are mounted at %s there.", h.SessionFilesRoot)
	}
	return text
}
