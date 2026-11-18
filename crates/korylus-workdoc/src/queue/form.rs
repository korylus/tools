//! 項目の「## 回答」の節にある欄を読み取り、回答と差し戻しの理由を書き込む。
//!
//! 欄は、「## 回答」の節のトップレベルの「### 」の見出しごとに1つある。
//! 欄の中のトップレベルのリストに `X: ` で始まるチェックボックスの項目があれば選択の欄、無ければ自由記述の欄として読む。
//! 引用やリストの項目の中の見出し・選択肢は読まない。

use std::ops::Range;
use std::sync::LazyLock;

use jiff::civil::DateTime;
use pulldown_cmark::{Event, HeadingLevel, Options, Parser, Tag, TagEnd};
use regex::Regex;

use super::{ANSWER_SECTION, one_line, push_item, stamp};
use crate::Error;
use crate::markdown::parse_source;

/// 差し戻しの節の見出し。
const SEND_BACK_SECTION: &str = "差し戻し";

/// 回答の書き手の行の接頭辞。
const WRITER_PREFIX: &str = "回答の書き手:";

/// 選択肢の本文にマッチし、ラベルと残りの本文を取り出す。
static OPTION: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^([A-Z]|[1-9][0-9]*):[\t ]*(.*)$").expect("固定の正規表現を作れなかった")
});

/// HTMLコメントにマッチする。
static COMMENT: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?s)<!--.*?-->").expect("固定の正規表現を作れなかった"));

/// 位置 `index` (0始まり) の選択肢のラベルを返す。26個まではA-Z、それより後は番号にする。
pub(super) fn label(index: usize) -> String {
    match u8::try_from(index) {
        Ok(i) if i < 26 => char::from(b'A' + i).to_string(),
        _ => (index + 1).to_string(),
    }
}

/// 項目の本文から読み取った回答の欄。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Form {
    /// 本文に書かれた順の欄。
    pub fields: Vec<Field>,
    /// 改行をLFに揃えた本文。書き込みでは、ここを書き換えたものを返す。
    src: String,
    /// 「## 回答」の見出しの行の次の位置。
    answer_start: usize,
    /// 「回答の書き手:」の行の範囲 (改行を除く)。
    writer: Option<Range<usize>>,
}

/// 回答の欄1つ。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Field {
    /// 欄の見出し (`### ` を除いたMarkdown)。Wikiリンクの括弧は文字参照で書かれる。
    pub heading: String,
    /// 欄の種類と、今の回答。
    pub kind: FieldKind,
    /// 見出しの行の次から、次の見出しの前までの範囲。
    content: Range<usize>,
}

/// 欄の種類と、今の回答。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FieldKind {
    /// 選択の欄。値は、書かれた順の選択肢。
    Choices(Vec<Choice>),
    /// 自由記述の欄。値は、HTMLコメントを除いて前後の空白を詰めた回答の文。
    Text(String),
}

/// 選択肢1つ。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Choice {
    /// 選択肢のラベル (`A` など)。
    pub label: String,
    /// ラベルを除いた選択肢の本文 (Markdown)。Wikiリンクの括弧は文字参照で書かれる。
    /// 元の選択肢との照合には `matches` を使う。
    pub text: String,
    /// チェックが入っているかどうか。
    pub checked: bool,
    /// チェックボックスの括弧の中の文字の、本文での位置 (バイト)。
    mark: usize,
}

/// 欄1つへの回答。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Response {
    /// 選んだ選択肢の位置 (0始まり)。
    Choice(usize),
    /// 書いた回答の文。
    Text(String),
}

impl Field {
    /// 元の見出しtextと、Markdownとしての表示が同じかを返す。生成時の改行の置き換えと文字参照を考慮する。
    pub fn heading_matches(&self, text: &str) -> bool {
        rendered(&self.heading) == rendered(&one_line(text))
    }
}

