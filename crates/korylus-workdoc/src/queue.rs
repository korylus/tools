//! 人間キューの項目 (Wikinoのページ) の本文を作り、人間が書いた回答を読み書きする。
//!
//! 項目は、人間の判断を待つ場面ごとにディレクターが積む。
//! 本文は「## 状況」と「## 回答」の節からなり、差し戻したときは先頭に「## 差し戻し」の節を足す。
//! 回答として読むのは、「## 回答」の節のトップレベルの「### 」の見出しの欄だけにする。
//! セッションやリポジトリから来た文は、リストの項目か引用の中に書き、欄の見出しや選択肢として読まれないようにする。

mod form;
mod literal;

use std::fmt::Write as _;

use jiff::civil::DateTime;

pub use form::{Choice, Field, FieldKind, Form, Response, parse, send_back};

/// Wikinoのページのタイトルの長さの上限 (文字数)。
const TITLE_MAX: usize = 200;

/// ディレクターが積んだ項目の書き手。
const DIRECTOR: &str = "ディレクター";

/// 回答の欄の節の見出し。
const ANSWER_SECTION: &str = "回答";

/// 回答の欄を持つ場面で、公開を頼む文。
const PUBLISH: &str = "回答を書き終えたら、このページを公開してください。\n公開すると回答が確定し、ディレクターが再開します。";

/// 回答の欄を持たない場面で、公開を頼む文。
const RESUME: &str = "公開すると、ディレクターが再開します。";

/// 自由記述の欄の見出し。
const QUESTION_FIELD: &str = "質問への回答";

/// レビューの上限の欄の見出し。
const REVIEW_LIMIT_FIELD: &str = "進め方";

/// レビューの上限の欄の選択肢。位置0が上限を延ばす選択肢、位置1が打ち切る選択肢。
pub const REVIEW_LIMIT_CHOICES: [&str; 2] = ["上限を延ばして再レビューする", "打ち切って次へ進む"];

/// 項目を積む場面。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Scene {
    /// 人が行うタスク。
    HumanTask,
    /// タスクの対象を求められないか、ブランチを準備できない。
    Branch,
    /// レビューに要人間判断の指摘がある。値は、回答を求める指摘。
    Review(Vec<Finding>),
    /// レビューが上限の回数に達しても、Request Changesだった。
    ReviewLimit,
    /// 同じステップのやり直しが、上限の回数に達した。
    RetryLimit,
    /// セッションが、人間の判断が要ると返して終わった。
    Question,
    /// 作業報告書を、人間が確認する。
    Report,
    /// ロードマップの並びの先頭の作業計画書を読み取れない (知らせの項目)。
    PlanUnreadable,
    /// 完了とみなす節にある作業計画書のブランチが、取り込まれていない (知らせの項目)。
    NotMerged,
}

impl Scene {
    /// 場面の表示名を返す。
    pub fn label(&self) -> &'static str {
        match self {
            Scene::HumanTask => "人が行うタスク",
            Scene::Branch => "タスクの準備",
            Scene::Review(_) => "レビューの要人間判断の指摘",
            Scene::ReviewLimit => "レビューの上限",
            Scene::RetryLimit => "やり直しの上限",
            Scene::Question => "セッションからの質問",
            Scene::Report => "作業報告書の確認",
            Scene::PlanUnreadable => "作業計画書を読み取れない",
            Scene::NotMerged => "ブランチが取り込まれていない",
        }
    }

    /// 「## 回答」の節の先頭に書く、人間がすることの案内を返す。
    fn guide(&self) -> String {
        match self {
            Scene::Review(_) => {
                format!("指摘ごとに、選択肢から1つにチェックを付けてください。\n{PUBLISH}")
            }
            Scene::ReviewLimit => format!("選択肢から1つにチェックを付けてください。\n{PUBLISH}"),
            Scene::Question => {
                format!("「{QUESTION_FIELD}」の見出しの下に、回答を書いてください。\n{PUBLISH}")
            }
            Scene::HumanTask => format!(
                "タスクを済ませ、作業計画書のタスクにチェックを付けてから、このページを公開してください。\n{RESUME}"
            ),
            Scene::Branch | Scene::RetryLimit => format!(
                "理由に書かれた原因を取り除いてから、このページを公開してください。\n{RESUME}"
            ),
            Scene::Report => format!(
                "作業報告書を確認し、フィードバック対応のフェーズを足してから、このページを公開してください。\n{RESUME}\n作業計画書を完了とするときは、このページを公開せずに、ロードマップで伝えてください。"
            ),
            Scene::PlanUnreadable | Scene::NotMerged => "この項目は知らせです。公開は要りません。\n理由に書かれた原因を取り除くと、ディレクターがこの項目を片付けます。".to_owned(),
        }
    }
}

