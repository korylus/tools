//! `korylusfactory start` の処理。監督のループ。
//!
//! ロードマップで承認された作業計画書を1本ずつ選び、状態遷移に次に行うことを尋ね、
//! ブランチの準備かセッションの起動を行い、結果とリポジトリの状態を確かめて記録する、を止めるまで繰り返す。
//! 人間を待つ間と、ロードマップに作業計画書が現れるのを待つ間は、間隔を空けて読み直す。
//!
//! 状態ファイルの読み取りから次のステップの判定・書き込みまでは、1つのファイルロックの中で行う。
//! セッションの実行中は、ロックを持たない。
//!
//! 止めるときのシグナルは扱わず、既定の動作で終わる。
//! 実行中のセッションは、セッションの監督プロセスが呼び出し元の終わりを取り消しとして扱い、子孫まで終わらせる。
//! 状態はセッションの結果を記録する前のまま残るため、次に起動したときに同じステップをやり直す。

mod prepare;
mod session;
#[cfg(test)]
mod tests;

use std::collections::HashSet;
use std::fmt::{self, Write as _};
use std::fs::{self, File, OpenOptions, TryLockError};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use jiff::{SignedDuration, Timestamp};
use korylusfactory_core::flow::{self, Next, PlanDocument, Prepared, Project, Task};
use korylusfactory_core::queue::{self, Head};
use korylusfactory_core::state::{self, PlanState, Store, Wait, WaitKind};

use crate::completion::{self, Completion};
use crate::config::{self, Config};
use crate::process::{Launcher, Runner};
use crate::roadmap::{self, Entry, State};
use crate::source::{self, Kind, Wikino};
use crate::version::{self, Build, Freshness};
use crate::{agent, branch, describe, instruction, status};
use session::Launch;

/// 二重起動を防ぐロックに使うファイルの、プロジェクトのルートからの相対パス。
pub const LOCK_PATH: &str = "docs/private/factory/start.lock";

/// セッションの子孫の回収が終わるまで監督プロセスが保持する排他のファイル。
pub const SESSION_LOCK_PATH: &str = "docs/private/factory/session.lock";

/// 未コミットの変更に数えない、リポジトリのルートからの相対パス。作業計画書やレビュードキュメントなど、コミットしないものを置く。
const EXCLUDED: [&str; 1] = ["docs/private"];

