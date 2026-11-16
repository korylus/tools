//! 作業計画書とロードマップの読み取り元 (Wikinoのページ)。
//!
//! 読み取り元の違いはこのモジュールに閉じ込め、呼び出し側は読んだ本文を作業計画書の
//! 読み取りに渡し、[`Wikino::name`] で計画書名を決めるだけにする。

use std::ffi::OsString;
use std::fmt;

use crate::process::{self, Runner};

/// 計画書とロードマップのURLとして受け付け、wikinoコマンドの接続先にするホスト。
///
/// 受け付けたURLと別のホストのページを読まないよう、両方をこの値から作る。
pub const WIKINO_HOST: &str = "https://wikino.app";

/// wikinoコマンドの名前。
const WIKINO: &str = "wikino";

// wikinoの終了コード。
const EXIT_AUTH: i32 = 3;
pub(crate) const EXIT_NOT_FOUND: i32 = 5;
const EXIT_RATE_LIMITED: i32 = 8;

/// 計画書やロードマップを読み取れないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 文書の指定がWikinoのページのURLの形ではない。
    #[error("{kind}の指定 {arg} を読み取れません。{host}/s/<スペース>/pages/<ページ番号> の形のURLで指定してください", host = WIKINO_HOST)]
    InvalidUrl {
        /// 読み取る文書の種類。
        kind: Kind,
        /// 受け取った指定。
        arg: String,
    },

    /// 文書のURLのページ番号が正の整数ではない。
    #[error("{kind}のURL {arg} のページ番号を読み取れません")]
    InvalidNumber {
        /// 読み取る文書の種類。
        kind: Kind,
        /// 受け取ったURL。
        arg: String,
    },

    /// 文書の下書きもページも無い。
    #[error("{kind} {url} が見つかりません。URLのスペースとページ番号を確かめてください")]
    NotFound {
        /// 読み取る文書の種類。
        kind: Kind,
        /// 文書のURL。
        url: String,
    },

    /// wikinoコマンドが失敗した。
    #[error(
        "{kind} {url} を読めません: {reason}。wikino auth status --host={host} --space={space} で状態を確かめてください{}",
        stderr_line(.stderr),
        host = WIKINO_HOST
    )]
    Read {
        /// 読み取る文書の種類。
        kind: Kind,
        /// 文書のURL。
        url: String,
        /// 文書のスペース。
        space: String,
        /// 失敗した理由。
        reason: ReadFailure,
        /// wikinoコマンドの標準エラー。
        stderr: String,
    },

    /// wikinoコマンドの出力のfrontmatterが閉じていない。
    #[error("wikinoの出力のfrontmatterが閉じていません")]
    UnclosedFrontmatter,

    /// wikinoコマンドの出力がUTF-8ではなく、データとして読めない。
    #[error("wikinoの出力がUTF-8ではありません")]
    OutputEncoding(#[source] std::string::FromUtf8Error),

    /// 計画書に計画書名の行が無い。
    #[error(
        "計画書 {0} に計画書名の行がありません。最初の「## 」の見出しより前に「**計画書名**: <英小文字・数字・ハイフンの名前>」の行を書いてください"
    )]
    NoName(String),

    /// wikinoコマンドが見つからない。
    #[error("wikinoコマンドが見つかりません。Wikino CLIをインストールしてログインしてください")]
    WikinoNotFound,

    /// wikinoコマンドを実行できなかった。
    #[error(transparent)]
    Run(process::Error),
}

/// wikinoコマンドが失敗した理由。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ReadFailure {
    /// ログインしていないか、資格情報が無効。
    Auth,
    /// 公開APIの利用量の上限に当たった。
    RateLimited,
    /// そのほかの終了コードで失敗した。
    Exit {
        /// 失敗したwikinoのサブコマンド (`draft get` など)。
        command: String,
        /// 終了コード。
        code: i32,
    },
}

impl fmt::Display for ReadFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Auth => f.write_str("ログインしていないか、資格情報が無効です"),
            Self::RateLimited => f.write_str("公開APIの利用量の上限に当たりました"),
            Self::Exit { command, code } => {
                write!(f, "wikino {command} が終了コード {code} で失敗しました")
            }
        }
    }
}

/// 標準エラーを、空でなければ改行に続けてメッセージに添える形にする。
pub(crate) fn stderr_line(stderr: &str) -> String {
    match stderr.trim() {
        "" => String::new(),
        s => format!("\n{s}"),
    }
}

