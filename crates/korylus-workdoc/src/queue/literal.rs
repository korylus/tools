//! 項目へ載せる文面のMarkdownを保ち、Wikiリンクを文字列として表示する。

use std::ops::Range;
use std::sync::LazyLock;

use pulldown_cmark::{Event, LinkType, Options, Parser, Tag};
use regex::Regex;

use crate::markdown::parse_source;

/// WikinoのLinkifyがリンクにするURL。goldmarkの `urlRegexp` と同じ形にする。
static URL: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(concat!(
        r"^(?:http|https|ftp)://[-a-zA-Z0-9@:%._+~#=]{1,256}\.[a-z]+(?::[0-9]+)?",
        r#"(?:[/#?][-a-zA-Z0-9@:%_+.~#$!?&/=();,'">^{}\[\]`]*)?"#,
    ))
    .expect("固定の正規表現を作れなかった")
});

/// WikinoのLinkifyがリンクにする `www.` で始まるURL。goldmarkの `wwwURLRegxp` と同じ形にする。
static WWW_URL: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(concat!(
        r"^www\.[-a-zA-Z0-9@:%._+~#=]{1,256}\.[a-z]+",
        r#"(?:[/#?][-a-zA-Z0-9@:%_+.~#!?&/=();,'">^{}\[\]`]*)?"#,
    ))
    .expect("固定の正規表現を作れなかった")
});

/// 本文から読み取った、Wikiリンクの扱いを決めるための範囲。
#[derive(Default)]
struct Ranges {
    /// WikinoがWikiリンクとして読まない範囲 (コード・自動リンク・行内のHTML)。
    keep: Vec<Range<usize>>,
    /// HTMLブロック。
    html: Vec<Range<usize>>,
    /// 自動リンクを除くリンクと画像。
    links: Vec<Range<usize>>,
    /// 段落・見出し・リストの項目。
    blocks: Vec<Range<usize>>,
    /// タスクリストのチェックボックス。
    markers: Vec<Range<usize>>,
}

impl Ranges {
    fn new(src: &str) -> Self {
        let parse_src = parse_source(src);
        let mut out = Self::default();
        for (event, range) in
            Parser::new_ext(&parse_src, Options::ENABLE_TASKLISTS).into_offset_iter()
        {
            match event {
                Event::Code(_)
                | Event::InlineHtml(_)
                | Event::Start(
                    Tag::CodeBlock(_)
                    | Tag::Link {
                        link_type: LinkType::Autolink | LinkType::Email,
                        ..
                    },
                ) => out.keep.push(range),
                Event::Start(Tag::Link { .. } | Tag::Image { .. }) => out.links.push(range),
                Event::Start(Tag::HtmlBlock) => out.html.push(range),
                Event::Start(Tag::Paragraph | Tag::Heading { .. } | Tag::Item) => {
                    out.blocks.push(range);
                }
                Event::TaskListMarker(_) => out.markers.push(range),
                _ => {}
            }
        }
        out
    }

    /// 位置 `i` が、WikinoのLinkifyが必ずリンクにする裸のURLの中にあるかを返す。
    fn in_bare_url(&self, src: &str, i: usize) -> bool {
        let line_start = src[..i].rfind('\n').map_or(0, |p| p + 1);
        src[line_start..i]
            .match_indices(['h', 'f', 'w'])
            .any(|(s, _)| {
                self.bare_url(src, line_start + s)
                    .is_some_and(|url| url.contains(&i))
            })
    }

    /// 位置 `s` から始まる、WikinoのLinkifyが必ずリンクにする裸のURLの範囲を返す。
    ///
    /// goldmarkのLinkifyは、行頭か半角スペースの直後から始まるURLを、段落にリンクの開き括弧が残っていないときに読む。
    /// リンクにするか決めきれないURL (`(`・タブ・強調の印の直後・前に `[` がある段落・末尾が `)` か `;`) は、
    /// Wikiリンクを無効にして未公開のページを作らないことを優先するため、`None` を返す。
    fn bare_url(&self, src: &str, s: usize) -> Option<Range<usize>> {
        if !matches!(src[..s].bytes().last(), None | Some(b' ' | b'\n')) {
            return None;
        }
        let inside = |ranges: &[Range<usize>]| ranges.iter().any(|r| r.contains(&s));
        if inside(&self.keep) || inside(&self.html) || inside(&self.links) {
            return None;
        }
        let block = self
            .blocks
            .iter()
            .filter(|r| r.contains(&s))
            .max_by_key(|r| r.start)?;
        let open_bracket = src[block.start..s]
            .match_indices('[')
            .any(|(j, _)| !self.markers.iter().any(|m| m.contains(&(block.start + j))));
        if open_bracket {
            return None;
        }

        let rest = &src[s..];
        let url = ["http:", "https:", "ftp:"]
            .iter()
            .any(|p| rest.starts_with(p))
            .then(|| URL.find(rest))
            .flatten()
            .or_else(|| {
                rest.starts_with("www.")
                    .then(|| WWW_URL.find(rest))
                    .flatten()
            })?;
        let url = rest[..url.end()].trim_end_matches(['?', '!', '.', ',', ':', '*', '_', '~']);
        if url.ends_with([')', ';']) {
            return None;
        }
        Some(s..s + url.len())
    }
}

