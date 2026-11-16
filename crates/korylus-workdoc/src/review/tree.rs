//! pulldown-cmarkのイベントから、ブロックの木を組み立てる。
//!
//! 移植元のGoはgoldmarkの構文木を辿って指摘を読み取る。
//! pulldown-cmarkはイベントの列だけを返すため、読み取りに要るブロックの親子と位置をここで持つ。

use std::ops::Range;

use pulldown_cmark::{Event, HeadingLevel, Parser, Tag, TagEnd};

use crate::markdown::parse_source;

/// ブロックの種類。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(super) enum Kind {
    /// 文書全体。
    Root,
    /// 見出し。値は見出しのレベル (1から6)。
    Heading(u8),
    /// 段落。
    Paragraph,
    /// 詰めたリストの項目の本文。goldmarkのTextBlockに当たり、段落とは区別する。
    TextBlock,
    List,
    Item,
    CodeBlock,
    HtmlBlock,
    BlockQuote,
    /// イベント列に現れない、参照形式のリンクの定義。
    LinkDefinition,
    /// 読み取りで区別しないブロック (区切り線など)。
    Other,
}

/// ブロック1つ。
#[derive(Debug, Clone)]
pub(super) struct Node {
    pub(super) kind: Kind,
    pub(super) parent: Option<usize>,
    pub(super) children: Vec<usize>,
    /// コンテナの構文と末尾の改行を含む、ブロック全体の範囲。
    span: Range<usize>,
    /// ブロックの中で直接書かれた本文の、行ごとの範囲。
    content: Vec<Range<usize>>,
}

/// ブロックの1行。
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct Line {
    /// 前後の空白を除いた本文。
    pub(super) text: String,
    /// 行番号 (1始まり)。
    pub(super) no: usize,
    /// 本文の先頭の、文書での位置 (バイト)。
    pub(super) start: usize,
}

/// 文書のブロックの木。添字0が文書全体。
#[derive(Debug)]
pub(super) struct Tree<'a> {
    src: &'a str,
    nodes: Vec<Node>,
}

impl<'a> Tree<'a> {
    /// `src` をCommonMarkとして解析し、ブロックの木を組み立てる。
    pub(super) fn parse(src: &'a str) -> Self {
        // 解析は写しで行い、本文と位置には元のソースを使う。
        let parse_src = parse_source(src);
        let mut nodes = vec![Node {
            kind: Kind::Root,
            parent: None,
            children: Vec::new(),
            span: 0..src.len(),
            content: Vec::new(),
        }];
        let mut stack = vec![0];
        for (event, range) in Parser::new(&parse_src).into_offset_iter() {
            let current = *stack.last().unwrap_or(&0);
            match event {
                Event::Start(tag) if !is_inline_tag(&tag) => {
                    let id = push(&mut nodes, current, block_kind(&tag));
                    nodes[id].span = range;
                    stack.push(id);
                }
                Event::End(tag) if !is_inline_tag_end(tag) => {
                    if stack.len() > 1 {
                        stack.pop();
                    }
                }
                Event::Rule => {
                    let id = push(&mut nodes, current, Kind::Other);
                    nodes[id].span = range;
                }
                _ => {
                    let target = if nodes[current].kind == Kind::Item {
                        // 詰めたリストの項目の本文は段落にならないため、TextBlockとして持つ。
                        match nodes[current].children.last() {
                            Some(&last) if nodes[last].kind == Kind::TextBlock => last,
                            _ => push(&mut nodes, current, Kind::TextBlock),
                        }
                    } else {
                        current
                    };
                    let quotes = stack
                        .iter()
                        .filter(|&&id| nodes[id].kind == Kind::BlockQuote)
                        .count();
                    if nodes[target].kind == Kind::TextBlock {
                        if nodes[target].content.is_empty() {
                            nodes[target].span = range.clone();
                        } else {
                            nodes[target].span.start = nodes[target].span.start.min(range.start);
                            nodes[target].span.end = nodes[target].span.end.max(range.end);
                        }
                    }
                    add_content(&mut nodes[target].content, src, range, quotes);
                }
            }
        }
        // 段落などのイベントの範囲は、末尾の改行を含むブロック全体の範囲を使う。
        // TextBlockだけはブロックのイベントが無いため、本文から範囲を求める。
        let mut tree = Self { src, nodes };
        tree.restore_definitions();
        for node in &mut tree.nodes {
            node.content.sort_by_key(|range| range.start);
            let mut lines: Vec<Range<usize>> = Vec::new();
            for range in node.content.drain(..) {
                if let Some(prev) = lines.last_mut()
                    && (range.start <= prev.end || !src[prev.end..range.start].contains('\n'))
                {
                    prev.end = prev.end.max(range.end);
                } else {
                    lines.push(range);
                }
            }
            node.content = lines;
        }
        tree
    }

