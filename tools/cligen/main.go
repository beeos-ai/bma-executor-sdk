// Command cligen generates the resource commands of the bma CLI
// (cmd/bma/api_gen.go) from the public BMA OpenAPI document (ADR-0102):
// every operation becomes one entry of apiOperations, from which cmd/bma
// builds a cobra command. Never edit api_gen.go by hand; regenerate:
//
//	go run ./tools/cligen -spec contracts/bma-api/bma.openapi.yaml -out cmd/bma/api_gen.go
//
// Naming: the path's resource segments are the command groups and the
// operation is the verb. A trailing segment after a path parameter is a
// sub-resource (its own group) when the document lists it or has items below
// it; otherwise it is an action and its name is the verb
// (POST /v1/sessions/{id}/archive -> `bma sessions archive <id>`). Collection
// GET is list, collection POST create, item GET get, item POST/PUT/PATCH
// update, item DELETE delete. Names are kebab-case.
//
// Operations a generic request cannot express are marked HandWritten and
// covered by hand-written commands that name the operation in their
// "operation" annotation: event streams (text/event-stream) and multipart
// uploads. cmd/bma's parity test checks that every operation has exactly one
// command and every generated command an operation.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"go/format"
	"log"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type doc struct {
	Servers         []struct{ URL string } `yaml:"servers"`
	ExtensionFields []extField             `yaml:"x-bma-extension-fields"`
	Paths           map[string]map[string]yaml.Node
	Components      struct {
		Parameters    map[string]parameter `yaml:"parameters"`
		Schemas       map[string]*schema   `yaml:"schemas"`
		RequestBodies map[string]body      `yaml:"requestBodies"`
		Responses     map[string]response  `yaml:"responses"`
	}
}

// extField is a bma_* field of an official operation's body or answer.
type extField struct {
	Operation   string  `yaml:"operation"` // "METHOD /path[, METHOD /path...]"
	Direction   string  `yaml:"direction"`
	Field       string  `yaml:"field"`
	Description string  `yaml:"description"`
	Schema      *schema `yaml:"schema"`
}

type operation struct {
	OperationID string      `yaml:"operationId"`
	Summary     string      `yaml:"summary"`
	Description string      `yaml:"description"`
	Parameters  []parameter `yaml:"parameters"`
	RequestBody *body       `yaml:"requestBody"`
	Responses   map[string]response
	Deprecated  bool `yaml:"deprecated"`
}

type parameter struct {
	Ref         string  `yaml:"$ref"`
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Required    bool    `yaml:"required"`
	Description string  `yaml:"description"`
	Schema      *schema `yaml:"schema"`
}

type body struct {
	Ref      string `yaml:"$ref"`
	Required bool   `yaml:"required"`
	Content  map[string]struct {
		Schema *schema `yaml:"schema"`
	} `yaml:"content"`
}

type response struct {
	Ref     string `yaml:"$ref"`
	Content map[string]struct {
		Schema *schema `yaml:"schema"`
	} `yaml:"content"`
}

type schema struct {
	Ref         string             `yaml:"$ref"`
	Type        any                `yaml:"type"`
	Description string             `yaml:"description"`
	Properties  map[string]*schema `yaml:"properties"`
	Required    []string           `yaml:"required"`
	Enum        []any              `yaml:"enum"`
	AllOf       []*schema          `yaml:"allOf"`
	OneOf       []*schema          `yaml:"oneOf"`
	AnyOf       []*schema          `yaml:"anyOf"`
	Items       *schema            `yaml:"items"`
}

var (
	methods   = []string{"get", "post", "put", "patch", "delete"}
	versionRE = regexp.MustCompile(`^v[0-9]+$`)
)

func main() {
	specPath := flag.String("spec", "", "the public OpenAPI document")
	out := flag.String("out", "", "the Go file to write")
	flag.Parse()
	if *specPath == "" || *out == "" {
		log.Fatal("usage: cligen -spec FILE -out FILE")
	}
	raw, err := os.ReadFile(*specPath)
	if err != nil {
		log.Fatal(err)
	}
	src, err := generate(raw, *specPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatal(err)
	}
}

