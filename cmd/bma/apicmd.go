//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// apiOp is one operation of the public API (api_gen.go, tools/cligen).
type apiOp struct {
	ID, Method, Path, Summary, Description string
	Group                                  []string
	Verb                                   string
	Params                                 []apiParam
	// Body: the operation takes a JSON body; List: it answers a page; Raw: it
	// answers bytes; HandWritten: a hand-written command serves it.
	Body, List, Raw, HandWritten bool
	// Idempotent: the operation requires an Idempotency-Key; the command
	// sends --idempotency-key, else a new key per run (the SDK's retries of
	// that run reuse it).
	Idempotent bool
}

// apiParam is a path parameter (positional), a query parameter or a
// top-level body field (flags).
type apiParam struct {
	Name, Flag, In, Type, Description string
	Required                          bool
	Enum                              []string
}

// group returns the command of the group path under root, creating it.
func group(root *cobra.Command, names []string) *cobra.Command {
	cur := root
	for i, n := range names {
		var found *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == n {
				found = c
				break
			}
		}
		if found == nil {
			found = &cobra.Command{Use: n, Short: "Manage " + strings.ReplaceAll(strings.Join(names[:i+1], " "), "-", " "),
				Args: args(0), RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
			if cur == root {
				found.GroupID = "api"
			}
			cur.AddCommand(found)
		}
		cur = found
	}
	return cur
}

// addAPICommands adds a command per generated operation.
func addAPICommands(root *cobra.Command) {
	for i := range apiOperations {
		op := &apiOperations[i]
		if op.HandWritten {
			continue
		}
		group(root, op.Group).AddCommand(apiCommand(op))
	}
}

func apiCommand(op *apiOp) *cobra.Command {
	var positional []string
	for _, p := range op.Params {
		if p.In == "path" {
			positional = append(positional, "<"+p.Name+">")
		}
	}
	long := op.Summary
	if op.Description != "" {
		long += ".\n\n" + op.Description
	}
	long += fmt.Sprintf("\n\n%s %s", op.Method, op.Path)
	cmd := &cobra.Command{
		Use:         strings.TrimSpace(op.Verb + " " + strings.Join(positional, " ")),
		Short:       op.Summary,
		Long:        long,
		Args:        args(len(positional)),
		Annotations: map[string]string{"operation": op.ID},
	}
	flags := map[string]*string{}
	for _, p := range op.Params {
		if p.In == "path" {
			continue
		}
		usage := p.Description
		if p.Type == "json" {
			usage = strings.TrimSpace(usage + " (JSON or YAML, @file, - for stdin)")
		}
		if len(p.Enum) > 0 {
			usage = strings.TrimSpace(usage + " (one of " + strings.Join(p.Enum, ", ") + ")")
		}
		if p.Required {
			usage = strings.TrimSpace(usage + " (required)")
		}
		flags[p.Flag] = cmd.Flags().String(p.Flag, "", usage)
	}
	var body, output string
	var all bool
	if op.Body {
		cmd.Flags().StringVar(&body, "body", "", "the whole request body: JSON or YAML, @file or - for stdin; flags override its fields")
	}
	if op.List {
		cmd.Flags().BoolVar(&all, "all", false, "fetch every page")
	}
	if op.Raw {
		cmd.Flags().StringVarP(&output, "output", "o", "", "write the content to this file (default: stdout)")
	}
	var key string
	if op.Idempotent {
		cmd.Flags().StringVar(&key, "idempotency-key", "",
			"Idempotency-Key of the request; reuse it to retry safely (default: a new key per run)")
	}
	cmd.RunE = func(cmd *cobra.Command, a []string) error {
		s, err := apiSession()
		if err != nil {
			return err
		}
		path, query, payload, err := buildRequest(op, a, body, func(flag string) (string, bool) {
			return *flags[flag], cmd.Flags().Changed(flag)
		})
		if err != nil {
			return err
		}
		var o []option.RequestOption
		if op.Idempotent {
			if key == "" {
				key = newIdempotencyKey()
			}
			o = append(o, option.WithHeader("Idempotency-Key", key))
		}
		return runOp(cmd.Context(), s, op, path, query, payload, all, output, o...)
	}
	return cmd
}

