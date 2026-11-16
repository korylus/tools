//! セッションへの指示の文面と、セッションが返す結果の形。
//!
//! 指示には、実行するスキルと引数に加えて、korylusfactoryが人間に代わって指示していること・
//! pushとPRの作成をしないこと・人間に質問せず判断が要るときは結果に書いて終えることを、毎回書く。

use std::fmt::Write as _;
use std::path::Path;

use korylusfactory_core::flow::{Next, Task};
use korylusfactory_core::state::{PlanState, Reviewer, TaskRecord};
use serde_json::{Value, json};

/// 指示を作れないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 進めているタスクの記録が無い。
    #[error("タスク {0} の記録がありません")]
    MissingRecord(String),
}

/// セッションを動かすエージェント。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Agent {
    /// Claude Code。作業者と、Claude Codeのレビュアーのセッションを動かす。
    Claude,
    /// Codex。Codexのレビュアーのセッションを動かす。
    Codex,
}

impl Agent {
    /// レビュアーreviewerのセッションを動かすエージェントを返す。
    pub fn reviewer(reviewer: Reviewer) -> Self {
        match reviewer {
            Reviewer::Codex => Agent::Codex,
            Reviewer::Claude => Agent::Claude,
        }
    }
}

/// セッションに頼むこと。
#[derive(Debug, Clone, PartialEq)]
pub struct Request {
    /// セッションを動かすエージェント。
    pub agent: Agent,
    /// セッションへの指示。
    pub prompt: String,
    /// セッションが返す結果のJSON Schema。
    pub schema: Value,
}

/// 指示を作るときに使う、作業計画書と状態の場所。
#[derive(Debug, Clone, Copy)]
pub struct Context<'a> {
    /// 作業計画書のURL。
    pub plan_url: &'a str,
    /// 状態ファイルのパス。作業報告書のスキルが材料として読む。
    pub state_path: &'a Path,
    /// やり直すときの、前のセッションが満たせなかった条件。初めてなら空。
    pub retry: &'a [String],
}

/// 次に行うことnextのセッションに頼むことを、状態stateと場所ctxから作る。
///
/// セッションを起動しないこと (タスクの準備・人間待ち・終わり) なら `None` を返す。
pub fn build(next: &Next, state: &PlanState, ctx: &Context<'_>) -> Result<Option<Request>, Error> {
    let record = |task: &Task| {
        state
            .task_record(&task.id)
            .ok_or_else(|| Error::MissingRecord(task.id.clone()))
    };
    let (agent, steps, kind) = match next {
        Next::Prepare { .. } | Next::Wait(_) | Next::Finished => return Ok(None),
        Next::Implement { task } => (
            Agent::Claude,
            implement(ctx, task, record(task)?),
            Kind::Implement,
        ),
        Next::Work { task } => (Agent::Claude, work(ctx, task), Kind::Plain),
        Next::Review { task, reviewer, .. } => (
            Agent::reviewer(*reviewer),
            review(ctx, task, record(task)?),
            Kind::Plain,
        ),
        Next::Fix {
            task,
            reviewer,
            doc,
        } => (
            Agent::reviewer(*reviewer),
            fix(task, record(task)?, doc),
            Kind::Plain,
        ),
        Next::Report { previous } => (
            Agent::Claude,
            report(ctx, state, previous.as_deref()),
            Kind::Report,
        ),
        Next::ReportReview {
            url,
            reviewer,
            prefix,
            ..
        } => (
            Agent::reviewer(*reviewer),
            report_review(ctx, url, prefix),
            Kind::Plain,
        ),
        Next::ReportFix { url, reviewer, doc } => (
            Agent::reviewer(*reviewer),
            report_fix(url, doc),
            Kind::Plain,
        ),
    };
    Ok(Some(Request {
        agent,
        prompt: prompt(ctx, &steps, kind),
        schema: kind.schema(),
    }))
}

/// セッションが返す結果の種類。
#[derive(Debug, Clone, Copy)]
enum Kind {
    /// 人間の判断が要るかだけを返す。
    Plain,
    /// 動作確認の項目も返す。
    Implement,
    /// 作業報告書のURLと、処理の流れを書かなかった理由も返す。
    Report,
}

