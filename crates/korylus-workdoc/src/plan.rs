//! 作業計画書のタスクリストを読み取る。
//!
//! 読み取るのは「## タスクリスト」節のトップレベルのタスク行と、その下の
//! `**担当**:` / `**保護領域**:` / `**想定行数**:` の行と、最初の節より前の
//! `**計画書名**:` / `**ベースブランチ**:` の行に限る。
//! HTMLコメントとコードブロックの中は、書式の例を誤って拾わないよう読み飛ばす。

mod sections;
mod snapshot;

use std::collections::{BTreeMap, HashMap, HashSet};
use std::num::IntErrorKind;
use std::sync::LazyLock;

use pulldown_cmark::{Event, Parser, Tag, TagEnd};
use regex::Regex;

pub use sections::section;
pub use snapshot::{Violation, check_feedback_phase, check_task_changes};

use crate::Error;
use crate::markdown::parse_source;
use crate::quote::quote;

/// タスクを読み取る節の見出し。
const SECTION_TITLE: &str = "## タスクリスト";

/// フィードバック対応のフェーズ番号の接頭辞 (fb1, fb2...)。
const FEEDBACK_PREFIX: &str = "fb";

/// 固定の正規表現を作る。
///
/// パターンはこのモジュールの定数で、どれもテストで一度は使うため、作れないのは実装の誤りに限る。
fn regex(pattern: &str) -> Regex {
    Regex::new(pattern).expect("固定の正規表現を作れなかった")
}

// 正規表現の空白は、移植元のGoの `\s` と同じくASCIIの空白 (`[\t\n\f\r ]`) に限る。
// Rustの `\s` はUnicodeの空白 (全角スペースなど) にも当たり、読み取りの結果が変わるため。

/// トップレベルのタスク行にマッチし、チェック・タスク番号・残りの本文を取り出す。
static TASK: LazyLock<Regex> = LazyLock::new(|| {
    regex(r"^- \[([ xX])\] \*\*([0-9A-Za-z]+(?:-[0-9A-Za-z]+)+)\*\*:[\t\n\f\r ]*(.*)$")
});

/// タスク名の先頭のプラットフォーム接頭辞 ([Go] など) にマッチする。
static PLATFORM: LazyLock<Regex> = LazyLock::new(|| regex(r"^\[([^\]]+)\][\t\n\f\r ]*(.*)$"));

/// タスクの下の `**名前**: 値` の行にマッチする。
static FIELD: LazyLock<Regex> =
    LazyLock::new(|| regex(r"^[\t\n\f\r ]+- \*\*([^*]+)\*\*:[\t\n\f\r ]*(.*)$"));

/// 想定行数の合計 (約N行) にマッチする。
static TOTAL: LazyLock<Regex> = LazyLock::new(|| regex(r"^約?([0-9,]+)行"));

/// 想定行数の内訳 (実装N行) にマッチする。
static PART: LazyLock<Regex> =
    LazyLock::new(|| regex(r"([^\t\n\f\r (+]+?)[\t\n\f\r ]*([0-9,]+)行"));

/// フィードバック対応のフェーズ番号にマッチする。
static FEEDBACK_PHASE: LazyLock<Regex> =
    LazyLock::new(|| regex(&format!("^{FEEDBACK_PREFIX}([0-9]+)$")));

/// 最初の節より前の `**計画書名**:` の行にマッチし、値を取り出す。
static NAME_LINE: LazyLock<Regex> =
    LazyLock::new(|| regex(r"^\*\*計画書名\*\*:[\t\n\f\r ]*(.*?)[\t\n\f\r ]*$"));

/// 計画書名に使える値にマッチする。
/// ブランチ名とパスの一部になるため、英小文字・数字と、間に挟むハイフンだけにする。
static NAME: LazyLock<Regex> = LazyLock::new(|| regex(r"^[a-z0-9]+(?:-[a-z0-9]+)*$"));

/// 最初の節より前の `**ベースブランチ**:` の行にマッチし、値を取り出す。
static BASE_LINE: LazyLock<Regex> =
    LazyLock::new(|| regex(r"^\*\*ベースブランチ\*\*:[\t\n\f\r ]*(.*?)[\t\n\f\r ]*$"));

/// 作業計画書から読み取った内容。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Plan {
    /// 本文の `**計画書名**:` の行に書かれた計画書名。行が無ければ空文字。
    pub name: String,
    /// 本文の `**ベースブランチ**:` の行に書かれた、リポジトリごとのベースの宣言。行が無ければ空。
    pub bases: Vec<BaseBranch>,
    /// 計画書に書かれた順のタスク。
    pub tasks: Vec<Task>,
}

/// 1つのタスク。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Task {
    /// タスク番号 (1-1, 2-0-1, 5a-1, fb1-1など)。
    pub id: String,
    /// チェックボックスにチェックが入っているかどうか。
    pub done: bool,
    /// プラットフォーム接頭辞 (Go / Railsなど)。無ければ空文字。
    pub platform: String,
    /// プラットフォーム接頭辞を除いたタスク名。
    pub title: String,
    /// タスクの下の `**担当**:` の行に書かれた担当。行が無ければ空文字。
    pub assignee: String,
    /// 宣言された保護領域。宣言が無ければ空。
    pub protected: Vec<Declaration>,
    /// 想定行数。`**想定行数**:` の行が無ければ `None`。
    pub lines: Option<Size>,
    /// タスク行の行番号 (1始まり)。
    pub line_no: usize,
}

