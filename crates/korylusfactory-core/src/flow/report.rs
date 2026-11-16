//! 作業報告書を書くステップと、作業報告書の確認の人間待ちと、作業計画書の完了の記録。

use jiff::Timestamp;
use korylus_workdoc::plan;

use super::review::{self, Target};
use super::{Error, Next, PlanDocument, Project, clear, describe_violation, start, wait};
use crate::state::{PlanState, ReportRound, ReviewTrail, Step, Wait, WaitKind};

/// 作業者が作業報告書を書いたあとの、セッションの結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reported {
    /// 作業報告書のページのURL。
    pub location: String,
    /// 処理の流れを書かなかった理由。書いたときは `None`。
    pub walkthrough_skipped: Option<String>,
}

/// 作業報告書の記録reportedを確かめて次の回として記録し、作業報告書のレビューか、作業報告書の確認の人間待ちに進める。
///
/// 処理の流れを書いたときだけ、コードと照合するレビューを挟む。
/// 記録が誤っていれば、状態を変えずに [`Error::Rejected`] で理由を返す。
/// 現在の日時nowを、作業報告書を記録した時刻にする。
pub fn finish_report(
    state: &mut PlanState,
    doc: &PlanDocument,
    project: &Project,
    reported: Reported,
    now: Timestamp,
) -> Result<(), Error> {
    if state.step != Some(Step::Report) {
        return Err(Error::WrongStep {
            expected: Step::Report,
            actual: state.step,
        });
    }
    let location = reported.location.trim().to_owned();
    let skipped = reported
        .walkthrough_skipped
        .map(|reason| reason.trim().to_owned());
    let mut reasons = Vec::new();
    if location.is_empty() {
        reasons.push("location に、作業報告書のURLを書いてください".to_owned());
    } else if !location.starts_with("https://") {
        reasons.push(format!(
            "location ({location}) は、https:// で始まる作業報告書のURLにしてください"
        ));
    }
    if skipped.as_deref() == Some("") {
        reasons.push("walkthrough_skipped に、処理の流れを書かなかった理由を書いてください。書いたときは walkthrough_skipped を渡さないでください".to_owned());
    }
    if !reasons.is_empty() {
        return Err(Error::Rejected(reasons));
    }

    let reviewed = skipped.is_none();
    let review = if reviewed {
        // 回をまたいで同じ接頭辞にし、前の回のレビューを修正検証の対象にする。
        ReviewTrail {
            review_prefix: Some(format!("{}-report", doc.name()?)),
            ..ReviewTrail::default()
        }
    } else {
        ReviewTrail::default()
    };
    let number = u32::try_from(state.report_rounds.len())
        .unwrap_or(u32::MAX)
        .saturating_add(1);
    state.report_rounds.push(ReportRound {
        number,
        at: now,
        location,
        walkthrough_skipped: skipped,
        review,
    });
    if reviewed {
        return review::start_review(state, project, &Target::Report);
    }
    wait_report(state);
    Ok(())
}

/// 作業報告書の確認を待っている作業計画書の完了 (マージ済み) を、現在の日時nowで記録する。
///
/// 完了したかの確認 (ロードマップの完了とみなす節にあり、ブランチが取り込まれている) は、呼び出し側が行う。
pub fn complete(state: &mut PlanState, now: Timestamp) -> Result<(), Error> {
    if !matches!(
        state.wait,
        Some(Wait {
            kind: WaitKind::Report,
            ..
        })
    ) {
        return Err(Error::NotWaitingReport);
    }
    clear(state);
    state.completed_at = Some(now);
    Ok(())
}

/// 状態stateを、作業者が作業報告書を書くステップにする。
///
/// 作業報告書の確認のあとに、人間がフィードバック対応のフェーズを足しただけかを確かめるため、作業計画書docの本文を残す。
pub(super) fn start_report(state: &mut PlanState, doc: &PlanDocument) {
    state.snapshot = Some(doc.body().to_owned());
    start(state, None, Step::Report);
}

/// 作業報告書を書くステップで、次に行うことを返す。
pub(super) fn next_report(state: &PlanState) -> Next {
    Next::Report {
        previous: state.latest_report().map(str::to_owned),
    }
}