impl Choice {
    /// 元の選択肢 `text` と、Markdownとしての表示が同じかを返す。
    ///
    /// 改行は生成時と同じく空白にし、Wikiリンクを無効にする文字参照の有無は区別しない。
    pub fn matches(&self, text: &str) -> bool {
        rendered(&self.text) == rendered(&one_line(text))
    }
}

/// 欄の見出しか選択肢のMarkdownを、文字参照を展開したHTMLに揃える。
fn rendered(text: &str) -> String {
    let mut out = String::new();
    // 行頭の文をブロックの構文として読ませない。
    let src = format!("本文: {text}");
    pulldown_cmark::html::push_html(&mut out, Parser::new(&src));
    scraper::Html::parse_fragment(&out).html()
}

/// トップレベルの見出し1つ。
struct Heading {
    level: u8,
    /// 見出しの文字列 (`#` を除いたMarkdown)。
    text: String,
    /// 見出しの行の始まりの位置。
    start: usize,
    /// 見出しの行の次の位置。
    end: usize,
}

/// 本文から読み取った、トップレベルのブロックの位置。
#[derive(Default)]
struct Outline {
    headings: Vec<Heading>,
    /// トップレベルのリストの項目のうち、チェックボックスで始まるもの (チェックボックスの括弧の位置・チェックの有無)。
    tasks: Vec<(Range<usize>, bool)>,
    /// トップレベルの段落の、1行目の範囲 (改行を除く)。
    paragraphs: Vec<Range<usize>>,
}

/// `src` を解析し、トップレベルの見出し・チェックボックス・段落の位置を返す。
fn outline(src: &str) -> Outline {
    let parse_src = parse_source(src);
    let mut out = Outline::default();
    let mut stack: Vec<TagEnd> = Vec::new();
    let mut heading: Option<(u8, usize, Option<Range<usize>>)> = None;
    for (event, range) in Parser::new_ext(&parse_src, Options::ENABLE_TASKLISTS).into_offset_iter()
    {
        // 見出しの中の要素の範囲をつなぎ、見出しの文字列を元の本文から取る。
        let closing = stack.len() == 1 && matches!(event, Event::End(TagEnd::Heading(_)));
        if !closing && let Some((_, _, text)) = &mut heading {
            *text = Some(match text.take() {
                Some(r) => r.start.min(range.start)..r.end.max(range.end),
                None => range.clone(),
            });
        }
        match event {
            Event::Start(tag) => {
                if stack.is_empty() {
                    match &tag {
                        Tag::Heading { level, .. } => {
                            heading = Some((heading_level(*level), range.start, None));
                        }
                        Tag::Paragraph => {
                            out.paragraphs.push(range.start..line_end(src, range.start));
                        }
                        _ => {}
                    }
                }
                stack.push(tag.to_end());
            }
            Event::End(end) => {
                stack.pop();
                if stack.is_empty()
                    && matches!(end, TagEnd::Heading(_))
                    && let Some((level, start, text)) = heading.take()
                {
                    let text = text.map_or_else(String::new, |r| src[r].trim().to_owned());
                    let end = next_line(src, range.end);
                    out.headings.push(Heading {
                        level,
                        text,
                        start,
                        end,
                    });
                }
            }
            Event::TaskListMarker(checked) => {
                let top_item = matches!(stack.first(), Some(TagEnd::List(_)))
                    && stack.get(1) == Some(&TagEnd::Item)
                    && stack[2..].iter().all(|t| *t == TagEnd::Paragraph);
                if top_item {
                    out.tasks.push((range, checked));
                }
            }
            _ => {}
        }
    }
    out
}