/// Wikinoのページで読む文書の種類。エラーのメッセージで、どの文書を読めないかを示す。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// 作業計画書。
    Plan,
    /// ロードマップ。
    Roadmap,
}

impl fmt::Display for Kind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Plan => "計画書",
            Self::Roadmap => "ロードマップ",
        })
    }
}

/// wikinoコマンドを1回実行した結果。
pub(crate) enum Outcome {
    /// 成功し、標準出力を返した。
    Found(String),
    /// 対象が無い。
    NotFound,
    /// 失敗した。
    Failed {
        /// 失敗した理由。
        reason: ReadFailure,
        /// 標準エラー。
        stderr: String,
    },
}

/// wikinoコマンドを引数argsで実行し、終了コードから結果を決める。
///
/// argsの先頭の2つは、失敗したサブコマンドとしてメッセージに使う。
pub(crate) fn run_wikino(runner: &dyn Runner, args: &[String]) -> Result<Outcome, Error> {
    let os_args: Vec<OsString> = args.iter().map(OsString::from).collect();
    let output = runner.run(WIKINO, &os_args).map_err(|err| match err {
        process::Error::NotFound(_) => Error::WikinoNotFound,
        err => Error::Run(err),
    })?;
    let reason = match output.code {
        // 出力はデータとして読むため、置換せずにUTF-8として検証する。
        // 標準エラーは表示に添えるだけのため、読めない部分を置換して使う。
        0 => {
            let stdout = String::from_utf8(output.stdout).map_err(Error::OutputEncoding)?;
            return Ok(Outcome::Found(stdout));
        }
        EXIT_NOT_FOUND => return Ok(Outcome::NotFound),
        EXIT_AUTH => ReadFailure::Auth,
        EXIT_RATE_LIMITED => ReadFailure::RateLimited,
        code => ReadFailure::Exit {
            command: args.iter().take(2).cloned().collect::<Vec<_>>().join(" "),
            code,
        },
    };
    Ok(Outcome::Failed {
        reason,
        stderr: String::from_utf8_lossy(&output.stderr).into_owned(),
    })
}

/// Wikinoのページから読んだ文書。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Document {
    /// frontmatterを除いた本文。
    pub body: String,
    /// ページが属するトピックの番号。wikinoの出力のfrontmatterに無ければ `None`。
    pub topic_number: Option<u64>,
}

/// WikinoのページのURLで指定した文書。wikinoコマンドを子プロセスで呼んで読む。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Wikino {
    /// 文書の種類。
    pub kind: Kind,
    /// 文書のURL。
    pub url: String,
    /// 文書のスペースの識別子。
    pub space: String,
    /// 文書のページ番号。
    pub number: u64,
}

impl Wikino {
    /// コマンドで受け取った計画書の指定argを読み取る。
    ///
    /// argはWikinoのページのURLである。ページ番号だけの指定はスペースを決められず、
    /// パスなどほかの指定は読み取り元が無いため、URLの形を示すエラーにする。
    pub fn parse(arg: &str) -> Result<Self, Error> {
        Self::parse_as(Kind::Plan, arg)
    }

    /// 種類kindの文書の指定argを、[`Wikino::parse`] と同じ規則で読み取る。
    pub fn parse_as(kind: Kind, arg: &str) -> Result<Self, Error> {
        let (space, number) = split_page_url(arg).ok_or_else(|| Error::InvalidUrl {
            kind,
            arg: arg.to_owned(),
        })?;
        // 範囲はGoのintと同じく、64ビットの符号付き整数の正の値にする。
        let number = number
            .parse::<i64>()
            .ok()
            .and_then(|n| u64::try_from(n).ok())
            .filter(|&n| n > 0)
            .ok_or_else(|| Error::InvalidNumber {
                kind,
                arg: arg.to_owned(),
            })?;
        Ok(Self {
            kind,
            url: arg.to_owned(),
            space: space.to_owned(),
            number,
        })
    }

    /// スペースspaceのページ番号numberのページを、種類kindの文書として指す。
    pub fn page(kind: Kind, space: &str, number: u64) -> Self {
        Self {
            kind,
            url: format!("{WIKINO_HOST}/s/{space}/pages/{number}"),
            space: space.to_owned(),
            number,
        }
    }

