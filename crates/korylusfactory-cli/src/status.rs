//! `korylusfactory status` の表示。
//!
//! korylusfactoryのバージョンと古いかどうか、これから進める作業計画書の並び、進行中の作業計画書を、文章かJSONで書く。

use std::fmt::Write as _;

use korylusfactory_core::state::{PlanState, Step, Wait};
use serde::Serialize;

use crate::completion::{Completion, Unmerged};
use crate::roadmap::{Entry, State};
use crate::version::{Build, Compared, Freshness};

/// korylusfactoryのバージョンと、古いかどうかを表す行を返す。
pub fn version_text(build: &Build, freshness: &Freshness) -> String {
    let mut out = build.summary();
    out.push('\n');
    out.push_str(&version_diagnostics(build, freshness));
    out
}

/// ビルドの古さに関する警告と、確認できなかった理由を返す。診断が無ければ空文字列を返す。
pub fn version_diagnostics(build: &Build, freshness: &Freshness) -> String {
    let mut out = String::new();
    match freshness {
        Freshness::Current { .. } => {}
        Freshness::Stale { compared, latest } => {
            let short = latest.get(..7).unwrap_or(latest);
            let _ = writeln!(
                out,
                "警告: このkorylusfactoryは、ビルド元のリポジトリの {} にあるRustのコードの変更 (コミット {short}) を含んでいません。make -C {} install-korylusfactory で入れ直してください",
                compared.reference(),
                shell_arg(&build.source_dir.display().to_string())
            );
        }
        Freshness::Unknown { reason } => {
            let _ = writeln!(out, "古いかどうかを確かめられません: {reason}");
        }
    }
    if let Freshness::Current {
        compared: Compared::Local,
    }
    | Freshness::Stale {
        compared: Compared::Local,
        ..
    } = freshness
    {
        out.push_str("リモートの main を取得できなかったため、手元の main と比べました\n");
    }
    out
}

/// 値をPOSIXシェルで1つの引数になるように引用する。
fn shell_arg(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

/// これから進める作業計画書の並びを表す行を返す。
pub fn plans_text(entries: &[Entry]) -> String {
    if entries.is_empty() {
        return "進める作業計画書はありません\n".to_owned();
    }
    let mut out = "進める作業計画書:\n".to_owned();
    for (i, entry) in entries.iter().enumerate() {
        let link = &entry.candidate.link;
        let title = match &link.topic {
            Some(topic) => format!("{topic}/{}", link.title),
            None => link.title.clone(),
        };
        let section = &entry.candidate.section;
        let _ = match &entry.state {
            State::Ready { url, name } => {
                writeln!(out, "{}. {name} [{section}]\n   {url}", i + 1)
            }
            State::NotFound => writeln!(
                out,
                "{}. 「{title}」 [{section}]: 作業計画書のページが見つかりません",
                i + 1
            ),
            State::Invalid { url, reason } => writeln!(
                out,
                "{}. 「{title}」 [{section}]: 作業計画書として読み取れません\n   {url}\n   {}",
                i + 1,
                reason.replace('\n', "\n   ")
            ),
        };
    }
    out
}

/// 進行中の作業計画書1つの表示に使う内容。
#[derive(Debug, Clone)]
pub struct Progress {
    /// 計画書名。
    pub name: String,
    /// 作業計画書の状態。
    pub state: PlanState,
    /// 作業報告書の確認を待っているときの、完了を検知した結果か、検知できなかった理由。待っていなければ `None`。
    pub completion: Option<Result<Completion, String>>,
}

/// 進行中の作業計画書を表す行を返す。
pub fn progress_text(progress: &[Progress]) -> String {
    if progress.is_empty() {
        return "進行中の作業計画書はありません\n".to_owned();
    }
    let mut out = "進行中の作業計画書:\n".to_owned();
    for p in progress {
        let _ = writeln!(out, "- {}\n  {}", p.name, p.state.url);
        let _ = writeln!(
            out,
            "  現在のステップ: {}",
            describe_step(p.state.task.as_deref(), p.state.step)
        );
        if let Some(wait) = &p.state.wait {
            let _ = writeln!(
                out,
                "  人間を待っています ({}): {}",
                wait.kind.label(),
                wait.reason
            );
            for detail in &wait.details {
                let _ = writeln!(out, "    {detail}");
            }
        }
        match &p.completion {
            Some(Ok(Completion::Unmerged(branches))) => {
                out.push_str("  リモートの既定のベースに取り込まれていないブランチ:\n");
                for branch in branches {
                    let note = if branch.missing {
                        "。ローカルにありません"
                    } else {
                        ""
                    };
                    let _ = writeln!(
                        out,
                        "    {} {} ({}{note})",
                        branch.repo.display(),
                        branch.branch,
                        branch.into
                    );
                }
            }
            Some(Ok(Completion::Done)) => out.push_str(
                "  ロードマップの完了とみなす節にあり、ブランチもリモートの既定のベースに取り込まれています\n",
            ),
            Some(Err(reason)) => {
                let _ = writeln!(out, "  完了を確かめられません: {reason}");
            }
            Some(Ok(Completion::NotListed)) | None => {}
        }
    }
    out
}

/// 進めているタスクtaskとステップstepを、「タスク 1-2 のレビュー」「作業報告書のレビュー」の形にする。
fn describe_step(task: Option<&str>, step: Option<Step>) -> String {
    match (task, step) {
        (Some(task), Some(step)) => format!("タスク {task} の{}", step.label()),
        (Some(task), None) => format!("タスク {task}"),
        (None, Some(Step::Report)) => Step::Report.label().to_owned(),
        (None, Some(step)) => format!("作業報告書の{}", step.label()),
        (None, None) => "なし".to_owned(),
    }
}

#[derive(Debug, Serialize)]
struct ProgressJson<'a> {
    name: &'a str,
    url: &'a str,
    task: Option<&'a str>,
    step: Option<Step>,
    wait: Option<&'a Wait>,
    completion: Option<CompletionJson<'a>>,
}