/// 項目の本文 `src` から、「## 回答」の節の欄を読み取る。
pub fn parse(src: &str) -> Result<Form, Error> {
    let src = src.replace("\r\n", "\n");
    let o = outline(&src);

    let answers: Vec<usize> = o
        .headings
        .iter()
        .enumerate()
        .filter(|(_, h)| h.level <= 2 && h.text == ANSWER_SECTION)
        .map(|(i, _)| i)
        .collect();
    let index = match answers.as_slice() {
        [i] => *i,
        [] => {
            return Err(Error::InvalidItem(vec![format!(
                "「## {ANSWER_SECTION}」の節がありません"
            )]));
        }
        _ => {
            return Err(Error::InvalidItem(vec![format!(
                "「## {ANSWER_SECTION}」の節が{}個あります",
                answers.len()
            )]));
        }
    };
    let answer_start = o.headings[index].end;
    let section_end = o.headings[index + 1..]
        .iter()
        .find(|h| h.level <= 2)
        .map_or(src.len(), |h| h.start);

    let fields_in: Vec<&Heading> = o.headings[index + 1..]
        .iter()
        .take_while(|h| h.start < section_end)
        .filter(|h| h.level == 3)
        .collect();
    let first_field = fields_in.first().map_or(section_end, |h| h.start);
    let writer = o
        .paragraphs
        .iter()
        .find(|p| {
            answer_start <= p.start
                && p.end <= first_field
                && src[p.start..p.end].starts_with(WRITER_PREFIX)
        })
        .cloned();

    let mut fields = Vec::new();
    for (i, h) in fields_in.iter().enumerate() {
        let end = fields_in.get(i + 1).map_or(section_end, |next| next.start);
        let content = h.end..end.max(h.end);
        let options: Vec<Choice> = o
            .tasks
            .iter()
            .filter(|(mark, _)| content.contains(&mark.start))
            .filter_map(|(mark, checked)| {
                let text = src[mark.end..line_end(&src, mark.end)].trim();
                let m = OPTION.captures(text)?;
                Some(Choice {
                    label: m[1].to_owned(),
                    text: m[2].trim().to_owned(),
                    checked: *checked,
                    mark: mark.start + 1,
                })
            })
            .collect();
        let kind = if options.is_empty() {
            FieldKind::Text(
                COMMENT
                    .replace_all(&src[content.clone()], "")
                    .trim()
                    .to_owned(),
            )
        } else {
            FieldKind::Choices(options)
        };
        fields.push(Field {
            heading: h.text.clone(),
            kind,
            content,
        });
    }
    Ok(Form {
        fields,
        src,
        answer_start,
        writer,
    })
}

impl Form {
    /// 各欄の回答を返す。
    ///
    /// 選択の欄はちょうど1つにチェックが入っていること、自由記述の欄は回答の文が空でないことを確かめる。
    /// 回答の形の誤りは、差し戻しの理由として人間に見せる文にして返す。
    pub fn answers(&self) -> Result<Vec<Response>, Error> {
        let mut responses = Vec::new();
        let mut errs = Vec::new();
        for field in &self.fields {
            match &field.kind {
                FieldKind::Choices(options) => {
                    let checked: Vec<usize> = options
                        .iter()
                        .enumerate()
                        .filter(|(_, o)| o.checked)
                        .map(|(i, _)| i)
                        .collect();
                    match checked.as_slice() {
                        [i] => responses.push(Response::Choice(*i)),
                        [] => errs.push(format!(
                            "「{}」: 選択肢にチェックがありません (1つにチェックを付けます)",
                            field.heading
                        )),
                        _ => errs.push(format!(
                            "「{}」: {}個の選択肢にチェックがあります (1つだけにします)",
                            field.heading,
                            checked.len()
                        )),
                    }
                }
                FieldKind::Text(text) if text.is_empty() => {
                    errs.push(format!("「{}」: 回答の文がありません", field.heading));
                }
                FieldKind::Text(text) => responses.push(Response::Text(text.clone())),
            }
        }
        if errs.is_empty() {
            Ok(responses)
        } else {
            Err(Error::InvalidAnswer(errs))
        }
    }