/// `korylusfactory start` を続けられないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 同じプロジェクトで、ほかの `korylusfactory start` が動いている。
    #[error(
        "このプロジェクトでは、ほかの korylusfactory start が動いています (ロック: {})",
        .0.display()
    )]
    Running(PathBuf),

    /// 前のstartが終了していても、セッションの子孫をまだ回収している。
    #[error("前のセッションを終了処理中です。回収が終わってから起動してください (ロック: {})", .0.display())]
    Finishing(PathBuf),

    /// 二重起動を防ぐロックを取れない。
    #[error("二重起動を防ぐロック {} を取れません", .path.display())]
    Lock {
        /// ロックに使うファイルのパス。
        path: PathBuf,
        /// 取れなかった理由。
        #[source]
        source: io::Error,
    },

    /// 進み具合を書き出せない。
    #[error("進み具合を書き出せません")]
    Output(#[source] io::Error),

    /// 設定ファイルを読めない。
    #[error(transparent)]
    Config(#[from] config::Error),

    /// 状態ファイルを読み書きできない。
    #[error(transparent)]
    State(#[from] state::Error),

    /// ロードマップを読み取れない。
    #[error(transparent)]
    Roadmap(#[from] roadmap::Error),

    /// 作業計画書を読めない。
    #[error(transparent)]
    Source(#[from] source::Error),

    /// 作業計画書を読み取れない。
    #[error("作業計画書 {url} を読み取れません")]
    Plan {
        /// 作業計画書のURL。
        url: String,
        /// 読み取れなかった理由。
        #[source]
        source: flow::PlanError,
    },

    /// リポジトリの状態を確かめられない。
    #[error(transparent)]
    Branch(#[from] branch::Error),

    /// 状態を進められない。
    #[error(transparent)]
    Flow(#[from] flow::Error),

    /// セッションへの指示を作れない。
    #[error(transparent)]
    Instruction(#[from] instruction::Error),

    /// セッションを起動できない。
    #[error(transparent)]
    Agent(#[from] agent::Error),

    /// セッションの記録を残すディレクトリを作れない。
    #[error("セッションの記録 {} を作れません", .path.display())]
    Logs {
        /// 作ろうとしたディレクトリ。
        path: PathBuf,
        /// 作れなかった理由。
        #[source]
        source: io::Error,
    },

    /// 進行中の作業計画書が複数あり、どれを進めるかを決められない。
    #[error(
        "進行中の作業計画書が複数あります: {}。進めない作業計画書の状態の置き場を片付けてください",
        .0.join("・")
    )]
    Ambiguous(Vec<String>),
}

/// 待つ時間と、セッションの時間の上限。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Timing {
    /// セッションの時間の上限。
    pub session: Duration,
    /// 同じステップをやり直す前に空ける間隔。
    pub retry: Duration,
    /// 進める作業計画書が無いときと、作業報告書の確認を待つ間に、ロードマップを読み直す間隔。
    pub roadmap: Duration,
    /// 人間を待つ間に、`korylusfactory resume` で再開されたかを状態ファイルで確かめる間隔。
    pub state: Duration,
}

impl Default for Timing {
    fn default() -> Self {
        Self {
            session: agent::DEFAULT_TIMEOUT,
            retry: Duration::from_mins(5),
            roadmap: Duration::from_mins(5),
            state: Duration::from_secs(5),
        }
    }
}

/// 現在の日時と、待つこと。テストでは差し替えて、実際には待たない。
pub trait Clock {
    /// 現在の日時を返す。
    fn now(&self) -> Timestamp;

    /// durationの間、待つ。
    fn sleep(&self, duration: Duration);
}

/// 本物の時計。
#[derive(Debug, Clone, Copy, Default)]
pub struct SystemClock;

impl Clock for SystemClock {
    fn now(&self) -> Timestamp {
        Timestamp::now()
    }

    fn sleep(&self, duration: Duration) {
        thread::sleep(duration);
    }
}

/// `korylusfactory start` が使う、外部とのやり取りと設定。
#[derive(Clone, Copy)]
pub struct Start<'a> {
    /// gitとwikinoを実行する。
    pub runner: &'a dyn Runner,
    /// セッションを起動する。
    pub launcher: &'a dyn Launcher,
    /// 現在の日時を返し、待つ。
    pub clock: &'a dyn Clock,
    /// 実行しているkorylusfactoryのビルド元の情報。古いかどうかを確かめる。
    pub build: &'a Build,
    /// プロジェクトのルート。
    pub root: &'a Path,
    /// 待つ時間と、セッションの時間の上限。
    pub timing: Timing,
}

impl fmt::Debug for Start<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Start")
            .field("build", self.build)
            .field("root", &self.root)
            .field("timing", &self.timing)
            .finish_non_exhaustive()
    }
}

/// プロジェクトのルートで、作業計画書を止めるまで進め続ける。進み具合はout、エラーと警告はerrに書く。
///
/// cancelが真になったら、実行中のセッションを終わらせて戻る。
/// 設定ファイルを読めないときと、同じプロジェクトでほかの `korylusfactory start` が動いているときは、エラーで終わる。
/// ループの中で起きたエラーは、表示してから間隔を空けて読み直す。
pub fn run(
    start: &Start<'_>,
    out: &mut dyn Write,
    err: &mut dyn Write,
    cancel: &AtomicBool,
) -> Result<(), Error> {
    let config = config::load(start.root)?;
    let _running = hold(start.root)?;
    // 前のstartが先に終了しても、そのセッションの後始末が終わるまでは起動しない。
    drop(
        hold_path(start.root.join(SESSION_LOCK_PATH)).map_err(|err| match err {
            Error::Running(path) => Error::Finishing(path),
            err => err,
        })?,
    );
    let project = Project {
        root: start.root.to_path_buf(),
        repo: branch::toplevel(start.runner, start.root)?,
    };
    let mut director = Director {
        start,
        config,
        project,
        store: Store::new(start.root),
        out,
        err,
        shown: HashSet::new(),
        roadmap_read: None,
    };
    let freshness = version::check(start.runner, start.build);
    director.say(&start.build.summary())?;
    director.version_notice(&freshness)?;
    director
        .say("ロードマップで承認された作業計画書を進めます。止めるには Ctrl-C を押してください")?;
    while !cancel.load(Ordering::SeqCst) {
        let pace = match director.tick(cancel) {
            Ok(pace) => pace,
            Err(Error::Output(source)) => return Err(Error::Output(source)),
            Err(err) => {
                director.diagnostic(format!(
                    "{}\n  {}分後に読み直します",
                    describe(&err),
                    minutes(start.timing.roadmap)
                ))?;
                Pace::Sleep(start.timing.roadmap)
            }
        };
        if let Pace::Sleep(duration) = pace
            && !cancel.load(Ordering::SeqCst)
        {
            start.clock.sleep(duration);
        }
    }
    Ok(())
}

/// プロジェクトのルートrootで、二重起動を防ぐロックを取る。返したファイルを閉じると、ロックが外れる。
///
/// プロセスが終われば、止め方にかかわらずロックが外れる。
fn hold(root: &Path) -> Result<File, Error> {
    hold_path(root.join(LOCK_PATH))
}

/// pathの排他を待たずに取り、保持するファイルを返す。
fn hold_path(path: PathBuf) -> Result<File, Error> {
    let lock_error = |source| Error::Lock {
        path: path.clone(),
        source,
    };
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(lock_error)?;
    }
    let file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(false)
        .open(&path)
        .map_err(lock_error)?;
    match file.try_lock() {
        Ok(()) => Ok(file),
        Err(TryLockError::WouldBlock) => Err(Error::Running(path)),
        Err(TryLockError::Error(source)) => Err(Error::Lock { path, source }),
    }
}

/// ループの1回を終えたあとに、次の回をいつ始めるか。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Pace {
    /// すぐに次の回を始める。
    Continue,
    /// 間隔を空けてから次の回を始める。
    Sleep(Duration),
}