/// レビューの要人間判断の指摘1件の、回答を求める内容。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Finding {
    /// 指摘の見出し。
    pub title: String,
    /// 指摘の要点 (Markdown)。引用として書く。
    pub summary: String,
    /// レビュードキュメントの対応方針の選択肢。書かれた順に並べる。
    pub options: Vec<String>,
}

/// 項目が指す作業計画書。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlanLink {
    /// 項目のタイトルに使う名前 (計画書名。読み取れないときはページのタイトル)。
    pub name: String,
    /// 作業計画書のページのタイトル。
    pub title: String,
    /// 作業計画書のページのURL。分からなければ `None`。
    pub url: Option<String>,
}

/// ディレクターが積む項目1件。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Item {
    /// プロジェクト名。
    pub project: String,
    /// 待っている作業計画書。
    pub plan: PlanLink,
    /// 待っている場面。
    pub scene: Scene,
    /// 待っているタスクの番号。タスクに依らない待ちでは `None`。
    pub task: Option<String>,
    /// 人間に伝える理由。
    pub reason: String,
    /// 理由の詳細 (人間にしてほしいこと・セッションの質問など)。
    pub details: Vec<String>,
    /// 項目を積んだ日時。
    pub at: DateTime,
}

impl Item {
    /// 項目のページのタイトルを返す。
    ///
    /// トピックの中で重複しないよう、積んだ日時を秒まで入れる。
    /// Wikinoのタイトルに使えない文字 (`:`・`/`・`\`) は `-` に置き換える。
    /// 場面と日時を残して、プロジェクト名と計画書名を200文字に収まる長さへ切り詰める。
    pub fn title(&self) -> String {
        let tail = format!(
            " - {} - {}",
            self.scene.label(),
            self.at.strftime("%Y-%m-%d %H時%M分%S秒"),
        );
        let budget = TITLE_MAX - tail.chars().count() - " - ".len();
        let plan = one_line(&self.plan.name);
        // 両方の名前が長いときも、それぞれに文字数を割り当てる。
        let project_max = budget - plan.chars().count().min(budget / 2);
        let project: String = one_line(&self.project).chars().take(project_max).collect();
        let plan: String = plan
            .chars()
            .take(budget - project.chars().count())
            .collect();
        format!("{} - {}{tail}", project.trim(), plan.trim()).replace([':', '/', '\\'], "-")
    }

