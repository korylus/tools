//! korylusfactoryの設定ファイルの読み取り。
//!
//! 設定ファイルは、プロジェクトのルートの [`CONFIG_PATH`] に置くJSONである。
//!
//! ```json
//! {
//!   "roadmap": "https://wikino.app/s/<スペース>/pages/<ページ番号>",
//!   "sections": ["やってる", "優先度: 高", "優先度: 中"],
//!   "done_sections": ["リリース待ち", "やった"],
//!   "project": "mewst",
//!   "queue": {"space": "<スペース>", "topic": 12},
//!   "notify": {"command": ["<通知のコマンド>", "<引数>"]}
//! }
//! ```
//!
//! `sections` は承認とみなすロードマップの節で、省略すると [`DEFAULT_SECTIONS`] を使う。
//! `done_sections` は完了 (マージ済み) とみなす節で、省略すると [`DEFAULT_DONE_SECTIONS`] を使う。
//! `project` はプロジェクト名で、`queue` か `notify` を書くときは必ず書く。
//! `queue` は人間キューの置き場 (Wikinoのスペースとトピック) で、省略すると項目を積まない。
//! `notify` は人間待ちを知らせるコマンドで、省略すると知らせない。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

pub use korylusfactory_core::queue::DEFAULT_SECTIONS;

/// 完了とみなす節の初期値。どちらの節も、既定のベースにマージしたあとの状態を表す。
pub const DEFAULT_DONE_SECTIONS: [&str; 2] = ["リリース待ち", "やった"];
use serde::Deserialize;

use crate::human_queue::Location;
use crate::source::{self, Kind, Wikino};

/// プロジェクトのルートからの、設定ファイルの相対パス。
pub const CONFIG_PATH: &str = "docs/private/factory/config.json";

/// 設定ファイルを読み取れないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 設定ファイルが無い。
    #[error(
        "設定ファイル {} がありません。ロードマップのURLを「roadmap」に書いたJSONのファイルを作ってください",
        .0.display()
    )]
    NotFound(PathBuf),

    /// 設定ファイルを読めない。
    #[error("設定ファイル {} を読めません", .path.display())]
    Read {
        /// 設定ファイルのパス。
        path: PathBuf,
        /// 読めなかった理由。
        #[source]
        source: io::Error,
    },

    /// 設定ファイルがJSONとして読めないか、項目が誤っている。
    #[error("設定ファイル {} の書き方が誤っています", .path.display())]
    Parse {
        /// 設定ファイルのパス。
        path: PathBuf,
        /// 誤っている箇所。
        #[source]
        source: serde_json::Error,
    },

    /// ロードマップのURLを読み取れない。
    #[error("設定ファイル {} の「roadmap」を読み取れません", .path.display())]
    Roadmap {
        /// 設定ファイルのパス。
        path: PathBuf,
        /// 読み取れなかった理由。
        #[source]
        source: Box<source::Error>,
    },

    /// 承認とみなす節が空。進める計画書が常に無くなるため、書き誤りとして扱う。
    #[error("設定ファイル {} の「sections」が空です。承認とみなす節の見出しを1つ以上書いてください", .0.display())]
    NoSections(PathBuf),

    /// 完了とみなす節が空。作業計画書の完了を検知できなくなるため、書き誤りとして扱う。
    #[error("設定ファイル {} の「done_sections」が空です。完了とみなす節の見出しを1つ以上書いてください", .0.display())]
    NoDoneSections(PathBuf),

    /// 承認とみなす節と完了とみなす節に、同じ節がある。
    #[error(
        "設定ファイル {} の「sections」と「done_sections」に同じ節 {} があります。承認とみなす節と完了とみなす節は分けてください",
        .path.display(),
        .sections.iter().map(|s| format!("「{s}」")).collect::<Vec<_>>().join("・")
    )]
    OverlappingSections {
        /// 設定ファイルのパス。
        path: PathBuf,
        /// 両方にある節。
        sections: Vec<String>,
    },

    /// 人間キューか通知のコマンドを書いたのに、プロジェクト名が無い。
    #[error(
        "設定ファイル {} に「project」がありません。「queue」か「notify」を書くときは、プロジェクト名を「project」に書いてください",
        .0.display()
    )]
    NoProject(PathBuf),

    /// プロジェクト名に使えない文字がある。
    #[error(
        "設定ファイル {} の「project」 {name:?} を使えません。英数字・ハイフン・アンダースコアで書いてください",
        .path.display()
    )]
    InvalidProject {
        /// 設定ファイルのパス。
        path: PathBuf,
        /// 書かれたプロジェクト名。
        name: String,
    },

    /// 人間キューの置き場のスペースかトピックが誤っている。
    #[error(
        "設定ファイル {} の「queue」を使えません。「space」にスペースの識別子を、「topic」に1以上のトピックの番号を書いてください",
        .0.display()
    )]
    InvalidQueue(PathBuf),

    /// 通知のコマンドが空。
    #[error(
        "設定ファイル {} の「notify」の「command」が空です。実行するコマンドと引数を並べてください",
        .0.display()
    )]
    NoNotifyCommand(PathBuf),
}

