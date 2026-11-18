//! 人間キューの置き場 (Wikinoのトピック) の読み書き。
//!
//! 項目は、置き場のトピックの1ページである。ディレクターが新しいページの下書きとして作り、人間が回答を書いて公開する。
//! 下書きがあれば人間待ち (公開済みのページの下書きなら差し戻し)、公開済みで下書きが無ければ回答済みと読む。
//! 呼び出し側は [`Store`] を通して読み書きし、テストでは差し替える。
//! 本物の実装 [`WikinoStore`] は、wikinoコマンドを子プロセスで呼ぶ。
//! `korylusfactory queue` の処理は [`command`] に置く。

pub mod command;

use std::fmt;
use std::io::{self, Write as _};

use korylusfactory_core::state::ItemLocation;
use serde::Deserialize;
use serde::de::DeserializeOwned;
use tempfile::NamedTempFile;

use crate::process::{Runner, System};
use crate::source::{self, Outcome, ReadFailure, WIKINO_HOST, run_wikino, stderr_line};

/// wikinoの終了コードのうち、通信の失敗かサーバーのエラー。作成では、作成されている場合がある。
const EXIT_UNKNOWN: i32 = 1;

/// wikinoの終了コードのうち、下書きが既にある (作成) か、読んだあとに下書きが更新された (削除)。
const EXIT_CONFLICT: i32 = 6;

/// 下書きの一覧から読む項目。
const LIST_FIELDS: &str = "page_number,title,topic_number";

/// 人間キューの置き場を読み書きできないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// wikinoコマンドを実行できないか、出力がUTF-8ではない。
    #[error(transparent)]
    Wikino(#[from] source::Error),

    /// wikinoコマンドが失敗した。
    #[error(
        "人間キューの{operation}: {reason}。wikino auth status --host={host} --space={space} で状態を確かめてください{}",
        stderr_line(.stderr),
        host = WIKINO_HOST
    )]
    Failed {
        /// 失敗した操作。
        operation: Operation,
        /// 置き場のスペース。
        space: String,
        /// 失敗した理由。
        reason: ReadFailure,
        /// wikinoコマンドの標準エラー。
        stderr: String,
    },

    /// 作成の結果が分からず、下書きや公開済みページの一覧でも作られたかを確かめられない。
    #[error(
        "人間キューの項目「{title}」の下書きを作れたかを確かめられません。送り直す前に、下書きや公開済みページの一覧で作られていないかを確かめてください"
    )]
    Unconfirmed {
        /// 作ろうとした項目のタイトル。
        title: String,
        /// 一覧で確かめられなかった理由。
        #[source]
        source: Box<Error>,
    },

    /// 置き場のトピックが無いか、見られない。
    #[error(
        "人間キューの置き場のトピック {topic} (スペース {space}) に下書きを作れません。設定ファイルの「queue」と、トークンのtopic:readのスコープを確かめてください"
    )]
    NoTopic {
        /// 置き場のスペース。
        space: String,
        /// 置き場のトピックの番号。
        topic: u64,
    },

    /// 下書きを作り直す項目のページが無い。
    #[error("人間キューの項目 {0} のページが見つかりません")]
    NoPage(u64),

    /// 下書きを作り直す項目に、下書きが既にある。
    #[error("人間キューの項目 {0} には下書きが既にあります")]
    DraftExists(u64),

    /// 書き換える項目に、下書きが無い。
    #[error("人間キューの項目 {0} には下書きがありません。公開されたか、片付けられています")]
    NoDraft(u64),

    /// 削除する下書きが、読んだあとに更新された。
    #[error("人間キューの項目 {0} の下書きが、読んだあとに更新されています")]
    Changed(u64),

    /// 読んだページが、置き場のトピックのページではない。
    #[error(
        "ページ {number} は、人間キューの置き場のトピック {expected} ではなく、トピック {actual} のページです"
    )]
    OtherTopic {
        /// 読んだページの番号。
        number: u64,
        /// 置き場のトピックの番号。
        expected: u64,
        /// ページが属するトピックの番号。
        actual: u64,
    },

    /// wikinoコマンドの出力を読み取れない。
    #[error("wikino {command} の出力を読み取れません")]
    Output {
        /// 出力を読み取れなかったサブコマンド。
        command: &'static str,
        /// 読み取れなかった理由。
        #[source]
        source: serde_json::Error,
    },

    /// 本文を渡す一時ファイルを書けない。
    #[error("人間キューの項目の本文を渡す一時ファイルを書けません")]
    BodyFile(#[source] io::Error),
}

/// 失敗した置き場の操作。エラーのメッセージで、何に失敗したかを示す。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Operation {
    /// 項目の下書きの作成。
    Create,
    /// 下書きの一覧の読み取り。
    List,
    /// 公開済みの項目の検索。
    Find,
    /// 項目の読み取り。
    Read(u64),
    /// 項目の下書きの削除。
    Delete(u64),
    /// 公開済みの項目への下書きの作り直し。
    Reopen(u64),
    /// 項目の下書きの書き換え。
    Update(u64),
}

impl fmt::Display for Operation {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Create => f.write_str("項目の下書きを作れません"),
            Self::List => f.write_str("下書きの一覧を読めません"),
            Self::Find => f.write_str("公開済みの項目を探せません"),
            Self::Read(number) => write!(f, "項目 {number} を読めません"),
            Self::Delete(number) => write!(f, "項目 {number} の下書きを削除できません"),
            Self::Reopen(number) => write!(f, "項目 {number} に下書きを作り直せません"),
            Self::Update(number) => write!(f, "項目 {number} の下書きに書けません"),
        }
    }
}

/// 人間キューの置き場。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Location {
    /// スペースの識別子。
    pub space: String,
    /// トピックの番号。
    pub topic: u64,
}

