//! ロードマップの読み取り。
//!
//! ロードマップは「## 」の見出しの節に分かれ、各節のトップレベルのリストの項目に、
//! 作業計画書やチケットへのWikiリンクを並べたページである。
//! 読み取るのは節の見出しと、トップレベルの項目のWikiリンクに限る。
//! 入れ子の項目と、コード (インラインのコードとコードブロック) の中は読み飛ばす。

mod wikilink;

use std::ops::Range;

use pulldown_cmark::{Event, HeadingLevel, Parser, Tag, TagEnd};

use crate::markdown::parse_source;

/// 作業計画書のページのタイトルの末尾。
const PLAN_SUFFIX: &str = "作業計画書";

/// 読み取ったロードマップ。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Roadmap {
    /// 本文に書かれた順の節。最初の「## 」の見出しより前の項目は、どの節にも入れない。
    pub sections: Vec<Section>,
}

/// ロードマップの1つの節。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Section {
    /// 見出しの文字列 (「## 」を除き、前後の空白を除いたもの)。
    pub title: String,
    /// 本文に書かれた順の、トップレベルの項目。
    pub items: Vec<Item>,
}

/// 節のトップレベルの項目。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Item {
    /// 項目の始まる行の番号 (1始まり)。
    pub line_no: usize,
    /// 項目の本文に書かれた順のWikiリンク。名前だけの項目では空。
    pub links: Vec<WikiLink>,
}

/// Wikiリンク (`[[タイトル]]` か `[[トピック名/タイトル]]`)。
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct WikiLink {
    /// 別のトピックのページを指すときのトピック名。同じトピックのページを指すときは `None`。
    pub topic: Option<String>,
    /// ページのタイトル。
    pub title: String,
}

impl WikiLink {
    /// 作業計画書のページへのリンクかどうかを、タイトルの末尾で返す。
    pub fn is_plan(&self) -> bool {
        self.title.ends_with(PLAN_SUFFIX)
    }

    /// `[[` と `]]` の間の文字列から、Wikiリンクを読み取る。
    ///
    /// Wikinoと同じく、最初の `/` より前をトピック名として読む。
    /// トピック名かタイトルが空なら、リンクとして読まない。
    fn parse(inner: &str) -> Option<Self> {
        let (topic, title) = match inner.trim().split_once('/') {
            Some((topic, title)) => (Some(topic), title),
            None => (None, inner.trim()),
        };
        if title.is_empty() || topic.is_some_and(str::is_empty) {
            return None;
        }
        Some(Self {
            topic: topic.map(str::to_owned),
            title: title.to_owned(),
        })
    }
}

/// 読んでいる途中のトップレベルの項目。
struct OpenItem {
    line_no: usize,
    span: Range<usize>,
}

/// `src` をロードマップとして読み取る。
pub fn parse(src: &str) -> Roadmap {
    let src = parse_source(&src.replace("\r\n", "\n"));
    let line_no = |offset: usize| src[..offset].matches('\n').count() + 1;

    let mut roadmap = Roadmap::default();
    let mut heading: Option<String> = None;
    let mut item: Option<OpenItem> = None;
    let mut stack = Vec::new();
    let mut excluded = Vec::new();
    let mut slots = Vec::new();

    for (event, range) in Parser::new(&src).into_offset_iter() {
        match event {
            Event::Start(tag) => {
                match &tag {
                    Tag::Heading {
                        level: HeadingLevel::H2,
                        ..
                    } if stack.is_empty() => {
                        heading = Some(String::new());
                    }
                    Tag::Item if stack.len() == 1 && matches!(stack[0], TagEnd::List(_)) => {
                        item = Some(OpenItem {
                            line_no: line_no(range.start),
                            span: range.clone(),
                        });
                    }
                    // 入れ子のリストは、親の項目のソース範囲に含まれるため別に除く。
                    Tag::List(_) if !stack.is_empty() => excluded.push(range.clone()),
                    _ => {}
                }
                stack.push(tag.to_end());
            }
            Event::End(end) => {
                if end == TagEnd::Heading(HeadingLevel::H2)
                    && stack.len() == 1
                    && let Some(title) = heading.take()
                {
                    roadmap.sections.push(Section {
                        title: title.trim().to_owned(),
                        items: Vec::new(),
                    });
                }
                if end == TagEnd::Item
                    && stack.len() == 2
                    && let (Some(open), Some(section)) = (item.take(), roadmap.sections.last_mut())
                {
                    let index = section.items.len();
                    section.items.push(Item {
                        line_no: open.line_no,
                        links: Vec::new(),
                    });
                    slots.push((roadmap.sections.len() - 1, index, open.span));
                }
                stack.pop();
            }
            Event::Text(text) | Event::Code(text) => {
                if let Some(title) = heading.as_mut() {
                    title.push_str(&text);
                }
            }
            Event::SoftBreak | Event::HardBreak => {
                if let Some(title) = heading.as_mut() {
                    title.push(' ');
                }
            }
            _ => {}
        }
    }
    let spans: Vec<_> = slots.iter().map(|(_, _, span)| span.clone()).collect();
    for (offset, link) in wikilink::scan(&src, &spans, &excluded) {
        if let Some((section, index, _)) = slots.iter().find(|(_, _, span)| span.contains(&offset))
        {
            roadmap.sections[*section].items[*index].links.push(link);
        }
    }
    roadmap
}

