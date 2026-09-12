package deploy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A test that compiles but is never selected by the CI matrix is not evidence.
// Keep every regular product scenario selected, and reject stale selectors.
func TestProductMatrixIncludesEveryRegularScenario(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/product-e2e.yml")
	if err != nil {
		t.Fatal(err)
	}
	selectorLines := regexp.MustCompile(`(?m)^\s+test:\s+([^\r\n]+)`).FindAllStringSubmatch(string(workflow), -1)
	if len(selectorLines) == 0 {
		t.Fatal("product matrix has no scenario selectors")
	}
	var selectors []*regexp.Regexp
	for _, line := range selectorLines {
		pattern, err := regexp.Compile(strings.TrimSpace(line[1]))
		if err != nil {
			t.Fatal(err)
		}
		selectors = append(selectors, pattern)
	}
	files, err := filepath.Glob("../e2e/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	var tests []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "//go:build e2e && extended") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, raw, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			if fn.Name.Name == "TestProductExtended" {
				continue
			} // explicitly manual soak, separately gated
			tests = append(tests, fn.Name.Name)
		}
	}
	if len(tests) == 0 {
		t.Fatal("no regular product scenarios found")
	}
	for _, name := range tests {
		matched := false
		for _, selector := range selectors {
			matched = matched || selector.MatchString(name)
		}
		if !matched {
			t.Errorf("product scenario %s is not selected in CI", name)
		}
	}
	for _, selector := range selectors {
		matched := false
		for _, name := range tests {
			matched = matched || selector.MatchString(name)
		}
		if !matched {
			t.Errorf("matrix selector %s selects no test", selector)
		}
	}
}
