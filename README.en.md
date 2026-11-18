<!-- last_synced: 2026-11-17 -->

# korylus-tools

> [日本語](./README.md) | English

A repository that gathers the development tools shared across the Korylus projects.
Go tools live in the Go module (`cmd/`, `internal/`), and Rust tools live in the Cargo workspace (`crates/`).

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
| `.rs`                     | Comments and doc comments extracted by a Rust parser                                       |
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
String literals in `.rs` are not checked either.
Error messages contain command names and options separated by spaces, and test data intentionally keeps forms that violate §3.
Code examples in Rust doc comments are excluded using rustdoc's Markdown rules, including fenced and indented code blocks.

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
Specify the plan by the URL of a Wikino page (`https://wikino.app/s/<space>/pages/<page number>`).
A path or a page number alone is not accepted (see "[Plan names and plans on Wikino pages](#plan-names-and-plans-on-wikino-pages)").
The role is one of `implementer`, `reviewer-codex` (reviewer (Codex)), and `reviewer-claude` (reviewer (Claude Code)).

| Subcommand | Role                                                                                                                                                                                                                                                         |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `wait`     | Wait until it is the role's turn and write the next instruction to standard output; return `action: wait` if the turn does not come within `-timeout`                                                                                                        |
| `done`     | Check and record the completion of the current step and decide the next step; read verification items for the implementation step, or the work report URL and the reason for not writing a walkthrough for the work report step, as JSON from standard input |
| `status`   | Show the plan's progress, the current task, step, role whose turn it is, why it is waiting for a human, and the time spent                                                                                                                                   |
| `report`   | Write facts gathered from the state, the plan, and git, such as the PRs to merge, to standard output; it does not change the state                                                                                                                           |

The first line of an instruction is `action: <kind>` (`implement`, `work`, `review`, `fix`, `ask`, `report`, `finish`, or `wait`).
It is followed by the skill and arguments to run and every step to take afterwards, every time.
This lets a session continue from the latest instruction alone, even after its context has been compacted.

`done` exits with the following codes.

| Exit code | Meaning                                                                                                  |
| --------- | -------------------------------------------------------------------------------------------------------- |
| 0         | It recorded the completion                                                                               |
| 1         | Error                                                                                                    |
| 2         | It did not accept the record because a completion condition is not met (fix it per the reason and retry) |

### Plan names and plans on Wikino pages

The plan name is used for the state location, branch names, and the heading of `korylusflow report`.
Write it before the first `## ` heading of the plan, on a line of the following form.
Use only lowercase letters, digits, and hyphens between them in the value.

```markdown
**計画書名**: feature-name
```

This line is required.
Changing the plan name in the middle of the loop moves the state location, and the loop can no longer continue from where it left off.

