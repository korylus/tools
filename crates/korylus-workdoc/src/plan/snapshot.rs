//! 計画書のスナップショットからの変更が、許される範囲だけかを確かめる。

use std::sync::LazyLock;

use regex::Regex;

use super::{FEEDBACK_PREFIX, SECTION_TITLE, Task, code_lines, parse, quote, regex};

/// タスクの対応で追記してよい節の見出し。
pub(super) const DECISION_LOG_TITLE: &str = "## 実装判断ログ";

/// チェックの入っていないチェックボックスの行頭にマッチする。
/// タスク行 (インデント無し) とサブタスクの行 (インデント有り) の両方に当たる。
static UNCHECKED: LazyLock<Regex> = LazyLock::new(|| regex(r"^([\t\n\f\r ]*- )\[ \]"));

/// 計画書のスナップショットからの、許されない変更1件。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Violation {
    /// 現在の計画書での行番号 (1始まり)。行を特定できなければ `None`。
    pub line_no: Option<usize>,
    /// 変更の内容。
    pub message: String,
}

impl Violation {
    fn at(line_no: usize, message: String) -> Self {
        Self {
            line_no: Some(line_no),
            message,
        }
    }

    fn anywhere(message: String) -> Self {
        Self {
            line_no: None,
            message,
        }
    }
}

/// 計画書の `## ` の見出しで区切った1つの節。
#[derive(Debug, Clone, Default)]
pub(super) struct Section {
    /// 見出しの行。最初の見出しより前は空文字。
    pub(super) title: String,
    /// 見出しの行の、ファイル全体での添字 (0始まり)。
    pub(super) start: usize,
    pub(super) lines: Vec<String>,
}

/// 計画書のスナップショット `snapshot` から現在の内容 `current` への変更が、
/// タスク `task_id` の対応で許される変更だけかを確かめ、許されない変更を返す。
///
/// 許される変更は次の2つに限る。
///
/// - 「タスクリスト」節で、`task_id` のタスク行とその下のサブタスクのチェックボックスにチェックを入れる
/// - 「実装判断ログ」節への行の追加 (節が無ければ、節ごとの追加)
pub fn check_task_changes(snapshot: &str, current: &str, task_id: &str) -> Vec<Violation> {
    let (old_secs, old_log) = split_sections(snapshot);
    let (new_secs, new_log) = split_sections(current);

    let mut vs = compare_sections(&old_secs, &new_secs, |o, n| check_task_list(o, n, task_id));
    match (old_log, new_log) {
        (Some(_), None) => vs.push(Violation::anywhere(format!(
            "「{}」節が削除されています",
            log_name()
        ))),
        (Some(old_log), Some(new_log)) => {
            if let Some(j) = missing_line(&old_log.lines, &new_log.lines) {
                vs.push(Violation::anywhere(format!(
                    "「{}」節の既存の行が変更・削除されています (スナップショットの{}行目: {})",
                    log_name(),
                    old_log.start + j + 1,
                    quote(&old_log.lines[j])
                )));
            }
        }
        _ => {}
    }
    vs
}

