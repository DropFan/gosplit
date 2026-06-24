// Command gosplit splits a large Go file's top-level declarations into
// multiple files within the same package, byte-for-byte and comment-preserving.
//
// It is a pure-move refactoring aid: declaration bodies, doc comments and
// trailing line comments are copied verbatim via the go/ast positions, so the
// resulting files compile to exactly the same program. Imports are recomputed
// per output file (goimports-style) so each file only imports what it uses.
//
// After producing the output in memory, gosplit re-parses every file and
// verifies that the multiset of top-level declarations equals the input's;
// it refuses to write anything if they differ. This guards against accidental
// loss or duplication when splitting.
//
// Usage:
//
//	# Map mode: assign declarations to files via a mapping file.
//	gosplit -map mapping.txt source.go
//
//	# Move mode: move a few declarations out; the rest stay in source.go.
//	gosplit -move Set,Delete -to write.go source.go
//
//	# Suggest mode: print a per-type mapping draft (-apply to split directly).
//	gosplit -suggest source.go
//
// Mapping file format (one per line, "#" comments allowed):
//
//	Get            read.go
//	(*Store).Flush flush.go          # methods: M, T.M, (T).M or (*T).M
//	Set            write.go
//
// With -with-methods, mapping a type also pulls its methods and New<T>/new<T>
// constructors into the same file unless they are explicitly mapped elsewhere.
//
// Declarations not listed anywhere stay in the "remainder" file, which keeps
// the same name as the source and retains the package doc comment.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/imports"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gosplit: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gosplit", flag.ContinueOnError)
	mapFile := fs.String("map", "", "mapping file: each line \"<declName> <targetFile>\"")
	move := fs.String("move", "", "comma-separated declaration names to move (use with -to)")
	to := fs.String("to", "", "target file for -move mode")
	outDir := fs.String("out", "", "output directory (default: directory of the source file)")
	dryRun := fs.Bool("dry-run", false, "print the plan and verification result, write nothing")
	noFormat := fs.Bool("no-format", false, "do not run goimports-style import cleanup on outputs")
	withMethods := fs.Bool("with-methods", false, "a mapped type's methods and New<T> constructors follow it")
	suggest := fs.Bool("suggest", false, "print a per-type mapping draft for the source file")
	apply := fs.Bool("apply", false, "with -suggest: skip the draft and split directly")
	verbose := fs.Bool("v", false, "verbose output")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gosplit [-map FILE | -move NAMES -to FILE | -suggest [-apply]] [flags] source.go")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("exactly one source file is required")
	}
	srcPath := fs.Arg(0)

	switch {
	case *suggest && (*mapFile != "" || *move != ""):
		return fmt.Errorf("-suggest cannot be combined with -map or -move")
	case *suggest && *withMethods:
		return fmt.Errorf("-with-methods cannot be combined with -suggest")
	case *apply && !*suggest:
		return fmt.Errorf("-apply requires -suggest")
	}

	var mapping map[string]string
	opts := splitOpts{withMethods: *withMethods}
	if *suggest {
		s, err := suggestMapping(srcPath)
		if err != nil {
			return err
		}
		if !*apply {
			fmt.Print(renderSuggestion(s, filepath.Base(srcPath)))
			return nil
		}
		mapping = s.mapping()
		opts = splitOpts{} // draft is already expanded; no need to follow methods
	} else {
		m, err := loadMapping(*mapFile, *move, *to)
		if err != nil {
			return err
		}
		mapping = m
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Dir(srcPath)
	}

	plan, err := split(srcPath, mapping, opts)
	if err != nil {
		return err
	}

	// Report the plan.
	names := make([]string, 0, len(plan.files))
	for name := range plan.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-28s %d decl(s)\n", name, plan.files[name].declCount)
	}
	if len(plan.unmapped) > 0 && *verbose {
		fmt.Printf("kept in %s (unmapped): %s\n", filepath.Base(srcPath), strings.Join(plan.unmapped, ", "))
	}
	for _, w := range plan.warnings {
		fmt.Println("warning: " + w)
	}

	fmt.Printf("verify: %d declarations in, %d out — %s\n", plan.declsIn, plan.declsOut, plan.verifyMsg)
	if !plan.verifyOK {
		return fmt.Errorf("declaration set changed; refusing to write")
	}

	if *dryRun {
		fmt.Println("dry-run: nothing written")
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		outPath := filepath.Join(dir, name)
		content := plan.files[name].content
		if !*noFormat {
			formatted, ferr := imports.Process(outPath, content, nil)
			if ferr != nil {
				return fmt.Errorf("format %s: %w", name, ferr)
			}
			content = formatted
		}
		if err := os.WriteFile(outPath, content, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d file(s) to %s\n", len(names), dir)
	return nil
}

// loadMapping builds the declName -> targetFile map from either a mapping file
// or the -move/-to convenience flags.
func loadMapping(mapFile, move, to string) (map[string]string, error) {
	m := map[string]string{}
	switch {
	case mapFile != "" && move != "":
		return nil, fmt.Errorf("use either -map or -move, not both")
	case mapFile != "":
		data, err := os.ReadFile(mapFile)
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if idx := strings.Index(line, "#"); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) != 2 {
				return nil, fmt.Errorf("%s:%d: expected \"<declName> <targetFile>\"", mapFile, i+1)
			}
			m[parts[0]] = parts[1]
		}
	case move != "":
		if to == "" {
			return nil, fmt.Errorf("-move requires -to")
		}
		for _, n := range strings.Split(move, ",") {
			if n = strings.TrimSpace(n); n != "" {
				m[n] = to
			}
		}
	default:
		return nil, fmt.Errorf("either -map or -move is required")
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("mapping is empty")
	}
	return m, nil
}