    /// 欄ごとの回答 `responses` を書き込み、書き手 `writer` と日時 `at` を記した本文を返す。
    ///
    /// 選択の欄は選んだ選択肢だけにチェックを入れ、自由記述の欄は回答の文で置き換える。
    /// 書き込んだ本文を読み直して、渡した回答と同じに読めることを確かめる。
    pub fn fill(
        &self,
        responses: &[Response],
        writer: &str,
        at: DateTime,
    ) -> Result<String, Error> {
        if responses.len() != self.fields.len() {
            return Err(Error::InvalidAnswer(vec![format!(
                "回答が{}個あります (回答の欄は{}個)",
                responses.len(),
                self.fields.len()
            )]));
        }
        let mut edits: Vec<(Range<usize>, String)> = Vec::new();
        let mut errs = Vec::new();
        // 読み直した回答と比べるため、回答の文は前後の空白を詰めた値にする。
        let mut expected = Vec::new();
        for (field, response) in self.fields.iter().zip(responses) {
            match (&field.kind, response) {
                (FieldKind::Choices(options), Response::Choice(index)) => {
                    if *index >= options.len() {
                        errs.push(format!(
                            "「{}」: {}番目の選択肢はありません (選択肢は{}個)",
                            field.heading,
                            index + 1,
                            options.len()
                        ));
                        continue;
                    }
                    for (i, option) in options.iter().enumerate() {
                        let mark = if i == *index { "x" } else { " " };
                        edits.push((option.mark..option.mark + 1, mark.to_owned()));
                    }
                    expected.push(Response::Choice(*index));
                }
                (FieldKind::Text(_), Response::Text(text)) => {
                    let text = text.trim();
                    if text.is_empty() {
                        errs.push(format!("「{}」: 回答の文がありません", field.heading));
                        continue;
                    }
                    let tail = if field.content.end < self.src.len() {
                        "\n"
                    } else {
                        ""
                    };
                    edits.push((field.content.clone(), format!("\n{text}\n{tail}")));
                    expected.push(Response::Text(text.to_owned()));
                }
                (FieldKind::Choices(_), Response::Text(_)) => {
                    errs.push(format!("「{}」は選択の欄です", field.heading));
                }
                (FieldKind::Text(_), Response::Choice(_)) => {
                    errs.push(format!("「{}」は自由記述の欄です", field.heading));
                }
            }
        }
        if !errs.is_empty() {
            return Err(Error::InvalidAnswer(errs));
        }

        let line = format!("{WRITER_PREFIX} {} ({})", one_line(writer), stamp(at));
        match &self.writer {
            Some(range) => edits.push((range.clone(), line)),
            None => edits.push((self.answer_start..self.answer_start, format!("\n{line}\n"))),
        }
        edits.sort_by_key(|(range, _)| std::cmp::Reverse(range.start));
        let mut out = self.src.clone();
        for (range, text) in edits {
            out.replace_range(range, &text);
        }

        let reread = parse(&out).and_then(|form| form.answers());
        if reread.ok() != Some(expected) {
            return Err(Error::InvalidAnswer(vec![
                "書き込んだ回答を読み直すと、渡した回答と同じに読めません (回答の文には、見出し・選択肢の形の行・HTMLコメントを含められません)".to_owned(),
            ]));
        }
        Ok(out)
    }
}

/// 項目の本文 `src` の先頭に、差し戻しの理由 `reasons` と日時 `at` を書いた「## 差し戻し」の節を置いた本文を返す。
///
/// 前に差し戻したときの節は取り除き、今回の理由だけを残す。
pub fn send_back(src: &str, reasons: &[String], at: DateTime) -> String {
    let src = src.replace("\r\n", "\n");
    let sections: Vec<Heading> = outline(&src)
        .headings
        .into_iter()
        .filter(|h| h.level <= 2)
        .collect();
    let mut rest = src.clone();
    for (i, h) in sections.iter().enumerate().rev() {
        if h.text == SEND_BACK_SECTION {
            let end = sections.get(i + 1).map_or(src.len(), |next| next.start);
            rest.replace_range(h.start..end, "");
        }
    }

    let mut out = format!(
        "## {SEND_BACK_SECTION}\n\nディレクターが、公開された回答を適用できませんでした ({})。\n次の点を直して、もう一度公開してください。\n\n",
        stamp(at)
    );
    for reason in reasons {
        push_item(&mut out, "", reason);
    }
    out = super::literal::escape_wikilinks(&out);
    out.push('\n');
    out.push_str(rest.trim_start_matches('\n'));
    out
}

