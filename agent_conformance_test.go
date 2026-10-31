package bmaexecutor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestAgentConformanceFixtures decodes every agent-capability example of the
// BMA contract strictly into the SDK types, and runs agent session events
// through the harness validation.
func TestAgentConformanceFixtures(t *testing.T) {
	raw, err := os.ReadFile("../contracts/bma/executor/examples/conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Messages []struct {
			Name    string `json:"name"`
			Method  string `json:"method"`
			Kind    string `json:"kind"`
			Message struct {
				Params json.RawMessage `json:"params"`
			} `json:"message"`
			Result json.RawMessage `json:"result"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	params := map[string]func() any{
		MethodTurnStart: func() any { return &TurnStartParams{} }, MethodTurnInput: func() any { return &TurnInputParams{} },
		MethodTurnCancel: func() any { return &TurnCancelParams{} }, MethodSessionClose: func() any { return &SessionCloseParams{} },
		MethodSessionEventDelta: func() any { return &SessionEventDeltaParams{} }, MethodInteractionRequest: func() any { return &InteractionRequestParams{} },
		MethodInteractionResponse: func() any { return &InteractionResponseParams{} }, MethodInteractionCancel: func() any { return &InteractionCancelParams{} },
		MethodSessionEvent: func() any { return &agentSessionEventParams{} },
	}
	results := map[string]func() any{
		MethodTurnStart: func() any { return &TurnStartResult{} }, MethodTurnInput: func() any { return &Ack{} },
		MethodTurnCancel: func() any { return &TurnCancelResult{} }, MethodSessionClose: func() any { return &SessionCloseResult{} },
		MethodInteractionRequest: func() any { return &InteractionRequestResult{} }, MethodInteractionResponse: func() any { return &InteractionResponseResult{} },
		MethodInteractionCancel: func() any { return &Ack{} }, MethodSessionEvent: func() any { return &agentSessionEventResult{} },
	}
	harness := NewHarness(HarnessOptions{Agent: newRecordingPlatform()})
	covered := map[string]bool{}
	for _, fixture := range fixtures.Messages {
		agentExample := Methods[fixture.Method].Capability == "agent" ||
			fixture.Method == MethodSessionEvent && strings.Contains(fixture.Name, "agent")
		if !agentExample {
			continue
		}
		body, factory := fixture.Message.Params, params[fixture.Method]
		if fixture.Kind == "result" {
			body, factory = fixture.Result, results[fixture.Method]
		}
		if factory == nil {
			t.Fatalf("%s: no SDK type", fixture.Name)
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(factory()); err != nil {
			t.Errorf("%s: %v", fixture.Name, err)
		}
		covered[fixture.Method+"/"+fixture.Kind] = true
		if fixture.Method == MethodSessionEvent && fixture.Kind == "request" {
			var event agentSessionEventParams
			_ = json.Unmarshal(body, &event)
			event.Sequence = 1 // each fixture is checked against a fresh session
			event.SessionID = fixture.Name
			fresh, _ := json.Marshal(event)
			if _, rpcError, handled := harness.dispatchAgent(context.Background(), &Executor{h: harness}, MethodSessionEvent, fresh); !handled || rpcError != nil {
				t.Errorf("%s: harness rejected the event: %+v", fixture.Name, rpcError)
			}
		}
	}
	for method := range params {
		kind := "request"
		if method == MethodSessionEventDelta {
			kind = "notification"
		}
		if !covered[method+"/"+kind] {
			t.Errorf("no %s example for %s", kind, method)
		}
	}
}