/// `**ベースブランチ**:` の行に書いたベースの宣言1件。
///
/// パスは字句の上で整えるだけにし、リポジトリを求める処理はgitを呼ぶ側に任せる。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BaseBranch {
    /// 宣言したディレクトリの絶対パス。
    pub dir: String,
    /// `dir` を含むリポジトリの、最初のタスクのブランチを作る元のブランチ。
    pub branch: String,
}

/// 保護領域の宣言1件。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Declaration {
    /// 保護領域の名前。
    pub name: String,
    /// 括弧内に書かれた触れる内容。無ければ空文字。
    pub note: String,
}

/// 想定行数。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Size {
    /// 合計の行数。
    pub total: isize,
    /// 内訳 (実装・テスト・ドキュメントなど) ごとの行数。
    pub parts: BTreeMap<String, isize>,
}

impl Task {
    /// タスクが属するフェーズ番号 (タスク番号の先頭の区切り) を返す。
    pub fn phase(&self) -> &str {
        self.id
            .split_once('-')
            .map_or(self.id.as_str(), |(phase, _)| phase)
    }

    /// フィードバック対応のフェーズの回 (fbNのN) を返す。
    /// フィードバック対応のタスクでなければ `None` を返す。
    pub fn feedback_round(&self) -> Option<isize> {
        FEEDBACK_PHASE.captures(self.phase())?[1].parse().ok()
    }
}

impl Plan {
    /// 最初の未完了のタスクを返す。すべて完了していれば `None` を返す。
    pub fn next(&self) -> Option<&Task> {
        self.tasks.iter().find(|task| !task.done)
    }

    /// 次に追加するフィードバック対応のフェーズの回 (既存のフェーズfbNの最大のN + 1) を返す。
    pub fn next_feedback_round(&self) -> isize {
        self.tasks
            .iter()
            .filter_map(Task::feedback_round)
            .max()
            .unwrap_or(0)
            .wrapping_add(1)
    }

    /// 完了したタスクの数を返す。
    pub fn done_count(&self) -> usize {
        self.tasks.iter().filter(|task| task.done).count()
    }
}

/// `src` を作業計画書として読み取る。
#[expect(
    clippy::too_many_lines,
    reason = "移植元のGoの `Parse` と1対1で見比べて、読み取りの規則が同じことを確かめるため"
)]
pub fn parse(src: &str) -> Result<Plan, Error> {
    let src = src.replace("\r\n", "\n");
    let lines: Vec<&str> = src.split('\n').collect();
    let in_code = code_lines(&lines);

    let mut plan = Plan::default();
    let mut errs = Vec::new();
    let mut seen: HashMap<String, usize> = HashMap::new();
    let mut assignee_lines: HashMap<String, usize> = HashMap::new();
    let (mut in_section, mut in_comment, mut before_sections) = (false, false, true);
    let (mut name_line_no, mut base_line_no) = (0, 0);
    // 読んでいるタスクの、plan.tasksでの添字。
    let mut cur: Option<usize> = None;

    for (i, &line) in lines.iter().enumerate() {
        let line_no = i + 1;

        if in_code[i] {
            continue;
        }
        if in_comment {
            if line.contains("-->") {
                in_comment = false;
            }
            continue;
        }
        if let Some(start) = line.find("<!--")
            && !line[start..].contains("-->")
        {
            in_comment = true;
            continue;
        }

        if line.starts_with("## ") {
            in_section = line.trim() == SECTION_TITLE;
            before_sections = false;
            cur = None;
            continue;
        }
        if before_sections {
            if let Some(m) = NAME_LINE.captures(line) {
                let value = &m[1];
                if name_line_no > 0 {
                    errs.push(format!(
                        "{line_no}行目: 計画書名の行が重複しています ({name_line_no}行目と同じ)"
                    ));
                } else if !NAME.is_match(value) {
                    errs.push(format!(
                        "{line_no}行目: 計画書名 {} には、英小文字・数字と、間に挟むハイフンだけを使ってください",
                        quote(value)
                    ));
                } else {
                    value.clone_into(&mut plan.name);
                }
                name_line_no = line_no;
            }
            if let Some(m) = BASE_LINE.captures(line) {
                if base_line_no > 0 {
                    errs.push(format!(
                        "{line_no}行目: ベースブランチの行が重複しています ({base_line_no}行目と同じ)"
                    ));
                } else {
                    match parse_bases(&m[1]) {
                        Ok(bases) => plan.bases = bases,
                        Err(problems) => errs.push(format!(
                            "{line_no}行目: ベースブランチ: {}",
                            problems.join("\n")
                        )),
                    }
                }
                base_line_no = line_no;
            }
            continue;
        }
        if !in_section {
            continue;
        }

        if let Some(m) = TASK.captures(line) {
            let mut task = Task {
                id: m[2].to_owned(),
                done: &m[1] != " ",
                title: m[3].trim().to_owned(),
                line_no,
                ..Task::default()
            };
            if let Some(pm) = PLATFORM.captures(&task.title) {
                let (platform, title) = (pm[1].to_owned(), pm[2].to_owned());
                task.platform = platform;
                task.title = title;
            }
            if let Some(prev) = seen.insert(task.id.clone(), line_no) {
                errs.push(format!(
                    "{line_no}行目: タスク番号 {} が重複しています ({prev}行目と同じ)",
                    task.id
                ));
            }
            plan.tasks.push(task);
            cur = Some(plan.tasks.len() - 1);
            continue;
        }
        let Some(index) = cur else {
            continue;
        };
        // インデントの無い行が来たら、そのタスクの記述は終わっている。
        if !line.is_empty() && !line.starts_with(' ') && !line.starts_with('\t') {
            cur = None;
            continue;
        }

        let Some(m) = FIELD.captures(line) else {
            continue;
        };
        let task = &mut plan.tasks[index];
        match &m[1] {
            "担当" => {
                let value = m[2].trim();
                if let Some(&prev) = assignee_lines.get(&task.id) {
                    errs.push(format!(
                        "{line_no}行目: タスク {} の担当の行が重複しています ({prev}行目と同じ)",
                        task.id
                    ));
                } else if value.is_empty() {
                    errs.push(format!("{line_no}行目: タスク {} の担当が空です", task.id));
                } else {
                    value.clone_into(&mut task.assignee);
                }
                assignee_lines.insert(task.id.clone(), line_no);
            }
            "保護領域" => match parse_declarations(&m[2]) {
                Ok(decls) => task.protected.extend(decls),
                Err(problem) => errs.push(format!(
                    "{line_no}行目: タスク {} の保護領域: {problem}",
                    task.id
                )),
            },
            "想定行数" => match parse_size(&m[2]) {
                Ok(size) => task.lines = Some(size),
                Err(problem) => {
                    errs.push(format!(
                        "{line_no}行目: タスク {} の想定行数: {problem}",
                        task.id
                    ));
                    task.lines = None;
                }
            },
            _ => {}
        }
    }

    if !errs.is_empty() {
        return Err(Error::InvalidPlan(errs));
    }
    if plan.tasks.is_empty() {
        return Err(Error::NoTasks);
    }
    Ok(plan)
}

