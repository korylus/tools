//! 1つの作業計画書のタスクを進める状態遷移。
//!
//! [`next`] が状態と作業計画書から次に行うことを決め、`finish_*` の関数が行ったことの結果を確かめて状態を進める。
//! どれも外部プロセスを起動しない。
//! ブランチの準備のgitとセッションの起動は呼び出し側 (`korylusfactory-cli`) が行い、その結果とリポジトリの状態を渡す。
//!
//! レビュードキュメントは、プロジェクトのルートの下から読み、自動対応可の指摘への回答を書き込む。
//!
//! 呼び出し側は、状態ファイルのロックを取ってから状態を読み、状態遷移で変えた状態を同じロックの中で書く。
//! エラーを返したときは、状態を書かない。

mod report;
mod retry;
mod review;

use std::io;
use std::path::PathBuf;

pub use korylus_workdoc::Error as PlanError;
use korylus_workdoc::plan::{self, Violation};
pub use korylus_workdoc::plan::{BaseBranch, Plan, Task};

pub use report::{Reported, complete, finish_report};
pub use retry::{RETRY_LIMIT, Retry, ask, begin_session, retry};
pub use review::{
    DEFAULT_REVIEW_LIMIT, Fixed, finish_fix, finish_report_fix, finish_review, resume, reviewer_for,
};

use crate::state::{
    PlanState, ReviewTrail, Reviewer, Step, TaskRecord, VerificationItem, Wait, WaitKind,
};

/// 状態遷移を進められないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// ステップの完了の条件を満たしていない。セッションに理由を伝えて、同じステップをやり直させる。
    #[error("ステップの完了の条件を満たしていません:\n- {}", .0.join("\n- "))]
    Rejected(Vec<String>),

    /// 完了を記録しようとしたステップが、進めているステップと異なる。
    #[error("{}の完了を記録しようとしましたが、{}", .expected.label(), describe_step(*.actual))]
    WrongStep {
        /// 完了を記録しようとしたステップ。
        expected: Step,
        /// 進めているステップ。
        actual: Option<Step>,
    },

    /// ステップを進めているのに、進めているタスクが無い。
    #[error("{}を進めているタスクがありません", .0.label())]
    NoTask(Step),

    /// 進めているタスクが、作業計画書に無い。
    #[error("進めているタスク {0} が作業計画書にありません")]
    MissingTask(String),

    /// 進めているタスクの記録が無い。
    #[error("タスク {0} の記録がありません")]
    MissingRecord(String),

    /// タスクを始めたときに残す、作業計画書のスナップショットが無い。
    #[error("タスク {0} を始めたときの作業計画書のスナップショットがありません")]
    MissingSnapshot(String),

    /// 作業計画書に計画書名の行が無く、タスクのブランチの名前を決められない。
    #[error("作業計画書に計画書名の行が無いため、タスクのブランチの名前を決められません")]
    NoPlanName,

    /// レビューの対象 (「タスク 1-1」や「作業報告書」) に、レビュードキュメントの名前に使う接頭辞が無い。
    #[error("{0}のレビュー接頭辞がありません")]
    NoReviewPrefix(String),

    /// レビューの対象 (「タスク 1-1」や「作業報告書」) に、レビューの記録が無い。
    #[error("{0}のレビューの記録がありません")]
    NoReview(String),

    /// 作業報告書のステップを進めているのに、作業報告書の記録が無い。
    #[error("作業報告書の記録がありません")]
    NoReport,

    /// 作業報告書を書き始めたときに残す、作業計画書のスナップショットが無い。
    #[error("作業報告書を書き始めたときの作業計画書のスナップショットがありません")]
    NoReportSnapshot,

    /// 作業報告書の確認を待っていないのに、作業計画書の完了を記録しようとした。
    #[error("作業報告書の確認を待っていないため、作業計画書の完了を記録できません")]
    NotWaitingReport,

    /// 作業報告書の修正を進めているのに、タスクの修正の完了を記録しようとした。
    #[error("作業報告書の修正を進めているため、タスクの修正の完了として記録できません")]
    ReportInProgress,

    /// タスクの修正を進めているのに、作業報告書の修正の完了を記録しようとした。
    #[error("タスク {0} の修正を進めているため、作業報告書の修正の完了として記録できません")]
    TaskInProgress(String),

    /// レビュードキュメントの名前から、連番を読み取れない。
    #[error("レビュードキュメント {} の連番を読み取れません", .0.display())]
    ReviewSequence(PathBuf),

    /// レビュードキュメントの置き場を読めない。
    #[error("レビュードキュメントの置き場 {} を読めません", .path.display())]
    ListReviews {
        /// レビュードキュメントの置き場。
        path: PathBuf,
        /// 読めなかった理由。
        #[source]
        source: io::Error,
    },

    /// レビュードキュメントを読めない。
    #[error("レビュードキュメント {} を読めません", .path.display())]
    ReadReview {
        /// レビュードキュメントのパス。
        path: PathBuf,
        /// 読めなかった理由。
        #[source]
        source: io::Error,
    },

    /// レビュードキュメントに、対応方針への回答を書けない。
    #[error("レビュードキュメント {} に回答を書けません", .path.display())]
    WriteReview {
        /// レビュードキュメントのパス。
        path: PathBuf,
        /// 書けなかった理由。
        #[source]
        source: io::Error,
    },

    /// 人間を待っている間に、ステップの完了を記録しようとした。
    #[error("{}で人間を待っている間は、ステップの完了を記録できません", .0.label())]
    Waiting(WaitKind),

    /// 人間を待っていないのに、再開しようとした。
    #[error("人間を待っていないため、再開するものがありません")]
    NotWaiting,

    /// セッションを起動するステップを進めていないのに、セッションの終わりを記録しようとした。
    #[error("セッションを起動するステップを進めていません。{}", describe_step(*.0))]
    NoSession(Option<Step>),
}

