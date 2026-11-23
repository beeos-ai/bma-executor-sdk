package bmaexecutor

// Reading a session's past from the platform (contract session.history,
// ADR-0078 of BMA): an agent that opens a session it holds no local state for
// rebuilds the conversation from the platform's log instead of starting
// without it, and declares AgentCapability.history_rebuild. The platform
// forks only sessions of such agents (ADR-0106).

import (
	"context"
	"errors"
)

// sessionHistoryPage is the page asked for (the contract's maximum).
const sessionHistoryPage = 500

type agentClientKey struct{}

// ReadSessionHistory reads every event of the conversation of sessionID
// before the opening input of turnID, in log order: messages, tool calls and
// their results. Call it from AgentHandler.OpenTurn (the ctx it is given).
// A platform that does not serve the method (-32601) has no history to give:
// nil.
func ReadSessionHistory(ctx context.Context, sessionID, turnID string) ([]SessionHistoryEvent, error) {
	client, _ := ctx.Value(agentClientKey{}).(*Client)
	if client == nil {
		return nil, errors.New("bmaexecutor: ReadSessionHistory outside AgentHandler.OpenTurn")
	}
	var all []SessionHistoryEvent
	var after int64
	for {
		page, err := Calls.SessionHistory.Call(ctx, client, SessionHistoryParams{SessionID: sessionID, TurnID: turnID, After: after, Limit: sessionHistoryPage})
		if rpcErr := (*RPCError)(nil); errors.As(err, &rpcErr) && rpcErr.Code == CodeMethodNotFound {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		all = append(all, page.Events...)
		if !page.HasMore || len(page.Events) == 0 {
			return all, nil
		}
		after = page.Events[len(page.Events)-1].BmaSequence
	}
}
