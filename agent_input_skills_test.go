package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnsupportedSelectedSkillsRefusedBeforeHandler(t *testing.T) {
	runtime := &agentRuntime{}
	params := TurnStartParams{SessionID: "sesn_test", TurnID: "turn_test", InputID: "in_test", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "test"}}, Config: &AgentTurnConfig{InputSkills: []InputSkillDocument{{SkillID: "skl_test", RevisionID: "skr_v1", Name: "Review", Body: "skill", BodySHA256: strings.Repeat("a", 64)}}}}
	_, err := runtime.turnStart(context.Background(), params)
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeCapabilityUnavailable {
		t.Fatal(err)
	}
	params.Config = &AgentTurnConfig{InputSkillReceipt: &InputSkillReceipt{InputID: "in_test"}}
	_, err = runtime.turnStart(context.Background(), params)
	if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams {
		t.Fatal(err)
	}
}

func TestSelectedSkillWirePresenceAndShape(t *testing.T) {
	runtime := &agentRuntime{}
	for _, value := range []string{`{}`, `[]`, `null`, `false`, `[null]`, `[{"skill_id":"skl_only"}]`} {
		raw := json.RawMessage(`{"session_id":"s","turn_id":"t","input_id":"i","input":[{"type":"text","text":"x"}],"config":{"input_skills":` + value + `}}`)
		_, err := runtime.turnStartRaw(context.Background(), raw)
		var rpc *RPCError
		if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams {
			t.Fatal(value, err)
		}
	}
	_, err := runtime.turnStartRaw(context.Background(), json.RawMessage(`{"config":{"input_skill_receipt":null}}`))
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeInvalidParams {
		t.Fatal(err)
	}
}

func TestSelectedSkillInputRejectsAnotherMidTurnInput(t *testing.T) {
	runtime := &agentRuntime{sessions: map[string]*agentSession{}}
	turn := &AgentTurn{params: TurnStartParams{TurnID: "t", Config: &AgentTurnConfig{InputSkills: []InputSkillDocument{{SkillID: "skl_one"}}}}, seen: map[string]bool{"opening": true}}
	runtime.sessions["s"] = &agentSession{turn: turn}
	_, err := runtime.turnInput(context.Background(), TurnInputParams{SessionID: "s", TurnID: "t", InputID: "other"})
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != CodeConflict {
		t.Fatal(err)
	}
	ack, err := runtime.turnInput(context.Background(), TurnInputParams{SessionID: "s", TurnID: "t", InputID: "opening"})
	if err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
}
