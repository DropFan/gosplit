# gosplit

Split a huge Go source file into several smaller files in the same package,
**byte-for-byte**, following a list of "which declaration goes to which file".

Built for refactoring without risk: it moves code, it does not change it.
Function bodies, doc comments and trailing line comments are copied verbatim by
their syntax-tree positions, so the split program compiles to **exactly the
same thing** as before. Each new file's imports are recomputed from what it
actually uses (like `goimports`), leaving no stray imports behind.

After splitting, gosplit **re-parses every output file** and checks that the
set of top-level declarations is identical to the original (nothing lost,
nothing duplicated). If they don't match, it refuses to write — that is what
makes it trustworthy.

## Install

```bash
cd /path/to/gosplit
go install .
# gosplit is then available from any project directory
```

## Usage

Supported declarations: functions, methods, structs, interfaces, type aliases,
named types, `const` / `var` — they are all top-level declarations, mapped by
name, with no need to distinguish their kind.

### 1. Mapping-file mode (`-map`)

Best for splitting into many files at once. Give a list of which declaration
goes to which file; **anything not listed stays in the original file** (same
file name, and it keeps the package doc comment).

```bash
gosplit -map split.txt store.go
```

Each line of `split.txt` is `<declaration> <target file>`, with `#` comments
allowed:

```
# reads
Get            read.go
List           read.go

# writes
Set            write.go
Delete         write.go

# methods can use the Type.Method qualified form to avoid name clashes
(*Store).Flush flush.go
```

A method (one with a receiver) can be written as the bare name `M` or in a
qualified form `T.M` / `(*T).M`; when methods of the same name belong to
different types, use a qualified form to pin it down.

**Make methods follow their type**: add `-with-methods`, and when you map a
type, all its methods and its `New<T>` / `new<T>` constructors follow it into
the same file by default; if you map a particular method elsewhere, your
explicit mapping wins (handy for spreading a method-heavy type across files).

```bash
# Store's methods and NewStore follow it to store.go; but Set is sent to write.go
gosplit -map split.txt -with-methods store.go
```

### 2. Move mode (`-move` + `-to`)

Best for "move these few out, leave the rest in place".

```bash
gosplit -move Set,Delete -to write.go store.go
```

### 3. Suggest mode (`-suggest`)

When you don't want to write the list by hand, let gosplit cluster each type
with its methods and `New<T>` constructors automatically and emit a mapping
draft (one file per type, named after the type in snake_case):

```bash
# print the draft; free functions and package-level var/const are marked "stays in source"
gosplit -suggest store.go

# save it, tweak as needed (rename files, move some method lines elsewhere), then split
gosplit -suggest store.go > split.txt
gosplit -map split.txt store.go

# or split directly, skipping the draft
gosplit -suggest -apply store.go
```

The draft is expanded declaration by declaration, so a plain `-map` applies it
and lets you adjust method by method. `-suggest` cannot be combined with `-map`
/ `-move` / `-with-methods`.

## Recommended workflow

```bash
# 1. Make sure git is clean (in-place splitting overwrites the source, so you can roll back)
git status

# 2. Dry-run first to see the distribution and the verification result; nothing is written
gosplit -map split.txt -dry-run store.go

# 3. Once it looks right, split for real
gosplit -map split.txt store.go

# 4. Build + test (gosplit does not build for you; tests are your safety net)
go build ./... && go test ./...
```

Example dry-run output:

```
store.go                      8 decl(s)
read.go                       6 decl(s)
write.go                      5 decl(s)
...
verify: 19 declarations in, 19 out — identical
dry-run: nothing written
```

## Common flags

| Flag | Meaning |
|---|---|
| `-map FILE` | mapping file |
| `-move NAMES` | comma-separated declaration names to move out (with `-to`) |
| `-to FILE` | target file for move mode |
| `-with-methods` | a mapped type's methods and `New<T>` constructors follow it (explicit mapping overrides) |
| `-suggest` | cluster by type and print a mapping draft (writes nothing by default) |
| `-apply` | with `-suggest`: skip the draft and split directly |
| `-out DIR` | output directory; defaults to the source file's directory (in-place) |
| `-dry-run` | print the plan and verification result only, write nothing |
| `-no-format` | skip the import cleanup |
| `-v` | show the unmapped declarations that stayed in the source file |

## How it works

1. Parse the source file with `go/parser` (keeping comments).
2. Assign each top-level declaration to its target file; the slice runs from
   its `doc` comment to the end of the declaration's line (including a trailing
   line comment).
3. Each output file = `package` clause + the original import block + its
   assigned declarations; the original file keeps the package doc comment.
4. Re-parse all outputs and verify the declaration set matches the original
   **one-to-one**; on any mismatch it errors out and writes nothing.
5. After writing, tidy each file's imports with `golang.org/x/tools/imports`
   (dropping the unused ones).

## Notes

- **In-place splitting overwrites the source file** (one output keeps the same
  name as the source and holds the "remainder"). Commit first, or use
  `-dry-run`.
- **Build constraints** (`//go:build` tags) are not copied to new files; if the
  source has them, add them to the relevant new files by hand.
- **Grouped declarations** (`type ( ... )` / `var ( ... )` / `const ( ... )`
  written together) move as a whole group, mapped by any name in the group;
  `-suggest` does not split such grouped blocks and leaves them in the remainder.
- gosplit **only moves, never modifies, and does not build for you**; always
  `go build` + `go test` after splitting.
- Mapping one declaration to several files is an error (a declaration can only
  go to one place).