/// 監督のループ。作業計画書を1本ずつ選んで進める。
struct Director<'a, 's> {
    start: &'a Start<'s>,
    config: Config,
    project: Project,
    store: Store,
    out: &'a mut dyn Write,
    err: &'a mut dyn Write,
    /// 前に進んでから表示した、待っている理由とエラー。進むまで、同じものを繰り返し表示しない。
    shown: HashSet<String>,
    /// 作業報告書の確認を待つ間に、最後にロードマップを読んだ日時。
    roadmap_read: Option<Timestamp>,
}

impl Director<'_, '_> {
    /// ループの1回。進行中の作業計画書があればそれを、無ければロードマップの並びの先頭の作業計画書を進める。
    fn tick(&mut self, cancel: &AtomicBool) -> Result<Pace, Error> {
        let mut active = self.store.active()?;
        let (name, state) = match active.len() {
            0 => match self.begin()? {
                Some(begun) => begun,
                None => return Ok(Pace::Sleep(self.start.timing.roadmap)),
            },
            1 => active.remove(0),
            _ => {
                return Err(Error::Ambiguous(
                    active.into_iter().map(|(name, _)| name).collect(),
                ));
            }
        };
        match &state.wait {
            Some(wait) => self.waiting(&name, &state, wait),
            None => self.advance(&name, cancel),
        }
    }

    /// ロードマップの並びの先頭の作業計画書を始め、計画書名と状態を返す。
    ///
    /// 進める作業計画書が無いときと、先頭を読み取れないときは、理由を表示して `None` を返す。
    fn begin(&mut self) -> Result<Option<(String, PlanState)>, Error> {
        let entries = roadmap::load(self.start.runner, &self.config, &self.store)?;
        let (url, name) = match queue::head(&entries) {
            Head::Empty => {
                self.notice("進める作業計画書がありません。ロードマップの承認の節に作業計画書が現れるのを待ちます".to_owned())?;
                return Ok(None);
            }
            Head::Blocked(entry) => {
                self.notice(blocked(entry))?;
                return Ok(None);
            }
            Head::Start { url, name } => (url.to_owned(), name.to_owned()),
        };
        let title = entries
            .first()
            .map(|entry| entry.candidate.link.title.clone())
            .unwrap_or_default();
        // 古いkorylusfactoryで作業計画書を始めないよう、始める前にも確かめる。
        let freshness = version::check(self.start.runner, self.start.build);
        if matches!(freshness, Freshness::Stale { .. }) {
            self.version_notice(&freshness)?;
        }
        let lock = self.store.plan(&name)?.lock()?;
        let state = if let Some(state) = lock.load()? {
            state
        } else {
            let state = PlanState::new(&url, title);
            lock.save(&state)?;
            state
        };
        drop(lock);
        self.say(&format!("作業計画書 {name} を始めます: {url}"))?;
        Ok(Some((name, state)))
    }