type fileOut struct {
	content   []byte
	declCount int
}

// splitOpts controls optional split behavior.
type splitOpts struct {
	withMethods bool // a mapped type's methods and New<T>/new<T> constructors follow it
}

type splitPlan struct {
	files     map[string]*fileOut
	unmapped  []string
	warnings  []string
	declsIn   int
	declsOut  int
	verifyOK  bool
	verifyMsg string
}

// split parses the source and produces the per-file output bytes in memory,
// then verifies that the output declarations match the input declarations.
func split(srcPath string, mapping map[string]string, opts splitOpts) (*splitPlan, error) {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	remainder := filepath.Base(srcPath)

	extendToEOL := func(off int) int {
		for off < len(src) && src[off] != '\n' {
			off++
		}
		if off < len(src) {
			off++ // include the newline (captures trailing line comments)
		}
		return off
	}

	// Capture all import blocks; inject into every output, then imports.Process trims.
	var importBlocks []string
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			s := fset.Position(gd.Pos()).Offset
			e := extendToEOL(fset.Position(gd.End()).Offset)
			importBlocks = append(importBlocks, string(src[s:e]))
		}
	}
	importBlock := strings.Join(importBlocks, "\n")

	type chunk struct {
		start, end int
		canonical  string
	}
	buckets := map[string][]chunk{}
	order := []string{}
	seen := map[string]bool{}
	var unmapped []string
	var warnings []string
	inNames := []string{}
	matchedKeys := map[string]bool{}

	addBucket := func(target string, c chunk) {
		if !seen[target] {
			seen[target] = true
			order = append(order, target)
		}
		buckets[target] = append(buckets[target], c)
	}

	// With -with-methods, a mapped type's methods and New<T>/new<T> constructors
	// follow it unless explicitly mapped. Precompute type name -> target file.
	typeTargets := map[string]string{}
	if opts.withMethods {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					if t, ok := mapping[ts.Name.Name]; ok {
						typeTargets[ts.Name.Name] = t
					}
				}
			}
		}
	}

	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		keys, canonical := declKeys(d)
		inNames = append(inNames, canonical)

		target, matchedKey, mapped := lookup(mapping, keys)
		switch {
		case mapped:
			matchedKeys[matchedKey] = true
		default:
			ft := ""
			if opts.withMethods {
				ft = followTarget(d, typeTargets)
			}
			if ft != "" {
				target = ft
			} else {
				target = remainder
				unmapped = append(unmapped, canonical)
			}
		}

		startPos := d.Pos()
		switch v := d.(type) {
		case *ast.GenDecl:
			if v.Doc != nil {
				startPos = v.Doc.Pos()
			}
		case *ast.FuncDecl:
			if v.Doc != nil {
				startPos = v.Doc.Pos()
			}
		}
		c := chunk{
			start:     fset.Position(startPos).Offset,
			end:       extendToEOL(fset.Position(d.End()).Offset),
			canonical: canonical,
		}
		addBucket(target, c)
	}

	// Warn about mapping entries that matched nothing.
	mapKeys := make([]string, 0, len(mapping))
	for k := range mapping {
		mapKeys = append(mapKeys, k)
	}
	sort.Strings(mapKeys)
	for _, k := range mapKeys {
		if !matchedKeys[k] {
			warnings = append(warnings, fmt.Sprintf("mapping %q matched no declaration", k))
		}
	}

	// Package doc only goes on the remainder file.
	pkgDoc := ""
	if f.Doc != nil {
		ds := fset.Position(f.Doc.Pos()).Offset
		de := fset.Position(f.Doc.End()).Offset
		pkgDoc = string(src[ds:de]) + "\n"
	}
	pkgName := f.Name.Name

	files := map[string]*fileOut{}
	for _, target := range order {
		var body []byte
		if target == remainder {
			body = append(body, []byte(pkgDoc)...)
		}
		body = append(body, []byte("package "+pkgName+"\n\n")...)
		if importBlock != "" {
			body = append(body, []byte(importBlock)...)
			body = append(body, '\n')
		}
		chunks := buckets[target]
		for i, c := range chunks {
			body = append(body, src[c.start:c.end]...)
			if i != len(chunks)-1 {
				body = append(body, '\n')
			}
		}
		files[target] = &fileOut{content: body, declCount: len(chunks)}
	}

	// Verify: re-parse outputs and compare the declaration multiset to the input.
	outNames, err := collectOutputDecls(files)
	if err != nil {
		return nil, fmt.Errorf("re-parsing generated output failed: %w", err)
	}
	verifyOK, verifyMsg := compareMultisets(inNames, outNames)

	return &splitPlan{
		files:     files,
		unmapped:  unmapped,
		warnings:  warnings,
		declsIn:   len(inNames),
		declsOut:  len(outNames),
		verifyOK:  verifyOK,
		verifyMsg: verifyMsg,
	}, nil
}

