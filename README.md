<!-- last_synced: 2026-06-03 -->

# korylus-tools

> English | [日本語](./README.ja.md)

A repository that gathers the development tools shared across the Korylus projects.

## koryluslint

`koryluslint` is a single binary that bundles the Korylus shared linters.
It dispatches to each linter through a subcommand.

| Subcommand | Role                                                         |
| ---------- | ------------------------------------------------------------ |
| `comment`  | Check misuse of the bilingual `[Ja]` marker in code comments |
| `md`       | Check (and fix) semantic line breaks in Markdown documents   |

### Usage

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

Passing `-base=<ref>` limits the check to lines added since `<ref>` (diff scope).

For `md`, `--all` checks every `.md` file in full instead of only changed lines, and `--write` rewrites the offending lines in place instead of reporting them.

## Installing and running

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
