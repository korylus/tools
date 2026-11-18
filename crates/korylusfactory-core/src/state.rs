//! 作業計画書ごとの進行を、状態ファイルに読み書きする。
//!
//! 状態は、プロジェクトのルートの [`ROOT`] の下に、計画書名ごとのディレクトリを作って置く。
//!
//! ```text
//! <計画書名>/state.json  形の版・計画書の進行・タスクごとの記録・作業計画書のスナップショット
//! <計画書名>/state.lock  ファイルロックに使う空のファイル
//! ```
//!
//! 状態を変えるときは、[`PlanDir::lock`] で取ったロックの中で、読み取りから書き込みまでを行う。
//! 書き込みは一時ファイルに書いてから置き換えるため、ロックを取らずに読んでも書きかけの状態は見えない。

mod item;
mod record;

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write as _};
use std::path::{Path, PathBuf};

use jiff::Timestamp;
use serde::{Deserialize, Serialize};

pub use item::{
    ItemLocation, ItemPage, NOTICES_PATH, Notice, NoticeKind, Notices, Notified, QueuedItem,
    WaitItem,
};
pub use record::{
    DeferredFinding, HumanFinding, HumanReview, ReviewRecord, ReviewTrail, Reviewer, TaskRecord,
    Verdict, VerificationItem, VerificationKind,
};

/// プロジェクトのルートからの、状態を置くディレクトリの相対パス。
pub const ROOT: &str = "docs/private/factory/plans";

/// このkorylusfactoryが読み書きする、状態ファイルの形の版。
pub const VERSION: u32 = 1;

/// 状態ファイルの名前。
const STATE_FILE: &str = "state.json";

/// 状態ファイルを置き換える前に書く、一時ファイルの名前。書き込みはロックの中だけで行うため、名前は固定でよい。
const TEMP_FILE: &str = "state.json.tmp";

/// ファイルロックに使うファイルの名前。
const LOCK_FILE: &str = "state.lock";

/// 状態ファイルを読み書きできないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 計画書名に、状態の置き場のディレクトリの名前に使えない文字がある。
    #[error(
        "計画書名 {0:?} を状態の置き場に使えません。英小文字・数字と、間に挟むハイフンだけを使ってください"
    )]
    InvalidName(String),

    /// 状態の置き場のディレクトリを読めない。
    #[error("状態の置き場 {} を読めません", .path.display())]
    List {
        /// 状態の置き場のパス。
        path: PathBuf,
        /// 読めなかった理由。
        #[source]
        source: io::Error,
    },

    /// 状態ファイルを読めない。
    #[error("状態ファイル {} を読めません", .path.display())]
    Read {
        /// 状態ファイルのパス。
        path: PathBuf,
        /// 読めなかった理由。
        #[source]
        source: io::Error,
    },

    /// 状態ファイルがJSONとして読めないか、項目が誤っている。
    #[error("状態ファイル {} の書き方が誤っています", .path.display())]
    Parse {
        /// 状態ファイルのパス。
        path: PathBuf,
        /// 誤っている箇所。
        #[source]
        source: serde_json::Error,
    },

    /// 状態ファイルの形の版が、このkorylusfactoryの読める版と異なる。
    #[error(
        "状態ファイル {} の形の版 {version} を読めません。このkorylusfactoryが読めるのは版 {VERSION} です",
        .path.display()
    )]
    UnsupportedVersion {
        /// 状態ファイルのパス。
        path: PathBuf,
        /// 状態ファイルに書かれていた形の版。
        version: u32,
    },

    /// 状態をJSONにできない。
    #[error("状態ファイル {} に書く内容を作れません", .path.display())]
    Encode {
        /// 状態ファイルのパス。
        path: PathBuf,
        /// 作れなかった理由。
        #[source]
        source: serde_json::Error,
    },

    /// 状態ファイルを書けない。
    #[error("状態ファイル {} を書けません", .path.display())]
    Write {
        /// 状態ファイルのパス。
        path: PathBuf,
        /// 書けなかった理由。
        #[source]
        source: io::Error,
    },

    /// 状態ファイルのロックを取れない。
    #[error("状態ファイルのロック {} を取れません", .path.display())]
    Lock {
        /// ロックに使うファイルのパス。
        path: PathBuf,
        /// 取れなかった理由。
        #[source]
        source: io::Error,
    },
}

