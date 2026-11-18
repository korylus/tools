//! `korylusfactory queue` の処理。人間キューの項目の一覧と内容の表示と、回答の書き込み。
//!
//! Wikinoを読み書きするだけで、状態ファイルは読み書きしない。
//! どのプロジェクトのVMからでも、置き場にある全プロジェクトの項目を読める。
//! 回答は項目の下書きに書くだけで、公開しない。回答の確定 (公開) は、人間がWeb画面で行う。

use std::ffi::OsString;
use std::fmt::Write as _;

use jiff::civil::DateTime;
use korylusfactory_core::item::{self, FieldKind, Form, FormError, Response};
use serde::Serialize;

use super::{Error as StoreError, Location, Status, Store};

/// `korylusfactory queue` を続けられないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 置き場を読み書きできない。
    #[error(transparent)]
    Store(#[from] StoreError),

    /// 項目の本文から、回答の欄を読み取れない。
    #[error("人間キューの項目 {number} の回答の欄を読み取れません")]
    Form {
        /// 項目のページ番号。
        number: u64,
        /// 読み取れなかった理由。
        #[source]
        source: FormError,
    },

    /// 回答を書こうとした項目が、人間待ちではない。
    #[error("人間キューの項目 {0} は人間待ちではありません。公開されたか、片付けられています")]
    NotWaiting(u64),

    /// 回答を書こうとした項目に、回答の欄が無い。
    #[error(
        "人間キューの項目 {0} には回答の欄がありません。公開が要るかどうかを含め、項目の本文の案内に従ってください"
    )]
    NoFields(u64),

    /// 回答の引数が、項目の回答の欄と合わない。
    #[error("回答を書けません:\n- {}", .0.join("\n- "))]
    Answer(Vec<String>),
}

/// 一覧に並べる、人間待ちの項目。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Entry {
    /// 項目のページ番号。
    pub number: u64,
    /// 項目のタイトル。
    pub title: String,
    /// 項目を編集画面で開くURL。
    pub url: String,
}

/// 置き場locationの人間待ちの項目を、積んだ順 (ページ番号の順) に返す。
///
/// allが偽なら、プロジェクトprojectの項目 (タイトルが「<プロジェクト名> - 」で始まるもの) だけを返す。
pub fn list(
    store: &dyn Store,
    location: &Location,
    project: &str,
    all: bool,
) -> Result<Vec<Entry>, Error> {
    let prefix = format!("{project} - ");
    let mut entries: Vec<Entry> = store
        .list()?
        .into_iter()
        .filter(|item| all || item.title.starts_with(&prefix))
        .map(|item| Entry {
            url: location.edit_url(item.number),
            number: item.number,
            title: item.title,
        })
        .collect();
    entries.sort_by_key(|entry| entry.number);
    Ok(entries)
}

/// 人間待ちの項目entriesの一覧の表示を返す。
pub fn list_text(entries: &[Entry]) -> String {
    if entries.is_empty() {
        return "人間待ちの項目はありません\n".to_owned();
    }
    let mut out = String::new();
    for entry in entries {
        let _ = writeln!(out, "{} {}\n  {}", entry.number, entry.title, entry.url);
    }
    out
}

/// 項目の、Wikinoでの状態。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum State {
    /// 下書きがある (人間待ちか、差し戻し中)。
    Waiting,
    /// 公開済みで、下書きが無い。
    Answered,
    /// 下書きが無く、ページが未公開か無い。
    Missing,
}

/// 表示する項目。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Shown {
    /// 項目のページ番号。
    pub number: u64,
    /// 項目を編集画面で開くURL。
    pub url: String,
    /// 項目の状態。
    pub state: State,
    /// 項目のタイトル。無ければ `None`。
    pub title: Option<String>,
    /// 項目の本文。項目が無ければ `None`。
    pub body: Option<String>,
    /// 回答の欄。本文から読み取れなければ `None` で、理由を `form_error` に書く。
    pub fields: Option<Vec<FieldView>>,
    /// 回答の欄を読み取れない理由。
    pub form_error: Option<String>,
}