/// 進めているステップstepを、エラーの文面に入れる形にする。
fn describe_step(step: Option<Step>) -> String {
    match step {
        Some(step) => format!("進めているステップは{}です", step.label()),
        None => "進めているステップがありません".to_owned(),
    }
}

/// 作業計画書の本文と、それを読み取った結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlanDocument {
    body: String,
    plan: Plan,
}

impl PlanDocument {
    /// 作業計画書の本文bodyを読み取る。
    pub fn parse(body: impl Into<String>) -> Result<Self, PlanError> {
        let body = body.into();
        let plan = plan::parse(&body)?;
        Ok(Self { body, plan })
    }

    /// 作業計画書の本文を返す。
    pub fn body(&self) -> &str {
        &self.body
    }

    /// 作業計画書を読み取った結果を返す。
    pub fn plan(&self) -> &Plan {
        &self.plan
    }

    /// タスク番号idのタスクを返す。
    fn task(&self, id: &str) -> Result<&Task, Error> {
        self.plan
            .tasks
            .iter()
            .find(|task| task.id == id)
            .ok_or_else(|| Error::MissingTask(id.to_owned()))
    }

    /// 計画書名を返す。計画書名の行が無ければ、ブランチやレビュードキュメントの名前を決められないためエラーにする。
    fn name(&self) -> Result<&str, Error> {
        if self.plan.name.is_empty() {
            return Err(Error::NoPlanName);
        }
        Ok(&self.plan.name)
    }

    /// タスク番号idのタスクのブランチの名前 (`<計画書名>-<タスク番号>`) を返す。
    fn branch(&self, id: &str) -> Result<String, Error> {
        Ok(format!("{}-{id}", self.name()?))
    }
}

/// 状態遷移が読み書きする、プロジェクトの場所。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Project {
    /// プロジェクトのルート。レビュードキュメントは、この下の `docs/private/reviews` に置かれる。
    pub root: PathBuf,
    /// プロジェクトのルートを含むリポジトリ (本体のリポジトリ) のルート。
    pub repo: PathBuf,
}

