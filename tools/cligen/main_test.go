package main

import (
	"strings"
	"testing"
)

const spec = `
servers: [{url: https://api.example.com}]
paths:
  /v1/reports:
    get:
      operationId: reports.list
      parameters:
        - {name: "group_by[]", in: query, schema: {type: array, items: {type: string}}}
        - {name: "created_at[gte]", in: query, schema: {type: string}}
      responses: {"200": {content: {application/json: {schema: {properties: {data: {type: array}}}}}}}
  /v1/pools:
    post:
      operationId: pools.create
      requestBody:
        content:
          application/json:
            schema:
              allOf:
                - {properties: {name: {type: string}}, required: [name]}
                - oneOf:
                    - {properties: {provider: {type: string, enum: [a]}, cpu: {type: integer}}}
                    - {properties: {provider: {type: string, enum: [b]}}}
      responses: {"200": {content: {application/json: {schema: {type: object}}}}}
`

func TestFlagsOfBracketedQueryAndMixedBodies(t *testing.T) {
	src, err := generate([]byte(spec), "spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		// Brackets are not flag syntax (and zsh globs them): the flag drops them.
		`{Name: "group_by[]", Flag: "group-by", In: "query", Type: "json"`,
		`{Name: "created_at[gte]", Flag: "created-at-gte", In: "query", Type: "string"`,
		// The fields every variant shares are flags; the variants go through --body.
		`{Name: "name", Flag: "name", In: "body", Type: "string", Required: true}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if strings.Contains(s, `Flag: "provider"`) {
		t.Errorf("a field of one oneOf variant became a flag:\n%s", s)
	}
}
