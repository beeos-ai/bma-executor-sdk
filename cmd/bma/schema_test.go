//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func schemaProperties(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile("schema/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for k := range s.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The published session schema has exactly the members launch reads.
func TestSessionSchemaMatchesTheLaunchFile(t *testing.T) {
	var fields []string
	typ := reflect.TypeOf(launchSpec{})
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Tag.Get("yaml"))
	}
	sort.Strings(fields)
	if got := schemaProperties(t, "session.schema.json"); !reflect.DeepEqual(got, fields) {
		t.Fatalf("schema %v, launch file %v", got, fields)
	}
}

// The agent schema covers every member of an agent create in the contract.
func TestAgentSchemaCoversTheAgentCreate(t *testing.T) {
	b, err := os.ReadFile("../../contracts/bma-api/bma.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]any `yaml:"properties"`
					} `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"requestBody"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	have := strings.Join(schemaProperties(t, "agent.schema.json"), ",")
	props := doc.Paths["/v1/agents"]["post"].RequestBody.Content["application/json"].Schema.Properties
	if len(props) == 0 {
		t.Fatal("no agent create body in the contract")
	}
	for k := range props {
		if !strings.Contains(","+have+",", ","+k+",") {
			t.Errorf("agent.schema.json lacks %s", k)
		}
	}
}

func TestExamplesAreValidFiles(t *testing.T) {
	for _, name := range []string{"session.yaml", "session-inline-agent.yaml"} {
		var spec launchSpec
		if _, err := readSpecFile("examples/"+name, &spec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	var agent map[string]any
	if _, err := readSpecFile("examples/agent.yaml", &agent); err != nil || agent["name"] == nil || agent["model"] == nil {
		t.Errorf("agent.yaml: %v %v", err, agent)
	}
}