// buildRequest fills the path, the query and the body of op from the
// positional arguments, --body and the flags set.
func buildRequest(op *apiOp, positional []string, body string, flag func(string) (string, bool)) (
	string, url.Values, []byte, error) {
	path := strings.TrimPrefix(op.Path, "/")
	query := url.Values{}
	fields := map[string]any{}
	if body != "" {
		v, err := readValue(body)
		if err != nil {
			return "", nil, nil, usageError("--body: " + err.Error())
		}
		m, ok := v.(map[string]any)
		if !ok {
			return "", nil, nil, usageError("--body must be an object")
		}
		fields = m
	}
	i := 0
	for _, p := range op.Params {
		if p.In == "path" {
			path = strings.Replace(path, "{"+p.Name+"}", url.PathEscape(positional[i]), 1)
			i++
			continue
		}
		raw, set := flag(p.Flag)
		if !set {
			continue
		}
		if len(p.Enum) > 0 && !contains(p.Enum, raw) {
			return "", nil, nil, usageError(fmt.Sprintf("--%s must be one of %s", p.Flag, strings.Join(p.Enum, ", ")))
		}
		if p.In == "query" {
			if p.Type == "json" { // an array is sent once per value
				v, err := typed(p, raw)
				if err != nil {
					return "", nil, nil, err
				}
				if list, ok := v.([]any); ok {
					for _, x := range list {
						query.Add(p.Name, fmt.Sprint(x))
					}
					continue
				}
			}
			query.Set(p.Name, raw)
			continue
		}
		v, err := typed(p, raw)
		if err != nil {
			return "", nil, nil, err
		}
		fields[p.Name] = v
	}
	var payload []byte
	if op.Body {
		var err error
		if payload, err = json.Marshal(fields); err != nil {
			return "", nil, nil, err
		}
	}
	return path, query, payload, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func typed(p apiParam, raw string) (any, error) {
	switch p.Type {
	case "string":
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, usageError("--" + p.Flag + " must be an integer")
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, usageError("--" + p.Flag + " must be a number")
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, usageError("--" + p.Flag + " must be true or false")
		}
		return b, nil
	}
	v, err := readValue(raw)
	if err != nil {
		return nil, usageError("--" + p.Flag + ": " + err.Error())
	}
	return v, nil
}

// readValue reads JSON or YAML from s, @file or - (stdin).
func readValue(s string) (any, error) {
	var b []byte
	var err error
	switch {
	case s == "-":
		b, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(s, "@"):
		b, err = os.ReadFile(s[1:])
	default:
		b = []byte(s)
	}
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil { // YAML is a superset of JSON
		return nil, err
	}
	return v, nil
}

// do sends one request through the SDK client (credentials, scope headers,
// retries with Retry-After, *anthropic.Error) and answers the raw response.
func (s *session) do(ctx context.Context, method, path string, query url.Values, payload []byte,
	o ...option.RequestOption) (*http.Response, error) {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var params any
	if payload != nil {
		params = payload
	}
	var resp *http.Response
	if err := s.client.Execute(ctx, method, path, params, &resp, o...); err != nil {
		return nil, err
	}
	return resp, nil
}

// newIdempotencyKey is a random Idempotency-Key.
func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "bma-cli-" + hex.EncodeToString(b)
}

func runOp(ctx context.Context, s *session, op *apiOp, path string, query url.Values, payload []byte, all bool,
	output string, o ...option.RequestOption) error {
	if op.Raw {
		resp, err := s.do(ctx, op.Method, path, query, payload, o...)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		return writeContent(resp.Body, output)
	}
	if !all {
		v, err := s.json(ctx, op.Method, path, query, payload, o...)
		if err != nil || v == nil {
			return err
		}
		if page, ok := v.(map[string]any); ok && op.List && opts.Query == "" && format(true) == "table" {
			return printValue(page["data"], true) // a page as a table: its items
		}
		return printValue(v, false)
	}
	var items []any
	for {
		v, err := s.json(ctx, op.Method, path, query, payload, o...)
		if err != nil {
			return err
		}
		page, _ := v.(map[string]any)
		data, _ := page["data"].([]any)
		items = append(items, data...)
		if next, _ := page["next_page"].(string); next != "" {
			query.Set("page", next)
		} else if more, _ := page["has_more"].(bool); more && page["last_id"] != nil {
			query.Set("after_id", fmt.Sprint(page["last_id"]))
		} else {
			break
		}
	}
	if items == nil {
		items = []any{}
	}
	return printValue(items, true)
}

// json sends a request and decodes its answer; nil for an empty answer.
func (s *session) json(ctx context.Context, method, path string, query url.Values, payload []byte,
	o ...option.RequestOption) (any, error) {
	resp, err := s.do(ctx, method, path, query, payload, o...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	return decodeJSON(b)
}

// writeContent copies r to the file output (atomically) or to stdout.
func writeContent(r io.Reader, output string) error {
	if output == "" || output == "-" {
		_, err := io.Copy(stdout, r)
		return err
	}
	tmp := output + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, output)
}