// op is one generated entry (mirrors cmd/bma apiOp).
type op struct {
	ID, Method, Path, Summary, Description string
	Group                                  []string
	Verb                                   string
	Params                                 []param
	Body, List, Raw, HandWritten           bool
	// Idempotent: the operation requires an Idempotency-Key header.
	Idempotent bool
}

type param struct {
	Name, Flag, In, Type, Description string
	Required                          bool
	Enum                              []string
}

func generate(raw []byte, source string) ([]byte, error) {
	var d doc
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	// The server URL's path prefixes the paths (/v1 for a document whose
	// paths start below it; nothing when the paths carry /v1 themselves).
	prefix := ""
	if len(d.Servers) > 0 {
		u, err := url.Parse(d.Servers[0].URL)
		if err != nil {
			return nil, err
		}
		prefix = strings.TrimRight(u.Path, "/")
	}
	extra := map[string][]extField{}
	for _, f := range d.ExtensionFields {
		if f.Direction != "request" {
			continue
		}
		for _, o := range strings.Split(f.Operation, ",") {
			extra[strings.TrimSpace(o)] = append(extra[strings.TrimSpace(o)], f)
		}
	}
	paths := make([]string, 0, len(d.Paths))
	for p := range d.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var ops []op
	seen := map[string]string{}
	for _, p := range paths {
		for _, m := range methods {
			n, ok := d.Paths[p][m]
			if !ok {
				continue
			}
			var o operation
			if err := n.Decode(&o); err != nil {
				return nil, fmt.Errorf("%s %s: %w", m, p, err)
			}
			if o.OperationID == "" {
				return nil, fmt.Errorf("%s %s has no operationId", m, p)
			}
			g, err := d.build(prefix, p, m, o, extra[strings.ToUpper(m)+" "+p])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", o.OperationID, err)
			}
			key := strings.Join(append(append([]string{}, g.Group...), g.Verb), " ")
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf("%s and %s both map to `bma %s`", prev, o.OperationID, key)
			}
			seen[key] = o.OperationID
			ops = append(ops, g)
		}
	}
	return render(ops, fmt.Sprintf("%s sha256:%x", source, sha256.Sum256(raw)))
}

func isParam(seg string) bool { return strings.HasPrefix(seg, "{") }

func kebab(s string) string { return strings.ReplaceAll(s, "_", "-") }

// flagName is the flag of a field or parameter: kebab-case without brackets,
// which are no flag syntax and which zsh would glob (group_by[] -> group-by,
// created_at[gte] -> created-at-gte).
func flagName(s string) string {
	s = strings.TrimSuffix(s, "[]")
	s = strings.NewReplacer("[", "-", "]", "").Replace(s)
	return kebab(s)
}

// subResource: a trailing segment after a parameter that the document lists
// (GET of a list) or has items below.
func (d *doc) subResource(path string) bool {
	for p := range d.Paths {
		if strings.HasPrefix(p, path+"/{") {
			return true
		}
	}
	if n, ok := d.Paths[path]["get"]; ok {
		var o operation
		if n.Decode(&o) == nil && d.isList(o) {
			return true
		}
	}
	return false
}