/// 次に行うこと。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Next {
    /// タスクの対象を求め、ブランチを作るタスクならブランチを準備する。結果は [`finish_prepare`] で記録する。
    Prepare {
        /// 準備するタスク。
        task: Task,
        /// ブランチを作るタスクのときに作る、タスクのブランチの名前。
        branch: String,
    },
    /// 作業者がタスクを実装してコミットする。結果は [`finish_implement`] で記録する。
    Implement {
        /// 実装するタスク。ブランチやリポジトリは、状態のタスクの記録にある。
        task: Task,
    },
    /// 作業者がブランチを作らずにタスクを行う。結果は [`finish_work`] で記録する。
    Work {
        /// 行うタスク。
        task: Task,
    },
    /// レビュアーがタスクをレビューし、レビュードキュメントを書く。結果は [`finish_review`] で記録する。
    Review {
        /// レビューするタスク。
        task: Task,
        /// レビューするレビュアー。
        reviewer: Reviewer,
        /// そのタスクで何回目のレビューか (1始まり)。
        number: u32,
        /// レビュードキュメントの名前に使う接頭辞。
        prefix: String,
        /// `/korylus-review` の `repo=` に渡すリポジトリ。渡さないときは `None`。
        repo: Option<PathBuf>,
    },
    /// 最後にレビューしたレビュアーが、回答した指摘を修正してコミットする。結果は [`finish_fix`] で記録する。
    Fix {
        /// 修正するタスク。
        task: Task,
        /// 修正するレビュアー。
        reviewer: Reviewer,
        /// 回答したレビュードキュメントの、プロジェクトのルートからの相対パス。
        doc: PathBuf,
    },
    /// 作業者が作業報告書を書く。結果は [`finish_report`] で記録する。
    Report {
        /// 前の回で書いた作業報告書のURL。新しく作らずに同じページを更新してもらう。初めての回なら `None`。
        previous: Option<String>,
    },
    /// レビュアーが最新の回の作業報告書をレビューし、レビュードキュメントを書く。結果は [`finish_review`] で記録する。
    ReportReview {
        /// レビューする作業報告書のURL。
        url: String,
        /// レビューするレビュアー。
        reviewer: Reviewer,
        /// その回で何回目のレビューか (1始まり)。
        number: u32,
        /// レビュードキュメントの名前に使う接頭辞。
        prefix: String,
    },
    /// 最後にレビューしたレビュアーが、回答した指摘に沿って作業報告書の下書きを直す。結果は [`finish_report_fix`] で記録する。
    ReportFix {
        /// 直す作業報告書のURL。
        url: String,
        /// 直すレビュアー。
        reviewer: Reviewer,
        /// 回答したレビュードキュメントの、プロジェクトのルートからの相対パス。
        doc: PathBuf,
    },
    /// 人間を待つ。
    Wait(Wait),
    /// 作業計画書の完了を記録した。
    Finished,
}

/// タスクの準備の結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Prepared {
    /// 人が行うタスクだった。
    Human,
    /// 作業者がブランチを作らずに行うタスクだった。
    Work,
    /// タスクのブランチを準備した。
    Branch {
        /// タスクのリポジトリのルート。
        repo: PathBuf,
        /// タスクの接頭辞が表す、作業するディレクトリ。
        dir: PathBuf,
        /// タスクのブランチを作った元のブランチ。
        base: String,
    },
    /// タスクの対象を求められないか、ブランチを準備できなかった。
    Failed {
        /// 人間に伝える理由。
        reason: String,
        /// 理由の詳細 (未コミットのファイルなど)。
        details: Vec<String>,
    },
}

/// 作業者が実装を終えたあとの、タスクのリポジトリの状態とセッションの結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Implemented {
    /// リポジトリで切り替えているブランチ。
    pub branch: String,
    /// 切り替えているブランチが、タスクのブランチを作った元のブランチより先に持つコミットの数。
    pub ahead: u64,
    /// 未コミットの変更 (`git status --porcelain` の形)。作業計画書などのコミットしないパスは除く。
    pub dirty: Vec<String>,
    /// セッションが返した動作確認の項目。
    pub verification: Vec<VerificationItem>,
}

/// 状態stateと作業計画書docから次に行うことを決め、状態を次のステップに進める。
///
/// 人間を待っていれば待つことを返し、進めているステップがあればそのステップをもう一度返す。
/// 進めているステップが無ければ、作業計画書の最初の未完了のタスクの準備を返し、
/// 未完了のタスクが無ければ、作業報告書を書くステップを返す。
pub fn next(state: &mut PlanState, doc: &PlanDocument) -> Result<Next, Error> {
    if let Some(wait) = &state.wait {
        return Ok(Next::Wait(wait.clone()));
    }
    if state.is_completed() {
        return Ok(Next::Finished);
    }
    match state.step {
        // 準備は始めても状態を変えていないため、人間が作業計画書を直していれば反映するよう、タスクを選び直す。
        None | Some(Step::Prepare) => {}
        Some(step @ Step::Implement) => {
            let id = current(state, step)?;
            if state.task_record(id).is_none() {
                return Err(Error::MissingRecord(id.to_owned()));
            }
            let task = doc.task(id)?.clone();
            return Ok(Next::Implement { task });
        }
        Some(step @ Step::Work) => {
            let task = doc.task(current(state, step)?)?.clone();
            return Ok(Next::Work { task });
        }
        Some(Step::Review) => return review::next_review(state, doc),
        Some(Step::Fix) => return review::next_fix(state, doc),
        Some(Step::Report) => return Ok(report::next_report(state)),
    }

    let Some(task) = doc.plan.next() else {
        report::start_report(state, doc);
        return Ok(report::next_report(state));
    };
    let branch = doc.branch(&task.id)?;
    start(state, Some(&task.id), Step::Prepare);
    Ok(Next::Prepare {
        task: task.clone(),
        branch,
    })
}

