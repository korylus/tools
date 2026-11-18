//! `/korylus-review` が書いたレビュードキュメントを読み取り、指摘ごとの区分と推奨をもとに対応方針へ決定的に回答する。
//!
//! 読み取るのは総合評価と、「ファイルごとのレビュー結果」「設計改善の提案」「過去レビューの修正検証」の各節にある「**対応方針**:」の選択肢に限る。
//! テンプレートの書式の例はHTMLコメントの中にあるため、ブロックの木でコメントとコードブロックを読み飛ばして構造を辿る。

mod answer;
mod tree;

use std::fmt;
use std::io;
use std::path::Path;
use std::sync::LazyLock;

use regex::Regex;

pub use answer::{Answers, Decision};

use crate::Error;
use crate::quote::quote;
use tree::{Kind, Tree};

/// レビュードキュメントを置く、プロジェクトのルートからの相対パス。
pub const DIR: &str = "docs/private/reviews";

/// 指摘を読み取る節の名前。
const SECTION_FINDINGS: &str = "ファイルごとのレビュー結果";
const SECTION_DESIGN: &str = "設計改善の提案";
const SECTION_VERIFICATION: &str = "過去レビューの修正検証";
const SECTION_SUMMARY: &str = "総合評価";

/// 推奨の選択肢の末尾に付く印。
const RECOMMENDED_MARK: &str = "(推奨)";

/// 固定の正規表現を作る。
///
/// パターンはこのモジュールの定数で、どれもテストで一度は使うため、作れないのは実装の誤りに限る。
fn regex(pattern: &str) -> Regex {
    Regex::new(pattern).expect("固定の正規表現を作れなかった")
}

// 正規表現の空白は、移植元のGoの `\s` と同じくASCIIの空白 (`[\t\n\f\r ]`) に限る。

static CATEGORY: LazyLock<Regex> =
    LazyLock::new(|| regex(r"^\*\*区分\*\*:[\t\n\f\r ]*([^\t\n\f\r ]+)"));
static VERDICT: LazyLock<Regex> = LazyLock::new(|| regex(r"^\*\*評価\*\*:[\t\n\f\r ]*(.*)$"));
/// GoのEqualFoldと同じ単純ケースフォールドで、評価全体を比較する。
static VERDICT_VALUES: LazyLock<[(Regex, Verdict); 3]> = LazyLock::new(|| {
    [Verdict::Approve, Verdict::RequestChanges, Verdict::Comment].map(|verdict| {
        (
            regex(&format!(r"(?i)\A{}\z", regex::escape(verdict.as_str()))),
            verdict,
        )
    })
});
static OPTION: LazyLock<Regex> = LazyLock::new(|| regex(r"^\[([ xX])\][\t\n\f\r ]*(.*)$"));
/// コードを変えない選択肢 (後続タスクに積む・現状維持・取り下げなど) にマッチする。
static DEFER: LazyLock<Regex> = LazyLock::new(|| {
    regex(
        r"^(後続タスクに積む|現状維持|現状のまま|指摘を取り下げる|今回は見送る|対応しない)([\t\n\f\r ]*\([^()\n]*\))*$",
    )
});

/// レビューの総合評価。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    /// 指摘が1件も無い。
    Approve,
    /// 必須の指摘があり、修正のあとに別のモデルが再レビューする。
    RequestChanges,
    /// 必須の指摘は無く、再レビューをせずに終える。
    Comment,
}

impl Verdict {
    /// レビュードキュメントに書く表記を返す。
    pub fn as_str(self) -> &'static str {
        match self {
            Verdict::Approve => "Approve",
            Verdict::RequestChanges => "Request Changes",
            Verdict::Comment => "Comment",
        }
    }
}

impl fmt::Display for Verdict {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// 指摘の区分。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Category {
    /// 推奨の選択肢をそのまま回答に選んでよい指摘。
    AutoFix,
    /// 人間が判断するまで止める指摘。
    NeedsHuman,
    /// どちらでもない値。回答では、区分を読み取れない指摘として止める。
    Other(String),
}

impl Category {
    fn parse(value: &str) -> Self {
        match value {
            "自動対応可" => Category::AutoFix,
            "要人間判断" => Category::NeedsHuman,
            other => Category::Other(other.to_owned()),
        }
    }

    /// レビュードキュメントに書く表記を返す。
    pub fn as_str(&self) -> &str {
        match self {
            Category::AutoFix => "自動対応可",
            Category::NeedsHuman => "要人間判断",
            Category::Other(value) => value,
        }
    }
}

/// レビュードキュメントから読み取った内容。
#[derive(Debug, Clone)]
pub struct Document {
    /// 「総合評価」節の「**評価**:」の行に書かれた総合評価。
    pub verdict: Verdict,
    /// 「**対応方針**:」を持つ指摘と、対応方針が欠けた指摘。
    pub findings: Vec<Finding>,
    /// 改行をLFに揃えた本文。回答では、ここにチェックを入れたものを返す。
    src: String,
}