impl Location {
    /// ページ番号numberの項目を、Web画面の編集画面で開くURLを返す。
    ///
    /// 人間待ちの項目は下書きのため、下書きを開ける編集画面のURLにする。
    pub fn edit_url(&self, number: u64) -> String {
        format!("{WIKINO_HOST}/s/{}/pages/{number}/edit", self.space)
    }
}

impl From<&Location> for ItemLocation {
    fn from(location: &Location) -> Self {
        Self {
            space: location.space.clone(),
            topic: location.topic,
        }
    }
}

impl From<&ItemLocation> for Location {
    fn from(location: &ItemLocation) -> Self {
        Self {
            space: location.space.clone(),
            topic: location.topic,
        }
    }
}

/// 下書きの一覧で見つけた項目。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Listed {
    /// ページ番号。
    pub number: u64,
    /// タイトル。
    pub title: String,
}

/// 項目の、Wikinoでの今の状態。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Status {
    /// 下書きがある。人間待ちで、公開済みのページの下書きなら差し戻し中。
    Drafted {
        /// 下書きのタイトル。タイトルの無い下書きでは `None`。
        title: Option<String>,
        /// 下書きの本文。
        body: String,
        /// 下書きの版。下書きを削除するときに渡す。
        etag: String,
    },
    /// 公開済みで、下書きが無い。回答済み。
    Published {
        /// ページのタイトル。
        title: Option<String>,
        /// ページの本文。
        body: String,
    },
    /// 下書きが無く、ページが未公開か、ページが無い (人間が下書きを削除したときなど)。
    Missing,
}

/// 人間キューの置き場の読み書き。
pub trait Store {
    /// タイトルtitle・本文bodyの項目を、新しいページの下書きとして作り、ページ番号を返す。
    ///
    /// 作成は冪等でないため、結果が分からないときは送り直さずに、下書きと公開済みページの一覧で作られたかを確かめる。
    /// 一覧でも確かめられないときは [`Error::Unconfirmed`] を返す。
    /// そのあとに作り直す側は、先に [`Store::find`] で作られていないかを確かめる。
    fn create(&self, title: &str, body: &str) -> Result<u64, Error>;

    /// 置き場のトピックにある、タイトルのある下書きの一覧を返す。
    fn list(&self) -> Result<Vec<Listed>, Error>;

    /// ページ番号numberの項目の、今の状態を読む。
    fn read(&self, number: u64) -> Result<Status, Error>;

    /// ページ番号numberの項目の、版etagの下書きを削除する。下書きが既に無ければ、何もしない。
    ///
    /// 書き込む前に置き場のトピックを確かめ、別のトピックのページは操作しない。
    /// 読んだあとに下書きが更新されていれば (人間が回答を書いているときなど)、削除せずに [`Error::Changed`] を返す。
    fn delete(&self, number: u64, etag: &str) -> Result<(), Error>;

    /// 公開済みの項目numberに、本文bodyの下書きを作り直す (差し戻し)。タイトルはページのまま変えない。
    /// 書き込む前に置き場のトピックを確かめ、別のトピックのページは操作しない。
    fn reopen(&self, number: u64, body: &str) -> Result<(), Error>;

    /// 項目numberの版etagの下書きを、本文bodyに書き換える。
    ///
    /// 書き込む前に置き場のトピックを確かめ、別のトピックのページは操作しない。
    /// 下書きが無ければ [`Error::NoDraft`] を、読んだあとに下書きが更新されていれば [`Error::Changed`] を返す。
    fn update(&self, number: u64, body: &str, etag: &str) -> Result<(), Error>;

    /// 置き場の下書きと公開済みのページから、タイトルtitleの項目を探し、ページ番号を返す。
    /// 結果不明の間に人間が公開した項目も、作り直さずに回収する。
    fn find(&self, title: &str) -> Result<Option<u64>, Error>;
}

/// 置き場を読み書きする [`Store`] を開く。テストでは、wikinoを呼ばない偽物の [`Store`] を開くものに差し替える。
pub trait Open {
    /// 置き場locationを読み書きする [`Store`] を返す。
    fn open<'a>(&'a self, location: &'a Location) -> Box<dyn Store + 'a>;
}

impl Open for System {
    fn open<'a>(&'a self, location: &'a Location) -> Box<dyn Store + 'a> {
        Box::new(WikinoStore::new(self, location))
    }
}

/// wikinoコマンドで読み書きする、本物の [`Store`]。
#[derive(Clone, Copy)]
pub struct WikinoStore<'a> {
    runner: &'a dyn Runner,
    location: &'a Location,
}

impl fmt::Debug for WikinoStore<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("WikinoStore")
            .field("location", self.location)
            .finish_non_exhaustive()
    }
}

/// `draft create` の出力から読む項目。
#[derive(Deserialize)]
struct Created {
    page_number: u64,
}

/// `draft list` の出力の1行から読む項目。
#[derive(Deserialize)]
struct ListLine {
    #[serde(alias = "number")]
    page_number: u64,
    title: Option<String>,
    topic_number: u64,
}

/// `draft get` の出力から読む項目。
#[derive(Deserialize)]
struct DraftJson {
    title: Option<String>,
    topic_number: u64,
    body: String,
    etag: String,
}

/// `page get` の出力から読む項目。
#[derive(Deserialize)]
struct PageJson {
    title: Option<String>,
    topic_number: u64,
    body: String,
    status: PageStatus,
}

/// ページの公開状態。
#[derive(Deserialize)]
#[serde(rename_all = "lowercase")]
enum PageStatus {
    Published,
    Unpublished,
}

impl<'a> WikinoStore<'a> {
    /// runnerでwikinoコマンドを実行し、置き場locationを読み書きする。
    pub fn new(runner: &'a dyn Runner, location: &'a Location) -> Self {
        Self { runner, location }
    }

