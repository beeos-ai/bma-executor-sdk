package bmaexecutor

import (
	"context"
	"errors"
	"testing"
)

// A turn whose input is closed (the host took its last input and ends it)
// answers a later turn.input conflict, so the platform runs that input as a
// turn of its own; one that waits unread keeps the input open until read.
func TestClosedTurnInputAnswersConflict(t *testing.T) {
	runtime := &agentRuntime{sessions: map[string]*agentSession{}}
	s := &agentSession{}
	turn := &AgentTurn{session: s, params: TurnStartParams{TurnID: "t"}, seen: map[string]bool{"opening": true},
		inputs: make(chan TurnInputParams, agentInputBuffer), done: make(chan struct{})}
	s.turn = turn
	runtime.sessions["s"] = s

	ack, err := runtime.turnInput(context.Background(), TurnInputParams{SessionID: "s", TurnID: "t", InputID: "steer"})
	if err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	if turn.closeInputs() {
		t.Fatal("an accepted input waits unread: the input must stay open")
	}
	<-turn.Inputs()
	if !turn.closeInputs() {
		t.Fatal("nothing waits: the input closes")
	}
	_, err = runtime.turnInput(context.Background(), TurnInputParams{SessionID: "s", TurnID: "t", InputID: "late"})
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeConflict {
		t.Fatalf("a turn.input after the input closed: %v, want conflict", err)
	}
}