/// `src` を行に分けた `lines` のうち、コードブロックの中身の行を返す。
///
/// フェンスの種類・長さと、リスト項目の中でのインデントの数え方はパーサーで判定する。
/// フェンスの行自体はタスク行にもフィールドの行にも当たらないため、中身の行だけを返す。
fn code_lines(lines: &[&str]) -> Vec<bool> {
    let mut starts = Vec::with_capacity(lines.len());
    let mut offset = 0;
    for line in lines {
        starts.push(offset);
        offset += line.len() + 1;
    }
    let line_at = |offset: usize| starts.partition_point(|&start| start <= offset) - 1;

    let src = parse_source(&lines.join("\n"));

    let mut in_code = vec![false; lines.len()];
    let mut depth = 0usize;
    for (event, range) in Parser::new(&src).into_offset_iter() {
        match event {
            Event::Start(Tag::CodeBlock(_)) => depth += 1,
            Event::End(TagEnd::CodeBlock) => depth = depth.saturating_sub(1),
            Event::Text(_) if depth > 0 && range.start < range.end => {
                for flag in &mut in_code[line_at(range.start)..=line_at(range.end - 1)] {
                    *flag = true;
                }
            }
            _ => {}
        }
    }
    in_code
}

/// 「<ディレクトリのパス>=<ブランチ名>, ...」の形のベースの宣言を読む。
/// 誤りがあれば、項目ごとの誤りをすべて返す。
fn parse_bases(s: &str) -> Result<Vec<BaseBranch>, Vec<String>> {
    let mut bases = Vec::new();
    let mut errs = Vec::new();
    let mut seen = HashSet::new();
    for item in s.split(", ") {
        let item = item.trim();
        let (dir, branch) = item.split_once('=').unwrap_or((item, ""));
        let dir = clean_path(dir.trim());
        let branch = branch.trim();
        if !item.contains('=') {
            errs.push(format!(
                "「<ディレクトリのパス>=<ブランチ名>」の形で書いてください: {}",
                quote(item)
            ));
        } else if !dir.starts_with('/') {
            errs.push(format!(
                "パスは / で始まる絶対パスにしてください: {}",
                quote(item)
            ));
        } else if branch.is_empty() {
            errs.push(format!("ブランチ名が空です: {}", quote(item)));
        } else if !seen.insert(dir.clone()) {
            errs.push(format!("パス {dir} が重複しています"));
        } else {
            bases.push(BaseBranch {
                dir,
                branch: branch.to_owned(),
            });
        }
    }
    if errs.is_empty() {
        Ok(bases)
    } else {
        Err(errs)
    }
}

/// パスを字句の上で整える (Goの `filepath.Clean` と同じ規則)。
///
/// 続く `/` と `.` を除き、`..` を手前の要素と打ち消し、末尾の `/` を除く。空なら `.` を返す。
fn clean_path(path: &str) -> String {
    let rooted = path.starts_with('/');
    let mut parts: Vec<&str> = Vec::new();
    for part in path.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if parts.last().is_some_and(|last| *last != "..") {
                    parts.pop();
                } else if !rooted {
                    parts.push("..");
                }
            }
            _ => parts.push(part),
        }
    }
    let joined = parts.join("/");
    if rooted {
        format!("/{joined}")
    } else if joined.is_empty() {
        ".".to_owned()
    } else {
        joined
    }
}

