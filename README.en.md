<!-- last_synced: 2026-09-11 -->

# korylus-tools

> [日本語](./README.md) | English

A repository that gathers the development tools shared across the Korylus projects.

korylus-tools is developed in Japanese. The Japanese documents are the canonical ones, and the English documents are translations provided as a guide.

## koryluslint

`koryluslint` is a single binary that bundles the Korylus shared linters.
It dispatches to each linter through a subcommand.

| Subcommand | Role                                                                                          |
| ---------- | --------------------------------------------------------------------------------------------- |
| `comment`  | Check the Japanese text style (korylus-lang.md §3) of code comments                           |
| `md`       | Check semantic line breaks and Japanese text style (korylus-lang.md §3) in Markdown documents |

### Usage

```sh
koryluslint comment [-base=<ref>] [paths...]
koryluslint md [-base=<ref>] [--all] [--write] [paths...]
```

Passing `-base=<ref>` limits the check to lines added since `<ref>` (diff scope).

For `md`, `--all` checks every `.md` file in full instead of only changed lines, and `--write` fixes semantic line breaks in place.
Japanese text style violations are reported with the updated line numbers, and remaining violations cause exit code 1.

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
