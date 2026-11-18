//! タスクと作業報告書のレビュー・回答・修正と、人間待ちからの再開の状態遷移。
//!
//! レビュードキュメントの対応方針には、自動対応可の指摘だけを推奨の選択肢で回答し、
//! 要人間判断の指摘と再検討提案は、人間が回答するまで待つ。

use std::fs;
use std::path::{Path, PathBuf};

use jiff::Timestamp;
use korylus_workdoc::queue::{FieldKind, Form, Response};
use korylus_workdoc::review::{self, Answers, Category, Document, Finding};

use super::report;
use super::{
    Error, Next, PlanDocument, Project, checkout_reason, clear, current, dirty_reason,
    plan_reasons, start, wait,
};
use crate::state::{
    DeferredFinding, HumanFinding, HumanReview, PlanState, ReviewRecord, ReviewTrail, Reviewer,
    Step, Verdict, WaitKind,
};

/// 人間キューの項目では選べない、選択肢に無い対応を表す選択肢の書き出し。
///
/// 対応の文はレビュードキュメントの回答の欄に書くもので、項目には書く欄が無いためである。
const OTHER: &str = "その他";

/// 人間が延ばしていないときの、1つのタスクか作業報告書の1回で行うレビューの上限の回数。
pub const DEFAULT_REVIEW_LIMIT: u32 = 3;

/// レビュアーが修正を終えたあとの、タスクのリポジトリの状態。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Fixed {
    /// リポジトリで切り替えているブランチ。
    pub branch: String,
    /// 未コミットの変更 (`git status --porcelain` の形)。作業計画書などのコミットしないパスは除く。
    pub dirty: Vec<String>,
}

/// レビューの対象。
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) enum Target {
    /// タスク番号のタスク。
    Task(String),
    /// 最新の回の作業報告書。
    Report,
}

impl Target {
    /// 対象を、エラーの文面に入れる形にする。
    fn label(&self) -> String {
        match self {
            Target::Task(id) => format!("タスク {id} "),
            Target::Report => "作業報告書".to_owned(),
        }
    }

    /// 対象のステップを進めるときの、進めているタスクの番号を返す。作業報告書なら `None`。
    fn task(&self) -> Option<&str> {
        match self {
            Target::Task(id) => Some(id),
            Target::Report => None,
        }
    }
}

/// number回目 (1始まり) のレビューを行うレビュアーを返す。
///
/// 修正したモデルが同じ変更を再レビューしないよう、Codexから始めて交互に替える。
pub fn reviewer_for(number: u32) -> Reviewer {
    if number % 2 == 1 {
        Reviewer::Codex
    } else {
        Reviewer::Claude
    }
}

/// レビューの完了を確かめてレビューを記録し、対応方針に回答する。
///
/// 回答のあとは、修正・再レビュー・人間待ち・レビューの終わりのどれかに進める。
/// タスクのレビューを終えたら次のタスクへ、作業報告書のレビューを終えたら作業報告書の確認の人間待ちへ進める。
/// レビュードキュメントの不備はレビュアーが直せるため、状態を変えずに [`Error::Rejected`] で理由を返す。
/// 現在の日時nowを、レビューを記録した時刻にする。
pub fn finish_review(
    state: &mut PlanState,
    project: &Project,
    now: Timestamp,
) -> Result<(), Error> {
    let target = target(state, Step::Review)?;
    let prefix = trail(state, &target)?
        .review_prefix
        .clone()
        .ok_or_else(|| Error::NoReviewPrefix(target.label()))?;
    let before = match &state.review_before {
        Some(before) => sequence(&prefix, before)?,
        None => 0,
    };
    let latest = latest(project, &prefix)?;
    let newer = match &latest {
        Some(path) => sequence(&prefix, path)? > before,
        None => false,
    };
    let Some(path) = latest.filter(|_| newer) else {
        return Err(Error::Rejected(vec![format!(
            "{}/{prefix}-NNN.md に、このレビューのレビュードキュメント (前回より新しい連番) がありません",
            review::DIR
        )]));
    };

    let document = read_review(project, &path)?;
    // チェック済みの選択肢は人間の回答として扱うため、レビュアーが入れたチェックで要人間判断の指摘を素通りさせない。
    let checked: Vec<String> = document
        .checked()
        .into_iter()
        .filter(|finding| !checked_as_recommended(finding))
        .map(Finding::describe)
        .collect();
    if !checked.is_empty() {
        let mut reasons = vec![format!(
            "{} で、レビュアーが対応方針の選択肢にチェックを入れています。チェックを外してください (回答はkorylusfactoryが行います)",
            path.display()
        )];
        reasons.extend(checked);
        return Err(Error::Rejected(reasons));
    }
    let answers = document.answer();
    check_invalid(&path, &answers)?;

    let trail = trail_mut(state, &target)?;
    let number = count(trail).saturating_add(1);
    trail.reviews.push(ReviewRecord {
        number,
        role: reviewer_for(number),
        doc: path.clone(),
        verdict: document.verdict,
        at: now,
    });
    let limit = limit(trail);
    if document.verdict == Verdict::RequestChanges && number >= limit {
        let end = match target {
            Target::Task(_) => "次のタスクへ進みます",
            Target::Report => "作業報告書のレビューを終えます",
        };
        wait(
            state,
            Some(Step::Fix),
            WaitKind::ReviewLimit,
            format!("レビューが上限の{limit}回に達しても Request Changes でした"),
            vec![
                format!("レビュードキュメント: {}", path.display()),
                format!(
                    "再開すると、回答と修正のあと、再レビューせずに{end}。上限を延ばして再開すると、修正のあとに再レビューします"
                ),
            ],
        );
        return Ok(());
    }
    apply(state, project, &target, &path, &answers)
}

/// タスクの修正の完了を確かめ、満たしていれば再レビューに進めるか、タスクを終える。
///
/// 修正の完了の条件は、タスクのブランチにいて、未コミットの変更が無く、作業計画書で変わったのが変えてよい範囲だけであること。
/// 条件を満たさなければ、状態を変えずに [`Error::Rejected`] で理由を返す。
pub fn finish_fix(
    state: &mut PlanState,
    doc: &PlanDocument,
    project: &Project,
    fixed: &Fixed,
) -> Result<(), Error> {
    if let Some(wait) = &state.wait {
        return Err(Error::Waiting(wait.kind));
    }
    let id = match target(state, Step::Fix)? {
        Target::Task(id) => id,
        Target::Report => return Err(Error::ReportInProgress),
    };
    let task = doc.task(&id)?;
    let record = state
        .task_record(&id)
        .ok_or_else(|| Error::MissingRecord(id.clone()))?;
    let mut reasons: Vec<String> = checkout_reason(record, &fixed.branch)
        .into_iter()
        .chain(dirty_reason(record, &fixed.dirty))
        .collect();
    reasons.extend(plan_reasons(state, doc, task)?);
    if !reasons.is_empty() {
        return Err(Error::Rejected(reasons));
    }
    after_fix(state, project, &Target::Task(id))
}

/// 作業報告書の修正の完了を記録し、再レビューに進めるか、作業報告書の確認の人間待ちに進める。
///
/// 作業報告書の修正は下書きを書き換えるだけでコミットしないため、リポジトリの状態を確かめない。
pub fn finish_report_fix(state: &mut PlanState, project: &Project) -> Result<(), Error> {
    if let Some(wait) = &state.wait {
        return Err(Error::Waiting(wait.kind));
    }
    match target(state, Step::Fix)? {
        Target::Task(id) => Err(Error::TaskInProgress(id)),
        Target::Report => after_fix(state, project, &Target::Report),
    }
}