/// 1つの作業計画書の進行。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PlanState {
    /// 作業計画書のページのURL。
    pub url: String,
    /// 作業計画書を進め始めたときに、ロードマップの項目のWikiリンクに書かれていた作業計画書のタイトル。
    ///
    /// 完了とみなす節のリンクのうち、作業計画書のページへ解決して確かめるものを、このタイトルで絞る。
    pub title: String,
    /// 進めているタスクの番号。
    ///
    /// タスクに属さないステップ (作業報告書を書く・レビューする・直す) を進めているときと、
    /// 進めているステップが無いときは `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub task: Option<String>,
    /// 進めているステップ。まだ選んでいなければ `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub step: Option<Step>,
    /// 進めているステップを、途中で止まったか完了の条件を満たさなかったためにやり直した回数。
    #[serde(default)]
    pub retries: u32,
    /// 進めているステップをやり直すときにセッションへ伝える、前のセッションが途中で止まった理由か、満たせなかった完了の条件。
    ///
    /// `korylusfactory start` を起動し直しても、やり直しの指示に添えられるよう状態に残す。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub retry_reasons: Vec<String>,
    /// セッションを起動して、結果の確認と記録をまだ終えていない。再起動時に中断を見分ける。
    #[serde(default)]
    pub session_started: bool,
    /// 人間を待っている理由。待っていなければ `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub wait: Option<Wait>,
    /// 進めているステップのセッションからの質問に、人間が人間キューの項目で答えた回答。答えた順に並べる。
    ///
    /// 同じステップで何度質問しても前の回答が要るため、やり直すセッションの指示にすべて添え、ステップを進めたら外す。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub answers: Vec<HumanAnswer>,
    /// 作業計画書の完了 (マージ済み) を記録した時刻。完了していなければ `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completed_at: Option<Timestamp>,
    /// 作業計画書の変えてよい範囲を確かめるときに、比べる元にする作業計画書の本文。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub snapshot: Option<String>,
    /// レビューのステップを始めたときの、最新のレビュードキュメントの、プロジェクトのルートからの相対パス。
    ///
    /// これより新しい連番のドキュメントだけを、そのステップのレビューとして受け付ける。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub review_before: Option<PathBuf>,
    /// 扱ったタスクの記録。扱い始めた順に並べる。
    ///
    /// 作業報告書のスキルが材料として読むため、空でも書く。
    #[serde(default)]
    pub tasks: Vec<TaskRecord>,
    /// 書いた作業報告書の記録。回の順に並べる。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub report_rounds: Vec<ReportRound>,
    /// 人間待ちに積んで、まだ片付けていない人間キューの項目。積んだ順に並べる。
    ///
    /// 待ちが解けたあとも、項目の下書きを片付けるまで残す。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub items: Vec<WaitItem>,
    /// 通知のコマンドで最後に知らせた人間待ち。同じ待ちを、通知のコマンドで繰り返し知らせないために使う。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub notified: Option<Notified>,
}

/// 作業報告書1回分の記録。
///
/// 作業報告書の本文は読まず、場所と、処理の流れを書かなかった理由と、作業報告書のレビューの記録だけを持つ。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReportRound {
    /// 何回目の作業報告書か (1始まり)。
    pub number: u32,
    /// 作業報告書を記録した時刻。
    pub at: Timestamp,
    /// 作業報告書のページのURL。
    pub location: String,
    /// 処理の流れを書かなかった理由。書いたときは `None` で、作業報告書をレビューする。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub walkthrough_skipped: Option<String>,
    /// 作業報告書のレビューの記録。
    #[serde(flatten)]
    pub review: ReviewTrail,
}