func (d *doc) build(prefix, path, method string, o operation, ext []extField) (op, error) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	version := ""
	if len(segs) > 1 && versionRE.MatchString(segs[0]) { // /v1/... : the version names no command
		version, segs = "/"+segs[0], segs[1:]
	}
	g := op{ID: o.OperationID, Method: strings.ToUpper(method), Path: prefix + path, Summary: o.Summary,
		Description: o.Description}
	last := segs[len(segs)-1]
	resource := segs
	switch {
	case isParam(last):
		resource = segs[:len(segs)-1]
		g.Verb = map[string]string{"get": "get", "post": "update", "put": "update", "patch": "update",
			"delete": "delete"}[method]
	case len(segs) == 1 || d.subResource(version+"/"+strings.Join(segs, "/")) || !isParam(segs[len(segs)-2]) && !d.hasCollection(version, segs):
		g.Verb = map[string]string{"get": "list", "post": "create", "put": "replace", "patch": "update",
			"delete": "delete"}[method]
		if method == "get" && !d.isList(o) {
			g.Verb = "get"
		}
	default:
		resource = segs[:len(segs)-1]
		g.Verb = kebab(last)
	}
	for _, s := range resource {
		if !isParam(s) {
			g.Group = append(g.Group, kebab(s))
		}
	}
	for _, s := range segs {
		if isParam(s) {
			name := strings.Trim(s, "{}")
			g.Params = append(g.Params, param{Name: name, In: "path", Type: "string", Required: true})
		}
	}
	for _, p := range o.Parameters {
		p = d.param(p)
		switch p.In {
		case "path":
			for i := range g.Params {
				if g.Params[i].In == "path" && g.Params[i].Name == p.Name {
					g.Params[i].Description = oneLine(p.Description)
				}
			}
		case "query":
			g.Params = append(g.Params, d.flagParam(p.Name, "query", p.Schema, p.Description, p.Required))
		case "header":
			if strings.EqualFold(p.Name, "Idempotency-Key") && p.Required {
				g.Idempotent = true
			}
		}
	}
	g.List = d.isList(o)
	for _, r := range o.Responses {
		r = d.response(r)
		for ct := range r.Content {
			switch {
			case ct == "text/event-stream":
				g.HandWritten = true
			case !strings.Contains(ct, "json"):
				g.Raw = true
			}
		}
	}
	if o.RequestBody != nil {
		b := *o.RequestBody
		if b.Ref != "" {
			b = d.Components.RequestBodies[refName(b.Ref)]
		}
		for ct, c := range b.Content {
			if strings.HasPrefix(ct, "multipart/") {
				g.HandWritten = true
				continue
			}
			g.Body = true
			s := d.flatten(c.Schema)
			if s == nil {
				continue
			}
			req := map[string]bool{}
			for _, r := range s.Required {
				req[r] = true
			}
			names := make([]string, 0, len(s.Properties))
			for n := range s.Properties {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				ps := d.resolve(s.Properties[n])
				g.Params = append(g.Params, d.flagParam(n, "body", ps, ps.Description, req[n]))
			}
		}
	}
	// bma_* request fields of official operations (x-bma-extension-fields).
	for _, f := range ext {
		g.Body = true
		g.Params = append(g.Params, d.flagParam(f.Field, "body", f.Schema, f.Description, false))
	}
	// A body field and a query parameter of one name share the flag name: the
	// body field is reached through --body.
	flags := map[string]bool{"body": true, "output": true, "all": true}
	var kept []param
	for _, p := range g.Params {
		if p.In != "path" {
			if flags[p.Flag] {
				continue
			}
			flags[p.Flag] = true
		}
		kept = append(kept, p)
	}
	g.Params = kept
	return g, nil
}

// hasCollection: the parent of a trailing non-parameter segment is itself a
// collection path (so the segment is an action on the collection, e.g.
// /machine_pools/check_connection).
func (d *doc) hasCollection(version string, segs []string) bool {
	_, ok := d.Paths[version+"/"+strings.Join(segs[:len(segs)-1], "/")]
	return ok
}

func (d *doc) flagParam(name, in string, s *schema, desc string, required bool) param {
	s = d.resolve(s)
	p := param{Name: name, Flag: flagName(name), In: in, Type: "json", Description: oneLine(desc), Required: required}
	if s != nil {
		if p.Description == "" {
			p.Description = oneLine(s.Description)
		}
		switch typeOf(s) {
		case "string":
			p.Type = "string"
		case "integer":
			p.Type = "integer"
		case "number":
			p.Type = "number"
		case "boolean":
			p.Type = "boolean"
		}
		for _, e := range s.Enum {
			if e != nil {
				p.Enum = append(p.Enum, fmt.Sprint(e))
			}
		}
		if p.Type == "json" && len(p.Enum) > 0 {
			p.Type = "string"
		}
	}
	return p
}

// typeOf is the schema's type, ignoring "null" in a type list; "" when it
// is not one scalar type.
func typeOf(s *schema) string {
	switch t := s.Type.(type) {
	case string:
		return t
	case []any:
		var out string
		for _, v := range t {
			if v == "null" {
				continue
			}
			if out != "" {
				return ""
			}
			out = fmt.Sprint(v)
		}
		return out
	}
	return ""
}