/// 人間待ちを解き、待っていた場面に応じて状態を進める。
///
/// 人が行うタスクとタスクの準備の待ちは、次に [`next`](super::next) を呼んだときに、作業計画書の最初の未完了のタスクから選び直す。
/// レビューの待ちは、人間が書いた回答を読んで対応方針に回答し直し、修正・再レビュー・レビューの終わりのどれかに進める。
/// 作業報告書の確認の待ちは、作業計画書docにフィードバック対応のフェーズが足されただけであることを確かめ、そのタスクから進める。
/// やり直しの上限とセッションからの質問の待ちは、待つ前のステップを新しいセッションでやり直す。
/// review_limitを渡すと、レビューの上限で待っているタスクか作業報告書の上限を、その回数に延ばす。
/// 再開できないときは、状態を変えずにエラーを返す。
pub fn resume(
    state: &mut PlanState,
    doc: &PlanDocument,
    project: &Project,
    review_limit: Option<u32>,
) -> Result<(), Error> {
    let kind = state.wait.as_ref().ok_or(Error::NotWaiting)?.kind;
    if review_limit.is_some() && kind != WaitKind::ReviewLimit {
        return Err(Error::Rejected(vec![
            "レビューの上限は、上限に達して人間を待っているときだけ延ばせます".to_owned(),
        ]));
    }
    let mut resumed = state.clone();
    resumed.wait = None;
    resumed.notified = None;
    match kind {
        WaitKind::HumanTask | WaitKind::Branch | WaitKind::RetryLimit | WaitKind::Question => {}
        WaitKind::Report => report::check_feedback(&resumed, doc)?,
        WaitKind::Review | WaitKind::ReviewLimit => {
            let target = target(&resumed, Step::Fix)?;
            if let Some(new_limit) = review_limit {
                let trail = trail_mut(&mut resumed, &target)?;
                let (old, done) = (limit(trail), count(trail));
                if new_limit <= old || new_limit <= done {
                    return Err(Error::Rejected(vec![format!(
                        "新しいレビューの上限には、今の上限の{old}回と、行ったレビューの{done}回を超える回数を指定してください"
                    )]));
                }
                trail.review_limit = Some(new_limit);
            }
            let path = last_review(&resumed, &target)?.doc.clone();
            let answers = read_review(project, &path)?.answer();
            check_invalid(&path, &answers)?;
            // 上限で待ったあとは、続けて要人間判断の指摘で待ってよい。
            if kind == WaitKind::Review && !answers.pending.is_empty() {
                let mut reasons = vec![format!(
                    "{} に、まだ対応方針に回答の無い指摘があります",
                    path.display()
                )];
                reasons.extend(answers.pending.iter().map(Finding::describe));
                return Err(Error::Rejected(reasons));
            }
            apply(&mut resumed, project, &target, &path, &answers)?;
        }
    }
    *state = resumed;
    Ok(())
}

/// レビューの要人間判断の指摘で待っている状態stateについて、人間キューの項目の回答の欄に載せる指摘を、欄の順に返す。
///
/// 待ちを始めたときの指摘の写しがあれば、回答を照合する ([`choose_findings`]) のと同じ写しを返す。
/// 回答をレビュードキュメントに書いたあと差し戻した待ちで項目を積み直しても、欄が照合する指摘と揃うようにするためである。
/// 写しの無い以前の待ちでは、最後のレビュードキュメントで人間の回答を待つ指摘を返す。
pub(crate) fn human_findings(
    state: &PlanState,
    project: &Project,
) -> Result<Vec<HumanFinding>, Error> {
    let target = target(state, Step::Fix)?;
    if let Some(saved) = &trail(state, &target)?.human_review {
        return Ok(saved.findings.clone());
    }
    let path = last_review(state, &target)?.doc.clone();
    Ok(read_review(project, &path)?
        .answer()
        .pending
        .iter()
        .map(HumanFinding::from)
        .collect())
}

/// 要人間判断の指摘で待っている状態stateについて、人間キューの項目の回答の欄formで選んだ選択肢を、最後のレビュードキュメントに書く。
///
/// 待ちを始めたときの指摘を照合し、欄の見出しと選択肢の数・本文が、その指摘と同じであることを確かめる。
/// すでにチェックを書いた指摘も対象に含め、状態保存前の中断や差し戻しのあとに回答を適用し直せる。
/// 確かめられないときと、選択肢に無い対応 (「その他」) を選んだときは、書かずに [`Error::Rejected`] で理由を返す。
/// 回答の欄は、[`Form::answers`] で形を確かめてから渡す。
pub(super) fn choose_findings(
    state: &PlanState,
    project: &Project,
    form: &Form,
    responses: &[Response],
) -> Result<(), Error> {
    let target = target(state, Step::Fix)?;
    let path = last_review(state, &target)?.doc.clone();
    let document = read_review(project, &path)?;
    let pending = answer_findings(state, &target, &path, &document)?;
    if form.fields.len() != pending.len() {
        return Err(Error::Rejected(vec![format!(
            "回答の欄が{}個ありますが、レビュードキュメント {} で人間の回答を待つ指摘は{}個です",
            form.fields.len(),
            path.display(),
            pending.len()
        )]));
    }
    let mut choices = Vec::new();
    let mut reasons = Vec::new();
    for (i, ((field, finding), response)) in form
        .fields
        .iter()
        .zip(pending.iter().copied())
        .zip(responses)
        .enumerate()
    {
        let heading = format!("指摘{}: {}", i + 1, finding.title);
        if !field.heading_matches(&heading) {
            reasons.push(format!(
                "回答の欄「{}」が、元の指摘の欄「{heading}」と違います。欄の順番と見出しを元に戻してください",
                field.heading
            ));
            continue;
        }
        let matched = match &field.kind {
            FieldKind::Choices(options) => {
                options.len() == finding.options.len()
                    && options
                        .iter()
                        .zip(&finding.options)
                        .all(|(option, original)| option.matches(&original.text))
            }
            FieldKind::Text(_) => false,
        };
        let chosen = match response {
            Response::Choice(chosen) if matched => *chosen,
            _ => {
                reasons.push(format!(
                    "「{}」の選択肢が、レビュードキュメント {} の指摘「{}」の選択肢と違います。選択肢を書き換えずに、1つにチェックを付けてください",
                    field.heading,
                    path.display(),
                    finding.title
                ));
                continue;
            }
        };
        if finding.options[chosen].text.starts_with(OTHER) {
            reasons.push(format!(
                "「{}」: 「{}」を選んだときの対応の文を書く欄が、項目にありません。ほかの選択肢を選ぶか、レビュードキュメント {} の回答の欄に対応を書いて korylusfactory resume で再開してください",
                field.heading,
                finding.options[chosen].text,
                path.display()
            ));
            continue;
        }
        choices.push((finding, chosen));
    }
    if !reasons.is_empty() {
        return Err(Error::Rejected(reasons));
    }
    let full = project.root.join(&path);
    fs::write(&full, document.choose(&choices))
        .map_err(|source| Error::WriteReview { path: full, source })
}