// declKeys returns the lookup keys for a declaration (most specific first) and
// a canonical name used for identity comparison.
//
//   - func F            -> ["F"], "F"
//   - func (T) M        -> ["T.M", "M"], "(T).M"
//   - type/var/const    -> [each name], joined names
func declKeys(d ast.Decl) (keys []string, canonical string) {
	switch v := d.(type) {
	case *ast.FuncDecl:
		if v.Recv != nil && len(v.Recv.List) > 0 {
			recv := recvTypeName(v.Recv.List[0].Type)
			m := v.Name.Name
			// Keys ordered most-specific first; canonical stays parenthesized.
			keys := []string{recv + "." + m, "(" + recv + ")." + m, "(*" + recv + ")." + m, m}
			return keys, "(" + recv + ")." + m
		}
		return []string{v.Name.Name}, v.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, spec := range v.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			}
		}
		return names, v.Tok.String() + " " + strings.Join(names, ",")
	}
	return nil, ""
}

// lookup tries each key against the mapping, returning the first hit and the
// key that matched.
func lookup(mapping map[string]string, keys []string) (target, matchedKey string, ok bool) {
	for _, k := range keys {
		if t, found := mapping[k]; found {
			return t, k, true
		}
	}
	return "", "", false
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // generic receiver: T[P]
		return recvTypeName(t.X)
	case *ast.IndexListExpr: // generic receiver: T[P, Q]
		return recvTypeName(t.X)
	}
	return ""
}

// followTarget returns the file a method or New<T>/new<T> constructor should
// follow to, given the precomputed type->target map; "" if it follows nothing.
func followTarget(d ast.Decl, typeTargets map[string]string) string {
	fn, ok := d.(*ast.FuncDecl)
	if !ok {
		return ""
	}
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return typeTargets[recvTypeName(fn.Recv.List[0].Type)]
	}
	for _, prefix := range []string{"New", "new"} {
		if t, ok := strings.CutPrefix(fn.Name.Name, prefix); ok {
			if target, found := typeTargets[t]; found {
				return target
			}
		}
	}
	return ""
}

// collectOutputDecls re-parses each generated file and returns the canonical
// names of all top-level declarations.
func collectOutputDecls(files map[string]*fileOut) ([]string, error) {
	var names []string
	for name, fo := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, fo.content, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			_, canonical := declKeys(d)
			names = append(names, canonical)
		}
	}
	return names, nil
}

