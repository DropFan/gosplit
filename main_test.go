package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `// Package foo does things.
package foo

import (
	"fmt"
	"strings"
)

// Greet greets by name.
func Greet(name string) string {
	return fmt.Sprintf("hi %s", name) // trailing comment kept
}

// Shout returns s uppercased.
func Shout(s string) string {
	return strings.ToUpper(s)
}

// T carries a number.
type T struct{ N int }

// M returns the number.
func (t T) M() int { return t.N }

const K = 1
`

// writeFixture writes the fixture into a fresh temp dir and returns its path.
func writeFixture(t *testing.T) (dir, src string) {
	t.Helper()
	dir = t.TempDir()
	src = filepath.Join(dir, "foo.go")
	if err := os.WriteFile(src, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, src
}

func TestSplit_Distribution(t *testing.T) {
	_, src := writeFixture(t)
	mapping := map[string]string{
		"Shout": "shout.go",
		"T.M":   "method.go", // Type.Method form
		"T":     "types.go",
	}
	plan, err := split(src, mapping)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if !plan.verifyOK {
		t.Fatalf("verify failed: %s", plan.verifyMsg)
	}
	if plan.declsIn != plan.declsOut || plan.declsIn != 5 {
		t.Fatalf("decl count: in=%d out=%d, want 5/5", plan.declsIn, plan.declsOut)
	}
	want := map[string]int{
		"foo.go":    2, // Greet + K (remainder, keeps package doc)
		"shout.go":  1,
		"types.go":  1,
		"method.go": 1,
	}
	for name, n := range want {
		fo, ok := plan.files[name]
		if !ok {
			t.Fatalf("missing output file %s", name)
		}
		if fo.declCount != n {
			t.Errorf("%s: declCount=%d, want %d", name, fo.declCount, n)
		}
	}
	// Package doc must live only on the remainder file.
	if !strings.Contains(string(plan.files["foo.go"].content), "// Package foo does things.") {
		t.Errorf("remainder lost package doc")
	}
	if strings.Contains(string(plan.files["shout.go"].content), "// Package foo") {
		t.Errorf("non-remainder file should not carry package doc")
	}
	// Trailing line comment must survive the move.
	if !strings.Contains(string(plan.files["foo.go"].content), "// trailing comment kept") {
		t.Errorf("trailing comment lost")
	}
}

func TestSplit_UnmatchedMappingWarns(t *testing.T) {
	_, src := writeFixture(t)
	plan, err := split(src, map[string]string{"DoesNotExist": "x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.warnings) != 1 || !strings.Contains(plan.warnings[0], "DoesNotExist") {
		t.Errorf("warnings = %v, want one about DoesNotExist", plan.warnings)
	}
	// A real declaration (e.g. type T) must NOT produce a false warning.
	plan2, _ := split(src, map[string]string{"T": "types.go"})
	if len(plan2.warnings) != 0 {
		t.Errorf("unexpected warnings for a matched type decl: %v", plan2.warnings)
	}
}

func TestRun_EndToEndTrimsImports(t *testing.T) {
	dir, src := writeFixture(t)
	mapFile := filepath.Join(dir, "map.txt")
	if err := os.WriteFile(mapFile, []byte("Shout shout.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-map", mapFile, src}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// foo.go keeps Greet (uses fmt, not strings) -> imports trimmed to fmt only.
	fooImports := importsOf(t, src)
	if has(fooImports, "strings") || !has(fooImports, "fmt") {
		t.Errorf("foo.go imports = %v, want [fmt] only", fooImports)
	}
	// shout.go has Shout (uses strings, not fmt) -> imports trimmed to strings only.
	shoutImports := importsOf(t, filepath.Join(dir, "shout.go"))
	if has(shoutImports, "fmt") || !has(shoutImports, "strings") {
		t.Errorf("shout.go imports = %v, want [strings] only", shoutImports)
	}
}

func importsOf(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	return out
}

func has(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestCompareMultisets(t *testing.T) {
	if ok, _ := compareMultisets([]string{"a", "b"}, []string{"b", "a"}); !ok {
		t.Error("same multiset should be equal regardless of order")
	}
	if ok, msg := compareMultisets([]string{"a", "b"}, []string{"a"}); ok || !strings.Contains(msg, "lost: b") {
		t.Errorf("missing element not reported: ok=%v msg=%q", ok, msg)
	}
	if ok, msg := compareMultisets([]string{"a"}, []string{"a", "a"}); ok || !strings.Contains(msg, "duplicated") {
		t.Errorf("duplicate not reported: ok=%v msg=%q", ok, msg)
	}
}