func (d *doc) param(p parameter) parameter {
	if p.Ref != "" {
		return d.Components.Parameters[refName(p.Ref)]
	}
	return p
}

func (d *doc) response(r response) response {
	if r.Ref != "" {
		return d.Components.Responses[refName(r.Ref)]
	}
	return r
}

func refName(ref string) string { return ref[strings.LastIndex(ref, "/")+1:] }

func (d *doc) resolve(s *schema) *schema {
	for s != nil && s.Ref != "" {
		s = d.Components.Schemas[refName(s.Ref)]
	}
	return s
}

// flatten merges allOf into one object schema; a oneOf/anyOf body has no
// field flags (it is given whole with --body), and an allOf part that is a
// oneOf/anyOf adds none (its variants' fields go through --body).
func (d *doc) flatten(s *schema) *schema {
	s = d.resolve(s)
	if s == nil || len(s.OneOf) > 0 || len(s.AnyOf) > 0 {
		return nil
	}
	if len(s.AllOf) == 0 {
		return s
	}
	out := &schema{Properties: map[string]*schema{}}
	for _, part := range s.AllOf {
		f := d.flatten(part)
		if f == nil {
			continue
		}
		for k, v := range f.Properties {
			out.Properties[k] = v
		}
		out.Required = append(out.Required, f.Required...)
	}
	for k, v := range s.Properties {
		out.Properties[k] = v
	}
	out.Required = append(out.Required, s.Required...)
	return out
}

// isList: the 200 answer is a page ({"data": [...]}).
func (d *doc) isList(o operation) bool {
	r, ok := o.Responses["200"]
	if !ok {
		return false
	}
	r = d.response(r)
	c, ok := r.Content["application/json"]
	if !ok {
		return false
	}
	s := d.flatten(c.Schema)
	if s == nil {
		return false
	}
	data := d.resolve(s.Properties["data"])
	return data != nil && typeOf(data) == "array"
}

// oneLine is the first line of a description, without backquotes (pflag
// reads a backquoted word of a flag's usage as the name of its value).
func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "`", "")
	if i := strings.IndexAny(s, "\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

func render(ops []op, source string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/cligen from %s. DO NOT EDIT.\n\n", strings.Fields(source)[0])
	b.WriteString("//go:build darwin || linux\n\npackage main\n\n")
	fmt.Fprintf(&b, "// contractSource is the OpenAPI document the commands were generated from.\nconst contractSource = %q\n\n", source)
	b.WriteString("// apiOperations are the operations of the public BMA API, one command each.\n")
	b.WriteString("var apiOperations = []apiOp{\n")
	for _, o := range ops {
		fmt.Fprintf(&b, "\t{ID: %q, Method: %q, Path: %q, Group: %#v, Verb: %q, Summary: %q,\n", o.ID, o.Method, o.Path,
			o.Group, o.Verb, o.Summary)
		if o.Description != "" {
			fmt.Fprintf(&b, "\t\tDescription: %q,\n", o.Description)
		}
		if o.Body || o.List || o.Raw || o.HandWritten {
			fmt.Fprintf(&b, "\t\tBody: %t, List: %t, Raw: %t, HandWritten: %t,\n", o.Body, o.List, o.Raw, o.HandWritten)
		}
		if o.Idempotent {
			b.WriteString("\t\tIdempotent: true,\n")
		}
		if len(o.Params) > 0 {
			b.WriteString("\t\tParams: []apiParam{\n")
			for _, p := range o.Params {
				fmt.Fprintf(&b, "\t\t\t{Name: %q, Flag: %q, In: %q, Type: %q, Required: %t", p.Name, p.Flag, p.In, p.Type,
					p.Required)
				if p.Description != "" {
					fmt.Fprintf(&b, ", Description: %q", p.Description)
				}
				if len(p.Enum) > 0 {
					fmt.Fprintf(&b, ", Enum: %#v", p.Enum)
				}
				b.WriteString("},\n")
			}
			b.WriteString("\t\t},\n")
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
