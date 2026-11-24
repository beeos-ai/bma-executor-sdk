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
  /v1/pools/{pool_id}/fork:
    post:
      operationId: pools.fork
      parameters:
        - {name: pool_id, in: path, required: true, schema: {type: string}}
        - {name: Idempotency-Key, in: header, required: true, schema: {type: string}}
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

// An operation that requires an Idempotency-Key header is marked so the
// command sends one; the header is no flag of its own.
func TestRequiredIdempotencyKeyMarksTheOperation(t *testing.T) {
	src, err := generate([]byte(spec), "spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, `{ID: "pools.fork"`)
	if i < 0 || !strings.Contains(s[i:], "Idempotent: true") || strings.Contains(s, `Name: "Idempotency-Key"`) {
		t.Fatalf("pools.fork not marked idempotent:\n%s", s)
	}
	if j := strings.Index(s, `{ID: "pools.create"`); strings.Contains(s[j:i], "Idempotent") {
		t.Fatalf("an operation without the header is marked:\n%s", s)
	}
}

const exampleSpec = `
servers: [{url: https://api.example.com}]
paths:
  /v1/things/{thing_id}/notes:
    post:
      operationId: notes.create
      parameters:
        - {name: thing_id, in: path, required: true, schema: {type: string}, example: thg_01}
      requestBody:
        content:
          application/json:
            schema: {properties: {text: {type: string}, tags: {type: array}, due: {type: string}}}
            example: {text: "it's done", tags: [a, b], due: 2026-11-24T06:00:00Z, extra: {x: 1}}
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
              example: {type: note, id: nte_01, done: true}
`

// The example is the documented call in key order: path examples as
// arguments, body fields as flags (quoted for a shell, JSON when not a
// string), fields without a flag in --body; the output keeps key order.
func TestExampleAndOutputFromTheDocumentedExamples(t *testing.T) {
	src, err := generate([]byte(exampleSpec), "spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		`Example: "bma things notes thg_01 --text 'it'\\''s done' --tags '[\"a\",\"b\"]' --due 2026-11-24T06:00:00Z --body '{\"extra\":{\"x\":1}}'"`,
		`Output:  "{\n  \"type\": \"note\",\n  \"id\": \"nte_01\",\n  \"done\": true\n}"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
}
