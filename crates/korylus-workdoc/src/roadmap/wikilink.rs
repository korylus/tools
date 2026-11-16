//! ソース上のWikiリンクを、MarkdownとHTMLでの表示範囲と対応付ける。
//!
//! Wikinoと同じく、表示でWikiリンクにならない範囲を先にソース上で求め、それを除いた区間ごとに
//! `[[...]]` を探す。区間に分けるのは、保護する範囲の中の `[[` から始まる一致が、
//! 後ろに書かれた別のWikiリンクを飲み込まないようにするためである。

use std::collections::HashMap;
use std::ops::Range;
use std::sync::LazyLock;

use pulldown_cmark::{CowStr, Event, LinkType, Parser, Tag, TagEnd};
use regex::Regex;
use scraper::{ElementRef, Html, Node};

use super::WikiLink;

/// Wikiリンクの括弧はデコードせず読む。タイトル内の単独の角括弧も許す。
static WIKILINK: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\[\[(.*?)\]\]").expect("固定の正規表現を作れなかった"));

/// HTMLブロックの中の、タグ・コメント・宣言など。
///
/// タグはCommonMarkの生のHTMLの定義に合わせる。`<!` と `<?` で始まるほかの構文は、HTML5の構文解析と
/// 同じく最初の `>` までとする。ここで見つからない構文の中の括弧は、描画後の木で表示されるかを確かめる。
static HTML_TOKEN: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(concat!(
        r"(?s)<!-->|<!--->|<!--.*?-->|<![^>]*>|<\?[^>]*>",
        r"|</[A-Za-z][A-Za-z0-9-]*[\t\n\x0C\r ]*>",
        r#"|<[A-Za-z][A-Za-z0-9-]*(?:[\t\n\x0C\r ]+[A-Za-z_:][A-Za-z0-9_.:-]*"#,
        r#"(?:[\t\n\x0C\r ]*=[\t\n\x0C\r ]*(?:[^\t\n\x0C\r "'=<>`]+|'[^']*'|"[^"]*"))?)*[\t\n\x0C\r ]*/?>"#,
    ))
    .expect("固定の正規表現を作れなかった")
});

/// 描画後の位置を確かめる、ソース上の範囲。
struct Mark {
    kind: MarkKind,
    spans: Vec<Range<usize>>,
}

enum MarkKind {
    /// テキスト・コード・コードブロック。描画後に隠れていれば保護する。
    Content,
    /// HTMLのタグやコメント。描画後に文字として表示されなければ保護する。
    Tag,
}

/// 開いているMarkdownのリンク。
struct OpenLink {
    span: Range<usize>,
    /// ラベルを含むリンク全体を保護するかどうか。許すURLのa要素として描画されるリンクと
    /// 自動リンクでは、ラベルも本文としてWikiリンクを作らない。
    whole: bool,
    label: Option<Range<usize>>,
}

/// Wikiリンクを探す前に求める範囲。
struct Ranges {
    /// 表示でWikiリンクにならないソースの範囲。
    protected: Vec<Range<usize>>,
    /// Markdownのエスケープを解釈しない範囲 (コードとHTMLブロック)。
    literal: Vec<Range<usize>>,
}

/// itemsのソース範囲からWikiリンクを返す。excludedの子ブロックは候補にしない。
pub(super) fn scan(
    src: &str,
    items: &[Range<usize>],
    excluded: &[Range<usize>],
) -> Vec<(usize, WikiLink)> {
    if items.is_empty() || !src.contains("[[") {
        return Vec::new();
    }
    let ranges = source_ranges(src);
    let mut boundaries: Vec<_> = ranges.protected.iter().chain(excluded).cloned().collect();
    boundaries.sort_by_key(|range| range.start);

    let mut found = Vec::new();
    for item in items {
        for span in unprotected_spans(item, &boundaries) {
            for matched in WIKILINK.captures_iter(&src[span.clone()]) {
                let (Some(whole), Some(inner)) = (matched.get(0), matched.get(1)) else {
                    continue;
                };
                let start = span.start + whole.start();
                // バックスラッシュは区間ではなく本文全体で数える。区間の直前のエスケープも開始括弧に掛かるためである。
                if !ranges.literal.iter().any(|range| range.contains(&start))
                    && src.as_bytes()[..start]
                        .iter()
                        .rev()
                        .take_while(|&&b| b == b'\\')
                        .count()
                        % 2
                        != 0
                {
                    continue;
                }
                if let Some(link) = WikiLink::parse(inner.as_str()) {
                    found.push((start, link));
                }
            }
        }
    }
    found
}

