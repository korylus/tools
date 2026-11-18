//! ロードマップから、これから進める作業計画書の並びを読み取る。
//!
//! 承認とみなす節の項目にある作業計画書へのWikiリンクから、タイトルで作業計画書のページを探し、
//! 計画書名を確かめる。まだ公開していない作業計画書はタイトルが下書きにしか無いため、
//! 自分の下書きを先に探し、無ければ公開済みのページを探す。
//! 完了を記録した作業計画書は、承認とみなす節にあっても並びに入れない。

use std::collections::HashSet;

use korylusfactory_core::queue::{self, WikiLink};
pub use korylusfactory_core::queue::{Entry, State};
use korylusfactory_core::state::{self, Store};
use serde::Deserialize;

use crate::config::Config;
use crate::process::Runner;
use crate::source::{
    self, Kind, Outcome, ReadFailure, WIKINO_HOST, Wikino, run_wikino, stderr_line,
};

/// 下書きの一覧から読む項目。
const DRAFT_FIELDS: &str = "page_number,title,topic_number,topic_name";

/// ページの一覧から読む項目。
const PAGE_FIELDS: &str = "number,title,topic_number,topic_name";

/// ロードマップを読み取れないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// ロードマップか作業計画書を読めない。
    #[error(transparent)]
    Source(#[from] source::Error),

    /// ロードマップのトピックの番号が、wikinoの出力に無い。同じトピックへのWikiリンクを探せない。
    #[error("ロードマップ {0} のトピックの番号を読み取れません")]
    NoTopic(String),

    /// 進める作業計画書を決められない。
    #[error(transparent)]
    Queue(#[from] queue::Error),

    /// 作業計画書の完了を記録したかを、状態ファイルから確かめられない。
    #[error(transparent)]
    State(#[from] state::Error),

    /// wikinoコマンドで作業計画書のページを探せない。
    #[error(
        "作業計画書のページを探せません: {reason}。wikino auth status --host={host} --space={space} で状態を確かめてください{}",
        stderr_line(.stderr),
        host = WIKINO_HOST
    )]
    Search {
        /// 探したスペース。
        space: String,
        /// 失敗した理由。
        reason: ReadFailure,
        /// wikinoコマンドの標準エラー。
        stderr: String,
    },

    /// wikinoコマンドの一覧の出力を読み取れない。
    #[error("wikino {command} の出力を読み取れません")]
    SearchOutput {
        /// 出力を読み取れなかったwikinoのサブコマンド。
        command: &'static str,
        /// 読み取れなかった理由。
        #[source]
        source: serde_json::Error,
    },
}

/// wikinoの一覧の1件。
#[derive(Debug, Deserialize)]
struct Listed {
    /// 下書きの一覧では `page_number`、ページの一覧では `number` で出る。
    #[serde(alias = "page_number")]
    number: u64,
    title: Option<String>,
    topic_number: u64,
    topic_name: String,
}

impl Listed {
    /// リンクlinkの指すページかどうかを返す。トピック名の無いリンクは、ロードマップのトピックtopic_numberのページを指す。
    fn is(&self, link: &WikiLink, topic_number: u64) -> bool {
        self.title.as_deref() == Some(link.title.as_str())
            && match &link.topic {
                Some(topic) => &self.topic_name == topic,
                None => self.topic_number == topic_number,
            }
    }
}

