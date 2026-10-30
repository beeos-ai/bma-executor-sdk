package bmaexecutor

import (
	"encoding/json"
	"os"
	"testing"
)

type conformanceFixture struct {
	Protocol string `json:"protocol"`
	Messages []struct {
		Name    string          `json:"name"`
		Method  string          `json:"method"`
		Kind    string          `json:"kind"`
		Message json.RawMessage `json:"message"`
		Result  json.RawMessage `json:"result"`
	} `json:"messages"`
}

func TestConformanceFixtureDecodesIntoGeneratedTypes(t *testing.T) {
	raw, err := os.ReadFile("testdata/conformance.json")
	if err != nil {
		t.Fatalf("read conformance fixture: %v", err)
	}
	var fixture conformanceFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode conformance fixture: %v", err)
	}
	if fixture.Protocol != ProtocolVersion {
		t.Fatalf("fixture protocol %q != %q", fixture.Protocol, ProtocolVersion)
	}
	if len(Methods) != 15 {
		t.Fatalf("expected 15 methods, got %d", len(Methods))
	}
	for _, entry := range fixture.Messages {
		spec, ok := Methods[entry.Method]
		if !ok {
			t.Errorf("%s: unknown method %s", entry.Name, entry.Method)
			continue
		}
		if entry.Kind == "result" {
			var envelope struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(entry.Message, &envelope); err == nil && envelope.ID != "" {
				t.Errorf("%s: result entry carries a message envelope", entry.Name)
			}
			if err := decodeResult(entry.Method, entry.Result); err != nil {
				t.Errorf("%s: %v", entry.Name, err)
			}
			continue
		}
		if string(spec.Kind) != entry.Kind {
			t.Errorf("%s: method kind %s != fixture %s", entry.Name, spec.Kind, entry.Kind)
		}
		var envelope struct {
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(entry.Message, &envelope); err != nil {
			t.Errorf("%s: decode envelope: %v", entry.Name, err)
			continue
		}
		if err := decodeParams(entry.Method, envelope.Params); err != nil {
			t.Errorf("%s: %v", entry.Name, err)
		}
	}
}

func decodeParams(method string, raw json.RawMessage) error {
	switch method {
	case MethodExecutorRegister:
		return json.Unmarshal(raw, &ExecutorRegisterParams{})
	case MethodExecutorHeartbeat:
		return json.Unmarshal(raw, &ExecutorHeartbeatParams{})
	case MethodExecutorCapabilitiesUpdate:
		return json.Unmarshal(raw, &ExecutorCapabilitiesUpdateParams{})
	case MethodExecutorDrain:
		return json.Unmarshal(raw, &ExecutorDrainParams{})
	case MethodExecutorClose:
		return json.Unmarshal(raw, &ExecutorCloseParams{})
	case MethodSessionAssign:
		return json.Unmarshal(raw, &SessionAssignParams{})
	case MethodSessionReady:
		return json.Unmarshal(raw, &SessionReadyParams{})
	case MethodSessionEvent:
		return json.Unmarshal(raw, &SessionEventParams{})
	case MethodSessionStatus:
		return json.Unmarshal(raw, &SessionStatusParams{})
	case MethodSessionStop:
		return json.Unmarshal(raw, &SessionStopParams{})
	case MethodSessionEnded:
		return json.Unmarshal(raw, &SessionEndedParams{})
	case MethodToolCall:
		return json.Unmarshal(raw, &ToolCallParams{})
	case MethodToolResult:
		return json.Unmarshal(raw, &ToolResultParams{})
	case MethodToolCancel:
		return json.Unmarshal(raw, &ToolCancelParams{})
	case MethodToolProgress:
		return json.Unmarshal(raw, &ToolProgressParams{})
	default:
		return nil
	}
}

func decodeResult(method string, raw json.RawMessage) error {
	switch method {
	case MethodExecutorRegister:
		return json.Unmarshal(raw, &ExecutorRegisterResult{})
	case MethodExecutorHeartbeat:
		return json.Unmarshal(raw, &ExecutorHeartbeatResult{})
	case MethodExecutorDrain:
		return json.Unmarshal(raw, &ExecutorDrainResult{})
	case MethodSessionAssign:
		return json.Unmarshal(raw, &SessionAssignResult{})
	case MethodSessionReady, MethodToolCall, MethodToolResult:
		return json.Unmarshal(raw, &Ack{})
	case MethodSessionEvent:
		return json.Unmarshal(raw, &SessionEventResult{})
	case MethodSessionStop:
		return json.Unmarshal(raw, &SessionStopResult{})
	case MethodSessionEnded:
		return json.Unmarshal(raw, &SessionEndedResult{})
	case MethodToolCancel:
		return json.Unmarshal(raw, &ToolCancelResult{})
	default:
		return nil
	}
}
