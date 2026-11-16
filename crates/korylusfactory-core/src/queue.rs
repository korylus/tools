//! ロードマップから、進める作業計画書の並びを決める。

use std::collections::HashSet;

use korylus_workdoc::roadmap;
pub use korylus_workdoc::roadmap::WikiLink;
use korylus_workdoc::{Error as WorkdocError, plan};

/// 承認とみなす節の初期値。上から順に進める。
pub const DEFAULT_SECTIONS: [&str; 3] = ["やってる", "優先度: 高", "優先度: 中"];

/// 進める作業計画書を決められないときのエラー。
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum Error {
    /// 承認とみなす節が、ロードマップに無い。節の名前の書き誤りで計画書を読み落とさないよう、エラーにする。
    #[error("ロードマップに、承認とみなす節 {} がありません", .0.iter().map(|s| format!("「{s}」")).collect::<Vec<_>>().join("・"))]
    MissingSections(Vec<String>),

    /// 完了とみなす節が、ロードマップに無い。節の名前の書き誤りで完了を見落とさないよう、エラーにする。
    #[error("ロードマップに、完了とみなす節 {} がありません", .0.iter().map(|s| format!("「{s}」")).collect::<Vec<_>>().join("・"))]
    MissingDoneSections(Vec<String>),
}

/// これから進める作業計画書の候補。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    /// 候補を書いたロードマップの節の見出し。
    pub section: String,
    /// 作業計画書へのWikiリンク。
    pub link: WikiLink,
}

/// これから進める作業計画書。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    /// 作業計画書へのリンクを書いたロードマップの節と、リンク。
    pub candidate: Candidate,
    /// 作業計画書のページの状態。
    pub state: State,
}

/// 作業計画書のページの状態。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum State {
    /// ページが見つかり、計画書名を読み取れた。
    Ready {
        /// 作業計画書のURL。
        url: String,
        /// 計画書名。
        name: String,
    },
    /// リンクのタイトルの下書きもページも無い。
    NotFound,
    /// ページは見つかったが、作業計画書として読み取れない。
    Invalid {
        /// 作業計画書のURL。
        url: String,
        /// 読み取れない理由。
        reason: String,
    },
}

/// 作業計画書の並びの先頭から決めた、次に進める作業計画書。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Head<'a> {
    /// 進める作業計画書が無い。
    Empty,
    /// 先頭の作業計画書を進める。
    Start {
        /// 作業計画書のURL。
        url: &'a str,
        /// 計画書名。
        name: &'a str,
    },
    /// 先頭の作業計画書を読み取れない。人間が作業計画書かロードマップを直すまで待つ。
    Blocked(&'a Entry),
}

/// 進める順に並べた作業計画書entriesの先頭から、次に進める作業計画書を決める。
///
/// 先頭を読み取れないときは、飛ばして次の作業計画書へ進まない。
/// 進める順番は人間がロードマップで決めたもので、飛ばすと順番が黙って変わるためである。
pub fn head(entries: &[Entry]) -> Head<'_> {
    match entries.first() {
        None => Head::Empty,
        Some(Entry {
            state: State::Ready { url, name },
            ..
        }) => Head::Start { url, name },
        Some(entry) => Head::Blocked(entry),
    }
}

/// ロードマップの本文srcの、承認とみなす節sectionsの項目から、作業計画書の候補を進める順に返す。
///
/// 節はsectionsの順に、節の中では上から順に並べる。
/// 項目のWikiリンクのうち、タイトルが「作業計画書」で終わるものだけを候補にし、
/// 同じリンクが複数の項目にあれば最初のものだけを残す。
pub fn candidates(src: &str, sections: &[String]) -> Result<Vec<Candidate>, Error> {
    let roadmap = roadmap::parse(src);
    let missing = missing(&roadmap, sections);
    if !missing.is_empty() {
        return Err(Error::MissingSections(missing));
    }

    let mut seen = HashSet::new();
    let mut found = Vec::new();
    for name in sections {
        let links = roadmap
            .sections
            .iter()
            .filter(|section| &section.title == name)
            .flat_map(|section| &section.items)
            .flat_map(|item| &item.links)
            .filter(|link| link.is_plan());
        for link in links {
            if seen.insert(link.clone()) {
                found.push(Candidate {
                    section: name.clone(),
                    link: link.clone(),
                });
            }
        }
    }
    Ok(found)
}

