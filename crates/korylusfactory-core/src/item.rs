//! 人間キューの項目の内容を、人間待ちと作業計画書の状態から決める。
//!
//! 本文とタイトルの形は `korylus-workdoc` の [`Item`] が決め、ここでは場面・理由・回答欄に載せる指摘を決める。
//! レビューの要人間判断の指摘の待ちでは、待ちを始めたときに写した人間の回答を待つ指摘を回答欄に載せる。
//! 写しの無い以前の待ちでは、最後のレビュードキュメントを読み、人間の回答を待つ指摘を載せる。

use jiff::civil::DateTime;
pub use korylus_workdoc::Error as FormError;
use korylus_workdoc::queue::Finding;
pub use korylus_workdoc::queue::{
    Choice, Field, FieldKind, Form, Item, PlanLink, Response, Scene, parse, send_back,
};

use crate::flow::{self, Error, Project};
use crate::queue::{Entry, State};
use crate::state::{PlanState, Wait, WaitKind};

/// プロジェクト名projectのプロジェクトで、計画書名nameの作業計画書が待ちwaitで人間を待つときの、項目の内容を返す。
///
/// stateは作業計画書の状態で、レビューの指摘の待ちでは、状態に写した指摘を回答の欄に載せる。
/// 写しの無い以前の待ちでは、プロジェクトのルートrootからレビュードキュメントを読む。
/// 項目を積んだ日時はatにする。
pub fn for_wait(
    project: &str,
    name: &str,
    state: &PlanState,
    wait: &Wait,
    root: &Project,
    at: DateTime,
) -> Result<Item, Error> {
    let scene = match wait.kind {
        WaitKind::HumanTask => Scene::HumanTask,
        WaitKind::Branch => Scene::Branch,
        WaitKind::Review => Scene::Review(findings(state, root)?),
        WaitKind::ReviewLimit => Scene::ReviewLimit,
        WaitKind::RetryLimit => Scene::RetryLimit,
        WaitKind::Question => Scene::Question,
        WaitKind::Report => Scene::Report,
    };
    Ok(Item {
        project: project.to_owned(),
        plan: PlanLink {
            name: name.to_owned(),
            title: state.title.clone(),
            url: Some(state.url.clone()),
        },
        scene,
        task: state.task.clone(),
        reason: wait.reason.clone(),
        details: wait.details.clone(),
        at,
    })
}

/// ロードマップの並びの先頭の作業計画書entryを読み取れないことを知らせる、項目の内容を返す。
pub fn plan_unreadable(project: &str, entry: &Entry, at: DateTime) -> Item {
    let (url, detail) = match &entry.state {
        State::NotFound => (None, "下書きもページも見つかりません".to_owned()),
        State::Invalid { url, reason } => (Some(url.clone()), reason.clone()),
        State::Ready { url, .. } => (Some(url.clone()), "読み取れました".to_owned()),
    };
    let title = &entry.candidate.link.title;
    Item {
        project: project.to_owned(),
        plan: PlanLink {
            name: title.clone(),
            title: title.clone(),
            url,
        },
        scene: Scene::PlanUnreadable,
        task: None,
        reason: format!(
            "ロードマップの「{}」の節の先頭の作業計画書を読み取れません。飛ばさずに、直されるのを待ちます",
            entry.candidate.section
        ),
        details: vec![detail],
        at,
    }
}

/// 計画書名nameの作業計画書 (状態state) がロードマップの完了とみなす節にあるが、ブランチが取り込まれていないことを知らせる、
/// 項目の内容を返す。branchesは、取り込まれていないブランチを1行ずつ書いたもの。
pub fn not_merged(
    project: &str,
    name: &str,
    state: &PlanState,
    branches: Vec<String>,
    at: DateTime,
) -> Item {
    Item {
        project: project.to_owned(),
        plan: PlanLink {
            name: name.to_owned(),
            title: state.title.clone(),
            url: Some(state.url.clone()),
        },
        scene: Scene::NotMerged,
        task: None,
        reason: "ロードマップの完了とみなす節にありますが、リモートの既定のベースに取り込まれていないブランチがあります".to_owned(),
        details: branches,
        at,
    }
}