/// タスクの準備の結果preparedを状態stateに記録し、タスクを行うステップか人間待ちに進める。
///
/// タスクを行うステップに進めるときは、作業計画書の変えてよい範囲を確かめるために、作業計画書docの本文を残す。
pub fn finish_prepare(
    state: &mut PlanState,
    doc: &PlanDocument,
    prepared: Prepared,
) -> Result<(), Error> {
    let id = current(state, Step::Prepare)?.to_owned();
    let task = doc.task(&id)?;
    // 人間が接頭辞や担当の行を直したときに前の対象の記録が残らないよう、記録は準備のたびに作り直す。
    state.tasks.retain(|record| record.id != id);
    match prepared {
        Prepared::Human => {
            let details = vec![format!("{} {}", task.id, task.title)];
            wait(
                state,
                None,
                WaitKind::HumanTask,
                "人が行うタスクです".to_owned(),
                details,
            );
        }
        Prepared::Work => {
            state.snapshot = Some(doc.body.clone());
            start(state, Some(&id), Step::Work);
        }
        Prepared::Branch { repo, dir, base } => {
            state.tasks.push(TaskRecord {
                branch: doc.branch(&id)?,
                id: id.clone(),
                repo,
                dir,
                base,
                prepared: true,
                review: ReviewTrail::default(),
                verification: Vec::new(),
            });
            state.snapshot = Some(doc.body.clone());
            start(state, Some(&id), Step::Implement);
        }
        Prepared::Failed { reason, details } => {
            wait(state, None, WaitKind::Branch, reason, details);
        }
    }
    Ok(())
}

/// 実装の完了を確かめ、満たしていれば動作確認の項目を記録して、1回目のレビューに進める。
///
/// 条件を満たさなければ、状態を変えずに [`Error::Rejected`] で理由を返す。
pub fn finish_implement(
    state: &mut PlanState,
    doc: &PlanDocument,
    project: &Project,
    implemented: Implemented,
) -> Result<(), Error> {
    let id = current(state, Step::Implement)?.to_owned();
    let task = doc.task(&id)?;
    let record = state
        .task_record(&id)
        .ok_or_else(|| Error::MissingRecord(id.clone()))?;

    let mut reasons = Vec::new();
    if let Some(reason) = checkout_reason(record, &implemented.branch) {
        reasons.push(reason);
    } else if implemented.ahead == 0 {
        reasons.push(format!(
            "ブランチ {} に、ベース {} より先のコミットがありません",
            record.branch, record.base
        ));
    }
    reasons.extend(dirty_reason(record, &implemented.dirty));
    reasons.extend(plan_reasons(state, doc, task)?);
    reasons.extend(verification_reasons(&implemented.verification));
    if !reasons.is_empty() {
        return Err(Error::Rejected(reasons));
    }

    if let Some(record) = state.tasks.iter_mut().find(|record| record.id == id) {
        record.verification = implemented.verification;
    }
    review::start_review(state, project, &review::Target::Task(id))
}

/// ブランチを作らずに行うタスクの完了を確かめ、満たしていればタスクを終える。
///
/// 条件を満たさなければ、状態を変えずに [`Error::Rejected`] で理由を返す。
pub fn finish_work(state: &mut PlanState, doc: &PlanDocument) -> Result<(), Error> {
    let id = current(state, Step::Work)?.to_owned();
    let reasons = plan_reasons(state, doc, doc.task(&id)?)?;
    if !reasons.is_empty() {
        return Err(Error::Rejected(reasons));
    }
    clear(state);
    Ok(())
}

/// 状態stateが進めているステップがexpectedであることを確かめ、進めているタスクの番号を返す。
fn current(state: &PlanState, expected: Step) -> Result<&str, Error> {
    if state.step != Some(expected) {
        return Err(Error::WrongStep {
            expected,
            actual: state.step,
        });
    }
    state.task.as_deref().ok_or(Error::NoTask(expected))
}

/// タスクの記録recordのリポジトリが、切り替えているブランチbranchでタスクのブランチにいなければ、その理由を返す。
fn checkout_reason(record: &TaskRecord, branch: &str) -> Option<String> {
    (branch != record.branch).then(|| {
        format!(
            "{} がタスクのブランチ {} ではなく {branch} にいます",
            record.repo.display(),
            record.branch
        )
    })
}