impl Kind {
    /// 結果のJSON Schemaを返す。
    ///
    /// CodexとClaude Codeの両方が受け付けるよう、すべての項目を必須にし、値が無いことはnullで表す。
    fn schema(self) -> Value {
        let nullable = json!({"type": ["string", "null"]});
        let mut properties = json!({"needs_human": nullable});
        match self {
            Kind::Plain => {}
            Kind::Implement => {
                properties["verification"] = json!({
                    "type": "array",
                    "items": {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["kind", "title", "target", "steps", "expected"],
                        "properties": {
                            "kind": {"type": "string", "enum": ["screen", "process"]},
                            "title": {"type": "string"},
                            "target": {"type": "string"},
                            "steps": {"type": "array", "items": {"type": "string"}},
                            "expected": {"type": "string"},
                        },
                    },
                });
            }
            Kind::Report => {
                properties["location"] = nullable.clone();
                properties["walkthrough_skipped"] = nullable;
            }
        }
        let required: Vec<&String> = properties
            .as_object()
            .map(|object| object.keys().collect())
            .unwrap_or_default();
        json!({
            "type": "object",
            "additionalProperties": false,
            "required": required,
            "properties": properties,
        })
    }

    /// 結果の項目の説明を返す。
    fn result(self) -> &'static str {
        match self {
            Kind::Plain => "- `needs_human`: 人間の判断が要るときはその理由、要らなければ null\n",
            Kind::Implement => {
                "- `needs_human`: 人間の判断が要るときはその理由、要らなければ null\n\
                 - `verification`: 動作確認の項目の配列。各項目は `kind` (画面の表示・フォームの挙動なら `screen`、バックグラウンドの処理・コマンドの出力なら `process`)・`title` (確認することを1文で)・`target` (画面ならアプリのパスだけ、処理なら実行するコマンド)・`steps` (人が行う操作の手順)・`expected` (確認できたと判断できる、観測可能な結果) を持つ。無ければ空の配列\n"
            }
            Kind::Report => {
                "- `needs_human`: 人間の判断が要るときはその理由、要らなければ null\n\
                 - `location`: 作業報告書のページのURL (`https://` で始まる)。書けなかったときは null\n\
                 - `walkthrough_skipped`: 処理の流れを書かなかったときはその理由、書いたときは null\n"
            }
        }
    }
}

/// 共通の前書き・やり直しの説明・手順stepsと、種類kindの結果の説明をつないで、指示の文面にする。
fn prompt(ctx: &Context<'_>, steps: &Steps, kind: Kind) -> String {
    let mut out = format!(
        "korylusfactoryが、人間に代わって作業計画書 {} の作業を指示しています。\n\n",
        ctx.plan_url
    );
    out.push_str(
        "## 守ること\n\n\
         - 手順にあるスキルの実行 (コミット・レビュー・作業報告書の作成と更新を含む) は、人間からの明示的な指示として扱う\n\
         - リモートへの `git push` と、Pull Requestの作成・マージはしない\n\
         - ブランチの作成と切り替えはしない。要るブランチは、korylusfactoryが準備している\n\
         - 人間に質問しない。人間の判断が要るときは、作業を止めて、結果の `needs_human` に理由を書いて終える\n\n",
    );
    if !ctx.retry.is_empty() {
        out.push_str("## やり直し\n\n前のセッションは、途中で止まったか、完了の条件を満たしませんでした。\n\n");
        for reason in ctx.retry {
            let _ = writeln!(out, "- {}", reason.replace('\n', "\n  "));
        }
        out.push_str("\n未コミットの変更があれば読み、続きから進めてください。\n\n");
    }
    if !steps.notes.is_empty() {
        out.push_str(&steps.notes);
        out.push('\n');
    }
    out.push_str("## 手順\n\nプロジェクトのルートで、次の順に作業する。\n\n");
    for (i, step) in steps.items.iter().enumerate() {
        let _ = writeln!(out, "{}. {step}", i + 1);
    }
    out.push_str("\n## 結果\n\n終えたら、次の項目を持つJSONで結果を答える。\n\n");
    out.push_str(kind.result());
    out
}

