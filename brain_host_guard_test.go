package bmaexecutor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"strconv"
	"testing"
)

// brainHostFiles are the source of the host: the platform semantics every
// brain shares (ADR-0066 of BMA). Adapters live in their own files.
var brainHostFiles = []string{
	"brain_adapter.go", "brain_host.go", "brain_host_open.go",
	"agent_acp_host_tools.go", "agent_acp_images.go", "agent_acp_remote.go", "agent_acp_resume.go",
}

// brainNames match a brain's name in code.
var brainNames = regexp.MustCompile(`(?i)claude|dsh`)

// The host never asks which brain it hosts: no identifier or string naming a
// brain, no profile, no assertion to an adapter's concrete type. Comments may
// name brains.
func TestBrainHostNamesNoBrain(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range brainHostFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range brainNameUses(file) {
			t.Errorf("%s: %s", fset.Position(problem.pos), problem.what)
		}
	}
}

type brainNameUse struct {
	pos  token.Pos
	what string
}

func brainNameUses(file *ast.File) []brainNameUse {
	var out []brainNameUse
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if brainNames.MatchString(n.Name) {
				out = append(out, brainNameUse{n.Pos(), "identifier " + n.Name + " names a brain"})
			}
		case *ast.BasicLit:
			if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && brainNames.MatchString(s) {
				out = append(out, brainNameUse{n.Pos(), "string " + n.Value + " names a brain"})
			}
		case *ast.SelectorExpr:
			if n.Sel.Name == "Profile" {
				out = append(out, brainNameUse{n.Pos(), "reads the profile"})
			}
		case *ast.TypeAssertExpr:
			if namedType(n.Type) {
				out = append(out, brainNameUse{n.Pos(), "asserts a named type"})
			}
		}
		return true
	})
	return out
}

// namedType reports whether a type assertion names a declared type (such as an
// adapter's), not a built-in one or a type literal (an interface, []any).
func namedType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	ident, ok := e.(*ast.Ident)
	return ok && types.Universe.Lookup(ident.Name) == nil
}

// The guard sees a brain branch (a test of the test).
func TestBrainHostGuardCatchesABranch(t *testing.T) {
	src := `package p
func f(a *BrainHost) bool {
	if a.opts.Profile == "claude-code" { return true }
	_, ok := a.adapter.(*dshAdapter)
	return ok
}`
	file, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := brainNameUses(file); len(got) != 4 { // Profile, "claude-code", dshAdapter, the assertion
		t.Fatalf("found %d uses, want 4: %+v", len(got), got)
	}
}