/// 位置 `offset` を含む行の終わり (改行の位置か本文の末尾) を返す。
fn line_end(src: &str, offset: usize) -> usize {
    src[offset..].find('\n').map_or(src.len(), |i| offset + i)
}

/// 位置 `offset` が行の始まりならそのまま、行の途中ならその行の次の行の始まりを返す。
fn next_line(src: &str, offset: usize) -> usize {
    if offset == 0 || src.as_bytes().get(offset - 1) == Some(&b'\n') {
        offset
    } else {
        (line_end(src, offset) + 1).min(src.len())
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

#[cfg(test)]
mod tests {
    use super::super::tests::{at, item, review};
    use super::super::{Item, Scene};
    use super::*;

    fn parse_ok(src: &str) -> Form {
        parse(src).unwrap_or_else(|err| panic!("項目を読み取れなかった: {err}\n{src}"))
    }

    fn replace(src: &str, from: &str, to: &str) -> String {
        assert!(src.contains(from), "置き換える文字列 {from:?} が無い");
        src.replacen(from, to, 1)
    }

    fn reasons(err: Error) -> Vec<String> {
        match err {
            Error::InvalidAnswer(reasons) | Error::InvalidItem(reasons) => reasons,
            other => panic!("回答か項目の誤りではない: {other:?}"),
        }
    }

    fn question() -> Item {
        let mut it = item(Scene::Question);
        it.details = vec!["リトライの回数を決めてください".to_owned()];
        it
    }

    #[test]
    fn 選択の欄の見出しと選択肢を読み取る() {
        let form = parse_ok(&item(review()).body());

        let headings: Vec<&str> = form.fields.iter().map(|f| f.heading.as_str()).collect();
        assert_eq!(
            headings,
            ["指摘1: エラー時のリトライ回数", "指摘2: `internal/a.go`"],
            "欄の見出しが期待と異なる"
        );
        let FieldKind::Choices(options) = &form.fields[1].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[1].kind);
        };
        let got: Vec<(&str, &str, bool)> = options
            .iter()
            .map(|o| (o.label.as_str(), o.text.as_str(), o.checked))
            .collect();
        assert_eq!(
            got,
            [
                ("A", "slogに変える (推奨)", false),
                ("B", "後続タスクに積む", false),
                ("C", "その他 (下の回答欄に記入)", false),
            ],
            "選択肢が期待と異なる"
        );
    }

    #[test]
    fn 人間がチェックを付けた選択肢を回答として読む() {
        let body = item(review()).body();
        let body = replace(&body, "- [ ] B: リトライしない", "- [x] B: リトライしない");
        let body = replace(&body, "- [ ] C: その他", "- [X] C: その他");

        assert_eq!(
            parse_ok(&body).answers().expect("回答を読み取れなかった"),
            [Response::Choice(1), Response::Choice(2)],
            "回答が期待と異なる"
        );
    }

    #[test]
    fn 詰めずに書いた選択肢やcrlfの本文も読む() {
        let body = item(review()).body().replace(
            "- [ ] A: 3回にする (推奨)\n- [ ] B: リトライしない\n",
            "- [ ] A:3回にする (推奨)\n\n- [x] B: リトライしない\n",
        );
        let body = body.replace('\n', "\r\n");
        let form = parse_ok(&body);

        let FieldKind::Choices(options) = &form.fields[0].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[0].kind);
        };
        assert_eq!(
            options[0].text, "3回にする (推奨)",
            "選択肢の本文が期待と異なる"
        );
        assert!(
            options[1].checked,
            "空行を挟んだ選択肢のチェックを読まなかった"
        );
    }

    #[test]
    fn チェックが無いか複数ある欄を_回答の誤りとする() {
        let body = item(review()).body();
        let body = replace(&body, "- [ ] A: slogに変える", "- [x] A: slogに変える");
        let body = replace(
            &body,
            "- [ ] B: 後続タスクに積む",
            "- [x] B: 後続タスクに積む",
        );
        let err = parse_ok(&body)
            .answers()
            .expect_err("誤った回答を受け付けた");

        assert_eq!(
            reasons(err),
            [
                "「指摘1: エラー時のリトライ回数」: 選択肢にチェックがありません (1つにチェックを付けます)",
                "「指摘2: `internal/a.go`」: 2個の選択肢にチェックがあります (1つだけにします)",
            ],
            "回答の誤りが期待と異なる"
        );
    }

    #[test]
    fn 自由記述の欄の回答の文を_htmlコメントを除いて読む() {
        let body = format!(
            "{}\n3回にしてください。\n<!-- 書きかけのメモ -->\n\n理由は、通信が不安定なためです。\n",
            question().body()
        );
        let form = parse_ok(&body);

        assert_eq!(
            form.answers().expect("回答を読み取れなかった"),
            [Response::Text(
                "3回にしてください。\n\n\n理由は、通信が不安定なためです。".to_owned()
            )],
            "回答が期待と異なる"
        );
    }

    #[test]
    fn 自由記述の欄が空なら_回答の誤りとする() {
        let err = parse_ok(&question().body())
            .answers()
            .expect_err("空の回答を受け付けた");

        assert_eq!(
            reasons(err),
            ["「質問への回答」: 回答の文がありません"],
            "回答の誤りが期待と異なる"
        );
    }

    #[test]
    fn 回答の節が無いか複数ある本文は読み取らない() {
        let body = item(review()).body();
        for (src, want) in [
            (
                body.replace("## 回答", "## 答え"),
                "「## 回答」の節がありません",
            ),
            (format!("{body}\n## 回答\n"), "「## 回答」の節が2個あります"),
        ] {
            let err = parse(&src).expect_err("読み取れない本文を読み取った");
            assert_eq!(reasons(err), [want], "読み取れない理由が期待と異なる");
        }
    }

    #[test]
    fn コードブロックや引用の中の見出しと選択肢は読まない() {
        let body = format!(
            "{}\n```\n### 指摘9: コードの中\n- [x] A: 選択肢\n```\n\n> ### 指摘10: 引用の中\n> - [x] A: 選択肢\n",
            item(review()).body()
        );
        let form = parse_ok(&body);

        assert_eq!(
            form.fields.len(),
            2,
            "欄の数が期待と異なる: {:?}",
            form.fields
        );
        let FieldKind::Choices(options) = &form.fields[1].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[1].kind);
        };
        assert_eq!(options.len(), 3, "選択肢の数が期待と異なる: {options:?}");
    }

    #[test]
    fn 選択の回答を書き込み_書き手と日時を記す() {
        let form = parse_ok(&item(review()).body());
        let responses = [Response::Choice(1), Response::Choice(0)];
        let out = form
            .fill(&responses, "プランナー", at())
            .expect("回答を書き込めなかった");

        assert!(
            out.contains("## 回答\n\n回答の書き手: プランナー (2026-11-17 21:04)\n\n指摘ごとに、"),
            "回答の書き手の行が期待と異なる:\n{out}"
        );
        for want in [
            "- [ ] A: 3回にする (推奨)\n- [x] B: リトライしない",
            "- [x] A: slogに変える (推奨)\n- [ ] B: 後続タスクに積む",
        ] {
            assert!(
                out.contains(want),
                "書き込んだ本文に {want:?} が無い:\n{out}"
            );
        }
        assert_eq!(
            parse_ok(&out)
                .answers()
                .expect("書き込んだ回答を読み取れなかった"),
            responses,
            "書き込んだ回答が期待と異なる"
        );
    }

    #[test]
    fn 書き込み直すと_前の回答と書き手を置き換える() {
        let first = parse_ok(&item(review()).body())
            .fill(
                &[Response::Choice(1), Response::Choice(0)],
                "プランナー",
                at(),
            )
            .expect("回答を書き込めなかった");
        let later = jiff::civil::date(2026, 11, 18).at(9, 30, 0, 0);
        let out = parse_ok(&first)
            .fill(&[Response::Choice(0), Response::Choice(2)], "人間", later)
            .expect("回答を書き込み直せなかった");

        assert_eq!(
            out.matches(WRITER_PREFIX).count(),
            1,
            "回答の書き手の行が1つではない:\n{out}"
        );
        assert!(
            out.contains("回答の書き手: 人間 (2026-11-18 09:30)"),
            "回答の書き手が置き換わっていない:\n{out}"
        );
        assert_eq!(
            parse_ok(&out)
                .answers()
                .expect("書き込んだ回答を読み取れなかった"),
            [Response::Choice(0), Response::Choice(2)],
            "書き込み直した回答が期待と異なる"
        );
    }

    #[test]
    fn 自由記述の回答を書き込み_書き込み直すと置き換える() {
        let form = parse_ok(&question().body());
        let first = form
            .fill(
                &[Response::Text("3回にしてください".to_owned())],
                "プランナー",
                at(),
            )
            .expect("回答を書き込めなかった");

        assert!(
            first.ends_with("### 質問への回答\n\n3回にしてください\n"),
            "回答の文の書き方が期待と異なる:\n{first}"
        );
        let text = "リトライしないでください。\n\n- 通信の失敗はすぐに知らせる".to_owned();
        let out = parse_ok(&first)
            .fill(&[Response::Text(text.clone())], "プランナー", at())
            .expect("回答を書き込み直せなかった");

        assert!(
            !out.contains("3回にしてください"),
            "前の回答が残った:\n{out}"
        );
        assert_eq!(
            parse_ok(&out)
                .answers()
                .expect("書き込んだ回答を読み取れなかった"),
            [Response::Text(text)],
            "書き込み直した回答が期待と異なる"
        );
    }

    #[test]
    fn 自由記述の欄の後ろに欄があっても_次の見出しを残して書き込む() {
        let body = format!("{}\n### メモ\n\n- [ ] A: 残す\n", question().body());
        let out = parse_ok(&body)
            .fill(
                &[Response::Text("回答".to_owned()), Response::Choice(0)],
                "プランナー",
                at(),
            )
            .expect("回答を書き込めなかった");

        assert!(
            out.contains("### 質問への回答\n\n回答\n\n### メモ\n\n- [x] A: 残す\n"),
            "書き込んだ本文が期待と異なる:\n{out}"
        );
    }

    #[test]
    fn 欄と合わない回答は書き込まない() {
        let form = parse_ok(&item(review()).body());
        let cases = [
            (
                vec![Response::Choice(0)],
                vec!["回答が1個あります (回答の欄は2個)"],
            ),
            (
                vec![Response::Choice(2), Response::Text("文".to_owned())],
                vec![
                    "「指摘1: エラー時のリトライ回数」: 3番目の選択肢はありません (選択肢は2個)",
                    "「指摘2: `internal/a.go`」は選択の欄です",
                ],
            ),
        ];
        for (responses, want) in cases {
            let err = form
                .fill(&responses, "プランナー", at())
                .expect_err("欄と合わない回答を書き込んだ");
            assert_eq!(reasons(err), want, "書き込めない理由が期待と異なる");
        }

        let form = parse_ok(&question().body());
        for (response, want) in [
            (Response::Choice(0), "「質問への回答」は自由記述の欄です"),
            (
                Response::Text(" \n".to_owned()),
                "「質問への回答」: 回答の文がありません",
            ),
        ] {
            let err = form
                .fill(&[response], "プランナー", at())
                .expect_err("欄と合わない回答を書き込んだ");
            assert_eq!(reasons(err), [want], "書き込めない理由が期待と異なる");
        }
    }

    #[test]
    fn 見出しや選択肢の形の行を含む回答の文は書き込まない() {
        let form = parse_ok(&question().body());
        for text in ["前半\n\n## 後半", "前半\n\n### 別の欄", "- [x] A: 選択肢"] {
            let err = form
                .fill(&[Response::Text(text.to_owned())], "プランナー", at())
                .expect_err("読み直せない回答を書き込んだ");
            assert!(
                reasons(err)[0].contains("読み直すと"),
                "{text:?} を書き込めない理由が期待と異なる"
            );
        }
    }

    #[test]
    fn 差し戻しの理由を本文の先頭に書く() {
        let body = item(review()).body();
        let out = send_back(
            &body,
            &["「指摘1: エラー時のリトライ回数」: 選択肢にチェックがありません".to_owned()],
            at(),
        );

        let want = "## 差し戻し

ディレクターが、公開された回答を適用できませんでした (2026-11-17 21:04)。
次の点を直して、もう一度公開してください。

- 「指摘1: エラー時のリトライ回数」: 選択肢にチェックがありません

## 状況
";
        assert!(out.starts_with(want), "差し戻しの節が期待と異なる:\n{out}");
        assert!(
            out.ends_with(&body),
            "差し戻しの節より後ろの本文が変わった:\n{out}"
        );
        assert_eq!(
            parse_ok(&out).fields.len(),
            2,
            "差し戻したあとに欄を読み取れない"
        );
    }

    #[test]
    fn 差し戻し直すと_前の理由を置き換える() {
        let body = item(review()).body();
        let first = send_back(&body, &["前の理由".to_owned()], at());
        let out = send_back(&first, &["今回の理由".to_owned()], at());

        assert_eq!(
            out.matches("## 差し戻し").count(),
            1,
            "差し戻しの節が1つではない:\n{out}"
        );
        assert!(!out.contains("前の理由"), "前の理由が残った:\n{out}");
        assert!(out.contains("- 今回の理由\n"), "今回の理由が無い:\n{out}");
        assert!(
            out.ends_with(&body),
            "差し戻しの節より後ろの本文が変わった:\n{out}"
        );
    }

    #[test]
    fn 差し戻し理由のwikiリンクだけを文字列にして_回答を保つ() {
        let body = format!("{}\n[[回答で参照するページ]]\n", question().body());
        let out = send_back(
            &body,
            &["[[エラーの詳細]] を確認してください".to_owned()],
            at(),
        );

        assert!(
            out.contains("- &#91;[エラーの詳細]] を確認してください"),
            "差し戻し理由を文字列にしていない:\n{out}"
        );
        assert!(out.ends_with(&body), "既存の回答を書き換えた:\n{out}");
        assert_eq!(
            parse_ok(&out).answers().expect("回答を読み取れなかった"),
            [Response::Text("[[回答で参照するページ]]".to_owned())],
            "元の回答が違う"
        );
    }

    #[test]
    fn 文字参照やコードを含む選択肢を元のmarkdownと照合できる() {
        let body = concat!(
            "## 回答\n\n### 指摘\n\n",
            "- [ ] A: **&#91;[計画書]]** と `[[コード]]` と &amp; を確認する\n",
        );
        let form = parse_ok(body);
        let FieldKind::Choices(options) = &form.fields[0].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[0].kind);
        };

        assert!(
            options[0].matches("**[[計画書]]** と `[[コード]]`\nと &amp; を確認する"),
            "元のMarkdownと一致しない"
        );
        assert!(
            !options[0].matches("[[計画書]] と `[[コード]]` と &amp; を確認する"),
            "強調を消した選択肢と一致した"
        );
        assert!(
            !options[0].matches("**[[別の計画書]]** と `[[コード]]` と &amp; を確認する"),
            "本文が違う選択肢と一致した"
        );
    }

    #[test]
    fn 選択肢のラベルを位置から決める() {
        assert_eq!(
            [label(0), label(25), label(26)],
            ["A", "Z", "27"],
            "ラベルが期待と異なる"
        );
    }
}