/// 対応方針の選択肢を持つ指摘1件。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Finding {
    /// 指摘がある節の名前。
    pub section: String,
    /// 指摘の見出し (再検討提案は、その項目の1行目)。
    pub title: String,
    /// 区分。書かれていなければ `None`。
    pub category: Option<Category>,
    /// 「過去レビューの修正検証」の再検討提案かどうか。
    pub proposal: bool,
    /// 対応方針の選択肢。書かれた順に並ぶ。
    /// 対応方針が欠けているか、チェックボックスの選択肢が無ければ空。
    pub options: Vec<Choice>,
    /// 「**対応方針**:」の行番号 (1始まり)。欠けていれば、欠落を見つけた区分・見出し・項目の行番号。
    pub line_no: usize,
    /// 指摘の本文 (Markdown)。見出し (再検討提案とリストの項目の指摘は、項目) から「**対応方針**:」の前までを、
    /// リストの項目の字下げと前後の空白を除いて返す。対応方針が欠けた指摘では空。
    pub summary: String,
}

/// 対応方針の選択肢1つ。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Choice {
    /// 「(推奨)」を含む選択肢の本文。
    pub text: String,
    /// チェックボックスにチェックが入っているかどうか。
    pub checked: bool,
    /// 本文の末尾に「(推奨)」が付いているかどうか。
    pub recommended: bool,
    /// チェックボックスの括弧の中の文字の、本文での位置 (バイト)。
    mark: usize,
}

impl Choice {
    /// この選択肢が、コードを変えずに指摘を先送りするものかを返す。
    pub fn defers(&self) -> bool {
        DEFER.is_match(&self.text)
    }
}

/// 1つの見出し (再検討提案は1つの項目) の中で読んだ区分の状態。
#[derive(Debug, Clone, Default)]
struct Group {
    /// 最後に読んだ区分。
    category: Option<Category>,
    /// まだ対応方針が続いていない区分の行番号。無ければ0。
    pending: usize,
}

/// `src` をレビュードキュメントとして読み取る。
#[expect(
    clippy::too_many_lines,
    reason = "移植元のGoの `Parse` と1対1で見比べて、読み取りの規則が同じことを確かめるため"
)]
pub fn parse(src: &str) -> Result<Document, Error> {
    let src = src.replace("\r\n", "\n");
    let tree = Tree::parse(&src);
    let mut reader = Reader {
        tree: &tree,
        src: &src,
        findings: Vec::new(),
        text_start: 0,
    };

    let mut errs = Vec::new();
    let (mut section, mut title) = (String::new(), String::new());
    let mut g = Group::default();
    let mut verdict = None;
    let mut verdicts = 0;
    let (mut group_start, mut group_line) = (0, 1);

    for &id in tree.root_children() {
        let kind = tree.node(id).kind;
        if let Kind::Heading(level) = kind {
            if level == 2 || level == 3 {
                reader.flush(&section, &title, &g, group_start, group_line);
                group_start = reader.findings.len();
                group_line = tree.start_line(id);
                reader.text_start = tree.end(id);
            }
            match level {
                2 => {
                    section = tree.line_text(id);
                    title.clear();
                    g = Group::default();
                }
                3 => {
                    title = tree.line_text(id);
                    g = Group::default();
                }
                _ => {}
            }
            continue;
        }
        if kind == Kind::Paragraph && section == SECTION_SUMMARY {
            for line in tree.lines(id) {
                let Some(m) = VERDICT.captures(&line.text) else {
                    continue;
                };
                verdicts += 1;
                match parse_verdict(&m[1]) {
                    Ok(v) => verdict = Some(v),
                    Err(err) => errs.push(format!("{}行目: {err}", line.no)),
                }
            }
            continue;
        }
        match section.as_str() {
            SECTION_FINDINGS | SECTION_DESIGN => {
                if kind == Kind::List {
                    reader.collect_list(id, &section, &title, &mut g);
                } else {
                    reader.collect(id, &section, &title, &mut g, false);
                }
            }
            SECTION_VERIFICATION => {
                // 再検討提案は、修正検証の項目 (トップレベルのリスト項目) ごとに区分と対応方針を持つ。
                if kind != Kind::List {
                    continue;
                }
                for &item in &tree.node(id).children {
                    let mut ig = Group::default();
                    let start = reader.findings.len();
                    let item_title = tree
                        .node(item)
                        .children
                        .first()
                        .map_or_else(String::new, |&c| tree.line_text(c));
                    // 項目の1行目は見出しとして読むため、本文は2行目から始める。
                    if let Some(start) = tree.start_offset(item) {
                        reader.text_start = reader.line_end(start);
                    }
                    reader.collect(item, &section, &item_title, &mut ig, true);
                    if ig.pending > 0 {
                        reader.add_missing(
                            &section,
                            &item_title,
                            ig.category.as_ref(),
                            true,
                            ig.pending,
                        );
                    } else if reader.findings.len() == start && item_title.contains("再検討提案")
                    {
                        let line = tree.start_line(item);
                        reader.add_missing(&section, &item_title, ig.category.as_ref(), true, line);
                    }
                }
            }
            _ => {}
        }
    }
    reader.flush(&section, &title, &g, group_start, group_line);

    if verdicts == 0 {
        errs.push(format!(
            "「{SECTION_SUMMARY}」節に「**評価**:」の行がありません"
        ));
    } else if verdicts > 1 {
        errs.push(format!(
            "「{SECTION_SUMMARY}」節に「**評価**:」の行が{verdicts}個あります"
        ));
    }
    match verdict {
        Some(verdict) if errs.is_empty() => Ok(Document {
            verdict,
            findings: reader.findings,
            src,
        }),
        _ => Err(Error::InvalidReview(errs)),
    }
}