#[cfg(test)]
fn links(text: &str) -> Vec<WikiLink> {
    wikilink::scan(text, std::slice::from_ref(&(0..text.len())), &[])
        .into_iter()
        .map(|(_, link)| link)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn link(topic: Option<&str>, title: &str) -> WikiLink {
        WikiLink {
            topic: topic.map(str::to_owned),
            title: title.to_owned(),
        }
    }

    #[test]
    fn 節ごとにトップレベルの項目のリンクを読み取る() {
        let src = "\
## やってる

- [[退会機能 作業計画書]]

## 優先度: 高

- [[ミュート機能]]
- 画像を添付できるようにする
- [[Annict/言語方針 作業計画書]], [[設定画面 (GET |settings) 作業計画書]]
";
        let got = parse(src);
        assert_eq!(
            got,
            Roadmap {
                sections: vec![
                    Section {
                        title: "やってる".to_owned(),
                        items: vec![Item {
                            line_no: 3,
                            links: vec![link(None, "退会機能 作業計画書")],
                        }],
                    },
                    Section {
                        title: "優先度: 高".to_owned(),
                        items: vec![
                            Item {
                                line_no: 7,
                                links: vec![link(None, "ミュート機能")],
                            },
                            Item {
                                line_no: 8,
                                links: vec![],
                            },
                            Item {
                                line_no: 9,
                                links: vec![
                                    link(Some("Annict"), "言語方針 作業計画書"),
                                    link(None, "設定画面 (GET |settings) 作業計画書"),
                                ],
                            },
                        ],
                    },
                ],
            }
        );
    }

    #[test]
    fn 入れ子の項目とコードの中は読み飛ばす() {
        let src = "\
## やってる

- [[A 作業計画書]]
  - [[入れ子 作業計画書]]
- `[[コード 作業計画書]]` と [[B 作業計画書]]
- 説明

  ```
  [[コードブロック 作業計画書]]
  ```
";
        let got = parse(src);
        let links: Vec<_> = got.sections[0]
            .items
            .iter()
            .flat_map(|item| item.links.clone())
            .collect();
        assert_eq!(
            links,
            [link(None, "A 作業計画書"), link(None, "B 作業計画書")]
        );
    }

    #[test]
    fn 最初の節より前の項目と見出しの階層の違う節は読まない() {
        let src = "\
- [[前 作業計画書]]

# タイトル

## やってる

### 小見出し

- [[A 作業計画書]]
";
        let got = parse(src);
        assert_eq!(got.sections.len(), 1, "節の数が期待と異なる");
        assert_eq!(got.sections[0].title, "やってる");
        assert_eq!(got.sections[0].items[0].links, [link(None, "A 作業計画書")]);
    }

    #[test]
    fn 空のトピック名やタイトルはリンクとして読まない() {
        assert_eq!(links("[[/A]] [[B/]] [[ ]] [[C]]"), [link(None, "C")]);
    }

    #[test]
    fn 改行をまたぐ括弧はリンクとして読まない() {
        let got = parse("## やってる\n\n- [[A\n  作業計画書]]\n- \\[\\[B 作業計画書]]\n");
        assert_eq!(got.sections[0].items[0].links, []);
        assert_eq!(
            got.sections[0].items[1].links,
            [],
            "エスケープした括弧をリンクとして読んだ"
        );
    }

    #[test]
    fn 復帰と改行で区切った本文も読み取る() {
        let got = parse("## やってる\r\n\r\n- [[A 作業計画書]]\r\n");
        assert_eq!(got.sections[0].items[0].line_no, 3);
        assert_eq!(got.sections[0].items[0].links, [link(None, "A 作業計画書")]);
    }

    #[test]
    fn タイトルの末尾で作業計画書へのリンクかを決める() {
        assert!(link(None, "退会機能 作業計画書").is_plan());
        assert!(!link(None, "作業計画書の書き方").is_plan());
        assert!(!link(None, "退会機能").is_plan());
    }

    #[test]
    fn wikilinkでない構文や表示されない文字からリンクを作らない() {
        for body in [
            r"\[[対象 作業計画書]]",
            r"\[\[対象 作業計画書]]",
            "&#91;&#91;対象 作業計画書]]",
            "[別のリンク [[対象 作業計画書]]](/u)",
            "![画像 [[対象 作業計画書]]](/x)",
            "<code>[[対象 作業計画書]]</code>",
            "<pre>[[対象 作業計画書]]</pre>",
            "<a href=\"/u\">[[対象 作業計画書]]</a>",
            "<script>[[対象 作業計画書]]</script>",
            "<style>[[対象 作業計画書]]</style>",
            "<!-- [[対象 作業計画書]] -->",
            "<span title=\"[[対象 作業計画書]]\">説明</span>",
            "[[コード `x` をまたぐ 作業計画書]]",
            "[[HTML <span>をまたぐ</span> 作業計画書]]",
            // 保護する範囲を挟まないエスケープは、後ろの括弧ごと一致を消費する。
            r"\[[x [[対象 作業計画書]]",
            // 角括弧で囲んだラベルのMarkdownのリンク。
            "[[対象 作業計画書]](/u)",
            "[[対象 作業計画書]][ref]\n\n  [ref]: /u",
            // 閉じていないコメントは、HTMLブロックの終わりまで続く。
            "<!-- [[対象 作業計画書]]",
            "<div>\n  <!-- [[対象 作業計画書]]\n  </div>",
        ] {
            let got = parse(&format!("## やってる\n\n- {body}\n"));
            assert!(
                got.sections[0]
                    .items
                    .iter()
                    .all(|item| item.links.is_empty()),
                "Wikiリンクでない記法を読んだ: {body:?}、{got:?}"
            );
        }
    }

    #[test]
    fn 保護する範囲の中の括弧の後ろにあるリンクも読む() {
        let target = link(None, "対象 作業計画書");
        for (body, want) in [
            (
                "`[[` の補完を直す [[Wikiリンクの補完 作業計画書]]",
                link(None, "Wikiリンクの補完 作業計画書"),
            ),
            (
                "<!-- [[ を含むメモ --> [[メモ 作業計画書]]",
                link(None, "メモ 作業計画書"),
            ),
            (
                "説明 <!-- [[ を含むメモ --> [[メモ 作業計画書]]",
                link(None, "メモ 作業計画書"),
            ),
            (
                "[公式ドキュメント](https://example.com/docs \"[[ の書き方\") [[Wikiリンクの記法 作業計画書]]",
                link(None, "Wikiリンクの記法 作業計画書"),
            ),
            ("<code>[[x</code> [[対象 作業計画書]]", target.clone()),
            (
                "<span title=\"[[t\">説明</span> [[対象 作業計画書]]",
                target.clone(),
            ),
            (
                "<https://example.com/[[x> [[対象 作業計画書]]",
                target.clone(),
            ),
            // エスケープした括弧の一致は、自動リンクの区切りの手前で終わる。
            (
                r"\[[<https://e.com/>[[A/B 作業計画書]]",
                link(Some("A"), "B 作業計画書"),
            ),
            (
                "<div>\n  <b>[[x</b> [[HTMLブロック 作業計画書]]\n  </div>",
                link(None, "HTMLブロック 作業計画書"),
            ),
            // HTML5の構文解析と同じく、`<!` で始まる構文は最初の `>` で閉じる。
            ("<![CDATA[x> [[対象 作業計画書]]", target.clone()),
        ] {
            let got = parse(&format!("## やってる\n\n- {body}\n"));
            assert_eq!(
                got.sections[0].items[0].links,
                [want],
                "保護する範囲の後ろのリンクを落とした: {body:?}"
            );
        }
    }

    #[test]
    fn 段落やコードや入れ子のリストをまたいでリンクを作らない() {
        for body in [
            "- [[対象\n\n  作業計画書]]",
            "- [[対象\n\n  ```\n  例\n  ```\n\n  作業計画書]]",
            "- [[対象\n  - 例\n\n  作業計画書]]",
        ] {
            let got = parse(&format!("## やってる\n\n{body}\n"));
            assert!(
                got.sections[0].items[0].links.is_empty(),
                "ブロックをまたいでリンクを作った: {got:?}"
            );
        }
    }

    #[test]
    fn ソース上のタイトルの記号を保持する() {
        let got = parse(
            "## やってる\n\n- [[**太字** 作業計画書]]\n- [[対象 [UI] 作業計画書]]\n- [[設定 (GET |settings) 作業計画書]]\n",
        );
        let found: Vec<_> = got.sections[0]
            .items
            .iter()
            .flat_map(|item| item.links.clone())
            .collect();
        assert_eq!(
            found,
            [
                link(None, "**太字** 作業計画書"),
                link(None, "対象 [UI] 作業計画書"),
                link(None, "設定 (GET |settings) 作業計画書")
            ],
            "タイトルを書き換えた"
        );
    }

    #[test]
    fn 引用や項目の見出しを節にせず_引用のリストを項目にしない() {
        let got = parse(
            "## 未振り分け\n\n- 説明\n  ## やってる\n\n  [[未承認 作業計画書]]\n\n> ## 優先度: 高\n>\n> - [[引用 作業計画書]]\n",
        );
        assert_eq!(got.sections.len(), 1, "入れ子の見出しで節を作った");
        assert_eq!(got.sections[0].title, "未振り分け", "節を切り替えた");
        assert_eq!(got.sections[0].items.len(), 1, "引用の項目も加えた");
        let got = parse("## やってる\n\n> - [[引用 作業計画書]]\n\n- [[通常 作業計画書]]\n");
        assert_eq!(got.sections[0].items.len(), 1, "引用のリストを読んだ");
        assert_eq!(
            got.sections[0].items[0].links,
            [link(None, "通常 作業計画書")]
        );
    }

    #[test]
    fn 見出し内の強調やインラインコードの文字も保持する() {
        let got = parse("## **優先度**: `高`\n\n- [[対象 作業計画書]]\n");
        assert_eq!(
            got.sections[0].title, "優先度: 高",
            "見出しの文字を落とした"
        );
        assert_eq!(got.sections[0].items[0].line_no, 3, "項目の行番号がずれた");
    }

    #[test]
    fn htmlの構造と印の文字参照を考慮してコードを除外する() {
        for body in [
            "<code><table></code>[[対象 作業計画書]]",
            "<code><td>[[対象 作業計画書]]</code>",
            "&#57344;wikilink0s&#57345;例&#57344;wikilink0e&#57345; <code>[[対象 作業計画書]]</code>",
        ] {
            let got = parse(&format!("## やってる\n\n- {body}\n"));
            assert!(
                got.sections[0].items[0].links.is_empty(),
                "コードの中のリンクを読んだ: {body:?}"
            );
        }
        for body in [
            "<span>[[対象 作業計画書]]</span>",
            "<a>[[対象 作業計画書]]</a>",
            "<textarea>[[対象 作業計画書]]</textarea>",
            "<textarea>[[対象 <code> 作業計画書]]</textarea>",
        ] {
            let got = parse(&format!("## やってる\n\n- {body}\n"));
            assert_eq!(
                got.sections[0].items[0].links.len(),
                1,
                "本文として見えるリンクを落とした: {body:?}"
            );
        }
    }
}