/// 回答の欄1つの表示。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum FieldView {
    /// 選択の欄。
    Choices {
        /// 欄の見出し。
        heading: String,
        /// 選択肢。
        choices: Vec<ChoiceView>,
    },
    /// 自由記述の欄。
    Text {
        /// 欄の見出し。
        heading: String,
        /// 今の回答の文。
        text: String,
    },
}

/// 選択肢1つの表示。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ChoiceView {
    /// 回答に使うラベル (`A` など)。
    pub label: String,
    /// 選択肢の本文。
    pub text: String,
    /// チェックが入っているか。
    pub checked: bool,
}

/// 置き場locationの項目numberを読み、内容と回答の欄を返す。
pub fn show(store: &dyn Store, location: &Location, number: u64) -> Result<Shown, Error> {
    let (state, title, body) = match store.read(number)? {
        Status::Drafted { title, body, .. } => (State::Waiting, title, Some(body)),
        Status::Published { title, body } => (State::Answered, title, Some(body)),
        Status::Missing => (State::Missing, None, None),
    };
    let (fields, form_error) = match body.as_deref().map(item::parse) {
        Some(Ok(form)) => (Some(views(&form)), None),
        Some(Err(err)) => (None, Some(err.to_string())),
        None => (None, None),
    };
    Ok(Shown {
        number,
        url: location.edit_url(number),
        state,
        title,
        body,
        fields,
        form_error,
    })
}

/// 回答の欄formを、表示の形にする。
fn views(form: &Form) -> Vec<FieldView> {
    form.fields
        .iter()
        .map(|field| match &field.kind {
            FieldKind::Choices(choices) => FieldView::Choices {
                heading: field.heading.clone(),
                choices: choices
                    .iter()
                    .map(|choice| ChoiceView {
                        label: choice.label.clone(),
                        text: choice.text.clone(),
                        checked: choice.checked,
                    })
                    .collect(),
            },
            FieldKind::Text(text) => FieldView::Text {
                heading: field.heading.clone(),
                text: text.clone(),
            },
        })
        .collect()
}

/// 項目shownの表示を返す。
pub fn show_text(shown: &Shown) -> String {
    let state = match shown.state {
        State::Waiting => "人間待ち",
        State::Answered => "回答済み",
        State::Missing => "見つかりません",
    };
    let mut out = format!(
        "項目 {}: {} ({state})\n{}\n",
        shown.number,
        shown.title.as_deref().unwrap_or("(タイトル無し)"),
        shown.url
    );
    if let Some(body) = &shown.body {
        let _ = write!(out, "\n{}\n", body.trim_end());
    }
    if let Some(err) = &shown.form_error {
        let _ = writeln!(out, "\n回答の欄を読み取れません:\n{err}");
    }
    if let Some(fields) = &shown.fields {
        let _ = writeln!(
            out,
            "\n回答の欄 (korylusfactory queue answer {} に、欄の順に回答を並べて渡します):",
            shown.number
        );
        if fields.is_empty() {
            out.push_str("無し (項目の本文の案内に従ってください)\n");
        }
        for (i, field) in fields.iter().enumerate() {
            match field {
                FieldView::Choices { heading, choices } => {
                    let _ = writeln!(out, "{}. {heading} (選択。ラベルで答えます)", i + 1);
                    for choice in choices {
                        let mark = if choice.checked { "x" } else { " " };
                        let _ = writeln!(out, "   [{mark}] {}: {}", choice.label, choice.text);
                    }
                }
                FieldView::Text { heading, text } => {
                    let _ = writeln!(out, "{}. {heading} (自由記述。文で答えます)", i + 1);
                    if !text.is_empty() {
                        let _ = writeln!(out, "   {}", text.replace('\n', "\n   "));
                    }
                }
            }
        }
    }
    out
}