/// 状態stateを、最新の回の作業報告書を人間が確認するのを待つ状態にする。
pub(super) fn wait_report(state: &mut PlanState) {
    let mut details = Vec::new();
    if let Some(url) = state.latest_report() {
        details.push(format!("作業報告書: {url}"));
    }
    details.push("フィードバックがあれば、/korylus-add-feedback-phase で作業計画書にフィードバック対応のフェーズを足してから再開してください".to_owned());
    details.push("フィードバックが無ければ、PRを作ってマージし、ロードマップで作業計画書の項目を完了とみなす節へ移してください".to_owned());
    state.task = None;
    wait(
        state,
        None,
        WaitKind::Report,
        "作業報告書の確認を待っています".to_owned(),
        details,
    );
}

/// 作業報告書の確認の人間待ちから再開する前に、人間が作業計画書docにフィードバック対応のフェーズを足しただけかを確かめる。
pub(super) fn check_feedback(state: &PlanState, doc: &PlanDocument) -> Result<(), Error> {
    let snapshot = state.snapshot.as_deref().ok_or(Error::NoReportSnapshot)?;
    plan::check_feedback_phase(snapshot, doc.body())
        .map(|_| ())
        .map_err(|violations| {
            let mut reasons = vec![
                "作業報告書の確認のあとは、作業計画書のタスクリストの末尾にフィードバック対応のフェーズを足すことだけをしてから再開してください".to_owned(),
            ];
            reasons.extend(violations.iter().map(describe_violation));
            Error::Rejected(reasons)
        })
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::path::PathBuf;

    use super::super::tests::{PLAN, checked, doc, implementing, new_state, project, rejected};
    use super::super::{
        DEFAULT_REVIEW_LIMIT, finish_fix, finish_report_fix, finish_review, next, resume,
    };
    use super::*;
    use crate::state::Reviewer;

    /// 作業報告書のURL。
    const URL: &str = "https://wikino.app/s/shimbaco/pages/2940";

    /// PLANの末尾に足す、フィードバック対応のフェーズfb1。
    const FB1: &str = "\n### フェーズfb1: フィードバック対応\n\n- [ ] **fb1-1**: [/korylus-tools] 表示を直す\n  - **想定行数**: 約10行 (実装10行)\n";

    /// 自動対応可で、直す選択肢を推奨する指摘。
    const AUTO_FIX: &str = "### 1. コードの抜粋を直す\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正案の通り直す (推奨)\n- [ ] 現状維持\n\n";

    /// 要人間判断の指摘。
    const NEEDS_HUMAN: &str = "### 2. 処理の流れの節を組み替える\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [ ] 節を組み替える (推奨)\n- [ ] 現状維持\n\n";

    fn now() -> Timestamp {
        "2026-11-16T07:00:00Z"
            .parse()
            .expect("日時を読み取れなかった")
    }

    /// PLANのタスクにすべてチェックを付けた作業計画書の本文。
    fn all_done() -> String {
        checked(&["1-1", "1-2", "1-3"])
    }

    /// すべてのタスクを終え、作業報告書を書くステップに進めた状態。
    fn reporting() -> PlanState {
        let mut state = new_state();
        next(&mut state, &doc(&all_done())).expect("次に行うことを決められなかった");
        state
    }

    fn reported(skipped: Option<&str>) -> Reported {
        Reported {
            location: URL.to_owned(),
            walkthrough_skipped: skipped.map(str::to_owned),
        }
    }

    /// 作業報告書を処理の流れを書かずに記録し、作業報告書の確認を待っている状態。
    fn waiting_report(project: &Project) -> PlanState {
        let mut state = reporting();
        finish_report(
            &mut state,
            &doc(&all_done()),
            project,
            reported(Some("ガイドラインの文面だけを直したため")),
            now(),
        )
        .expect("作業報告書の記録を受け付けなかった");
        state
    }

    /// 作業報告書のレビュー接頭辞のnumber番目のレビュードキュメントを、総合評価verdictと指摘findingsで書き、相対パスを返す。
    fn write_review(project: &Project, number: u32, verdict: &str, findings: &str) -> PathBuf {
        let path = PathBuf::from(format!(
            "{}/demo-report-{number:03}.md",
            korylus_workdoc::review::DIR
        ));
        let full = project.root.join(&path);
        fs::create_dir_all(full.parent().expect("親ディレクトリが無い"))
            .expect("ディレクトリを作れなかった");
        fs::write(
            &full,
            format!(
                "# レビュー: 作業報告書\n\n## ファイルごとのレビュー結果\n\n{findings}## 総合評価\n\n**評価**: {verdict}\n"
            ),
        )
        .expect("レビュードキュメントを書けなかった");
        path
    }

    fn wait_kind(state: &PlanState) -> Option<WaitKind> {
        state.wait.as_ref().map(|wait| wait.kind)
    }

    #[test]
    fn タスクがすべて完了したら作業報告書を書くステップに進み_作業計画書を残す() {
        let mut state = new_state();
        let body = all_done();
        assert_eq!(
            next(&mut state, &doc(&body)).expect("次に行うことを決められなかった"),
            Next::Report { previous: None }
        );
        assert_eq!(
            (state.task.as_deref(), state.step),
            (None, Some(Step::Report))
        );
        assert_eq!(
            state.snapshot.as_deref(),
            Some(body.as_str()),
            "作業計画書のスナップショットを残していない"
        );
    }

    #[test]
    fn 処理の流れを書いた作業報告書は記録してcodexがレビューする() {
        let (_root, project) = project();
        let mut state = reporting();
        let plan = doc(&all_done());
        finish_report(&mut state, &plan, &project, reported(None), now())
            .expect("作業報告書の記録を受け付けなかった");

        assert_eq!(
            state.report_rounds,
            [ReportRound {
                number: 1,
                at: now(),
                location: URL.to_owned(),
                walkthrough_skipped: None,
                review: ReviewTrail {
                    review_prefix: Some("demo-report".to_owned()),
                    ..ReviewTrail::default()
                },
            }],
            "作業報告書の回の記録が期待と異なる"
        );
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::ReportReview {
                url: URL.to_owned(),
                reviewer: Reviewer::Codex,
                number: 1,
                prefix: "demo-report".to_owned(),
            }
        );
    }

    #[test]
    fn 処理の流れを書かなかった作業報告書はレビューせずに確認を待つ() {
        let (_root, project) = project();
        let mut state = waiting_report(&project);
        let wait = state.wait.clone().expect("人間を待っていない");
        assert_eq!(wait.kind, WaitKind::Report, "人間を待つ場面が違う");
        assert_eq!(wait.details[0], format!("作業報告書: {URL}"));
        assert_eq!(
            next(&mut state, &doc(&all_done())).expect("次に行うことを決められなかった"),
            Next::Wait(wait),
            "作業報告書の確認を待たずに先へ進んだ"
        );
    }

    #[test]
    fn 作業報告書の記録が誤っていれば状態を変えずに理由を返す() {
        let (_root, project) = project();
        let cases = [
            (
                "URLが空",
                "",
                None,
                "location に、作業報告書のURLを書いてください",
            ),
            (
                "httpsではない",
                "http://wikino.app/s/shimbaco/pages/1",
                None,
                "location (http://wikino.app/s/shimbaco/pages/1) は、https:// で始まる作業報告書のURLにしてください",
            ),
            (
                "書かなかった理由が空",
                URL,
                Some("  "),
                "walkthrough_skipped に、処理の流れを書かなかった理由を書いてください。書いたときは walkthrough_skipped を渡さないでください",
            ),
        ];
        for (name, location, skipped, want) in cases {
            let mut state = reporting();
            let before = state.clone();
            let result = finish_report(
                &mut state,
                &doc(&all_done()),
                &project,
                Reported {
                    location: location.to_owned(),
                    walkthrough_skipped: skipped.map(str::to_owned),
                },
                now(),
            );
            assert_eq!(rejected(result), [want], "{name}");
            assert_eq!(state, before, "{name}: 受け付けなかった記録で状態を変えた");
        }
    }

    #[test]
    fn 作業報告書のレビューで修正が要れば下書きを直し_直したら確認を待つ() {
        let (_root, project) = project();
        let mut state = reporting();
        let plan = doc(&all_done());
        finish_report(&mut state, &plan, &project, reported(None), now())
            .expect("作業報告書の記録を受け付けなかった");
        let path = write_review(&project, 1, "Comment", AUTO_FIX);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::ReportFix {
                url: URL.to_owned(),
                reviewer: Reviewer::Codex,
                doc: path,
            }
        );

        let fixed = crate::flow::Fixed {
            branch: "demo-1-1".to_owned(),
            dirty: Vec::new(),
        };
        let err = finish_fix(&mut state, &plan, &project, &fixed)
            .expect_err("作業報告書の修正をタスクの修正として記録した");
        assert!(matches!(err, Error::ReportInProgress), "エラー = {err:?}");

        finish_report_fix(&mut state, &project).expect("作業報告書の修正の完了を受け付けなかった");
        assert_eq!(
            wait_kind(&state),
            Some(WaitKind::Report),
            "作業報告書の確認を待っていない"
        );
    }

    #[test]
    fn 作業報告書のレビューに指摘が無ければ確認を待つ() {
        let (_root, project) = project();
        let mut state = reporting();
        finish_report(
            &mut state,
            &doc(&all_done()),
            &project,
            reported(None),
            now(),
        )
        .expect("作業報告書の記録を受け付けなかった");
        write_review(&project, 1, "Approve", "");
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            wait_kind(&state),
            Some(WaitKind::Report),
            "作業報告書の確認を待っていない"
        );
        assert_eq!(
            state.report_rounds[0].review.reviews.len(),
            1,
            "レビューを記録していない"
        );
    }

    #[test]
    fn 作業報告書のレビューに要人間判断の指摘があれば人間を待ち_回答してから再開すると下書きを直す()
    {
        let (_root, project) = project();
        let mut state = reporting();
        let plan = doc(&all_done());
        finish_report(&mut state, &plan, &project, reported(None), now())
            .expect("作業報告書の記録を受け付けなかった");
        let path = write_review(&project, 1, "Comment", NEEDS_HUMAN);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            wait_kind(&state),
            Some(WaitKind::Review),
            "要人間判断の指摘で人間を待っていない"
        );

        let full = project.root.join(&path);
        let answered = fs::read_to_string(&full)
            .expect("レビュードキュメントを読めなかった")
            .replace("- [ ] 節を組み替える (推奨)", "- [x] 節を組み替える (推奨)");
        fs::write(&full, answered).expect("回答を書けなかった");
        resume(&mut state, &plan, &project, None).expect("回答してから再開できなかった");
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::ReportFix {
                url: URL.to_owned(),
                reviewer: Reviewer::Codex,
                doc: path,
            },
            "再開したあとに作業報告書の修正へ進まなかった"
        );

        finish_report_fix(&mut state, &project).expect("作業報告書の修正の完了を受け付けなかった");
        assert_eq!(
            wait_kind(&state),
            Some(WaitKind::Report),
            "修正のあとに作業報告書の確認を待っていない"
        );
    }

    #[test]
    fn 作業報告書のレビューが上限に達してもrequest_changesなら人間を待ち_再開すると修正のあとに確認を待つ()
     {
        let (_root, project) = project();
        let mut state = reporting();
        let plan = doc(&all_done());
        finish_report(&mut state, &plan, &project, reported(None), now())
            .expect("作業報告書の記録を受け付けなかった");
        let mut reviewers = Vec::new();
        let mut last = PathBuf::new();
        for number in 1..=DEFAULT_REVIEW_LIMIT {
            match next(&mut state, &plan).expect("次に行うことを決められなかった") {
                Next::ReportReview { reviewer, .. } => reviewers.push(reviewer),
                other => panic!("{number}回目のレビューに進まなかった: {other:?}"),
            }
            last = write_review(&project, number, "Request Changes", AUTO_FIX);
            finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
            if number < DEFAULT_REVIEW_LIMIT {
                finish_report_fix(&mut state, &project)
                    .expect("作業報告書の修正の完了を受け付けなかった");
            }
        }
        assert_eq!(
            reviewers,
            [Reviewer::Codex, Reviewer::Claude, Reviewer::Codex],
            "レビュアーを交互に替えていない"
        );
        let wait = state.wait.clone().expect("人間を待っていない");
        assert_eq!(wait.kind, WaitKind::ReviewLimit, "人間を待つ場面が違う");
        assert_eq!(
            wait.details[1],
            "再開すると、回答と修正のあと、再レビューせずに作業報告書のレビューを終えます。上限を延ばして再開すると、修正のあとに再レビューします"
        );

        resume(&mut state, &plan, &project, None).expect("再開できなかった");
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::ReportFix {
                url: URL.to_owned(),
                reviewer: Reviewer::Codex,
                doc: last,
            },
            "再開したあとに作業報告書の修正へ進まなかった"
        );
        finish_report_fix(&mut state, &project).expect("作業報告書の修正の完了を受け付けなかった");
        assert_eq!(
            wait_kind(&state),
            Some(WaitKind::Report),
            "上限のあとの修正で再レビューに進んだ"
        );
    }

    #[test]
    fn タスクの修正を進めているときは作業報告書の修正の完了として記録しない() {
        let (_root, project) = project();
        // タスク1-1のレビューのあと、修正を進めている状態にする。
        let mut state = implementing();
        state.step = Some(Step::Fix);
        let before = state.clone();
        let err = finish_report_fix(&mut state, &project)
            .expect_err("タスクの修正を作業報告書の修正として記録した");
        assert!(
            matches!(&err, Error::TaskInProgress(id) if id == "1-1"),
            "エラー = {err:?}"
        );
        assert_eq!(state, before, "受け付けなかった記録で状態を変えた");
    }

    #[test]
    fn フィードバック対応のフェーズを足して再開するとそのタスクから進め_次の作業報告書は前の回を更新する()
     {
        let (_root, project) = project();
        let mut state = waiting_report(&project);
        let with_fb = format!("{}{FB1}", all_done());
        resume(&mut state, &doc(&with_fb), &project, None).expect("再開できなかった");
        assert_eq!(state.wait, None, "人間待ちを解いていない");
        assert!(
            matches!(
                next(&mut state, &doc(&with_fb)).expect("次に行うことを決められなかった"),
                Next::Prepare { task, .. } if task.id == "fb1-1"
            ),
            "フィードバック対応のタスクに進まなかった"
        );

        // フィードバック対応のタスクを終えたものとして、次に行うことを尋ね直す。
        state.task = None;
        state.step = None;
        let fb_done = with_fb.replace("- [ ] **fb1-1**", "- [x] **fb1-1**");
        assert_eq!(
            next(&mut state, &doc(&fb_done)).expect("次に行うことを決められなかった"),
            Next::Report {
                previous: Some(URL.to_owned())
            },
            "次の回の作業報告書で、前の回のページを渡さなかった"
        );
    }

    #[test]
    fn フィードバック対応のフェーズを足す以外の変更では再開しない() {
        let (_root, project) = project();
        let cases = [
            ("何も足していない", all_done()),
            (
                "設計を書き換えた",
                format!("{}{FB1}", all_done())
                    .replace("状態ファイルを作る。", "設計を書き換えた。"),
            ),
        ];
        for (name, body) in cases {
            let mut state = waiting_report(&project);
            let before = state.clone();
            let reasons = rejected(resume(&mut state, &doc(&body), &project, None));
            assert_eq!(
                reasons[0],
                "作業報告書の確認のあとは、作業計画書のタスクリストの末尾にフィードバック対応のフェーズを足すことだけをしてから再開してください",
                "{name}"
            );
            assert!(
                reasons.len() > 1,
                "{name}: 受け付けなかった変更を挙げていない"
            );
            assert_eq!(state, before, "{name}: 再開できないのに状態を変えた");
        }
    }

    #[test]
    fn 作業報告書の確認を待っている作業計画書の完了を記録する() {
        let (_root, project) = project();
        let mut state = waiting_report(&project);
        complete(&mut state, now()).expect("完了を記録できなかった");
        assert_eq!(
            state.completed_at,
            Some(now()),
            "完了した時刻を記録していない"
        );
        assert_eq!(state.wait, None, "人間待ちを解いていない");
        assert_eq!(
            next(&mut state, &doc(PLAN)).expect("次に行うことを決められなかった"),
            Next::Finished,
            "完了した作業計画書を進めた"
        );
    }

    #[test]
    fn 作業報告書の確認を待っていなければ完了を記録しない() {
        let mut state = reporting();
        let err = complete(&mut state, now()).expect_err("完了を記録した");
        assert!(matches!(err, Error::NotWaitingReport), "エラー = {err:?}");
        assert_eq!(state.completed_at, None, "完了した時刻を記録した");
    }
}