    pub(super) fn node(&self, id: usize) -> &Node {
        &self.nodes[id]
    }

    /// 文書全体の直下のブロックを返す。
    pub(super) fn root_children(&self) -> &[usize] {
        &self.nodes[0].children
    }

    /// ブロックの本文の行を返す (goldmarkの `Lines` に当たる)。
    ///
    /// 本文を持つのは見出し・段落・TextBlock・コードブロック・HTMLブロック・リンク定義で、それ以外は空を返す。
    pub(super) fn lines(&self, id: usize) -> Vec<Line> {
        let node = &self.nodes[id];
        if !matches!(
            node.kind,
            Kind::Heading(_)
                | Kind::Paragraph
                | Kind::TextBlock
                | Kind::CodeBlock
                | Kind::HtmlBlock
                | Kind::LinkDefinition
        ) {
            return Vec::new();
        }
        node.content
            .iter()
            .map(|range| {
                let raw = &self.src[range.clone()];
                Line {
                    text: raw.trim().to_owned(),
                    no: self.line_no(range.start),
                    start: range.start + (raw.len() - raw.trim_start().len()),
                }
            })
            .collect()
    }

    /// ブロックの1行目の本文を返す。本文が無ければ空文字を返す。
    pub(super) fn line_text(&self, id: usize) -> String {
        self.lines(id)
            .into_iter()
            .next()
            .map(|line| line.text)
            .unwrap_or_default()
    }

    /// ブロックの全行を空白でつないだ本文を返す。
    pub(super) fn block_text(&self, id: usize) -> String {
        self.lines(id)
            .into_iter()
            .map(|line| line.text)
            .collect::<Vec<_>>()
            .join(" ")
    }

    /// ブロックの1行目の行番号を返す。
    /// 本文を持たないブロック (リスト項目など) は、最初の子を辿って数える。辿れなければ0を返す。
    pub(super) fn start_line(&self, mut id: usize) -> usize {
        loop {
            if let Some(line) = self.lines(id).first() {
                return line.no;
            }
            match self.nodes[id].children.first() {
                Some(&child) => id = child,
                None => return 0,
            }
        }
    }

    /// ブロック `id` のあとに続く兄弟のブロックを返す。
    pub(super) fn next_siblings(&self, id: usize) -> &[usize] {
        let Some(parent) = self.nodes[id].parent else {
            return &[];
        };
        let siblings = &self.nodes[parent].children;
        let index = siblings
            .iter()
            .position(|&s| s == id)
            .map_or(siblings.len(), |i| i + 1);
        &siblings[index..]
    }

    /// 位置 `offset` の行番号 (1始まり) を返す。
    fn line_no(&self, offset: usize) -> usize {
        self.src[..offset].matches('\n').count() + 1
    }