/// 「名前 (触れる内容), 名前 (触れる内容)」の形の宣言を読む。
/// 触れる内容にカンマが入ることがあるため、括弧の外のカンマだけで区切る。
fn parse_declarations(s: &str) -> Result<Vec<Declaration>, String> {
    let mut items = Vec::new();
    let (mut depth, mut start) = (0isize, 0);
    for (i, c) in s.char_indices() {
        match c {
            '(' => depth += 1,
            ')' => depth -= 1,
            ',' if depth == 0 => {
                items.push(&s[start..i]);
                start = i + 1;
            }
            _ => {}
        }
    }
    if depth != 0 {
        return Err(format!("括弧が閉じていません: {}", quote(s)));
    }
    items.push(&s[start..]);

    let mut decls = Vec::with_capacity(items.len());
    for item in items {
        let item = item.trim();
        let (name, note) = match item.split_once('(') {
            Some((name, note)) => (name, Some(note)),
            None => (item, None),
        };
        let name = name.trim();
        if name.is_empty() {
            return Err(format!("保護領域の名前がありません: {}", quote(item)));
        }
        decls.push(Declaration {
            name: name.to_owned(),
            note: note
                .map(|note| {
                    let note = note.trim();
                    note.strip_suffix(')').unwrap_or(note).trim().to_owned()
                })
                .unwrap_or_default(),
        });
    }
    Ok(decls)
}

/// 「約N行 (実装N行 + テストN行)」の形の想定行数を読む。
fn parse_size(s: &str) -> Result<Size, String> {
    let Some(m) = TOTAL.captures(s.trim()) else {
        return Err(format!("「約N行」の形で書いてください: {}", quote(s)));
    };
    let mut size = Size {
        total: atoi(&m[1]),
        parts: BTreeMap::new(),
    };
    if let Some((_, breakdown)) = s.split_once('(') {
        for pm in PART.captures_iter(breakdown) {
            let part = size.parts.entry(pm[1].to_owned()).or_default();
            *part = part.wrapping_add(atoi(&pm[2]));
        }
    }
    Ok(size)
}