/// 設定ファイルのJSONの形。
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct File {
    roadmap: String,
    sections: Option<Vec<String>>,
    done_sections: Option<Vec<String>>,
    project: Option<String>,
    queue: Option<QueueFile>,
    notify: Option<NotifyFile>,
}

/// 設定ファイルの「queue」の形。
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct QueueFile {
    space: String,
    topic: u64,
}

/// 設定ファイルの「notify」の形。
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NotifyFile {
    command: Vec<String>,
}

/// korylusfactoryの設定。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Config {
    /// ロードマップのページ。
    pub roadmap: Wikino,
    /// 承認とみなすロードマップの節の見出し。上から順に進める。
    pub sections: Vec<String>,
    /// 完了 (マージ済み) とみなすロードマップの節の見出し。
    pub done_sections: Vec<String>,
    /// プロジェクト名と、人間待ちの置き場・知らせ方。「project」を書かなければ `None`。
    pub project: Option<Project>,
}

/// プロジェクト名と、そのプロジェクトの人間待ちの置き場・知らせ方。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Project {
    /// プロジェクト名。人間キューの項目のタイトルと「状況」に書き、通知のコマンドに渡す。
    pub name: String,
    /// 人間キューの置き場。`None` なら項目を積まず、`korylusfactory resume` だけで再開する。
    pub queue: Option<Location>,
    /// 人間待ちを知らせるコマンド。`None` なら知らせない。
    pub notify: Option<Notify>,
}

/// 人間待ちを知らせるコマンド。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Notify {
    /// 実行するコマンド。
    pub program: String,
    /// コマンドの引数。
    pub args: Vec<String>,
}

/// プロジェクトのルートrootの設定ファイルを読み取る。
pub fn load(root: &Path) -> Result<Config, Error> {
    let path = root.join(CONFIG_PATH);
    let src = fs::read_to_string(&path).map_err(|source| match source.kind() {
        io::ErrorKind::NotFound => Error::NotFound(path.clone()),
        _ => Error::Read {
            path: path.clone(),
            source,
        },
    })?;
    let file: File = serde_json::from_str(&src).map_err(|source| Error::Parse {
        path: path.clone(),
        source,
    })?;
    let roadmap =
        Wikino::parse_as(Kind::Roadmap, &file.roadmap).map_err(|source| Error::Roadmap {
            path: path.clone(),
            source: Box::new(source),
        })?;
    let sections = match file.sections {
        Some(sections) if sections.is_empty() => return Err(Error::NoSections(path)),
        Some(sections) => sections,
        None => DEFAULT_SECTIONS.map(str::to_owned).to_vec(),
    };
    let done_sections = match file.done_sections {
        Some(done) if done.is_empty() => return Err(Error::NoDoneSections(path)),
        Some(done) => done,
        None => DEFAULT_DONE_SECTIONS.map(str::to_owned).to_vec(),
    };
    let overlapping: Vec<String> = done_sections
        .iter()
        .filter(|name| sections.contains(name))
        .cloned()
        .collect();
    if !overlapping.is_empty() {
        return Err(Error::OverlappingSections {
            path,
            sections: overlapping,
        });
    }
    let project = read_project(path, file.project, file.queue, file.notify)?;
    Ok(Config {
        roadmap,
        sections,
        done_sections,
        project,
    })
}