/// itemのうち、boundaries (開始位置の順) のどれにも重ならない区間を返す。
fn unprotected_spans(item: &Range<usize>, boundaries: &[Range<usize>]) -> Vec<Range<usize>> {
    let mut spans = Vec::new();
    let mut cursor = item.start;
    for range in boundaries {
        if range.start >= item.end {
            break;
        }
        if range.end <= cursor {
            continue;
        }
        if range.start > cursor {
            spans.push(cursor..range.start);
        }
        cursor = range.end;
    }
    if cursor < item.end {
        spans.push(cursor..item.end);
    }
    spans
}

/// srcを解析し、保護する範囲とエスケープを解釈しない範囲を求める。
///
/// 構文の範囲 (Markdownのリンクと画像・自動リンク・リンク参照定義) は解析結果から決める。
/// テキスト・コード・HTMLのタグが表示されるかどうかは、解析したイベントの列に印を差し込んで描画し、
/// HTML5の木で確かめる。印は解析した後のイベントに差し込み、リンクの括弧の並びなどの構文を変えないようにする。
#[expect(
    clippy::too_many_lines,
    reason = "イベントの列を1回で辿りながら、印・リンク・画像・コードブロック・HTMLブロックの状態を共有して持つため"
)]
fn source_ranges(src: &str) -> Ranges {
    let parser = Parser::new(src).into_offset_iter();
    let mut protected: Vec<_> = parser
        .reference_definitions()
        .iter()
        .map(|(_, definition)| definition.span.clone())
        .collect();
    let events: Vec<_> = parser.collect();

    // 文字参照で書かれた印も見誤らないよう、描画後の本文にも無い接頭辞を選ぶ。
    let original_text = texts(&render(events.iter().map(|(event, _)| event.clone()))).text;
    let mut prefix = "\u{e000}wikilink".to_owned();
    while src.contains(&prefix) || original_text.contains(&prefix) {
        prefix.push('_');
    }

    let mut marker = Marker {
        prefix,
        marks: Vec::new(),
    };
    let mut literal = Vec::new();
    let mut annotated = Vec::with_capacity(events.len() * 2);
    let mut links: Vec<OpenLink> = Vec::new();
    let mut image_depth = 0usize;
    let mut in_code_block = false;
    let mut html_block: Option<Vec<(CowStr, Range<usize>)>> = None;

    for (event, range) in events {
        if let Some(link) = links.last_mut()
            && event != Event::End(TagEnd::Link)
        {
            link.label = Some(match &link.label {
                Some(label) => label.start.min(range.start)..label.end.max(range.end),
                None => range.clone(),
            });
        }
        match event {
            Event::Start(Tag::Link {
                link_type,
                ref dest_url,
                ..
            }) => {
                links.push(OpenLink {
                    span: range.clone(),
                    whole: matches!(link_type, LinkType::Autolink | LinkType::Email)
                        || allowed_url(dest_url),
                    label: None,
                });
                annotated.push(event);
            }
            Event::End(TagEnd::Link) => {
                if let Some(link) = links.pop() {
                    match link.label {
                        Some(label) if !link.whole => {
                            // ラベルは表示されるため、括弧とリンク先の構文だけを保護する。
                            protected.push(link.span.start..label.start);
                            protected.push(label.end..link.span.end);
                        }
                        _ => protected.push(link.span),
                    }
                }
                annotated.push(event);
            }
            Event::Start(Tag::Image { .. }) => {
                // 代替テキストは属性になり、本文として表示されない。
                if image_depth == 0 {
                    protected.push(range);
                }
                image_depth += 1;
                annotated.push(event);
            }
            Event::End(TagEnd::Image) => {
                image_depth = image_depth.saturating_sub(1);
                annotated.push(event);
            }
            _ if image_depth > 0 => annotated.push(event),
            Event::Start(Tag::CodeBlock(_)) => {
                literal.push(range.clone());
                annotated.push(event);
                annotated.push(Event::Text(marker.content(vec![range]).into()));
                in_code_block = true;
            }
            Event::End(TagEnd::CodeBlock) => {
                in_code_block = false;
                annotated.push(event);
            }
            Event::Start(Tag::HtmlBlock) => {
                literal.push(range);
                html_block = Some(Vec::new());
                annotated.push(event);
            }
            Event::Html(text) if html_block.is_some() => {
                if let Some(lines) = html_block.as_mut() {
                    lines.push((text, range));
                }
            }
            Event::End(TagEnd::HtmlBlock) => {
                if let Some(lines) = html_block.take() {
                    marker.html_block(src, lines, &mut annotated, &mut protected);
                }
                annotated.push(event);
            }
            Event::Text(_) if !in_code_block => {
                annotated.push(Event::Text(marker.content(vec![range]).into()));
                annotated.push(event);
            }
            Event::Code(text) => {
                literal.push(range.clone());
                // 手前で開いたraw textの要素がcode要素を文字に変える場合も、印がコードと同じ扱いになるよう中に置く。
                let mark = marker.content(vec![range]);
                annotated.push(Event::Code(format!("{mark}{text}").into()));
            }
            Event::InlineHtml(_) => {
                let (begin, end) = marker.tag(vec![range]);
                annotated.push(Event::Text(begin.into()));
                annotated.push(event);
                annotated.push(Event::Text(end.into()));
            }
            _ => annotated.push(event),
        }
    }

    let rendered = texts(&render(annotated.into_iter()));
    let positions = marker.positions(&rendered.text);
    for (index, mark) in marker.marks.iter().enumerate() {
        let shown = match mark.kind {
            MarkKind::Content => positions
                .get(&(index, 'm'))
                .is_some_and(|range| !rendered.hidden_overlaps(range)),
            MarkKind::Tag => match (positions.get(&(index, 's')), positions.get(&(index, 'e'))) {
                (Some(begin), Some(end)) if begin.end <= end.start => {
                    // textareaなどでタグが通常の文字として届く場合は、HTMLの構文として扱わない。
                    let range = begin.end..end.start;
                    rendered.nodes.iter().any(|node| contains(node, &range))
                        && rendered.text[range.clone()].starts_with('<')
                        && !rendered.hidden_overlaps(&range)
                }
                _ => false,
            },
        };
        if !shown {
            protected.extend(mark.spans.iter().cloned());
        }
    }
    Ranges { protected, literal }
}