/// レビュードキュメントのブロックの木を辿り、指摘を集める。
struct Reader<'a> {
    tree: &'a Tree<'a>,
    src: &'a str,
    findings: Vec<Finding>,
    /// 次に読む指摘の本文が始まる、文書での位置 (バイト)。
    text_start: usize,
}

impl Reader<'_> {
    /// 見出しの境界で、対応方針の欠落を確かめる。
    /// 対応方針の欠落を指摘なしと取り違えないよう、見出しを読み終えるたびに確かめる。
    fn flush(
        &mut self,
        section: &str,
        title: &str,
        g: &Group,
        group_start: usize,
        group_line: usize,
    ) {
        if section != SECTION_FINDINGS && section != SECTION_DESIGN {
            return;
        }
        if g.pending > 0 {
            self.add_missing(section, title, g.category.as_ref(), false, g.pending);
        } else if (!title.is_empty() || g.category.is_some()) && self.findings.len() == group_start
        {
            self.add_missing(section, title, g.category.as_ref(), false, group_line);
        }
    }

    /// `id` の中を文書の順に辿り、「**区分**:」を `g` に残し、「**対応方針**:」の直後のリストを指摘として加える。
    ///
    /// 区分は同じ見出しの中で最後に書かれたものを、そのあとの対応方針に当てる。
    /// 対応方針が続かないまま次の区分が書かれたら、前の区分の指摘を対応方針の欠落として加える。
    fn collect(&mut self, id: usize, section: &str, title: &str, g: &mut Group, proposal: bool) {
        let tree = self.tree;
        match tree.node(id).kind {
            Kind::CodeBlock | Kind::HtmlBlock => return,
            Kind::Paragraph => {}
            _ => {
                for &child in &tree.node(id).children {
                    self.collect(child, section, title, g, proposal);
                }
                return;
            }
        }
        // 空行を挟まずに続けて書いた「**区分**:」と「**対応方針**:」は1つの段落になるため、行ごとに見る。
        for line in tree.lines(id) {
            if let Some(m) = CATEGORY.captures(&line.text) {
                if g.pending > 0 {
                    self.add_missing(section, title, g.category.as_ref(), proposal, g.pending);
                }
                g.category = Some(Category::parse(&m[1]));
                g.pending = line.no;
                continue;
            }
            if !line.text.starts_with("**対応方針**:") {
                continue;
            }
            g.pending = 0;
            let list = self.next_list(id);
            let options = list.map(|list| self.options(list)).unwrap_or_default();
            let summary = self.summary(line.start);
            self.findings.push(Finding {
                section: section.to_owned(),
                title: title.to_owned(),
                category: g.category.clone(),
                proposal,
                options,
                line_no: line.no,
                summary,
            });
            // 同じ見出しの中で続く指摘の本文は、この指摘の選択肢と回答の欄のあとから始める。
            self.text_start =
                list.map_or_else(|| self.line_end(line.start), |list| self.after_answer(list));
        }
    }

    /// 指摘の節のトップレベルのリストを項目ごとに辿る。
    ///
    /// テンプレートは1つの見出しの下に問題点を項目として並べるため、対応方針を持つ項目が1つでもあれば、
    /// 対応方針を持たない項目も対応方針の欠落として加える。
    fn collect_list(&mut self, list: usize, section: &str, title: &str, g: &mut Group) {
        let tree = self.tree;
        let mut empty = Vec::new();
        let mut found = false;
        for &item in &tree.node(list).children {
            let (start, before) = (self.findings.len(), g.pending);
            let text_start = self.text_start;
            // 項目の中の指摘の本文は、項目の1行目から始める。
            if let Some(offset) = tree.start_offset(item) {
                self.text_start = offset;
            }
            self.collect(item, section, title, g, false);
            if self.findings.len() > start {
                found = true;
                continue;
            }
            // 指摘の無い項目 (チェックしたガイドラインの一覧など) は、見出しから続く本文の始まりを変えない。
            self.text_start = text_start;
            if g.pending > 0 && g.pending != before {
                // 項目の中で区分を書いたのに対応方針が続かなかった。
                self.add_missing(section, title, g.category.as_ref(), false, g.pending);
                g.pending = 0;
            } else {
                empty.push(item);
            }
        }
        if !found {
            return;
        }
        for item in empty {
            let line = tree.start_line(item);
            self.add_missing(section, title, g.category.as_ref(), false, line);
        }
    }

    /// 対応方針が欠けた指摘を、選択肢の無い指摘として加える。回答では、回答できない指摘として止める。
    fn add_missing(
        &mut self,
        section: &str,
        title: &str,
        category: Option<&Category>,
        proposal: bool,
        line_no: usize,
    ) {
        self.findings.push(Finding {
            section: section.to_owned(),
            title: title.to_owned(),
            category: category.cloned(),
            proposal,
            options: Vec::new(),
            line_no,
            summary: String::new(),
        });
    }

    /// 位置offsetを含む行の、次の行の先頭の位置を返す。
    fn line_end(&self, offset: usize) -> usize {
        self.src[offset..]
            .find('\n')
            .map_or(self.src.len(), |i| offset + i + 1)
    }

    /// 指摘の本文が始まる位置から、「**対応方針**:」の行の位置endの前までを、指摘の本文として返す。
    fn summary(&self, end: usize) -> String {
        let text = self.src.get(self.text_start..end).unwrap_or_default();
        dedent(text).trim().to_owned()
    }

    /// リストの項目のうち、チェックボックスで始まるものを選択肢として返す。
    fn options(&self, list: usize) -> Vec<Choice> {
        let tree = self.tree;
        let mut options = Vec::new();
        for &item in &tree.node(list).children {
            let Some(&block) = tree.node(item).children.first() else {
                continue;
            };
            let Some(first) = tree.lines(block).into_iter().next() else {
                continue;
            };
            let text = tree.block_text(block);
            let Some(m) = OPTION.captures(&text) else {
                continue;
            };
            if !self.src[first.start..].starts_with('[') {
                continue;
            }
            let body = m[2].trim().to_owned();
            options.push(Choice {
                checked: &m[1] != " ",
                recommended: body.ends_with(RECOMMENDED_MARK),
                text: body,
                mark: first.start + 1,
            });
        }
        options
    }

    /// 段落 `id` のあとに続くリストを返す。
    /// 間のHTMLコメント (「開発者が回答を記入してください」など) は読み飛ばす。
    fn next_list(&self, id: usize) -> Option<usize> {
        for &sibling in self.tree.next_siblings(id) {
            match self.tree.node(sibling).kind {
                Kind::HtmlBlock => {}
                Kind::List => return Some(sibling),
                _ => return None,
            }
        }
        None
    }

    /// 選択肢のリスト `list` のあとの回答の欄 (「**回答**:」の段落と、続くコードブロック) の終わりの位置を返す。
    /// 回答の欄が無ければ、リストの終わりの位置を返す。
    ///
    /// テンプレートは選択肢のあとに回答の欄を置くため、続く指摘の本文に前の指摘の回答の欄を混ぜないよう飛ばす。
    fn after_answer(&self, list: usize) -> usize {
        let tree = self.tree;
        let mut end = tree.end(list);
        let mut answer = false;
        for &sibling in tree.next_siblings(list) {
            match tree.node(sibling).kind {
                Kind::HtmlBlock => {}
                Kind::Paragraph if !answer && tree.line_text(sibling).starts_with("**回答**:") => {
                    answer = true;
                    end = tree.end(sibling);
                }
                Kind::CodeBlock if answer => return tree.end(sibling),
                _ => break,
            }
        }
        end
    }
}