impl PlanState {
    /// URLがurlで、ロードマップの項目のWikiリンクにタイトルtitleで書かれた作業計画書の、まだ何も進めていない進行を返す。
    pub fn new(url: impl Into<String>, title: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            title: title.into(),
            task: None,
            step: None,
            retries: 0,
            retry_reasons: Vec::new(),
            session_started: false,
            wait: None,
            answers: Vec::new(),
            completed_at: None,
            snapshot: None,
            review_before: None,
            tasks: Vec::new(),
            report_rounds: Vec::new(),
            items: Vec::new(),
            notified: None,
        }
    }

    /// 作業計画書の完了を記録したかを返す。
    pub fn is_completed(&self) -> bool {
        self.completed_at.is_some()
    }

    /// タスク番号idの記録を返す。まだ扱っていなければ `None` を返す。
    pub fn task_record(&self, id: &str) -> Option<&TaskRecord> {
        self.tasks.iter().find(|record| record.id == id)
    }

    /// リポジトリrepoで、最後にブランチを準備できたタスクのブランチを返す。
    ///
    /// 次のタスクのブランチを作る元の候補にするため、進めているタスクの記録は除く。
    pub fn previous_branch(&self, repo: &Path) -> Option<&str> {
        self.tasks
            .iter()
            .rev()
            .filter(|record| Some(&record.id) != self.task.as_ref())
            .find(|record| record.repo == repo && record.prepared)
            .map(|record| record.branch.as_str())
    }

    /// リポジトリごとの最後のタスクの記録を、PRにするブランチを持つ記録として返す。
    ///
    /// 同じリポジトリのタスクは、前のタスクのブランチを元にして繋がる限り1つの並びにまとめ、並びの最後のタスクを返す。
    /// 前のタスクのブランチが消えていて既定のベースから作り直したときなど、繋がっていない並びは別に返す。
    /// リポジトリは最初にタスクを進めた順、並びはリポジトリの中でタスクを進めた順に並べる。
    pub fn last_branches(&self) -> Vec<&TaskRecord> {
        let prepared: Vec<&TaskRecord> = self.tasks.iter().filter(|r| r.prepared).collect();
        let mut repos: Vec<&Path> = Vec::new();
        for record in &prepared {
            if !repos.contains(&record.repo.as_path()) {
                repos.push(&record.repo);
            }
        }
        let mut heads: Vec<&TaskRecord> = Vec::new();
        for repo in repos {
            let first = heads.len();
            for &record in prepared.iter().filter(|r| r.repo == repo) {
                // 同じリポジトリの並びのうち最後のものに、元のブランチで繋がっていれば並びを延ばす。
                let last = heads.len().checked_sub(1).filter(|&i| i >= first);
                match last {
                    Some(i) if heads[i].branch == record.base => heads[i] = record,
                    _ => heads.push(record),
                }
            }
        }
        heads
    }

    /// 今の人間待ちに積んだ項目の記録を返す。待っていないか、まだ積んでいなければ `None` を返す。
    pub fn waiting_item(&self) -> Option<&WaitItem> {
        let wait = self.wait.as_ref()?;
        self.items.iter().find(|item| &item.wait == wait)
    }

    /// 最新の回の作業報告書のURLを返す。まだ書いていなければ `None` を返す。
    pub fn latest_report(&self) -> Option<&str> {
        self.report_rounds
            .last()
            .map(|round| round.location.as_str())
    }
}

/// 1つの作業計画書の中のステップ。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Step {
    /// タスクの対象を求め、ブランチを作るタスクならブランチを準備する。
    Prepare,
    /// 作業者が実装してコミットする。
    Implement,
    /// 作業者が、ブランチを作らずにタスクを行う。
    Work,
    /// レビュアーがタスクか作業報告書をレビューする。
    Review,
    /// レビューの指摘に回答し、レビュアーが修正する。
    Fix,
    /// 作業者が作業報告書を書く。
    Report,
}

impl Step {
    /// ステップの表示名を返す。
    pub fn label(self) -> &'static str {
        match self {
            Step::Prepare => "タスクの準備",
            Step::Implement => "実装",
            Step::Work => "ブランチを作らない作業",
            Step::Review => "レビュー",
            Step::Fix => "修正",
            Step::Report => "作業報告書の作成",
        }
    }
}

/// 人間を待っている理由。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Wait {
    /// 待っている場面。再開するときに確かめる条件が場面ごとに異なる。
    pub kind: WaitKind,
    /// 人間に伝える理由。
    pub reason: String,
    /// 理由の詳細 (人間にしてほしいこと・未コミットのファイルなど)。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub details: Vec<String>,
}

/// セッションからの質問と、人間の回答。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HumanAnswer {
    /// セッションの質問。
    pub question: String,
    /// 人間の回答。
    pub answer: String,
}

/// 人間を待つ場面。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WaitKind {
    /// 人が行うタスク。
    HumanTask,
    /// タスクの対象を求められないか、ブランチを準備できない。
    Branch,
    /// レビューに要人間判断の指摘がある。
    Review,
    /// レビューが上限の回数に達しても、Request Changesだった。
    ReviewLimit,
    /// 同じステップのやり直しが、上限の回数に達した。
    RetryLimit,
    /// セッションが、人間の判断が要ると返して終わった。
    Question,
    /// 作業報告書を、人間が確認する。
    Report,
}

impl WaitKind {
    /// 人間を待つ場面の表示名を返す。
    pub fn label(self) -> &'static str {
        match self {
            WaitKind::HumanTask => "人が行うタスク",
            WaitKind::Branch => "タスクの準備",
            WaitKind::Review => "レビューの要人間判断の指摘",
            WaitKind::ReviewLimit => "レビューの上限",
            WaitKind::RetryLimit => "やり直しの上限",
            WaitKind::Question => "セッションからの質問",
            WaitKind::Report => "作業報告書の確認",
        }
    }
}

/// プロジェクトの状態の置き場。
#[derive(Debug, Clone)]
pub struct Store {
    root: PathBuf,
}

impl Store {
    /// プロジェクトのルートproject_rootの、状態の置き場を返す。ディレクトリは、ロックを取るときに作る。
    pub fn new(project_root: &Path) -> Self {
        Self {
            root: project_root.join(ROOT),
        }
    }

