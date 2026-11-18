//! 人間キューの項目に書かれた回答を読み、人間待ちから再開する状態遷移。
//!
//! 回答の形 (欄の数と種類・選んだ選択肢の数) と場面ごとの回答の中身を確かめてから、[`resume`] と同じ条件で再開する。
//! 人間が項目を直せば済む誤りと、再開の条件を満たさないことは、状態を変えずに [`Error::Rejected`] で理由を返す。
//! 呼び出し側は、その理由を項目に書いて差し戻す。

use korylus_workdoc::Error as DocError;
use korylus_workdoc::queue::{self, FieldKind, Form, REVIEW_LIMIT_CHOICES, Response};

use super::review::{choose_findings, extended_limit};
use super::{Error, PlanDocument, Project, resume};
use crate::state::{HumanAnswer, PlanState, WaitKind};

/// 人間を待っている状態stateを、公開された人間キューの項目の本文bodyに書かれた回答で再開する。
///
/// - 人が行うタスク・タスクの準備・やり直しの上限・作業報告書の確認: 公開だけで再開する。本文は読まない
/// - レビューの要人間判断の指摘: 指摘ごとに選んだ選択肢を最後のレビュードキュメントに書き、修正へ進める
/// - レビューの上限: 「上限を延ばして再レビューする」なら上限を1回延ばし、「打ち切って次へ進む」ならそのまま再開する
/// - セッションからの質問: 質問と回答の文を状態に残し、やり直すセッションの指示に添える
///
/// どの場面も、再開の条件は [`resume`] と同じである。
/// レビューの指摘への回答は、再開の条件を確かめる前にレビュードキュメントへ書くため、
/// 条件を満たさずに差し戻したあとも、ドキュメントには選んだ選択肢が残る。
pub fn resume_with(
    state: &mut PlanState,
    doc: &PlanDocument,
    project: &Project,
    body: &str,
) -> Result<(), Error> {
    let wait = state.wait.clone().ok_or(Error::NotWaiting)?;
    let mut resumed = state.clone();
    match wait.kind {
        WaitKind::HumanTask | WaitKind::Branch | WaitKind::RetryLimit | WaitKind::Report => {
            resume(&mut resumed, doc, project, None)?;
        }
        WaitKind::ReviewLimit => {
            let (form, responses) = read(body)?;
            let extend = match (form.fields.as_slice(), responses.as_slice()) {
                ([field], [Response::Choice(chosen)]) if is_review_limit(&field.kind) => {
                    *chosen == 0
                }
                _ => {
                    return Err(mismatch(
                        &form,
                        "「上限を延ばして再レビューする」「打ち切って次へ進む」の選択の欄が1つ",
                    ));
                }
            };
            let limit = if extend {
                Some(extended_limit(&resumed)?)
            } else {
                None
            };
            resume(&mut resumed, doc, project, limit)?;
        }
        WaitKind::Question => {
            let (form, responses) = read(body)?;
            let [Response::Text(text)] = responses.as_slice() else {
                return Err(mismatch(&form, "自由記述の欄が1つ"));
            };
            resume(&mut resumed, doc, project, None)?;
            // セッションの質問は、待ちの詳細の先頭に書かれている (`ask`)。
            let question = wait.details.first().cloned().unwrap_or(wait.reason);
            resumed.answers.push(HumanAnswer {
                question,
                answer: text.clone(),
            });
        }
        WaitKind::Review => {
            let (form, responses) = read(body)?;
            choose_findings(&resumed, project, &form, &responses)?;
            resume(&mut resumed, doc, project, None)?;
        }
    }
    *state = resumed;
    Ok(())
}

/// 項目の本文bodyから回答の欄を読み、欄ごとの回答の形を確かめる。
fn read(body: &str) -> Result<(Form, Vec<Response>), Error> {
    let form = queue::parse(body).map_err(rejected)?;
    let responses = form.answers().map_err(rejected)?;
    Ok((form, responses))
}

/// 欄の種類kindが、レビューの上限の選択の欄かを返す。
fn is_review_limit(kind: &FieldKind) -> bool {
    match kind {
        FieldKind::Choices(options) => {
            options.len() == REVIEW_LIMIT_CHOICES.len()
                && options
                    .iter()
                    .zip(REVIEW_LIMIT_CHOICES)
                    .all(|(option, original)| option.matches(original))
        }
        FieldKind::Text(_) => false,
    }
}

/// 回答の欄formが、場面の回答の欄expectedと合わないことを、差し戻す理由にする。
fn mismatch(form: &Form, expected: &str) -> Error {
    let headings: Vec<String> = form
        .fields
        .iter()
        .map(|field| format!("「{}」", field.heading))
        .collect();
    Error::Rejected(vec![format!(
        "回答の欄が、この場面の欄 ({expected}) と違います。回答の欄: {}",
        if headings.is_empty() {
            "無し".to_owned()
        } else {
            headings.join("・")
        }
    )])
}

/// 項目の本文を読み取れないか、回答の形が誤っていることを、差し戻す理由にする。
fn rejected(err: DocError) -> Error {
    match err {
        DocError::InvalidItem(reasons) | DocError::InvalidAnswer(reasons) => {
            Error::Rejected(reasons)
        }
        err => Error::Rejected(vec![err.to_string()]),
    }
}