/// 設定ファイルpathの「project」「queue」「notify」を読み取る。
fn read_project(
    path: PathBuf,
    name: Option<String>,
    queue: Option<QueueFile>,
    notify: Option<NotifyFile>,
) -> Result<Option<Project>, Error> {
    let Some(name) = name else {
        if queue.is_some() || notify.is_some() {
            return Err(Error::NoProject(path));
        }
        return Ok(None);
    };
    // プロジェクト名は項目のタイトルの先頭に「<プロジェクト名> - 」と書き、このプロジェクトの項目を見分けるのに使う。
    // 区切りやタイトルに使えない文字を含めないよう、使える文字を絞る。
    let valid_name = !name.is_empty()
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_');
    if !valid_name {
        return Err(Error::InvalidProject { path, name });
    }
    let queue = match queue {
        Some(QueueFile { space, topic }) if source::is_space(&space) && topic > 0 => {
            Some(Location { space, topic })
        }
        Some(_) => return Err(Error::InvalidQueue(path)),
        None => None,
    };
    let notify = match notify.map(|notify| notify.command) {
        Some(command) => {
            let mut command = command.into_iter();
            match command.next() {
                Some(program) if !program.is_empty() => Some(Notify {
                    program,
                    args: command.collect(),
                }),
                _ => return Err(Error::NoNotifyCommand(path)),
            }
        }
        None => None,
    };
    Ok(Some(Project {
        name,
        queue,
        notify,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::error::Error as _;

    /// 設定ファイルにcontentを書いたプロジェクトのルートを作る。
    fn project(content: &str) -> tempfile::TempDir {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = root.path().join(CONFIG_PATH);
        fs::create_dir_all(path.parent().expect("親ディレクトリが無い"))
            .expect("ディレクトリを作れなかった");
        fs::write(path, content).expect("設定ファイルを書けなかった");
        root
    }

    #[test]
    fn 節を省略すると初期値の節を使う() {
        let root = project(r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214"}"#);
        let config = load(root.path()).expect("設定を読めなかった");
        assert_eq!(
            config,
            Config {
                roadmap: Wikino {
                    kind: Kind::Roadmap,
                    url: "https://wikino.app/s/shimbaco/pages/1214".to_owned(),
                    space: "shimbaco".to_owned(),
                    number: 1214,
                },
                sections: vec![
                    "やってる".to_owned(),
                    "優先度: 高".to_owned(),
                    "優先度: 中".to_owned(),
                ],
                done_sections: vec!["リリース待ち".to_owned(), "やった".to_owned()],
                project: None,
            }
        );
    }

    #[test]
    fn プロジェクト名と人間キューの置き場と通知のコマンドを読む() {
        let root = project(
            r#"{
              "roadmap": "https://wikino.app/s/shimbaco/pages/1214",
              "project": "mewst",
              "queue": {"space": "shimbaco", "topic": 12},
              "notify": {"command": ["ntfy", "publish", "factory"]}
            }"#,
        );
        let config = load(root.path()).expect("設定を読めなかった");
        assert_eq!(
            config.project,
            Some(Project {
                name: "mewst".to_owned(),
                queue: Some(Location {
                    space: "shimbaco".to_owned(),
                    topic: 12,
                }),
                notify: Some(Notify {
                    program: "ntfy".to_owned(),
                    args: vec!["publish".to_owned(), "factory".to_owned()],
                }),
            })
        );
    }

    #[test]
    fn 人間キューと通知のコマンドは片方だけでも使える() {
        let root = project(
            r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "project": "mewst", "notify": {"command": ["notify-send"]}}"#,
        );
        let got = load(root.path())
            .expect("設定を読めなかった")
            .project
            .expect("プロジェクトが無い");
        assert_eq!(got.queue, None);
        assert_eq!(
            got.notify,
            Some(Notify {
                program: "notify-send".to_owned(),
                args: Vec::new(),
            })
        );

        let root = project(
            r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "project": "mewst", "queue": {"space": "shimbaco", "topic": 12}}"#,
        );
        let got = load(root.path())
            .expect("設定を読めなかった")
            .project
            .expect("プロジェクトが無い");
        assert!(got.queue.is_some(), "置き場を読まなかった");
        assert_eq!(got.notify, None);
    }

    #[test]
    fn 人間待ちの設定の誤りはエラー() {
        let cases = [
            (
                "queueだけでprojectが無い",
                r#""queue": {"space": "shimbaco", "topic": 12}"#,
                "に「project」がありません",
            ),
            (
                "notifyだけでprojectが無い",
                r#""notify": {"command": ["notify-send"]}"#,
                "に「project」がありません",
            ),
            (
                "プロジェクト名が空",
                r#""project": """#,
                "の「project」 \"\" を使えません",
            ),
            (
                "プロジェクト名に区切りの文字がある",
                r#""project": "mewst - web""#,
                "の「project」 \"mewst - web\" を使えません",
            ),
            (
                "スペースがハイフンで始まる",
                r#""project": "mewst", "queue": {"space": "-x", "topic": 12}"#,
                "の「queue」を使えません",
            ),
            (
                "トピックが0",
                r#""project": "mewst", "queue": {"space": "shimbaco", "topic": 0}"#,
                "の「queue」を使えません",
            ),
            (
                "通知のコマンドが空",
                r#""project": "mewst", "notify": {"command": []}"#,
                "の「notify」の「command」が空です",
            ),
            (
                "通知のコマンドの名前が空",
                r#""project": "mewst", "notify": {"command": ["", "x"]}"#,
                "の「notify」の「command」が空です",
            ),
        ];
        for (name, fields, want) in cases {
            let root = project(&format!(
                r#"{{"roadmap": "https://wikino.app/s/shimbaco/pages/1", {fields}}}"#
            ));
            let err = load(root.path()).expect_err(&format!("{name}: エラーにならなかった"));
            assert!(
                err.to_string().contains(want),
                "{name}: エラー = {err}、期待値 = {want:?} を含むエラー"
            );
        }
    }

    #[test]
    fn 書いた完了とみなす節を使う() {
        let root = project(
            r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214", "done_sections": ["マージ済み"]}"#,
        );
        let config = load(root.path()).expect("設定を読めなかった");
        assert_eq!(config.done_sections, ["マージ済み"]);
    }

    #[test]
    fn 完了とみなす節が空ならエラー() {
        let root =
            project(r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "done_sections": []}"#);
        let err = load(root.path()).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NoDoneSections(_)), "エラー = {err:?}");
    }

    #[test]
    fn 承認とみなす節と完了とみなす節が重なればエラー() {
        let root = project(
            r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "sections": ["やってる", "やった"]}"#,
        );
        let err = load(root.path()).expect_err("エラーにならなかった");
        assert!(
            matches!(&err, Error::OverlappingSections { sections, .. } if sections == &["やった"]),
            "エラー = {err:?}"
        );
        assert!(
            err.to_string().ends_with(
                "に同じ節 「やった」 があります。承認とみなす節と完了とみなす節は分けてください"
            ),
            "エラー = {err}"
        );
    }

    #[test]
    fn 書いた節をその順に使う() {
        let root = project(
            r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214", "sections": ["優先度: 高", "やってる"]}"#,
        );
        let config = load(root.path()).expect("設定を読めなかった");
        assert_eq!(config.sections, ["優先度: 高", "やってる"]);
    }

    #[test]
    fn 設定ファイルが無ければ作り方を示す() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let err = load(root.path()).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NotFound(_)), "エラー = {err:?}");
        assert!(
            err.to_string()
                .contains("「roadmap」に書いたJSONのファイルを作ってください"),
            "エラー = {err}"
        );
    }

    #[test]
    fn 書き方の誤りはエラー() {
        let cases = [
            ("JSONではない", "roadmap = 1"),
            ("roadmapが無い", r#"{"sections": ["やってる"]}"#),
            (
                "知らない項目がある",
                r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "section": []}"#,
            ),
            (
                "queueに知らない項目がある",
                r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "project": "mewst", "queue": {"space": "shimbaco", "topic": 12, "title": "x"}}"#,
            ),
            (
                "queueのtopicが数ではない",
                r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "project": "mewst", "queue": {"space": "shimbaco", "topic": "12"}}"#,
            ),
            (
                "notifyのcommandが文字列",
                r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "project": "mewst", "notify": {"command": "notify-send"}}"#,
            ),
        ];
        for (name, content) in cases {
            let root = project(content);
            let err = load(root.path()).expect_err(&format!("{name}: エラーにならなかった"));
            assert!(
                matches!(err, Error::Parse { .. }),
                "{name}: エラー = {err:?}"
            );
        }
    }

    #[test]
    fn ロードマップのurlを読み取れなければその理由を添える() {
        let root = project(r#"{"roadmap": "https://example.com/s/shimbaco/pages/1"}"#);
        let err = load(root.path()).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Roadmap { .. }), "エラー = {err:?}");
        let source = err.source().expect("理由が無い").to_string();
        assert!(
            source.starts_with(
                "ロードマップの指定 https://example.com/s/shimbaco/pages/1 を読み取れません"
            ),
            "理由 = {source}"
        );
    }

    #[test]
    fn 節が空ならエラー() {
        let root =
            project(r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1", "sections": []}"#);
        let err = load(root.path()).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NoSections(_)), "エラー = {err:?}");
    }
}