    /// イベント間のソースに残るリンク定義を、元のコンテナの子として戻す。
    fn restore_definitions(&mut self) {
        for id in (0..self.nodes.len()).rev() {
            if self.nodes[id].kind == Kind::List {
                // リストのイベントの終端は、項目の外のリンク定義まで含むことがある。
                if let Some(&last) = self.nodes[id].children.last() {
                    self.nodes[id].span.end = self.nodes[last].span.end;
                }
                continue;
            }
            if !matches!(
                self.nodes[id].kind,
                Kind::Root | Kind::Item | Kind::BlockQuote
            ) {
                continue;
            }
            let mut quotes = 0;
            let mut ancestor = Some(id);
            while let Some(a) = ancestor {
                quotes += usize::from(self.nodes[a].kind == Kind::BlockQuote);
                ancestor = self.nodes[a].parent;
            }
            let mut cursor = self.nodes[id].span.start;
            if self.nodes[id].kind == Kind::Item {
                cursor += list_marker_len(&self.src[cursor..]);
            }
            let mut children = Vec::new();
            for child in self.nodes[id].children.clone() {
                self.append_definitions(
                    id,
                    cursor..self.nodes[child].span.start,
                    quotes,
                    &mut children,
                );
                children.push(child);
                cursor = self.nodes[child].span.end;
            }
            self.append_definitions(id, cursor..self.nodes[id].span.end, quotes, &mut children);
            self.nodes[id].children = children;
        }
    }

    /// ソースの隙間にあるリンク定義を、位置を保ったブロックとして加える。
    fn append_definitions(
        &mut self,
        parent: usize,
        gap: Range<usize>,
        quotes: usize,
        children: &mut Vec<usize>,
    ) {
        if gap.start >= gap.end {
            return;
        }
        let mut text = String::new();
        let mut lines = Vec::new();
        let mut offset = gap.start;
        for raw in self.src[gap].split_inclusive('\n') {
            let body = without_quotes(raw, quotes).trim_start_matches([' ', '\t']);
            let start = offset + raw.len() - body.len();
            lines.push((text.len(), start..start + body.len()));
            text.push_str(body);
            offset += raw.len();
        }
        let mut cursor = 0;
        while cursor < text.len() {
            // 同じラベルの定義もブロックの境界になる。残りを読み直して重複も拾う。
            let parser = Parser::new(&text[cursor..]);
            let Some(span) = parser
                .reference_definitions()
                .iter()
                .map(|(_, def)| def.span.clone())
                .min_by_key(|span| span.start)
            else {
                break;
            };
            let span = cursor + span.start..cursor + span.end;
            cursor = span.end;
            let mut content = Vec::new();
            for (generated, original) in &lines {
                let start = span.start.max(*generated);
                let end = span.end.min(generated + original.len());
                if start < end {
                    let range =
                        original.start + start - generated..original.start + end - generated;
                    add_content(&mut content, self.src, range, 0);
                }
            }
            if let (Some(first), Some(last)) = (content.first(), content.last()) {
                let id = self.nodes.len();
                self.nodes.push(Node {
                    kind: Kind::LinkDefinition,
                    parent: Some(parent),
                    children: Vec::new(),
                    span: first.start..last.end,
                    content,
                });
                children.push(id);
            }
        }
    }
}

/// イベントの範囲を行に分け、コンテナの構文を除いた本文の範囲を残す。
fn add_content(content: &mut Vec<Range<usize>>, src: &str, range: Range<usize>, quotes: usize) {
    let mut offset = range.start;
    for (i, raw) in src[range.clone()].split_inclusive('\n').enumerate() {
        let line = raw.strip_suffix('\n').unwrap_or(raw);
        // イベントの先頭は本文を指す。継続行だけ、構文木上の引用の深さまで接頭辞を除く。
        let text = if i == 0 {
            line
        } else {
            without_quotes(line, quotes)
        };
        let end = offset + line.len();
        let mut start = end - text.len();
        // pulldown-cmarkはエスケープした文字の範囲をバックスラッシュの後ろから始めるが、
        // 移植元のgoldmarkの行はバックスラッシュを含むため、行の先頭では範囲に戻す。
        if i == 0 && start > 0 && src.as_bytes()[start - 1] == b'\\' {
            start -= 1;
        }
        content.push(start..end);
        offset += raw.len();
    }
}

/// 引用の継続行から、構文木上の深さまで引用の接頭辞を除く。本文の記号は残す。
fn without_quotes(mut text: &str, depth: usize) -> &str {
    for _ in 0..depth {
        let trimmed = text.trim_start_matches([' ', '\t']);
        let Some(rest) = trimmed.strip_prefix('>') else {
            break;
        };
        text = rest;
    }
    text
}