/// 待ちを始めたときの対象に対応する、今のドキュメントdocumentの指摘を返す。
///
/// 回答済みかどうかで対象を取り直さない。指摘の場所・本文・選択肢が変わっていれば、書き込まずに差し戻す。
/// 以前の状態ファイルでは、待ちの詳細に保存済みの場所・見出し・区分で照合する。
fn answer_findings<'a>(
    state: &PlanState,
    target: &Target,
    path: &Path,
    document: &'a Document,
) -> Result<Vec<&'a Finding>, Error> {
    let saved = trail(state, target)?.human_review.as_ref();
    let changed = || {
        Error::Rejected(vec![format!(
            "レビュードキュメント {} の回答対象が、人間待ちを始めたときと違います。指摘を元に戻すか、レビュードキュメントに回答して korylusfactory resume で再開してください",
            path.display()
        )])
    };
    if let Some(saved) = saved {
        if saved.doc != path {
            return Err(changed());
        }
        return saved
            .findings
            .iter()
            .map(|original| {
                document
                    .findings
                    .iter()
                    .find(|finding| original.matches(finding))
                    .ok_or_else(changed)
            })
            .collect();
    }

    // スナップショットが無い以前の待ちでも、未回答の指摘だけを取り直すと中断後に再適用できなくなる。
    let details = state
        .wait
        .as_ref()
        .ok_or(Error::NotWaiting)?
        .details
        .iter()
        .skip(1);
    if details.len() == 0 {
        return Err(changed());
    }
    details
        .map(|description| {
            document
                .findings
                .iter()
                .find(|finding| finding.describe() == *description)
                .ok_or_else(changed)
        })
        .collect()
}

/// レビューの上限で待っている状態stateについて、上限を1回延ばした回数を返す。
pub(super) fn extended_limit(state: &PlanState) -> Result<u32, Error> {
    let target = target(state, Step::Fix)?;
    let trail = trail(state, &target)?;
    Ok(limit(trail).max(count(trail)).saturating_add(1))
}

/// レビューのステップで、次に行うことを返す。
pub(super) fn next_review(state: &PlanState, doc: &PlanDocument) -> Result<Next, Error> {
    let target = target(state, Step::Review)?;
    let trail = trail(state, &target)?;
    let prefix = trail
        .review_prefix
        .clone()
        .ok_or_else(|| Error::NoReviewPrefix(target.label()))?;
    let number = count(trail).saturating_add(1);
    let reviewer = reviewer_for(number);
    Ok(match target {
        Target::Task(id) => Next::Review {
            task: doc.task(&id)?.clone(),
            reviewer,
            number,
            prefix,
            repo: trail.review_repo.clone(),
        },
        Target::Report => Next::ReportReview {
            url: report_url(state)?,
            reviewer,
            number,
            prefix,
        },
    })
}

/// 修正のステップで、次に行うことを返す。修正は、最後にレビューしたレビュアーが行う。
pub(super) fn next_fix(state: &PlanState, doc: &PlanDocument) -> Result<Next, Error> {
    let target = target(state, Step::Fix)?;
    let last = last_review(state, &target)?;
    Ok(match target {
        Target::Task(id) => Next::Fix {
            task: doc.task(&id)?.clone(),
            reviewer: last.role,
            doc: last.doc.clone(),
        },
        Target::Report => Next::ReportFix {
            url: report_url(state)?,
            reviewer: last.role,
            doc: last.doc.clone(),
        },
    })
}

/// 状態stateを、レビューの対象targetの次のレビューのステップにする。
///
/// タスクのレビュー接頭辞は、最初のレビューを始めるときに決める。作業報告書のレビュー接頭辞は、回を記録するときに決めてある。
/// 始める時点の最新のレビュードキュメントを残し、それより新しい連番だけを、このステップのレビューとして受け付ける。
pub(super) fn start_review(
    state: &mut PlanState,
    project: &Project,
    target: &Target,
) -> Result<(), Error> {
    let prefix = match target {
        Target::Task(id) => task_prefix(state, project, id)?,
        Target::Report => trail(state, target)?
            .review_prefix
            .clone()
            .ok_or_else(|| Error::NoReviewPrefix(target.label()))?,
    };
    let before = latest(project, &prefix)?;
    start(state, target.task(), Step::Review);
    state.review_before = before;
    Ok(())
}

/// タスクidのレビュー接頭辞を返す。まだ決めていなければ、ここで決めて記録する。
fn task_prefix(state: &mut PlanState, project: &Project, id: &str) -> Result<String, Error> {
    let record = state
        .tasks
        .iter_mut()
        .find(|record| record.id == id)
        .ok_or_else(|| Error::MissingRecord(id.to_owned()))?;
    if let Some(prefix) = &record.review.review_prefix {
        return Ok(prefix.clone());
    }
    // `/korylus-review` は、本体と別のリポジトリのレビューが混ざらないよう、別のリポジトリではリポジトリ名を接頭辞に足す。
    let prefix = if record.repo == project.repo {
        record.branch.clone()
    } else {
        record.review.review_repo = Some(record.repo.clone());
        let name = record
            .repo
            .file_name()
            .map(|name| name.to_string_lossy())
            .unwrap_or_default();
        format!("{name}-{}", record.branch)
    };
    record.review.review_prefix = Some(prefix.clone());
    Ok(prefix)
}

/// 最新のレビューのドキュメントpathへの回答answersを、レビューの対象targetについて状態stateに反映し、次のステップを決める。
///
/// 人間の判断が要る指摘があれば、回答を書かずに人間を待つ。
/// 回答は状態を書く前にレビュードキュメントに書くため、状態を書けずにやり直しても同じ回答になる。
fn apply(
    state: &mut PlanState,
    project: &Project,
    target: &Target,
    path: &Path,
    answers: &Answers,
) -> Result<(), Error> {
    if !answers.pending.is_empty() {
        trail_mut(state, target)?.human_review = Some(HumanReview {
            doc: path.to_path_buf(),
            findings: answers.pending.iter().map(HumanFinding::from).collect(),
        });
        let mut details = vec![format!("レビュードキュメント: {}", path.display())];
        details.extend(answers.pending.iter().map(Finding::describe));
        wait(
            state,
            Some(Step::Fix),
            WaitKind::Review,
            "レビューに、人間の判断が要る指摘 (要人間判断・再検討提案) があります".to_owned(),
            details,
        );
        return Ok(());
    }

    if answers.decisions.iter().any(|decision| decision.auto) {
        let full = project.root.join(path);
        fs::write(&full, &answers.output)
            .map_err(|source| Error::WriteReview { path: full, source })?;
    }
    let trail = trail_mut(state, target)?;
    trail.human_review = None;
    for decision in answers
        .decisions
        .iter()
        .filter(|decision| decision.defers())
    {
        for choice in &decision.chosen {
            let deferred = DeferredFinding {
                doc: path.to_path_buf(),
                title: decision.finding.title.clone(),
                choice: choice.text.clone(),
            };
            if !trail.deferred.contains(&deferred) {
                trail.deferred.push(deferred);
            }
        }
    }
    if answers.needs_fix() {
        start(state, target.task(), Step::Fix);
        return Ok(());
    }
    after_fix(state, project, target)
}

/// 修正を終えたか、修正の要らない回答のあとに、再レビューするかレビューを終えるかを決める。
///
/// 最後のレビューがRequest Changesで、上限に達していなければ、別のレビュアーで再レビューする。
/// レビューを終えたら、タスクなら次のタスクへ、作業報告書なら作業報告書の確認の人間待ちへ進める。
fn after_fix(state: &mut PlanState, project: &Project, target: &Target) -> Result<(), Error> {
    let trail = trail(state, target)?;
    let last = last_review(state, target)?;
    if last.verdict == Verdict::RequestChanges && last.number < limit(trail) {
        return start_review(state, project, target);
    }
    match target {
        Target::Task(_) => clear(state),
        Target::Report => report::wait_report(state),
    }
    Ok(())
}

