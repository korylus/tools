//! タスクごとの記録。
//!
//! 作業報告書のスキル (`/korylus-write-report`・`/korylus-review-doc`) が材料として読むため、
//! 項目名をkorylusflowの状態ファイル (`flow.json`) の `tasks` と同じにし、korylusflowと同じく空の項目は書かない。

use std::path::PathBuf;

use jiff::Timestamp;
pub use korylus_workdoc::review::Verdict;
use serde::{Deserialize, Serialize};

/// タスク1件の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct TaskRecord {
    /// タスク番号。
    pub id: String,
    /// タスクのリポジトリのルート。
    pub repo: PathBuf,
    /// タスクの接頭辞が表す、作業するディレクトリ。
    pub dir: PathBuf,
    /// タスクのブランチ。
    pub branch: String,
    /// タスクのブランチを作った元のブランチ。
    pub base: String,
    /// ブランチを準備できたか。
    ///
    /// 準備できなかったタスクのブランチは作られていないため、同じリポジトリの次のタスクのベースにしない。
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub prepared: bool,
    /// タスクのレビューの記録。
    #[serde(flatten)]
    pub review: ReviewTrail,
    /// 実装の完了で受け取った動作確認の項目。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub verification: Vec<VerificationItem>,
}

/// タスクか作業報告書の1回について行ったレビューの記録。
///
/// 状態ファイルでは、記録を埋め込んだ側の項目として平らに書く。
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReviewTrail {
    /// レビュードキュメントの名前に使う接頭辞。最初のレビューを始めるときに決める。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub review_prefix: Option<String>,
    /// `/korylus-review` の `repo=` に渡すリポジトリ。渡さないとき (本体のリポジトリと作業報告書) は `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub review_repo: Option<PathBuf>,
    /// 行ったレビュー。行った順に並べる。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub reviews: Vec<ReviewRecord>,
    /// 人間が延長したレビューの上限。延長していなければ `None` で、既定の上限を使う。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub review_limit: Option<u32>,
    /// 人間の回答を待ち始めたときの指摘。回答を書いたあと状態を保存できなくても、同じ対象に再適用するために残す。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub human_review: Option<HumanReview>,
    /// 変更しない選択肢 (後続タスクに積む・現状維持など) で回答した指摘。
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub deferred: Vec<DeferredFinding>,
}

/// 人間キューの回答を適用する、レビュー待ちを始めたときの対象。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HumanReview {
    /// レビュードキュメントの、プロジェクトのルートからの相対パス。
    pub doc: PathBuf,
    /// 項目の回答欄に並べる順の指摘。チェックの有無は保存しない。
    pub findings: Vec<HumanFinding>,
}

/// チェックの有無に左右されず、回答対象が変わっていないことを照合する指摘の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HumanFinding {
    /// 指摘がある節の名前。
    pub section: String,
    /// 指摘の見出し。
    pub title: String,
    /// 「対応方針」の行番号。同じ見出しの指摘を取り違えないために使う。
    pub line_no: usize,
    /// 判断の根拠になる本文。
    pub summary: String,
    /// 書かれた順の選択肢の本文。
    pub options: Vec<String>,
}

impl From<&korylus_workdoc::review::Finding> for HumanFinding {
    fn from(finding: &korylus_workdoc::review::Finding) -> Self {
        Self {
            section: finding.section.clone(),
            title: finding.title.clone(),
            line_no: finding.line_no,
            summary: finding.summary.clone(),
            options: finding
                .options
                .iter()
                .map(|option| option.text.clone())
                .collect(),
        }
    }
}

impl HumanFinding {
    /// 今の指摘findingが、回答待ちを始めたときと同じ内容・場所かを返す。チェックの有無だけは比較しない。
    pub(crate) fn matches(&self, finding: &korylus_workdoc::review::Finding) -> bool {
        self.section == finding.section
            && self.title == finding.title
            && self.line_no == finding.line_no
            && self.summary == finding.summary
            && self.options.len() == finding.options.len()
            && self
                .options
                .iter()
                .zip(&finding.options)
                .all(|(text, option)| text == &option.text)
    }
}

/// レビュー1回の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReviewRecord {
    /// そのタスクか作業報告書の回で、何回目のレビューか (1始まり)。
    pub number: u32,
    /// レビューしたレビュアー。
    pub role: Reviewer,
    /// レビュードキュメントの、プロジェクトのルートからの相対パス。
    pub doc: PathBuf,
    /// レビューの総合評価。状態ファイルには、レビュードキュメントに書く表記 (`Request Changes` など) で書く。
    #[serde(with = "verdict")]
    pub verdict: Verdict,
    /// レビューを記録した時刻。
    pub at: Timestamp,
}

/// レビュアーの役割。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Reviewer {
    /// Codexのレビュアー。
    #[serde(rename = "reviewer-codex")]
    Codex,
    /// Claude Codeのレビュアー。
    #[serde(rename = "reviewer-claude")]
    Claude,
}

/// 先送りした指摘1件。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DeferredFinding {
    /// 指摘があるレビュードキュメントの、プロジェクトのルートからの相対パス。
    pub doc: PathBuf,
    /// 指摘の見出し。
    pub title: String,
    /// 選ばれた選択肢の本文。
    pub choice: String,
}