    /// 人間を待っている作業計画書nameの理由を表示する。
    ///
    /// 作業報告書の確認を待っているときは、ロードマップを読み直す間隔ごとに完了を確かめ、完了していれば記録する。
    fn waiting(&mut self, name: &str, state: &PlanState, wait: &Wait) -> Result<Pace, Error> {
        let mut text = format!(
            "{name}: 人間を待っています ({}): {}",
            wait.kind.label(),
            wait.reason
        );
        for detail in &wait.details {
            text.push_str("\n  ");
            text.push_str(detail);
        }
        if wait.kind != WaitKind::Report {
            text.push_str("\n  対応したら korylusfactory resume を実行してください");
        }
        self.notice(text)?;
        if wait.kind == WaitKind::Report && self.roadmap_due() {
            self.roadmap_read = Some(self.start.clock.now());
            match completion::check(self.start.runner, &self.config, state) {
                Ok(Completion::Done) => return self.complete(name),
                Ok(Completion::NotListed) => {}
                Ok(Completion::Unmerged(branches)) => {
                    let mut text = format!(
                        "{name}: ロードマップの完了とみなす節にありますが、リモートの既定のベースに取り込まれていないブランチがあります"
                    );
                    for branch in branches {
                        let note = if branch.missing {
                            "。ローカルにありません"
                        } else {
                            ""
                        };
                        let _ = write!(
                            text,
                            "\n  {} {} ({}{note})",
                            branch.repo.display(),
                            branch.branch,
                            branch.into
                        );
                    }
                    self.notice(text)?;
                }
                Err(err) => {
                    self.diagnostic(format!(
                        "{name}: 完了を確かめられません: {}",
                        describe(&err)
                    ))?;
                }
            }
        }
        Ok(Pace::Sleep(self.start.timing.state))
    }

    /// 作業報告書の確認を待つ間に、前にロードマップを読んでから、読み直す間隔が過ぎたかを返す。
    fn roadmap_due(&self) -> bool {
        let interval =
            SignedDuration::try_from(self.start.timing.roadmap).unwrap_or(SignedDuration::MAX);
        let now = self.start.clock.now();
        self.roadmap_read
            .is_none_or(|read| now.duration_since(read) >= interval)
    }

    /// 作業報告書の確認を待っている作業計画書nameの完了を記録する。
    fn complete(&mut self, name: &str) -> Result<Pace, Error> {
        let lock = self.store.plan(name)?.lock()?;
        // ロックを取るまでの間に再開されていれば、完了を記録しない。
        if let Some(mut state) = lock.load()?
            && state
                .wait
                .as_ref()
                .is_some_and(|wait| wait.kind == WaitKind::Report)
        {
            flow::complete(&mut state, self.start.clock.now())?;
            lock.save(&state)?;
            drop(lock);
            self.roadmap_read = None;
            self.say(&format!("作業計画書 {name} の完了を記録しました"))?;
        }
        Ok(Pace::Continue)
    }

    /// 作業計画書nameの次に行うことを決め、ブランチの準備かセッションを1つ行う。
    fn advance(&mut self, name: &str, cancel: &AtomicBool) -> Result<Pace, Error> {
        let dir = self.store.plan(name)?;
        let lock = dir.lock()?;
        // ロックを取るまでの間に、`korylusfactory resume` などが状態を変えていることがあるため、ロックの中で読み直す。
        let Some(mut state) = lock.load()? else {
            return Ok(Pace::Continue);
        };
        if state.wait.is_some() || state.is_completed() {
            return Ok(Pace::Continue);
        }
        let doc = self.read_plan(&state.url)?;
        let before = state.clone();
        let next = flow::next(&mut state, &doc)?;
        if let Next::Prepare { task, branch } = &next {
            // ブランチの準備はすぐ終わるため、判定から記録までを同じロックの中で行う。
            let prepared = prepare::prepare(self.start.runner, &state, &doc, task, branch);
            flow::finish_prepare(&mut state, &doc, prepared.clone())?;
            lock.save(&state)?;
            drop(lock);
            self.prepared(name, task, branch, &prepared)?;
            return Ok(Pace::Continue);
        }
        if state != before {
            lock.save(&state)?;
        }
        if matches!(next, Next::Wait(_) | Next::Finished) {
            return Ok(Pace::Continue);
        }
        // 指示と記録のディレクトリを用意できてから開始の印を保存し、起動しなかったセッションを中断として伝えない。
        flow::begin_session(&mut state)?;
        let Some(launch) = Launch::new(&dir, &state, &next, self.start.clock.now())? else {
            return Ok(Pace::Continue);
        };
        lock.save(&state)?;
        drop(lock);
        session::run(self, &dir, name, &state, &next, launch, cancel)
    }