/// リスト項目の先頭から、マーカーと直後の空白の長さを返す。
fn list_marker_len(text: &str) -> usize {
    let marker = text.bytes().take_while(u8::is_ascii_digit).count();
    let marker = if marker > 0 { marker + 1 } else { 1 };
    marker
        + text
            .get(marker..)
            .unwrap_or("")
            .bytes()
            .take_while(|b| matches!(b, b' ' | b'\t'))
            .count()
}

/// 子のブロックを加え、その添字を返す。
fn push(nodes: &mut Vec<Node>, parent: usize, kind: Kind) -> usize {
    let id = nodes.len();
    nodes.push(Node {
        kind,
        parent: Some(parent),
        children: Vec::new(),
        span: 0..0,
        content: Vec::new(),
    });
    nodes[parent].children.push(id);
    id
}

fn block_kind(tag: &Tag) -> Kind {
    match tag {
        Tag::Heading { level, .. } => Kind::Heading(heading_level(*level)),
        Tag::Paragraph => Kind::Paragraph,
        Tag::List(_) => Kind::List,
        Tag::Item => Kind::Item,
        Tag::CodeBlock(_) => Kind::CodeBlock,
        Tag::HtmlBlock => Kind::HtmlBlock,
        Tag::BlockQuote(_) => Kind::BlockQuote,
        _ => Kind::Other,
    }
}

fn heading_level(level: HeadingLevel) -> u8 {
    match level {
        HeadingLevel::H1 => 1,
        HeadingLevel::H2 => 2,
        HeadingLevel::H3 => 3,
        HeadingLevel::H4 => 4,
        HeadingLevel::H5 => 5,
        HeadingLevel::H6 => 6,
    }
}

/// ブロックの中に書くインラインの要素のタグかを返す。
fn is_inline_tag(tag: &Tag) -> bool {
    matches!(
        tag,
        Tag::Emphasis
            | Tag::Strong
            | Tag::Strikethrough
            | Tag::Superscript
            | Tag::Subscript
            | Tag::Link { .. }
            | Tag::Image { .. }
    )
}