/// ロードマップの本文srcの完了とみなす節sectionsの項目から、タイトルtitleの作業計画書へのWikiリンクを返す。
///
/// トピック名の省略を含めてリンクを保ち、呼び出し側がページに解決して同じ作業計画書かを確かめる。
/// 節がロードマップに無ければ、節の名前の書き誤りで完了を見落とさないよう、エラーにする。
pub fn completion_links(
    src: &str,
    sections: &[String],
    title: &str,
) -> Result<Vec<WikiLink>, Error> {
    let roadmap = roadmap::parse(src);
    let missing = missing(&roadmap, sections);
    if !missing.is_empty() {
        return Err(Error::MissingDoneSections(missing));
    }
    Ok(roadmap
        .sections
        .iter()
        .filter(|section| sections.contains(&section.title))
        .flat_map(|section| &section.items)
        .flat_map(|item| &item.links)
        .filter(|link| link.title == title)
        .cloned()
        .collect())
}

/// 節sectionsのうち、ロードマップroadmapに無いものを返す。
fn missing(roadmap: &roadmap::Roadmap, sections: &[String]) -> Vec<String> {
    sections
        .iter()
        .filter(|name| !roadmap.sections.iter().any(|s| &s.title == *name))
        .cloned()
        .collect()
}

/// 作業計画書の本文srcを読み取り、計画書名を返す。計画書名の行が無ければ空文字を返す。
pub fn plan_name(src: &str) -> Result<String, WorkdocError> {
    plan::parse(src).map(|plan| plan.name)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sections(names: &[&str]) -> Vec<String> {
        names.iter().map(|&name| name.to_owned()).collect()
    }

    fn candidate(section: &str, topic: Option<&str>, title: &str) -> Candidate {
        Candidate {
            section: section.to_owned(),
            link: WikiLink {
                topic: topic.map(str::to_owned),
                title: title.to_owned(),
            },
        }
    }

    const ROADMAP: &str = "\
## 未振り分け

- [[未振り分け 作業計画書]]

## やってる

- [[退会機能 作業計画書]]

## 優先度: 高

- [[ミュート機能]]
- 名前だけの 作業計画書
- [[Annict/言語方針 作業計画書]], [[退会機能 作業計画書]]

## 優先度: 中

- [[パスワード 作業計画書]]

## やった

- [[やった 作業計画書]]
";

    #[test]
    fn 承認とみなす節の作業計画書を節の順に上から並べる() {
        let got =
            candidates(ROADMAP, &sections(&DEFAULT_SECTIONS)).expect("候補を決められなかった");
        assert_eq!(
            got,
            [
                candidate("やってる", None, "退会機能 作業計画書"),
                candidate("優先度: 高", Some("Annict"), "言語方針 作業計画書"),
                candidate("優先度: 中", None, "パスワード 作業計画書"),
            ]
        );
    }

    #[test]
    fn 節は指定した順に並べる() {
        let got = candidates(ROADMAP, &sections(&["優先度: 中", "やってる"]))
            .expect("候補を決められなかった");
        assert_eq!(
            got,
            [
                candidate("優先度: 中", None, "パスワード 作業計画書"),
                candidate("やってる", None, "退会機能 作業計画書"),
            ]
        );
    }

    #[test]
    fn 承認とみなす節がロードマップに無ければエラー() {
        let err = candidates(ROADMAP, &sections(&["やってる", "優先度：高", "保留"]))
            .expect_err("エラーにならなかった");
        assert_eq!(
            err,
            Error::MissingSections(sections(&["優先度：高", "保留"]))
        );
        assert_eq!(
            err.to_string(),
            "ロードマップに、承認とみなす節 「優先度：高」・「保留」 がありません"
        );
    }

    #[test]
    fn 計画書名の行の値を返す() {
        let src = "**計画書名**: demo\n\n## タスクリスト\n\n- [ ] **1-1**: タスク\n";
        assert_eq!(plan_name(src).expect("読み取れなかった"), "demo");
    }

    #[test]
    fn 計画書として読めなければ読み取りのエラーを返す() {
        let err = plan_name("## タスクリスト\n").expect_err("エラーにならなかった");
        assert_eq!(err, WorkdocError::NoTasks);
    }

    fn entry(title: &str, state: State) -> Entry {
        Entry {
            candidate: candidate("やってる", None, title),
            state,
        }
    }

    fn ready(name: &str) -> State {
        State::Ready {
            url: format!("https://wikino.app/s/shimbaco/pages/{name}"),
            name: name.to_owned(),
        }
    }

    #[test]
    fn 先頭の作業計画書を読み取れれば先頭を進める() {
        let entries = [
            entry("先頭 作業計画書", ready("first")),
            entry("次 作業計画書", State::NotFound),
        ];
        assert_eq!(
            head(&entries),
            Head::Start {
                url: "https://wikino.app/s/shimbaco/pages/first",
                name: "first",
            }
        );
    }

    #[test]
    fn 先頭の作業計画書を読み取れなければ次を進めずに待つ() {
        let cases = [
            ("ページが無い", State::NotFound),
            (
                "作業計画書として読み取れない",
                State::Invalid {
                    url: "https://wikino.app/s/shimbaco/pages/1".to_owned(),
                    reason: "タスクリストがありません".to_owned(),
                },
            ),
        ];
        for (name, state) in cases {
            let entries = [
                entry("先頭 作業計画書", state),
                entry("次 作業計画書", ready("next")),
            ];
            assert_eq!(
                head(&entries),
                Head::Blocked(&entries[0]),
                "{name}: 先頭を飛ばした"
            );
        }
    }

    #[test]
    fn 作業計画書が無ければ進めるものが無い() {
        assert_eq!(head(&[]), Head::Empty);
    }

    const DONE_ROADMAP: &str = "\
## やってる

- [[進行中 作業計画書]]

## リリース待ち

- [[Mewst/トピック付き 作業計画書]]

## やった

- [[完了 作業計画書]]
";

    #[test]
    fn 完了とみなす節の同じタイトルのリンクをトピックの表記を保って返す() {
        let done = sections(&["リリース待ち", "やった"]);
        let cases = [
            ("完了とみなす節にある", "完了 作業計画書", vec![None]),
            ("承認とみなす節にだけある", "進行中 作業計画書", vec![]),
            (
                "ロードマップのリンクがトピック名を書いている",
                "トピック付き 作業計画書",
                vec![Some("Mewst")],
            ),
        ];
        for (name, title, topics) in cases {
            let want: Vec<WikiLink> = topics
                .into_iter()
                .map(|topic| WikiLink {
                    topic: topic.map(str::to_owned),
                    title: title.to_owned(),
                })
                .collect();
            assert_eq!(
                completion_links(DONE_ROADMAP, &done, title).expect("節を読めなかった"),
                want,
                "{name}"
            );
        }
    }

    #[test]
    fn 完了とみなす節がロードマップに無ければエラー() {
        let err = completion_links(
            DONE_ROADMAP,
            &sections(&["リリース待ち", "完了"]),
            "完了 作業計画書",
        )
        .expect_err("エラーにならなかった");
        assert_eq!(err, Error::MissingDoneSections(sections(&["完了"])));
        assert_eq!(
            err.to_string(),
            "ロードマップに、完了とみなす節 「完了」 がありません"
        );
    }

    #[test]
    fn 入れ子の見出しで承認する節を作らない() {
        for src in [
            "## 未振り分け\n\n- 説明\n  ## やってる\n\n  [[未承認 作業計画書]]\n",
            "> ## やってる\n>\n> - [[未承認 作業計画書]]\n",
        ] {
            assert_eq!(
                candidates(src, &sections(&["やってる"])),
                Err(Error::MissingSections(sections(&["やってる"]))),
                "入れ子の見出しを承認とみなした"
            );
        }
    }
}