// compareMultisets reports whether a and b contain the same elements with the
// same multiplicities, and a human-readable message describing any difference.
func compareMultisets(a, b []string) (bool, string) {
	count := func(xs []string) map[string]int {
		m := map[string]int{}
		for _, x := range xs {
			m[x]++
		}
		return m
	}
	ca, cb := count(a), count(b)
	var missing, extra []string
	for k, n := range ca {
		if cb[k] < n {
			missing = append(missing, k)
		}
	}
	for k, n := range cb {
		if ca[k] < n {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return true, "identical"
	}
	sort.Strings(missing)
	sort.Strings(extra)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "lost: "+strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		parts = append(parts, "duplicated/new: "+strings.Join(extra, ", "))
	}
	return false, strings.Join(parts, "; ")
}

// suggestGroup is one suggested output file: a type plus its methods and
// New<T>/new<T> constructors, listed as mapping keys.
type suggestGroup struct {
	typeName string
	file     string
	members  []string // mapping keys: type name, T.M methods, constructor names
}

// suggestion is a per-type split proposal for a source file.
type suggestion struct {
	groups    []suggestGroup
	remainder []string // canonical names of declarations that stay in the source
}

// mapping flattens the suggestion into a declName -> targetFile map usable by split.
func (s *suggestion) mapping() map[string]string {
	m := map[string]string{}
	for _, g := range s.groups {
		for _, k := range g.members {
			m[k] = g.file
		}
	}
	return m
}

// suggestMapping analyzes a source file and clusters each single-spec type with
// its methods and New<T>/new<T> constructors into its own output file. Grouped
// type blocks, free functions and package-level var/const stay in the remainder.
func suggestMapping(srcPath string) (*suggestion, error) {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	groups := map[string]*suggestGroup{}
	var typeOrder []string
	inGroup := map[ast.Decl]bool{}

	// Each single-spec `type T ...` becomes its own cluster.
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE || len(gd.Specs) != 1 {
			continue
		}
		name := gd.Specs[0].(*ast.TypeSpec).Name.Name
		groups[name] = &suggestGroup{typeName: name, file: toSnake(name) + ".go", members: []string{name}}
		typeOrder = append(typeOrder, name)
		inGroup[d] = true
	}

	// Attach methods and New<T>/new<T> constructors to their type's cluster.
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			recv := recvTypeName(fn.Recv.List[0].Type)
			if g, ok := groups[recv]; ok {
				g.members = append(g.members, recv+"."+fn.Name.Name)
				inGroup[d] = true
			}
			continue
		}
		for _, prefix := range []string{"New", "new"} {
			if t, ok := strings.CutPrefix(fn.Name.Name, prefix); ok {
				if g, ok := groups[t]; ok {
					g.members = append(g.members, fn.Name.Name)
					inGroup[d] = true
					break
				}
			}
		}
	}

	var remainder []string
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			continue
		}
		if !inGroup[d] {
			_, canonical := declKeys(d)
			remainder = append(remainder, canonical)
		}
	}

	s := &suggestion{remainder: remainder}
	for _, name := range typeOrder {
		s.groups = append(s.groups, *groups[name])
	}
	return s, nil
}

// renderSuggestion formats a suggestion as a mapping draft: each member on its
// own line grouped by type, with the remainder listed as comments. The output
// is itself a valid mapping file.
func renderSuggestion(s *suggestion, srcBase string) string {
	width := 0
	for _, g := range s.groups {
		for _, k := range g.members {
			if len(k) > width {
				width = len(k)
			}
		}
	}
	var b strings.Builder
	for _, g := range s.groups {
		fmt.Fprintf(&b, "# %s -> %s\n", g.typeName, g.file)
		for _, k := range g.members {
			fmt.Fprintf(&b, "%-*s  %s\n", width, k, g.file)
		}
		b.WriteByte('\n')
	}
	if len(s.remainder) > 0 {
		fmt.Fprintf(&b, "# Unassigned (stays in %s):\n", srcBase)
		for _, name := range s.remainder {
			fmt.Fprintf(&b, "#   %s\n", name)
		}
	}
	return b.String()
}

// toSnake converts a CamelCase identifier to snake_case, treating runs of
// upper-case letters as acronyms (e.g. HTTPServer -> http_server, ID -> id).
func toSnake(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