    /// 項目のページの本文を返す。
    pub fn body(&self) -> String {
        let mut out = String::from("## 状況\n\n");
        let _ = writeln!(out, "- プロジェクト: {}", one_line(&self.project));
        let plan = match &self.plan.url {
            Some(url) => format!("[{}]({})", link_text(&self.plan.title), link_url(url)),
            None => one_line(&self.plan.title),
        };
        let _ = writeln!(out, "- 作業計画書: {plan}");
        match &self.task {
            Some(task) => {
                let _ = writeln!(
                    out,
                    "- 場面: {} (タスク {})",
                    self.scene.label(),
                    one_line(task)
                );
            }
            None => {
                let _ = writeln!(out, "- 場面: {}", self.scene.label());
            }
        }
        let _ = writeln!(out, "- 書き手: {DIRECTOR} ({})", stamp(self.at));
        push_item(&mut out, "", &format!("理由: {}", self.reason));
        for detail in &self.details {
            push_item(&mut out, "  ", detail);
        }

        let _ = write!(out, "\n## {ANSWER_SECTION}\n\n{}\n", self.scene.guide());
        match &self.scene {
            Scene::Review(findings) => {
                for (i, finding) in findings.iter().enumerate() {
                    let heading = format!("指摘{}: {}", i + 1, finding.title);
                    push_choices(&mut out, &heading, &finding.summary, &finding.options);
                }
            }
            Scene::ReviewLimit => {
                let options = REVIEW_LIMIT_CHOICES.map(str::to_owned);
                push_choices(&mut out, REVIEW_LIMIT_FIELD, "", &options);
            }
            Scene::Question => {
                let _ = writeln!(out, "\n### {QUESTION_FIELD}");
            }
            _ => {}
        }
        literal::escape_wikilinks(&out)
    }
}

/// 本文に書く日時の表記 (分まで) を返す。
fn stamp(at: DateTime) -> String {
    at.strftime("%Y-%m-%d %H:%M").to_string()
}

/// 選択の欄を書く。
fn push_choices(out: &mut String, heading: &str, summary: &str, options: &[String]) {
    let _ = writeln!(out, "\n### {}\n", one_line(heading));
    if !summary.trim().is_empty() {
        push_quote(out, summary);
        out.push('\n');
    }
    for (i, option) in options.iter().enumerate() {
        let _ = writeln!(out, "- [ ] {}: {}", form::label(i), one_line(option));
    }
}

/// 外から来た文 `text` を、字下げ `indent` のリストの項目として書く。
///
/// 続きの行を項目の中に字下げし、見出し・コードブロック・HTMLのブロックが項目の外へ出ないようにする。
fn push_item(out: &mut String, indent: &str, text: &str) {
    for (i, line) in text.trim().lines().enumerate() {
        if i == 0 {
            let _ = writeln!(out, "{indent}- {line}");
        } else if line.trim().is_empty() {
            out.push('\n');
        } else {
            let _ = writeln!(out, "{indent}  {line}");
        }
    }
}

/// 外から来た文 `text` を、引用として書く。
///
/// すべての行に引用の印を付け、見出し・選択肢・コードブロックが引用の外へ出ないようにする。
fn push_quote(out: &mut String, text: &str) {
    for line in text.trim().lines() {
        if line.trim().is_empty() {
            out.push_str(">\n");
        } else {
            let _ = writeln!(out, "> {line}");
        }
    }
}

/// 改行を空白に置き換え、前後の空白を除いた1行にする。
fn one_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// リンクの文字列に書くため、角括弧とバックスラッシュをエスケープする。
fn link_text(text: &str) -> String {
    one_line(text)
        .replace('\\', r"\\")
        .replace('[', r"\[")
        .replace(']', r"\]")
}