/// 1つのステップの手順。
#[derive(Debug, Default)]
struct Steps {
    /// 手順の前に置く材料 (状態ファイルのパス・ブランチなど)。無ければ空。
    notes: String,
    /// 番号を付けて並べる手順。
    items: Vec<String>,
}

/// 状態ファイルのパスを伝える段落を返す。
fn state_note(ctx: &Context<'_>) -> String {
    format!(
        "状態ファイル: `{}`\n\n作業報告書のスキルは、korylusflowの状態ファイル (`flow.json`) の代わりに、この状態ファイルの `tasks` を材料として読む。状態ファイルは書き換えない。\n",
        ctx.state_path.display()
    )
}

fn implement(ctx: &Context<'_>, task: &Task, record: &TaskRecord) -> Steps {
    let repo = record.repo.display();
    Steps {
        notes: String::new(),
        items: vec![
            format!(
                "`/korylus-do-plan-task {} {} repo={repo} base={}` を実行する。ブランチ {} は作成済みのため、ブランチを作らない",
                ctx.plan_url, task.id, record.base, record.branch
            ),
            format!(
                "`/korylus-commit repo={repo}` で、タスクの変更を {repo} にコミットする。docs/private 配下 (作業計画書・レビュードキュメントなど) はコミットしない"
            ),
            "変更のうち、人が画面や処理で確かめられるものを、結果の `verification` にまとめる。無ければ空の配列にする。テストで担保した内容は重ねて書かない".to_owned(),
        ],
    }
}

fn work(ctx: &Context<'_>, task: &Task) -> Steps {
    Steps {
        notes: String::new(),
        items: vec![
            format!(
                "作業計画書 {} のタスク {} を読み、書かれた作業を行う。ブランチの作成・切り替えとコミットはしない",
                ctx.plan_url, task.id
            ),
            "取り返しのつかない操作 (gitで追跡していないファイルの削除など) や、gitのリポジトリのファイルの変更が要るときは、行わずに `needs_human` に理由を書いて終える".to_owned(),
            format!(
                "作業を終えたら、作業計画書のタスク {} にチェックを付ける。作業計画書のほかの箇所は、実装判断ログへの追記を除いて変えない",
                task.id
            ),
        ],
    }
}

fn review(ctx: &Context<'_>, task: &Task, record: &TaskRecord) -> Steps {
    let mut command = format!("/korylus-review {} {}", record.base, ctx.plan_url);
    if let Some(repo) = &record.review.review_repo {
        let _ = write!(command, " repo={}", repo.display());
    }
    Steps {
        notes: String::new(),
        items: vec![
            format!(
                "`{command}` を実行し、ブランチ {} のタスク {} の差分をレビューする。ブランチは切り替えない",
                record.branch, task.id
            ),
            "レビュードキュメントの対応方針の選択肢にはチェックを入れない。回答は、korylusfactoryが区分と推奨をもとに行う".to_owned(),
        ],
    }
}

fn fix(task: &Task, record: &TaskRecord, doc: &Path) -> Steps {
    let (repo, dir) = (record.repo.display(), record.dir.display());
    Steps {
        notes: String::new(),
        items: vec![
            format!(
                "レビュードキュメント {} で対応方針にチェックの入ったタスク {} の指摘を、選ばれた方針どおりに {dir} で修正する。チェック・評価・総評は書き換えず、自分で再レビューしない",
                doc.display(),
                task.id
            ),
            format!("{dir} に当てはまるフォーマット・リント・テストを実行し、通るまで直す"),
            format!(
                "`/korylus-commit repo={repo}` で、修正を {repo} のブランチ {} にコミットする。docs/private 配下 (作業計画書・レビュードキュメントなど) はコミットしない",
                record.branch
            ),
        ],
    }
}

