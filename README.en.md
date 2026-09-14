<!-- last_synced: 2026-09-14 -->

# korylus-tools

> [日本語](./README.md) | English

A repository that gathers the development tools shared across the Korylus projects.

korylus-tools is developed in Japanese. The Japanese documents are the canonical ones, and the English documents are translations provided as a guide.

## koryluslint

`koryluslint` is a single binary that bundles the Korylus shared linters.
It dispatches to each linter through a subcommand.

| Subcommand | Role                                                                                          |
| ---------- | --------------------------------------------------------------------------------------------- |
| `comment`  | Check the Japanese text style (korylus-lang.md §3) of source code                             |
| `md`       | Check semantic line breaks and Japanese text style (korylus-lang.md §3) in Markdown documents |

### Usage

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

Passing `-base=<ref>` limits the check to lines added since `<ref>` (diff scope).

What `comment` looks at depends on the format.

| Format                    | Checked part                                                                               |
| ------------------------- | ------------------------------------------------------------------------------------------ |
| `.go`                     | Comments                                                                                   |
| `.templ`                  | Leading `//` comments                                                                      |
| `.sh`                     | Comments, literal arguments and other strings, and heredoc text extracted by a Bash parser |
| `.ts`                     | Comments and string text extracted by a TypeScript parser, including template literals     |
| `.sql` / `.css` / `.toml` | Lines containing Japanese                                                                  |

Japanese text outside comments is checked because §3 also covers i18n translations and messages printed by shell scripts.
Shell command names, options and expansions, and operators in TypeScript interpolation expressions, are not part of the checked text.
Separators between unquoted shell arguments are checked as spaces in the text that the command prints.
In `.sh` and `.ts`, Markdown inline code in comments is excluded, while backticks in strings are treated as text.
In `.sql`, `.css` and `.toml`, the whole line is treated as Markdown, so ranges enclosed in backticks are excluded even inside strings.
Shell and TypeScript files that fail syntax parsing are reported to standard error and skipped.

Lines without Japanese are not checked.
Generated files are excluded when their names end in `_templ.go` or their first 20 lines contain the standard `// Code generated ... DO NOT EDIT.` header.
Other generated files, and Japanese text within SQL, CSS, and TOML code, remain subject to line-based checks.

String literals in `.go` are not checked.
Test literals compared against rendered output intentionally keep forms that violate §3 so that they match the template output.

For `md`, `--all` checks every `.md` file in full instead of only changed lines, and `--write` fixes semantic line breaks in place.
Japanese text style violations are reported with the updated line numbers, and remaining violations cause exit code 1.

## Installing and running

Building requires Go, a C compiler (GCC or Clang), and `CGO_ENABLED=1`.
Shell and TypeScript parsing use the [Go bindings for Tree-sitter](https://github.com/tree-sitter/go-tree-sitter).

Each project pins a version of `koryluslint` to fetch it.

- Go projects: pin `github.com/korylus/tools/cmd/koryluslint` with the `tool` directive in `go.mod` and run it with `go tool koryluslint ...`.
- Non-Go projects: pin it with mise's `go:` backend.

## Development

```sh
make build       # build koryluslint (to bin/koryluslint)
make test        # run the tests
make vet         # run go vet
make lint        # run golangci-lint
make fmt         # format with gofmt + goimports + Oxfmt
make fmt-check   # check formatting
```
