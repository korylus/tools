//! レビュードキュメントの対応方針へ回答する。

use super::{Category, Choice, Document, Finding, RECOMMENDED_MARK};

/// 対応方針への回答の結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Answers {
    /// 回答した指摘ごとの、選ばれた選択肢。
    pub decisions: Vec<Decision>,
    /// 人間の判断を待つ指摘 (要人間判断と再検討提案のうち、まだ選択肢が選ばれていないもの)。
    pub pending: Vec<Finding>,
    /// 推奨どおりに回答できない指摘の理由。
    pub invalid: Vec<String>,
    /// 推奨の選択肢にチェックを入れたレビュードキュメントの内容。
    pub output: String,
}

/// 指摘1件への回答。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Decision {
    /// 回答した指摘。
    pub finding: Finding,
    /// 選ばれた選択肢。
    pub chosen: Vec<Choice>,
    /// 回答で推奨の選択肢を選んだかどうか。偽なら、回答の前から選ばれていた (人間が選んだ) 選択肢を使う。
    pub auto: bool,
}

impl Decision {
    /// 選ばれた選択肢が、どれもコードを変えないものかを返す。
    pub fn defers(&self) -> bool {
        self.chosen.iter().all(Choice::defers)
    }
}

impl Answers {
    /// 人間の判断を待つ指摘か、回答できない指摘があるかを返す。
    pub fn blocked(&self) -> bool {
        !self.pending.is_empty() || !self.invalid.is_empty()
    }

    /// 修正が要る回答があるかを返す。
    pub fn needs_fix(&self) -> bool {
        self.decisions.iter().any(|d| !d.defers())
    }
}

impl Document {
    /// 各指摘の対応方針に回答する。
    ///
    /// - すでに選択肢が選ばれている指摘は、その選択肢を回答とする (人間が止まった指摘に回答して再開したとき)
    /// - 要人間判断の指摘と再検討提案は、人間の判断を待つ
    /// - 自動対応可の指摘は、「(推奨)」の付いた選択肢にチェックを入れる
    ///
    /// 区分や推奨が読み取れない指摘は、推測で回答せずに `invalid` に理由を残す。
    pub fn answer(&self) -> Answers {
        let mut a = Answers {
            decisions: Vec::new(),
            pending: Vec::new(),
            invalid: Vec::new(),
            output: self.src.clone(),
        };
        for f in &self.findings {
            let checked: Vec<Choice> = f.options.iter().filter(|o| o.checked).cloned().collect();
            let recommended: Vec<&Choice> = f.options.iter().filter(|o| o.recommended).collect();
            if f.options.is_empty() {
                a.invalid
                    .push(f.describe_with("対応方針にチェックボックスの選択肢がありません"));
            } else if !checked.is_empty() {
                a.decisions.push(Decision {
                    finding: f.clone(),
                    chosen: checked,
                    auto: false,
                });
            } else if f.proposal || f.category == Some(Category::NeedsHuman) {
                a.pending.push(f.clone());
            } else if f.category != Some(Category::AutoFix) {
                a.invalid.push(f.describe_with(&format!(
                    "区分 ({} / {}) を読み取れません",
                    Category::AutoFix.as_str(),
                    Category::NeedsHuman.as_str()
                )));
            } else if let [o] = recommended.as_slice() {
                // 括弧の中は半角スペース1文字のため、同じ長さの `x` に置き換えても位置はずれない。
                a.output.replace_range(o.mark..=o.mark, "x");
                let mut o = (*o).clone();
                o.checked = true;
                a.decisions.push(Decision {
                    finding: f.clone(),
                    chosen: vec![o],
                    auto: true,
                });
            } else {
                a.invalid.push(f.describe_with(&format!(
                    "「{RECOMMENDED_MARK}」の付いた選択肢が{}個あります (1個を期待)",
                    recommended.len()
                )));
            }
        }
        a
    }