/// タスクの記録recordのリポジトリに未コミットの変更dirtyがあれば、その理由を返す。
fn dirty_reason(record: &TaskRecord, dirty: &[String]) -> Option<String> {
    (!dirty.is_empty()).then(|| {
        format!(
            "{} に未コミットの変更があります: {}",
            record.repo.display(),
            dirty.join(", ")
        )
    })
}

/// タスクtaskの完了の条件のうち、作業計画書docについての条件を満たさない理由を返す。
///
/// タスクにチェックが付いていて、タスクを始めたときのスナップショットから変わったのが、
/// タスクのチェックボックスと実装判断ログへの追記だけであることを確かめる。
fn plan_reasons(state: &PlanState, doc: &PlanDocument, task: &Task) -> Result<Vec<String>, Error> {
    let snapshot = state
        .snapshot
        .as_deref()
        .ok_or_else(|| Error::MissingSnapshot(task.id.clone()))?;
    let mut reasons = Vec::new();
    if !task.done {
        reasons.push(format!(
            "作業計画書のタスク {} にチェックが付いていません",
            task.id
        ));
    }
    let violations = plan::check_task_changes(snapshot, &doc.body, &task.id);
    if !violations.is_empty() {
        reasons.push(format!(
            "作業計画書で変えてよいのは、タスク {} のチェックボックスと「実装判断ログ」節への追記だけです",
            task.id
        ));
        reasons.extend(violations.iter().map(describe_violation));
    }
    Ok(reasons)
}

/// 作業計画書の変えてよい範囲の外の変更violationを、理由の文面にする。
fn describe_violation(violation: &Violation) -> String {
    match violation.line_no {
        Some(line_no) => format!("作業計画書の{line_no}行目: {}", violation.message),
        None => format!("作業計画書: {}", violation.message),
    }
}

/// 動作確認の項目itemsのうち、書き漏れのある項目を理由の文面にして返す。
fn verification_reasons(items: &[VerificationItem]) -> Vec<String> {
    items
        .iter()
        .enumerate()
        .filter(|(_, item)| {
            item.title.trim().is_empty()
                || item.target.trim().is_empty()
                || item.expected.trim().is_empty()
                || item.steps.is_empty()
        })
        .map(|(i, _)| {
            format!(
                "動作確認の{}件目の title・target・steps・expected をすべて書いてください",
                i + 1
            )
        })
        .collect()
}

/// 状態stateを、タスクtaskのステップstepに進める。タスクに属さないステップ (作業報告書) なら、taskは `None` にする。
fn start(state: &mut PlanState, task: Option<&str>, step: Step) {
    state.session_started = false;
    state.task = task.map(str::to_owned);
    state.step = Some(step);
    state.retries = 0;
    state.retry_reasons.clear();
    state.wait = None;
    state.review_before = None;
}

/// 状態stateから、進めているタスクとステップを外す。
fn clear(state: &mut PlanState) {
    state.session_started = false;
    state.task = None;
    state.step = None;
    state.retries = 0;
    state.retry_reasons.clear();
    state.wait = None;
    state.review_before = None;
}

