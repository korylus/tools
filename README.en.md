<!-- last_synced: 2026-10-31 -->

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

## korylusflow

`korylusflow` is a CLI that decides whose turn it is and what to do next, and records the state, when the tasks of a work plan are carried out by interactive sessions, one per role.
The implementer, reviewer (Codex), and reviewer (Claude Code) sessions each ask `korylusflow` whether it is their turn, work as the returned instruction says, and then record completion.
Only `korylusflow` decides what happens next; agents never decide it.
`korylusflow` itself never starts Claude Code or Codex.

### Usage

```sh
korylusflow wait <plan> -role=<role> [-timeout=<duration>]
korylusflow done <plan> -role=<role>
korylusflow status <plan>
korylusflow report <plan>
```

Run every subcommand at the project root.
Specify the plan by its path relative to that root or by the URL of a Wikino page (`https://wikino.app/s/<space>/pages/<page number>`) (see "[Plan names and plans on Wikino pages](#plan-names-and-plans-on-wikino-pages)").
The role is one of `implementer`, `reviewer-codex` (reviewer (Codex)), and `reviewer-claude` (reviewer (Claude Code)).

| Subcommand | Role                                                                                                                                                                                                                                                   |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `wait`     | Wait until it is the role's turn and write the next instruction to standard output; return `action: wait` if the turn does not come within `-timeout`                                                                                                  |
| `done`     | Check and record the completion of the current step and decide the next step; read verification items for the implementation step, or the walkthrough location or the reason for not writing one for the walkthrough step, as JSON from standard input |
| `status`   | Show the plan's progress, the current task, step, role whose turn it is, why it is waiting for a human, and the time spent                                                                                                                             |
| `report`   | Rebuild the latest version of the work report; if steps were recorded after the latest version, build the next version                                                                                                                                 |

The first line of an instruction is `action: <kind>` (`implement`, `work`, `review`, `fix`, `ask`, `walkthrough`, `finish`, or `wait`).
It is followed by the skill and arguments to run and every step to take afterwards, every time.
This lets a session continue from the latest instruction alone, even after its context has been compacted.

`done` exits with the following codes.

| Exit code | Meaning                                                                                                  |
| --------- | -------------------------------------------------------------------------------------------------------- |
| 0         | It recorded the completion                                                                               |
| 1         | Error                                                                                                    |
| 2         | It did not accept the record because a completion condition is not met (fix it per the reason and retry) |

### Plan names and plans on Wikino pages

The plan name is used for the state location, branch names, and the heading of work reports.
Write it before the first `## ` heading of the plan, on a line of the following form.
Use only lowercase letters, digits, and hyphens between them in the value.

```markdown
**計画書名**: feature-name
```

For a plan in a file, omitting this line makes it use the file name (without the extension).
When you write the line, use the same value as the file name.
For a plan on a Wikino page, this line is required.
Changing the plan name in the middle of the loop moves the state location, and the loop can no longer continue from where it left off.