    /// 指摘ごとに選んだ選択肢choicesにだけチェックを入れた、レビュードキュメントの内容を返す。
    ///
    /// choicesは、このドキュメントの指摘 (`findings` か、[`Document::answer`] の `pending` の要素) と、選んだ選択肢の位置の組。
    /// 選んだ指摘のほかの選択肢のチェックは外す。
    pub fn choose(&self, choices: &[(&Finding, usize)]) -> String {
        let mut out = self.src.clone();
        for (finding, chosen) in choices {
            for (i, option) in finding.options.iter().enumerate() {
                // 括弧の中は1文字のため、`x` か半角スペースに置き換えても位置はずれない。
                let mark = if i == *chosen { "x" } else { " " };
                out.replace_range(option.mark..=option.mark, mark);
            }
        }
        out
    }

    /// 選択肢にチェックが入っている指摘を返す。
    /// レビュアーが書いた直後のドキュメントでは、チェックはレビュアーが入れたものになる。
    pub fn checked(&self) -> Vec<&Finding> {
        self.findings
            .iter()
            .filter(|f| f.options.iter().any(|o| o.checked))
            .collect()
    }
}

impl Finding {
    /// 指摘の場所と見出しを添えたメッセージを返す。
    fn describe_with(&self, message: &str) -> String {
        format!("{}行目「{}」: {message}", self.line_no, self.title)
    }

