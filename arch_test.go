package bmaexecutor

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The dependency rules of the conformance packages. They pull package testing
// into whatever imports them, so they are for tests: no non-test code of the
// module may import them. They test through the SDK's public interface only, so
// they may import the root package and each other, nothing of this module
// besides, in particular no internal/ package (a third party cannot).
const (
	modulePath      = "github.com/beeos-ai/bma-executor-sdk"
	conformancePath = modulePath + "/conformance"
)

func importsOfNonTestFiles(t *testing.T) map[string][]string { // file -> import paths
	t.Helper()
	out := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "typescript", "python", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(path)] = append(out[filepath.ToSlash(path)], p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func isConformance(path string) bool {
	return path == conformancePath || strings.HasPrefix(path, conformancePath+"/")
}

func TestProductionCodeDoesNotImportTheConformancePackages(t *testing.T) {
	imports := importsOfNonTestFiles(t)
	if len(imports) < 50 {
		t.Fatalf("saw only %d non-test files: the walk is broken", len(imports))
	}
	checked := 0
	for file, paths := range imports {
		if isConformance(modulePath + "/" + filepath.ToSlash(filepath.Dir(file))) {
			continue
		}
		for _, p := range paths {
			if isConformance(p) {
				t.Errorf("%s imports %s: production code must not depend on the conformance packages (they import package testing)", file, p)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no production file was checked")
	}
}

func TestConformancePackagesUseOnlyThePublicInterface(t *testing.T) {
	seen := 0
	for file, paths := range importsOfNonTestFiles(t) {
		if !strings.HasPrefix(file, "conformance/") {
			continue
		}
		seen++
		for _, p := range paths {
			if !strings.HasPrefix(p, modulePath) {
				continue // standard library (the module has no third-party dependency in these packages)
			}
			if p != modulePath && !isConformance(p) {
				t.Errorf("%s imports %s: the conformance packages may use the SDK's public interface only", file, p)
			}
		}
		if strings.HasPrefix(file, "conformance/gatewaystub/") && slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, modulePath) }) {
			t.Errorf("%s: the gateway double must not share an implementation with the routes it tests (standard library only)", file)
		}
	}
	if seen == 0 {
		t.Fatal("no conformance file was found")
	}
}