/// 計画書のスナップショット `snapshot` から現在の内容 `current` への変更が、
/// 「タスクリスト」節の末尾へのフィードバック対応のフェーズfbN (Nはスナップショットの最大の回 + 1) の追加だけかを確かめる。
///
/// 追加されたタスクを返し、許されない変更があればそれを返す。
/// 追加されたタスクは、どれもフェーズfbNの未完了のタスクでなければならない。
pub fn check_feedback_phase(snapshot: &str, current: &str) -> Result<Vec<Task>, Vec<Violation>> {
    let old = parse(snapshot).map_err(|err| {
        vec![Violation::anywhere(format!(
            "計画書のスナップショットを読み取れません: {err}"
        ))]
    })?;
    let cur = parse(current).map_err(|err| {
        vec![Violation::anywhere(format!(
            "計画書を読み取れません: {err}"
        ))]
    })?;

    let (old_secs, old_log) = split_sections(snapshot);
    let (new_secs, new_log) = split_sections(current);
    let mut vs = compare_sections(&old_secs, &new_secs, check_appended);
    match (old_log, new_log) {
        (Some(old_log), Some(new_log)) => {
            if let Some(j) = first_diff(&old_log.lines, &new_log.lines) {
                vs.push(Violation::at(
                    new_log.start + j + 1,
                    format!("「{}」節が変更されています", log_name()),
                ));
            }
        }
        (None, None) => {}
        _ => vs.push(Violation::anywhere(format!(
            "「{}」節が追加・削除されています",
            log_name()
        ))),
    }
    if !vs.is_empty() {
        return Err(vs);
    }

    // 既存の行は変わっていないため、追加されたタスクは現在の内容の末尾に並ぶ。
    // 追加した行がHTMLコメントやコードブロックを閉じて既存の行の読み方を変えた場合に備え、
    // 既存のタスクがそのまま残っているかも確かめる。
    let round = old.next_feedback_round();
    if !cur.tasks.starts_with(&old.tasks) {
        return Err(vec![Violation::anywhere(
            "既存のタスクの読み取り結果が変わっています".to_owned(),
        )]);
    }
    let added = cur.tasks[old.tasks.len()..].to_vec();
    if added.is_empty() {
        return Err(vec![Violation::anywhere(format!(
            "フェーズ{FEEDBACK_PREFIX}{round}のタスクが追加されていません"
        ))]);
    }
    for task in &added {
        if task.feedback_round() != Some(round) {
            vs.push(Violation::at(
                task.line_no,
                format!(
                    "タスク {} はフェーズ{FEEDBACK_PREFIX}{round}のタスクではありません",
                    task.id
                ),
            ));
        }
        if task.done {
            vs.push(Violation::at(
                task.line_no,
                format!("追加されたタスク {} にチェックが入っています", task.id),
            ));
        }
    }
    if vs.is_empty() { Ok(added) } else { Err(vs) }
}

/// 節を見出しの順に対応付けて比べる。
/// 「タスクリスト」節は `task_list` で、それ以外の節は内容が変わっていないかで比べる。
fn compare_sections(
    old_secs: &[Section],
    new_secs: &[Section],
    task_list: impl Fn(&Section, &Section) -> Vec<Violation>,
) -> Vec<Violation> {
    let mut vs = Vec::new();
    for i in 0..old_secs.len().max(new_secs.len()) {
        let (Some(o), Some(n)) = (old_secs.get(i), new_secs.get(i)) else {
            if let Some(n) = new_secs.get(i) {
                vs.push(Violation::at(
                    n.start + 1,
                    format!("節 {} が追加されています", quote(&n.title)),
                ));
            } else {
                vs.push(Violation::anywhere(format!(
                    "節 {} が削除されています",
                    quote(&old_secs[i].title)
                )));
            }
            break;
        };
        if o.title != n.title {
            // 見出しがずれると以降の節を正しく対応付けられないため、ここで比較をやめる。
            vs.push(Violation::at(
                n.start + 1,
                format!(
                    "節 {} が {} に変わっています",
                    quote(&o.title),
                    quote(&n.title)
                ),
            ));
            break;
        }
        if o.title == SECTION_TITLE {
            vs.extend(task_list(o, n));
            continue;
        }
        if let Some(j) = first_diff(&o.lines, &n.lines) {
            vs.push(Violation::at(
                n.start + j + 1,
                format!("{}が変更されています", section_name(&o.title)),
            ));
        }
    }
    vs
}

/// 「タスクリスト」節の変更が、節の末尾 (次の見出しの前の空行より前) への行の追加だけで、
/// 追記がフィードバック対応のフェーズの見出しから始まるかを確かめる。
///
/// 既存のタスクの説明行は読み取りの結果に現れないため、見出しの前の追記を許すと、
/// 最後の既存タスクの説明を書き足す変更を検知できない。
fn check_appended(o: &Section, n: &Section) -> Vec<Violation> {
    let ol = trim_trailing_blank(&o.lines);
    let nl = trim_trailing_blank(&n.lines);
    if !nl.starts_with(ol) {
        let j = first_diff(ol, nl).unwrap_or(0);
        return vec![Violation::at(
            n.start + j + 1,
            format!(
                "{}の既存の行が変更・削除されています",
                section_name(&o.title)
            ),
        )];
    }
    for (j, line) in nl.iter().enumerate().skip(ol.len()) {
        if line.trim().is_empty() {
            continue;
        }
        if !line.starts_with("### フェーズfb") {
            return vec![Violation::at(
                n.start + j + 1,
                "追記はフィードバック対応のフェーズの見出しから始めてください".to_owned(),
            )];
        }
        break;
    }
    Vec::new()
}

