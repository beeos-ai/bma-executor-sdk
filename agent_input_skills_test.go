package bmaexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
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

func TestInputSkillReceiptMatchesExactOpeningSelection(t *testing.T) {
	params := TurnStartParams{InputID: "opening", Config: &AgentTurnConfig{InputSkills: []InputSkillDocument{{SkillID: "skl_one", RevisionID: "skr_one", BodySHA256: strings.Repeat("a", 64)}}}}
	result := TurnStartResult{Effective: AgentTurnConfig{InputSkillReceipt: &InputSkillReceipt{InputID: "opening", Skills: []InputSkillVersionReceipt{{SkillID: "skl_one", RevisionID: "skr_one", BodySHA256: strings.Repeat("a", 64)}}}}}
	if err := validateInputSkillReceipt(params, result); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*TurnStartResult){
		func(r *TurnStartResult) { r.Effective.InputSkillReceipt = nil },
		func(r *TurnStartResult) { r.Effective.InputSkillReceipt.InputID = "other" },
		func(r *TurnStartResult) { r.Effective.InputSkillReceipt.Skills[0].RevisionID = "skr_other" },
		func(r *TurnStartResult) { r.Effective.InputSkillReceipt.Skills = nil },
	} {
		raw, _ := json.Marshal(result)
		var changed TurnStartResult
		_ = json.Unmarshal(raw, &changed)
		change(&changed)
		if validateInputSkillReceipt(params, changed) == nil {
			t.Fatal("mismatched receipt accepted")
		}
	}
}

type heldSkillReceiptAgent struct {
	nopAgent
	started, release chan struct{}
}

func (a heldSkillReceiptAgent) OpenTurn(context.Context, TurnStartParams) (TurnStartResult, error) {
	close(a.started)
	<-a.release
	return TurnStartResult{Accepted: true}, nil
}
func TestSkillReceiptRefusalUnblocksConcurrentSessionClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	client := &Client{rootCtx: ctx, sessions: map[string]struct{}{"s": {}}}
	runtime := &agentRuntime{client: client, handler: heldSkillReceiptAgent{started: started, release: release}, capability: AgentCapability{InputSkills: true}, sessions: map[string]*agentSession{}}
	drained := make(chan struct{})
	close(drained)
	session := &agentSession{runtime: runtime, id: "s", drained: drained, wake: make(chan struct{}, 1), results: map[string]TurnStartResult{}}
	runtime.sessions["s"] = session
	opened := make(chan error, 1)
	go func() {
		_, err := runtime.turnStart(ctx, TurnStartParams{SessionID: "s", TurnID: "t", InputID: "opening", Input: []AgentInputBlock{{Type: AgentInputBlockTypeText, Text: "x"}}, Config: &AgentTurnConfig{InputSkills: []InputSkillDocument{{SkillID: "skl_one", RevisionID: "skr_one", Name: "One", Body: "skill", BodySHA256: strings.Repeat("a", 64)}}}})
		opened <- err
	}()
	<-started
	session.mu.Lock()
	turn := session.turn
	originalCancel := turn.cancel
	closing := make(chan struct{})
	turn.cancel = func() {
		originalCancel()
		select {
		case <-closing:
		default:
			close(closing)
		}
	}
	session.mu.Unlock()
	closed := make(chan error, 1)
	go func() { _, err := runtime.sessionClose(ctx, SessionCloseParams{SessionID: "s"}); closed <- err }()
	<-closing
	close(release)
	if err := <-opened; err == nil {
		t.Fatal("missing receipt accepted")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("close blocked after receipt refusal")
	}
}