#[cfg(test)]
mod tests {
    use super::super::tests::{checked, doc, implementing, project, rejected};
    use super::super::{Prepared, ask, finish_prepare, next};
    use super::*;
    use crate::state::Step;

    /// 人が行うタスク1-3で人間を待っている状態。
    fn human_waiting() -> PlanState {
        let plan = doc(&checked(&["1-1", "1-2"]));
        let mut state = implementing();
        state.step = None;
        state.task = None;
        next(&mut state, &plan).expect("次に行うことを決められなかった");
        finish_prepare(&mut state, &plan, Prepared::Human).expect("準備を記録できなかった");
        state
    }

    /// 実装のセッションが質問をして、人間を待っている状態。
    fn asking() -> PlanState {
        let mut state = implementing();
        ask(&mut state, "名前はどちらにしますか".to_owned()).expect("質問を記録できなかった");
        state
    }

    /// 「質問への回答」の欄にtextを書いた項目の本文。
    fn question_body(text: &str) -> String {
        format!(
            "## 状況\n\n- 場面: セッションからの質問\n\n## 回答\n\n### 質問への回答\n\n{text}\n"
        )
    }

    #[test]
    fn 回答の欄の無い場面は本文を読まずに公開だけで再開する() {
        let (_root, project) = project();
        let mut state = human_waiting();
        resume_with(
            &mut state,
            &doc(&checked(&["1-1", "1-2"])),
            &project,
            "読めない本文",
        )
        .expect("再開できなかった");
        assert_eq!(state.wait, None, "人間を待ったまま");
    }

    #[test]
    fn 質問への回答を状態に残して同じステップをやり直す() {
        let (_root, project) = project();
        let mut state = asking();
        resume_with(
            &mut state,
            &doc(&checked(&[])),
            &project,
            &question_body("短いほうにする"),
        )
        .expect("再開できなかった");
        assert_eq!(state.wait, None, "人間を待ったまま");
        assert_eq!(
            state.step,
            Some(Step::Implement),
            "同じステップをやり直さない"
        );
        assert_eq!(
            state.answers,
            [HumanAnswer {
                question: "名前はどちらにしますか".to_owned(),
                answer: "短いほうにする".to_owned(),
            }]
        );
    }

    #[test]
    fn 同じステップで続けて質問に答えても前の回答を残し_ステップを進めたら外す() {
        let (_root, project) = project();
        let mut state = asking();
        resume_with(
            &mut state,
            &doc(&checked(&[])),
            &project,
            &question_body("短いほうにする"),
        )
        .expect("1つ目の回答で再開できなかった");
        ask(&mut state, "テストの名前も変えますか".to_owned())
            .expect("2つ目の質問を記録できなかった");
        resume_with(
            &mut state,
            &doc(&checked(&[])),
            &project,
            &question_body("変える"),
        )
        .expect("2つ目の回答で再開できなかった");
        assert_eq!(
            state.answers,
            [
                HumanAnswer {
                    question: "名前はどちらにしますか".to_owned(),
                    answer: "短いほうにする".to_owned(),
                },
                HumanAnswer {
                    question: "テストの名前も変えますか".to_owned(),
                    answer: "変える".to_owned(),
                },
            ],
            "前の質問への回答が消えた"
        );

        super::super::start(&mut state, Some("1-1"), Step::Review);
        assert_eq!(state.answers, [], "ステップを進めても回答が残った");
    }

    #[test]
    fn 回答の形が誤っていれば状態を変えずに理由を返す() {
        let (_root, project) = project();
        let cases = [
            (question_body(""), "「質問への回答」: 回答の文がありません"),
            (
                "## 回答\n\n### 進め方\n\n- [x] A: 上限を延ばして再レビューする\n- [ ] B: 打ち切って次へ進む\n"
                    .to_owned(),
                "回答の欄が、この場面の欄 (自由記述の欄が1つ) と違います。回答の欄: 「進め方」",
            ),
            ("回答の節が無い本文\n".to_owned(), "「## 回答」の節がありません"),
        ];
        for (body, want) in cases {
            let mut state = asking();
            let before = state.clone();
            let reasons = rejected(resume_with(
                &mut state,
                &doc(&checked(&[])),
                &project,
                &body,
            ));
            assert_eq!(reasons, [want], "理由が期待と異なる");
            assert_eq!(state, before, "受け付けなかった回答で状態を変えた");
        }
    }

    #[test]
    fn 再開の条件を満たさなければ状態を変えずに理由を返す() {
        let (_root, project) = project();
        let mut state = implementing();
        state.wait = Some(crate::state::Wait {
            kind: WaitKind::Report,
            reason: "作業報告書の確認を待っています".to_owned(),
            details: Vec::new(),
        });
        state.step = None;
        state.task = None;
        let before = state.clone();
        let reasons = rejected(resume_with(&mut state, &doc(&checked(&[])), &project, ""));
        assert_eq!(
            reasons.last().map(String::as_str),
            Some("作業計画書: フェーズfb1のタスクが追加されていません")
        );
        assert_eq!(state, before, "再開できなかったのに状態を変えた");
    }
}