/// textの2行目以降に共通する字下げを除く。
///
/// リストの項目の本文は、1行目が項目の印のあとから始まり、2行目以降が項目の字下げの分だけ下がっているためである。
fn dedent(text: &str) -> String {
    let mut lines = text.split('\n');
    let mut out = lines.next().unwrap_or_default().to_owned();
    let rest: Vec<&str> = lines.collect();
    let indent = rest
        .iter()
        .filter(|line| !line.trim().is_empty())
        .map(|line| line.len() - line.trim_start_matches(' ').len())
        .min()
        .unwrap_or(0);
    for line in rest {
        out.push('\n');
        out.push_str(line.get(indent..).unwrap_or_default());
    }
    out
}

/// 「**評価**:」の値を総合評価にする。
fn parse_verdict(s: &str) -> Result<Verdict, String> {
    let v = s.trim().trim_matches(['*', '`']);
    VERDICT_VALUES
        .iter()
        .find(|(pattern, _)| pattern.is_match(v))
        .map(|(_, verdict)| *verdict)
        .ok_or_else(|| {
            format!(
                "評価 {} を読み取れません (Approve / Request Changes / Commentのどれか1つを書きます)",
                quote(s)
            )
        })
}

/// `root` のレビュードキュメントの置き場 (`DIR`) から、レビュー接頭辞 `prefix` のドキュメント
/// (`{prefix}-{3桁以上の連番}.md`) のうち連番が最大のものを、ルートからの相対パスで返す。
///
/// 置き場が無いか、該当するドキュメントが無ければ `None` を返す。
pub fn latest(root: &Path, prefix: &str) -> io::Result<Option<String>> {
    let entries = match std::fs::read_dir(root.join(DIR)) {
        Ok(entries) => entries,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err),
    };
    let mut best: Option<(usize, String)> = None;
    for entry in entries {
        let entry = entry?;
        if entry.file_type()?.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let Some(n) = sequence_of(prefix, &name) else {
            continue;
        };
        // 連番が同じ (桁数違いで同じ番号) なら、名前の順で後ろのものを選ぶ。
        // ディレクトリの読み取り順に左右されず、名前の順に読む移植元のGoと同じ結果になる。
        if best
            .as_ref()
            .is_none_or(|(max, best_name)| (n, &name) > (*max, best_name))
        {
            best = Some((n, name));
        }
    }
    Ok(best.map(|(_, name)| format!("{DIR}/{name}")))
}