#[derive(Debug, Serialize)]
#[serde(tag = "state", rename_all = "snake_case")]
enum CompletionJson<'a> {
    NotListed,
    Unmerged { branches: Vec<UnmergedJson<'a>> },
    Done,
    Failed { reason: &'a str },
}

#[derive(Debug, Serialize)]
struct UnmergedJson<'a> {
    repo: String,
    branch: &'a str,
    into: &'a str,
    missing: bool,
}

impl<'a> ProgressJson<'a> {
    fn new(progress: &'a Progress) -> Self {
        let completion = progress
            .completion
            .as_ref()
            .map(|completion| match completion {
                Ok(Completion::NotListed) => CompletionJson::NotListed,
                Ok(Completion::Unmerged(branches)) => CompletionJson::Unmerged {
                    branches: branches.iter().map(UnmergedJson::new).collect(),
                },
                Ok(Completion::Done) => CompletionJson::Done,
                Err(reason) => CompletionJson::Failed { reason },
            });
        Self {
            name: &progress.name,
            url: &progress.state.url,
            task: progress.state.task.as_deref(),
            step: progress.state.step,
            wait: progress.state.wait.as_ref(),
            completion,
        }
    }
}

impl<'a> UnmergedJson<'a> {
    fn new(branch: &'a Unmerged) -> Self {
        Self {
            repo: branch.repo.display().to_string(),
            branch: &branch.branch,
            into: &branch.into,
            missing: branch.missing,
        }
    }
}

/// `korylusfactory status --json` の出力。
#[derive(Debug, Serialize)]
pub struct Report<'a> {
    /// korylusfactoryのバージョンと、古いかどうか。
    korylusfactory: VersionJson<'a>,
    /// これから進める作業計画書。進める順に並べる。
    plans: Vec<PlanJson<'a>>,
    /// 進め始めて、まだ完了を記録していない作業計画書。計画書名の順に並べる。
    progress: Vec<ProgressJson<'a>>,
}

#[derive(Debug, Serialize)]
struct VersionJson<'a> {
    version: &'a str,
    commit: &'a str,
    build_date: &'a str,
    source_dir: String,
    freshness: FreshnessJson<'a>,
}

#[derive(Debug, Serialize)]
#[serde(tag = "state", rename_all = "snake_case")]
enum FreshnessJson<'a> {
    Current { compared: String },
    Stale { compared: String, latest: &'a str },
    Unknown { reason: &'a str },
}

#[derive(Debug, Serialize)]
struct PlanJson<'a> {
    section: &'a str,
    topic: Option<&'a str>,
    title: &'a str,
    #[serde(flatten)]
    state: StateJson<'a>,
}

#[derive(Debug, Serialize)]
#[serde(tag = "state", rename_all = "snake_case")]
enum StateJson<'a> {
    Ready { url: &'a str, name: &'a str },
    NotFound,
    Invalid { url: &'a str, reason: &'a str },
}