A plan on a Wikino page is read with the `wikino` command of the [Wikino CLI](https://github.com/wikinoapp/wikino/tree/main/cli): its draft if there is one, otherwise the page.
This is because the checks and the implementation decision log written by the implementer exist only in the draft until it is published.
`korylusflow` never writes to Wikino, so the token you log in with via `wikino auth login <space>` only needs read permissions.
Because `korylusflow` reads pages on `https://wikino.app`, add `--host=https://wikino.app` when you log in if you have changed the default host.
When the plan cannot be read, for example because you are not logged in or hit the rate limit, it ends with an error without changing the state.

### Flow of a task

1. Find the repository from the task's prefix and create the branch `<plan name>-<task number>` from the previous task's branch in the same repository (develop for the first task or when the previous task's branch no longer exists locally, or main if there is no develop)
2. The implementer implements the task with `/korylus-do-plan-task` and commits
3. A reviewer reviews with `/korylus-review`; Codex does the first review, and later reviews are done by a different role than the previous one
4. `korylusflow` answers the auto-fixable findings in the review document with the recommended options, and the reviewer fixes the code and commits
5. On Request Changes go back to 3 (up to 3 reviews); on Comment or Approve move on to the next task

For tasks done by a human, findings that need a human decision, and reaching the review limit, it tells the current session to ask the human.

When the plan reaches the end, before building the work report, it tells the implementer to write a walkthrough document (`action: walkthrough`).
The instruction lists the last task branch per repository with its head commit, and the location of the walkthrough written for the previous version.
The implementer writes the walkthrough with `/korylus-write-walkthrough` and passes its location (`{"location":"<URL or path>"}`) or the reason for not writing one (`{"skipped":"<reason>"}`) to `done`.
The location must be a URL starting with `https://` or an existing file given as a path relative to the project root.
`korylusflow` only records the location and the reason; it does not read the walkthrough itself.

Task prefixes are read as follows.

| Prefix                                           | Working directory                                            |
| ------------------------------------------------ | ------------------------------------------------------------ |
| One starting with `/`, such as `[/workspace/go]` | That path                                                    |
| `[Go]`, `[Rails]`, or no prefix                  | Read as `/workspace/go`, `/workspace/rails`, or `/workspace` |
| `[implementer]`                                  | None (a task the implementer does without creating a branch) |
| Anything else (the name of the person)           | None (a task done by a human)                                |

Use `[implementer]` tasks for work that the implementer can do without creating a branch or a PR, such as creating Wikino drafts or editing a repository that is committed to directly without branches.
It tells the implementer to do the work (`action: work`) without preparing a branch or adding a review.
The implementer does not commit, gets the human's approval before irreversible operations, and shows the results to the human.
The task is complete once the human checks the results and checks the task in the plan.

When a task has a `**担当**:` (assignee) line under it, the prefix is read only as the path of the repository to create the branch in, and the assignee is read from the line's value.

```markdown
- [ ] **1-1**: [/workspace/go] マイグレーションを作る
- [ ] **1-2**: ヘルプページの下書きを作る
  - **担当**: implementer
- [ ] **1-3**: ヘルプページの下書きを公開する
  - **担当**: shimbaco
```

| Assignee line                   | Prefix                | Task                                                                |
| ------------------------------- | --------------------- | ------------------------------------------------------------------- |
| None                            | As in the table above | As in the table above                                               |
| `implementer`                   | A path                | A task the implementer does by creating a branch in that repository |
| `implementer`                   | None                  | A task the implementer does without creating a branch               |
| Anything else (a person's name) | A path or none        | A task done by a human                                              |
| Present                         | Not a path            | Ask the human (the prefix and the assignee line disagree)           |

A task with more than one assignee line, or with an empty value, is an error that stops the plan from being read.
`korylusflow status` and the work report show the assignee of tasks that have the line (for example, `1-2 ヘルプページの下書きを作る (担当: implementer)`).

### Work report

When the plan reaches the end, it builds a Markdown work report `report-<version>.md` from the state, timing records, the plan, and git.
It does not have an LLM write any text; it lists only the facts in the records.

- Overview (plan, period, the walkthrough location or the reason for not writing one, and the PR to merge per repository)
- Changes per task (a table with one row per task: branch, changed line counts, expected line counts, and review verdicts)
- Tasks the implementer did without creating a branch
- Deferred findings
- Protected areas (what was declared and the command to see the diff)
- The verification checklist and a field for overall comments

Time spent and time spent waiting for a human are not in the work report; `korylusflow status` shows them.
When `report` rebuilds a version made before the walkthrough step existed, it shows the walkthrough as not recorded.

`korylusflow` does not push, create PRs, or merge.
After a human checks the work report, they create one PR per repository from the last task's branch to the first task's base branch and merge it with a merge commit.
When the branches do not form a single line, for example because the previous task's branch no longer existed and the branch was recreated from the first base, the report shows a PR for each connected line.

A human checks OK or NG for each checklist item and writes comments.
If tasks are added to the plan and the loop runs again, it builds the next version at the end of the plan.
The next version lists the items that were NG in the previous version and new items first, and lists items that were OK in an earlier version only by title as verified.
Rebuilding the same version with `report` keeps the results already written in it.
If what was written hides the report's section boundaries, for example with an unclosed code block, it does not rebuild the report and shows what to fix instead.

### Where state and work reports are stored

State and work reports are kept in `docs/private/plans/files/<plan name>/` so that they remain as history together with the plan.

| File                  | Content                                                                 |
| --------------------- | ----------------------------------------------------------------------- |
| `flow.json`           | The current task, step, and role whose turn it is, and records per task |
| `flow.lock`           | An empty file for the file lock                                         |
| `timeline.jsonl`      | The start and end times of each step                                    |
| `report-<version>.md` | Work reports                                                            |

### Installing

Build it with `make install-korylusflow`, which places it at `~/.local/bin/korylusflow` (change the location with `BINDIR=<directory>`).
Use the built binary instead of running it from the working tree with `go run`, so that the running code does not change when a branch is switched in the middle of the loop.

## Installing and running

Building requires Go, a C compiler (GCC or Clang), and `CGO_ENABLED=1`.
Shell and TypeScript parsing use the [Go bindings for Tree-sitter](https://github.com/tree-sitter/go-tree-sitter).

Each project pins a version of `koryluslint` to fetch it.

- Go projects: pin `github.com/korylus/tools/cmd/koryluslint` with the `tool` directive in `go.mod` and run it with `go tool koryluslint ...`.
- Non-Go projects: pin it with mise's `go:` backend.

## Development

```sh
make build       # build koryluslint (to bin/koryluslint)
make install-korylusflow  # build korylusflow (to ~/.local/bin/korylusflow)
make test        # run the tests
make vet         # run go vet
make lint        # run golangci-lint
make fmt         # format with gofmt + goimports + Oxfmt
make fmt-check   # check formatting
```
