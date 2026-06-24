package main

import (
	"go/ast"
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
	plan, err := split(src, mapping, splitOpts{})
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
	plan, err := split(src, map[string]string{"DoesNotExist": "x.go"}, splitOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.warnings) != 1 || !strings.Contains(plan.warnings[0], "DoesNotExist") {
		t.Errorf("warnings = %v, want one about DoesNotExist", plan.warnings)
	}
	// A real declaration (e.g. type T) must NOT produce a false warning.
	plan2, _ := split(src, map[string]string{"T": "types.go"}, splitOpts{})
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

const fixtureMethods = `package foo

type Store struct{ data map[string]string }

func NewStore() *Store { return &Store{data: map[string]string{}} }

func (s *Store) Get(k string) string { return s.data[k] }

func (s *Store) Set(k, v string) { s.data[k] = v }

func Helper() string { return "free" }
`

// declNamesIn re-parses output bytes and returns the canonical names present.
func declNamesIn(t *testing.T, content []byte) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", content, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		_, canon := declKeys(d)
		out[canon] = true
	}
	return out
}

func writeMethodsFixture(t *testing.T) (dir, src string) {
	t.Helper()
	dir = t.TempDir()
	src = filepath.Join(dir, "foo.go")
	if err := os.WriteFile(src, []byte(fixtureMethods), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, src
}

func TestSplit_WithMethods_TypeCarriesMethodsAndCtor(t *testing.T) {
	_, src := writeMethodsFixture(t)
	plan, err := split(src, map[string]string{"Store": "store.go"}, splitOpts{withMethods: true})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	store := declNamesIn(t, plan.files["store.go"].content)
	for _, want := range []string{"type Store", "NewStore", "(Store).Get", "(Store).Set"} {
		if !store[want] {
			t.Errorf("store.go missing %q (have %v)", want, store)
		}
	}
	rem := declNamesIn(t, plan.files["foo.go"].content)
	if !rem["Helper"] {
		t.Errorf("Helper should stay in remainder (have %v)", rem)
	}
	if store["Helper"] {
		t.Errorf("free func Helper must not follow a type")
	}
}

func TestSplit_WithMethods_ExplicitOverrides(t *testing.T) {
	_, src := writeMethodsFixture(t)
	plan, err := split(src, map[string]string{
		"Store":     "store.go",
		"Store.Set": "write.go", // a fat type can spread methods across files
	}, splitOpts{withMethods: true})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	store := declNamesIn(t, plan.files["store.go"].content)
	write := declNamesIn(t, plan.files["write.go"].content)
	if !store["(Store).Get"] {
		t.Errorf("Get should follow Store into store.go (have %v)", store)
	}
	if store["(Store).Set"] {
		t.Errorf("Set was explicitly mapped elsewhere, must not be in store.go")
	}
	if !write["(Store).Set"] {
		t.Errorf("Set should be in write.go (have %v)", write)
	}
}

func TestSplit_WithMethods_OffKeepsLegacyBehavior(t *testing.T) {
	_, src := writeMethodsFixture(t)
	plan, err := split(src, map[string]string{"Store": "store.go"}, splitOpts{})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	store := declNamesIn(t, plan.files["store.go"].content)
	if !store["type Store"] {
		t.Errorf("Store should be in store.go (have %v)", store)
	}
	for _, leak := range []string{"NewStore", "(Store).Get", "(Store).Set"} {
		if store[leak] {
			t.Errorf("with-methods off: %q must NOT follow (have %v)", leak, store)
		}
	}
}

func TestSplit_WithMethods_UnmappedTypeDoesNotFollow(t *testing.T) {
	_, src := writeMethodsFixture(t)
	plan, err := split(src, map[string]string{"Helper": "helpers.go"}, splitOpts{withMethods: true})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	rem := declNamesIn(t, plan.files["foo.go"].content)
	for _, want := range []string{"type Store", "NewStore", "(Store).Get", "(Store).Set"} {
		if !rem[want] {
			t.Errorf("unmapped type's members should stay in remainder: missing %q (have %v)", want, rem)
		}
	}
}

func TestRun_WithMethodsEndToEnd(t *testing.T) {
	dir, src := writeMethodsFixture(t)
	mf := filepath.Join(dir, "m.txt")
	if err := os.WriteFile(mf, []byte("Store store.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-map", mf, "-with-methods", src}); err != nil {
		t.Fatalf("run: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "store.go"))
	if err != nil {
		t.Fatalf("store.go not written: %v", err)
	}
	store := declNamesIn(t, content)
	for _, want := range []string{"type Store", "NewStore", "(Store).Get", "(Store).Set"} {
		if !store[want] {
			t.Errorf("store.go missing %q (have %v)", want, store)
		}
	}
}