/// 回答を書いた項目。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Answered {
    /// 項目のページ番号。
    pub number: u64,
    /// 項目を編集画面で開くURL。人間はここで回答を確かめて公開する。
    pub url: String,
    /// 回答の書き手。
    pub writer: String,
}

/// 置き場locationの項目numberの下書きに、欄の順に並べた回答argsを、書き手writerと日時atを記して書く。
///
/// 選択の欄にはラベル (`A` など、大文字と小文字を区別しない) を、自由記述の欄には回答の文を渡す。
/// 回答の形を確かめてから書き、公開はしない。
/// 回答の欄の無い項目 (公開だけで再開する場面と、公開の要らない知らせ) には、書く回答が無いため書かない。
pub fn answer(
    store: &dyn Store,
    location: &Location,
    number: u64,
    args: &[String],
    writer: &str,
    at: DateTime,
) -> Result<Answered, Error> {
    let Status::Drafted { body, etag, .. } = store.read(number)? else {
        return Err(Error::NotWaiting(number));
    };
    let form = item::parse(&body).map_err(|source| Error::Form { number, source })?;
    if form.fields.is_empty() {
        return Err(Error::NoFields(number));
    }
    let responses = responses(&form, args)?;
    let filled = form.fill(&responses, writer, at).map_err(|err| match err {
        FormError::InvalidItem(reasons) | FormError::InvalidAnswer(reasons) => {
            Error::Answer(reasons)
        }
        source => Error::Form { number, source },
    })?;
    store.update(number, &filled, &etag)?;
    Ok(Answered {
        number,
        url: location.edit_url(number),
        writer: writer.to_owned(),
    })
}

/// 欄の順に並べた回答の引数argsを、回答の欄formへの回答にする。
fn responses(form: &Form, args: &[String]) -> Result<Vec<Response>, Error> {
    if args.len() != form.fields.len() {
        let headings: Vec<String> = form
            .fields
            .iter()
            .map(|field| format!("「{}」", field.heading))
            .collect();
        return Err(Error::Answer(vec![format!(
            "回答が{}個あります。回答の欄は{}個です{}",
            args.len(),
            form.fields.len(),
            if headings.is_empty() {
                String::new()
            } else {
                format!(": {}", headings.join("・"))
            }
        )]));
    }
    let mut responses = Vec::new();
    let mut reasons = Vec::new();
    for (field, arg) in form.fields.iter().zip(args) {
        match &field.kind {
            FieldKind::Choices(choices) => {
                if let Some(index) = choices
                    .iter()
                    .position(|choice| choice.label.eq_ignore_ascii_case(arg.trim()))
                {
                    responses.push(Response::Choice(index));
                } else {
                    let labels: Vec<&str> =
                        choices.iter().map(|choice| choice.label.as_str()).collect();
                    reasons.push(format!(
                        "「{}」: 選択肢 {arg} がありません。{} のどれかを渡します",
                        field.heading,
                        labels.join("・")
                    ));
                }
            }
            FieldKind::Text(_) => responses.push(Response::Text(arg.clone())),
        }
    }
    if reasons.is_empty() {
        Ok(responses)
    } else {
        Err(Error::Answer(reasons))
    }
}

/// 回答を書いた項目answeredの表示を返す。公開を頼む文と、項目のURLを書く。
pub fn answered_text(answered: &Answered) -> String {
    format!(
        "項目 {} の下書きに回答を書きました (書き手: {})。\n内容を確かめて、Web画面でページを公開してください。公開すると回答が確定します:\n{}\n",
        answered.number, answered.writer, answered.url
    )
}