/// 桁区切りのカンマを含む数字を整数にする。
///
/// 呼び出し元の正規表現で数字とカンマだけであることを確かめている。
/// Goの `strconv.Atoi` のエラーを無視した戻り値と揃え、範囲外なら最大値、空なら0にする。
fn atoi(s: &str) -> isize {
    s.replace(',', "")
        .parse()
        .unwrap_or_else(|err: std::num::ParseIntError| {
            if *err.kind() == IntErrorKind::PosOverflow {
                isize::MAX
            } else {
                0
            }
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 計画書テンプレートの書式に沿った計画書の例。
    /// タスクリスト節の外と、HTMLコメント・コードブロックの中にあるタスク行は読み取らない。
    const SAMPLE_PLAN: &str = concat!(
        "# 例の作業計画書\n",
        "\n",
        "## 設計\n",
        "\n",
        "- [ ] **9-1**: [Go] 設計節のタスク行は読まない\n",
        "\n",
        "## タスクリスト\n",
        "\n",
        "<!--\n",
        "- [ ] **1-1**: [Go] コメントの中の例は読まない\n",
        "-->\n",
        "\n",
        "### フェーズ1: 基盤\n",
        "\n",
        "- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n",
        "  - サブタスク\n",
        "  - **保護領域**: schema (posts に visibility 列を追加), module-dependency (usecase → repository の新しい依存)\n",
        "  - **想定ファイル数**: 約6ファイル (実装4 + テスト2)\n",
        "  - **想定行数**: 約300行 (実装150行 + テスト150行)\n",
        "\n",
        "- [ ] **2-0-1**: 接頭辞の無いタスク\n",
        "  - **想定行数**: 約1,050行 (実装400行 + ドキュメント200行 + テスト450行)\n",
        "\n",
        "```markdown\n",
        "- [ ] **8-1**: [Go] コードブロックの中の例は読まない\n",
        "```\n",
        "\n",
        "### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n",
        "\n",
        "- [X] **fb1-1**: [Rails] 保存後に編集画面へ戻す\n",
        "  - **想定行数**: 約30行\n",
        "  - **対応するフィードバック**: 1回目「保存できない」\n",
        "\n",
        "## 恒久判断記録への反映\n",
        "\n",
        "- [ ] **7-1**: [Go] 後ろの節のタスク行は読まない\n",
    );

    const TASKS: &str = "## タスクリスト\n\n- [ ] **1-1**: タスク\n";

    fn size(total: isize, parts: &[(&str, isize)]) -> Size {
        Size {
            total,
            parts: parts
                .iter()
                .map(|(name, lines)| ((*name).to_owned(), *lines))
                .collect(),
        }
    }

    fn declaration(name: &str, note: &str) -> Declaration {
        Declaration {
            name: name.to_owned(),
            note: note.to_owned(),
        }
    }

    fn base(dir: &str, branch: &str) -> BaseBranch {
        BaseBranch {
            dir: dir.to_owned(),
            branch: branch.to_owned(),
        }
    }

    fn task_ids(plan: &Plan) -> Vec<&str> {
        plan.tasks.iter().map(|task| task.id.as_str()).collect()
    }

    /// 読み取りに失敗したことを確かめ、エラーのメッセージを返す。
    fn parse_error(src: &str) -> String {
        match parse(src) {
            Ok(plan) => panic!("エラーが返らなかった: {plan:?}"),
            Err(err) => err.to_string(),
        }
    }

    #[test]
    fn タスクリスト節のタスクだけを読み取る() {
        let plan = parse(SAMPLE_PLAN).expect("読み取れなかった");

        let want = vec![
            Task {
                id: "1-1".to_owned(),
                done: true,
                platform: "Go".to_owned(),
                title: "投稿の公開範囲を保存する".to_owned(),
                protected: vec![
                    declaration("schema", "posts に visibility 列を追加"),
                    declaration("module-dependency", "usecase → repository の新しい依存"),
                ],
                lines: Some(size(300, &[("実装", 150), ("テスト", 150)])),
                line_no: 15,
                ..Task::default()
            },
            Task {
                id: "2-0-1".to_owned(),
                title: "接頭辞の無いタスク".to_owned(),
                lines: Some(size(
                    1050,
                    &[("実装", 400), ("ドキュメント", 200), ("テスト", 450)],
                )),
                line_no: 21,
                ..Task::default()
            },
            Task {
                id: "fb1-1".to_owned(),
                done: true,
                platform: "Rails".to_owned(),
                title: "保存後に編集画面へ戻す".to_owned(),
                lines: Some(size(30, &[])),
                line_no: 30,
                ..Task::default()
            },
        ];
        assert_eq!(plan.tasks, want, "タスクが期待と異なる");
    }

    #[test]
    fn 書式の誤りを行番号付きで返す() {
        let tests = [
            (
                "タスクリスト節が無い",
                "## 設計\n\n- [ ] **1-1**: [Go] a\n",
                "「タスクリスト」節にタスクが見つかりません",
            ),
            (
                "タスク番号の重複",
                "## タスクリスト\n\n- [ ] **1-1**: a\n- [ ] **1-1**: b\n",
                "4行目: タスク番号 1-1 が重複しています (3行目と同じ)",
            ),
            (
                "保護領域の名前が無い",
                "## タスクリスト\n\n- [ ] **1-1**: a\n  - **保護領域**: (触れる場合だけ書く)\n",
                "4行目: タスク 1-1 の保護領域: 保護領域の名前がありません",
            ),
            (
                "保護領域の括弧が閉じていない",
                "## タスクリスト\n\n- [ ] **1-1**: a\n  - **保護領域**: schema (列を追加\n",
                "括弧が閉じていません",
            ),
            (
                "想定行数が数字で始まらない",
                "## タスクリスト\n\n- [ ] **1-1**: a\n  - **想定行数**: 約N行 (実装N行 + テストN行)\n",
                "4行目: タスク 1-1 の想定行数: 「約N行」の形で書いてください",
            ),
            (
                "担当の行の重複",
                "## タスクリスト\n\n- [ ] **1-1**: a\n  - **担当**: implementer\n  - **担当**: shimbaco\n",
                "5行目: タスク 1-1 の担当の行が重複しています (4行目と同じ)",
            ),
            (
                "担当が空",
                "## タスクリスト\n\n- [ ] **1-1**: a\n  - **担当**:\n",
                "4行目: タスク 1-1 の担当が空です",
            ),
        ];
        for (name, src, want) in tests {
            let got = parse_error(src);
            assert!(
                got.contains(want),
                "{name}: エラー = {got:?}、含まれることを期待した文字列 = {want:?}"
            );
        }
    }

    #[test]
    fn 担当の行を読み取る() {
        let src = concat!(
            "## タスクリスト\n\n",
            "- [ ] **1-1**: [/workspace/go] マイグレーションを作る\n",
            "  - **想定行数**: 約80行\n",
            "- [ ] **1-2**: ヘルプページの下書きを作る\n",
            "  - **担当**: implementer\n",
            "- [ ] **1-3**: ヘルプページの下書きを公開する\n",
            "  - **担当**:  shimbaco \n",
        );
        let plan = parse(src).expect("読み取れなかった");

        let got: Vec<(&str, &str)> = plan
            .tasks
            .iter()
            .map(|task| (task.id.as_str(), task.assignee.as_str()))
            .collect();
        assert_eq!(
            got,
            [("1-1", ""), ("1-2", "implementer"), ("1-3", "shimbaco")],
            "担当が期待と異なる"
        );
        assert_eq!(plan.tasks[1].platform, "", "1-2に接頭辞がある");
        assert_eq!(
            plan.tasks[1].title, "ヘルプページの下書きを作る",
            "1-2のタスク名が期待と異なる"
        );
    }

    #[test]
    fn タスクの記述はインデントの無い行で終わる() {
        let plan =
            parse("## タスクリスト\n\n- [ ] **1-1**: a\n\n地の文\n  - **想定行数**: 約10行\n")
                .expect("読み取れなかった");

        assert_eq!(plan.tasks[0].lines, None, "地の文のあとの想定行数を拾った");
    }

    #[test]
    fn crlfの改行の計画書を読み取る() {
        let src = "## タスクリスト\r\n\r\n- [ ] **1-1**: [Go] a\r\n  - **想定行数**: 約10行 (実装5行 + テスト5行)\r\n";
        let plan = parse(src).expect("読み取れなかった");

        assert_eq!(plan.tasks[0].title, "a", "タスク名が期待と異なる");
        assert_eq!(
            plan.tasks[0].lines.as_ref().map(|size| size.total),
            Some(10),
            "想定行数が期待と異なる"
        );
    }

    #[test]
    fn タスク番号からフェーズとフィードバックの回を求める() {
        let tests = [
            ("1-2", "1", None),
            ("2-0-1", "2", None),
            ("5a-1", "5a", None),
            ("fb1-1", "fb1", Some(1)),
            ("fb12-3", "fb12", Some(12)),
            ("fbx-1", "fbx", None),
        ];
        for (id, want_phase, want_round) in tests {
            let task = Task {
                id: id.to_owned(),
                ..Task::default()
            };
            assert_eq!(task.phase(), want_phase, "{id}のフェーズが期待と異なる");
            assert_eq!(
                task.feedback_round(),
                want_round,
                "{id}のフィードバックの回が期待と異なる"
            );
        }
    }

    #[test]
    fn 完了したタスクの数と次のタスクを返す() {
        let task = |id: &str, done: bool| Task {
            id: id.to_owned(),
            done,
            ..Task::default()
        };
        let mut plan = Plan {
            tasks: vec![task("1-1", true), task("1-2", false), task("1-3", false)],
            ..Plan::default()
        };
        assert_eq!(plan.done_count(), 1, "完了したタスクの数が期待と異なる");
        assert_eq!(
            plan.next().map(|task| task.id.as_str()),
            Some("1-2"),
            "次のタスクが期待と異なる"
        );

        plan.tasks[1].done = true;
        plan.tasks[2].done = true;
        assert_eq!(
            plan.next(),
            None,
            "すべて完了しているのに次のタスクを返した"
        );
    }

    #[test]
    fn 次のフィードバックの回は既存の最大の回の次にする() {
        let plan = parse(SAMPLE_PLAN).expect("読み取れなかった");
        assert_eq!(plan.next_feedback_round(), 2, "次の回が期待と異なる");

        let plan = parse(TASKS).expect("読み取れなかった");
        assert_eq!(
            plan.next_feedback_round(),
            1,
            "フィードバックの無い計画書の次の回が期待と異なる"
        );
    }

    #[test]
    fn 計画書名の行を読み取る() {
        let tests = [
            ("行が無い", "# 計画書\n\n", ""),
            (
                "行の値を読む",
                "# 計画書\n\n**計画書名**: doc-with-wikino\n\n",
                "doc-with-wikino",
            ),
            (
                "HTMLコメントの中は読まない",
                "# 計画書\n\n<!--\n**計画書名**: example\n-->\n\n",
                "",
            ),
            (
                "コードブロックの中は読まない",
                "# 計画書\n\n```markdown\n**計画書名**: example\n```\n\n",
                "",
            ),
            (
                "最初の節より後ろは読まない",
                "# 計画書\n\n## 概要\n\n**計画書名**: example\n\n",
                "",
            ),
        ];
        for (name, head, want) in tests {
            let plan = parse(&format!("{head}{TASKS}")).expect("読み取れなかった");
            assert_eq!(plan.name, want, "{name}: 計画書名が期待と異なる");
        }
    }

    #[test]
    fn 計画書名の誤りを返す() {
        let tests = [
            (
                "英大文字",
                "**計画書名**: Doc\n",
                "1行目: 計画書名 \"Doc\" には",
            ),
            (
                "両端のハイフン",
                "**計画書名**: -doc\n",
                "計画書名 \"-doc\" には",
            ),
            (
                "続くハイフン",
                "**計画書名**: doc--wikino\n",
                "計画書名 \"doc--wikino\" には",
            ),
            ("空の値", "**計画書名**:\n", "計画書名 \"\" には"),
            (
                "重複",
                "**計画書名**: doc\n**計画書名**: doc\n",
                "2行目: 計画書名の行が重複しています (1行目と同じ)",
            ),
        ];
        for (name, head, want) in tests {
            let got = parse_error(&format!("{head}\n{TASKS}"));
            assert!(
                got.contains(want),
                "{name}: エラー = {got:?}、含まれることを期待した文字列 = {want:?}"
            );
        }
    }

    #[test]
    fn コードフェンスの中のタスク行は読まない() {
        let tests = [
            (
                "短いバッククォートで終了しない",
                "````markdown",
                "```text\n- [ ] **9-1**: 例\n```",
                "````",
            ),
            (
                "異なる文字種のチルダで終了しない",
                "```text",
                "~~~\n- [ ] **9-1**: 例",
                "```",
            ),
            (
                "異なる文字種のバッククォートで終了しない",
                "~~~text",
                "```\n- [ ] **9-1**: 例",
                "~~~",
            ),
            (
                "短いチルダで終了しない",
                "~~~~text",
                "~~~\n- [ ] **9-1**: 例",
                "~~~~",
            ),
            (
                "終端の後ろの文字列を許可しない",
                "```text",
                "```text\n- [ ] **9-1**: 例",
                "```",
            ),
            (
                "長い終端と末尾の空白を許可する",
                "```text",
                "- [ ] **9-1**: 例",
                "````` \t",
            ),
            (
                "4スペースの行で終了しない",
                "```text",
                "    ```\n- [ ] **9-1**: 例",
                "```",
            ),
            (
                "タブでインデントした行で終了しない",
                "```text",
                "\t```\n- [ ] **9-1**: 例",
                "```",
            ),
        ];
        for (name, opening, body, closing) in tests {
            let src = format!(
                "## タスクリスト\n- [ ] **1-1**: 実タスクA\n\n{opening}\n{body}\n{closing}\n\n- [ ] **1-2**: 実タスクB\n"
            );
            let plan = parse(&src).expect("読み取れなかった");
            assert_eq!(
                task_ids(&plan),
                ["1-1", "1-2"],
                "{name}: タスク番号が期待と異なる"
            );
        }
    }

    #[test]
    fn コードフェンスにならない行は読み飛ばさない() {
        for line in ["    ~~~", "\t~~~", "```text`"] {
            let plan = parse(&format!(
                "## タスクリスト\n{line}\n\n- [ ] **1-1**: 実タスク\n"
            ))
            .expect("読み取れなかった");
            assert_eq!(
                task_ids(&plan),
                ["1-1"],
                "{line:?}: タスク番号が期待と異なる"
            );
        }
    }

    #[test]
    fn リスト項目の中のコードフェンスの例は読まない() {
        let tests = [
            (
                "リスト項目の内容に揃えてインデントしたフェンス",
                "## タスクリスト\n\n- [ ] **1-1**: A\n  - 例:\n\n    ```markdown\n    - **保護領域**: schema\n    ```\n\n- [ ] **1-2**: B\n",
            ),
            (
                "入れ子のリスト項目の中のフェンス",
                "## タスクリスト\n\n- [ ] **1-1**: A\n  - 例:\n    - 書式:\n\n      ~~~markdown\n      - **保護領域**: schema\n      ~~~\n\n- [ ] **1-2**: B\n",
            ),
        ];
        for (name, src) in tests {
            let plan = parse(src).expect("読み取れなかった");
            assert_eq!(plan.tasks.len(), 2, "{name}: タスクの数が期待と異なる");
            assert!(
                plan.tasks[0].protected.is_empty(),
                "{name}: コードブロックの中の保護領域を読んだ: {:?}",
                plan.tasks[0].protected
            );
        }
    }

    #[test]
    fn リスト項目の外では3スペースまでインデントしたフェンスもコードブロックにする() {
        let plan = parse(
            "## タスクリスト\n\n   ~~~text\n- [ ] **9-1**: 例\n  ~~~\n\n- [ ] **1-1**: 実タスク\n",
        )
        .expect("読み取れなかった");
        assert_eq!(task_ids(&plan), ["1-1"], "タスク番号が期待と異なる");
    }

    #[test]
    fn ベースブランチの行を読み取る() {
        let tests = [
            ("行が無い", "# 計画書\n\n", vec![]),
            (
                "1つのリポジトリ",
                "**ベースブランチ**: /workspace=prev-feature-1-2\n",
                vec![base("/workspace", "prev-feature-1-2")],
            ),
            (
                "複数のリポジトリとパスの正規化",
                "**計画書名**: next\n**ベースブランチ**: /workspace/go/=prev ,  /korylus-tools=prev-tools\n",
                vec![
                    base("/workspace/go", "prev"),
                    base("/korylus-tools", "prev-tools"),
                ],
            ),
            (
                "カンマを含むブランチ名",
                "**ベースブランチ**: /workspace=prev,extra\n",
                vec![base("/workspace", "prev,extra")],
            ),
            (
                "カンマを含むブランチ名と複数のリポジトリ",
                "**ベースブランチ**: /workspace=prev,extra, /korylus-tools=tools,extra\n",
                vec![
                    base("/workspace", "prev,extra"),
                    base("/korylus-tools", "tools,extra"),
                ],
            ),
            (
                "HTMLコメントの中は読まない",
                "<!--\n**ベースブランチ**: /workspace=example\n-->\n",
                vec![],
            ),
            (
                "最初の節より後ろは読まない",
                "## 概要\n\n**ベースブランチ**: /workspace=example\n\n",
                vec![],
            ),
        ];
        for (name, head, want) in tests {
            let plan = parse(&format!("{head}\n{TASKS}")).expect("読み取れなかった");
            assert_eq!(plan.bases, want, "{name}: ベースブランチが期待と異なる");
        }
    }

    #[test]
    fn ベースブランチの誤りを返す() {
        let tests = [
            (
                "= が無い",
                "**ベースブランチ**: /workspace\n",
                "1行目: ベースブランチ: 「<ディレクトリのパス>=<ブランチ名>」の形で書いてください: \"/workspace\"",
            ),
            (
                "空の値",
                "**ベースブランチ**:\n",
                "「<ディレクトリのパス>=<ブランチ名>」の形で書いてください: \"\"",
            ),
            (
                "相対パス",
                "**ベースブランチ**: workspace=prev\n",
                "パスは / で始まる絶対パスにしてください: \"workspace=prev\"",
            ),
            (
                "空のブランチ名",
                "**ベースブランチ**: /workspace=\n",
                "ブランチ名が空です: \"/workspace=\"",
            ),
            (
                "同じパスの重複",
                "**ベースブランチ**: /workspace=a, /workspace/=b\n",
                "パス /workspace が重複しています",
            ),
            (
                "行の重複",
                "**ベースブランチ**: /workspace=a\n**ベースブランチ**: /workspace=a\n",
                "2行目: ベースブランチの行が重複しています (1行目と同じ)",
            ),
        ];
        for (name, head, want) in tests {
            let got = parse_error(&format!("{head}\n{TASKS}"));
            assert!(
                got.contains(want),
                "{name}: エラー = {got:?}、含まれることを期待した文字列 = {want:?}"
            );
        }
    }

    #[test]
    fn パスをgoのfilepath_cleanと同じ規則で整える() {
        let tests = [
            ("", "."),
            ("/", "/"),
            ("/workspace/", "/workspace"),
            ("/workspace//go/./", "/workspace/go"),
            ("/workspace/go/..", "/workspace"),
            ("/..", "/"),
            ("workspace", "workspace"),
            ("../a/../..", "../.."),
        ];
        for (path, want) in tests {
            assert_eq!(clean_path(path), want, "{path:?}の整え方が期待と異なる");
        }
    }

    #[test]
    fn 全角スペースは区切りの空白にしない() {
        // 移植元のGoの `\s` はASCIIの空白だけに当たるため、全角スペースは値の一部になる。
        let plan = parse("## タスクリスト\n\n- [ ] **1-1**:\u{3000}a\n").expect("読み取れなかった");
        assert_eq!(
            plan.tasks[0].title, "a",
            "タスク名の前後の空白の扱いが期待と異なる"
        );

        let got = parse_error(&format!("**計画書名**: doc\u{3000}\n\n{TASKS}"));
        assert!(
            got.contains("1行目: 計画書名")
                && got.contains("には、英小文字・数字と、間に挟むハイフンだけを使ってください"),
            "全角スペースを計画書名の末尾の空白として除いた: {got:?}"
        );
    }

    #[test]
    fn 想定行数の範囲外の値はgoのintの最大値にする() {
        let max = isize::MAX;
        let values = [
            ("0".to_owned(), 0),
            ((max - 1).to_string(), max - 1),
            (max.to_string(), max),
            (
                (u128::try_from(max).expect("isizeの最大値は負ではない") + 1).to_string(),
                max,
            ),
            (usize::MAX.to_string(), max),
            ((usize::MAX as u128 + 1).to_string(), max),
            ("9".repeat(100), max),
        ];
        for (value, want) in values {
            let src = format!("{TASKS}  - **想定行数**: 約{value}行\n");
            let plan = parse(&src).expect("読み取れなかった");
            assert_eq!(
                plan.tasks[0].lines.as_ref().map(|size| size.total),
                Some(want),
                "{value}の想定行数がGoと異なる"
            );
        }
    }

    #[test]
    fn 想定行数の内訳の加算はgoのintと同じ範囲で折り返す() {
        for value in [isize::MAX.to_string(), "9".repeat(100)] {
            let src = format!("{TASKS}  - **想定行数**: 約1行 (実装{value}行 + 実装1行)\n");
            let plan = parse(&src).expect("読み取れなかった");
            assert_eq!(
                plan.tasks[0]
                    .lines
                    .as_ref()
                    .and_then(|size| size.parts.get("実装")),
                Some(&isize::MIN),
                "{value}に1を足した内訳がGoと異なる"
            );
        }
    }

    #[test]
    fn フィードバックの回の範囲と次の回の加算をgoに揃える() {
        for (value, want_round, want_next) in [
            (
                (isize::MAX - 1).to_string(),
                Some(isize::MAX - 1),
                isize::MAX,
            ),
            (isize::MAX.to_string(), Some(isize::MAX), isize::MIN),
            ((isize::MAX as u128 + 1).to_string(), None, 1),
            (usize::MAX.to_string(), None, 1),
            ("9".repeat(100), None, 1),
        ] {
            let src = format!("## タスクリスト\n- [ ] **fb{value}-1**: タスク\n");
            let plan = parse(&src).expect("読み取れなかった");
            assert_eq!(
                plan.tasks[0].feedback_round(),
                want_round,
                "fb{value}のフィードバックの回がGoと異なる"
            );
            assert_eq!(
                plan.next_feedback_round(),
                want_next,
                "fb{value}の次の回がGoと異なる"
            );
        }
    }

    #[test]
    fn 引用する値を含むエラーメッセージ全体をgoに揃える() {
        let cases = [
            (
                format!("**計画書名**: doc\u{3000}\n{TASKS}"),
                r#"1行目: 計画書名 "doc\u3000" には、英小文字・数字と、間に挟むハイフンだけを使ってください"#,
            ),
            (
                format!("**計画書名**: a\0b\n{TASKS}"),
                r#"1行目: 計画書名 "a\x00b" には、英小文字・数字と、間に挟むハイフンだけを使ってください"#,
            ),
            (
                format!("**計画書名**: a\u{c}b\n{TASKS}"),
                r#"1行目: 計画書名 "a\fb" には、英小文字・数字と、間に挟むハイフンだけを使ってください"#,
            ),
            (
                format!("**ベースブランチ**: bad\u{3000}dir=main\n{TASKS}"),
                r#"1行目: ベースブランチ: パスは / で始まる絶対パスにしてください: "bad\u3000dir=main""#,
            ),
            (
                format!("{TASKS}  - **保護領域**: schema (a\0\n"),
                r#"4行目: タスク 1-1 の保護領域: 括弧が閉じていません: "schema (a\x00""#,
            ),
            (
                format!("{TASKS}  - **保護領域**: (a\0)\n"),
                r#"4行目: タスク 1-1 の保護領域: 保護領域の名前がありません: "(a\x00)""#,
            ),
            (
                format!("{TASKS}  - **想定行数**: 約N\u{3000}行\n"),
                r#"4行目: タスク 1-1 の想定行数: 「約N行」の形で書いてください: "約N\u3000行""#,
            ),
        ];
        for (src, want) in cases {
            assert_eq!(parse_error(&src), want, "エラーのメッセージがGoと異なる");
        }
    }
}