    /// `wikino <command>` を、フラグflagsに置き場のスペースとJSONの出力を足して実行する。
    ///
    /// 値はフラグとして読まれないよう、`--名前=値` の1つの引数で渡す。
    fn run(&self, command: &str, flags: Vec<String>) -> Result<Outcome, Error> {
        let mut args: Vec<String> = command.split(' ').map(str::to_owned).collect();
        args.extend(flags);
        args.extend([
            format!("--host={WIKINO_HOST}"),
            format!("--space={}", self.location.space),
            "--json".to_owned(),
        ]);
        Ok(run_wikino(self.runner, &args)?)
    }

    fn failed(&self, operation: Operation, reason: ReadFailure, stderr: String) -> Error {
        Error::Failed {
            operation,
            space: self.location.space.clone(),
            reason,
            stderr,
        }
    }

    /// ページnumberのトピックtopicが、置き場のトピックかを確かめる。
    ///
    /// 置き場の外のページを、項目として読んだり書き換えたりしないためである。
    fn check_topic(&self, number: u64, topic: u64) -> Result<(), Error> {
        if topic == self.location.topic {
            return Ok(());
        }
        Err(Error::OtherTopic {
            number,
            expected: self.location.topic,
            actual: topic,
        })
    }
}

impl Store for WikinoStore<'_> {
    fn find(&self, title: &str) -> Result<Option<u64>, Error> {
        if let Some(item) = self.list()?.into_iter().find(|item| item.title == title) {
            return Ok(Some(item.number));
        }
        let stdout = match self.run(
            "page list",
            vec![
                "--all".to_owned(),
                format!("--topic={}", self.location.topic),
                format!("--title={title}"),
                "--status=published".to_owned(),
                "--fields=number,title,topic_number".to_owned(),
            ],
        )? {
            Outcome::Found(stdout) => stdout,
            Outcome::NotFound => {
                return Err(self.failed(
                    Operation::Find,
                    ReadFailure::Exit {
                        command: "page list".to_owned(),
                        code: source::EXIT_NOT_FOUND,
                    },
                    String::new(),
                ));
            }
            Outcome::Failed { reason, stderr } => {
                return Err(self.failed(Operation::Find, reason, stderr));
            }
        };
        for line in stdout.lines().filter(|line| !line.trim().is_empty()) {
            let item: ListLine = parse("page list", line)?;
            if item.topic_number == self.location.topic && item.title.as_deref() == Some(title) {
                return Ok(Some(item.page_number));
            }
        }
        Ok(None)
    }

    fn create(&self, title: &str, body: &str) -> Result<u64, Error> {
        let (_file, body_flag) = body_file(body)?;
        let flags = vec![
            format!("--topic={}", self.location.topic),
            format!("--title={title}"),
            body_flag,
        ];
        let failed = match self.run("draft create", flags) {
            Ok(Outcome::Found(stdout)) => match parse::<Created>("draft create", &stdout) {
                Ok(created) => return Ok(created.page_number),
                Err(err) => err,
            },
            Ok(Outcome::NotFound) => {
                return Err(Error::NoTopic {
                    space: self.location.space.clone(),
                    topic: self.location.topic,
                });
            }
            Ok(Outcome::Failed { reason, stderr }) => {
                let unknown =
                    matches!(reason, ReadFailure::Exit { code, .. } if code == EXIT_UNKNOWN);
                let failed = self.failed(Operation::Create, reason, stderr);
                if !unknown {
                    return Err(failed);
                }
                failed
            }
            // プロセスが起動したあとの終了や、成功した出力の読み取り失敗では、サーバーで作られている場合がある。
            Err(
                err @ Error::Wikino(
                    source::Error::OutputEncoding(_)
                    | source::Error::Run(
                        crate::process::Error::Signaled(_) | crate::process::Error::Wait { .. },
                    ),
                ),
            ) => err,
            Err(err) => return Err(err),
        };
        // 作成されている場合があるため、送り直す前に一覧で確かめる。
        // 積んだ日時を秒まで含むタイトルは、置き場の中で重ならない。
        match self.find(title) {
            Ok(Some(number)) => Ok(number),
            Ok(None) => Err(failed),
            Err(err) => Err(Error::Unconfirmed {
                title: title.to_owned(),
                source: Box::new(err),
            }),
        }
    }

    fn list(&self) -> Result<Vec<Listed>, Error> {
        let flags = vec!["--all".to_owned(), format!("--fields={LIST_FIELDS}")];
        let stdout = match self.run("draft list", flags)? {
            Outcome::Found(stdout) => stdout,
            // 一覧は該当が無くても成功するため、見つからない終了コードもほかの失敗と同じく扱う。
            Outcome::NotFound => {
                return Err(self.failed(
                    Operation::List,
                    ReadFailure::Exit {
                        command: "draft list".to_owned(),
                        code: source::EXIT_NOT_FOUND,
                    },
                    String::new(),
                ));
            }
            Outcome::Failed { reason, stderr } => {
                return Err(self.failed(Operation::List, reason, stderr));
            }
        };
        // `--all` を付けると、1行に1件のJSON Linesで出る。
        let mut items = Vec::new();
        for line in stdout.lines().filter(|line| !line.trim().is_empty()) {
            let line: ListLine = parse("draft list", line)?;
            if line.topic_number != self.location.topic {
                continue;
            }
            // タイトルの無い下書きは、ディレクターが積んだ項目ではない。
            if let Some(title) = line.title {
                items.push(Listed {
                    number: line.page_number,
                    title,
                });
            }
        }
        Ok(items)
    }

    fn read(&self, number: u64) -> Result<Status, Error> {
        match self.run("draft get", vec![number.to_string()])? {
            Outcome::Found(stdout) => {
                let draft: DraftJson = parse("draft get", &stdout)?;
                self.check_topic(number, draft.topic_number)?;
                return Ok(Status::Drafted {
                    title: draft.title,
                    body: draft.body,
                    etag: draft.etag,
                });
            }
            Outcome::NotFound => {}
            Outcome::Failed { reason, stderr } => {
                return Err(self.failed(Operation::Read(number), reason, stderr));
            }
        }
        let stdout = match self.run("page get", vec![number.to_string()])? {
            Outcome::Found(stdout) => stdout,
            Outcome::NotFound => return Ok(Status::Missing),
            Outcome::Failed { reason, stderr } => {
                return Err(self.failed(Operation::Read(number), reason, stderr));
            }
        };
        let page: PageJson = parse("page get", &stdout)?;
        self.check_topic(number, page.topic_number)?;
        Ok(match page.status {
            PageStatus::Published => Status::Published {
                title: page.title,
                body: page.body,
            },
            // 公開されなかった新しいページの下書きを削除すると、タイトルの無い未公開のページが残る。
            PageStatus::Unpublished => Status::Missing,
        })
    }

    fn delete(&self, number: u64, etag: &str) -> Result<(), Error> {
        match self.read(number)? {
            Status::Drafted { .. } => {}
            Status::Published { .. } | Status::Missing => return Ok(()),
        }
        let flags = vec![
            number.to_string(),
            // 確認のために読み直した版へ置き換えず、呼び出し側が読んだ版を使う。
            format!("--if-match={etag}"),
            // 端末でないときは、`--yes` が無いと削除の要求を送らずに確認を求めて終わる。
            "--yes".to_owned(),
        ];
        match self.run("draft delete", flags)? {
            // 下書きが既に無いとき (削除し直したとき・人間が公開か削除したとき) も、下書きの無い状態になっている。
            Outcome::Found(_) | Outcome::NotFound => Ok(()),
            Outcome::Failed {
                reason:
                    ReadFailure::Exit {
                        code: EXIT_CONFLICT,
                        ..
                    },
                ..
            } => Err(Error::Changed(number)),
            Outcome::Failed { reason, stderr } => {
                Err(self.failed(Operation::Delete(number), reason, stderr))
            }
        }
    }

    fn update(&self, number: u64, body: &str, etag: &str) -> Result<(), Error> {
        match self.read(number)? {
            Status::Drafted { .. } => {}
            Status::Published { .. } | Status::Missing => return Err(Error::NoDraft(number)),
        }
        let (_file, body_flag) = body_file(body)?;
        let flags = vec![number.to_string(), body_flag, format!("--if-match={etag}")];
        match self.run("draft update", flags)? {
            Outcome::Found(_) => Ok(()),
            Outcome::NotFound => Err(Error::NoDraft(number)),
            Outcome::Failed {
                reason:
                    ReadFailure::Exit {
                        code: EXIT_CONFLICT,
                        ..
                    },
                ..
            } => Err(Error::Changed(number)),
            Outcome::Failed { reason, stderr } => {
                Err(self.failed(Operation::Update(number), reason, stderr))
            }
        }
    }

    fn reopen(&self, number: u64, body: &str) -> Result<(), Error> {
        match self.read(number)? {
            Status::Published { .. } => {}
            Status::Drafted { .. } => return Err(Error::DraftExists(number)),
            Status::Missing => return Err(Error::NoPage(number)),
        }
        let (_file, body_flag) = body_file(body)?;
        // 作成の結果が分からなかったときは、次に読んだときに下書きがあれば差し戻し中と読めるため、確かめずに返す。
        match self.run("draft create", vec![number.to_string(), body_flag])? {
            Outcome::Found(_) => Ok(()),
            Outcome::NotFound => Err(Error::NoPage(number)),
            Outcome::Failed {
                reason:
                    ReadFailure::Exit {
                        code: EXIT_CONFLICT,
                        ..
                    },
                ..
            } => Err(Error::DraftExists(number)),
            Outcome::Failed { reason, stderr } => {
                Err(self.failed(Operation::Reopen(number), reason, stderr))
            }
        }
    }
}

