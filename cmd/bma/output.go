//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/itchyny/gojq"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// stdoutTerminal reports whether stdout is a terminal.
var stdoutTerminal = func() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), ioctlReadTermios)
	return err == nil
}

// decodeJSON decodes an API answer keeping numbers exact.
func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("the answer is not JSON: %w", err)
	}
	return v, nil
}

// format is the output format of a value: the flag, else a table for lists
// on a terminal, else JSON.
func format(list bool) string {
	if opts.Format != "" {
		return opts.Format
	}
	if list && opts.Query == "" && stdoutTerminal() {
		return "table"
	}
	return "json"
}

// printValue writes v (a decoded answer, or a list of items when list) in
// the output format after --query.
func printValue(v any, list bool) error {
	v = normalize(v)
	if opts.Query != "" {
		q, err := gojq.Parse(opts.Query)
		if err != nil {
			return usageError("--query: " + err.Error())
		}
		var results []any
		iter := q.Run(v)
		for {
			r, ok := iter.Next()
			if !ok {
				break
			}
			if err, ok := r.(error); ok {
				return fmt.Errorf("--query: %w", err)
			}
			results = append(results, r)
		}
		f := opts.Format
		if f == "" || f == "table" {
			f = "jsonl"
		}
		for _, r := range results {
			if s, ok := r.(string); ok && opts.Format == "" {
				fmt.Fprintln(stdout, s) // raw strings, as jq -r: the common scripting case
				continue
			}
			if err := write(r, f); err != nil {
				return err
			}
		}
		return nil
	}
	f := format(list)
	if list && f == "jsonl" {
		items, _ := v.([]any)
		for _, it := range items {
			if err := write(it, "jsonl"); err != nil {
				return err
			}
		}
		return nil
	}
	return write(v, f)
}

func write(v any, f string) error {
	switch f {
	case "jsonl":
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	case "yaml":
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = stdout.Write(b)
		return err
	case "table":
		if items, ok := v.([]any); ok {
			return table(items)
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", b)
	return err
}

// normalize turns json.Number into int64/float64 for gojq and yaml.
func normalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = normalize(x)
		}
	case []any:
		for i, x := range t {
			t[i] = normalize(x)
		}
	}
	return v
}

// tableColumns are the columns a list shows, the first of each group the
// items have.
var tableColumns = [][]string{{"id"}, {"name", "display_name", "title"}, {"type"}, {"status", "state"},
	{"created_at"}}

func table(items []any) error {
	var cols []string
	for _, group := range tableColumns {
	next:
		for _, c := range group {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					if _, ok := m[c]; ok {
						cols = append(cols, c)
						break next
					}
				}
			}
		}
	}
	if len(cols) == 0 {
		return write(items, "json")
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.ToUpper(strings.Join(cols, "\t")))
	for _, it := range items {
		m, _ := it.(map[string]any)
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = cell(m[c])
		}
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	return w.Flush()
}

func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case string:
		return t
	case map[string]any:
		if s, ok := t["type"].(string); ok {
			return s
		}
		b, _ := json.Marshal(t)
		return string(b)
	}
	return fmt.Sprint(v)
}