fn report(ctx: &Context<'_>, state: &PlanState, previous: Option<&str>) -> Steps {
    let mut notes = state_note(ctx);
    notes.push_str("\nリポジトリごとの最後のタスクのブランチ:\n\n");
    let branches = state.last_branches();
    if branches.is_empty() {
        notes.push_str("- なし (ブランチを作ったタスクがありません)\n");
    }
    for record in branches {
        let _ = writeln!(notes, "- {}: `{}`", record.repo.display(), record.branch);
    }
    let mut write = format!(
        "`/korylus-write-report {}` を実行し、作業計画書の作業報告書を書く",
        ctx.plan_url
    );
    if let Some(previous) = previous {
        let _ = write!(notes, "\n前の回の作業報告書: {previous}\n");
        write.push_str(
            "。前の回の作業報告書を、新しく作らずに上のブランチのコードに合わせて更新する",
        );
    }
    write.push_str("。ブランチは切り替えない");
    Steps {
        notes,
        items: vec![
            write,
            "書いた作業報告書のURLを結果の `location` に入れる。処理の流れを書かなかったときは、その理由を `walkthrough_skipped` に入れる".to_owned(),
        ],
    }
}

fn report_review(ctx: &Context<'_>, url: &str, prefix: &str) -> Steps {
    Steps {
        notes: state_note(ctx),
        items: vec![
            format!(
                "`/korylus-review-doc {url} prefix={prefix}` を実行し、作業報告書 {url} をレビューする。ブランチは切り替えない"
            ),
            "レビュードキュメントの対応方針の選択肢にはチェックを入れない。回答は、korylusfactoryが区分と推奨をもとに行う".to_owned(),
        ],
    }
}

fn report_fix(url: &str, doc: &Path) -> Steps {
    Steps {
        notes: String::new(),
        items: vec![
            format!(
                "レビュードキュメント {} で対応方針にチェックの入った指摘を、選ばれた方針どおりに、作業報告書 {url} の下書きを書き換えて修正する。フィードバックの節には触れない。チェック・評価・総評は書き換えず、自分で再レビューしない",
                doc.display()
            ),
            "作業報告書は下書きで、コミットしない。ブランチも切り替えない".to_owned(),
        ],
    }
}

#[cfg(test)]
mod tests {
    use std::path::PathBuf;

    use korylusfactory_core::flow::PlanDocument;
    use korylusfactory_core::state::{ReportRound, ReviewTrail, Wait, WaitKind};

    use super::*;

    const URL: &str = "https://wikino.app/s/shimbaco/pages/2930";

    fn task(id: &str) -> Task {
        PlanDocument::parse(format!(
            "**計画書名**: demo\n\n## タスクリスト\n\n- [ ] **{id}**: [/korylus-tools] 状態ファイルを作る\n"
        ))
        .expect("作業計画書を読み取れなかった")
        .plan()
        .tasks[0]
            .clone()
    }

    /// タスク1-1のブランチを /korylus-tools に準備した状態。
    fn state() -> PlanState {
        let mut state = PlanState::new(URL, "デモ 作業計画書");
        state.tasks.push(TaskRecord {
            id: "1-1".to_owned(),
            repo: PathBuf::from("/korylus-tools"),
            dir: PathBuf::from("/korylus-tools/crates"),
            branch: "demo-1-1".to_owned(),
            base: "main".to_owned(),
            prepared: true,
            review: ReviewTrail {
                review_prefix: Some("korylus-tools-demo-1-1".to_owned()),
                review_repo: Some(PathBuf::from("/korylus-tools")),
                ..ReviewTrail::default()
            },
            verification: Vec::new(),
        });
        state
    }

