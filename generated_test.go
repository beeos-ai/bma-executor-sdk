package bmaexecutor

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestConformanceExamplesRoundTrip decodes every example of the vendored
// contract snapshot into the generated type and encodes it again: a member the
// type does not model, a wrong omitempty, or a wrong JSON name changes it.
func TestConformanceExamplesRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/bma/executor/examples/conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Messages []struct {
			Name    string `json:"name"`
			Method  string `json:"method"`
			Kind    string `json:"kind"`
			Message *struct {
				Params json.RawMessage `json:"params"`
			} `json:"message"`
			Result json.RawMessage `json:"result"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, example := range fixture.Messages {
		spec, ok := Methods[example.Method]
		if !ok {
			t.Errorf("%s: method not in the generated catalog", example.Name)
			continue
		}
		if spec.Notification != (example.Kind == "notification") && example.Kind != "result" {
			t.Errorf("%s: kind %s does not match the catalog", example.Name, example.Kind)
		}
		payload, target := json.RawMessage(nil), any(nil)
		if example.Kind == "result" {
			payload, target = example.Result, NewResult(example.Method)
		} else {
			payload, target = example.Message.Params, NewParams(example.Method)
		}
		if err := json.Unmarshal(payload, target); err != nil {
			t.Errorf("%s: decode: %v", example.Name, err)
			continue
		}
		encoded, _ := json.Marshal(target)
		var want, got any
		_ = json.Unmarshal(payload, &want)
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s: round trip changed the message\nwant %s\ngot  %s", example.Name, payload, encoded)
		}
		covered[example.Method] = true
	}
	for method := range Methods {
		if !covered[method] {
			t.Errorf("method %s has no conformance example", method)
		}
	}
}