    /// 計画書名nameの作業計画書の状態を置くディレクトリを返す。
    ///
    /// 計画書名は作業計画書の読み取りで確かめているが、パスの区切りや `..` を含む名前で
    /// 状態の置き場の外に書かないよう、ここでも確かめる。
    pub fn plan(&self, name: &str) -> Result<PlanDir, Error> {
        let valid = !name.is_empty()
            && !name.starts_with('-')
            && name
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-');
        if !valid {
            return Err(Error::InvalidName(name.to_owned()));
        }
        Ok(PlanDir {
            path: self.root.join(name),
        })
    }

    /// 計画書名nameの作業計画書の完了を記録したかを返す。状態ファイルが無ければ、完了していないものとする。
    pub fn is_completed(&self, name: &str) -> Result<bool, Error> {
        Ok(self
            .plan(name)?
            .load()?
            .is_some_and(|state| state.is_completed()))
    }

    /// 進め始めて、まだ完了を記録していない作業計画書の計画書名と状態を、計画書名の順に返す。
    ///
    /// 状態の置き場の中の、計画書名に使えない名前のディレクトリと、状態ファイルの無いディレクトリは数えない。
    pub fn active(&self) -> Result<Vec<(String, PlanState)>, Error> {
        Ok(self
            .plans()?
            .into_iter()
            .filter(|(_, state)| !state.is_completed())
            .collect())
    }

    /// 状態ファイルのある作業計画書の計画書名と状態を、完了を記録したものも含めて、計画書名の順に返す。
    ///
    /// 状態の置き場の中の、計画書名に使えない名前のディレクトリと、状態ファイルの無いディレクトリは数えない。
    pub fn plans(&self) -> Result<Vec<(String, PlanState)>, Error> {
        let list_error = |source| Error::List {
            path: self.root.clone(),
            source,
        };
        let entries = match fs::read_dir(&self.root) {
            Ok(entries) => entries,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(source) => return Err(list_error(source)),
        };
        let mut names = Vec::new();
        for entry in entries {
            let entry = entry.map_err(list_error)?;
            if entry.file_type().map_err(list_error)?.is_dir()
                && let Some(name) = entry.file_name().to_str()
            {
                names.push(name.to_owned());
            }
        }
        names.sort();
        let mut plans = Vec::new();
        for name in names {
            let Ok(dir) = self.plan(&name) else {
                continue;
            };
            if let Some(state) = dir.load()? {
                plans.push((name, state));
            }
        }
        Ok(plans)
    }
}

/// 1つの作業計画書の状態を置くディレクトリ。
#[derive(Debug, Clone)]
pub struct PlanDir {
    path: PathBuf,
}

impl PlanDir {
    /// ディレクトリのパスを返す。
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// 状態ファイルのパスを返す。
    pub fn state_path(&self) -> PathBuf {
        self.path.join(STATE_FILE)
    }

    /// ロックを取らずに状態ファイルを読む。状態ファイルが無ければ `None` を返す。
    pub fn load(&self) -> Result<Option<PlanState>, Error> {
        let path = self.state_path();
        let src = match fs::read(&path) {
            Ok(src) => src,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(source) => return Err(Error::Read { path, source }),
        };
        // 知らない版の形を読み違えないよう、版だけを先に読んで確かめる。
        let header: Header = match serde_json::from_slice(&src) {
            Ok(header) => header,
            Err(source) => return Err(Error::Parse { path, source }),
        };
        if header.version != VERSION {
            return Err(Error::UnsupportedVersion {
                path,
                version: header.version,
            });
        }
        match serde_json::from_slice(&src) {
            Ok(state) => Ok(Some(state)),
            Err(source) => Err(Error::Parse { path, source }),
        }
    }

    /// 状態ファイルの排他ロックを取る。ほかのプロセスがロックを持っていれば、外されるまで待つ。
    ///
    /// 返したロックを捨てると、ロックが外れる。
    pub fn lock(&self) -> Result<Lock, Error> {
        let path = self.path.join(LOCK_FILE);
        let lock_error = |source| Error::Lock {
            path: path.clone(),
            source,
        };
        fs::create_dir_all(&self.path).map_err(lock_error)?;
        let file = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .open(&path)
            .map_err(lock_error)?;
        file.lock().map_err(lock_error)?;
        Ok(Lock {
            dir: self.clone(),
            _file: file,
        })
    }
}

/// 1つの作業計画書の状態ファイルの排他ロック。状態ファイルは、ロックを持っている間だけ書ける。
#[derive(Debug)]
pub struct Lock {
    dir: PlanDir,
    /// ロックを取ったファイル。ファイルを閉じると、ロックが外れる。
    _file: File,
}

impl Lock {
    /// 状態ファイルを読む。状態ファイルが無ければ `None` を返す。
    pub fn load(&self) -> Result<Option<PlanState>, Error> {
        self.dir.load()
    }