    /// 下書きがあれば下書きを、無ければページを読み、frontmatterを除いた本文を返す。
    ///
    /// 実装者が付けたチェックや実装判断ログは、公開するまで下書きにしか無いためである。
    pub fn read(&self, runner: &dyn Runner) -> Result<String, Error> {
        self.fetch(runner).map(|document| document.body)
    }

    /// [`Wikino::read`] と同じく読み、本文とページが属するトピックの番号を返す。
    pub fn fetch(&self, runner: &dyn Runner) -> Result<Document, Error> {
        for kind in ["draft", "page"] {
            let args = [
                kind.to_owned(),
                "get".to_owned(),
                self.number.to_string(),
                format!("--host={WIKINO_HOST}"),
                format!("--space={}", self.space),
                "--md".to_owned(),
            ];
            match run_wikino(runner, &args)? {
                Outcome::Found(stdout) => return parse_page_file(&stdout),
                Outcome::NotFound => {}
                Outcome::Failed { reason, stderr } => {
                    return Err(Error::Read {
                        kind: self.kind,
                        url: self.url.clone(),
                        space: self.space.clone(),
                        reason,
                        stderr,
                    });
                }
            }
        }
        Err(Error::NotFound {
            kind: self.kind,
            url: self.url.clone(),
        })
    }

    /// 作業計画書から読み取った計画書名nameを返す。
    ///
    /// 計画書名は状態の置き場とブランチ名に使うため、計画書名の行を必須にする。
    pub fn name(&self, name: &str) -> Result<String, Error> {
        if name.is_empty() {
            return Err(Error::NoName(self.url.clone()));
        }
        Ok(name.to_owned())
    }
}

/// WikinoのページのURLから、スペースとページ番号の文字列を取り出す。
///
/// スペースは `-` で始まる値をwikinoコマンドにフラグとして読ませないよう、先頭にハイフンを許さない。
fn split_page_url(arg: &str) -> Option<(&str, &str)> {
    let rest = arg.strip_prefix(WIKINO_HOST)?.strip_prefix("/s/")?;
    let (space, number) = rest.split_once("/pages/")?;
    let mut chars = space.chars();
    let valid_space = chars
        .next()
        .is_some_and(|c| c.is_ascii_alphanumeric() || c == '_')
        && chars.all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-');
    let valid_number = !number.is_empty() && number.bytes().all(|b| b.is_ascii_digit());
    (valid_space && valid_number).then_some((space, number))
}