/// 設定configのロードマップを読み、これから進める作業計画書を進める順に返す。
///
/// 同じページを指す候補が複数あれば、最初のものだけを残す。
/// 状態の置き場storeに完了を記録した作業計画書は返さない。
pub fn load(runner: &dyn Runner, config: &Config, store: &Store) -> Result<Vec<Entry>, Error> {
    let roadmap = config.roadmap.fetch(runner)?;
    let topic_number = roadmap
        .topic_number
        .ok_or_else(|| Error::NoTopic(config.roadmap.url.clone()))?;
    let candidates = queue::candidates(&roadmap.body, &config.sections)?;
    if candidates.is_empty() {
        return Ok(Vec::new());
    }

    let space = &config.roadmap.space;
    let drafts = list(
        runner,
        space,
        "draft list",
        &[format!("--fields={DRAFT_FIELDS}")],
    )?;
    // 同じトピックのページは、トピック名を付けたリンクでも付けないリンクでも指せる。
    // coreでは別のリンクになるため、見つけたページの番号でも重複を除く。
    let mut pages = HashSet::new();
    let mut entries = Vec::new();
    for candidate in candidates {
        let number = page_number(runner, space, topic_number, &drafts, &candidate.link)?;
        if let Some(number) = number
            && !pages.insert(number)
        {
            continue;
        }
        let state = match number {
            Some(number) => read_plan(runner, &Wikino::page(Kind::Plan, space, number))?,
            None => State::NotFound,
        };
        if let State::Ready { name, .. } = &state
            && store.is_completed(name)?
        {
            continue;
        }
        entries.push(Entry { candidate, state });
    }
    Ok(entries)
}

/// スペースspaceのリンクlinksに、ページ番号numberのページを指すものがあるかを返す。
///
/// トピック名の無いリンクは、ロードマップのトピックtopic_numberのページに解決する。
pub(crate) fn links_to(
    runner: &dyn Runner,
    space: &str,
    topic_number: u64,
    links: &[WikiLink],
    number: u64,
) -> Result<bool, Error> {
    let drafts = list(
        runner,
        space,
        "draft list",
        &[format!("--fields={DRAFT_FIELDS}")],
    )?;
    for link in links {
        if page_number(runner, space, topic_number, &drafts, link)? == Some(number) {
            return Ok(true);
        }
    }
    Ok(false)
}

/// リンクlinkの指すページ番号を、下書きdraftsを優先し、無ければ公開済みのページから求める。
fn page_number(
    runner: &dyn Runner,
    space: &str,
    topic_number: u64,
    drafts: &[Listed],
    link: &WikiLink,
) -> Result<Option<u64>, Error> {
    if let Some(draft) = drafts.iter().find(|draft| draft.is(link, topic_number)) {
        return Ok(Some(draft.number));
    }
    Ok(list(
        runner,
        space,
        "page list",
        &[
            format!("--title={}", link.title),
            format!("--fields={PAGE_FIELDS}"),
        ],
    )?
    .iter()
    .find(|page| page.is(link, topic_number))
    .map(|page| page.number))
}

/// 作業計画書planを読み、計画書名を確かめる。
fn read_plan(runner: &dyn Runner, plan: &Wikino) -> Result<State, Error> {
    let body = match plan.read(runner) {
        Ok(body) => body,
        Err(source::Error::NotFound { .. }) => return Ok(State::NotFound),
        Err(err) => return Err(err.into()),
    };
    let name = queue::plan_name(&body)
        .map_err(|err| err.to_string())
        .and_then(|name| plan.name(&name).map_err(|err| err.to_string()));
    Ok(match name {
        Ok(name) => State::Ready {
            url: plan.url.clone(),
            name,
        },
        Err(reason) => State::Invalid {
            url: plan.url.clone(),
            reason,
        },
    })
}