/// 状態stateを、進めているタスクを残したまま、ステップstepで人間待ちにする。
///
/// ステップが `None` なら、再開すると作業計画書の最初の未完了のタスクから選び直す。
/// 人間が作業計画書を直したり、人が行うタスクにチェックを付けたりしたことを反映するためである。
fn wait(
    state: &mut PlanState,
    step: Option<Step>,
    kind: WaitKind,
    reason: String,
    details: Vec<String>,
) {
    state.session_started = false;
    state.step = step;
    state.retries = 0;
    state.retry_reasons.clear();
    state.review_before = None;
    state.wait = Some(Wait {
        kind,
        reason,
        details,
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::VerificationKind;

    /// ブランチを作るタスク・作業者がブランチを作らずに行うタスク・人が行うタスクを並べた作業計画書。
    pub(super) const PLAN: &str = "\
**計画書名**: demo

## 設計

状態ファイルを作る。

## 実装判断ログ

## タスクリスト

- [ ] **1-1**: [/korylus-tools] 状態ファイルを作る
  - **想定行数**: 約100行 (実装50行 + テスト50行)
- [ ] **1-2**: ヘルプページの下書きを作る
  - **担当**: implementer
- [ ] **1-3**: APIキーを発行する
  - **担当**: shimbaco
";

    pub(super) fn doc(body: &str) -> PlanDocument {
        PlanDocument::parse(body).expect("作業計画書を読み取れなかった")
    }

    /// PLANで、タスクidsにチェックを付けた作業計画書。
    pub(super) fn checked(ids: &[&str]) -> String {
        ids.iter().fold(PLAN.to_owned(), |body, id| {
            body.replace(&format!("- [ ] **{id}**"), &format!("- [x] **{id}**"))
        })
    }

    pub(super) fn new_state() -> PlanState {
        PlanState::new("https://wikino.app/s/shimbaco/pages/1", "デモ 作業計画書")
    }

    pub(super) fn task<'a>(doc: &'a PlanDocument, id: &str) -> &'a Task {
        doc.task(id).expect("タスクが無い")
    }

    pub(super) fn branch_prepared() -> Prepared {
        Prepared::Branch {
            repo: PathBuf::from("/korylus-tools"),
            dir: PathBuf::from("/korylus-tools"),
            base: "main".to_owned(),
        }
    }

    /// PLANのタスク1-1のブランチを準備し、実装のステップに進めた状態。
    pub(super) fn implementing() -> PlanState {
        let mut state = new_state();
        let plan = doc(PLAN);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        finish_prepare(&mut state, &plan, branch_prepared()).expect("準備を記録できなかった");
        state
    }

    pub(super) fn verification() -> Vec<VerificationItem> {
        vec![VerificationItem {
            kind: VerificationKind::Process,
            title: "statusで状態を表示できる".to_owned(),
            target: "korylusfactory status".to_owned(),
            steps: vec!["korylusfactory status を実行する".to_owned()],
            expected: "進行中の計画書が表示される".to_owned(),
        }]
    }

    /// タスクのブランチ demo-1-1 で、コミットを済ませて実装を終えた結果。
    pub(super) fn implemented() -> Implemented {
        Implemented {
            branch: "demo-1-1".to_owned(),
            ahead: 2,
            dirty: Vec::new(),
            verification: verification(),
        }
    }

    pub(super) fn rejected(result: Result<(), Error>) -> Vec<String> {
        match result {
            Err(Error::Rejected(reasons)) => reasons,
            other => panic!("完了を受け付けなかった理由を返さなかった: {other:?}"),
        }
    }

    /// 一時ディレクトリをルートにしたプロジェクト。
    ///
    /// 本体のリポジトリはルートで、PLANのタスク1-1のリポジトリ (/korylus-tools) とは別になる。
    pub(super) fn project() -> (tempfile::TempDir, Project) {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let project = Project {
            root: root.path().to_path_buf(),
            repo: root.path().to_path_buf(),
        };
        (root, project)
    }

    #[test]
    fn 最初の未完了のタスクの準備から始める() {
        let mut state = new_state();
        let plan = doc(PLAN);
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Prepare {
                task: task(&plan, "1-1").clone(),
                branch: "demo-1-1".to_owned(),
            }
        );
        assert_eq!(state.task.as_deref(), Some("1-1"), "進めているタスクが違う");
        assert_eq!(state.step, Some(Step::Prepare), "進めているステップが違う");
    }

    #[test]
    fn ブランチを準備したらタスクの記録と作業計画書のスナップショットを残して実装に進む() {
        let mut state = new_state();
        let plan = doc(PLAN);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        state.retries = 2;
        finish_prepare(&mut state, &plan, branch_prepared()).expect("準備を記録できなかった");

        assert_eq!(
            state.tasks,
            [TaskRecord {
                id: "1-1".to_owned(),
                repo: PathBuf::from("/korylus-tools"),
                dir: PathBuf::from("/korylus-tools"),
                branch: "demo-1-1".to_owned(),
                base: "main".to_owned(),
                prepared: true,
                review: ReviewTrail::default(),
                verification: Vec::new(),
            }],
            "タスクの記録が期待と異なる"
        );
        assert_eq!(
            state.snapshot.as_deref(),
            Some(PLAN),
            "スナップショットを残していない"
        );
        assert_eq!(state.step, Some(Step::Implement), "実装に進んでいない");
        assert_eq!(state.retries, 0, "やり直しの回数を戻していない");
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Implement {
                task: task(&plan, "1-1").clone()
            }
        );
    }

    #[test]
    fn 実装を終えたら動作確認の項目を記録してレビューに進む() {
        let (_root, project) = project();
        let mut state = implementing();
        let body = checked(&["1-1"]).replace(
            "## 実装判断ログ\n",
            "## 実装判断ログ\n\n### タスク 1-1: 状態ファイルを作る\n\n記録した判断。\n",
        );
        finish_implement(&mut state, &doc(&body), &project, implemented())
            .expect("実装の完了を受け付けなかった");

        assert_eq!(
            state.tasks[0].verification,
            verification(),
            "動作確認の項目を記録していない"
        );
        assert_eq!(
            (state.task.as_deref(), state.step),
            (Some("1-1"), Some(Step::Review)),
            "レビューに進んでいない"
        );
    }

    #[test]
    fn 実装の完了の条件を満たさなければ状態を変えずに理由を返す() {
        let (_root, project) = project();
        let mut state = implementing();
        let before = state.clone();
        let mut item = verification().remove(0);
        item.steps.clear();
        let result = finish_implement(
            &mut state,
            &doc(PLAN),
            &project,
            Implemented {
                branch: "main".to_owned(),
                ahead: 0,
                dirty: vec![" M src/lib.rs".to_owned()],
                verification: vec![verification().remove(0), item],
            },
        );
        assert_eq!(
            rejected(result),
            [
                "/korylus-tools がタスクのブランチ demo-1-1 ではなく main にいます",
                "/korylus-tools に未コミットの変更があります:  M src/lib.rs",
                "作業計画書のタスク 1-1 にチェックが付いていません",
                "動作確認の2件目の title・target・steps・expected をすべて書いてください",
            ]
        );
        assert_eq!(state, before, "受け付けなかった完了で状態を変えた");
    }

    #[test]
    fn ベースより先のコミットが無ければ実装の完了を受け付けない() {
        let (_root, project) = project();
        let mut state = implementing();
        let result = finish_implement(
            &mut state,
            &doc(&checked(&["1-1"])),
            &project,
            Implemented {
                ahead: 0,
                ..implemented()
            },
        );
        assert_eq!(
            rejected(result),
            ["ブランチ demo-1-1 に、ベース main より先のコミットがありません"]
        );
    }

    #[test]
    fn 作業計画書の変えてよい範囲の外が変わっていれば実装の完了を受け付けない() {
        let (_root, project) = project();
        let mut state = implementing();
        let body = checked(&["1-1", "1-2"]).replace("状態ファイルを作る。", "設計を書き換えた。");
        assert_eq!(
            rejected(finish_implement(
                &mut state,
                &doc(&body),
                &project,
                implemented()
            )),
            [
                "作業計画書で変えてよいのは、タスク 1-1 のチェックボックスと「実装判断ログ」節への追記だけです",
                "作業計画書の5行目: 「設計」節が変更されています",
                "作業計画書の13行目: 「タスクリスト」節で、タスク 1-1 のチェック以外の行が変更されています",
            ]
        );
    }

    #[test]
    fn 人が行うタスクは人間を待ち_チェックを付けて再開すると次へ進む() {
        let mut state = new_state();
        let plan = doc(&checked(&["1-1", "1-2"]));
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        finish_prepare(&mut state, &plan, Prepared::Human).expect("準備を記録できなかった");

        let want = Wait {
            kind: WaitKind::HumanTask,
            reason: "人が行うタスクです".to_owned(),
            details: vec!["1-3 APIキーを発行する".to_owned()],
        };
        assert_eq!(state.wait.as_ref(), Some(&want), "人間を待っていない");
        assert_eq!(
            state.task.as_deref(),
            Some("1-3"),
            "待っているタスクを残していない"
        );
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Wait(want),
            "人間を待っている間に先へ進んだ"
        );

        // チェックを付けずに再開したときは、同じタスクをもう一度準備する。
        state.wait = None;
        assert!(
            matches!(
                next(&mut state, &plan).expect("次に行うことを決められなかった"),
                Next::Prepare { task, .. } if task.id == "1-3"
            ),
            "チェックの無い人が行うタスクを飛ばした"
        );

        let plan = doc(&checked(&["1-1", "1-2", "1-3"]));
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Report { previous: None },
            "チェックを付けた人が行うタスクの次へ進まなかった"
        );
        assert_eq!(
            (state.task.as_deref(), state.step),
            (None, Some(Step::Report)),
            "作業報告書のステップに待っていたタスクが残った"
        );
    }

    #[test]
    fn ブランチを作らずに行うタスクは作業のステップに進み_チェックが付いたら終える() {
        let mut state = new_state();
        let before = checked(&["1-1"]);
        let plan = doc(&before);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        finish_prepare(&mut state, &plan, Prepared::Work).expect("準備を記録できなかった");
        assert_eq!(state.step, Some(Step::Work), "作業のステップに進んでいない");
        assert!(
            state.tasks.is_empty(),
            "ブランチを作らないタスクの記録を作った"
        );
        assert_eq!(
            state.snapshot.as_deref(),
            Some(before.as_str()),
            "スナップショットを残していない"
        );
        assert_eq!(
            next(&mut state, &plan).expect("次に行うことを決められなかった"),
            Next::Work {
                task: task(&plan, "1-2").clone()
            }
        );

        assert_eq!(
            rejected(finish_work(&mut state, &plan)),
            ["作業計画書のタスク 1-2 にチェックが付いていません"]
        );
        finish_work(&mut state, &doc(&checked(&["1-1", "1-2"])))
            .expect("作業の完了を受け付けなかった");
        assert_eq!(
            (state.task.as_deref(), state.step),
            (None, None),
            "タスクを終えていない"
        );
    }

    #[test]
    fn ブランチを準備できなければ人間を待ち_再開すると準備し直す() {
        let mut state = new_state();
        let plan = doc(PLAN);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        let failed = Prepared::Failed {
            reason:
                "/korylus-tools に未コミットの変更があるため、ブランチ demo-1-1 を準備できません"
                    .to_owned(),
            details: vec![" M Cargo.toml".to_owned()],
        };
        finish_prepare(&mut state, &plan, failed).expect("準備を記録できなかった");

        let wait = state.wait.clone().expect("人間を待っていない");
        assert_eq!(wait.kind, WaitKind::Branch, "人間を待つ場面が違う");
        assert_eq!(wait.details, [" M Cargo.toml"], "理由の詳細を残していない");
        assert!(
            state.tasks.is_empty(),
            "準備できなかったタスクの記録を作った"
        );

        state.wait = None;
        assert!(
            matches!(
                next(&mut state, &plan).expect("次に行うことを決められなかった"),
                Next::Prepare { task, .. } if task.id == "1-1"
            ),
            "再開したあとに準備し直さなかった"
        );
    }

    #[test]
    fn 準備し直すときは前に準備したタスクの記録を作り直す() {
        let mut state = implementing();
        state.step = Some(Step::Prepare);
        finish_prepare(&mut state, &doc(PLAN), Prepared::Work).expect("準備を記録できなかった");
        assert!(state.tasks.is_empty(), "前に準備したブランチの記録が残った");
    }

    #[test]
    fn 準備のステップで止まっていたら作業計画書から次のタスクを選び直す() {
        let mut state = new_state();
        next(&mut state, &doc(PLAN)).expect("次に行うことを決められなかった");
        let plan = doc(&checked(&["1-1"]));
        assert!(
            matches!(
                next(&mut state, &plan).expect("次に行うことを決められなかった"),
                Next::Prepare { task, .. } if task.id == "1-2"
            ),
            "人間がチェックを付けたタスクを準備し直した"
        );
    }

    #[test]
    fn 完了を記録した作業計画書は終わりを返す() {
        let mut state = new_state();
        state.completed_at = Some(
            "2026-11-16T05:15:40Z"
                .parse()
                .expect("日時を読み取れなかった"),
        );
        assert_eq!(
            next(&mut state, &doc(PLAN)).expect("次に行うことを決められなかった"),
            Next::Finished,
            "完了した作業計画書のタスクを進めた"
        );
    }

    #[test]
    fn 進めているステップと異なるステップの完了は記録しない() {
        let (_root, project) = project();
        let mut state = new_state();
        let plan = doc(PLAN);
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        let err = finish_implement(&mut state, &plan, &project, implemented())
            .expect_err("準備のステップで実装の完了を記録した");
        assert!(
            matches!(
                err,
                Error::WrongStep {
                    expected: Step::Implement,
                    actual: Some(Step::Prepare),
                }
            ),
            "エラー = {err:?}"
        );
        assert_eq!(
            err.to_string(),
            "実装の完了を記録しようとしましたが、進めているステップはタスクの準備です"
        );
    }

    #[test]
    fn 作業計画書から消えたタスクは進められない() {
        let mut state = implementing();
        let body = PLAN.replace("**1-1**", "**1-0**");
        let err = next(&mut state, &doc(&body)).expect_err("消えたタスクを進めた");
        assert!(
            matches!(&err, Error::MissingTask(id) if id == "1-1"),
            "エラー = {err:?}"
        );
    }

    #[test]
    fn 計画書名の行が無ければブランチの名前を決められない() {
        let mut state = new_state();
        let body = PLAN.replace("**計画書名**: demo\n", "");
        let err = next(&mut state, &doc(&body)).expect_err("ブランチの名前を決めた");
        assert!(matches!(err, Error::NoPlanName), "エラー = {err:?}");
        assert_eq!(
            state.step, None,
            "ブランチの名前を決められないのに準備を始めた"
        );
    }
}