A plan on a Wikino page is read with the `wikino` command of the [Wikino CLI](https://github.com/wikinoapp/wikino/tree/main/cli): its draft if there is one, otherwise the page.
This is because the checks and the implementation decision log written by the implementer exist only in the draft until it is published.
`korylusflow` never writes to Wikino, so the token you log in with via `wikino auth login <space>` only needs read permissions.
Because `korylusflow` reads pages on `https://wikino.app`, add `--host=https://wikino.app` when you log in if you have changed the default host.
When the plan cannot be read, for example because you are not logged in or hit the rate limit, it ends with an error without changing the state.

### Base branches

To stack a plan on top of another plan's branch that has not been merged yet, write the branch to create each repository's first task branch from, before the first `## ` heading of the plan.
The value is a list of `<directory path>=<branch name>` separated by `, `.
As with a task's prefix, the path is an absolute path to a directory in the repository, and it sets the base of the repository that contains the directory.

```markdown
**ベースブランチ**: /workspace=prev-feature-1-3, /korylus-tools=prev-feature-1-2
```

The branch to create a task branch from is decided in the following order.
The current branch of the working tree is not used.

1. The previous task's branch in the same repository, if it exists locally
2. The branch written for that repository on the base branch line, if any
3. Otherwise develop, or main if there is no develop

When a path on the line cannot be resolved to a repository, different branches are written for the same repository, or a branch on the line does not exist locally, it does not proceed automatically and tells the implementer to ask the human.
A malformed line (no `=`, a path not starting with `/`, an empty branch name, or a duplicated path or line) is an error that makes the plan unreadable.
The base of a task whose branch has already been prepared does not change even if the line is edited later.

### Flow of a task

1. Find the repository from the task's prefix and create the branch `<plan name>-<task number>`; the branch to create it from is decided in the order described in "[Base branches](#base-branches)"
2. The implementer implements the task with `/korylus-do-plan-task` and commits
3. A reviewer reviews with `/korylus-review`; Codex does the first review, and later reviews are done by a different role than the previous one
4. `korylusflow` answers the auto-fixable findings in the review document with the recommended options, and the reviewer fixes the code and commits
5. On Request Changes go back to 3 (up to 3 reviews); on Comment or Approve move on to the next task

For tasks done by a human, findings that need a human decision, and reaching the review limit, it tells the current session to ask the human.

When the plan reaches the end, it tells the implementer to write a work report on Wikino (`action: report`).
The instruction lists the last task branch per repository with its head commit, and the URL of the work report written in the previous round.
The implementer writes the work report with `/korylus-write-report` and passes its URL (`{"location":"<URL>"}`) to `done`.
When the implementer did not write a walkthrough, they add the reason (`{"location":"<URL>","walkthrough_skipped":"<reason>"}`).
The URL must start with `https://`.
`korylusflow` only records the URL and the reason; it neither reads the work report nor writes to Wikino.

When the implementer wrote a walkthrough, the work report is then reviewed.

1. A reviewer reviews it with `/korylus-review-doc <work report URL> prefix=<plan name>-report`; Codex does the first review, and later reviews are done by a different role than the previous one
2. `korylusflow` answers the auto-fixable findings in the review document with the recommended options, and the reviewer fixes the work report draft; the draft is not committed, so completing the fix does not check the branch or commits
3. On Request Changes go back to 1 (up to 3 reviews); on Comment or Approve the work report review ends

Findings that need a human decision and the review limit are handled the same way as in task reviews.
Review documents use the same prefix (`<plan name>-report`) with sequential numbers across rounds, so the next round verifies fixes from the previous round's reviews.
When the implementer did not write a walkthrough, there is nothing to check against the code, so the review is skipped.

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
`korylusflow status` and `korylusflow report` show the assignee of tasks that have the line (for example, `1-2 ヘルプページの下書きを作る (担当: implementer)`).

### Work reports and facts

A work report is a document the implementer writes on a Wikino page, with a walkthrough and sections for feedback.
A human reads the work report, collects feedback, adds feedback tasks to the plan, and runs the loop again.
Each time the plan reaches the end, the implementer updates the same work report, and `korylusflow` records its URL for that round.

`korylusflow report` writes facts gathered from the state, timing records, the plan, and git to standard output.
It does not mix in explanations by agents; it lists only the facts in the records.
It does not change the state, so you can run it in the middle of the loop.

- Overview (plan, when the plan started, the URL of the latest work report, and the PR to merge per repository)
- Changes per task (a table with one row per task: branch, changed line counts, expected line counts, and review verdicts)
- Tasks the implementer did without creating a branch
- Deferred findings
- Protected areas (what was declared and the command to see the diff)

`korylusflow status` shows the time spent and the time spent waiting for a human.

`korylusflow` does not push, create PRs, or merge.
After a human checks the work report, they follow the PRs to merge in `korylusflow report`: one PR per repository from the last task's branch to the first task's base branch, merged with a merge commit.
When the branches do not form a single line, for example because the previous task's branch no longer existed and the branch was recreated from the first base, it shows a PR for each connected line.

When a plan is stacked on a branch other than the default base (develop, or main if there is no develop) with the "[Base branches](#base-branches)" line, it checks with the local branches whether that base has been merged into the default base, and shows the result.

| State of the base            | Base column        | Note                                                                      |
| ---------------------------- | ------------------ | ------------------------------------------------------------------------- |
| Merged into the default base | The default base   | That it has been merged                                                   |
| Not merged yet               | The stacked branch | Merge the stacked branch's PR first, then open the PR to the default base |
| Does not exist locally       | The stacked branch | That it cannot check whether it has been merged                           |

State written while Markdown work reports (`report-<version>.md`) were built can still be read.
Earlier versions are carried over as rounds without a page, and the next time the plan reaches the end, the implementer creates a new work report page.
Existing `report-<version>.md` files are not deleted.

### Where state is stored

State is kept in `docs/private/plans/files/<plan name>/` so that it remains as history together with the plan.

| File             | Content                                                                 |
| ---------------- | ----------------------------------------------------------------------- |
| `flow.json`      | The current task, step, and role whose turn it is, and records per task |
| `flow.lock`      | An empty file for the file lock                                         |
| `timeline.jsonl` | The start and end times of each step                                    |

### Installing

Build it with `make install-korylusflow`, which places it at `~/.local/bin/korylusflow` (change the location with `BINDIR=<directory>`).
Use the built binary instead of running it from the working tree with `go run`, so that the running code does not change when a branch is switched in the middle of the loop.

## korylusfactory

`korylusfactory` is a CLI (written in Rust) for advancing work plans without a person attending to them.
It picks the work plans approved on the roadmap one at a time and advances each through implementation, review, fixes, and the work report by launching Claude Code and Codex sessions unattended.
When a decision needs a human, it stops that work plan, adds an item to the human queue (a Wikino page), and lets the human know.
It waits for the human to write an answer in the item and publish it, or to resume the plan with a command.

```sh
korylusfactory start
korylusfactory status [--json]
korylusfactory resume [--review-limit <count>]
korylusfactory queue list [--all] [--json]
korylusfactory queue show <page number> [--json]
korylusfactory queue answer <page number> [<answer>...] [--json]
korylusfactory --version
```

Run every subcommand at the project root.

### Configuration file

Write the roadmap URL and the roadmap sections that count as approved and as done in `docs/private/factory/config.json` at the project root.
To use the human queue and the notifications of human waits, also write the project name, where the human queue lives, and the notification command.

```json
{
  "roadmap": "https://wikino.app/s/<space>/pages/<page number>",
  "sections": ["やってる", "優先度: 高", "優先度: 中"],
  "done_sections": ["リリース待ち", "やった"],
  "project": "<project name>",
  "queue": { "space": "<space>", "topic": 12 },
  "notify": { "command": ["<notification command>", "<argument>"] }
}
```

| Field           | Description                                                                                                                                                                            |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `roadmap`       | The URL of the roadmap page. Only pages on `https://wikino.app` are accepted                                                                                                           |
| `sections`      | The headings (H2) of the sections that count as approved, advanced in the order written. Defaults to 「やってる」「優先度: 高」「優先度: 中」                                          |
| `done_sections` | The headings (H2) of the sections that count as done (merged). Defaults to 「リリース待ち」 and 「やった」                                                                             |
| `project`       | The project name, written with letters, digits, hyphens, and underscores. It is passed to human queue items and the notification command. Required when `queue` or `notify` is written |
| `queue`         | Where the human queue lives (`space`: the space identifier, `topic`: the topic number). When omitted, no items are added and plans are resumed only with `resume`                      |
| `notify`        | The command that announces human waits (`command`: an array of the command and its arguments), run without a shell. When omitted, nothing is announced                                 |

A section missing from the roadmap, a section listed as both approved and done, or an unknown field is treated as a mistake and reported as an error.

The roadmap and work plans are read with the `wikino` command, preferring the draft when there is one.
Read permission on the token used for `wikino auth login <space>` is enough for korylusfactory itself.
Sessions launched by `start` read and write drafts of work plans and work reports.
To use `start`, log in to the `wikino` command used by the sessions with a token that has draft permissions (`draft_page:*`) and permission to read pages and topics (`page:read` and `topic:read`), for each space the sessions work in.
The human queue is read and written with the same permissions.
Do not give the token permission to publish pages (`page:write`).
Publishing is done by a human in the web UI, and publishing an item is what confirms the answer.

### `start`

`start` keeps running until you stop it.
It advances the work plan in progress if there is one, and otherwise the first work plan in the roadmap order (see [`status`](#status)).
When the first work plan in the order cannot be read, it does not skip it and waits for a human to fix the work plan or the roadmap (when the human queue is configured, it adds a notice item).
When there is no work plan to advance, it waits for one to appear on the roadmap.
It rereads the roadmap every 5 minutes.

Tasks that create branches follow korylusflow's [flow of a task](#flow-of-a-task).
The branch a task's branch is created from is also chosen in the same order as korylusflow's [base branches](#base-branches).
The worker session runs on Claude Code, and the reviewer session runs on Codex for the first review and on the other agent than the previous review after that.
Each session is told which skill to run with which arguments, and returns its result as JSON in a fixed shape.
korylusfactory records a step as done only after checking the result and the repository state (the task branch, commits, uncommitted changes, review documents), and that the only changes to the work plan are the task's checkbox and the implementation decision log.
For tasks the implementer does without creating a branch, the session checks the task in the work plan.
korylusfactory checks the completion conditions before moving on to the next task.
At the end of the work plan, the worker writes the work report, and when it includes the walkthrough, a reviewer reviews the work report.

Progress goes to standard output, and errors and warnings go to standard error.
When it starts and before it begins a work plan, it warns if korylusfactory is outdated (see [Warning when outdated](#warning-when-outdated)).
You cannot run `start` twice for the same project.

### Sessions

Sessions run without asking for confirmation, so use korylusfactory in an isolated environment such as a VM.
Pushing and creating or merging pull requests are forbidden in the instructions, and are also blocked by the following.

- Claude Code's deny rules (`git push`, `gh pr create`, `gh pr merge`). They also deny `korylusfactory resume` and `korylusfactory queue answer`, which resume plans with a human's answer
- A Git entry point placed at the front of the session's `PATH`, which refuses pushes, including `git -C <dir> push`
- A supplementary Git setting that rewrites push destinations to a nonexistent remote helper, and an invalid `GH_TOKEN`

A session has a time limit of 2 hours.
When a session ends without returning a result, exceeds the time limit, or does not meet the completion conditions, korylusfactory waits 5 minutes and redoes the same step, adding the unmet conditions to the instructions.
If the step is still not done after 3 retries, it waits for a human.

Each session's instructions, result shape, standard output, and standard error are kept under `docs/private/factory/plans/<plan name>/sessions/`.

### Stopping

Stop `start` with a signal such as Ctrl-C.
The running session is ended, down to the child processes the session started.
The state stays as it was before the session's result was recorded, and the next run redoes the same step, telling the session that it was interrupted.
Until the stopped session has been fully ended, the next `start` cannot run.

### Human waits and `resume`

When a decision needs a human, korylusfactory stops that work plan, shows the reason, and checks the state file every 5 seconds until the wait is lifted.
When the human queue is configured, every situation is resumed by writing an answer in the human queue item and publishing it (see [Human queue](#human-queue)).
`resume` is the command for resuming from a terminal, for example when Wikino cannot be read or written.

| Situation                                | What the human does                                                         | Resuming                                                       |
| ---------------------------------------- | --------------------------------------------------------------------------- | -------------------------------------------------------------- |
| A task done by a person                  | Do the work and check the task in the work plan                             | `resume`                                                       |
| Preparing a task                         | Clean up uncommitted changes, or fix the task prefix or the base branch     | `resume`                                                       |
| A review finding that needs a human      | Answer the response options in the review document                          | `resume`                                                       |
| The review limit                         | Decide whether to continue or stop                                          | `resume` (`--review-limit` to continue)                        |
| The retry limit                          | Remove the cause of the stop                                                | `resume`                                                       |
| A question from a session                | Handle the question the session wrote in its result                         | `resume`                                                       |
| Checking the work report (with feedback) | Add a feedback phase to the work plan                                       | `resume`                                                       |
| Checking the work report (done)          | Create and merge the PR, and move the item to a done section on the roadmap | Not needed (see [Reporting completion](#reporting-completion)) |

`resume` resumes the work plan waiting for a human, according to the situation it was waiting in.

- For a task done by a person and for preparing a task, it picks again from the first unfinished task in the work plan
- For review waits, it reads the human's answers and answers the response options again. When waiting at the review limit, raising the limit with `--review-limit <count>` re-reviews after the fixes, and not raising it moves on after the fixes without re-reviewing
- For the retry limit and a question from a session, it redoes the same step in a new session
- For checking the work report, it confirms that only a feedback phase was added to the work plan, then continues from that task

When the conditions for resuming are not met, it shows the reason and leaves the state unchanged.
It is an error when no work plan, or more than one, is waiting for a human.

### Human queue

A human queue item is a page in the topic set in `queue` in the configuration file.
When a plan starts waiting for a human, `start` adds an item as a draft of a new page.
The item holds the work plan, the situation, the reason, and answer fields that depend on the situation.

| State in Wikino            | Meaning                         |
| -------------------------- | ------------------------------- |
| A draft of a new page      | Waiting for a human             |
| Published, without a draft | Answered                        |
| Published, with a draft    | Sent back (waiting for a human) |

Items waiting for a human appear in Wikino's list of drafts.
Their titles have the form `<project name> - <plan name> - <situation> - <time added>`.
The time is written in the time zone of the environment running `start` (the `TZ` environment variable or the system setting).

Items can also be answered from the web UI on a smartphone.

1. Open the item from Wikino's list of drafts
2. Write the answer in the 「## 回答」 section: check one option in a choice field, and write text under the heading of a free-text field
3. Publish the page

While waiting for a human, `start` reads the item every minute.
When it has been published, `start` applies the answer according to the situation and resumes.

| Situation                                                                            | Answer fields                                                                                                     |
| ------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------- |
| A review finding that needs a human                                                  | The response options of each finding. The chosen options are written to the review document, and the fixes follow |
| The review limit                                                                     | 「上限を延ばして再レビューする」 (raises the limit by one) and 「打ち切って次へ進む」                             |
| A question from a session                                                            | Free text. The answer is added to the instructions of the session that redoes the same step                       |
| A task done by a person, preparing a task, the retry limit, checking the work report | None. Publish after handling it, and the plan resumes under the same conditions as `resume`                       |
| The first work plan cannot be read, branches are not merged                          | None. No need to publish; `start` cleans up the item once the cause is resolved                                   |

For a review finding, a response not in the options (「その他」) cannot be chosen in the item.
In the review document, the human checks 「その他」 under 「対応方針」 for the finding.
They write the response in the 「回答」 field of the same finding, then run `resume` from a terminal.

When the answer has the wrong form (no option chosen, two options chosen, options rewritten, and so on) or the conditions for resuming are not met, `start` creates a new draft on the published page and writes the reasons in a 「差し戻し」 section at the top of the body.
Fix the draft and publish it again.

When a wait is lifted before its item is published (by `resume` or by detecting completion), `start` keeps the body in the state directory and then deletes the item's draft.
When a human deletes an item's draft, `start` adds the item again.
When Wikino cannot be read or written, `start` shows a warning and retries a minute later.
It does not stop recording the human wait or the task in progress.

### Notifying human waits

Human waits are announced with the notification command and on the terminal.

The notification command (`notify` in the configuration file) runs once when a plan starts waiting for a human (after the item is added) and once each time an answer is sent back.
When the item could not be added, it announces the wait without the URL, and announces it again with the URL once the item is added.
The notice items for a first work plan that cannot be read and for branches that are not merged are announced, with the URL, only once the item is added.
Without the human queue, these two situations are not announced.
The command receives only the following environment variables, never the item's body.

| Environment variable     | Content                                                                                                                                                |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `KORYLUSFACTORY_PROJECT` | The project name                                                                                                                                       |
| `KORYLUSFACTORY_PLAN`    | The plan name. For the notice that the first work plan cannot be read, the plan name is unknown, so the title of the Wikilink in the roadmap is passed |
| `KORYLUSFACTORY_SCENE`   | The situation. When an answer is sent back, 「<situation> (差し戻し)」                                                                                 |
| `KORYLUSFACTORY_URL`     | The URL that opens the item in the editor. Not passed when no item was added                                                                           |

The part that sends to a destination (such as a push to a smartphone) is prepared on the command's side.
Secrets for the destination are kept by the command, not in the configuration file.
Here is an example of using a script that sends to ntfy as the notification command.

```sh
#!/bin/sh
# ~/.local/bin/notify-factory (the ntfy topic is passed in the NTFY_TOPIC environment variable)
curl -fsS -H "Title: $KORYLUSFACTORY_PROJECT: $KORYLUSFACTORY_SCENE" \
  -H "Click: ${KORYLUSFACTORY_URL:-}" \
  --data-raw "$KORYLUSFACTORY_PLAN" "https://ntfy.sh/$NTFY_TOPIC"
```

```json
{ "notify": { "command": ["notify-factory"] } }
```

The command is stopped after 30 seconds, and failures and timeouts only produce a warning.

When the standard error of `start` is a terminal, `start` shows the current state in the terminal title and rings the bell once when a plan starts waiting for a human.
The title looks like `korylusfactory - <plan name> - タスク 1-2 の実装` during a session and `korylusfactory - 人間待ち - <situation>` while waiting for a human.

### `queue`

`queue` lists and shows human queue items and writes answers to an item's draft.
It only reads and writes Wikino and never writes state files.
From any project, it can read the items of every project in the queue.

| Subcommand                               | Role                                                                                                                             |
| ---------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `queue list [--all]`                     | Shows the items waiting for a human in the order they were added. By default this project's items; `--all` shows every project's |
| `queue show <page number>`               | Shows the item's body and its answer fields (labels and options of choice fields)                                                |
| `queue answer <page number> <answer>...` | Writes answers, in field order, to the item's draft. It does not publish                                                         |

Pass `queue answer` the answers in field order: a label (such as `A`) for a choice field and the answer text for a free-text field.
Pass text starting with `-` after `--`.
It checks the form of the answers before writing, and records a 「回答の書き手: <writer> (<time>)」 line before the answer fields.
The writer is told apart by whether the command runs inside a Claude Code or Codex session.
It does not write to items without answer fields.
The answer is confirmed when a human publishes the page in the web UI.

With `--json`, each of them writes JSON.

### Reporting completion

korylusfactory learns that a work plan is done (merged) from the roadmap, not from a command.
While waiting for the work report to be checked, `start` reads the roadmap every 5 minutes, and when both of the following hold, it records the completion and moves on to the next work plan.

1. A Wikilink to the work plan is in an item of a done section
2. For each repository where tasks created branches, the last task's branch is merged into the default base (develop, or main if there is none) on the remote `origin`

When 1 holds but 2 does not, it shows the branches that are not merged and waits.
A work plan without tasks that create branches is done with 1 alone.
A work plan recorded as done is not advanced even if it is in an approved section of the roadmap.

### `status`

It shows the korylusfactory version, the order of the work plans to advance, and the work plans in progress.

The order lists the Wikilinks whose titles end with 「作業計画書」 in the items of the approved sections, by section order and then from top to bottom within each section.
Items with only a name (plans not written yet), links to tickets, and nested items are skipped.
When several links point to the same work plan, only the first is kept.
Work plans recorded as done are left out of the order.

Each work plan is shown as one of the following.

| Shown                              | Meaning                                                                      |
| ---------------------------------- | ---------------------------------------------------------------------------- |
| The plan name and URL              | The work plan was read                                                       |
| 作業計画書のページが見つかりません | There is neither a draft nor a page with the link's title                    |
| 作業計画書として読み取れません     | The page exists but cannot be read as a work plan. The URL and reason follow |

Each work plan in progress is shown with its current step and the reason it is waiting for a human.
When an item was added to the human queue, its URL is shown too.
When it is waiting for the work report to be checked, `status` checks for completion and shows the branches that are not merged.

With `--json`, the same content is written as JSON.
When the roadmap cannot be read (not logged in, Wikino is down, and so on), it writes the reason to standard error and exits with code 1.

### Warning when outdated

`status` and `start` look up the last commit that changed the Rust code (`crates/`, `Cargo.toml`, `Cargo.lock`) on `main` of the repository it was built from.
If the installed korylusfactory does not contain that commit, they warn you to reinstall with `make install-korylusfactory`.
They only warn and do not reinstall automatically.

Before comparing, they fetch the remote `main`, and compare with the local `main` when fetching fails.
When they cannot check (for example, the source repository is missing), they do not warn and instead show that they could not check and why.

### Where the state is kept

The state is kept in `docs/private/factory/` at the project root.

| File                           | Contents                                                                                                                                                               |
| ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config.json`                  | The configuration file                                                                                                                                                 |
| `plans/<plan name>/state.json` | The progress of each work plan (current task and step, retry count, reason for waiting for a human, time of completion, human queue items added) and a record per task |
| `plans/<plan name>/queue/`     | The bodies of items cleaned up without being published (`<page number>.md`)                                                                                            |
| `queue.json`                   | The records of notice items added for waits not kept in state files (the first work plan cannot be read, branches are not merged)                                      |
| `queue/`                       | The bodies of notice items that were cleaned up (`<page number>.md`)                                                                                                   |
| `plans/<plan name>/sessions/`  | A record per session                                                                                                                                                   |
| `plans/<plan name>/state.lock` | An empty file used for file locking                                                                                                                                    |
| `start.lock`, `session.lock`   | Empty files used for file locking, to detect a second `start` and the cleanup of a stopped session                                                                     |

The per-task records use the same field names as `tasks` in korylusflow's `flow.json`, so that the work report skills can read them as material.

### `--version`

`--version` shows the version number, the commit it was built from, the build date, and the location of the repository it was built from.
`-V` shows the same on one line, without the repository location.

### Installing

Build it with `make install-korylusfactory`, which places it at `~/.local/bin/korylusfactory` (change the location with `BINDIR=<directory>`).
It is built with the Rust version in `mise.toml` (`mise install rust`).
Use the built executable so that switching branches while `start` is running does not change the code being run.

## Installing and running

Building koryluslint requires Go, a C compiler (GCC or Clang), and `CGO_ENABLED=1`.
Shell and TypeScript parsing use the [Go bindings for Tree-sitter](https://github.com/tree-sitter/go-tree-sitter).

Each project pins a version of `koryluslint` to fetch it.

- Go projects: pin `github.com/korylus/tools/cmd/koryluslint` with the `tool` directive in `go.mod` and run it with `go tool koryluslint ...`.
- Non-Go projects: pin it with mise's `go:` backend.

## Development

```sh
make build                   # build koryluslint (to bin/koryluslint)
make install-korylusflow     # build korylusflow (to ~/.local/bin/korylusflow)
make install-korylusfactory  # build korylusfactory (to ~/.local/bin/korylusfactory)
make test                    # run the Go and Rust tests
make vet                     # run go vet
make lint                    # run golangci-lint and clippy
make fmt                     # format with gofmt + goimports + rustfmt + Oxfmt
make fmt-check               # check formatting
```

Append `-go` or `-rust` to `test`, `lint`, or `fmt-check` to run only one of them (e.g. `make test-rust`).
`fmt-check-md` checks only the formatting of Markdown and other documents.
