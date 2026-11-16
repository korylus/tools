//! korylusfactoryの設定ファイルの読み取り。
//!
//! 設定ファイルは、プロジェクトのルートの [`CONFIG_PATH`] に置くJSONである。
//!
//! ```json
//! {
//!   "roadmap": "https://wikino.app/s/<スペース>/pages/<ページ番号>",
//!   "sections": ["やってる", "優先度: 高", "優先度: 中"],
//!   "done_sections": ["リリース待ち", "やった"]
//! }
//! ```
//!
//! `sections` は承認とみなすロードマップの節で、省略すると [`DEFAULT_SECTIONS`] を使う。
//! `done_sections` は完了 (マージ済み) とみなす節で、省略すると [`DEFAULT_DONE_SECTIONS`] を使う。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

pub use korylusfactory_core::queue::DEFAULT_SECTIONS;

/// 完了とみなす節の初期値。どちらの節も、既定のベースにマージしたあとの状態を表す。
pub const DEFAULT_DONE_SECTIONS: [&str; 2] = ["リリース待ち", "やった"];
use serde::Deserialize;

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
}

/// 設定ファイルのJSONの形。
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct File {
    roadmap: String,
    sections: Option<Vec<String>>,
    done_sections: Option<Vec<String>>,
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
    Ok(Config {
        roadmap,
        sections,
        done_sections,
    })
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
            }
        );
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