fn is_inline_tag_end(tag: TagEnd) -> bool {
    matches!(
        tag,
        TagEnd::Emphasis
            | TagEnd::Strong
            | TagEnd::Strikethrough
            | TagEnd::Superscript
            | TagEnd::Subscript
            | TagEnd::Link
            | TagEnd::Image
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 見出しの本文は記号と閉じる記号を除き_コードスパンのバッククォートを残す() {
        let tree = Tree::parse("### `internal/a.go`: 見出し ##\n");
        let heading = tree.root_children()[0];

        assert_eq!(
            tree.node(heading).kind,
            Kind::Heading(3),
            "見出しのレベルが期待と異なる"
        );
        assert_eq!(
            tree.line_text(heading),
            "`internal/a.go`: 見出し",
            "見出しの本文が期待と異なる"
        );
    }

    #[test]
    fn 詰めたリストの項目の本文は段落と区別する() {
        let tree = Tree::parse("- [ ] 直す\n- [ ] 直さない\n\n段落\n");
        let list = tree.root_children()[0];
        let item = tree.node(list).children[0];
        let block = tree.node(item).children[0];

        assert_eq!(
            tree.node(block).kind,
            Kind::TextBlock,
            "詰めたリストの項目の本文の種類が期待と異なる"
        );
        assert_eq!(
            tree.line_text(block),
            "[ ] 直す",
            "項目の本文が期待と異なる"
        );
        assert_eq!(
            tree.node(tree.root_children()[1]).kind,
            Kind::Paragraph,
            "段落の種類が期待と異なる"
        );
    }

    #[test]
    fn 段落の各行を行番号と先頭の位置とともに返す() {
        let src = "- a\n\n  **区分**: 自動対応可\n  **対応方針**:\n";
        let tree = Tree::parse(src);
        let item = tree.node(tree.root_children()[0]).children[0];
        let paragraph = tree.node(item).children[1];

        let lines = tree.lines(paragraph);
        let got: Vec<(&str, usize)> = lines.iter().map(|l| (l.text.as_str(), l.no)).collect();
        assert_eq!(
            got,
            [("**区分**: 自動対応可", 3), ("**対応方針**:", 4)],
            "段落の行が期待と異なる"
        );
        assert_eq!(
            &src[lines[1].start..lines[1].start + 2],
            "**",
            "2行目の先頭の位置が期待と異なる"
        );
        assert_eq!(
            tree.start_line(item),
            1,
            "項目の1行目の行番号が期待と異なる"
        );
    }

    #[test]
    fn 行の先頭のエスケープのバックスラッシュを本文に含める() {
        let src = "### \\[問題\\]\n\n\\[ ] a\n補足\n\\**区分**: 自動対応可\n\n> - \\[ ] b\n";
        let tree = Tree::parse(src);
        let [heading, paragraph, quote] = tree.root_children() else {
            panic!("ブロックの数が期待と異なる: {:?}", tree.root_children());
        };
        let list = tree.node(*quote).children[0];
        let item = tree.node(list).children[0];
        let block = tree.node(item).children[0];

        assert_eq!(
            tree.line_text(*heading),
            "\\[問題\\]",
            "見出しの本文が期待と異なる"
        );
        let lines = tree.lines(*paragraph);
        assert_eq!(
            lines.iter().map(|l| l.text.as_str()).collect::<Vec<_>>(),
            ["\\[ ] a", "補足", "\\**区分**: 自動対応可"],
            "段落の行が期待と異なる"
        );
        assert_eq!(
            tree.line_text(block),
            "\\[ ] b",
            "引用の中の項目の本文が期待と異なる"
        );
        for line in lines.into_iter().chain(tree.lines(block)) {
            assert!(
                src[line.start..].starts_with(&line.text),
                "本文の位置が異なる: {line:?}"
            );
        }
    }

    #[test]
    fn 複数行のインライン要素でも引用の構文だけを除く() {
        let src = "> - **a\n>   b**\n>   `c\n>   d > e`\n";
        let tree = Tree::parse(src);
        let quote = tree.root_children()[0];
        let list = tree.node(quote).children[0];
        let item = tree.node(list).children[0];
        let block = tree.node(item).children[0];
        let lines = tree.lines(block);
        assert_eq!(
            lines
                .iter()
                .map(|l| (l.text.as_str(), l.no))
                .collect::<Vec<_>>(),
            [("**a", 1), ("b**", 2), ("`c", 3), ("d > e`", 4)],
            "引用の構文と本文の記号を取り違えた"
        );
        for line in lines {
            assert!(
                src[line.start..].starts_with(&line.text),
                "本文の位置が異なる: {line:?}"
            );
        }
    }

    #[test]
    fn リンク定義の重複とリストの外の境界を保持する() {
        let src = "- a\n\n  [r]: /x\n  [r]: /y\n\n  - b\n\n[q]: /z\n[q]: /a\n";
        let tree = Tree::parse(src);
        let root = tree.root_children();
        assert_eq!(
            root.iter()
                .map(|&id| tree.node(id).kind)
                .collect::<Vec<_>>(),
            [Kind::List, Kind::LinkDefinition, Kind::LinkDefinition],
            "リストの外の定義をリストに含めた"
        );
        let item = tree.node(root[0]).children[0];
        let children = &tree.node(item).children;
        assert_eq!(
            children
                .iter()
                .map(|&id| tree.node(id).kind)
                .collect::<Vec<_>>(),
            [
                Kind::Paragraph,
                Kind::LinkDefinition,
                Kind::LinkDefinition,
                Kind::List
            ],
            "項目内の定義の境界を失った"
        );
        assert_eq!(
            tree.line_text(children[1]),
            "[r]: /x",
            "1つ目の定義が異なる"
        );
        assert_eq!(
            tree.line_text(children[2]),
            "[r]: /y",
            "重複した定義が異なる"
        );
        assert_eq!(
            tree.start_line(children[2]),
            4,
            "重複した定義の行番号が異なる"
        );
    }
}