    /// 状態stateを、今の形の版を付けて状態ファイルに書く。
    ///
    /// 書きかけのファイルが残らないよう、一時ファイルに書いてから置き換える。
    pub fn save(&self, state: &PlanState) -> Result<(), Error> {
        let path = self.dir.state_path();
        let mut data = match serde_json::to_vec_pretty(&Versioned {
            version: VERSION,
            state,
        }) {
            Ok(data) => data,
            Err(source) => return Err(Error::Encode { path, source }),
        };
        data.push(b'\n');

        let temp = self.dir.path.join(TEMP_FILE);
        if let Err(source) = write_synced(&temp, &data).and_then(|()| fs::rename(&temp, &path)) {
            // 置き換えなかった一時ファイルは次の書き込みで上書きするため、消せなくても続けてよい。
            let _ = fs::remove_file(&temp);
            return Err(Error::Write { path, source });
        }
        Ok(())
    }
}

/// 状態ファイルの、形の版だけを読む形。
#[derive(Deserialize)]
struct Header {
    version: u32,
}

/// 状態ファイルに書く形。形の版を先頭に置き、続けて進行の項目を平らに並べる。
#[derive(Serialize)]
struct Versioned<'a> {
    version: u32,
    #[serde(flatten)]
    state: &'a PlanState,
}

