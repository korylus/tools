//! 作業計画書の節の本文を取り出す。

use std::sync::LazyLock;

use regex::Regex;

use super::regex;
use super::snapshot::split_sections;

/// HTMLコメント (テンプレートの書き方のガイドライン) にマッチする。
static COMMENT: LazyLock<Regex> = LazyLock::new(|| regex(r"(?s)<!--.*?-->"));

/// 見出しが `## name` の節の本文 (見出しの行を除く) を返す。
///
/// HTMLコメントは除き、前後の空行を詰める。節が無ければ `None` を返す。
pub fn section(src: &str, name: &str) -> Option<String> {
    let (mut secs, log) = split_sections(src);
    secs.extend(log);
    let title = format!("## {name}");
    let found = secs.into_iter().find(|s| s.title == title)?;
    let body = found.lines[1..].join("\n");
    Some(COMMENT.replace_all(&body, "").trim().to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 節の本文と実装判断ログの論点を読む計画書の例。
    const SECTIONS_PLAN: &str = concat!(
        "# 例の作業計画書\n",
        "\n",
        "## 合意済みの設計制約\n",
        "\n",
        "<!--\n",
        "書き方のガイドライン\n",
        "-->\n",
        "\n",
        "### DBスキーマ\n",
        "\n",
        "postsの列は追加だけにする。\n",
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
    );

    #[test]
    fn 節の本文をhtmlコメントを除いて返す() {
        assert_eq!(
            section(SECTIONS_PLAN, "合意済みの設計制約").as_deref(),
            Some("### DBスキーマ\n\npostsの列は追加だけにする。"),
            "「合意済みの設計制約」節の本文が期待と異なる"
        );
        assert!(
            section(SECTIONS_PLAN, "実装判断ログ").is_some(),
            "「実装判断ログ」節が見つからない"
        );
        assert_eq!(section(SECTIONS_PLAN, "設計"), None, "無い節が見つかった");
    }
}