/// 描画したHTMLから位置を探す印を作る。
struct Marker {
    prefix: String,
    marks: Vec<Mark>,
}

impl Marker {
    fn push(&mut self, kind: MarkKind, spans: Vec<Range<usize>>) -> usize {
        self.marks.push(Mark { kind, spans });
        self.marks.len() - 1
    }

    fn text(&self, index: usize, kind: char) -> String {
        format!("{}{index}{kind}\u{e001}", self.prefix)
    }

    /// spansのテキストの直前に置く印を返す。
    fn content(&mut self, spans: Vec<Range<usize>>) -> String {
        let index = self.push(MarkKind::Content, spans);
        self.text(index, 'm')
    }

    /// spansのタグを挟む、前と後ろの印を返す。
    fn tag(&mut self, spans: Vec<Range<usize>>) -> (String, String) {
        let index = self.push(MarkKind::Tag, spans);
        (self.text(index, 's'), self.text(index, 'e'))
    }

    /// HTMLブロックの行linesを、タグとテキストに分けて印を付け、annotatedに加える。
    ///
    /// 行をまたぐコメントもあるため、ブロックの行をつないでからタグを探す。
    fn html_block<'a>(
        &mut self,
        src: &str,
        lines: Vec<(CowStr<'a>, Range<usize>)>,
        annotated: &mut Vec<Event<'a>>,
        protected: &mut Vec<Range<usize>>,
    ) {
        if lines
            .iter()
            .any(|(text, range)| text.as_ref() != &src[range.clone()])
        {
            // ソースと対応付けられない行は、表示されるかを確かめず保護する。
            for (text, range) in lines {
                protected.push(range);
                annotated.push(Event::Html(text));
            }
            return;
        }
        let content: String = lines.iter().map(|(text, _)| text.as_ref()).collect();
        let tokens: Vec<_> = HTML_TOKEN.find_iter(&content).map(|m| m.range()).collect();
        let mut tags: Vec<Option<(usize, String, String)>> = vec![None; tokens.len()];
        let mut offset = 0;
        for (text, range) in lines {
            let line = offset..offset + text.len();
            let mut cursor = line.start;
            while cursor < line.end {
                let token = tokens
                    .iter()
                    .position(|token| token.end > cursor && token.start < line.end);
                let (piece, tag) = match token {
                    Some(index) if tokens[index].start <= cursor => {
                        (cursor..tokens[index].end.min(line.end), Some(index))
                    }
                    Some(index) => (cursor..tokens[index].start, None),
                    None => (cursor..line.end, None),
                };
                let to_source = |r: &Range<usize>| {
                    range.start + r.start - line.start..range.start + r.end - line.start
                };
                if let Some(index) = tag {
                    let token = &tokens[index];
                    let (mark, begin, end) = if let Some(tag) = &tags[index] {
                        tag.clone()
                    } else {
                        let mark = self.push(MarkKind::Tag, Vec::new());
                        let tag = (mark, self.text(mark, 's'), self.text(mark, 'e'));
                        tags[index] = Some(tag.clone());
                        tag
                    };
                    self.marks[mark].spans.push(to_source(&piece));
                    if piece.start == token.start {
                        annotated.push(Event::Html(begin.into()));
                    }
                    annotated.push(Event::Html(content[piece.clone()].to_owned().into()));
                    if piece.end == token.end {
                        annotated.push(Event::Html(end.into()));
                    }
                } else {
                    // 閉じていないコメントのように、タグとして探せない構文の中に括弧が入ることがあるため、
                    // `[[` と `]]` の直前にも印を置き、括弧ごとに表示されるかを確かめる。
                    let bytes = content.as_bytes();
                    let mut starts = vec![piece.start];
                    starts.extend((piece.start + 1..piece.end).filter(|&i| {
                        bytes[i..].starts_with(b"[[") || bytes[i..].starts_with(b"]]")
                    }));
                    for (k, &start) in starts.iter().enumerate() {
                        let part = start..starts.get(k + 1).copied().unwrap_or(piece.end);
                        let mark = self.content(vec![to_source(&part)]);
                        annotated.push(Event::Html(mark.into()));
                        annotated.push(Event::Html(content[part].to_owned().into()));
                    }
                }
                cursor = piece.end;
            }
            offset = line.end;
        }
    }