/// スペースspaceで `wikino <command>` を全件について実行し、一覧を返す。
///
/// 値はフラグとして読まれないよう、`--名前=値` の1つの引数で渡す。
fn list(
    runner: &dyn Runner,
    space: &str,
    command: &'static str,
    flags: &[String],
) -> Result<Vec<Listed>, Error> {
    let mut args: Vec<String> = command.split(' ').map(str::to_owned).collect();
    args.extend([
        format!("--host={WIKINO_HOST}"),
        format!("--space={space}"),
        "--all".to_owned(),
    ]);
    args.extend_from_slice(flags);
    let stdout = match run_wikino(runner, &args)? {
        Outcome::Found(stdout) => stdout,
        // 一覧は該当が無くても成功するため、見つからない終了コードもほかの失敗と同じく扱う。
        Outcome::NotFound => {
            return Err(Error::Search {
                space: space.to_owned(),
                reason: ReadFailure::Exit {
                    command: command.to_owned(),
                    code: source::EXIT_NOT_FOUND,
                },
                stderr: String::new(),
            });
        }
        Outcome::Failed { reason, stderr } => {
            return Err(Error::Search {
                space: space.to_owned(),
                reason,
                stderr,
            });
        }
    };
    // `--all` を付けると、1行に1件のJSON Linesで出る。
    stdout
        .lines()
        .filter(|line| !line.trim().is_empty())
        .map(|line| {
            serde_json::from_str(line).map_err(|source| Error::SearchOutput { command, source })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::{self, Output};
    use korylusfactory_core::queue::Candidate;
    use korylusfactory_core::state::PlanState;
    use std::cell::RefCell;
    use std::ffi::OsString;
    use std::fs;

    /// 節sectionに書いた、トピックtopicのタイトルtitleへのリンクの候補。
    fn link(section: &str, topic: Option<&str>, title: &str) -> Candidate {
        Candidate {
            section: section.to_owned(),
            link: WikiLink {
                topic: topic.map(str::to_owned),
                title: title.to_owned(),
            },
        }
    }

    /// wikinoの引数から出力を決める関数。
    type Respond = Box<dyn Fn(&[String]) -> Output>;

    /// wikinoのサブコマンドと引数に応じて決めた出力を返し、呼ばれた引数を記録するRunner。
    struct FakeWikino {
        respond: Respond,
        calls: RefCell<Vec<Vec<String>>>,
    }

    impl FakeWikino {
        fn new(respond: impl Fn(&[String]) -> Output + 'static) -> Self {
            Self {
                respond: Box::new(respond),
                calls: RefCell::new(Vec::new()),
            }
        }
    }

    impl Runner for FakeWikino {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            assert_eq!(program, "wikino", "wikino以外のコマンドを実行した");
            let args: Vec<String> = args
                .iter()
                .map(|a| a.to_string_lossy().into_owned())
                .collect();
            let output = (self.respond)(&args);
            self.calls.borrow_mut().push(args);
            Ok(output)
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

    /// 引数argsのうち、`--名前=` で始まるものの値を返す。
    fn flag<'a>(args: &'a [String], name: &str) -> Option<&'a str> {
        args.iter()
            .find_map(|arg| arg.strip_prefix(&format!("--{name}=")))
    }

    fn config() -> Config {
        Config {
            roadmap: Wikino::page(Kind::Roadmap, "shimbaco", 1214),
            sections: vec!["やってる".to_owned(), "優先度: 高".to_owned()],
            done_sections: vec!["やった".to_owned()],
            project: None,
        }
    }

    /// 状態ファイルの無いプロジェクトで、config()のロードマップを読む。
    fn load_new(runner: &dyn Runner) -> Result<Vec<Entry>, Error> {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        load(runner, &config(), &Store::new(root.path()))
    }

    const ROADMAP: &str = "\
---
wikino_title: \"ロードマップ\"
wikino_number: 1214
wikino_topic_number: 4
---

## やってる

- [[下書きだけ 作業計画書]]

## 優先度: 高

- [[公開済み 作業計画書]]
- [[Annict/別のトピック 作業計画書]]
- [[まだ無い 作業計画書]]
- [[計画書名が無い 作業計画書]]
- [[ミュート機能]]
";

    const DRAFTS: &str = r#"{"page_number":20,"title":"下書きだけ 作業計画書","topic_number":4,"topic_name":"Mewst"}
{"page_number":21,"title":"公開済み 作業計画書","topic_number":10,"topic_name":"Korylus"}
"#;

    fn plan(name: &str) -> Output {
        ok(&format!(
            "---\nwikino_number: 1\n---\n\n**計画書名**: {name}\n\n## タスクリスト\n\n- [ ] **1-1**: タスク\n"
        ))
    }

    /// ROADMAPとDRAFTSを返し、ページの一覧はタイトルごとに決めた結果を返すwikino。
    fn wikino() -> FakeWikino {
        FakeWikino::new(|args| match (args[0].as_str(), args[1].as_str()) {
            ("draft", "get") if args[2] == "1214" => exit(5, ""),
            ("page", "get") if args[2] == "1214" => ok(ROADMAP),
            ("draft", "list") => ok(DRAFTS),
            ("page", "list") => match flag(args, "title") {
                Some("公開済み 作業計画書") => ok(concat!(
                    r#"{"number":30,"title":"公開済み 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                    "\n"
                )),
                Some("別のトピック 作業計画書") => ok(concat!(
                    r#"{"number":31,"title":"別のトピック 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                    "\n",
                    r#"{"number":32,"title":"別のトピック 作業計画書","topic_number":3,"topic_name":"Annict"}"#,
                    "\n"
                )),
                Some("計画書名が無い 作業計画書") => ok(concat!(
                    r#"{"number":33,"title":"計画書名が無い 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                    "\n"
                )),
                _ => ok(""),
            },
            ("draft", "get") => match args[2].as_str() {
                "20" => plan("draft-only"),
                _ => exit(5, ""),
            },
            ("page", "get") => match args[2].as_str() {
                "30" => plan("published"),
                "32" => plan("other-topic"),
                "33" => ok("## タスクリスト\n\n- [ ] **1-1**: タスク\n"),
                number => panic!("ページ {number} を読んだ"),
            },
            _ => panic!("想定しないwikinoの実行: {args:?}"),
        })
    }

    fn ready(number: u64, name: &str) -> State {
        State::Ready {
            url: format!("https://wikino.app/s/shimbaco/pages/{number}"),
            name: name.to_owned(),
        }
    }

    #[test]
    fn 下書きを先に探し_無ければ公開済みのページを探す() {
        let runner = wikino();
        let got = load_new(&runner).expect("ロードマップを読めなかった");
        let want = vec![
            Entry {
                candidate: link("やってる", None, "下書きだけ 作業計画書"),
                state: ready(20, "draft-only"),
            },
            Entry {
                candidate: link("優先度: 高", None, "公開済み 作業計画書"),
                state: ready(30, "published"),
            },
            Entry {
                candidate: link("優先度: 高", Some("Annict"), "別のトピック 作業計画書"),
                state: ready(32, "other-topic"),
            },
            Entry {
                candidate: link("優先度: 高", None, "まだ無い 作業計画書"),
                state: State::NotFound,
            },
            Entry {
                candidate: link("優先度: 高", None, "計画書名が無い 作業計画書"),
                state: State::Invalid {
                    url: "https://wikino.app/s/shimbaco/pages/33".to_owned(),
                    reason: "計画書 https://wikino.app/s/shimbaco/pages/33 に計画書名の行がありません。最初の「## 」の見出しより前に「**計画書名**: <英小文字・数字・ハイフンの名前>」の行を書いてください".to_owned(),
                },
            },
        ];
        assert_eq!(got, want);

        let calls = runner.calls.into_inner();
        let lists: Vec<_> = calls.iter().filter(|args| args[1] == "list").collect();
        assert_eq!(
            lists[0],
            &[
                "draft",
                "list",
                "--host=https://wikino.app",
                "--space=shimbaco",
                "--all",
                "--fields=page_number,title,topic_number,topic_name",
            ],
            "下書きの一覧の引数が期待と異なる"
        );
        assert_eq!(
            lists.iter().filter(|args| args[0] == "draft").count(),
            1,
            "下書きの一覧を作業計画書ごとに読んだ"
        );
        assert_eq!(
            flag(lists[1], "title"),
            Some("公開済み 作業計画書"),
            "ページの一覧をタイトルで絞り込んでいない"
        );
    }

    #[test]
    fn 作業計画書の候補が無ければ一覧を読まない() {
        let runner = FakeWikino::new(|args| match args[1].as_str() {
            "get" => ok(
                "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[ミュート機能]]\n\n## 優先度: 高\n",
            ),
            _ => panic!("想定しないwikinoの実行: {args:?}"),
        });
        assert_eq!(load_new(&runner).expect("ロードマップを読めなかった"), []);
    }

    #[test]
    fn ロードマップのトピックの番号が無ければエラー() {
        let runner = FakeWikino::new(|_| ok("## やってる\n"));
        let err = load_new(&runner).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NoTopic(_)), "エラー = {err:?}");
    }

    #[test]
    fn ロードマップを読めなければロードマップのエラー() {
        let runner = FakeWikino::new(|_| exit(5, ""));
        let err = load_new(&runner).expect_err("エラーにならなかった");
        assert_eq!(
            err.to_string(),
            "ロードマップ https://wikino.app/s/shimbaco/pages/1214 が見つかりません。URLのスペースとページ番号を確かめてください"
        );
    }

    #[test]
    fn 承認とみなす節が無ければエラー() {
        let runner = FakeWikino::new(|_| ok("---\nwikino_topic_number: 4\n---\n\n## やってる\n"));
        let err = load_new(&runner).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Queue(_)), "エラー = {err:?}");
    }

    #[test]
    fn 一覧を読めなければ理由と標準エラーを添える() {
        let runner = FakeWikino::new(|args| match args[1].as_str() {
            "get" => ok(ROADMAP),
            _ => exit(3, "ログインしてください"),
        });
        let err = load_new(&runner).expect_err("エラーにならなかった");
        let message = err.to_string();
        assert!(
            message.starts_with("作業計画書のページを探せません: ログインしていないか")
                && message.contains("--space=shimbaco")
                && message.ends_with("\nログインしてください"),
            "エラー = {message}"
        );
    }

    #[test]
    fn 一覧の出力を読み取れなければエラー() {
        let runner = FakeWikino::new(|args| match args[1].as_str() {
            "get" => ok(ROADMAP),
            _ => ok("{\"page_number\": \"20\"}\n"),
        });
        let err = load_new(&runner).expect_err("エラーにならなかった");
        assert!(
            matches!(
                err,
                Error::SearchOutput {
                    command: "draft list",
                    ..
                }
            ),
            "エラー = {err:?}"
        );
    }

    #[test]
    fn 一覧の後で消えた計画書も並びに残し_次の候補を読む() {
        for published in [false, true] {
            let runner = FakeWikino::new(move |args| match (args[0].as_str(), args[1].as_str()) {
                ("draft", "get") if args[2] == "1214" => ok(
                    "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[消えた 作業計画書]]\n- [[次の 作業計画書]]\n\n## 優先度: 高\n",
                ),
                ("draft", "list") => {
                    let mut items = vec![
                        r#"{"page_number":21,"title":"次の 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                    ];
                    if !published {
                        items.push(r#"{"page_number":20,"title":"消えた 作業計画書","topic_number":4,"topic_name":"Mewst"}"#);
                    }
                    ok(&items.join("\n"))
                }
                ("page", "list") => {
                    assert_eq!(
                        flag(args, "title"),
                        Some("消えた 作業計画書"),
                        "別の候補のページを探した"
                    );
                    ok(
                        r#"{"number":20,"title":"消えた 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                    )
                }
                ("draft" | "page", "get") if args[2] == "20" => exit(5, ""),
                ("draft", "get") if args[2] == "21" => plan("next"),
                _ => panic!("想定しないwikinoの実行: {args:?}"),
            });
            let entries = load_new(&runner).expect("消えた計画書で並び全体をエラーにした");
            assert_eq!(entries.len(), 2, "消えた候補か次の候補を落とした");
            assert_eq!(
                entries[0].candidate.link.title, "消えた 作業計画書",
                "候補の順が変わった"
            );
            assert_eq!(
                entries[0].state,
                State::NotFound,
                "本文取得時の404をNotFoundにしなかった"
            );
            assert_eq!(
                entries[1].state,
                ready(21, "next"),
                "次の候補を読まなかった"
            );
        }
    }

    #[test]
    fn トピック名の有無で書き分けた同じページは最初の候補だけを残す() {
        let runner = FakeWikino::new(|args| match (args[0].as_str(), args[1].as_str()) {
            ("draft", "get") if args[2] == "1214" => ok(
                "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[退会機能 作業計画書]]\n\n## 優先度: 高\n\n- [[Mewst/退会機能 作業計画書]]\n- [[次の 作業計画書]]\n",
            ),
            ("draft", "list") => ok(concat!(
                r#"{"page_number":20,"title":"退会機能 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                "\n",
                r#"{"page_number":21,"title":"次の 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                "\n"
            )),
            ("draft", "get") if args[2] == "20" => plan("withdraw"),
            ("draft", "get") if args[2] == "21" => plan("next"),
            _ => panic!("想定しないwikinoの実行: {args:?}"),
        });
        let entries = load_new(&runner).expect("ロードマップを読めなかった");
        assert_eq!(
            entries,
            [
                Entry {
                    candidate: link("やってる", None, "退会機能 作業計画書"),
                    state: ready(20, "withdraw"),
                },
                Entry {
                    candidate: link("優先度: 高", None, "次の 作業計画書"),
                    state: ready(21, "next"),
                },
            ],
            "同じページを2回並べた"
        );
        let reads = runner
            .calls
            .into_inner()
            .into_iter()
            .filter(|args| args[1] == "get" && args[2] == "20")
            .count();
        assert_eq!(reads, 1, "重複した候補の計画書も読んだ");
    }

    #[test]
    fn 計画書の本文の認証エラーや利用量の制限では全体を止める() {
        for code in [3, 8, 1] {
            let runner = FakeWikino::new(move |args| match (args[0].as_str(), args[1].as_str()) {
                ("draft", "get") if args[2] == "1214" => ok(
                    "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[対象 作業計画書]]\n\n## 優先度: 高\n",
                ),
                ("draft", "list") => ok(
                    r#"{"page_number":20,"title":"対象 作業計画書","topic_number":4,"topic_name":"Mewst"}"#,
                ),
                ("draft", "get") if args[2] == "20" => exit(code, "失敗の理由"),
                _ => panic!("失敗の後でwikinoを実行した: {args:?}"),
            });
            let err = load_new(&runner).expect_err("本文を読めないのに並びを返した");
            assert!(
                matches!(err, Error::Source(source::Error::Read { .. })),
                "元の取得エラーを失った: {err:?}"
            );
            assert!(err.to_string().contains("失敗の理由"), "標準エラーを失った");
        }
    }

    #[test]
    fn 完了を記録した計画書は承認とみなす節にあっても並びに入れない() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let store = Store::new(root.path());
        for (name, completed) in [("draft-only", true), ("published", false)] {
            let mut plan = PlanState::new(
                format!("https://wikino.app/s/shimbaco/pages/{name}"),
                format!("{name} 作業計画書"),
            );
            if completed {
                plan.completed_at = Some(
                    "2026-11-16T05:15:40Z"
                        .parse()
                        .expect("日時を読み取れなかった"),
                );
            }
            store
                .plan(name)
                .expect("状態の置き場を決められなかった")
                .lock()
                .expect("ロックを取れなかった")
                .save(&plan)
                .expect("状態を保存できなかった");
        }

        let entries = load(&wikino(), &config(), &store).expect("ロードマップを読めなかった");
        let titles: Vec<_> = entries
            .iter()
            .map(|entry| entry.candidate.link.title.as_str())
            .collect();
        assert_eq!(
            titles,
            [
                "公開済み 作業計画書",
                "別のトピック 作業計画書",
                "まだ無い 作業計画書",
                "計画書名が無い 作業計画書",
            ],
            "完了を記録した計画書を残したか、完了していない計画書を除いた"
        );
    }

    #[test]
    fn 状態ファイルを読めなければエラー() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let store = Store::new(root.path());
        let dir = store
            .plan("draft-only")
            .expect("状態の置き場を決められなかった");
        fs::create_dir_all(dir.path()).expect("ディレクトリを作れなかった");
        fs::write(dir.state_path(), "{").expect("状態ファイルを書けなかった");

        let err = load(&wikino(), &config(), &store).expect_err("エラーにならなかった");
        assert!(
            matches!(err, Error::State(state::Error::Parse { .. })),
            "エラー = {err:?}"
        );
    }
}