/// レビュー接頭辞 `prefix` のドキュメントのパス `doc` から連番を返す。
/// ファイル名が `{prefix}-{3桁以上の連番}.md` の形でなければ `None` を返す。
pub fn sequence(prefix: &str, doc: &str) -> Option<usize> {
    let name = doc.trim_end_matches('/').rsplit('/').next().unwrap_or(doc);
    sequence_of(prefix, name)
}

/// ファイル名 `name` が `{prefix}-{3桁以上の連番}.md` の形なら、その連番を返す。
fn sequence_of(prefix: &str, name: &str) -> Option<usize> {
    let digits = name
        .strip_prefix(prefix)?
        .strip_prefix('-')?
        .strip_suffix(".md")?;
    if digits.len() < 3 || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return None;
    }
    // Goのstrconv.Atoiが受け付ける、符号付きのintの範囲に揃える。
    digits
        .parse::<isize>()
        .ok()
        .and_then(|n| usize::try_from(n).ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 無人実行モードのレビュードキュメントの例。テンプレートのHTMLコメントの中の書式の例は読み取らない。
    pub(super) const TEST_DOC: &str = r"# コードレビュー: p-1-1

<!--
**問題点のフォーマット**:

**区分**: 要人間判断

**対応方針**:

- [ ] コメントの中の選択肢 (推奨)
-->

## 過去レビューの修正検証

- [x] [p-1-1-001.md](./p-1-1-001.md) 「ログの出力」: 対応済み
- [ ] [p-1-1-001.md](./p-1-1-001.md) 「エラーの握りつぶし」: 再検討提案

  **根拠**: 呼び出し元で処理されている

  **区分**: 要人間判断

  **対応方針**:
  - [ ] 指摘を取り下げる
  - [ ] 決定済みの方針どおり修正する (推奨)

## ファイルごとのレビュー結果

### 1. ゲートの失敗を差し戻さない

**ステータス**: 要修正

**区分**: 自動対応可

**対応方針**:

<!-- 開発者が回答を記入してください -->
- [ ] 修正案の通り差し戻す (推奨)
- [ ] その他 (下の回答欄に記入)

**回答**:

```
**対応方針**:
- [ ] コードブロックの中の選択肢 (推奨)
```

### `internal/a.go`

**区分**: 要人間判断

**問題点・改善提案**:

- **[go-coding.md#ログ]**: log.Printfを使っている

  **対応方針**:
  - [ ] slogに変える (推奨)
  - [ ] その他 (下の回答欄に記入)

## 設計改善の提案

### `internal/b.go`: 関数を分ける

**区分**: 自動対応可
**対応方針**:

- [ ] 提案通り変更する
- [ ] 後続タスクに積む (推奨)

## 保護領域の照合

| 保護領域 | 観点 | 結果 | 根拠 |
| -------- | ---- | ---- | ---- |
| schema   | ロックの長さ | ok | 列の追加だけ |

## 総合評価

**評価**: Request Changes

**次のアクション**: 修正後に別モデルへ再レビューを依頼
";

    /// 総合評価の節だけを持つ文書に、指摘の節 `section` と本文 `body` を足す。
    pub(super) fn doc_with(section: &str, body: &str) -> String {
        format!("## {section}\n\n{body}\n\n## 総合評価\n\n**評価**: Comment\n")
    }

    pub(super) fn parse_ok(src: &str) -> Document {
        parse(src).unwrap_or_else(|err| panic!("読み取れなかった: {err}"))
    }

    #[test]
    fn 総合評価と対応方針の選択肢を持つ指摘を読み取る() {
        let d = parse_ok(TEST_DOC);
        assert_eq!(d.verdict, Verdict::RequestChanges, "総合評価が期待と異なる");

        let want = [
            (
                SECTION_VERIFICATION,
                "[ ] [p-1-1-001.md](./p-1-1-001.md) 「エラーの握りつぶし」: 再検討提案",
                Some(Category::NeedsHuman),
                true,
                2,
            ),
            (
                SECTION_FINDINGS,
                "1. ゲートの失敗を差し戻さない",
                Some(Category::AutoFix),
                false,
                2,
            ),
            (
                SECTION_FINDINGS,
                "`internal/a.go`",
                Some(Category::NeedsHuman),
                false,
                2,
            ),
            (
                SECTION_DESIGN,
                "`internal/b.go`: 関数を分ける",
                Some(Category::AutoFix),
                false,
                2,
            ),
        ];
        assert_eq!(
            d.findings.len(),
            want.len(),
            "指摘の数が期待と異なる: {:?}",
            d.findings
        );
        for (f, (section, title, category, proposal, options)) in d.findings.iter().zip(want) {
            assert_eq!(
                (
                    f.section.as_str(),
                    f.title.as_str(),
                    &f.category,
                    f.proposal,
                    f.options.len()
                ),
                (section, title, &category, proposal, options),
                "指摘が期待と異なる"
            );
        }

        let options = &d.findings[1].options;
        assert!(
            options[0].recommended,
            "推奨の選択肢を推奨として読まなかった"
        );
        assert!(
            !options[1].recommended,
            "推奨でない選択肢を推奨として読んだ"
        );
        assert_eq!(
            options[0].text, "修正案の通り差し戻す (推奨)",
            "選択肢の本文が期待と異なる"
        );

        let (before, _) = TEST_DOC
            .split_once("**区分**: 自動対応可\n**対応方針**:")
            .expect("設計改善の提案の区分が無い");
        assert_eq!(
            d.findings[3].line_no,
            before.matches('\n').count() + 2,
            "段落の2行目の対応方針の行番号が期待と異なる"
        );
    }

    #[test]
    fn 指摘の本文を見出しか項目から対応方針の前まで読み取る() {
        let d = parse_ok(TEST_DOC);
        let summaries: Vec<&str> = d.findings.iter().map(|f| f.summary.as_str()).collect();
        assert_eq!(
            summaries,
            [
                "**根拠**: 呼び出し元で処理されている\n\n**区分**: 要人間判断",
                "**ステータス**: 要修正\n\n**区分**: 自動対応可",
                "**[go-coding.md#ログ]**: log.Printfを使っている",
                "**区分**: 自動対応可",
            ]
        );
    }

    #[test]
    fn 見出しの下の一覧や続く指摘を前の指摘の本文に混ぜない() {
        // 回答の欄は、無い・テンプレートどおりのコードブロック・段落に直接書いた回答、のいずれでも本文に混ぜない。
        for answer in [
            "",
            "**回答**:\n\n```\n(ここに回答を記入)\n```\n\n",
            "**回答**: 選択肢のとおりに直す\n\n",
        ] {
            let src = doc_with(
                SECTION_FINDINGS,
                &format!(
                    "### 1. 二つの問題\n\n\
                     **チェックしたガイドライン**:\n\n\
                     - [Rustのテスト](/rules/testing.md)\n\n\
                     1つ目の問題の説明。\n  字下げした続き。\n\n\
                     **区分**: 要人間判断\n\n\
                     **対応方針**:\n\n\
                     - [ ] 直す (推奨)\n\n\
                     {answer}\
                     2つ目の問題の説明。\n\n\
                     **区分**: 要人間判断\n\n\
                     **対応方針**:\n\n\
                     - [ ] 直す (推奨)\n\n\
                     {answer}"
                ),
            );
            let d = parse_ok(&src);
            let summaries: Vec<&str> = d.findings.iter().map(|f| f.summary.as_str()).collect();
            assert_eq!(
                summaries,
                [
                    "**チェックしたガイドライン**:\n\n- [Rustのテスト](/rules/testing.md)\n\n1つ目の問題の説明。\n  字下げした続き。\n\n**区分**: 要人間判断",
                    "2つ目の問題の説明。\n\n**区分**: 要人間判断",
                ],
                "回答の欄 {answer:?} で、本文の範囲が違う"
            );
        }
    }

    #[test]
    fn 総合評価を読み取る() {
        let tests = [
            ("Approve", "**評価**: Approve\n", Ok(Verdict::Approve)),
            (
                "Comment",
                "**評価**: Comment\n\n**総評**: 良い\n",
                Ok(Verdict::Comment),
            ),
            (
                "太字の評価",
                "**評価**: **Request Changes**\n",
                Ok(Verdict::RequestChanges),
            ),
            (
                "大文字と小文字を区別しない",
                "**評価**: request changes\n",
                Ok(Verdict::RequestChanges),
            ),
            (
                "単純ケースフォールドで一致する文字",
                "**評価**: Requeſt Changeſ\n",
                Ok(Verdict::RequestChanges),
            ),
            (
                "似ていても一致しない文字",
                "**評価**: Apprоve\n",
                Err("読み取れません"),
            ),
            (
                "評価の後ろの改行以外の文字を許さない",
                "**評価**: Approve\\\n",
                Err("読み取れません"),
            ),
            (
                "テンプレートのまま",
                "**評価**: [Approve / Request Changes / Comment]\n",
                Err("読み取れません"),
            ),
            (
                "評価が無い",
                "**総評**: 良い\n",
                Err("「**評価**:」の行がありません"),
            ),
            (
                "評価が2つある",
                "**評価**: Approve\n\n**評価**: Comment\n",
                Err("2個あります"),
            ),
            (
                "行の先頭をエスケープした行は評価にしない",
                "\\**評価**: Comment\n",
                Err("「**評価**:」の行がありません"),
            ),
        ];
        for (name, summary, want) in tests {
            let got = parse(&format!("# レビュー\n\n## 総合評価\n\n{summary}"));
            match (got, want) {
                (Ok(d), Ok(verdict)) => {
                    assert_eq!(d.verdict, verdict, "{name}: 総合評価が期待と異なる");
                }
                (Err(err), Err(message)) => assert!(
                    err.to_string().contains(message),
                    "{name}: エラー = {err}、含まれることを期待した文字列 = {message:?}"
                ),
                (got, want) => panic!("{name}: 結果 = {got:?}、期待値 = {want:?}"),
            }
        }
    }

    #[test]
    fn 読み取れない評価をgoと同じ形で引用する() {
        let err = parse("## 総合評価\n\n**評価**: \"不明\"\n").expect_err("読み取れてしまった");
        assert_eq!(
            err.to_string(),
            "3行目: 評価 \"\\\"不明\\\"\" を読み取れません (Approve / Request Changes / Commentのどれか1つを書きます)",
            "エラーが期待と異なる"
        );
    }

    #[test]
    fn 対応方針の欠落を選択肢の無い指摘にする() {
        let tests = [
            (
                "要人間判断",
                SECTION_FINDINGS,
                "### 人間の判断\n\n**区分**: 要人間判断\n\n方針を選ぶ必要がある。",
            ),
            (
                "自動対応可",
                SECTION_FINDINGS,
                "### エラー処理\n\n**区分**: 自動対応可\n\nエラーを返す必要がある。",
            ),
            (
                "設計改善",
                SECTION_DESIGN,
                "### 構造の見直し\n\n**区分**: 要人間判断",
            ),
            (
                "区分も欠落",
                SECTION_FINDINGS,
                "### エラー処理\n\nエラーを返す必要がある。",
            ),
            (
                "見出しが欠落",
                SECTION_FINDINGS,
                "**区分**: 要人間判断\n\n方針を選ぶ必要がある。",
            ),
            (
                "再検討提案",
                SECTION_VERIFICATION,
                "- [ ] 以前の指摘: 再検討提案\n\n  **区分**: 要人間判断\n\n  **根拠**: 前提が変わった。",
            ),
            (
                "再検討提案の区分も欠落",
                SECTION_VERIFICATION,
                "- [ ] 以前の指摘: 再検討提案\n\n  **根拠**: 前提が変わった。",
            ),
        ];
        for (name, section, body) in tests {
            let d = parse_ok(&doc_with(section, body));
            let a = d.answer();
            assert!(
                d.findings.len() == 1 && a.blocked() && a.invalid.len() == 1,
                "{name}: 対応方針の欠落を検出できなかった: 指摘 = {:?}、回答 = {a:?}",
                d.findings
            );
            assert!(
                d.findings[0].line_no >= 1,
                "{name}: 指摘の行番号 = {}",
                d.findings[0].line_no
            );
        }
    }

    #[test]
    fn 次の指摘で対応方針の欠落を隠さない() {
        let d = parse_ok(&doc_with(
            SECTION_FINDINGS,
            "### 対応方針の欠落\n\n**区分**: 要人間判断\n\n### 正常な指摘\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)",
        ));
        let a = d.answer();
        assert!(
            a.blocked() && a.invalid.len() == 1 && a.decisions.len() == 1,
            "次の指摘で欠落が隠れた: {a:?}"
        );
    }

    #[test]
    fn リストの項目の対応方針の欠落を検出する() {
        use std::fmt::Write as _;

        let item = |label: &str, category: &str, policy: &str| {
            let mut s = format!("- **[{label}]**: 問題点\n\n");
            if !category.is_empty() {
                let _ = write!(s, "  **区分**: {category}\n\n");
            }
            if !policy.is_empty() {
                let _ = write!(
                    s,
                    "  **対応方針**:\n\n  <!-- 開発者が回答を記入してください -->\n  - [ ] {policy} (推奨)\n  - [ ] その他 (下の回答欄に記入)\n\n"
                );
            }
            s + "  **修正案**: 直す\n\n"
        };
        let tests = [
            // テンプレートどおり、見出しに区分を1つ書き、問題点を項目として並べる形。
            (
                "見出しに区分",
                format!(
                    "**区分**: 自動対応可\n\n**問題点・改善提案**:\n\n{}{}",
                    item("規約", "", "修正する"),
                    item("規約", "", "")
                ),
            ),
            // 問題点ごとに区分を書く形。対応方針の無い要人間判断を素通りさせない。
            (
                "項目ごとに区分",
                format!(
                    "**問題点・改善提案**:\n\n{}{}",
                    item("規約", "自動対応可", "修正する"),
                    item("設計", "要人間判断", "")
                ),
            ),
            (
                "欠落した項目が先",
                format!(
                    "**問題点・改善提案**:\n\n{}{}",
                    item("設計", "要人間判断", ""),
                    item("規約", "自動対応可", "修正する")
                ),
            ),
        ];
        for (name, body) in tests {
            let src = format!(
                "## ファイルごとのレビュー結果\n\n### `a.go`: 問題\n\n**ステータス**: 要修正\n\n{body}## 総合評価\n\n**評価**: Comment\n"
            );
            let d = parse_ok(&src);
            let a = d.answer();
            assert!(
                d.findings.len() == 2
                    && a.blocked()
                    && a.invalid.len() == 1
                    && a.decisions.len() == 1,
                "{name}: 項目の対応方針の欠落を検出できなかった: 指摘 = {:?}、回答 = {a:?}",
                d.findings
            );
        }
    }

    #[test]
    fn 対応方針を持たない項目だけのリストは指摘にしない() {
        let d = parse_ok(&doc_with(
            SECTION_FINDINGS,
            concat!(
                "### `a.go`: 問題\n\n**区分**: 自動対応可\n\n**チェックしたガイドライン**:\n\n- 規約A\n- 規約B\n\n",
                "**修正案**:\n\n- こう直す\n- テストを足す\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\n- [ ] その他 (下の回答欄に記入)"
            ),
        ));
        let a = d.answer();
        assert!(
            d.findings.len() == 1 && !a.blocked() && a.needs_fix(),
            "指摘 = {:?}、回答 = {a:?}",
            d.findings
        );
    }

    #[test]
    fn crlfの改行の文書を読み取る() {
        let d = parse_ok(&TEST_DOC.replace('\n', "\r\n"));
        assert_eq!(d.findings.len(), 4, "指摘の数が期待と異なる");
        assert!(
            !d.answer().output.contains('\r'),
            "回答の本文にCRが残っている"
        );
    }

    #[test]
    fn 最新のレビュードキュメントを連番で選ぶ() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        assert_eq!(
            latest(root.path(), "p-1-1").expect("置き場が無いときに失敗した"),
            None,
            "置き場が無いのにドキュメントを返した"
        );

        let dir = root.path().join(DIR);
        std::fs::create_dir_all(&dir).expect("置き場を作れなかった");
        for name in [
            "p-1-1-001.md",
            "p-1-1-010.md",
            "p-1-1-002.md",
            "p-1-10-099.md",
            "p-1-1-003.txt",
            "p-1-1-abc.md",
        ] {
            std::fs::write(dir.join(name), "").expect("ドキュメントを作れなかった");
        }
        std::fs::create_dir(dir.join("p-1-1-999.md")).expect("ディレクトリを作れなかった");

        assert_eq!(
            latest(root.path(), "p-1-1")
                .expect("最新のドキュメントを選べなかった")
                .as_deref(),
            Some("docs/private/reviews/p-1-1-010.md"),
            "最新のドキュメントが期待と異なる"
        );

        // 桁数違いで同じ番号の連番は、作った順にかかわらず名前の順で後ろのものを選ぶ。
        for names in [
            ["p-2-1-0001.md", "p-2-1-001.md", "p-2-1-00001.md"],
            ["p-2-1-001.md", "p-2-1-00001.md", "p-2-1-0001.md"],
        ] {
            let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
            let dir = root.path().join(DIR);
            std::fs::create_dir_all(&dir).expect("置き場を作れなかった");
            for name in names {
                std::fs::write(dir.join(name), "").expect("ドキュメントを作れなかった");
            }
            assert_eq!(
                latest(root.path(), "p-2-1")
                    .expect("最新のドキュメントを選べなかった")
                    .as_deref(),
                Some("docs/private/reviews/p-2-1-001.md"),
                "同じ番号の連番の選び方が期待と異なる ({names:?})"
            );
        }
    }

    #[test]
    fn ドキュメントのパスから連番を読む() {
        let tests = [
            (
                "置き場からのパスの連番を読む",
                "docs/private/reviews/p-1-1-002.md",
                Some(2),
            ),
            ("4桁以上の連番を読む", "p-1-1-1000.md", Some(1000)),
            ("接頭辞が違えば読まない", "p-1-10-099.md", None),
            ("3桁に満たない連番は読まない", "p-1-1-02.md", None),
            ("数字でない連番は読まない", "p-1-1-abc.md", None),
            ("Markdownでなければ読まない", "p-1-1-003.txt", None),
            ("末尾のスラッシュを除く", "p-1-1-002.md/", Some(2)),
            ("末尾の複数のスラッシュを除く", "p-1-1-002.md//", Some(2)),
            (
                "末尾のドットはファイル名として扱う",
                "p-1-1-002.md/./",
                None,
            ),
        ];
        for (name, doc, want) in tests {
            assert_eq!(sequence("p-1-1", doc), want, "{name}: 連番が期待と異なる");
        }
    }

    #[test]
    fn 連番の範囲をgoのintに揃える() {
        let max = isize::MAX as usize;
        for (digits, want) in [
            (max.to_string(), Some(max)),
            ((max as u128 + 1).to_string(), None),
            (u128::MAX.to_string(), None),
        ] {
            assert_eq!(
                sequence("p", &format!("p-{digits}.md")),
                want,
                "連番の範囲が異なる: {digits}"
            );
        }
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let dir = root.path().join(DIR);
        std::fs::create_dir_all(&dir).expect("置き場を作れなかった");
        for name in ["p-001.md".to_owned(), format!("p-{}.md", max as u128 + 1)] {
            std::fs::write(dir.join(name), "").expect("ドキュメントを作れなかった");
        }
        assert_eq!(
            latest(root.path(), "p")
                .expect("最新のドキュメントを選べなかった")
                .as_deref(),
            Some("docs/private/reviews/p-001.md"),
            "範囲外の連番を選んだ"
        );
    }
}