impl<'a> Report<'a> {
    /// ビルド元の情報build・古いかどうかfreshness・作業計画書の並びentries・進行中の作業計画書progressから、JSONの出力を作る。
    pub fn new(
        build: &'a Build,
        freshness: &'a Freshness,
        entries: &'a [Entry],
        progress: &'a [Progress],
    ) -> Self {
        let freshness = match freshness {
            Freshness::Current { compared } => FreshnessJson::Current {
                compared: compared.reference(),
            },
            Freshness::Stale { compared, latest } => FreshnessJson::Stale {
                compared: compared.reference(),
                latest,
            },
            Freshness::Unknown { reason } => FreshnessJson::Unknown { reason },
        };
        let plans = entries
            .iter()
            .map(|entry| PlanJson {
                section: &entry.candidate.section,
                topic: entry.candidate.link.topic.as_deref(),
                title: &entry.candidate.link.title,
                state: match &entry.state {
                    State::Ready { url, name } => StateJson::Ready { url, name },
                    State::NotFound => StateJson::NotFound,
                    State::Invalid { url, reason } => StateJson::Invalid { url, reason },
                },
            })
            .collect();
        Self {
            korylusfactory: VersionJson {
                version: &build.version,
                commit: &build.commit,
                build_date: &build.date,
                source_dir: build.source_dir.display().to_string(),
                freshness,
            },
            plans,
            progress: progress.iter().map(ProgressJson::new).collect(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use korylusfactory_core::queue::{Candidate, WikiLink};
    use std::path::PathBuf;

    fn build() -> Build {
        Build {
            version: "0.1.0".to_owned(),
            commit: "abc1234def5678abc1234def5678abc1234def56".to_owned(),
            commit_short: "abc1234".to_owned(),
            date: "2026-11-15".to_owned(),
            source_dir: PathBuf::from("/korylus-tools"),
        }
    }

    fn entry(section: &str, topic: Option<&str>, title: &str, state: State) -> Entry {
        Entry {
            candidate: Candidate {
                section: section.to_owned(),
                link: WikiLink {
                    topic: topic.map(str::to_owned),
                    title: title.to_owned(),
                },
            },
            state,
        }
    }

    fn entries() -> Vec<Entry> {
        vec![
            entry(
                "やってる",
                None,
                "退会機能 作業計画書",
                State::Ready {
                    url: "https://wikino.app/s/shimbaco/pages/10".to_owned(),
                    name: "withdrawal".to_owned(),
                },
            ),
            entry(
                "優先度: 高",
                Some("Annict"),
                "言語方針 作業計画書",
                State::NotFound,
            ),
            entry(
                "優先度: 中",
                None,
                "パスワード 作業計画書",
                State::Invalid {
                    url: "https://wikino.app/s/shimbaco/pages/12".to_owned(),
                    reason: "1行目: 計画書名がありません\n2行目: タスクがありません".to_owned(),
                },
            ),
        ]
    }

    #[test]
    fn 最新ならバージョンだけを表示する() {
        let freshness = Freshness::Current {
            compared: Compared::Remote,
        };
        assert_eq!(
            version_text(&build(), &freshness),
            "korylusfactory 0.1.0 (コミット abc1234、2026-11-15にビルド)\n"
        );
    }

    #[test]
    fn 古ければ入れ直すよう警告する() {
        let freshness = Freshness::Stale {
            compared: Compared::Remote,
            latest: "def5678abc1234def5678abc1234def5678abc12".to_owned(),
        };
        assert_eq!(
            version_text(&build(), &freshness),
            "korylusfactory 0.1.0 (コミット abc1234、2026-11-15にビルド)\n\
             警告: このkorylusfactoryは、ビルド元のリポジトリの origin/main にあるRustのコードの変更 (コミット def5678) を含んでいません。make -C '/korylus-tools' install-korylusfactory で入れ直してください\n"
        );
    }

    #[cfg(unix)]
    #[test]
    fn 警告のコマンドを実行してもパスは展開されず1つの引数になる() {
        use std::process::Command;

        for path in [
            "/tmp/korylus-tools",
            "/tmp/korylus tools",
            "/tmp/korylus'tools",
            "/tmp/korylus\"$KORYLUS_QUOTE_TEST\"",
            "/tmp/korylus $(printf 展開しない) `printf 展開しない`",
            "/tmp/korylus; printf 余計\n次の行",
        ] {
            let mut build = build();
            build.source_dir = PathBuf::from(path);
            let freshness = Freshness::Stale {
                compared: Compared::Remote,
                latest: "def5678".to_owned(),
            };
            let text = version_text(&build, &freshness);
            let command = text
                .split_once("make -C ")
                .and_then(|(_, tail)| tail.split_once(" で入れ直してください"))
                .map(|(args, _)| format!("make -C {args}"))
                .expect("警告にコマンドが無い");
            // makeの代わりに引数を出す関数を置き、表示したコマンドをシェルで解釈させる。
            let script = format!("make() {{ printf '%s\\0' \"$@\"; }}\n{command}");
            let output = Command::new("sh")
                .args(["-c", &script])
                .env("KORYLUS_QUOTE_TEST", "展開しない")
                .output()
                .expect("shを実行できなかった");
            assert!(output.status.success(), "コマンドが失敗した: {path:?}");
            let expected = format!("-C\0{path}\0install-korylusfactory\0");
            assert_eq!(
                output.stdout,
                expected.as_bytes(),
                "パスが変わった: {path:?}"
            );
            let json = serde_json::to_value(Report::new(&build, &freshness, &[], &[]))
                .expect("JSONにできなかった");
            assert_eq!(
                json["korylusfactory"]["source_dir"], path,
                "JSONのパスを引用した"
            );
        }
    }

    #[test]
    fn 手元のmainと比べたらそのことを添える() {
        let freshness = Freshness::Current {
            compared: Compared::Local,
        };
        assert!(
            version_text(&build(), &freshness)
                .ends_with("リモートの main を取得できなかったため、手元の main と比べました\n")
        );
    }

    #[test]
    fn 確かめられなければ理由を表示し警告しない() {
        let freshness = Freshness::Unknown {
            reason: "ビルド元のリポジトリ /korylus-tools がありません".to_owned(),
        };
        let text = version_text(&build(), &freshness);
        assert!(!text.contains("警告"), "警告した: {text}");
        assert!(
            text.ends_with(
                "古いかどうかを確かめられません: ビルド元のリポジトリ /korylus-tools がありません\n"
            ),
            "表示 = {text}"
        );
    }

    #[test]
    fn 作業計画書の並びを状態とともに表示する() {
        assert_eq!(
            plans_text(&entries()),
            "進める作業計画書:\n\
             1. withdrawal [やってる]\n   https://wikino.app/s/shimbaco/pages/10\n\
             2. 「Annict/言語方針 作業計画書」 [優先度: 高]: 作業計画書のページが見つかりません\n\
             3. 「パスワード 作業計画書」 [優先度: 中]: 作業計画書として読み取れません\n   https://wikino.app/s/shimbaco/pages/12\n   1行目: 計画書名がありません\n   2行目: タスクがありません\n"
        );
    }

    #[test]
    fn 作業計画書が無ければそう表示する() {
        assert_eq!(plans_text(&[]), "進める作業計画書はありません\n");
    }

    #[test]
    fn jsonで出す() {
        let build = build();
        let freshness = Freshness::Stale {
            compared: Compared::Local,
            latest: "def5678".to_owned(),
        };
        let entries = entries();
        let json = serde_json::to_value(Report::new(&build, &freshness, &entries, &[]))
            .expect("JSONにできなかった");
        assert_eq!(
            json,
            serde_json::json!({
                "korylusfactory": {
                    "version": "0.1.0",
                    "commit": "abc1234def5678abc1234def5678abc1234def56",
                    "build_date": "2026-11-15",
                    "source_dir": "/korylus-tools",
                    "freshness": {"state": "stale", "compared": "main", "latest": "def5678"},
                },
                "plans": [
                    {
                        "section": "やってる",
                        "topic": null,
                        "title": "退会機能 作業計画書",
                        "state": "ready",
                        "url": "https://wikino.app/s/shimbaco/pages/10",
                        "name": "withdrawal",
                    },
                    {
                        "section": "優先度: 高",
                        "topic": "Annict",
                        "title": "言語方針 作業計画書",
                        "state": "not_found",
                    },
                    {
                        "section": "優先度: 中",
                        "topic": null,
                        "title": "パスワード 作業計画書",
                        "state": "invalid",
                        "url": "https://wikino.app/s/shimbaco/pages/12",
                        "reason": "1行目: 計画書名がありません\n2行目: タスクがありません",
                    },
                ],
                "progress": [],
            })
        );
        let unknown = Freshness::Unknown {
            reason: "理由".to_owned(),
        };
        let json = serde_json::to_value(Report::new(&build, &unknown, &[], &[]))
            .expect("JSONにできなかった");
        assert_eq!(
            json["korylusfactory"]["freshness"],
            serde_json::json!({"state": "unknown", "reason": "理由"})
        );
    }

    /// タスク1-2のレビューを進めている作業計画書と、作業報告書の確認を待っている作業計画書。
    fn progress() -> Vec<Progress> {
        let mut reviewing = PlanState::new(
            "https://wikino.app/s/shimbaco/pages/20",
            "レビュー中 作業計画書",
        );
        reviewing.task = Some("1-2".to_owned());
        reviewing.step = Some(Step::Review);

        let mut reported = PlanState::new(
            "https://wikino.app/s/shimbaco/pages/21",
            "確認待ち 作業計画書",
        );
        reported.wait = Some(Wait {
            kind: korylusfactory_core::state::WaitKind::Report,
            reason: "作業報告書の確認を待っています".to_owned(),
            details: vec!["作業報告書: https://wikino.app/s/shimbaco/pages/30".to_owned()],
        });
        vec![
            Progress {
                name: "reviewing".to_owned(),
                state: reviewing,
                completion: None,
            },
            Progress {
                name: "reported".to_owned(),
                state: reported,
                completion: Some(Ok(Completion::Unmerged(vec![
                    Unmerged {
                        repo: PathBuf::from("/korylus-tools"),
                        branch: "reported-1-2".to_owned(),
                        into: "origin/main".to_owned(),
                        missing: false,
                    },
                    Unmerged {
                        repo: PathBuf::from("/workspace"),
                        branch: "reported-1-1".to_owned(),
                        into: "origin/develop".to_owned(),
                        missing: true,
                    },
                ]))),
            },
        ]
    }

    #[test]
    fn 進行中の作業計画書の現在のステップと人間待ちの理由と取り込まれていないブランチを表示する() {
        assert_eq!(
            progress_text(&progress()),
            concat!(
                "進行中の作業計画書:\n",
                "- reviewing\n",
                "  https://wikino.app/s/shimbaco/pages/20\n",
                "  現在のステップ: タスク 1-2 のレビュー\n",
                "- reported\n",
                "  https://wikino.app/s/shimbaco/pages/21\n",
                "  現在のステップ: なし\n",
                "  人間を待っています (作業報告書の確認): 作業報告書の確認を待っています\n",
                "    作業報告書: https://wikino.app/s/shimbaco/pages/30\n",
                "  リモートの既定のベースに取り込まれていないブランチ:\n",
                "    /korylus-tools reported-1-2 (origin/main)\n",
                "    /workspace reported-1-1 (origin/develop。ローカルにありません)\n",
            )
        );
    }

    #[test]
    fn 進めている作業報告書のステップと確かめられなかった完了を表示する() {
        let mut state = progress().remove(0).state;
        state.task = None;
        state.step = Some(Step::Fix);
        let text = progress_text(&[Progress {
            name: "reviewing".to_owned(),
            state,
            completion: Some(Err("リモートを取得できません".to_owned())),
        }]);
        assert!(
            text.contains("  現在のステップ: 作業報告書の修正\n")
                && text.ends_with("  完了を確かめられません: リモートを取得できません\n"),
            "表示 = {text}"
        );
    }

    #[test]
    fn 進行中の作業計画書が無ければそう表示する() {
        assert_eq!(progress_text(&[]), "進行中の作業計画書はありません\n");
    }

    #[test]
    fn 進行中の作業計画書をjsonで出す() {
        let build = build();
        let freshness = Freshness::Current {
            compared: Compared::Remote,
        };
        let progress = progress();
        let json = serde_json::to_value(Report::new(&build, &freshness, &[], &progress))
            .expect("JSONにできなかった");
        assert_eq!(
            json["progress"],
            serde_json::json!([
                {
                    "name": "reviewing",
                    "url": "https://wikino.app/s/shimbaco/pages/20",
                    "task": "1-2",
                    "step": "review",
                    "wait": null,
                    "completion": null,
                },
                {
                    "name": "reported",
                    "url": "https://wikino.app/s/shimbaco/pages/21",
                    "task": null,
                    "step": null,
                    "wait": {
                        "kind": "report",
                        "reason": "作業報告書の確認を待っています",
                        "details": ["作業報告書: https://wikino.app/s/shimbaco/pages/30"],
                    },
                    "completion": {
                        "state": "unmerged",
                        "branches": [
                            {"repo": "/korylus-tools", "branch": "reported-1-2", "into": "origin/main", "missing": false},
                            {"repo": "/workspace", "branch": "reported-1-1", "into": "origin/develop", "missing": true},
                        ],
                    },
                },
            ])
        );
    }
}