/// `lines` の末尾の空行を除いたものを返す。
fn trim_trailing_blank(lines: &[String]) -> &[String] {
    let end = lines
        .iter()
        .rposition(|line| !line.trim().is_empty())
        .map_or(0, |i| i + 1);
    &lines[..end]
}

/// `src` を `## ` の見出しで節に分ける。
///
/// 「実装判断ログ」節は他の節と比べ方が違うため分けて返し、無ければ `None` を返す。
/// コードブロックの中の見出しは区切りにしない。
pub(super) fn split_sections(src: &str) -> (Vec<Section>, Option<Section>) {
    let src = src.replace("\r\n", "\n");
    let lines: Vec<&str> = src.split('\n').collect();
    let in_code = code_lines(&lines);

    let mut secs = vec![Section::default()];
    for (i, line) in lines.iter().enumerate() {
        if !in_code[i] && line.starts_with("## ") {
            secs.push(Section {
                title: line.trim().to_owned(),
                start: i,
                lines: Vec::new(),
            });
        }
        if let Some(cur) = secs.last_mut() {
            cur.lines.push((*line).to_owned());
        }
    }

    let mut log = None;
    let mut others = Vec::with_capacity(secs.len());
    for s in secs {
        if log.is_none() && s.title == DECISION_LOG_TITLE {
            log = Some(s);
        } else {
            others.push(s);
        }
    }
    (others, log)
}

/// 「タスクリスト」節の変更が、`task_id` のタスクのチェックボックスにチェックを入れる変更だけかを確かめる。
fn check_task_list(o: &Section, n: &Section, task_id: &str) -> Vec<Violation> {
    if o.lines.len() != n.lines.len() {
        let j = first_diff(&o.lines, &n.lines).unwrap_or(0);
        return vec![Violation::at(
            n.start + j + 1,
            format!("{}の行が増減しています", section_name(&o.title)),
        )];
    }

    let mut vs = Vec::new();
    let mut in_task = false;
    for (j, (line, new_line)) in o.lines.iter().zip(&n.lines).enumerate() {
        if let Some(m) = super::TASK.captures(line) {
            in_task = &m[2] == task_id;
        } else if !line.is_empty() && !line.starts_with(' ') && !line.starts_with('\t') {
            in_task = false;
        }
        if line == new_line {
            continue;
        }
        if in_task
            && UNCHECKED.is_match(line)
            && UNCHECKED.replace(line, "${1}[x]") == new_line.as_str()
        {
            continue;
        }
        vs.push(Violation::at(
            n.start + j + 1,
            format!(
                "{}で、タスク {task_id} のチェック以外の行が変更されています",
                section_name(&o.title)
            ),
        ));
    }
    vs
}

/// 「実装判断ログ」節の見出しの名前を返す。
fn log_name() -> &'static str {
    DECISION_LOG_TITLE.trim_start_matches("## ")
}

/// 節の見出しを表示用に返す。
fn section_name(title: &str) -> String {
    if title.is_empty() {
        "最初の見出しより前".to_owned()
    } else {
        format!("「{}」節", title.trim_start_matches("## "))
    }
}

/// `a` と `b` で最初に異なる行の添字を返す。同じなら `None` を返す。
fn first_diff(a: &[String], b: &[String]) -> Option<usize> {
    let common = a.len().min(b.len());
    (0..common)
        .find(|&i| a[i] != b[i])
        .or((a.len() != b.len()).then_some(common))
}