/// wikinoの `--md` の出力 (frontmatter付きのMarkdown) を、本文とトピックの番号に分ける。
fn parse_page_file(s: &str) -> Result<Document, Error> {
    let s = s.replace("\r\n", "\n");
    let Some(rest) = s.strip_prefix("---\n") else {
        return Ok(Document {
            body: s,
            topic_number: None,
        });
    };
    let (front, body) = rest
        .split_once("\n---\n")
        .ok_or(Error::UnclosedFrontmatter)?;
    let topic_number = front
        .lines()
        .find_map(|line| line.strip_prefix("wikino_topic_number:"))
        .and_then(|value| value.trim().parse().ok());
    Ok(Document {
        body: body.strip_prefix('\n').unwrap_or(body).to_owned(),
        topic_number,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::Output;
    use std::cell::RefCell;
    use std::collections::VecDeque;

    #[test]
    fn 計画書のページの指定からスペースとページ番号を読み取る() {
        let url = "https://wikino.app/s/shimbaco_2/pages/1214";
        let got = Wikino::parse(url).expect("URLを読み取れなかった");
        assert_eq!(
            got,
            Wikino {
                kind: Kind::Plan,
                url: url.to_owned(),
                space: "shimbaco_2".to_owned(),
                number: 1214,
            }
        );
    }

    #[test]
    fn 計画書の指定がページの形でなければエラー() {
        let cases = [
            (
                "ページ番号だけ",
                "1214",
                "計画書の指定 1214 を読み取れません",
            ),
            (
                "パス",
                "docs/private/plans/1_doing/demo.md",
                "の形のURLで指定してください",
            ),
            (
                "httpのURL",
                "http://wikino.app/s/shimbaco/pages/1",
                "を読み取れません",
            ),
            (
                "別のホスト",
                "https://example.com/s/shimbaco/pages/1",
                "を読み取れません",
            ),
            (
                "ハイフンで始まるスペース",
                "https://wikino.app/s/-x/pages/1",
                "を読み取れません",
            ),
            (
                "空のスペース",
                "https://wikino.app/s//pages/1",
                "を読み取れません",
            ),
            (
                "数字でないページ番号",
                "https://wikino.app/s/shimbaco/pages/1a",
                "を読み取れません",
            ),
            (
                "ページ番号の後ろに続きがある",
                "https://wikino.app/s/shimbaco/pages/1/edit",
                "を読み取れません",
            ),
            (
                "ページ番号が0",
                "https://wikino.app/s/shimbaco/pages/0",
                "のページ番号を読み取れません",
            ),
            (
                "ページ番号が64ビットの符号付き整数に収まらない",
                "https://wikino.app/s/shimbaco/pages/9223372036854775808",
                "のページ番号を読み取れません",
            ),
        ];
        for (name, arg, want) in cases {
            let err = Wikino::parse(arg).expect_err(&format!("{name}: エラーにならなかった"));
            assert!(
                err.to_string().contains(want),
                "{name}: エラー = {err}、期待値 = {want:?} を含むエラー"
            );
        }
    }

    /// 決めた結果を順に返し、呼ばれた引数を記録するRunner。
    struct FakeRunner {
        results: RefCell<VecDeque<Result<Output, process::Error>>>,
        calls: RefCell<Vec<Vec<String>>>,
    }

    impl FakeRunner {
        fn new(results: Vec<Result<Output, process::Error>>) -> Self {
            Self {
                results: RefCell::new(results.into()),
                calls: RefCell::new(Vec::new()),
            }
        }
    }

    impl Runner for FakeRunner {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            assert_eq!(program, "wikino", "wikino以外のコマンドを実行した");
            self.calls.borrow_mut().push(
                args.iter()
                    .map(|a| a.to_string_lossy().into_owned())
                    .collect(),
            );
            self.results
                .borrow_mut()
                .pop_front()
                .expect("決めた結果より多く実行した")
        }
    }

    #[expect(
        clippy::unnecessary_wraps,
        reason = "起動の失敗 (Err) と同じ列に並べる、差し替えたwikinoの結果を作るため"
    )]
    fn ok(stdout: &str) -> Result<Output, process::Error> {
        Ok(Output {
            stdout: stdout.as_bytes().to_vec(),
            ..Output::default()
        })
    }

    #[expect(
        clippy::unnecessary_wraps,
        reason = "起動の失敗 (Err) と同じ列に並べる、差し替えたwikinoの結果を作るため"
    )]
    fn exit(code: i32, stderr: &str) -> Result<Output, process::Error> {
        Ok(Output {
            stderr: stderr.as_bytes().to_vec(),
            code,
            ..Output::default()
        })
    }

    const PAGE: &str = "---\nwikino_title: \"計画書\"\nwikino_number: 12\n---\n\n**計画書名**: demo\n\n## タスクリスト\n";
    const BODY: &str = "**計画書名**: demo\n\n## タスクリスト\n";

    fn draft_args() -> Vec<String> {
        args("draft")
    }

    fn page_args() -> Vec<String> {
        args("page")
    }

    fn args(kind: &str) -> Vec<String> {
        [
            kind,
            "get",
            "12",
            "--host=https://wikino.app",
            "--space=shimbaco",
            "--md",
        ]
        .map(str::to_owned)
        .to_vec()
    }

    /// 結果resultsを返すRunnerで計画書を読み、読んだ結果とwikinoの引数の記録を返す。
    fn read(
        results: Vec<Result<Output, process::Error>>,
    ) -> (Result<String, Error>, Vec<Vec<String>>) {
        let runner = FakeRunner::new(results);
        let wikino = Wikino {
            kind: Kind::Plan,
            url: "https://wikino.app/s/shimbaco/pages/12".to_owned(),
            space: "shimbaco".to_owned(),
            number: 12,
        };
        let got = wikino.read(&runner);
        (got, runner.calls.into_inner())
    }

    /// 読んだ結果がエラーで、メッセージがwantsをすべて含むことを確かめる。
    fn assert_err(got: Result<String, Error>, wants: &[&str]) {
        let err = got.expect_err("エラーにならなかった").to_string();
        for want in wants {
            assert!(err.contains(want), "エラーに {want:?} が無い:\n{err}");
        }
    }

    #[test]
    fn 下書きがあれば下書きを読む() {
        let (got, calls) = read(vec![ok(PAGE)]);
        assert_eq!(got.expect("読めなかった"), BODY);
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn 下書きが無ければページを読む() {
        let (got, calls) = read(vec![exit(5, ""), ok(&PAGE.replace('\n', "\r\n"))]);
        assert_eq!(got.expect("読めなかった"), BODY);
        assert_eq!(calls, [draft_args(), page_args()]);
    }

    #[test]
    fn 出力がutf8でなければ本文を置換せずエラーにする() {
        let mut stdout = "**計画書名**: demo".as_bytes().to_vec();
        stdout.push(0xff);
        stdout.extend_from_slice("\n\n## タスクリスト\n".as_bytes());
        let (got, calls) = read(vec![Ok(Output {
            stdout: stdout.clone(),
            ..Output::default()
        })]);

        let err = got.expect_err("置換した本文を返した");
        let Error::OutputEncoding(source) = &err else {
            panic!("エラーの種類が期待と異なる: {err:?}");
        };
        assert_eq!(source.as_bytes(), stdout, "元の出力を保っていない");
        assert_eq!(calls, [draft_args()], "下書きを読んだあとに続けて実行した");
    }

    #[test]
    fn frontmatterが無ければそのまま返す() {
        let (got, calls) = read(vec![ok(BODY)]);
        assert_eq!(got.expect("読めなかった"), BODY);
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn ページも無ければ見つからないエラー() {
        let (got, calls) = read(vec![exit(5, ""), exit(5, "")]);
        assert_err(
            got,
            &["計画書 https://wikino.app/s/shimbaco/pages/12 が見つかりません"],
        );
        assert_eq!(calls, [draft_args(), page_args()]);
    }

    #[test]
    fn 未ログインなら標準エラーを添える() {
        let (got, calls) = read(vec![exit(3, "ログインしてください\n")]);
        assert_err(
            got,
            &[
                "ログインしていないか",
                "wikino auth status --host=https://wikino.app --space=shimbaco",
                "\nログインしてください",
            ],
        );
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn 標準エラーが空なら改行を添えない() {
        let (got, _) = read(vec![exit(3, " \n")]);
        let err = got.expect_err("エラーにならなかった").to_string();
        assert!(!err.contains('\n'), "エラーに改行がある:\n{err}");
    }

    #[test]
    fn レート制限() {
        let (got, calls) = read(vec![exit(5, ""), exit(8, "")]);
        assert_err(got, &["公開APIの利用量の上限に当たりました"]);
        assert_eq!(calls, [draft_args(), page_args()]);
    }

    #[test]
    fn そのほかの終了コード() {
        let (got, calls) = read(vec![exit(1, "")]);
        assert_err(got, &["wikino draft get が終了コード 1 で失敗しました"]);
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn コマンドが見つからなければインストールを促す() {
        let (got, calls) = read(vec![Err(process::Error::NotFound("wikino".to_owned()))]);
        assert_err(
            got,
            &["wikinoコマンドが見つかりません。Wikino CLIをインストール"],
        );
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn 起動できなければそのエラー() {
        let (got, calls) = read(vec![Err(process::Error::Signaled("wikino".to_owned()))]);
        assert_err(got, &["wikinoコマンドがシグナルで終了しました"]);
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn frontmatterが閉じていなければエラー() {
        let (got, calls) = read(vec![ok("---\nwikino_number: 12\n")]);
        assert_err(got, &["frontmatterが閉じていません"]);
        assert_eq!(calls, [draft_args()]);
    }

    #[test]
    fn frontmatterからトピックの番号を読む() {
        let runner = FakeRunner::new(vec![ok(
            "---\nwikino_number: 12\nwikino_topic_number: 4\n---\n\n本文\n",
        )]);
        let got = Wikino::page(Kind::Roadmap, "shimbaco", 12)
            .fetch(&runner)
            .expect("読めなかった");
        assert_eq!(
            got,
            Document {
                body: "本文\n".to_owned(),
                topic_number: Some(4),
            }
        );
        assert_eq!(runner.calls.into_inner(), [draft_args()]);
    }

    #[test]
    fn 計画書名の行があればその名前を返す() {
        let wikino =
            Wikino::parse("https://wikino.app/s/shimbaco/pages/12").expect("URLを読み取れなかった");
        assert_eq!(wikino.name("demo").expect("名前を返さなかった"), "demo");
        let err = wikino
            .name("")
            .expect_err("名前が空でもエラーにならなかった");
        assert!(
            err.to_string().contains("計画書名の行がありません"),
            "エラー = {err}"
        );
    }
}