    /// タスクtaskの準備の結果preparedを表示する。人間を待つときは、次の回に待っている理由を表示する。
    fn prepared(
        &mut self,
        name: &str,
        task: &Task,
        branch: &str,
        prepared: &Prepared,
    ) -> Result<(), Error> {
        match prepared {
            Prepared::Branch { repo, base, .. } => self.say(&format!(
                "{name}: タスク {} のブランチ {branch} を {} に準備しました (元のブランチ: {base})",
                task.id,
                repo.display()
            )),
            Prepared::Work => self.say(&format!(
                "{name}: タスク {} は、ブランチを作らずに行います",
                task.id
            )),
            Prepared::Human | Prepared::Failed { .. } => Ok(()),
        }
    }

    /// URLがurlの作業計画書を読み、読み取る。
    fn read_plan(&self, url: &str) -> Result<PlanDocument, Error> {
        let body = Wikino::parse_as(Kind::Plan, url)?.read(self.start.runner)?;
        PlanDocument::parse(body).map_err(|source| Error::Plan {
            url: url.to_owned(),
            source,
        })
    }

    /// 進み具合textを表示する。進んだため、待っている理由を表示し直せるようにする。
    fn say(&mut self, text: &str) -> Result<(), Error> {
        self.shown.clear();
        write_line(self.out, text)
    }

    /// 待っている理由かエラーtextを表示する。前に進んでから同じものを表示していれば、表示しない。
    fn notice(&mut self, text: String) -> Result<(), Error> {
        if self.shown.contains(&text) {
            return Ok(());
        }
        write_line(self.out, &text)?;
        self.shown.insert(text);
        Ok(())
    }

    /// エラーと警告を、標準エラーに対応する出力先へ1回だけ書く。
    fn diagnostic(&mut self, text: String) -> Result<(), Error> {
        if self.shown.contains(&text) {
            return Ok(());
        }
        write_line(self.err, &text)?;
        self.shown.insert(text);
        Ok(())
    }

    fn version_notice(&mut self, freshness: &Freshness) -> Result<(), Error> {
        let text = status::version_diagnostics(self.start.build, freshness);
        if !text.is_empty() {
            self.diagnostic(text)?;
        }
        Ok(())
    }
}

/// 出力先outへ、末尾の改行を補って書き出す。
fn write_line(out: &mut dyn Write, text: &str) -> Result<(), Error> {
    let newline = if text.ends_with('\n') { "" } else { "\n" };
    write!(out, "{text}{newline}")
        .and_then(|()| out.flush())
        .map_err(Error::Output)
}

/// 読み取れない作業計画書entryで待つ理由を返す。
fn blocked(entry: &Entry) -> String {
    let reason = match &entry.state {
        State::NotFound => "下書きもページも見つかりません".to_owned(),
        State::Invalid { url, reason } => {
            format!("{url} を作業計画書として読み取れません: {reason}")
        }
        State::Ready { url, .. } => format!("{url} を読み取れました"),
    };
    format!(
        "ロードマップの「{}」の節の先頭の作業計画書 [[{}]] は、{reason}。飛ばさずに、直されるのを待ちます",
        entry.candidate.section, entry.candidate.link.title
    )
}

/// 次に行うことnextを、「タスク 1-1 の実装」の形にする。
fn describe_next(next: &Next) -> String {
    match next {
        Next::Prepare { task, .. } => format!("タスク {} の準備", task.id),
        Next::Implement { task } => format!("タスク {} の実装", task.id),
        Next::Work { task } => format!("タスク {} のブランチを作らない作業", task.id),
        Next::Review { task, number, .. } => format!("タスク {} の{number}回目のレビュー", task.id),
        Next::Fix { task, .. } => format!("タスク {} の修正", task.id),
        Next::Report { .. } => "作業報告書の作成".to_owned(),
        Next::ReportReview { number, .. } => format!("作業報告書の{number}回目のレビュー"),
        Next::ReportFix { .. } => "作業報告書の修正".to_owned(),
        Next::Wait(wait) => format!("{}の人間待ち", wait.kind.label()),
        Next::Finished => "作業計画書の完了".to_owned(),
    }
}

/// durationを、表示に使う分の数にする。
fn minutes(duration: Duration) -> u64 {
    duration.as_secs() / 60
}