/// 本文bodyを一時ファイルに書き、ファイルと、wikinoコマンドに渡すフラグを返す。
///
/// 本文を引数で渡すと、長い本文が1つの引数の長さの上限を超えうるため、ファイルで渡す。
/// ファイルは、返したファイルを捨てたときに消える。
fn body_file(body: &str) -> Result<(NamedTempFile, String), Error> {
    let mut file = NamedTempFile::new().map_err(Error::BodyFile)?;
    file.write_all(body.as_bytes()).map_err(Error::BodyFile)?;
    let path = file.path().to_str().ok_or_else(|| {
        Error::BodyFile(io::Error::other("一時ファイルのパスがUTF-8ではありません"))
    })?;
    let flag = format!("--body-file={path}");
    Ok((file, flag))
}

/// wikino commandの出力jsonを読み取る。
fn parse<T: DeserializeOwned>(command: &'static str, json: &str) -> Result<T, Error> {
    serde_json::from_str(json).map_err(|source| Error::Output { command, source })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::{self, Output};
    use std::cell::RefCell;
    use std::ffi::OsString;
    use std::fs;

    /// wikinoの引数から出力を決める関数。
    type Respond = Box<dyn Fn(&[String]) -> Result<Output, process::Error>>;

    /// wikinoの引数に応じて決めた出力を返し、呼ばれた引数と、渡された本文のファイルの中身を記録するRunner。
    struct FakeWikino {
        respond: Respond,
        calls: RefCell<Vec<Vec<String>>>,
        bodies: RefCell<Vec<String>>,
    }

    impl FakeWikino {
        fn new(respond: impl Fn(&[String]) -> Output + 'static) -> Self {
            Self::fallible(move |args| Ok(respond(args)))
        }

        fn fallible(
            respond: impl Fn(&[String]) -> Result<Output, process::Error> + 'static,
        ) -> Self {
            Self {
                respond: Box::new(respond),
                calls: RefCell::new(Vec::new()),
                bodies: RefCell::new(Vec::new()),
            }
        }

        /// 呼ばれたサブコマンド (`draft create` など) を、呼ばれた順に返す。
        fn commands(&self) -> Vec<String> {
            self.calls
                .borrow()
                .iter()
                .map(|args| args[..2].join(" "))
                .collect()
        }
    }

    impl Runner for FakeWikino {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            assert_eq!(program, "wikino", "wikino以外のコマンドを実行した");
            let args: Vec<String> = args
                .iter()
                .map(|a| a.to_string_lossy().into_owned())
                .collect();
            if let Some(path) = args.iter().find_map(|a| a.strip_prefix("--body-file=")) {
                let body = fs::read_to_string(path).expect("本文のファイルを読めなかった");
                self.bodies.borrow_mut().push(body);
            }
            let output = (self.respond)(&args);
            self.calls.borrow_mut().push(args);
            output
        }
    }

    fn ok(stdout: &str) -> Output {
        Output {
            stdout: stdout.as_bytes().to_vec(),
            ..Output::default()
        }
    }

    fn exit(code: i32, stderr: &str) -> Output {
        Output {
            stderr: stderr.as_bytes().to_vec(),
            code,
            ..Output::default()
        }
    }

    /// 引数argsの最初の2つ (サブコマンド) を返す。
    fn command(args: &[String]) -> String {
        args[..2].join(" ")
    }

    fn location() -> Location {
        Location {
            space: "shimbaco".to_owned(),
            topic: 12,
        }
    }

    const TITLE: &str = "mewst - demo - セッションからの質問 - 2026-11-17 21時04分05秒";
    const BODY: &str = "## 状況\n\n- プロジェクト: mewst\n\n## 回答\n\n### 質問への回答\n";

    /// 置き場のトピック12にTITLEの下書き (ページ30) があり、ほかのトピックにも下書きがある一覧。
    const LISTED: &str = "{\"page_number\":31,\"title\":\"計画書\",\"topic_number\":4}\n\
         {\"page_number\":29,\"title\":null,\"topic_number\":12}\n\
         {\"page_number\":30,\"title\":\"mewst - demo - セッションからの質問 - 2026-11-17 21時04分05秒\",\"topic_number\":12}\n";

    #[test]
    fn 項目を新しいページの下書きとして作る() {
        let wikino = FakeWikino::new(|_| ok(r#"{"page_number":30,"etag":"W/\"1\""}"#));
        let location = location();
        let number = WikinoStore::new(&wikino, &location)
            .create(TITLE, BODY)
            .expect("作れなかった");

        assert_eq!(number, 30);
        let calls = wikino.calls.into_inner();
        let body_file = calls[0]
            .iter()
            .find(|a| a.starts_with("--body-file="))
            .expect("本文をファイルで渡していない");
        assert_eq!(
            calls,
            [vec![
                "draft".to_owned(),
                "create".to_owned(),
                "--topic=12".to_owned(),
                format!("--title={TITLE}"),
                body_file.clone(),
                "--host=https://wikino.app".to_owned(),
                "--space=shimbaco".to_owned(),
                "--json".to_owned(),
            ]]
        );
        assert_eq!(wikino.bodies.into_inner(), [BODY]);
    }

    #[test]
    fn 作成の結果が分からなければ送り直さずに一覧で確かめる() {
        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft create" => exit(1, "通信に失敗しました"),
            _ => ok(LISTED),
        });
        let location = location();
        let number = WikinoStore::new(&wikino, &location)
            .create(TITLE, BODY)
            .expect("一覧で見つけた項目を返さなかった");

        assert_eq!(number, 30);
        assert_eq!(wikino.commands(), ["draft create", "draft list"]);
        assert!(
            wikino.calls.into_inner()[1]
                .contains(&"--fields=page_number,title,topic_number".to_owned()),
            "一覧で読む項目を絞っていない"
        );
    }

    #[test]
    fn 作成の結果が分からず一覧に無ければ作成の失敗を返す() {
        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft create" => exit(1, "通信に失敗しました"),
            _ => ok("{\"page_number\":31,\"title\":\"計画書\",\"topic_number\":4}\n"),
        });
        let location = location();
        let err = WikinoStore::new(&wikino, &location)
            .create(TITLE, BODY)
            .expect_err("作れていないのにエラーにならなかった");

        assert!(
            matches!(
                err,
                Error::Failed {
                    operation: Operation::Create,
                    ..
                }
            ),
            "エラー = {err:?}"
        );
        assert_eq!(
            err.to_string(),
            "人間キューの項目の下書きを作れません: wikino draft create が終了コード 1 で失敗しました。wikino auth status --host=https://wikino.app --space=shimbaco で状態を確かめてください\n通信に失敗しました"
        );
        assert_eq!(
            wikino.commands(),
            ["draft create", "draft list", "page list"]
        );
    }

    #[test]
    fn 作成の結果が分からず一覧も読めなければ確かめられないエラー() {
        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft create" => exit(1, ""),
            _ => exit(8, ""),
        });
        let location = location();
        let err = WikinoStore::new(&wikino, &location)
            .create(TITLE, BODY)
            .expect_err("確かめられないのにエラーにならなかった");

        assert!(
            matches!(&err, Error::Unconfirmed { title, .. } if title == TITLE),
            "エラー = {err:?}"
        );
        assert!(
            crate::describe(&err).ends_with(
                "下書きや公開済みページの一覧で作られていないかを確かめてください: 人間キューの下書きの一覧を読めません: 公開APIの利用量の上限に当たりました。wikino auth status --host=https://wikino.app --space=shimbaco で状態を確かめてください"
            ),
            "エラー = {}",
            crate::describe(&err)
        );
    }

    #[test]
    fn 作成のそのほかの失敗では一覧を読まない() {
        let cases = [
            (exit(7, "タイトルが長すぎます"), "終了コード 7"),
            (exit(3, ""), "ログインしていないか"),
            (
                exit(5, ""),
                "トピック 12 (スペース shimbaco) に下書きを作れません",
            ),
        ];
        for (output, want) in cases {
            let wikino = FakeWikino::new(move |_| output.clone());
            let location = location();
            let err = WikinoStore::new(&wikino, &location)
                .create(TITLE, BODY)
                .expect_err("エラーにならなかった");
            assert!(
                err.to_string().contains(want),
                "エラー = {err}、期待値 = {want:?} を含むエラー"
            );
            assert_eq!(wikino.commands(), ["draft create"], "{want}: 一覧を読んだ");
        }
    }

    #[test]
    fn 作成後のプロセス終了や読めない出力でも一覧で作成結果を確かめる() {
        for failure in 0..4 {
            for verification in 0..3 {
                let wikino = FakeWikino::fallible(move |args| match command(args).as_str() {
                    "draft create" => match failure {
                        0 => Err(process::Error::Signaled("wikino".to_owned())),
                        1 => Ok(ok("{broken")),
                        2 => Ok(Output {
                            stdout: vec![255],
                            ..Output::default()
                        }),
                        _ => Err(process::Error::Wait {
                            program: "wikino".to_owned(),
                            source: io::Error::other("終了を確認できません"),
                        }),
                    },
                    "draft list" => Ok(match verification {
                        0 => ok(LISTED),
                        1 => exit(8, ""),
                        _ => ok(""),
                    }),
                    "page list" if verification == 2 => Ok(ok("")),
                    other => panic!("予想外のコマンド: {other}"),
                });
                let location = location();
                let result = WikinoStore::new(&wikino, &location).create(TITLE, BODY);

                match verification {
                    0 => assert_eq!(result.expect("作成されたページを回収できなかった"), 30),
                    1 => assert!(
                        matches!(result, Err(Error::Unconfirmed { .. })),
                        "一覧も読めないのに結果不明にならない: {result:?}"
                    ),
                    _ => assert!(
                        matches!(result, Err(Error::Wikino(_) | Error::Output { .. })),
                        "一覧に無いときに元の失敗を返さない: {result:?}"
                    ),
                }
                assert_eq!(
                    wikino.commands(),
                    if verification == 2 {
                        vec!["draft create", "draft list", "page list"]
                    } else {
                        vec!["draft create", "draft list"]
                    },
                    "作成結果を確かめていない: 失敗{failure}、確認{verification}"
                );
            }
        }
    }

    #[test]
    fn コマンドが起動できないときは作成結果の一覧確認をしない() {
        for missing in [true, false] {
            let wikino = FakeWikino::fallible(move |_| {
                Err(if missing {
                    process::Error::NotFound("wikino".to_owned())
                } else {
                    process::Error::Spawn {
                        program: "wikino".to_owned(),
                        source: io::Error::from(io::ErrorKind::PermissionDenied),
                    }
                })
            });
            let location = location();
            WikinoStore::new(&wikino, &location)
                .create(TITLE, BODY)
                .expect_err("起動できないのに成功した");
            assert_eq!(
                wikino.commands(),
                ["draft create"],
                "起動前の失敗で一覧を読んだ"
            );
        }
    }

    #[test]
    fn 一覧は置き場のトピックのタイトルのある下書きだけを返す() {
        let wikino = FakeWikino::new(|_| ok(LISTED));
        let location = location();
        let store = WikinoStore::new(&wikino, &location);

        assert_eq!(
            store.list().expect("一覧を読めなかった"),
            [Listed {
                number: 30,
                title: TITLE.to_owned(),
            }]
        );
        assert_eq!(store.find(TITLE).expect("探せなかった"), Some(30));
        assert_eq!(store.find("計画書").expect("探せなかった"), None);
        assert_eq!(
            wikino.calls.into_inner()[0],
            [
                "draft",
                "list",
                "--all",
                "--fields=page_number,title,topic_number",
                "--host=https://wikino.app",
                "--space=shimbaco",
                "--json",
            ]
        );
    }

    #[test]
    fn 結果不明の下書きが公開されていても_同じトピックのページを回収する() {
        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft create" => exit(1, "通信に失敗しました"),
            "draft list" => ok(""),
            "page list" => ok(&format!(
                "{{\"number\":29,\"title\":{title},\"topic_number\":4}}\n{{\"number\":30,\"title\":{title},\"topic_number\":12}}\n",
                title = serde_json::to_string(TITLE).expect("タイトルをJSONにできなかった"),
            )),
            other => panic!("予想外のコマンド: {other}"),
        });
        let location = location();
        let store = WikinoStore::new(&wikino, &location);
        assert_eq!(
            store
                .create(TITLE, BODY)
                .expect("公開済みページを回収できなかった"),
            30
        );
        assert_eq!(
            wikino.commands(),
            ["draft create", "draft list", "page list"]
        );
        let calls = wikino.calls.borrow();
        for flag in [
            "--all".to_owned(),
            "--topic=12".to_owned(),
            format!("--title={TITLE}"),
            "--status=published".to_owned(),
            "--fields=number,title,topic_number".to_owned(),
        ] {
            assert!(
                calls[2].contains(&flag),
                "公開済みの検索に {flag} を渡していない"
            );
        }
    }

    #[test]
    fn 公開済みページの一覧も読めなければ_作成結果が不明とする() {
        for output in [exit(8, "利用量の上限"), ok("読めないJSON")] {
            let wikino = FakeWikino::new(move |args| match command(args).as_str() {
                "draft create" => exit(1, ""),
                "draft list" => ok(""),
                "page list" => output.clone(),
                other => panic!("予想外のコマンド: {other}"),
            });
            let location = location();
            let result = WikinoStore::new(&wikino, &location).create(TITLE, BODY);
            assert!(
                matches!(result, Err(Error::Unconfirmed { .. })),
                "公開済みの確認ができないのに再作成できる: {result:?}"
            );
            assert_eq!(
                wikino.commands(),
                ["draft create", "draft list", "page list"]
            );
        }
    }

    /// 下書きdraftとページpageを返すwikinoで、項目30を読む。
    fn read(draft: Output, page: Output) -> (Result<Status, Error>, Vec<String>) {
        let wikino = FakeWikino::new(move |args| match command(args).as_str() {
            "draft get" => draft.clone(),
            _ => page.clone(),
        });
        let location = location();
        let got = WikinoStore::new(&wikino, &location).read(30);
        (got, wikino.commands())
    }

    #[test]
    fn 下書きがあれば人間待ちと読む() {
        let (got, commands) = read(
            ok(
                r#"{"page_number":30,"title":"項目","topic_number":12,"body":"本文\n","etag":"W/\"2\""}"#,
            ),
            exit(99, ""),
        );
        assert_eq!(
            got.expect("読めなかった"),
            Status::Drafted {
                title: Some("項目".to_owned()),
                body: "本文\n".to_owned(),
                etag: "W/\"2\"".to_owned(),
            }
        );
        assert_eq!(commands, ["draft get"]);
    }

    #[test]
    fn 下書きが無く公開済みなら回答済みと読む() {
        let (got, commands) = read(
            exit(5, ""),
            ok(
                r#"{"number":30,"title":"項目","topic_number":12,"body":"回答\n","status":"published","etag":"W/\"3\""}"#,
            ),
        );
        assert_eq!(
            got.expect("読めなかった"),
            Status::Published {
                title: Some("項目".to_owned()),
                body: "回答\n".to_owned(),
            }
        );
        assert_eq!(commands, ["draft get", "page get"]);
    }

    #[test]
    fn 下書きが無くページが未公開か無ければ無くなったと読む() {
        let unpublished = ok(
            r#"{"number":30,"title":null,"topic_number":12,"body":"","status":"unpublished","etag":"W/\"4\""}"#,
        );
        for page in [unpublished, exit(5, "")] {
            let (got, _) = read(exit(5, ""), page);
            assert_eq!(got.expect("読めなかった"), Status::Missing);
        }
    }

    #[test]
    fn 置き場のトピックでないページは読まない() {
        let cases = [
            (
                ok(
                    r#"{"page_number":30,"title":"計画書","topic_number":4,"body":"","etag":"W/\"1\""}"#,
                ),
                exit(99, ""),
            ),
            (
                exit(5, ""),
                ok(
                    r#"{"number":30,"title":"計画書","topic_number":4,"body":"","status":"published","etag":"W/\"1\""}"#,
                ),
            ),
        ];
        for (draft, page) in cases {
            let (got, _) = read(draft, page);
            let err = got.expect_err("ほかのトピックのページを読んだ");
            assert_eq!(
                err.to_string(),
                "ページ 30 は、人間キューの置き場のトピック 12 ではなく、トピック 4 のページです"
            );
        }
    }

    #[test]
    fn 読み取りの失敗と読めない出力はエラー() {
        let (got, _) = read(exit(3, "ログインしてください"), exit(99, ""));
        let err = got.expect_err("エラーにならなかった").to_string();
        assert!(
            err.starts_with("人間キューの項目 30 を読めません: ログインしていないか"),
            "エラー = {err}"
        );

        let (got, _) = read(exit(5, ""), ok(r#"{"number":30,"status":"draft"}"#));
        let err = got.expect_err("読めない出力でエラーにならなかった");
        assert!(
            matches!(
                err,
                Error::Output {
                    command: "page get",
                    ..
                }
            ),
            "エラー = {err:?}"
        );
    }

    /// 削除で出力outputを返すwikinoで、項目30の下書きを版etagで削除する。
    fn delete(output: Output) -> (Result<(), Error>, Vec<Vec<String>>) {
        let wikino = FakeWikino::new(move |args| match command(args).as_str() {
            "draft get" => {
                ok(r#"{"title":"項目","topic_number":12,"body":"本文","etag":"W/\"new\""}"#)
            }
            _ => output.clone(),
        });
        let location = location();
        let got = WikinoStore::new(&wikino, &location).delete(30, "W/\"2\"");
        (got, wikino.calls.into_inner())
    }

    #[test]
    fn 読んだ版を渡して確認を省いて削除する() {
        let (got, calls) = delete(ok(r#"{"page_number":30,"deleted":true}"#));
        got.expect("削除できなかった");
        assert_eq!(
            calls[1..],
            [[
                "draft",
                "delete",
                "30",
                "--if-match=W/\"2\"",
                "--yes",
                "--host=https://wikino.app",
                "--space=shimbaco",
                "--json",
            ]]
        );
    }

    #[test]
    fn 下書きが既に無ければ削除できたとみなす() {
        let (got, _) = delete(exit(5, ""));
        got.expect("下書きが無いときにエラーになった");
    }

    #[test]
    fn 読んだあとに下書きが更新されていれば削除しない() {
        let (got, _) = delete(exit(6, ""));
        let err = got.expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Changed(30)), "エラー = {err:?}");

        let (got, _) = delete(exit(4, ""));
        let err = got.expect_err("エラーにならなかった").to_string();
        assert!(
            err.starts_with(
                "人間キューの項目 30 の下書きを削除できません: wikino draft delete が終了コード 4"
            ),
            "エラー = {err}"
        );
    }

    /// 作成で出力outputを返すwikinoで、項目30に本文BODYの下書きを作り直す。
    fn reopen(output: Output) -> (Result<(), Error>, FakeWikino) {
        let wikino = FakeWikino::new(move |args| match command(args).as_str() {
            "draft get" => exit(5, ""),
            "page get" => {
                ok(r#"{"title":"項目","topic_number":12,"body":"回答","status":"published"}"#)
            }
            _ => output.clone(),
        });
        let location = location();
        let got = WikinoStore::new(&wikino, &location).reopen(30, BODY);
        (got, wikino)
    }

    #[test]
    fn 公開済みの項目にタイトルを変えずに下書きを作り直す() {
        let (got, wikino) = reopen(ok(r#"{"page_number":30,"etag":"W/\"5\""}"#));
        got.expect("作り直せなかった");
        let calls = wikino.calls.into_inner();
        let args = &calls[2];
        assert_eq!(args[..3], ["draft", "create", "30"]);
        assert!(
            !args
                .iter()
                .any(|a| a.starts_with("--title=") || a.starts_with("--topic=")),
            "タイトルかトピックを渡した: {args:?}"
        );
        assert_eq!(wikino.bodies.into_inner(), [BODY]);
    }

    #[test]
    fn 作り直す項目に下書きがあるかページが無ければエラー() {
        let (got, _) = reopen(exit(6, ""));
        let err = got.expect_err("エラーにならなかった");
        assert!(matches!(err, Error::DraftExists(30)), "エラー = {err:?}");

        let (got, _) = reopen(exit(5, ""));
        let err = got.expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NoPage(30)), "エラー = {err:?}");

        let (got, _) = reopen(exit(1, ""));
        let err = got.expect_err("エラーにならなかった").to_string();
        assert!(
            err.starts_with(
                "人間キューの項目 30 に下書きを作り直せません: wikino draft create が終了コード 1"
            ),
            "エラー = {err}"
        );
    }

    #[test]
    fn 別トピックの下書きとページには削除も差し戻しも送らない() {
        for published in [false, true] {
            for deletion in [false, true] {
                let wikino = FakeWikino::new(move |args| match command(args).as_str() {
                    "draft get" if published => exit(5, ""),
                    "draft get" => ok(
                        r#"{"title":"計画書","topic_number":10,"body":"計画書の下書き","etag":"old"}"#,
                    ),
                    "page get" => ok(
                        r#"{"title":"計画書","topic_number":10,"body":"計画書","status":"published"}"#,
                    ),
                    other => panic!("別トピックへの書き込みを送った: {other}"),
                });
                let location = location();
                let store = WikinoStore::new(&wikino, &location);
                let result = if deletion {
                    store.delete(30, "old")
                } else {
                    store.reopen(30, BODY)
                };
                assert!(
                    matches!(
                        result,
                        Err(Error::OtherTopic {
                            number: 30,
                            expected: 12,
                            actual: 10
                        })
                    ),
                    "トピック違いのエラーではない: {result:?}"
                );
                assert_eq!(
                    wikino.commands().len(),
                    if published { 2 } else { 1 },
                    "対象を確かめる以外のコマンドを送った"
                );
            }
        }
    }

    #[test]
    fn 対象が既に無いときは削除成功とし_差し戻しの要求も送らない() {
        for page in [
            exit(5, ""),
            ok(r#"{"title":null,"topic_number":12,"body":"","status":"unpublished"}"#),
        ] {
            for deletion in [false, true] {
                let page = page.clone();
                let wikino = FakeWikino::new(move |args| match command(args).as_str() {
                    "draft get" => exit(5, ""),
                    "page get" => page.clone(),
                    other => panic!("無い対象への書き込みを送った: {other}"),
                });
                let location = location();
                let store = WikinoStore::new(&wikino, &location);
                if deletion {
                    store
                        .delete(30, "old")
                        .expect("既に無い下書きの削除に失敗した");
                } else {
                    assert!(
                        matches!(store.reopen(30, BODY), Err(Error::NoPage(30))),
                        "対象が無いエラーではない"
                    );
                }
                assert_eq!(wikino.commands(), ["draft get", "page get"]);
            }
        }
    }

    #[test]
    fn 既存の下書きがあれば差し戻しを書き込まない() {
        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft get" => {
                ok(r#"{"title":"項目","topic_number":12,"body":"書きかけの回答","etag":"new"}"#)
            }
            other => panic!("書きかけの下書きに書き込みを送った: {other}"),
        });
        let location = location();
        let result = WikinoStore::new(&wikino, &location).reopen(30, BODY);
        assert!(
            matches!(result, Err(Error::DraftExists(30))),
            "既存の下書きを拒否しない: {result:?}"
        );
        assert_eq!(wikino.commands(), ["draft get"]);
    }
    /// 書き換えで出力outputを返すwikinoで、項目30の版etagの下書きを本文BODYに書き換える。
    fn update(output: Output) -> (Result<(), Error>, FakeWikino) {
        let wikino = FakeWikino::new(move |args| match command(args).as_str() {
            "draft get" => {
                ok(r#"{"title":"項目","topic_number":12,"body":"本文","etag":"W/\"2\""}"#)
            }
            _ => output.clone(),
        });
        let location = location();
        let got = WikinoStore::new(&wikino, &location).update(30, BODY, "W/\"2\"");
        (got, wikino)
    }

    #[test]
    fn 読んだ版を渡して下書きを書き換える() {
        let (got, wikino) = update(ok(r#"{"page_number":30,"etag":"W/\"3\""}"#));
        got.expect("書き換えられなかった");
        let calls = wikino.calls.borrow();
        assert_eq!(calls[1][..3], ["draft", "update", "30"]);
        assert!(
            calls[1].contains(&"--if-match=W/\"2\"".to_owned()),
            "読んだ版を渡していない: {:?}",
            calls[1]
        );
        assert_eq!(*wikino.bodies.borrow(), [BODY]);
    }

    #[test]
    fn 書き換える下書きが更新されたか無ければエラー() {
        let (got, _) = update(exit(6, ""));
        let err = got.expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Changed(30)), "エラー = {err:?}");

        let wikino = FakeWikino::new(|args| match command(args).as_str() {
            "draft get" => exit(5, ""),
            "page get" => {
                ok(r#"{"title":"項目","topic_number":12,"body":"回答","status":"published"}"#)
            }
            other => panic!("公開済みの項目に書き込もうとした: {other}"),
        });
        let location = location();
        let err = WikinoStore::new(&wikino, &location)
            .update(30, BODY, "W/\"2\"")
            .expect_err("公開済みの項目に書いた");
        assert!(matches!(err, Error::NoDraft(30)), "エラー = {err:?}");
    }
}