/// 状態stateが進めているステップがexpectedであることを確かめ、レビューの対象を返す。
///
/// 進めているタスクが無ければ、最新の回の作業報告書を対象にする。
fn target(state: &PlanState, expected: Step) -> Result<Target, Error> {
    if state.step != Some(expected) {
        return Err(Error::WrongStep {
            expected,
            actual: state.step,
        });
    }
    match &state.task {
        Some(_) => current(state, expected).map(|id| Target::Task(id.to_owned())),
        None if state.report_rounds.is_empty() => Err(Error::NoReport),
        None => Ok(Target::Report),
    }
}

/// 最新の回の作業報告書のURLを返す。
fn report_url(state: &PlanState) -> Result<String, Error> {
    state
        .latest_report()
        .map(str::to_owned)
        .ok_or(Error::NoReport)
}

/// レビュー接頭辞prefixの、最新のレビュードキュメントの、プロジェクトのルートからの相対パスを返す。
fn latest(project: &Project, prefix: &str) -> Result<Option<PathBuf>, Error> {
    review::latest(&project.root, prefix)
        .map(|path| path.map(PathBuf::from))
        .map_err(|source| Error::ListReviews {
            path: project.root.join(review::DIR),
            source,
        })
}

/// レビュー接頭辞prefixのレビュードキュメントのパスpathから、連番を返す。
fn sequence(prefix: &str, path: &Path) -> Result<usize, Error> {
    review::sequence(prefix, &path.to_string_lossy())
        .ok_or_else(|| Error::ReviewSequence(path.to_path_buf()))
}

/// プロジェクトのルートからの相対パスpathのレビュードキュメントを読み取る。
///
/// 読み取れなければ、書いた人が直せるよう [`Error::Rejected`] で理由を返す。
fn read_review(project: &Project, path: &Path) -> Result<Document, Error> {
    let full = project.root.join(path);
    let src =
        fs::read_to_string(&full).map_err(|source| Error::ReadReview { path: full, source })?;
    review::parse(&src).map_err(|err| {
        Error::Rejected(vec![
            format!("レビュードキュメント {} を読み取れません", path.display()),
            err.to_string(),
        ])
    })
}

/// 回答answersに推奨どおりに回答できない指摘があれば、レビュードキュメントpathを添えて理由を返す。
fn check_invalid(path: &Path, answers: &Answers) -> Result<(), Error> {
    if answers.invalid.is_empty() {
        return Ok(());
    }
    let mut reasons = vec![format!(
        "{} に、推奨どおりに回答できない指摘があります",
        path.display()
    )];
    reasons.extend(answers.invalid.iter().cloned());
    Err(Error::Rejected(reasons))
}

/// 指摘findingのチェックが、自動対応可の指摘の推奨の選択肢1つだけに入っているかを返す。
///
/// 回答で入れるチェックと同じになるため、レビュアーが入れていても回答の結果は変わらない。
fn checked_as_recommended(finding: &Finding) -> bool {
    if finding.proposal || finding.category != Some(Category::AutoFix) {
        return false;
    }
    let mut checked = finding.options.iter().filter(|choice| choice.checked);
    matches!((checked.next(), checked.next()), (Some(choice), None) if choice.recommended)
}

/// レビューの記録trailについて、人間が延ばした上限か既定の上限を返す。
fn limit(trail: &ReviewTrail) -> u32 {
    trail.review_limit.unwrap_or(DEFAULT_REVIEW_LIMIT)
}

/// レビューの記録trailで行ったレビューの回数を返す。
fn count(trail: &ReviewTrail) -> u32 {
    u32::try_from(trail.reviews.len()).unwrap_or(u32::MAX)
}

/// レビューの対象targetのレビューの記録を返す。
fn trail<'a>(state: &'a PlanState, target: &Target) -> Result<&'a ReviewTrail, Error> {
    match target {
        Target::Task(id) => state
            .task_record(id)
            .map(|record| &record.review)
            .ok_or_else(|| Error::MissingRecord(id.clone())),
        Target::Report => state
            .report_rounds
            .last()
            .map(|round| &round.review)
            .ok_or(Error::NoReport),
    }
}

/// レビューの対象targetのレビューの記録を、書き換えられる形で返す。
fn trail_mut<'a>(state: &'a mut PlanState, target: &Target) -> Result<&'a mut ReviewTrail, Error> {
    match target {
        Target::Task(id) => state
            .tasks
            .iter_mut()
            .find(|record| &record.id == id)
            .map(|record| &mut record.review)
            .ok_or_else(|| Error::MissingRecord(id.clone())),
        Target::Report => state
            .report_rounds
            .last_mut()
            .map(|round| &mut round.review)
            .ok_or(Error::NoReport),
    }
}

/// レビューの対象targetの最後のレビューの記録を返す。
fn last_review<'a>(state: &'a PlanState, target: &Target) -> Result<&'a ReviewRecord, Error> {
    trail(state, target)?
        .reviews
        .last()
        .ok_or_else(|| Error::NoReview(target.label()))
}

#[cfg(test)]
mod tests {
    use super::super::tests::{
        PLAN, checked, doc, implemented, implementing, new_state, project, rejected, task,
    };
    use super::super::{Prepared, finish_implement, finish_prepare, next};
    use super::*;
    use std::fmt::Write as _;

    /// PLANのタスク1-1のレビュー接頭辞。タスクのリポジトリが本体と別のため、リポジトリ名を足す。
    const PREFIX: &str = "korylus-tools-demo-1-1";

    /// 自動対応可で、修正する選択肢を推奨する指摘。
    const AUTO_FIX: &str = "### 1. 名前を揃える\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正案の通り直す (推奨)\n- [ ] 現状維持\n\n";

    /// 自動対応可で、先送りする選択肢を推奨する指摘。
    const AUTO_DEFER: &str = "### 2. 関数を分ける\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 提案通り変更する\n- [ ] 後続タスクに積む (推奨)\n\n";

    /// 要人間判断の指摘。
    const NEEDS_HUMAN: &str = "### 3. 設計を変える\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [ ] 設計を変える (推奨)\n- [ ] 現状維持\n\n";

    fn now() -> Timestamp {
        "2026-11-16T06:00:00Z"
            .parse()
            .expect("日時を読み取れなかった")
    }

    /// PLANのタスク1-1にチェックを付けた作業計画書。
    fn done() -> PlanDocument {
        doc(&checked(&["1-1"]))
    }

    /// タスク1-1を実装して、1回目のレビューに進めた状態。
    fn reviewing(project: &Project) -> PlanState {
        let mut state = implementing();
        finish_implement(&mut state, &done(), project, implemented())
            .expect("実装の完了を受け付けなかった");
        state
    }

    /// レビュー接頭辞PREFIXのnumber番目のレビュードキュメントを、総合評価verdictと指摘findingsで書き、
    /// プロジェクトのルートからの相対パスを返す。
    fn write_review(project: &Project, number: u32, verdict: &str, findings: &str) -> PathBuf {
        let path = PathBuf::from(format!("{}/{PREFIX}-{number:03}.md", review::DIR));
        let full = project.root.join(&path);
        fs::create_dir_all(full.parent().expect("親ディレクトリが無い"))
            .expect("ディレクトリを作れなかった");
        fs::write(
            &full,
            format!(
                "# コードレビュー: demo-1-1\n\n## ファイルごとのレビュー結果\n\n{findings}## 総合評価\n\n**評価**: {verdict}\n"
            ),
        )
        .expect("レビュードキュメントを書けなかった");
        path
    }

    fn read(project: &Project, path: &Path) -> String {
        fs::read_to_string(project.root.join(path)).expect("レビュードキュメントを読めなかった")
    }