/// pathにdataを書き、置き換えたあとに中身が失われないよう、ディスクに書き出すまで待つ。
pub(crate) fn write_synced(path: &Path, data: &[u8]) -> io::Result<()> {
    let mut file = File::create(path)?;
    file.write_all(data)?;
    file.sync_all()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::TryLockError;

    /// 一時ディレクトリをプロジェクトのルートにした状態の置き場。
    fn store() -> (tempfile::TempDir, Store) {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let store = Store::new(root.path());
        (root, store)
    }

    fn plan_dir(store: &Store, name: &str) -> PlanDir {
        store
            .plan(name)
            .expect("計画書名から状態の置き場を決められなかった")
    }

    /// ロードマップの項目のWikiリンクに書かれた、作業計画書のタイトル。
    const TITLE: &str = "デモ 作業計画書";

    /// すべての項目に値を入れた進行。
    fn sample() -> PlanState {
        PlanState {
            url: "https://wikino.app/s/shimbaco/pages/2930".to_owned(),
            title: TITLE.to_owned(),
            task: Some("1-2".to_owned()),
            step: Some(Step::Review),
            retries: 1,
            retry_reasons: vec!["セッションが終了コード 1 で終わりました".to_owned()],
            session_started: true,
            wait: Some(Wait {
                kind: WaitKind::Review,
                reason: "レビューに要人間判断の指摘があります".to_owned(),
                details: vec!["docs/private/reviews/demo-1-2-001.md".to_owned()],
            }),
            answers: vec![HumanAnswer {
                question: "名前をどちらにしますか".to_owned(),
                answer: "短いほうにしてください".to_owned(),
            }],
            completed_at: Some(
                "2026-11-16T05:15:40.996853Z"
                    .parse()
                    .expect("日時を読み取れなかった"),
            ),
            snapshot: Some(
                "**計画書名**: demo\n\n## タスクリスト\n\n- [x] **1-1**: タスク\n- [ ] **1-2**: タスク\n"
                    .to_owned(),
            ),
            review_before: Some(PathBuf::from("docs/private/reviews/demo-1-2-001.md")),
            tasks: vec![TaskRecord {
                id: "1-2".to_owned(),
                repo: PathBuf::from("/korylus-tools"),
                dir: PathBuf::from("/korylus-tools"),
                branch: "demo-1-2".to_owned(),
                base: "demo-1-1".to_owned(),
                prepared: true,
                review: ReviewTrail {
                    review_prefix: Some("korylus-tools-demo-1-2".to_owned()),
                    review_repo: Some(PathBuf::from("/korylus-tools")),
                    reviews: vec![ReviewRecord {
                        number: 1,
                        role: Reviewer::Codex,
                        doc: PathBuf::from("docs/private/reviews/demo-1-2-001.md"),
                        verdict: Verdict::RequestChanges,
                        at: "2026-11-16T04:00:00Z"
                            .parse()
                            .expect("日時を読み取れなかった"),
                    }],
                    review_limit: Some(4),
                    human_review: None,
                    deferred: vec![DeferredFinding {
                        doc: PathBuf::from("docs/private/reviews/demo-1-2-001.md"),
                        title: "エラーの文面を揃える".to_owned(),
                        choice: "後続タスクに積む".to_owned(),
                    }],
                },
                verification: vec![VerificationItem {
                    kind: VerificationKind::Process,
                    title: "statusで進行を表示できる".to_owned(),
                    target: "korylusfactory status".to_owned(),
                    steps: vec!["korylusfactory status を実行する".to_owned()],
                    expected: "進行中の計画書が表示される".to_owned(),
                }],
            }],
            report_rounds: vec![ReportRound {
                number: 1,
                at: "2026-11-16T05:00:00Z"
                    .parse()
                    .expect("日時を読み取れなかった"),
                location: "https://wikino.app/s/shimbaco/pages/2940".to_owned(),
                walkthrough_skipped: Some("ガイドラインの文面だけを直したため".to_owned()),
                review: ReviewTrail::default(),
            }],
            items: vec![WaitItem {
                wait: Wait {
                    kind: WaitKind::Review,
                    reason: "レビューに要人間判断の指摘があります".to_owned(),
                    details: vec!["docs/private/reviews/demo-1-2-001.md".to_owned()],
                },
                item: QueuedItem {
                    location: None,
                    title: "mewst - demo - レビューの要人間判断の指摘 - 2026-11-16 14時00分00秒"
                        .to_owned(),
                    at: "2026-11-16T14:00:00".parse().expect("日時を読み取れなかった"),
                    page: Some(ItemPage {
                        number: 30,
                        url: "https://wikino.app/s/shimbaco/pages/30/edit".to_owned(),
                    }),
                },
            }],
            notified: Some(Notified {
                wait: Wait {
                    kind: WaitKind::Review,
                    reason: "レビューに要人間判断の指摘があります".to_owned(),
                    details: vec!["docs/private/reviews/demo-1-2-001.md".to_owned()],
                },
                url: Some("https://wikino.app/s/shimbaco/pages/30/edit".to_owned()),
            }),
        }
    }

    #[test]
    fn 今の待ちに積んだ項目だけを今の項目として返す() {
        let mut state = sample();
        assert_eq!(
            state.waiting_item().map(|item| item.item.title.as_str()),
            Some("mewst - demo - レビューの要人間判断の指摘 - 2026-11-16 14時00分00秒")
        );
        state.wait = Some(Wait {
            kind: WaitKind::Question,
            reason: "質問".to_owned(),
            details: Vec::new(),
        });
        assert_eq!(state.waiting_item(), None, "前の待ちの項目を返した");
        state.wait = None;
        assert_eq!(state.waiting_item(), None, "待っていないのに項目を返した");
    }

    /// 計画書名nameの状態ファイルに、内容contentを直接書く。
    fn write_state(store: &Store, name: &str, content: &str) {
        let dir = plan_dir(store, name);
        fs::create_dir_all(dir.path()).expect("ディレクトリを作れなかった");
        fs::write(dir.state_path(), content).expect("状態ファイルを書けなかった");
    }

    #[test]
    fn 保存した状態をそのまま読み直せる() {
        let (_root, store) = store();
        let dir = plan_dir(&store, "demo");
        let lock = dir.lock().expect("ロックを取れなかった");
        lock.save(&sample()).expect("状態を保存できなかった");
        assert_eq!(
            lock.load().expect("ロックの中で状態を読めなかった"),
            Some(sample()),
            "ロックの中で読んだ状態が保存したものと異なる"
        );
        drop(lock);
        assert_eq!(
            dir.load().expect("ロックを取らずに状態を読めなかった"),
            Some(sample()),
            "ロックを外してから読んだ状態が保存したものと異なる"
        );
    }

    #[test]
    fn 状態ファイルが無ければ無いことを返す() {
        let (_root, store) = store();
        let dir = plan_dir(&store, "demo");
        assert_eq!(
            dir.load().expect("状態を読めなかった"),
            None,
            "ロックの外で状態があることにした"
        );
        let lock = dir.lock().expect("ロックを取れなかった");
        assert_eq!(
            lock.load().expect("状態を読めなかった"),
            None,
            "ロックの中で状態があることにした"
        );
    }

    #[test]
    fn 計画書名のディレクトリに形の版を付けて書き_一時ファイルを残さない() {
        let (root, store) = store();
        let lock = plan_dir(&store, "demo")
            .lock()
            .expect("ロックを取れなかった");
        lock.save(&PlanState::new(
            "https://wikino.app/s/shimbaco/pages/2930",
            TITLE,
        ))
        .expect("状態を保存できなかった");

        let dir = root.path().join("docs/private/factory/plans/demo");
        let content =
            fs::read_to_string(dir.join("state.json")).expect("状態ファイルを読めなかった");
        assert_eq!(
            content,
            "{\n  \"version\": 1,\n  \"url\": \"https://wikino.app/s/shimbaco/pages/2930\",\n  \"title\": \"デモ 作業計画書\",\n  \"retries\": 0,\n  \"session_started\": false,\n  \"tasks\": []\n}\n",
            "状態ファイルの内容が期待と異なる"
        );
        let mut names: Vec<_> = fs::read_dir(&dir)
            .expect("ディレクトリを読めなかった")
            .map(|entry| entry.expect("ディレクトリの項目を読めなかった").file_name())
            .collect();
        names.sort();
        assert_eq!(names, ["state.json", "state.lock"], "一時ファイルが残った");
    }

    #[test]
    fn 開始の印が無い既存の状態は未開始として読める() {
        let (_root, store) = store();
        write_state(
            &store,
            "demo",
            r#"{"version":1,"url":"https://wikino.app/s/shimbaco/pages/2930","title":"デモ 作業計画書","tasks":[]}"#,
        );
        let state = plan_dir(&store, "demo")
            .load()
            .expect("既存の状態を読めなかった")
            .expect("状態が無い");
        assert!(!state.session_started, "既存の状態を実行中にした");
    }

    #[test]
    fn 書き込みに失敗したときは前の状態を残す() {
        let (_root, store) = store();
        let dir = plan_dir(&store, "demo");
        let lock = dir.lock().expect("ロックを取れなかった");
        let before = PlanState::new("https://wikino.app/s/shimbaco/pages/1", TITLE);
        lock.save(&before).expect("状態を保存できなかった");

        // 一時ファイルの場所にディレクトリを置き、一時ファイルを作れないようにする。
        fs::create_dir(dir.path().join(TEMP_FILE)).expect("ディレクトリを作れなかった");
        let err = lock.save(&sample()).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Write { .. }), "エラー = {err:?}");
        assert_eq!(
            dir.load().expect("状態を読めなかった"),
            Some(before),
            "書けなかった状態で前の状態を置き換えた"
        );
    }

    #[test]
    fn ロックを持っている間はほかのロックを取れない() {
        let (_root, store) = store();
        let dir = plan_dir(&store, "demo");
        let lock = dir.lock().expect("ロックを取れなかった");

        let other = File::open(dir.path().join(LOCK_FILE)).expect("ロックのファイルを開けなかった");
        assert!(
            matches!(other.try_lock(), Err(TryLockError::WouldBlock)),
            "ロックを持っている間にほかのロックを取れた"
        );
        drop(lock);
        other
            .try_lock()
            .expect("ロックを外したあとにほかのロックを取れなかった");
    }

    #[test]
    fn 知らない形の版の状態ファイルは読まずにエラーにする() {
        let (_root, store) = store();
        // 版1の形では読めない項目を置き、版を確かめる前に読もうとしないことを確かめる。
        write_state(&store, "demo", r#"{"version": 2, "url": 1}"#);
        let err = plan_dir(&store, "demo")
            .load()
            .expect_err("エラーにならなかった");
        assert!(
            matches!(err, Error::UnsupportedVersion { version: 2, .. }),
            "エラー = {err:?}"
        );
        assert!(
            err.to_string()
                .ends_with("の形の版 2 を読めません。このkorylusfactoryが読めるのは版 1 です"),
            "エラー = {err}"
        );
    }

    #[test]
    fn 形の版が無いか項目が誤っていれば書き方の誤りにする() {
        let cases = [
            ("JSONではない", "version = 1"),
            (
                "形の版が無い",
                r#"{"url": "https://wikino.app/s/shimbaco/pages/1", "retries": 0, "tasks": []}"#,
            ),
            ("項目の型が誤っている", r#"{"version": 1, "url": 1}"#),
            (
                "知らないステップ",
                r#"{"version": 1, "url": "https://wikino.app/s/shimbaco/pages/1", "step": "merge"}"#,
            ),
        ];
        for (name, content) in cases {
            let (_root, store) = store();
            write_state(&store, "demo", content);
            let err = plan_dir(&store, "demo")
                .load()
                .expect_err(&format!("{name}: エラーにならなかった"));
            assert!(
                matches!(err, Error::Parse { .. }),
                "{name}: エラー = {err:?}"
            );
        }
    }

    #[test]
    fn 状態の置き場に使えない計画書名はエラー() {
        let (_root, store) = store();
        for name in ["", "../demo", "demo/1-1", ".", "Demo", "-demo", "デモ"] {
            let err = store
                .plan(name)
                .expect_err(&format!("{name:?}: エラーにならなかった"));
            assert!(
                matches!(&err, Error::InvalidName(got) if got == name),
                "{name:?}: エラー = {err:?}"
            );
        }
    }

    #[test]
    fn 完了を記録した計画書だけを完了とみなす() {
        let (_root, store) = store();
        assert!(
            !store
                .is_completed("demo")
                .expect("完了を確かめられなかった"),
            "状態ファイルの無い計画書を完了とみなした"
        );

        let lock = plan_dir(&store, "demo")
            .lock()
            .expect("ロックを取れなかった");
        let mut state = PlanState::new("https://wikino.app/s/shimbaco/pages/1", TITLE);
        lock.save(&state).expect("状態を保存できなかった");
        assert!(
            !store
                .is_completed("demo")
                .expect("完了を確かめられなかった"),
            "完了を記録していない計画書を完了とみなした"
        );

        state.completed_at = Some(
            "2026-11-16T05:15:40Z"
                .parse()
                .expect("日時を読み取れなかった"),
        );
        lock.save(&state).expect("状態を保存できなかった");
        assert!(
            store
                .is_completed("demo")
                .expect("完了を確かめられなかった"),
            "完了を記録した計画書を完了とみなさなかった"
        );
    }

    #[test]
    fn 同じリポジトリで最後に準備できたほかのタスクのブランチを返す() {
        let record = |id: &str, repo: &str, prepared: bool| TaskRecord {
            id: id.to_owned(),
            repo: PathBuf::from(repo),
            dir: PathBuf::from(repo),
            branch: format!("demo-{id}"),
            base: "main".to_owned(),
            prepared,
            review: ReviewTrail::default(),
            verification: Vec::new(),
        };
        let mut state = PlanState::new("https://wikino.app/s/shimbaco/pages/1", TITLE);
        state.tasks = vec![
            record("1-1", "/workspace", true),
            record("1-2", "/korylus-tools", true),
            record("1-3", "/workspace", false),
            record("1-4", "/workspace", true),
        ];
        state.task = Some("1-4".to_owned());

        assert_eq!(
            state.previous_branch(Path::new("/workspace")),
            Some("demo-1-1"),
            "進めているタスクか、準備できなかったタスクのブランチを返した"
        );
        assert_eq!(
            state.previous_branch(Path::new("/korylus-tools")),
            Some("demo-1-2")
        );
        assert_eq!(state.previous_branch(Path::new("/nonoto")), None);
    }

    #[test]
    fn リポジトリごとに元のブランチで繋がった並びの最後のタスクを返す() {
        let record = |id: &str, repo: &str, base: &str, prepared: bool| TaskRecord {
            id: id.to_owned(),
            repo: PathBuf::from(repo),
            dir: PathBuf::from(repo),
            branch: format!("demo-{id}"),
            base: base.to_owned(),
            prepared,
            review: ReviewTrail::default(),
            verification: Vec::new(),
        };
        let mut state = PlanState::new("https://wikino.app/s/shimbaco/pages/1", TITLE);
        state.tasks = vec![
            record("1-1", "/workspace", "develop", true),
            record("1-2", "/korylus-tools", "main", true),
            record("1-3", "/workspace", "demo-1-1", true),
            // 前のタスクのブランチが消えていて、既定のベースから作り直した並び。
            record("1-4", "/workspace", "develop", true),
            record("1-5", "/workspace", "demo-1-4", false),
        ];
        let ids: Vec<&str> = state
            .last_branches()
            .into_iter()
            .map(|record| record.id.as_str())
            .collect();
        assert_eq!(ids, ["1-3", "1-4", "1-2"]);
    }

    #[test]
    fn 進め始めて完了していない作業計画書を計画書名の順に返す() {
        let (_root, store) = store();
        for (name, completed) in [("b-plan", false), ("a-plan", false), ("done-plan", true)] {
            let mut state =
                PlanState::new(format!("https://wikino.app/s/shimbaco/pages/{name}"), TITLE);
            if completed {
                state.completed_at = Some(
                    "2026-11-16T05:15:40Z"
                        .parse()
                        .expect("日時を読み取れなかった"),
                );
            }
            plan_dir(&store, name)
                .lock()
                .expect("ロックを取れなかった")
                .save(&state)
                .expect("状態を保存できなかった");
        }
        // 計画書名に使えない名前のディレクトリと、状態ファイルの無いディレクトリは数えない。
        fs::create_dir_all(store.root.join("Bad")).expect("ディレクトリを作れなかった");
        fs::create_dir_all(store.root.join("empty")).expect("ディレクトリを作れなかった");

        let names: Vec<String> = store
            .active()
            .expect("進行中の作業計画書を読めなかった")
            .into_iter()
            .map(|(name, _)| name)
            .collect();
        assert_eq!(names, ["a-plan", "b-plan"]);
    }

    #[test]
    fn 状態の置き場が無ければ進行中の作業計画書は無い() {
        let (_root, store) = store();
        assert_eq!(
            store.active().expect("進行中の作業計画書を読めなかった"),
            [],
            "状態の置き場が無いのに進行中の作業計画書を返した"
        );
    }

    #[test]
    fn 状態ファイルを読めなければ完了を確かめられない() {
        let (_root, store) = store();
        write_state(&store, "demo", "{");
        let err = store
            .is_completed("demo")
            .expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Parse { .. }), "エラー = {err:?}");
    }
}