/// WikinoがWikiリンクとして読む `[[` の開始括弧を文字参照にする。
///
/// MarkdownとHTMLで文字参照は表示時に括弧へ戻るが、Wikiリンクの構文にはならない。
/// WikinoがWikiリンクとして読まない範囲 (コード・自動リンク・行内のHTML・Linkifyが必ずリンクにする裸のURL) は、
/// 文字参照が括弧へ戻らずにそのまま表示されるため変えない。
/// Markdownで既にエスケープされた括弧も変えず、表示上のバックスラッシュを増やさない。
pub(super) fn escape_wikilinks(src: &str) -> String {
    let ranges = Ranges::new(src);
    let mut out = String::new();
    let mut cursor = 0;
    for (i, _) in src.match_indices('[') {
        if !src[i..].starts_with("[[")
            || ranges.keep.iter().any(|r| r.contains(&i))
            || ranges.in_bare_url(src, i)
        {
            continue;
        }
        let escaped = src.as_bytes()[..i]
            .iter()
            .rev()
            .take_while(|&&b| b == b'\\')
            .count()
            % 2
            != 0;
        if escaped && !ranges.html.iter().any(|r| r.contains(&i)) {
            continue;
        }
        out.push_str(&src[cursor..i]);
        out.push_str("&#91;");
        cursor = i + 1;
    }
    out.push_str(&src[cursor..]);
    out
}

#[cfg(test)]
mod tests {
    use pulldown_cmark::html::push_html;

    use super::*;

    fn rendered(src: &str) -> String {
        let mut html = String::new();
        push_html(&mut html, Parser::new(src));
        scraper::Html::parse_fragment(&html).html()
    }

    #[test]
    fn wikiリンクを無効にしてもmarkdownの表示を保つ() {
        for src in [
            "**強調** と [[作業計画書]]、[[別のページ]]",
            "[[[ページ]]]",
            r"\[[ページ]] と \\[[別のページ]] と \[\[別の表記]]",
            "[リンク](https://example.com/[[path]]) と [[ページ]]",
            "`[[コード]]` と [[ページ]]",
            "```text\n[[コード]]\n```\n\n[[ページ]]",
            "    [[字下げのコード]]\n\n[[ページ]]",
            "<div>\n\\[[ページ]]\n</div>",
            "> <div>\n> \\[[ページ]]\n> </div>",
            "<https://example.com/[[x]]> と [[ページ]]",
            "a <!-- [[メモ]] --> と <span title=\"[[属性]]\">[[ページ]]</span>",
            "見る https://example.com/[[x]] と [[ページ]]",
        ] {
            let out = escape_wikilinks(src);
            assert_eq!(rendered(&out), rendered(src), "表示が変わった: {src:?}");
            assert_eq!(escape_wikilinks(&out), out, "二重に変換した: {src:?}");
        }
        assert_eq!(
            escape_wikilinks("[[ページ]]"),
            "&#91;[ページ]]",
            "開始括弧を文字参照にしていない"
        );
        assert_eq!(
            escape_wikilinks("`[[コード]]`"),
            "`[[コード]]`",
            "コードの中を変えた"
        );
    }

    #[test]
    fn wikinoがリンクにしない範囲のwikiリンクは変えない() {
        for (src, want) in [
            (
                "<https://example.com/[[x]]> と [[ページ]]",
                "<https://example.com/[[x]]> と &#91;[ページ]]",
            ),
            (
                "a <!-- [[メモ]] --> <span title=\"[[属性]]\">[[ページ]]</span>",
                "a <!-- [[メモ]] --> <span title=\"[[属性]]\">&#91;[ページ]]</span>",
            ),
            (
                "見る https://example.com/[[x]]. と [[ページ]]",
                "見る https://example.com/[[x]]. と &#91;[ページ]]",
            ),
            (
                "https://example.com/[[x]]\n\nwww.example.com/[[y]]",
                "https://example.com/[[x]]\n\nwww.example.com/[[y]]",
            ),
            (
                "- [ ] A: https://example.com/[[x]]",
                "- [ ] A: https://example.com/[[x]]",
            ),
        ] {
            assert_eq!(escape_wikilinks(src), want, "変えた範囲が違う: {src:?}");
        }
    }

    #[test]
    fn linkifyがリンクにするか決めきれない裸のurlは_wikiリンクを無効にする() {
        for (src, want) in [
            (
                "[注 https://example.com/[[x]]",
                "[注 https://example.com/&#91;[x]]",
            ),
            (
                "<div>\nhttps://example.com/[[x]]\n</div>",
                "<div>\nhttps://example.com/&#91;[x]]\n</div>",
            ),
            (
                "(https://example.com/[[x]])",
                "(https://example.com/&#91;[x]])",
            ),
            (
                "見る https://example.com/[[x]])",
                "見る https://example.com/&#91;[x]])",
            ),
            (
                "見る\thttps://example.com/[[x]]",
                "見る\thttps://example.com/&#91;[x]]",
            ),
            ("見る https://[[x]]", "見る https://&#91;[x]]"),
        ] {
            assert_eq!(escape_wikilinks(src), want, "変えた範囲が違う: {src:?}");
        }
    }
}