/// リンク先に書くため、空白と丸括弧をパーセントエンコードする。
fn link_url(url: &str) -> String {
    url.trim()
        .replace(' ', "%20")
        .replace('(', "%28")
        .replace(')', "%29")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 例の項目の日時 (2026-11-17 21:04:05)。
    pub(super) fn at() -> DateTime {
        jiff::civil::date(2026, 11, 17).at(21, 4, 5, 0)
    }

    /// 場面 `scene` の例の項目を返す。
    pub(super) fn item(scene: Scene) -> Item {
        Item {
            project: "mewst".to_owned(),
            plan: PlanLink {
                name: "human-queue".to_owned(),
                title: "人間キュー 作業計画書".to_owned(),
                url: Some("https://wikino.app/s/example/pages/12".to_owned()),
            },
            scene,
            task: Some("1-2".to_owned()),
            reason: "レビューに要人間判断の指摘があります".to_owned(),
            details: vec![
                "レビュードキュメント: docs/private/reviews/human-queue-1-2-001.md".to_owned(),
            ],
            at: at(),
        }
    }

    /// 指摘2件のレビューの場面を返す。
    pub(super) fn review() -> Scene {
        Scene::Review(vec![
            Finding {
                title: "エラー時のリトライ回数".to_owned(),
                summary: "通信の失敗をリトライせずに返している".to_owned(),
                options: vec!["3回にする (推奨)".to_owned(), "リトライしない".to_owned()],
            },
            Finding {
                title: "`internal/a.go`".to_owned(),
                summary: String::new(),
                options: vec![
                    "slogに変える (推奨)".to_owned(),
                    "後続タスクに積む".to_owned(),
                    "その他 (下の回答欄に記入)".to_owned(),
                ],
            },
        ])
    }

    #[test]
    fn タイトルにプロジェクト_計画書名_場面_積んだ日時を並べる() {
        assert_eq!(
            item(review()).title(),
            "mewst - human-queue - レビューの要人間判断の指摘 - 2026-11-17 21時04分05秒",
            "タイトルが期待と異なる"
        );
    }

    #[test]
    fn タイトルに使えない文字を置き換え_長い計画書名を切り詰める() {
        let mut it = item(Scene::PlanUnreadable);
        it.project = "a:b".to_owned();
        it.plan.name = format!("x/y\\z {}", "長".repeat(200));
        let title = it.title();

        assert!(
            !title.contains([':', '/', '\\']),
            "タイトルに使えない文字が残った: {title}"
        );
        assert!(
            title.starts_with("a-b - x-y-z 長"),
            "置き換えた結果が期待と異なる: {title}"
        );
        assert!(
            title.chars().count() <= 200,
            "タイトルが200文字を超えた: {}文字",
            title.chars().count()
        );
    }

    #[test]
    fn 名前が両方長くても_場面と日時を残してタイトルを200文字に収める() {
        for scene in [
            Scene::HumanTask,
            Scene::Branch,
            review(),
            Scene::ReviewLimit,
            Scene::RetryLimit,
            Scene::Question,
            Scene::Report,
            Scene::PlanUnreadable,
            Scene::NotMerged,
        ] {
            for (project, plan) in [(100, 100), (300, 1), (1, 300)] {
                let mut it = item(scene.clone());
                it.project = "あ😀".repeat(project);
                it.plan.name = "計画".repeat(plan);
                let title = it.title();

                assert_eq!(title.chars().count(), 200, "タイトルの長さが違う: {title}");
                assert!(title.starts_with('あ'), "プロジェクト名が無い: {title}");
                assert!(title.contains(" - 計"), "計画書名が無い: {title}");
                let tail = format!(" - {} - 2026-11-17 21時04分05秒", scene.label());
                assert!(title.ends_with(&tail), "場面か日時を切り詰めた: {title}");
            }
        }
    }

    #[test]
    fn レビューの指摘の項目に_指摘ごとの選択の欄を書く() {
        let want = "## 状況

- プロジェクト: mewst
- 作業計画書: [人間キュー 作業計画書](https://wikino.app/s/example/pages/12)
- 場面: レビューの要人間判断の指摘 (タスク 1-2)
- 書き手: ディレクター (2026-11-17 21:04)
- 理由: レビューに要人間判断の指摘があります
  - レビュードキュメント: docs/private/reviews/human-queue-1-2-001.md

## 回答

指摘ごとに、選択肢から1つにチェックを付けてください。
回答を書き終えたら、このページを公開してください。
公開すると回答が確定し、ディレクターが再開します。

### 指摘1: エラー時のリトライ回数

> 通信の失敗をリトライせずに返している

- [ ] A: 3回にする (推奨)
- [ ] B: リトライしない

### 指摘2: `internal/a.go`

- [ ] A: slogに変える (推奨)
- [ ] B: 後続タスクに積む
- [ ] C: その他 (下の回答欄に記入)
";
        assert_eq!(item(review()).body(), want, "本文が期待と異なる");
    }

    #[test]
    fn 質問の項目に_自由記述の欄を書く() {
        let mut it = item(Scene::Question);
        it.task = None;
        it.reason = "実装のセッションが、人間の判断を求めて終わりました".to_owned();
        it.details = vec![
            "リトライの回数を決めてください\n\n候補は3回か、リトライしないかです".to_owned(),
            "対応してから再開してください".to_owned(),
        ];
        let want = "## 状況

- プロジェクト: mewst
- 作業計画書: [人間キュー 作業計画書](https://wikino.app/s/example/pages/12)
- 場面: セッションからの質問
- 書き手: ディレクター (2026-11-17 21:04)
- 理由: 実装のセッションが、人間の判断を求めて終わりました
  - リトライの回数を決めてください

    候補は3回か、リトライしないかです
  - 対応してから再開してください

## 回答

「質問への回答」の見出しの下に、回答を書いてください。
回答を書き終えたら、このページを公開してください。
公開すると回答が確定し、ディレクターが再開します。

### 質問への回答
";
        assert_eq!(it.body(), want, "本文が期待と異なる");
    }

    #[test]
    fn レビューの上限の項目に_進め方の選択の欄を書く() {
        let form = parse(&item(Scene::ReviewLimit).body()).expect("項目を読み取れなかった");

        assert_eq!(form.fields.len(), 1, "欄の数が期待と異なる");
        assert_eq!(
            form.fields[0].heading, REVIEW_LIMIT_FIELD,
            "欄の見出しが期待と異なる"
        );
        let FieldKind::Choices(options) = &form.fields[0].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[0].kind);
        };
        let texts: Vec<&str> = options.iter().map(|o| o.text.as_str()).collect();
        assert_eq!(texts, REVIEW_LIMIT_CHOICES, "選択肢が期待と異なる");
    }

    #[test]
    fn 回答の欄の無い場面は_案内だけを書く() {
        for (scene, want) in [
            (Scene::HumanTask, "作業計画書のタスクにチェックを付けてから"),
            (Scene::Branch, "原因を取り除いてから"),
            (Scene::RetryLimit, "原因を取り除いてから"),
            (Scene::Report, "フィードバック対応のフェーズを足してから"),
            (Scene::PlanUnreadable, "公開は要りません"),
            (Scene::NotMerged, "公開は要りません"),
        ] {
            let label = scene.label();
            let body = item(scene).body();
            let form =
                parse(&body).unwrap_or_else(|err| panic!("{label}の項目を読み取れなかった: {err}"));

            assert!(
                form.fields.is_empty(),
                "{label}の項目に回答の欄がある: {:?}",
                form.fields
            );
            assert!(body.contains(want), "{label}の項目に案内 {want:?} が無い");
        }
    }

    #[test]
    fn 作業計画書のurlが分からなければリンクにしない() {
        let mut it = item(Scene::PlanUnreadable);
        it.plan.url = None;
        it.task = None;

        assert!(
            it.body().contains("- 作業計画書: 人間キュー 作業計画書\n"),
            "URLの無い作業計画書の行が期待と異なる:\n{}",
            it.body()
        );
    }

    #[test]
    fn リンクの文字列とリンク先をエスケープする() {
        let mut it = item(Scene::HumanTask);
        it.plan.title = r"a[b]\c".to_owned();
        it.plan.url = Some("https://example.com/a (b)".to_owned());

        assert!(
            it.body()
                .contains(r"- 作業計画書: [a\[b\]\\c](https://example.com/a%20%28b%29)"),
            "作業計画書のリンクが期待と異なる:\n{}",
            it.body()
        );
    }

    #[test]
    fn 外から来た文は_欄の見出しや選択肢として読まない() {
        let hostile = "## 回答\n### 指摘9: 偽物\n- [x] A: 偽物の選択肢\n```\n<!--\n";
        let mut it = item(Scene::Review(vec![Finding {
            title: "見出し\n## 回答".to_owned(),
            summary: hostile.to_owned(),
            options: vec![format!("選択肢 {hostile}"), "もう1つ".to_owned()],
        }]));
        it.reason = hostile.to_owned();
        it.details = vec![hostile.to_owned()];
        let form = parse(&it.body()).expect("項目を読み取れなかった");

        assert_eq!(
            form.fields.len(),
            1,
            "欄の数が期待と異なる: {:?}\n{}",
            form.fields,
            it.body()
        );
        let FieldKind::Choices(options) = &form.fields[0].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[0].kind);
        };
        assert_eq!(options.len(), 2, "選択肢の数が期待と異なる: {options:?}");
        assert!(
            options.iter().all(|o| !o.checked),
            "チェックの入った選択肢を読んだ: {options:?}"
        );
    }

    #[test]
    fn 外から来た文のwikiリンクを文字列にして_選択肢を照合できる() {
        let source = "**[[作業計画書]]** と [参考](https://example.com/guide)\nで確認する";
        let mut it = item(Scene::Review(vec![Finding {
            title: "[[見出し]]".to_owned(),
            summary: "> [[要点]]\n\n- [[補足]]".to_owned(),
            options: vec![source.to_owned(), "別の案".to_owned()],
        }]));
        it.project = "[[プロジェクト]]".to_owned();
        it.plan.title = "[[計画書]]".to_owned();
        it.task = Some("[[タスク]]".to_owned());
        it.reason = "[[理由]]".to_owned();
        it.details = vec!["[[詳細]]".to_owned()];

        for url in [
            None,
            Some("https://wikino.app/s/example/pages/12".to_owned()),
        ] {
            it.plan.url = url;
            let body = it.body();
            assert!(
                !body.contains("[["),
                "本文にWikiリンクの開始括弧がある:\n{body}"
            );
            let form = parse(&body).expect("項目を読み取れなかった");
            let FieldKind::Choices(options) = &form.fields[0].kind else {
                panic!("選択の欄ではない: {:?}", form.fields[0].kind);
            };
            assert_eq!(options.len(), 2, "選択肢の数が違う");
            assert!(options[0].matches(source), "元の選択肢と照合できない");
            assert!(!options[0].matches("別の案"), "別の選択肢と一致した");
            let out = form
                .fill(&[Response::Choice(0)], "プランナー", at())
                .expect("選択肢に回答できなかった");
            assert_eq!(
                parse(&out)
                    .expect("回答を読み取れなかった")
                    .answers()
                    .expect("回答の形が違う"),
                [Response::Choice(0)],
                "選んだ回答が違う"
            );
        }
    }

    #[test]
    fn 自動リンクや行内のhtmlやurlの中のwikiリンクを含む選択肢も_元の選択肢と照合できる() {
        let sources = [
            "<https://example.com/[[x]]> を見る",
            "<!-- [[メモ]] --> を残す",
            "見る https://example.com/[[y]]",
            "(https://example.com/[[z]]) を見る",
        ];
        let it = item(Scene::Review(vec![Finding {
            title: "指摘".to_owned(),
            summary: String::new(),
            options: sources.map(str::to_owned).to_vec(),
        }]));
        let body = it.body();
        let form = parse(&body).expect("項目を読み取れなかった");
        let FieldKind::Choices(options) = &form.fields[0].kind else {
            panic!("選択の欄ではない: {:?}", form.fields[0].kind);
        };

        assert_eq!(options.len(), sources.len(), "選択肢の数が違う");
        for (option, source) in options.iter().zip(sources) {
            assert!(
                option.matches(source),
                "元の選択肢 {source:?} と照合できない:\n{body}"
            );
        }
        assert!(
            body.contains("- [ ] A: <https://example.com/[[x]]> を見る\n"),
            "自動リンクの中を変えた:\n{body}"
        );
    }
}