    /// 描画後のテキストtextで見つかった印の位置を、印の番号と種類ごとに返す。属性やコメントの中にある印は見つからない。
    fn positions(&self, text: &str) -> HashMap<(usize, char), Range<usize>> {
        let mut found = HashMap::new();
        for (start, _) in text.match_indices(&self.prefix) {
            let rest = &text[start + self.prefix.len()..];
            let digits = rest.bytes().take_while(u8::is_ascii_digit).count();
            let Ok(index) = rest[..digits].parse::<usize>() else {
                continue;
            };
            let mut chars = rest[digits..].chars();
            let (Some(kind), Some('\u{e001}')) = (chars.next(), chars.next()) else {
                continue;
            };
            let end = start + self.prefix.len() + digits + kind.len_utf8() + '\u{e001}'.len_utf8();
            found.entry((index, kind)).or_insert(start..end);
        }
        found
    }
}

/// 描画したHTMLのテキストノードを、文書の順につないだもの。
struct Texts {
    text: String,
    /// textのうち、画面の本文としてWikiリンクを作らない要素の中にある範囲。
    hidden: Vec<Range<usize>>,
    /// textのうち、各テキストノードの範囲。
    nodes: Vec<Range<usize>>,
}

impl Texts {
    fn hidden_overlaps(&self, range: &Range<usize>) -> bool {
        self.hidden.iter().any(|hidden| overlaps(hidden, range))
    }
}

fn texts(dom: &Html) -> Texts {
    let mut texts = Texts {
        text: String::new(),
        hidden: Vec::new(),
        nodes: Vec::new(),
    };
    for node in dom.tree.root().descendants() {
        if let Node::Text(content) = node.value() {
            let start = texts.text.len();
            texts.text.push_str(&content.text);
            let range = start..texts.text.len();
            if node
                .ancestors()
                .filter_map(ElementRef::wrap)
                .any(|element| protected(element.value()))
            {
                texts.hidden.push(range.clone());
            }
            texts.nodes.push(range);
        }
    }
    texts
}

/// Markdownのイベントを描画し、HTML5の構造を復元する。タグの閉じ方が不整合な場合もこの木で判断する。
fn render<'a>(events: impl Iterator<Item = Event<'a>>) -> Html {
    let mut html = String::new();
    pulldown_cmark::html::push_html(&mut html, events);
    Html::parse_fragment(&html)
}

fn contains(outer: &Range<usize>, inner: &Range<usize>) -> bool {
    outer.start <= inner.start && inner.end <= outer.end
}

fn overlaps(a: &Range<usize>, b: &Range<usize>) -> bool {
    a.start < b.end && b.start < a.end
}

/// 画面の本文としてWikiリンクを作らない要素。
fn protected(element: &scraper::node::Element) -> bool {
    match element.name() {
        "a" => element.attr("href").is_some_and(allowed_url),
        "code" | "pre" | "img" | "script" | "style" | "frame" | "frameset" | "iframe"
        | "noembed" | "noframes" | "noscript" | "nostyle" | "object" | "title" => true,
        _ => false,
    }
}

/// 相対URLと、Wikinoでリンクとして表示するスキームを受け付ける。
fn allowed_url(value: &str) -> bool {
    let value = value.trim();
    if value.is_empty() || value.chars().any(char::is_control) {
        return false;
    }
    let first = value.split(['/', '?', '#']).next().unwrap_or("");
    match first.split_once(':') {
        Some((scheme, _)) => ["http", "https", "mailto"]
            .iter()
            .any(|s| scheme.eq_ignore_ascii_case(s)),
        None => true,
    }
}