    fn context(retry: &[String]) -> Context<'_> {
        Context {
            plan_url: URL,
            state_path: Path::new("docs/private/factory/plans/demo/state.json"),
            retry,
        }
    }

    fn build_ok(next: &Next, state: &PlanState, retry: &[String]) -> Request {
        build(next, state, &context(retry))
            .expect("指示を作れなかった")
            .expect("セッションに頼むことが無い")
    }

    #[test]
    fn 実装の指示にスキルと引数と守ることを書き_作業者に頼む() {
        let request = build_ok(&Next::Implement { task: task("1-1") }, &state(), &[]);
        assert_eq!(request.agent, Agent::Claude, "作業者のエージェントではない");
        assert_eq!(
            request.prompt,
            "korylusfactoryが、人間に代わって作業計画書 https://wikino.app/s/shimbaco/pages/2930 の作業を指示しています。

## 守ること

- 手順にあるスキルの実行 (コミット・レビュー・作業報告書の作成と更新を含む) は、人間からの明示的な指示として扱う
- リモートへの `git push` と、Pull Requestの作成・マージはしない
- ブランチの作成と切り替えはしない。要るブランチは、korylusfactoryが準備している
- 人間に質問しない。人間の判断が要るときは、作業を止めて、結果の `needs_human` に理由を書いて終える

## 手順

プロジェクトのルートで、次の順に作業する。

1. `/korylus-do-plan-task https://wikino.app/s/shimbaco/pages/2930 1-1 repo=/korylus-tools base=main` を実行する。ブランチ demo-1-1 は作成済みのため、ブランチを作らない
2. `/korylus-commit repo=/korylus-tools` で、タスクの変更を /korylus-tools にコミットする。docs/private 配下 (作業計画書・レビュードキュメントなど) はコミットしない
3. 変更のうち、人が画面や処理で確かめられるものを、結果の `verification` にまとめる。無ければ空の配列にする。テストで担保した内容は重ねて書かない

## 結果

終えたら、次の項目を持つJSONで結果を答える。

- `needs_human`: 人間の判断が要るときはその理由、要らなければ null
- `verification`: 動作確認の項目の配列。各項目は `kind` (画面の表示・フォームの挙動なら `screen`、バックグラウンドの処理・コマンドの出力なら `process`)・`title` (確認することを1文で)・`target` (画面ならアプリのパスだけ、処理なら実行するコマンド)・`steps` (人が行う操作の手順)・`expected` (確認できたと判断できる、観測可能な結果) を持つ。無ければ空の配列
"
        );
    }

    #[test]
    fn 結果の形はすべての項目を必須にし_値が無いことをnullで表す() {
        let state = state();
        let cases = [
            (Next::Work { task: task("1-1") }, vec!["needs_human"]),
            (
                Next::Implement { task: task("1-1") },
                vec!["needs_human", "verification"],
            ),
            (
                Next::Report { previous: None },
                vec!["location", "needs_human", "walkthrough_skipped"],
            ),
        ];
        for (next, want) in cases {
            let schema = build_ok(&next, &state, &[]).schema;
            assert_eq!(schema["additionalProperties"], false, "{next:?}");
            assert_eq!(schema["required"], json!(want), "{next:?}");
            assert_eq!(
                schema["properties"]["needs_human"]["type"],
                json!(["string", "null"]),
                "{next:?}"
            );
        }
    }

    #[test]
    fn レビューと修正はレビュアーに応じたエージェントに頼み_別のリポジトリはrepoを渡す() {
        let state = state();
        let review = build_ok(
            &Next::Review {
                task: task("1-1"),
                reviewer: Reviewer::Codex,
                number: 1,
                prefix: "korylus-tools-demo-1-1".to_owned(),
                repo: Some(PathBuf::from("/korylus-tools")),
            },
            &state,
            &[],
        );
        assert_eq!(review.agent, Agent::Codex);
        assert!(
            review.prompt.contains(
                "1. `/korylus-review main https://wikino.app/s/shimbaco/pages/2930 repo=/korylus-tools` を実行し、ブランチ demo-1-1 のタスク 1-1 の差分をレビューする。"
            ) && review.prompt.contains("対応方針の選択肢にはチェックを入れない"),
            "指示 = {}",
            review.prompt
        );

        let fix = build_ok(
            &Next::Fix {
                task: task("1-1"),
                reviewer: Reviewer::Claude,
                doc: PathBuf::from("docs/private/reviews/korylus-tools-demo-1-1-002.md"),
            },
            &state,
            &[],
        );
        assert_eq!(fix.agent, Agent::Claude);
        assert!(
            fix.prompt.contains(
                "1. レビュードキュメント docs/private/reviews/korylus-tools-demo-1-1-002.md で対応方針にチェックの入ったタスク 1-1 の指摘を、選ばれた方針どおりに /korylus-tools/crates で修正する。"
            ) && fix
                .prompt
                .contains("`/korylus-commit repo=/korylus-tools` で、修正を /korylus-tools のブランチ demo-1-1 にコミットする"),
            "指示 = {}",
            fix.prompt
        );
    }

    #[test]
    fn 作業報告書の指示に状態ファイルのパスとブランチと前の回を書く() {
        let mut state = state();
        let request = build_ok(
            &Next::Report {
                previous: Some("https://wikino.app/s/shimbaco/pages/2940".to_owned()),
            },
            &state,
            &[],
        );
        assert_eq!(request.agent, Agent::Claude);
        for want in [
            "状態ファイル: `docs/private/factory/plans/demo/state.json`",
            "korylusflowの状態ファイル (`flow.json`) の代わりに、この状態ファイルの `tasks` を材料として読む",
            "- /korylus-tools: `demo-1-1`",
            "前の回の作業報告書: https://wikino.app/s/shimbaco/pages/2940",
            "1. `/korylus-write-report https://wikino.app/s/shimbaco/pages/2930` を実行し、作業計画書の作業報告書を書く。前の回の作業報告書を、新しく作らずに上のブランチのコードに合わせて更新する。ブランチは切り替えない",
            "- `location`: 作業報告書のページのURL",
        ] {
            assert!(
                request.prompt.contains(want),
                "{want:?} が無い: {}",
                request.prompt
            );
        }

        state.report_rounds.push(ReportRound {
            number: 1,
            at: "2026-11-16T07:00:00Z"
                .parse()
                .expect("日時を読み取れなかった"),
            location: "https://wikino.app/s/shimbaco/pages/2940".to_owned(),
            walkthrough_skipped: None,
            review: ReviewTrail::default(),
        });
        let review = build_ok(
            &Next::ReportReview {
                url: "https://wikino.app/s/shimbaco/pages/2940".to_owned(),
                reviewer: Reviewer::Codex,
                number: 1,
                prefix: "demo-report".to_owned(),
            },
            &state,
            &[],
        );
        assert_eq!(review.agent, Agent::Codex);
        assert!(
            review.prompt.contains("状態ファイル: `docs/private/factory/plans/demo/state.json`")
                && review.prompt.contains(
                    "1. `/korylus-review-doc https://wikino.app/s/shimbaco/pages/2940 prefix=demo-report` を実行し"
                ),
            "指示 = {}",
            review.prompt
        );
    }

    #[test]
    fn やり直すときは満たせなかった条件と続きから進めることを書く() {
        let retry = [
            "/korylus-tools に未コミットの変更があります: M src/lib.rs".to_owned(),
            "作業計画書の5行目: 「設計」節が\n変更されています".to_owned(),
        ];
        let request = build_ok(&Next::Work { task: task("1-1") }, &state(), &retry);
        assert!(
            request.prompt.contains(
                "## やり直し\n\n前のセッションは、途中で止まったか、完了の条件を満たしませんでした。\n\n- /korylus-tools に未コミットの変更があります: M src/lib.rs\n- 作業計画書の5行目: 「設計」節が\n  変更されています\n\n未コミットの変更があれば読み、続きから進めてください。\n"
            ),
            "指示 = {}",
            request.prompt
        );
    }

    #[test]
    fn セッションを起動しないことには頼むことが無い() {
        let state = state();
        let wait = Wait {
            kind: WaitKind::HumanTask,
            reason: "人が行うタスクです".to_owned(),
            details: Vec::new(),
        };
        for next in [
            Next::Prepare {
                task: task("1-1"),
                branch: "demo-1-1".to_owned(),
            },
            Next::Wait(wait),
            Next::Finished,
        ] {
            assert_eq!(
                build(&next, &state, &context(&[])).expect("指示を作れなかった"),
                None,
                "{next:?}"
            );
        }
    }

    #[test]
    fn 記録の無いタスクの実装は頼めない() {
        let err = build(
            &Next::Implement { task: task("1-9") },
            &state(),
            &context(&[]),
        )
        .expect_err("記録の無いタスクの指示を作った");
        assert!(
            matches!(&err, Error::MissingRecord(id) if id == "1-9"),
            "エラー = {err:?}"
        );
    }
}