    /// 人間の判断を待つ指摘を、停止理由の詳細に載せる形にする。
    pub fn describe(&self) -> String {
        let kind = if self.proposal {
            "再検討提案"
        } else {
            self.category.as_ref().map_or("", Category::as_str)
        };
        self.describe_with(kind)
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::{TEST_DOC, doc_with, parse_ok};
    use super::super::{SECTION_FINDINGS, parse};
    use super::*;

    fn replace(src: &str, from: &str, to: &str) -> String {
        assert!(src.contains(from), "置き換える文字列 {from:?} が無い");
        src.replacen(from, to, 1)
    }

    #[test]
    fn 選んだ選択肢にだけチェックを入れ_同じ指摘のほかのチェックを外す() {
        let src = doc_with(
            SECTION_FINDINGS,
            "### 1. 設計を変える\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [x] 変える (推奨)\n- [ ] 現状維持\n\n\
             ### 2. 名前を揃える\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [ ] 揃える (推奨)\n- [ ] 現状維持\n",
        );
        let d = parse_ok(&src);
        let out = d.choose(&[(&d.findings[0], 1), (&d.findings[1], 0)]);
        assert_eq!(
            out,
            replace(
                &replace(&src, "- [x] 変える", "- [ ] 変える"),
                "- [ ] 現状維持\n\n### 2.",
                "- [x] 現状維持\n\n### 2.",
            )
            .replace("- [ ] 揃える", "- [x] 揃える")
        );
        let answered = parse_ok(&out).answer();
        assert!(answered.pending.is_empty(), "選んだ指摘が人間待ちのまま");
    }

    #[test]
    fn 自動対応可の推奨にチェックを入れ_要人間判断と再検討提案は待つ() {
        let a = parse_ok(TEST_DOC).answer();

        assert!(
            a.invalid.is_empty(),
            "回答できない指摘がある: {:?}",
            a.invalid
        );
        assert_eq!(
            a.pending.len(),
            2,
            "人間の判断を待つ指摘の数が期待と異なる: {:?}",
            a.pending
        );
        assert!(a.pending[0].proposal, "再検討提案を待っていない");
        assert_eq!(
            a.pending[1].title, "`internal/a.go`",
            "要人間判断の指摘が期待と異なる"
        );
        assert!(a.blocked(), "人間の判断を待つ指摘があるのに止まらない");
        assert_eq!(
            a.decisions.len(),
            2,
            "回答の数が期待と異なる: {:?}",
            a.decisions
        );
        assert!(a.decisions[0].auto, "推奨の選択肢を自動で選んでいない");
        assert!(!a.decisions[0].defers(), "修正する回答を先送りとした");
        assert!(
            a.decisions[1].defers(),
            "後続タスクに積む回答を先送りとしなかった"
        );

        for want in [
            "- [x] 修正案の通り差し戻す (推奨)",
            "- [x] 後続タスクに積む (推奨)",
            "- [ ] slogに変える (推奨)",
            "- [ ] 決定済みの方針どおり修正する (推奨)",
            "- [ ] コメントの中の選択肢 (推奨)",
            "- [ ] コードブロックの中の選択肢 (推奨)",
        ] {
            assert!(
                a.output.contains(want),
                "回答したドキュメントに {want:?} が無い"
            );
        }
        assert_eq!(
            a.output.matches("[x]").count(),
            TEST_DOC.matches("[x]").count() + 2,
            "チェックの数が期待と異なる"
        );
    }

    #[test]
    fn 人間が選んだ選択肢を回答として使う() {
        let src = replace(TEST_DOC, "- [ ] 指摘を取り下げる", "- [x] 指摘を取り下げる");
        let src = replace(
            &src,
            "- [ ] その他 (下の回答欄に記入)\n\n## 設計",
            "- [X] その他 (下の回答欄に記入)\n\n## 設計",
        );
        let a = parse_ok(&src).answer();

        assert!(
            !a.blocked(),
            "止まった: 待つ指摘 = {:?}、回答できない指摘 = {:?}",
            a.pending,
            a.invalid
        );
        assert_eq!(
            a.decisions.len(),
            4,
            "回答の数が期待と異なる: {:?}",
            a.decisions
        );
        let (proposal, other) = (&a.decisions[0], &a.decisions[2]);
        assert!(
            !proposal.auto && proposal.defers() && proposal.chosen[0].text == "指摘を取り下げる",
            "再検討提案への回答が期待と異なる: {proposal:?}"
        );
        assert!(
            !other.auto && !other.defers(),
            "その他を選んだ回答が期待と異なる: {other:?}"
        );
        assert!(
            a.needs_fix(),
            "修正が要る回答があるのに修正は要らないとした"
        );
    }

    #[test]
    fn 区分と推奨を読み取れない指摘は回答しない() {
        let src = concat!(
            "# レビュー\n\n## ファイルごとのレビュー結果\n\n",
            "### 区分が無い\n\n**対応方針**:\n\n- [ ] 直す (推奨)\n\n",
            "### 推奨が2つ\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 直す (推奨)\n- [ ] 直さない (推奨)\n\n",
            "### 選択肢が無い\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n直す\n\n",
            "## 総合評価\n\n**評価**: Comment\n",
        );
        let a = parse_ok(src).answer();

        assert_eq!(
            a.invalid.len(),
            3,
            "回答できない指摘の数が期待と異なる: {:?}",
            a.invalid
        );
        for (got, want) in a.invalid.iter().zip([
            "「区分が無い」: 区分",
            "「推奨が2つ」: 「(推奨)」の付いた選択肢が2個",
            "「選択肢が無い」: 対応方針にチェックボックスの選択肢がありません",
        ]) {
            assert!(
                got.contains(want),
                "回答できない理由 = {got:?}、含まれることを期待した文字列 = {want:?}"
            );
        }
        assert_eq!(
            a.output, src,
            "回答できない指摘があるのにドキュメントを書き換えた"
        );
    }

    #[test]
    fn 指摘が無ければ修正は要らない() {
        let a = parse_ok("# レビュー\n\n## ファイルごとのレビュー結果\n\n指摘はありません。\n\n## 総合評価\n\n**評価**: Approve\n")
            .answer();
        assert!(
            !a.blocked() && !a.needs_fix() && a.decisions.is_empty(),
            "指摘が無いのに回答がある: {a:?}"
        );
    }

    #[test]
    fn コードを変えない選択肢だけを先送りとする() {
        let tests = [
            ("修正案の通り修正する (推奨)", false),
            ("提案通り変更する", false),
            ("その他 (下の回答欄に記入)", false),
            ("後続タスクに積む (推奨)", true),
            ("現状のまま (理由を回答欄に記入)", true),
            ("現状維持", true),
            ("指摘を取り下げる", true),
            ("今回は見送る", true),
            ("対応しない (理由を回答欄に記入)", true),
            ("決定済みの方針どおり修正する (推奨)", false),
        ];
        for (text, want) in tests {
            let choice = Choice {
                text: text.to_owned(),
                checked: false,
                recommended: false,
                mark: 0,
            };
            assert_eq!(
                choice.defers(),
                want,
                "{text:?}を先送りとするかが期待と異なる"
            );
        }
    }

    #[test]
    fn 先送りの語で始まる修正の選択肢は先送りにしない() {
        for option in [
            "現状の実装を修正する",
            "後続処理のエラーハンドリングを追加する",
            "現状のままにせず修正する",
            "今回は見送る処理を削除する",
            "対応しない分岐を修正する",
            "後続タスクに積む前にエラー処理を直す",
        ] {
            let src = doc_with(
                SECTION_FINDINGS,
                &format!(
                    "### エラー処理\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] {option} (推奨)"
                ),
            );
            let a = parse_ok(&src).answer();
            assert!(
                !a.blocked() && a.needs_fix(),
                "{option:?}: 修正する選択肢を先送りにした: {a:?}"
            );
        }
    }

    #[test]
    fn チェックの入った指摘を返す() {
        let src = doc_with(
            SECTION_FINDINGS,
            concat!(
                "### 未回答\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\n\n",
                "### 回答済み\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [x] 修正する (推奨)\n- [ ] 現状のまま"
            ),
        );
        let d = parse(&src).expect("読み取れなかった");
        let titles: Vec<&str> = d.checked().iter().map(|f| f.title.as_str()).collect();
        assert_eq!(titles, ["回答済み"], "チェックの入った指摘が期待と異なる");
    }

    #[test]
    fn 人間の判断を待つ指摘を区分と場所を添えて表す() {
        let a = parse_ok(TEST_DOC).answer();
        let got: Vec<String> = a.pending.iter().map(Finding::describe).collect();
        assert!(
            got[0].ends_with("再検討提案」: 再検討提案"),
            "再検討提案の表し方が期待と異なる: {got:?}"
        );
        assert!(
            got[1].ends_with("「`internal/a.go`」: 要人間判断"),
            "要人間判断の表し方が期待と異なる: {got:?}"
        );
    }

    #[test]
    fn 引用内の対応方針の行番号とチェックする位置を保つ() {
        for prefix in ["> ", "> > "] {
            for (body, line) in [
                (
                    "**区分**: 自動対応可\n**対応方針**:\n- [ ] 修正する (推奨)",
                    6,
                ),
                (
                    "- 問題\n\n  **区分**: 自動対応可\n  **対応方針**:\n  - [ ] 修正する (推奨)",
                    8,
                ),
            ] {
                let body = body
                    .lines()
                    .map(|l| format!("{prefix}{l}"))
                    .collect::<Vec<_>>()
                    .join("\n");
                let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
                let d = parse_ok(&src);
                let a = d.answer();
                assert_eq!(d.findings.len(), 1, "指摘の数が異なる: {prefix:?}");
                assert_eq!(d.findings[0].line_no, line, "対応方針の行番号が異なる");
                assert!(
                    !a.blocked() && a.needs_fix(),
                    "引用内の回答の判定が異なる: {a:?}"
                );
                assert_eq!(
                    a.output,
                    src.replace("[ ]", "[x]"),
                    "チェック以外も書き換えた"
                );
            }
        }
    }

    #[test]
    fn 引用内で折り返した先送りの選択肢を読み取る() {
        for option in ["現状維持\n  (推奨)", "現状維持\n  (理由)\n  (推奨)"] {
            let body = format!("**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] {option}");
            let body = body
                .lines()
                .map(|l| format!("> > {l}"))
                .collect::<Vec<_>>()
                .join("\n");
            let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
            let d = parse_ok(&src);
            let a = d.answer();
            assert!(
                !a.blocked() && !a.needs_fix(),
                "先送りの判定が異なる: {a:?}"
            );
            assert_eq!(
                a.decisions[0].chosen[0].text,
                option
                    .split('\n')
                    .map(str::trim)
                    .collect::<Vec<_>>()
                    .join(" "),
                "引用の構文が本文に残った"
            );
            assert_eq!(
                a.output,
                src.replace("[ ]", "[x]"),
                "チェック以外も書き換えた"
            );
        }
    }

    #[test]
    fn 行の先頭をエスケープした選択肢と区分と対応方針は読まずに止める() {
        let tests = [
            (
                "選択肢",
                "**区分**: 自動対応可\n\n**対応方針**:\n\n- \\[ ] 修正する (推奨)\n- [ ] 現状維持",
                "「(推奨)」の付いた選択肢が0個あります",
            ),
            (
                "区分",
                "\\**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)",
                "区分 (自動対応可 / 要人間判断) を読み取れません",
            ),
            (
                "継続行の区分",
                "補足\n\\**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)",
                "区分 (自動対応可 / 要人間判断) を読み取れません",
            ),
            (
                "対応方針",
                "**区分**: 自動対応可\n\n\\**対応方針**:\n\n- [ ] 修正する (推奨)",
                "対応方針にチェックボックスの選択肢がありません",
            ),
            (
                "引用の中の選択肢",
                "> **区分**: 自動対応可\n>\n> **対応方針**:\n>\n> - \\[ ] 修正する (推奨)",
                "対応方針にチェックボックスの選択肢がありません",
            ),
        ];
        for (name, body, want) in tests {
            let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
            let a = parse_ok(&src).answer();
            assert!(
                a.blocked() && a.decisions.is_empty(),
                "{name}: エスケープした行を読んで回答した: {a:?}"
            );
            assert!(
                a.invalid.len() == 1 && a.invalid[0].contains(want),
                "{name}: 回答できない理由 = {:?}、含まれることを期待した文字列 = {want:?}",
                a.invalid
            );
            assert_eq!(a.output, src, "{name}: 停止したのに本文を書き換えた");
        }
    }

    #[test]
    fn 対応方針とリストの間のリンク定義で自動回答を止める() {
        for gap in [
            "",
            "<!-- 回答 -->",
            "[ref]: /x",
            "[ref]: /x\n[ref]: /y",
            "[ref]:\n  /x\n  \"参照\"",
        ] {
            for prefix in ["", "> ", "> > "] {
                for item in [false, true] {
                    let body = format!(
                        "**区分**: 自動対応可\n\n**対応方針**:\n\n{gap}\n\n- [ ] 修正する (推奨)"
                    );
                    let body = if item {
                        format!(
                            "- 問題\n\n{}",
                            body.lines()
                                .map(|l| format!("  {l}"))
                                .collect::<Vec<_>>()
                                .join("\n")
                        )
                    } else {
                        body
                    };
                    let body = body
                        .lines()
                        .map(|l| format!("{prefix}{l}"))
                        .collect::<Vec<_>>()
                        .join("\n");
                    let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
                    let a = parse_ok(&src).answer();
                    if gap.starts_with('[') {
                        assert!(
                            a.blocked() && a.decisions.is_empty() && a.invalid.len() == 1,
                            "リンク定義を越えて回答した: {gap:?}、{prefix:?}、{item}: {a:?}"
                        );
                        assert_eq!(a.output, src, "停止したのに本文を書き換えた");
                    } else {
                        assert!(
                            !a.blocked() && a.decisions.len() == 1,
                            "回答できなかった: {gap:?}、{prefix:?}、{item}: {a:?}"
                        );
                        assert_eq!(
                            a.output,
                            src.replace("[ ]", "[x]"),
                            "チェック以外も書き換えた"
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn 行末にタブがあるフェンスの後ろの指摘へ元の本文を保って回答する() {
        for marker in ["`", "~"] {
            for open in [3, 5] {
                for extra in [0, 1] {
                    for tail in ["\t", " \t", "\t ", " \t\t"] {
                        for context in ["本文", "引用", "リスト"] {
                            let body = format!(
                                "{}\n**区分**: 要人間判断\n**対応方針**:\n\n- [ ] コードの選択肢 (推奨)\t\n{}{tail}\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\t\n",
                                marker.repeat(open),
                                marker.repeat(open + extra),
                            );
                            let body = match context {
                                "引用" => body
                                    .lines()
                                    .map(|l| format!("> {l}"))
                                    .collect::<Vec<_>>()
                                    .join("\n"),
                                "リスト" => format!(
                                    "- 問題\n\n{}",
                                    body.lines()
                                        .map(|l| format!("  {l}"))
                                        .collect::<Vec<_>>()
                                        .join("\n")
                                ),
                                _ => body,
                            };
                            let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
                            let d = parse_ok(&src);
                            let a = d.answer();
                            assert_eq!(
                                d.findings.len(),
                                1,
                                "指摘の数が異なる: {marker}、{open}、{extra}、{tail:?}、{context}"
                            );
                            let f = &d.findings[0];
                            let policy = src.rfind("**対応方針**:").expect("対応方針が無い");
                            assert_eq!(
                                f.line_no,
                                src[..policy].matches('\n').count() + 1,
                                "対応方針の行番号が異なる"
                            );
                            assert_eq!(f.options.len(), 1, "コード内の選択肢も読み取った");
                            assert_eq!(
                                f.options[0].text, "修正する (推奨)",
                                "選択肢の本文が異なる"
                            );
                            assert!(
                                !a.blocked() && a.needs_fix() && a.decisions.len() == 1,
                                "回答の判定が異なる: {a:?}"
                            );
                            assert_eq!(
                                a.output,
                                src.replacen("[ ] 修正する", "[x] 修正する", 1),
                                "チェック以外の本文やタブを書き換えた"
                            );
                        }
                    }
                }
            }
        }
    }

    #[test]
    fn 総合評価の後のタブ付きフェンスで人間の判断待ちを隠さない() {
        for marker in ["`", "~"] {
            let src = format!(
                "## 総合評価\n\n**評価**: Comment\n\n## ファイルごとのレビュー結果\n\n{marker}{marker}{marker}\nコードの例\n{marker}{marker}{marker}\t\n\n### 問題\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\t\n"
            );
            for crlf in [false, true] {
                let input = if crlf {
                    src.replace('\n', "\r\n")
                } else {
                    src.clone()
                };
                let d = parse_ok(&input);
                let a = d.answer();
                assert_eq!(
                    d.verdict,
                    super::super::Verdict::Comment,
                    "総合評価が異なる"
                );
                assert_eq!(d.findings.len(), 1, "フェンスの後ろの指摘を読み飛ばした");
                assert!(
                    a.blocked()
                        && a.pending.len() == 1
                        && a.decisions.is_empty()
                        && a.invalid.is_empty(),
                    "人間の判断を待たずに進んだ: {a:?}"
                );
                assert_eq!(a.output, src, "本文のタブや改行を誤って書き換えた");
            }
        }
    }

    #[test]
    fn タブがあっても記号の数が足りないフェンスでは閉じない() {
        for marker in ["`", "~"] {
            for tail in ["\t", " \t", "\t ", " \t\t"] {
                let body = format!(
                    "{}\nコードの例\n{}{tail}\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)",
                    marker.repeat(4),
                    marker.repeat(3)
                );
                let src = doc_with(SECTION_FINDINGS, &format!("### 問題\n\n{body}"));
                let err = parse(&src).expect_err("閉じていないコードの中の総合評価を読んだ");
                assert_eq!(
                    err.to_string(),
                    "「総合評価」節に「**評価**:」の行がありません",
                    "読み取りエラーが異なる"
                );
            }
        }
    }
}