/// 回答の書き手を、実行している環境の環境変数varから見分ける。
///
/// Claude CodeとCodexのセッションは、それぞれのコマンドの実行に目印の環境変数を渡す。
/// どちらでもなければ、端末で人間が実行したものとする。
pub fn writer(var: impl Fn(&str) -> Option<OsString>) -> &'static str {
    if var("CLAUDECODE").is_some() {
        "Claude Codeのセッション"
    } else if var("CODEX_THREAD_ID").is_some() {
        "Codexのセッション"
    } else {
        "端末"
    }
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::collections::BTreeMap;

    use super::super::{Error as StoreError, Listed, Status, Store};
    use super::*;

    /// 置き場の偽物。ページ番号ごとに項目の状態を持ち、書き換えを記録する。
    #[derive(Default)]
    struct Fake {
        items: BTreeMap<u64, Status>,
        updates: RefCell<Vec<(u64, String, String)>>,
    }

    impl Store for Fake {
        fn create(&self, _title: &str, _body: &str) -> Result<u64, StoreError> {
            panic!("queueのコマンドが項目を作った")
        }

        fn list(&self) -> Result<Vec<Listed>, StoreError> {
            Ok(self
                .items
                .iter()
                .rev()
                .filter_map(|(&number, status)| match status {
                    Status::Drafted {
                        title: Some(title), ..
                    } => Some(Listed {
                        number,
                        title: title.clone(),
                    }),
                    _ => None,
                })
                .collect())
        }

        fn read(&self, number: u64) -> Result<Status, StoreError> {
            Ok(self.items.get(&number).cloned().unwrap_or(Status::Missing))
        }

        fn delete(&self, _number: u64, _etag: &str) -> Result<(), StoreError> {
            panic!("queueのコマンドが下書きを削除した")
        }

        fn reopen(&self, _number: u64, _body: &str) -> Result<(), StoreError> {
            panic!("queueのコマンドが差し戻した")
        }

        fn update(&self, number: u64, body: &str, etag: &str) -> Result<(), StoreError> {
            self.updates
                .borrow_mut()
                .push((number, body.to_owned(), etag.to_owned()));
            Ok(())
        }

        fn find(&self, _title: &str) -> Result<Option<u64>, StoreError> {
            panic!("queueのコマンドが項目を探した")
        }
    }

    fn location() -> Location {
        Location {
            space: "shimbaco".to_owned(),
            topic: 12,
        }
    }

    /// 選択の欄と自由記述の欄を1つずつ持つ項目の本文。
    const BODY: &str = "## 状況\n\n- プロジェクト: mewst\n\n## 回答\n\n回答を書き終えたら、このページを公開してください。\n\n### 指摘1: 名前\n\n- [ ] A: 直す (推奨)\n- [ ] B: 現状維持\n\n### 質問への回答\n";

    fn drafted(title: &str, body: &str) -> Status {
        Status::Drafted {
            title: Some(title.to_owned()),
            body: body.to_owned(),
            etag: "W/\"1\"".to_owned(),
        }
    }

    fn fake() -> Fake {
        let mut fake = Fake::default();
        fake.items.insert(
            30,
            drafted("mewst - demo - 質問 - 2026-11-17 21時04分05秒", BODY),
        );
        fake.items.insert(
            31,
            drafted("annict - other - 質問 - 2026-11-17 21時05分00秒", BODY),
        );
        fake.items.insert(
            29,
            drafted("mewst-web - demo - 質問 - 2026-11-17 21時00分00秒", BODY),
        );
        fake.items.insert(
            28,
            Status::Published {
                title: Some("mewst - demo - 回答済み".to_owned()),
                body: BODY.to_owned(),
            },
        );
        fake
    }

    fn at() -> DateTime {
        "2026-11-17T22:00:00"
            .parse()
            .expect("日時を読み取れなかった")
    }

    #[test]
    fn 一覧は既定でこのプロジェクトの人間待ちの項目を積んだ順に返す() {
        let numbers = |all| -> Vec<u64> {
            list(&fake(), &location(), "mewst", all)
                .expect("一覧を作れなかった")
                .iter()
                .map(|entry| entry.number)
                .collect()
        };
        assert_eq!(numbers(false), [30], "ほかのプロジェクトの項目を含めた");
        assert_eq!(numbers(true), [29, 30, 31]);
        let entries = list(&fake(), &location(), "mewst", false).expect("一覧を作れなかった");
        assert_eq!(
            list_text(&entries),
            "30 mewst - demo - 質問 - 2026-11-17 21時04分05秒\n  https://wikino.app/s/shimbaco/pages/30/edit\n"
        );
        assert_eq!(list_text(&[]), "人間待ちの項目はありません\n");
    }

    #[test]
    fn 項目の内容と回答の欄を表示する() {
        let shown = show(&fake(), &location(), 30).expect("項目を読めなかった");
        assert_eq!(shown.state, State::Waiting);
        assert_eq!(
            shown.fields,
            Some(vec![
                FieldView::Choices {
                    heading: "指摘1: 名前".to_owned(),
                    choices: vec![
                        ChoiceView {
                            label: "A".to_owned(),
                            text: "直す (推奨)".to_owned(),
                            checked: false,
                        },
                        ChoiceView {
                            label: "B".to_owned(),
                            text: "現状維持".to_owned(),
                            checked: false,
                        },
                    ],
                },
                FieldView::Text {
                    heading: "質問への回答".to_owned(),
                    text: String::new(),
                },
            ])
        );
        let text = show_text(&shown);
        assert!(
            text.starts_with("項目 30: mewst - demo - 質問 - 2026-11-17 21時04分05秒 (人間待ち)\nhttps://wikino.app/s/shimbaco/pages/30/edit\n\n## 状況\n"),
            "表示 = {text}"
        );
        assert!(
            text.ends_with("回答の欄 (korylusfactory queue answer 30 に、欄の順に回答を並べて渡します):\n1. 指摘1: 名前 (選択。ラベルで答えます)\n   [ ] A: 直す (推奨)\n   [ ] B: 現状維持\n2. 質問への回答 (自由記述。文で答えます)\n"),
            "表示 = {text}"
        );

        assert_eq!(
            show(&fake(), &location(), 28)
                .expect("項目を読めなかった")
                .state,
            State::Answered
        );
        let missing = show(&fake(), &location(), 99).expect("項目を読めなかった");
        assert_eq!((missing.state, missing.body), (State::Missing, None));

        let mut broken = fake();
        broken
            .items
            .insert(32, drafted("mewst - 壊れた項目", "本文だけ\n"));
        let shown = show(&broken, &location(), 32).expect("項目を読めなかった");
        assert_eq!(shown.fields, None);
        assert!(
            show_text(&shown).contains("回答の欄を読み取れません:\n「## 回答」の節がありません"),
            "表示 = {}",
            show_text(&shown)
        );
    }

    #[test]
    fn 回答欄の無い項目は公開で再開する場面と公開不要の知らせを断定せず本文へ案内する() {
        for scene in [
            item::Scene::HumanTask,
            item::Scene::PlanUnreadable,
            item::Scene::NotMerged,
        ] {
            let body = item::Item {
                project: "mewst".to_owned(),
                plan: item::PlanLink {
                    name: "demo".to_owned(),
                    title: "デモ 作業計画書".to_owned(),
                    url: None,
                },
                scene,
                task: None,
                reason: "対応を待っています".to_owned(),
                details: Vec::new(),
                at: at(),
            }
            .body();
            let mut store = Fake::default();
            store
                .items
                .insert(30, drafted("mewst - demo - 知らせ", &body));
            let shown = show(&store, &location(), 30).expect("項目を読めなかった");
            assert_eq!(shown.fields, Some(Vec::new()), "回答欄が無い場面ではない");
            let text = show_text(&shown);
            assert!(
                text.ends_with("無し (項目の本文の案内に従ってください)\n"),
                "表示 = {text}"
            );
            assert!(
                !text.contains("公開だけで回答が確定します"),
                "公開不要の場面にも公開を促した"
            );
            assert!(
                text.contains(body.trim_end()),
                "場面ごとの本文の案内を落とした"
            );
        }
    }

    #[test]
    fn 回答欄の無い項目には回答を書かずに本文の案内に従うよう返す() {
        for scene in [
            item::Scene::HumanTask,
            item::Scene::PlanUnreadable,
            item::Scene::NotMerged,
        ] {
            let body = item::Item {
                project: "mewst".to_owned(),
                plan: item::PlanLink {
                    name: "demo".to_owned(),
                    title: "デモ 作業計画書".to_owned(),
                    url: None,
                },
                scene,
                task: None,
                reason: "対応を待っています".to_owned(),
                details: Vec::new(),
                at: at(),
            }
            .body();
            let mut store = Fake::default();
            store
                .items
                .insert(30, drafted("mewst - demo - 知らせ", &body));
            let err = answer(&store, &location(), 30, &[], "端末", at())
                .expect_err("回答の欄の無い項目に書いた");
            assert!(matches!(err, Error::NoFields(30)), "エラー = {err:?}");
            assert!(
                err.to_string().contains("項目の本文の案内に従ってください"),
                "エラー = {err}"
            );
            assert!(
                store.updates.borrow().is_empty(),
                "回答の欄の無い項目の下書きを書き換えた"
            );
        }
    }

    #[test]
    fn 欄の順に並べた回答を書き手と時刻を記して下書きに書く() {
        let fake = fake();
        let answered = answer(
            &fake,
            &location(),
            30,
            &["b".to_owned(), "短いほうにする".to_owned()],
            "端末",
            at(),
        )
        .expect("回答を書けなかった");
        assert_eq!(
            answered,
            Answered {
                number: 30,
                url: "https://wikino.app/s/shimbaco/pages/30/edit".to_owned(),
                writer: "端末".to_owned(),
            }
        );
        let updates = fake.updates.borrow();
        let (number, body, etag) = &updates[0];
        assert_eq!(
            (*number, etag.as_str()),
            (30, "W/\"1\""),
            "読んだ版で書いていない"
        );
        for want in [
            "回答の書き手: 端末 (2026-11-17 22:00)",
            "- [ ] A: 直す (推奨)\n- [x] B: 現状維持",
            "### 質問への回答\n\n短いほうにする\n",
        ] {
            assert!(body.contains(want), "{want:?} を書いていない:\n{body}");
        }
        assert!(
            answered_text(&answered).contains("Web画面でページを公開してください"),
            "公開を頼んでいない"
        );
    }

    #[test]
    fn 欄と合わない回答と人間待ちでない項目には書かない() {
        let cases: [(u64, Vec<&str>, &str); 4] = [
            (
                30,
                vec!["A"],
                "回答が1個あります。回答の欄は2個です: 「指摘1: 名前」・「質問への回答」",
            ),
            (
                30,
                vec!["C", "文"],
                "「指摘1: 名前」: 選択肢 C がありません。A・B のどれかを渡します",
            ),
            (30, vec!["A", "## 見出し"], "書き込んだ回答を読み直すと"),
            (
                28,
                vec!["A", "文"],
                "人間キューの項目 28 は人間待ちではありません",
            ),
        ];
        for (number, args, want) in cases {
            let fake = fake();
            let args: Vec<String> = args.into_iter().map(str::to_owned).collect();
            let err = answer(&fake, &location(), number, &args, "端末", at())
                .expect_err("書けない回答を書いた");
            assert!(
                err.to_string().contains(want),
                "エラー = {err}、期待値 = {want:?} を含むエラー"
            );
            assert!(fake.updates.borrow().is_empty(), "書けない回答を書いた");
        }
    }

    #[test]
    fn 書き手をセッションの環境変数から見分ける() {
        let env = |name: &'static str| move |var: &str| (var == name).then(|| OsString::from("1"));
        assert_eq!(writer(env("CLAUDECODE")), "Claude Codeのセッション");
        assert_eq!(writer(env("CODEX_THREAD_ID")), "Codexのセッション");
        assert_eq!(writer(|_: &str| None), "端末");
    }
}