/// レビューの要人間判断の指摘で待っている状態stateの、回答の欄に載せる指摘を返す。
///
/// 欄は、回答を照合するときと同じく、待ちを始めたときの指摘の写しから作る。
fn findings(state: &PlanState, root: &Project) -> Result<Vec<Finding>, Error> {
    Ok(flow::human_findings(state, root)?
        .into_iter()
        .map(|finding| Finding {
            title: finding.title,
            summary: finding.summary,
            options: finding.options,
        })
        .collect())
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::PathBuf;

    use super::*;
    use crate::queue::{Candidate, WikiLink};
    use crate::state::{ReviewRecord, ReviewTrail, Reviewer, Step, TaskRecord, Verdict};

    fn at() -> DateTime {
        "2026-11-17T21:04:05"
            .parse()
            .expect("日時を読み取れなかった")
    }

    fn waiting(kind: WaitKind) -> (PlanState, Wait) {
        let mut state = PlanState::new("https://wikino.app/s/shimbaco/pages/20", "デモ 作業計画書");
        state.task = Some("1-2".to_owned());
        let wait = Wait {
            kind,
            reason: "理由".to_owned(),
            details: vec!["詳細".to_owned()],
        };
        state.wait = Some(wait.clone());
        (state, wait)
    }

    fn project() -> (tempfile::TempDir, Project) {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let project = Project {
            root: root.path().to_path_buf(),
            repo: root.path().to_path_buf(),
        };
        (root, project)
    }

    #[test]
    fn 待ちの場面から項目の場面を決め_理由と詳細をそのまま載せる() {
        let (_root, project) = project();
        let cases = [
            (WaitKind::HumanTask, Scene::HumanTask),
            (WaitKind::Branch, Scene::Branch),
            (WaitKind::ReviewLimit, Scene::ReviewLimit),
            (WaitKind::RetryLimit, Scene::RetryLimit),
            (WaitKind::Question, Scene::Question),
            (WaitKind::Report, Scene::Report),
        ];
        for (kind, scene) in cases {
            let (state, wait) = waiting(kind);
            let item = for_wait("mewst", "demo", &state, &wait, &project, at())
                .expect("項目の内容を決められなかった");
            assert_eq!(
                item,
                Item {
                    project: "mewst".to_owned(),
                    plan: PlanLink {
                        name: "demo".to_owned(),
                        title: "デモ 作業計画書".to_owned(),
                        url: Some("https://wikino.app/s/shimbaco/pages/20".to_owned()),
                    },
                    scene,
                    task: Some("1-2".to_owned()),
                    reason: "理由".to_owned(),
                    details: vec!["詳細".to_owned()],
                    at: at(),
                }
            );
        }
    }

    #[test]
    fn レビューの指摘の待ちでは人間の回答を待つ指摘だけを回答欄に載せる() {
        let (_root, project) = project();
        let doc = PathBuf::from("docs/private/reviews/demo-1-2-001.md");
        let full = project.root.join(&doc);
        fs::create_dir_all(full.parent().expect("親ディレクトリが無い"))
            .expect("ディレクトリを作れなかった");
        fs::write(
            &full,
            "## ファイルごとのレビュー結果\n\n\
             ### 1. 名前を揃える\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 直す (推奨)\n- [ ] 現状維持\n\n\
             ### 2. 設計を変える\n\n処理を分けるかを決める。\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n\
             - [ ] 分ける (推奨)\n- [ ] 後続タスクに積む\n\n\
             ## 総合評価\n\n**評価**: Request Changes\n",
        )
        .expect("レビュードキュメントを書けなかった");
        let (mut state, wait) = waiting(WaitKind::Review);
        state.step = Some(Step::Fix);
        state.tasks.push(TaskRecord {
            id: "1-2".to_owned(),
            repo: project.repo.clone(),
            dir: project.repo.clone(),
            branch: "demo-1-2".to_owned(),
            base: "demo-1-1".to_owned(),
            prepared: true,
            review: ReviewTrail {
                reviews: vec![ReviewRecord {
                    number: 1,
                    role: Reviewer::Codex,
                    doc,
                    verdict: Verdict::RequestChanges,
                    at: "2026-11-17T12:00:00Z"
                        .parse()
                        .expect("日時を読み取れなかった"),
                }],
                ..ReviewTrail::default()
            },
            verification: Vec::new(),
        });

        let item = for_wait("mewst", "demo", &state, &wait, &project, at())
            .expect("項目の内容を決められなかった");
        assert_eq!(
            item.scene,
            Scene::Review(vec![Finding {
                title: "2. 設計を変える".to_owned(),
                summary: "処理を分けるかを決める。\n\n**区分**: 要人間判断".to_owned(),
                options: vec!["分ける (推奨)".to_owned(), "後続タスクに積む".to_owned()],
            }])
        );

        fs::remove_file(&full).expect("レビュードキュメントを消せなかった");
        let err = for_wait("mewst", "demo", &state, &wait, &project, at())
            .expect_err("レビュードキュメントが無いのに項目を作った");
        assert!(matches!(err, Error::ReadReview { .. }), "エラー = {err:?}");
    }

    #[test]
    fn 読み取れない先頭の作業計画書を知らせる() {
        let entry = |state| Entry {
            candidate: Candidate {
                section: "やってる".to_owned(),
                link: WikiLink {
                    topic: None,
                    title: "デモ 作業計画書".to_owned(),
                },
            },
            state,
        };
        let item = plan_unreadable(
            "mewst",
            &entry(State::Invalid {
                url: "https://wikino.app/s/shimbaco/pages/20".to_owned(),
                reason: "1行目: 計画書名がありません".to_owned(),
            }),
            at(),
        );
        assert_eq!(item.scene, Scene::PlanUnreadable);
        assert_eq!(
            item.plan.url.as_deref(),
            Some("https://wikino.app/s/shimbaco/pages/20")
        );
        assert_eq!(item.details, ["1行目: 計画書名がありません"]);
        assert!(
            item.reason
                .starts_with("ロードマップの「やってる」の節の先頭の作業計画書を読み取れません"),
            "理由 = {}",
            item.reason
        );

        let item = plan_unreadable("mewst", &entry(State::NotFound), at());
        assert_eq!(item.plan.url, None);
        assert_eq!(item.details, ["下書きもページも見つかりません"]);
    }

    #[test]
    fn 取り込まれていないブランチを知らせる() {
        let (state, _) = waiting(WaitKind::Report);
        let item = not_merged(
            "mewst",
            "demo",
            &state,
            vec!["/korylus-tools demo-1-2 (main)".to_owned()],
            at(),
        );
        assert_eq!(item.scene, Scene::NotMerged);
        assert_eq!(item.task, None, "タスクに依らない知らせにタスクを書いた");
        assert_eq!(item.details, ["/korylus-tools demo-1-2 (main)"]);
        assert_eq!(item.plan.name, "demo");
    }
}