    /// タスクのブランチ demo-1-1 で、修正をコミットし終えた結果。
    fn fixed() -> Fixed {
        Fixed {
            branch: "demo-1-1".to_owned(),
            dirty: Vec::new(),
        }
    }

    fn trail_of(state: &PlanState) -> &ReviewTrail {
        &state
            .task_record("1-1")
            .expect("タスク1-1の記録が無い")
            .review
    }

    fn step_of(state: &PlanState) -> (Option<&str>, Option<Step>) {
        (state.task.as_deref(), state.step)
    }

    #[test]
    fn 実装を終えたら_1回目のレビューをcodexに頼む() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let plan = done();
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Review {
                task: task(&plan, "1-1").clone(),
                reviewer: Reviewer::Codex,
                number: 1,
                prefix: PREFIX.to_owned(),
                repo: Some(PathBuf::from("/korylus-tools")),
            }
        );
    }

    #[test]
    fn 本体のリポジトリのタスクはブランチの名前をレビュー接頭辞にする() {
        let (_root, mut project) = project();
        project.repo = PathBuf::from("/korylus-tools");
        let state = reviewing(&project);
        assert_eq!(trail_of(&state).review_prefix.as_deref(), Some("demo-1-1"));
        assert_eq!(
            trail_of(&state).review_repo,
            None,
            "本体のリポジトリを repo= に渡そうとした"
        );
    }

    #[test]
    fn レビューを始める前からあるレビュードキュメントは今回のレビューとして受け付けない() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let want = [format!(
            "docs/private/reviews/{PREFIX}-NNN.md に、このレビューのレビュードキュメント (前回より新しい連番) がありません"
        )];
        assert_eq!(
            rejected(finish_review(&mut state, &project, now())),
            want,
            "レビュードキュメントが無いのに受け付けた"
        );

        let old = write_review(&project, 1, "Approve", "");
        let mut state = reviewing(&project);
        assert_eq!(
            state.review_before,
            Some(old),
            "始めたときの最新のドキュメントを残していない"
        );
        assert_eq!(
            rejected(finish_review(&mut state, &project, now())),
            want,
            "前からあるドキュメントを受け付けた"
        );

        write_review(&project, 2, "Approve", "");
        finish_review(&mut state, &project, now()).expect("新しいドキュメントを受け付けなかった");
    }

    #[test]
    fn 指摘が無ければレビューを記録してタスクを終える() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let path = write_review(&project, 1, "Approve", "");
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");

        assert_eq!(
            trail_of(&state).reviews,
            [ReviewRecord {
                number: 1,
                role: Reviewer::Codex,
                doc: path,
                verdict: Verdict::Approve,
                at: now(),
            }]
        );
        assert_eq!(step_of(&state), (None, None), "タスクを終えていない");
    }

    #[test]
    fn 自動対応可の指摘に推奨の選択肢で回答し_最後にレビューしたレビュアーが修正する() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let findings = format!("{AUTO_FIX}{AUTO_DEFER}");
        let path = write_review(&project, 1, "Request Changes", &findings);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");

        let content = read(&project, &path);
        assert!(
            content.contains("- [x] 修正案の通り直す (推奨)")
                && content.contains("- [x] 後続タスクに積む (推奨)"),
            "推奨の選択肢で回答していない: {content}"
        );
        assert_eq!(
            trail_of(&state).deferred,
            [DeferredFinding {
                doc: path.clone(),
                title: "2. 関数を分ける".to_owned(),
                choice: "後続タスクに積む (推奨)".to_owned(),
            }],
            "先送りした指摘を記録していない"
        );
        let plan = done();
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Fix {
                task: task(&plan, "1-1").clone(),
                reviewer: Reviewer::Codex,
                doc: path,
            }
        );
    }

    #[test]
    fn request_changesなら修正のあとに別のレビュアーが再レビューする() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let first = write_review(&project, 1, "Request Changes", AUTO_FIX);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        finish_fix(&mut state, &done(), &project, &fixed()).expect("修正の完了を受け付けなかった");

        assert_eq!(
            state.review_before,
            Some(first),
            "前回のドキュメントを残していない"
        );
        let plan = done();
        assert!(
            matches!(
                next(&mut state, &plan).expect("次に行うことを決められなかった"),
                Next::Review {
                    reviewer: Reviewer::Claude,
                    number: 2,
                    ..
                }
            ),
            "別のレビュアーの2回目のレビューに進まなかった"
        );

        write_review(&project, 2, "Approve", "");
        finish_review(&mut state, &project, now()).expect("再レビューの完了を受け付けなかった");
        assert_eq!(step_of(&state), (None, None), "タスクを終えていない");
    }

    #[test]
    fn commentなら修正のあとに再レビューせずにタスクを終える() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        write_review(&project, 1, "Comment", AUTO_FIX);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            step_of(&state),
            (Some("1-1"), Some(Step::Fix)),
            "修正に進んでいない"
        );
        finish_fix(&mut state, &done(), &project, &fixed()).expect("修正の完了を受け付けなかった");
        assert_eq!(step_of(&state), (None, None), "タスクを終えていない");
    }

    #[test]
    fn 先送りだけの回答なら修正を挟まない() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        write_review(&project, 1, "Comment", AUTO_DEFER);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(step_of(&state), (None, None), "Commentで修正を挟んだ");

        let (_root, project) = super::super::tests::project();
        let mut state = reviewing(&project);
        write_review(&project, 1, "Request Changes", AUTO_DEFER);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            step_of(&state),
            (Some("1-1"), Some(Step::Review)),
            "Request Changesで修正を挟むか、再レビューしなかった"
        );
    }

    #[test]
    fn 修正の完了の条件を満たさなければ状態を変えずに理由を返す() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        write_review(&project, 1, "Request Changes", AUTO_FIX);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        let before = state.clone();

        let body = checked(&["1-1"]).replace("状態ファイルを作る。", "設計を書き換えた。");
        let result = finish_fix(
            &mut state,
            &doc(&body),
            &project,
            &Fixed {
                branch: "main".to_owned(),
                dirty: vec!["?? notes.md".to_owned()],
            },
        );
        assert_eq!(
            rejected(result),
            [
                "/korylus-tools がタスクのブランチ demo-1-1 ではなく main にいます",
                "/korylus-tools に未コミットの変更があります: ?? notes.md",
                "作業計画書で変えてよいのは、タスク 1-1 のチェックボックスと「実装判断ログ」節への追記だけです",
                "作業計画書の5行目: 「設計」節が変更されています",
            ]
        );
        assert_eq!(state, before, "受け付けなかった完了で状態を変えた");
    }

    #[test]
    fn 要人間判断の指摘があれば人間を待ち_回答してから再開すると修正に進む() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let findings = format!("{AUTO_FIX}{NEEDS_HUMAN}");
        let path = write_review(&project, 1, "Request Changes", &findings);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");

        let wait = state.wait.clone().expect("人間を待っていない");
        assert_eq!(wait.kind, WaitKind::Review, "人間を待つ場面が違う");
        assert_eq!(
            wait.details[0],
            format!("レビュードキュメント: {}", path.display())
        );
        assert!(
            wait.details[1].ends_with("「3. 設計を変える」: 要人間判断"),
            "人間の判断が要る指摘を挙げていない: {:?}",
            wait.details
        );
        assert!(
            !read(&project, &path).contains("[x]"),
            "人間を待つ前に回答を書いた"
        );
        assert!(
            matches!(
                next(&mut state, &done()).expect("次に行うことを決められなかった"),
                Next::Wait(_)
            ),
            "人間を待っている間に先へ進んだ"
        );
        let err = finish_fix(&mut state, &done(), &project, &fixed())
            .expect_err("人間を待っている間に修正の完了を記録した");
        assert!(
            matches!(err, Error::Waiting(WaitKind::Review)),
            "エラー = {err:?}"
        );

        let before = state.clone();
        let reasons = rejected(resume(&mut state, &done(), &project, None));
        assert_eq!(
            reasons[0],
            format!(
                "{} に、まだ対応方針に回答の無い指摘があります",
                path.display()
            )
        );
        assert_eq!(state, before, "再開できないのに状態を変えた");

        let answered = read(&project, &path).replace(
            "- [ ] 現状維持\n\n## 総合評価",
            "- [x] 現状維持\n\n## 総合評価",
        );
        fs::write(project.root.join(&path), answered).expect("回答を書けなかった");
        resume(&mut state, &done(), &project, None).expect("回答してから再開できなかった");

        assert!(
            read(&project, &path).contains("- [x] 修正案の通り直す (推奨)"),
            "自動対応可の指摘に回答していない"
        );
        assert_eq!(
            trail_of(&state).deferred,
            [DeferredFinding {
                doc: path,
                title: "3. 設計を変える".to_owned(),
                choice: "現状維持".to_owned(),
            }],
            "人間が先送りした指摘を記録していない"
        );
        assert_eq!(state.wait, None, "人間待ちを解いていない");
        assert_eq!(
            step_of(&state),
            (Some("1-1"), Some(Step::Fix)),
            "修正に進んでいない"
        );
    }

    /// レビューの上限を1回にして、1回目のレビューでRequest Changesを受け取り、上限で人間を待っている状態。
    fn at_review_limit(project: &Project) -> PlanState {
        let mut state = reviewing(project);
        if let Some(record) = state.tasks.iter_mut().find(|record| record.id == "1-1") {
            record.review.review_limit = Some(1);
        }
        write_review(project, 1, "Request Changes", AUTO_FIX);
        finish_review(&mut state, project, now()).expect("レビューの完了を受け付けなかった");
        state
    }

    #[test]
    fn レビューの上限に達してもrequest_changesなら人間を待ち_再開すると再レビューせずに終える() {
        let (_root, project) = project();
        let mut state = at_review_limit(&project);
        let wait = state.wait.clone().expect("人間を待っていない");
        assert_eq!(wait.kind, WaitKind::ReviewLimit, "人間を待つ場面が違う");
        assert_eq!(
            wait.reason,
            "レビューが上限の1回に達しても Request Changes でした"
        );

        resume(&mut state, &done(), &project, None).expect("再開できなかった");
        assert_eq!(
            step_of(&state),
            (Some("1-1"), Some(Step::Fix)),
            "修正に進んでいない"
        );
        finish_fix(&mut state, &done(), &project, &fixed()).expect("修正の完了を受け付けなかった");
        assert_eq!(step_of(&state), (None, None), "上限を超えて再レビューした");
    }

    #[test]
    fn レビューの上限を延ばして再開すると修正のあとに再レビューする() {
        let (_root, project) = project();
        let mut state = at_review_limit(&project);
        resume(&mut state, &done(), &project, Some(2)).expect("上限を延ばして再開できなかった");
        assert_eq!(
            trail_of(&state).review_limit,
            Some(2),
            "上限を延ばしていない"
        );

        finish_fix(&mut state, &done(), &project, &fixed()).expect("修正の完了を受け付けなかった");
        assert!(
            matches!(
                next(&mut state, &done()).expect("次に行うことを決められなかった"),
                Next::Review {
                    reviewer: Reviewer::Claude,
                    number: 2,
                    ..
                }
            ),
            "延ばした上限で再レビューしなかった"
        );
    }

    #[test]
    fn レビューの上限を延ばせないときは再開しない() {
        let (_root, project) = project();
        let mut state = at_review_limit(&project);
        let before = state.clone();
        assert_eq!(
            rejected(resume(&mut state, &done(), &project, Some(1))),
            [
                "新しいレビューの上限には、今の上限の1回と、行ったレビューの1回を超える回数を指定してください"
            ]
        );
        assert_eq!(state, before, "延ばせないのに状態を変えた");

        let (_root, project) = super::super::tests::project();
        let mut state = reviewing(&project);
        write_review(&project, 1, "Request Changes", NEEDS_HUMAN);
        finish_review(&mut state, &project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            rejected(resume(&mut state, &done(), &project, Some(5))),
            ["レビューの上限は、上限に達して人間を待っているときだけ延ばせます"]
        );
    }

    #[test]
    fn レビュアーが推奨の選択肢以外にチェックを入れたレビューは受け付けない() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let path = write_review(
            &project,
            1,
            "Comment",
            &AUTO_FIX.replace("- [ ] 現状維持", "- [x] 現状維持"),
        );
        let reasons = rejected(finish_review(&mut state, &project, now()));
        assert_eq!(
            reasons[0],
            format!(
                "{} で、レビュアーが対応方針の選択肢にチェックを入れています。チェックを外してください (回答はkorylusfactoryが行います)",
                path.display()
            )
        );

        // 推奨の選択肢へのチェックは、回答と同じになるため受け付ける。
        write_review(
            &project,
            1,
            "Comment",
            &AUTO_FIX.replace(
                "- [ ] 修正案の通り直す (推奨)",
                "- [x] 修正案の通り直す (推奨)",
            ),
        );
        finish_review(&mut state, &project, now())
            .expect("推奨の選択肢へのチェックで受け付けなかった");
    }

    #[test]
    fn 読み取れないか推奨どおりに回答できないレビュードキュメントは受け付けない() {
        let (_root, project) = project();
        let mut state = reviewing(&project);
        let path = write_review(&project, 1, "LGTM", "");
        let reasons = rejected(finish_review(&mut state, &project, now()));
        assert_eq!(
            reasons[0],
            format!("レビュードキュメント {} を読み取れません", path.display())
        );

        write_review(&project, 1, "Comment", &AUTO_FIX.replace(" (推奨)", ""));
        let reasons = rejected(finish_review(&mut state, &project, now()));
        assert_eq!(
            reasons[0],
            format!(
                "{} に、推奨どおりに回答できない指摘があります",
                path.display()
            )
        );
        assert!(
            trail_of(&state).reviews.is_empty(),
            "受け付けなかったレビューを記録した"
        );
    }

    #[test]
    fn 人が行うタスクの待ちは再開すると次に作業計画書から選び直す() {
        let (_root, project) = project();
        let mut state = new_state();
        let plan = doc(PLAN);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        finish_prepare(&mut state, &plan, Prepared::Human).expect("準備を記録できなかった");

        resume(&mut state, &done(), &project, None).expect("再開できなかった");
        assert_eq!(state.wait, None, "人間待ちを解いていない");
        assert!(
            matches!(
                next(&mut state, &plan).expect("次に行うことを決められなかった"),
                Next::Prepare { .. }
            ),
            "作業計画書から選び直さなかった"
        );
    }

    #[test]
    fn 人間を待っていなければ再開するものが無い() {
        let (_root, project) = project();
        let mut state = implementing();
        let err = resume(&mut state, &done(), &project, None)
            .expect_err("人間を待っていないのに再開した");
        assert!(matches!(err, Error::NotWaiting), "エラー = {err:?}");
    }

    #[test]
    fn レビュアーはcodexから交互に替える() {
        let reviewers: Vec<Reviewer> = (1..=4).map(reviewer_for).collect();
        assert_eq!(
            reviewers,
            [
                Reviewer::Codex,
                Reviewer::Claude,
                Reviewer::Codex,
                Reviewer::Claude
            ]
        );
    }

    /// 要人間判断の指摘への回答の欄を、選んだ選択肢chosenで書いた項目の本文。optionsは欄の選択肢。
    fn review_body(options: &[&str], chosen: usize) -> String {
        let mut body = "## 回答\n\n### 指摘1: 3. 設計を変える\n\n> 要点\n\n".to_owned();
        for (i, option) in options.iter().enumerate() {
            let mark = if i == chosen { "x" } else { " " };
            let label = char::from(b'A' + u8::try_from(i).expect("選択肢が多すぎる"));
            let _ = writeln!(body, "- [{mark}] {label}: {option}");
        }
        body
    }

    /// 要人間判断の指摘neededを含むレビューで、人間を待っている状態と、レビュードキュメントのパス。
    fn waiting_review(project: &Project, needed: &str) -> (PlanState, PathBuf) {
        let mut state = reviewing(project);
        let path = write_review(
            project,
            1,
            "Request Changes",
            &format!("{AUTO_FIX}{needed}"),
        );
        finish_review(&mut state, project, now()).expect("レビューの完了を受け付けなかった");
        assert_eq!(
            state.wait.as_ref().map(|wait| wait.kind),
            Some(WaitKind::Review),
            "人間を待っていない"
        );
        (state, path)
    }

    #[test]
    fn 項目で選んだ選択肢をレビュードキュメントに書いて修正に進む() {
        let (_root, project) = project();
        let (mut state, path) = waiting_review(&project, NEEDS_HUMAN);
        let body = review_body(&["設計を変える (推奨)", "現状維持"], 1);
        super::super::resume_with(&mut state, &done(), &project, &body)
            .expect("回答で再開できなかった");
        assert_eq!(state.wait, None, "人間を待ったまま");
        assert_eq!(
            step_of(&state),
            (Some("1-1"), Some(Step::Fix)),
            "修正に進まない"
        );
        let written = read(&project, &path);
        assert!(
            written.contains("- [ ] 設計を変える (推奨)\n- [x] 現状維持"),
            "選んだ選択肢を書いていない:\n{written}"
        );
        assert!(
            written.contains("- [x] 修正案の通り直す (推奨)"),
            "自動対応可の指摘に回答していない:\n{written}"
        );
    }

    #[test]
    fn 回答を書いたあと状態を保存せずに中断しても同じ回答で再開できる() {
        for legacy in [false, true] {
            let (_root, project) = project();
            let (mut state, path) = waiting_review(&project, NEEDS_HUMAN);
            if legacy {
                trail_mut(&mut state, &Target::Task("1-1".to_owned()))
                    .expect("レビューの記録が無い")
                    .human_review = None;
            }
            let store = crate::state::Store::new(&project.root);
            let dir = store.plan("demo").expect("状態の置き場を求められなかった");
            dir.lock()
                .expect("ロックを取れなかった")
                .save(&state)
                .expect("人間待ちを保存できなかった");
            let body = review_body(&["設計を変える (推奨)", "現状維持"], 1);
            super::super::resume_with(&mut state, &done(), &project, &body)
                .expect("最初の回答を適用できなかった");
            let written = read(&project, &path);
            // レビュードキュメントだけ書いたところで停止し、再開後は保存済みの待ちを読み直す。
            let mut restarted = dir.load().expect("状態を読めなかった").expect("状態が無い");
            assert!(restarted.wait.is_some(), "保存前の待ちを再現できていない");
            super::super::resume_with(&mut restarted, &done(), &project, &body)
                .expect("中断後に同じ回答を再適用できなかった");
            assert_eq!(restarted, state, "中断で再開後の状態が変わった");
            assert_eq!(
                read(&project, &path),
                written,
                "同じ回答でドキュメントが変わった"
            );
        }
    }

    #[test]
    fn 再開の条件で差し戻したあとも回答を選び直してチェックを書き直せる() {
        let (_root, project) = project();
        let (mut state, path) = waiting_review(&project, NEEDS_HUMAN);
        let before = state.clone();
        let invalid = AUTO_DEFER.replace(" (推奨)", "");
        let changed =
            read(&project, &path).replace("## 総合評価", &format!("{invalid}## 総合評価"));
        fs::write(project.root.join(&path), changed).expect("レビューを更新できなかった");
        let reasons = rejected(super::super::resume_with(
            &mut state,
            &done(),
            &project,
            &review_body(&["設計を変える (推奨)", "現状維持"], 0),
        ));
        assert!(
            reasons[0].contains("推奨どおりに回答できない指摘"),
            "理由 = {reasons:?}"
        );
        assert_eq!(state, before, "再開できないのに状態を変えた");
        let written = read(&project, &path);
        assert!(
            written.contains("- [x] 設計を変える (推奨)"),
            "回答を書いた後の失敗を再現できていない"
        );
        fs::write(project.root.join(&path), written.replace(&invalid, ""))
            .expect("再開を妨げる不備を直せなかった");
        super::super::resume_with(
            &mut state,
            &done(),
            &project,
            &review_body(&["設計を変える (推奨)", "現状維持"], 1),
        )
        .expect("選び直した回答を適用できなかった");
        assert!(
            read(&project, &path).contains("- [ ] 設計を変える (推奨)\n- [x] 現状維持"),
            "選び直す前のチェックが残った"
        );
        assert_eq!(state.wait, None, "人間を待ったまま");
    }

    #[test]
    fn 回答を書いたあと差し戻した待ちで積み直した項目にも回答して再開できる() {
        let (_root, project) = project();
        let (mut state, path) = waiting_review(&project, NEEDS_HUMAN);
        let invalid = AUTO_DEFER.replace(" (推奨)", "");
        let changed =
            read(&project, &path).replace("## 総合評価", &format!("{invalid}## 総合評価"));
        fs::write(project.root.join(&path), changed).expect("レビューを更新できなかった");
        rejected(super::super::resume_with(
            &mut state,
            &done(),
            &project,
            &review_body(&["設計を変える (推奨)", "現状維持"], 0),
        ));
        assert!(
            read(&project, &path).contains("- [x] 設計を変える (推奨)"),
            "回答を書いたあとの差し戻しを再現できていない"
        );
        fs::write(
            project.root.join(&path),
            read(&project, &path).replace(&invalid, ""),
        )
        .expect("再開を妨げる不備を直せなかった");

        // 差し戻したあとに項目が無くなり、積み直した項目で選び直して回答する。
        let item = crate::item::for_wait(
            "mewst",
            "demo",
            &state,
            state.wait.as_ref().expect("人間を待っていない"),
            &project,
            "2026-11-17T21:04:05"
                .parse()
                .expect("日時を読み取れなかった"),
        )
        .expect("項目を作れなかった");
        let form = korylus_workdoc::queue::parse(&item.body()).expect("項目を読めなかった");
        assert_eq!(
            form.fields.len(),
            1,
            "回答を書いた指摘の欄が、積み直した項目から消えた"
        );
        let body = form
            .fill(
                &[Response::Choice(1)],
                "プランナー",
                "2026-11-17T21:05:05"
                    .parse()
                    .expect("日時を読み取れなかった"),
            )
            .expect("項目に回答できなかった");
        super::super::resume_with(&mut state, &done(), &project, &body)
            .expect("積み直した項目の回答で再開できなかった");
        assert!(
            read(&project, &path).contains("- [ ] 設計を変える (推奨)\n- [x] 現状維持"),
            "選び直した回答を書いていない"
        );
        assert_eq!(state.wait, None, "人間を待ったまま");
    }

    #[test]
    fn 同じ選択肢の指摘でも欄やレビューの指摘を入れ替えた回答を書かない() {
        let first = NEEDS_HUMAN.to_owned();
        let second = NEEDS_HUMAN.replace("3. 設計を変える", "4. 実行を変える");
        let first_field = review_body(&["設計を変える (推奨)", "現状維持"], 0);
        let second_field = review_body(&["設計を変える (推奨)", "現状維持"], 1)
            .replace("指摘1: 3. 設計を変える", "指摘2: 4. 実行を変える");
        for swap_document in [false, true] {
            let (_root, project) = project();
            let (mut state, path) = waiting_review(&project, &format!("{first}{second}"));
            let before = state.clone();
            let body = if swap_document {
                write_review(
                    &project,
                    1,
                    "Request Changes",
                    &format!("{AUTO_FIX}{second}{first}"),
                );
                format!(
                    "{first_field}{}",
                    second_field.trim_start_matches("## 回答\n")
                )
            } else {
                format!(
                    "{second_field}{}",
                    first_field.trim_start_matches("## 回答\n")
                )
            };
            let doc_before = read(&project, &path);
            let reasons = rejected(super::super::resume_with(
                &mut state,
                &done(),
                &project,
                &body,
            ));
            assert!(!reasons.is_empty(), "入れ替わりの理由が無い");
            assert_eq!(state, before, "取り違えた回答で状態を進めた");
            assert_eq!(
                read(&project, &path),
                doc_before,
                "別の指摘にチェックを書いた"
            );
        }
    }

    #[test]
    fn 同じ見出しと選択肢でも判断の本文が入れ替わった指摘には回答しない() {
        let first = NEEDS_HUMAN.replace("**区分**:", "判断する内容A。\n\n**区分**:");
        let second = NEEDS_HUMAN.replace("**区分**:", "判断する内容B。\n\n**区分**:");
        let (_root, project) = project();
        let (mut state, path) = waiting_review(&project, &format!("{first}{second}"));
        write_review(
            &project,
            1,
            "Request Changes",
            &format!("{AUTO_FIX}{second}{first}"),
        );
        let doc_before = read(&project, &path);
        let field = review_body(&["設計を変える (推奨)", "現状維持"], 0);
        let body = format!(
            "{field}{}",
            field
                .trim_start_matches("## 回答\n")
                .replace("指摘1:", "指摘2:")
        );
        let reasons = rejected(super::super::resume_with(
            &mut state,
            &done(),
            &project,
            &body,
        ));
        assert!(
            reasons[0].contains("回答対象が、人間待ちを始めたときと違います"),
            "理由 = {reasons:?}"
        );
        assert_eq!(
            read(&project, &path),
            doc_before,
            "判断の本文が違う指摘に回答した"
        );
    }

    #[test]
    fn 項目の見出しが文字参照でも元の指摘と照合して回答できる() {
        let (_root, project) = project();
        let needed = NEEDS_HUMAN.replace("3. 設計を変える", "**[[設計]]** を変える");
        let (mut state, path) = waiting_review(&project, &needed);
        let item = crate::item::for_wait(
            "mewst",
            "demo",
            &state,
            state.wait.as_ref().expect("人間を待っていない"),
            &project,
            "2026-11-17T21:04:05"
                .parse()
                .expect("日時を読み取れなかった"),
        )
        .expect("項目を作れなかった");
        let form = korylus_workdoc::queue::parse(&item.body()).expect("項目を読めなかった");
        assert!(
            form.fields[0].heading.contains("&#91;"),
            "文字参照を再現できていない"
        );
        let body = form
            .fill(
                &[Response::Choice(1)],
                "プランナー",
                "2026-11-17T21:05:05"
                    .parse()
                    .expect("日時を読み取れなかった"),
            )
            .expect("項目に回答できなかった");
        super::super::resume_with(&mut state, &done(), &project, &body)
            .expect("文字参照の見出しで再開できなかった");
        assert!(
            read(&project, &path).contains("- [ ] 設計を変える (推奨)\n- [x] 現状維持"),
            "元の指摘に回答していない"
        );
    }

    #[test]
    fn 選択肢が書き換えられたか選択肢に無い対応を選んだ回答は書かずに理由を返す() {
        let other = NEEDS_HUMAN.replace("- [ ] 現状維持", "- [ ] その他 (下の回答欄に記入)");
        let cases = [
            (
                NEEDS_HUMAN.to_owned(),
                review_body(&["設計を変える (推奨)", "別の方針にする"], 0),
                "「指摘1: 3. 設計を変える」の選択肢が、レビュードキュメント docs/private/reviews/korylus-tools-demo-1-1-001.md の指摘「3. 設計を変える」の選択肢と違います",
            ),
            (
                other,
                review_body(&["設計を変える (推奨)", "その他 (下の回答欄に記入)"], 1),
                "「指摘1: 3. 設計を変える」: 「その他 (下の回答欄に記入)」を選んだときの対応の文を書く欄が、項目にありません",
            ),
        ];
        for (needed, body, want) in cases {
            let (_root, project) = project();
            let (mut state, path) = waiting_review(&project, &needed);
            let (before, doc_before) = (state.clone(), read(&project, &path));
            let reasons = rejected(super::super::resume_with(
                &mut state,
                &done(),
                &project,
                &body,
            ));
            assert_eq!(reasons.len(), 1, "理由 = {reasons:?}");
            assert!(reasons[0].starts_with(want), "理由 = {}", reasons[0]);
            assert_eq!(state, before, "受け付けなかった回答で状態を変えた");
            assert_eq!(
                read(&project, &path),
                doc_before,
                "受け付けなかった回答を書いた"
            );
        }
    }

    #[test]
    fn 回答の欄の数が人間の回答を待つ指摘と違えば理由を返す() {
        let (_root, project) = project();
        let (mut state, _) = waiting_review(&project, NEEDS_HUMAN);
        let body = format!(
            "{}\n### 指摘2: 余分な欄\n\n- [x] A: 直す\n",
            review_body(&["設計を変える (推奨)", "現状維持"], 0)
        );
        let reasons = rejected(super::super::resume_with(
            &mut state,
            &done(),
            &project,
            &body,
        ));
        assert_eq!(
            reasons,
            [
                "回答の欄が2個ありますが、レビュードキュメント docs/private/reviews/korylus-tools-demo-1-1-001.md で人間の回答を待つ指摘は1個です"
            ]
        );
    }

    #[test]
    fn レビューの上限の項目で延ばすか打ち切るかを選んで再開する() {
        let body = |chosen: usize| {
            let marks = if chosen == 0 { ["x", " "] } else { [" ", "x"] };
            format!(
                "## 回答\n\n### 進め方\n\n- [{}] A: 上限を延ばして再レビューする\n- [{}] B: 打ち切って次へ進む\n",
                marks[0], marks[1]
            )
        };
        let (_root, project) = project();
        let mut state = at_review_limit(&project);
        super::super::resume_with(&mut state, &done(), &project, &body(0))
            .expect("上限を延ばして再開できなかった");
        assert_eq!(state.wait, None, "人間を待ったまま");
        assert_eq!(
            trail_of(&state).review_limit,
            Some(2),
            "上限を1回延ばしていない"
        );

        let (_root, project) = super::super::tests::project();
        let mut state = at_review_limit(&project);
        super::super::resume_with(&mut state, &done(), &project, &body(1))
            .expect("打ち切って再開できなかった");
        assert_eq!(state.wait, None, "人間を待ったまま");
        assert_eq!(
            trail_of(&state).review_limit,
            Some(1),
            "打ち切ったのに上限を延ばした"
        );
    }
}