/// 動作確認の項目1件。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct VerificationItem {
    /// 確かめるものの種類。
    pub kind: VerificationKind,
    /// 確認することを1文で書いたもの。
    pub title: String,
    /// 画面ならアプリのパス、処理なら実行するコマンド。
    pub target: String,
    /// 人が行う操作の手順。
    pub steps: Vec<String>,
    /// 確認できたと判断できる、観測可能な結果。
    pub expected: String,
}

/// 動作確認で確かめるものの種類。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum VerificationKind {
    /// 画面の表示・フォームの挙動。
    Screen,
    /// バックグラウンドの処理・コマンドの出力。
    Process,
}

/// 評価を、レビュードキュメントに書く表記で読み書きする。
mod verdict {
    use serde::de::Error as _;
    use serde::{Deserialize, Deserializer, Serializer};

    use super::Verdict;

    /// 読み取れる評価。
    const ALL: [Verdict; 3] = [Verdict::Approve, Verdict::RequestChanges, Verdict::Comment];

    #[expect(
        clippy::trivially_copy_pass_by_ref,
        reason = "serdeの `with` が、書く値を参照で渡すため"
    )]
    pub(super) fn serialize<S: Serializer>(
        verdict: &Verdict,
        serializer: S,
    ) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(verdict.as_str())
    }

    pub(super) fn deserialize<'de, D: Deserializer<'de>>(
        deserializer: D,
    ) -> Result<Verdict, D::Error> {
        let value = String::deserialize(deserializer)?;
        ALL.into_iter()
            .find(|verdict| verdict.as_str() == value)
            .ok_or_else(|| {
                D::Error::custom(format!(
                    "評価 {value:?} を読み取れません。Approve・Request Changes・Commentのいずれかを書いてください"
                ))
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// korylusflowが書いた、すべての項目を持つタスクの記録。
    const FLOW_TASK: &str = r#"{
  "id": "1-2",
  "repo": "/korylus-tools",
  "dir": "/korylus-tools",
  "branch": "demo-1-2",
  "base": "main",
  "prepared": true,
  "review_prefix": "korylus-tools-demo-1-2",
  "review_repo": "/korylus-tools",
  "reviews": [
    {
      "number": 1,
      "role": "reviewer-codex",
      "doc": "docs/private/reviews/korylus-tools-demo-1-2-001.md",
      "verdict": "Request Changes",
      "at": "2026-11-15T07:49:51.756676425Z"
    },
    {
      "number": 2,
      "role": "reviewer-claude",
      "doc": "docs/private/reviews/korylus-tools-demo-1-2-002.md",
      "verdict": "Comment",
      "at": "2026-11-15T08:08:10.562431049Z"
    }
  ],
  "review_limit": 4,
  "deferred": [
    {
      "doc": "docs/private/reviews/korylus-tools-demo-1-2-002.md",
      "title": "エラーの文面を揃える",
      "choice": "後続タスクに積む"
    }
  ],
  "verification": [
    {
      "kind": "process",
      "title": "statusで作業計画書の並びを表示できる",
      "target": "korylusfactory status",
      "steps": ["プロジェクトのルートで korylusfactory status を実行する"],
      "expected": "進める作業計画書が並ぶ"
    },
    {
      "kind": "screen",
      "title": "設定画面を開ける",
      "target": "/settings",
      "steps": [],
      "expected": "設定画面が表示される"
    }
  ]
}"#;

    #[test]
    fn korylusflowが書いたタスクの記録を読み_同じ形で書き戻す() {
        let record: TaskRecord =
            serde_json::from_str(FLOW_TASK).expect("タスクの記録を読み取れなかった");
        assert_eq!(
            record.review.reviews[0].verdict,
            Verdict::RequestChanges,
            "評価を読み違えた"
        );
        assert_eq!(
            record.review.reviews[1].role,
            Reviewer::Claude,
            "レビュアーを読み違えた"
        );
        assert_eq!(
            record.verification[1].kind,
            VerificationKind::Screen,
            "動作確認の種類を読み違えた"
        );

        let written = serde_json::to_value(&record).expect("タスクの記録を書けなかった");
        let want: serde_json::Value =
            serde_json::from_str(FLOW_TASK).expect("見本のJSONを読み取れなかった");
        assert_eq!(written, want, "korylusflowと異なる形で書いた");
    }

    #[test]
    fn 空の項目は書かない() {
        let record = TaskRecord {
            id: "1-1".to_owned(),
            repo: PathBuf::from("/workspace"),
            dir: PathBuf::from("/workspace/go"),
            branch: "demo-1-1".to_owned(),
            base: "develop".to_owned(),
            prepared: false,
            review: ReviewTrail::default(),
            verification: Vec::new(),
        };
        assert_eq!(
            serde_json::to_value(&record).expect("タスクの記録を書けなかった"),
            serde_json::json!({
                "id": "1-1",
                "repo": "/workspace",
                "dir": "/workspace/go",
                "branch": "demo-1-1",
                "base": "develop",
            })
        );
    }

    #[test]
    fn 知らない評価は読み取りのエラーにする() {
        let src = FLOW_TASK.replace("\"Comment\"", "\"LGTM\"");
        let err = serde_json::from_str::<TaskRecord>(&src).expect_err("エラーにならなかった");
        assert!(
            err.to_string().contains("評価 \"LGTM\" を読み取れません"),
            "エラー = {err}"
        );
    }
}