/// `old` の行が順序を保ったまま `new` に残っているか (`new` が `old` に行を足しただけか) を確かめ、
/// 残っていない最初の行の添字を返す。
fn missing_line(old: &[String], new: &[String]) -> Option<usize> {
    let mut rest = new.iter();
    old.iter()
        .position(|line| !rest.any(|candidate| candidate == line))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 計画書のスナップショットの例。
    const SNAPSHOT_PLAN: &str = concat!(
        "# 例の作業計画書\n",
        "\n",
        "## 設計\n",
        "\n",
        "```markdown\n",
        "## コードブロックの中の見出しは節の区切りにしない\n",
        "```\n",
        "\n",
        "## 実装判断ログ\n",
        "\n",
        "### タスク1-1: 投稿の公開範囲を保存する\n",
        "\n",
        "#### 列の型をどうするか?\n",
        "\n",
        "## タスクリスト\n",
        "\n",
        "- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n",
        "\n",
        "- [ ] **1-2**: [Go] 公開範囲を表示する\n",
        "  - [ ] 一覧に表示する\n",
        "  - [ ] 詳細に表示する\n",
        "\n",
        "- [ ] **1-3**: [Go] 公開範囲を編集する\n",
        "\n",
        "## 参考資料\n",
        "\n",
        "- なし\n",
    );

    /// 実装判断ログ節を、見出しから次の節の前まで含めて除いた文字列。
    const DECISION_LOG: &str = "## 実装判断ログ\n\n### タスク1-1: 投稿の公開範囲を保存する\n\n#### 列の型をどうするか?\n\n";

    fn replace(src: &str, from: &str, to: &str) -> String {
        assert!(src.contains(from), "置き換える文字列 {from:?} が無い");
        src.replacen(from, to, 1)
    }

    fn messages(vs: &[Violation]) -> String {
        vs.iter()
            .map(|v| v.message.as_str())
            .collect::<Vec<_>>()
            .join("\n")
    }

    /// 違反が無いか、期待した文字列をすべて含む違反があることを確かめる。
    fn assert_violations(name: &str, vs: &[Violation], want: &[&str]) {
        if want.is_empty() {
            assert!(
                vs.is_empty(),
                "{name}: 違反なしを期待したが違反がある: {vs:?}"
            );
            return;
        }
        let got = messages(vs);
        for w in want {
            assert!(
                got.contains(w),
                "{name}: 違反 = {got:?}、含まれることを期待した文字列 = {w:?}"
            );
        }
    }

    #[test]
    fn タスクの対応で許される変更だけかを確かめる() {
        let checked = replace(
            &replace(
                &replace(SNAPSHOT_PLAN, "- [ ] **1-2**", "- [x] **1-2**"),
                "  - [ ] 一覧に表示する",
                "  - [x] 一覧に表示する",
            ),
            "#### 列の型をどうするか?\n",
            "#### 列の型をどうするか?\n\n- [x] 文字列\n\n### タスク1-2: 公開範囲を表示する\n\n#### 表示する場所は?\n",
        );
        let tests: Vec<(&str, String, Vec<&str>)> = vec![
            ("変更なし", SNAPSHOT_PLAN.to_owned(), vec![]),
            (
                "タスクとサブタスクのチェックと、実装判断ログへの追記",
                checked,
                vec![],
            ),
            (
                "CRLFの改行に変わっても違反にしない",
                replace(SNAPSHOT_PLAN, "- [ ] **1-2**", "- [x] **1-2**").replace('\n', "\r\n"),
                vec![],
            ),
            (
                "別のタスクのチェック",
                replace(SNAPSHOT_PLAN, "- [ ] **1-3**", "- [x] **1-3**"),
                vec!["「タスクリスト」節で、タスク 1-2 のチェック以外の行が変更されています"],
            ),
            (
                "完了済みのタスクのチェックを外す",
                replace(SNAPSHOT_PLAN, "- [x] **1-1**", "- [ ] **1-1**"),
                vec!["タスク 1-2 のチェック以外"],
            ),
            (
                "対象のタスクの本文の変更",
                replace(
                    SNAPSHOT_PLAN,
                    "  - [ ] 詳細に表示する",
                    "  - [ ] 詳細には表示しない",
                ),
                vec!["タスク 1-2 のチェック以外"],
            ),
            (
                "タスクの追加",
                replace(
                    SNAPSHOT_PLAN,
                    "- [ ] **1-3**: [Go] 公開範囲を編集する\n",
                    "- [ ] **1-3**: [Go] 公開範囲を編集する\n- [ ] **1-4**: [Go] 追加\n",
                ),
                vec!["「タスクリスト」節の行が増減しています"],
            ),
            (
                "設計節の変更",
                replace(SNAPSHOT_PLAN, "## 設計\n\n", "## 設計\n\n新しい方針\n"),
                vec!["「設計」節が変更されています"],
            ),
            (
                "コードブロックの中の変更も、その節の変更として扱う",
                replace(
                    SNAPSHOT_PLAN,
                    "## コードブロックの中の見出しは",
                    "## コードブロックの中の見出しを",
                ),
                vec!["「設計」節が変更されています"],
            ),
            (
                "実装判断ログの既存の行の書き換え",
                replace(SNAPSHOT_PLAN, "#### 列の型をどうするか?", "#### 列の型は?"),
                vec!["「実装判断ログ」節の既存の行が変更・削除されています"],
            ),
            (
                "実装判断ログの削除",
                replace(SNAPSHOT_PLAN, DECISION_LOG, ""),
                vec!["「実装判断ログ」節が削除されています"],
            ),
            (
                "節の追加",
                format!("{SNAPSHOT_PLAN}\n## 追加の節\n"),
                vec!["節 \"## 追加の節\" が追加されています"],
            ),
            (
                "節の見出しの変更",
                replace(SNAPSHOT_PLAN, "## 参考資料", "## 参考"),
                vec!["節 \"## 参考資料\" が \"## 参考\" に変わっています"],
            ),
        ];
        for (name, current, want) in tests {
            let vs = check_task_changes(SNAPSHOT_PLAN, &current, "1-2");
            assert_violations(name, &vs, &want);
        }
    }

    #[test]
    fn 違反に現在の計画書での行番号を添える() {
        let current = replace(SNAPSHOT_PLAN, "- [ ] **1-3**", "- [x] **1-3**");
        let vs = check_task_changes(SNAPSHOT_PLAN, &current, "1-2");

        assert_eq!(vs.len(), 1, "違反の数が期待と異なる: {vs:?}");
        let offset = SNAPSHOT_PLAN.find("- [ ] **1-3**").expect("1-3の行が無い");
        let want = SNAPSHOT_PLAN[..offset].matches('\n').count() + 1;
        assert_eq!(vs[0].line_no, Some(want), "行番号が期待と異なる");
    }

    #[test]
    fn 実装判断ログ節が無ければ節ごと追加してよい() {
        let snapshot = replace(SNAPSHOT_PLAN, DECISION_LOG, "");
        let current = replace(
            &snapshot,
            "## タスクリスト\n",
            "## 実装判断ログ\n\n### タスク1-2: 公開範囲を表示する\n\n## タスクリスト\n",
        );
        let vs = check_task_changes(&snapshot, &current, "1-2");
        assert!(vs.is_empty(), "違反なしを期待したが違反がある: {vs:?}");
    }

    /// `SNAPSHOT_PLAN` のタスクリストの末尾に足すフェーズfb1の例。
    fn fb_phase(tasks: &[&str]) -> String {
        format!(
            "### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n\n{}\n\n",
            tasks.join("\n")
        )
    }

    /// `src` の「タスクリスト」節の末尾 (次の節の見出しの前) に `added` を足す。
    fn append_tasks(src: &str, added: &str) -> String {
        replace(src, "## 参考資料", &format!("{added}## 参考資料"))
    }

    #[test]
    #[expect(clippy::too_many_lines, reason = "ケースの表を1つのテストに並べるため")]
    fn フィードバック対応のフェーズの追加だけかを確かめる() {
        struct Case {
            name: &'static str,
            snapshot: String,
            current: String,
            want_added: Vec<&'static str>,
            want: Vec<&'static str>,
        }

        let fb11 = "- [ ] **fb1-1**: [Go] 一覧の表示を直す\n  - **対応するフィードバック**: 1回目「一覧が崩れる」";
        let fb12 = "- [ ] **fb1-2**: [Go] 列を足す\n  - **保護領域**: schema (postsに列を追加)";
        let fb11_done = fb11.replacen("[ ]", "[x]", 1);
        let with_fb1 = append_tasks(SNAPSHOT_PLAN, &fb_phase(&[&fb11_done]));

        let ok = |name, snapshot: &str, current: String, want_added| Case {
            name,
            snapshot: snapshot.to_owned(),
            current,
            want_added,
            want: vec![],
        };
        let ng = |name, current: String, want| Case {
            name,
            snapshot: SNAPSHOT_PLAN.to_owned(),
            current,
            want_added: vec![],
            want,
        };
        let tests = vec![
            ok(
                "フェーズfb1の追加",
                SNAPSHOT_PLAN,
                append_tasks(SNAPSHOT_PLAN, &fb_phase(&[fb11, fb12])),
                vec!["fb1-1", "fb1-2"],
            ),
            ok(
                "既存のフェーズfb1があればfb2を足す",
                &with_fb1,
                append_tasks(
                    SNAPSHOT_PLAN,
                    &format!(
                        "{}### フェーズfb2: 動作確認のフィードバック対応 (第2版)\n\n- [ ] **fb2-1**: [Go] 再度直す\n\n",
                        fb_phase(&[&fb11_done])
                    ),
                ),
                vec!["fb2-1"],
            ),
            ng(
                "タスクの追加が無い",
                SNAPSHOT_PLAN.to_owned(),
                vec!["フェーズfb1のタスクが追加されていません"],
            ),
            ng(
                "フェーズfbNでないタスクの追加",
                append_tasks(SNAPSHOT_PLAN, &fb_phase(&["- [ ] **1-4**: [Go] 追加"])),
                vec!["タスク 1-4 はフェーズfb1のタスクではありません"],
            ),
            ng(
                "回を飛ばしたフェーズの追加",
                append_tasks(SNAPSHOT_PLAN, &fb_phase(&["- [ ] **fb2-1**: [Go] 追加"])),
                vec!["タスク fb2-1 はフェーズfb1のタスクではありません"],
            ),
            ng(
                "チェック済みのタスクの追加",
                append_tasks(SNAPSHOT_PLAN, &fb_phase(&[&fb11_done])),
                vec!["追加されたタスク fb1-1 にチェックが入っています"],
            ),
            ng(
                "既存のタスクの変更",
                append_tasks(
                    &replace(SNAPSHOT_PLAN, "- [ ] **1-3**", "- [x] **1-3**"),
                    &fb_phase(&[fb11]),
                ),
                vec!["「タスクリスト」節の既存の行が変更・削除されています"],
            ),
            ng(
                "タスクリストの途中への追加",
                replace(
                    SNAPSHOT_PLAN,
                    "- [ ] **1-3**",
                    &format!("{}- [ ] **1-3**", fb_phase(&[fb11])),
                ),
                vec!["「タスクリスト」節の既存の行が変更・削除されています"],
            ),
            ng(
                "設計節の変更",
                append_tasks(
                    &replace(SNAPSHOT_PLAN, "## 設計\n\n", "## 設計\n\n新しい方針\n"),
                    &fb_phase(&[fb11]),
                ),
                vec!["「設計」節が変更されています"],
            ),
            ng(
                "実装判断ログへの追記",
                append_tasks(
                    &replace(
                        SNAPSHOT_PLAN,
                        "#### 列の型をどうするか?\n",
                        "#### 列の型をどうするか?\n\n- [x] 文字列\n",
                    ),
                    &fb_phase(&[fb11]),
                ),
                vec!["「実装判断ログ」節が変更されています"],
            ),
            ng(
                "節の追加",
                format!(
                    "{}\n## 追加の節\n",
                    append_tasks(SNAPSHOT_PLAN, &fb_phase(&[fb11]))
                ),
                vec!["節 \"## 追加の節\" が追加されています"],
            ),
        ];

        for case in tests {
            match check_feedback_phase(&case.snapshot, &case.current) {
                Ok(added) => {
                    assert!(
                        case.want.is_empty(),
                        "{}: 違反を期待したが、追加されたタスクを返した: {added:?}",
                        case.name
                    );
                    let ids: Vec<&str> = added.iter().map(|task| task.id.as_str()).collect();
                    assert_eq!(
                        ids, case.want_added,
                        "{}: 追加されたタスクが期待と異なる",
                        case.name
                    );
                }
                Err(vs) => assert_violations(case.name, &vs, &case.want),
            }
        }
    }

    #[test]
    fn 既存のタスクへの追記はフィードバック対応のフェーズの追加として受け付けない() {
        let old = "## タスクリスト\n\n- [x] **1-1**: [Go] 元タスク\n";
        for extra in [
            "  - **保護領域**: schema (列を追加)",
            "  - **想定行数**: 約9999行 (実装9999行)",
            "  - **対応するフィードバック**: 1回目「修正」",
            "  - 元タスクの説明を追加する",
        ] {
            let current = format!("{old}{extra}\n\n{}", fb_phase(&["- [ ] **fb1-1**: 修正"]));
            let result = check_feedback_phase(old, &current);
            assert!(
                result.is_err(),
                "既存タスクへの追記を受け入れた ({extra:?}): {result:?}"
            );
        }
    }

    #[test]
    fn コメントを閉じて既存のタスクに想定行数を足す追記は受け付けない() {
        // 追記が閉じていないHTMLコメントを閉じると、続く行が既存のタスクの想定行数として読まれる。
        // 内訳の無い「約0行」を足しても、想定行数が無いタスクとは読み取りの結果が異なる。
        let old = "## タスクリスト\n\n- [x] **1-1**: タスク\n<!--\n";
        for lines in ["約0行", "約5行"] {
            let current = format!(
                "{old}### フェーズfb1: 対応\n-->\n  - **想定行数**: {lines}\n- [ ] **fb1-1**: 直す\n"
            );
            assert_eq!(
                check_feedback_phase(old, &current),
                Err(vec![Violation::anywhere(
                    "既存のタスクの読み取り結果が変わっています".to_owned()
                )]),
                "既存のタスクに想定行数 ({lines}) を足す追記を受け入れた"
            );
        }
    }

    #[test]
    fn 行を足しただけかを順序を保って確かめる() {
        let lines = |s: &str| s.split('\n').map(str::to_owned).collect::<Vec<_>>();
        assert_eq!(
            missing_line(&lines("a\nb"), &lines("a\nx\nb\ny")),
            None,
            "行を足しただけなのに違反にした"
        );
        assert_eq!(
            missing_line(&lines("a\nb"), &lines("b\na")),
            Some(1),
            "順序の入れ替えを見逃した"
        );
        assert_eq!(
            missing_line(&lines("a\nb"), &lines("a")),
            Some(1),
            "行の削除を見逃した"
        );
    }

    #[test]
    fn 見出しと実装判断ログの行をgoの引用形式で診断する() {
        let old = "## 設計\u{3000}名\n\n方針\n\n## タスクリスト\n\n- [ ] **1-1**: タスク\n";
        let current = old.replace("## 設計\u{3000}名", "## 別名");
        let vs = check_task_changes(old, &current, "1-1");
        assert_eq!(vs.len(), 1, "違反の数が期待と異なる: {vs:?}");
        assert_eq!(
            vs[0].message, r###"節 "## 設計\u3000名" が "## 別名" に変わっています"###,
            "見出しの診断がGoと異なる"
        );

        let old =
            "## 実装判断ログ\n\n論点\u{3000}本文\n\n## タスクリスト\n\n- [ ] **1-1**: タスク\n";
        let current = old.replace("論点\u{3000}本文", "別の本文");
        let vs = check_task_changes(old, &current, "1-1");
        assert_eq!(vs.len(), 1, "違反の数が期待と異なる: {vs:?}");
        assert_eq!(
            vs[0].message,
            r#"「実装判断ログ」節の既存の行が変更・削除されています (スナップショットの3行目: "論点\u3000本文")"#,
            "実装判断ログの診断がGoと異なる"
        );
    }
}
