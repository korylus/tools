//! 人間キューに積んだ項目の記録。
//!
//! 作業計画書の人間待ちに積んだ項目は、その作業計画書の状態 ([`PlanState::items`](super::PlanState::items)) に持つ。
//! 状態ファイルに持たない待ち (並びの先頭の作業計画書を読み取れない・ブランチが取り込まれていない) に積んだ知らせの項目は、
//! プロジェクトの [`NOTICES_PATH`] に持つ。
//! どちらも、項目を片付けるか回答を適用するまで残し、終えたら記録から除く。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use jiff::civil::DateTime;
use serde::{Deserialize, Serialize};

use super::{Error, VERSION, Wait, write_synced};

/// プロジェクトのルートからの、知らせの項目の記録の相対パス。
pub const NOTICES_PATH: &str = "docs/private/factory/queue.json";

/// 項目を積んだWikinoのページ。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ItemPage {
    /// ページ番号。
    pub number: u64,
    /// Web画面で項目を開くURL。
    pub url: String,
}

/// 項目の作成要求を送ったWikinoの置き場。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ItemLocation {
    /// スペースの識別子。
    pub space: String,
    /// トピックの番号。
    pub topic: u64,
}

/// 人間キューに積んだ項目1件の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct QueuedItem {
    /// 項目を積んだ置き場。置き場を記録する前の状態ファイルでは `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub location: Option<ItemLocation>,
    /// 項目のタイトル。作成の結果が分からなかったときに、下書きと公開済みページからこのタイトルを探す。
    pub title: String,
    /// 項目を積み始めた日時。積み直すときも、同じタイトルと本文を作るために使う。
    pub at: DateTime,
    /// 項目のページ。作れたと分かるまでは `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub page: Option<ItemPage>,
}

/// 通知のコマンドで知らせた人間待ち。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Notified {
    /// 知らせた待ち。
    pub wait: Wait,
    /// 知らせたときに渡した、人間キューの項目のURL。項目を積めていなかったか、人間キューを使わないときは `None`。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub url: Option<String>,
}

/// 作業計画書の人間待ちに積んだ項目の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WaitItem {
    /// 項目を積んだ待ち。作業計画書の待ちがこれと同じ間は、項目は人間待ちである。
    pub wait: Wait,
    /// 項目の記録。
    #[serde(flatten)]
    pub item: QueuedItem,
}

/// 状態ファイルに持たない待ちの場面。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NoticeKind {
    /// ロードマップの並びの先頭の作業計画書を読み取れない。
    PlanUnreadable,
    /// 作業計画書がロードマップの完了とみなす節にあるが、ブランチが取り込まれていない。
    NotMerged,
}

/// 状態ファイルに持たない待ちに積んだ、知らせの項目の記録。
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Notice {
    /// 待ちが解け、片付けを待っている。計画書の進行やプロセスの再起動をまたいで再試行するために残す。
    #[serde(default)]
    pub resolved: bool,
    /// 知らせの場面。
    pub kind: NoticeKind,
    /// 知らせの対象の作業計画書の、ロードマップのWikiリンクに書かれたタイトル。
    pub plan: String,
    /// 項目の記録。
    #[serde(flatten)]
    pub item: QueuedItem,
}

/// 知らせの項目の記録のファイル。
///
/// 書くのは、プロジェクトに1つだけ動く `korylusfactory start` だけのため、ロックを取らない。
/// 書き込みは一時ファイルに書いてから置き換えるため、ほかのコマンドが読んでも書きかけの記録は見えない。
#[derive(Debug, Clone)]
pub struct Notices {
    path: PathBuf,
}

/// 知らせの項目の記録のファイルの形。
#[derive(Serialize, Deserialize)]
struct NoticesFile {
    version: u32,
    #[serde(default)]
    notices: Vec<Notice>,
}

impl Notices {
    /// プロジェクトのルートproject_rootの、知らせの項目の記録のファイルを返す。
    pub fn new(project_root: &Path) -> Self {
        Self {
            path: project_root.join(NOTICES_PATH),
        }
    }

    /// 記録を読む。ファイルが無ければ、空の記録を返す。
    pub fn load(&self) -> Result<Vec<Notice>, Error> {
        let path = self.path.clone();
        let src = match fs::read(&path) {
            Ok(src) => src,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(source) => return Err(Error::Read { path, source }),
        };
        let file: NoticesFile = match serde_json::from_slice(&src) {
            Ok(file) => file,
            Err(source) => return Err(Error::Parse { path, source }),
        };
        if file.version != VERSION {
            return Err(Error::UnsupportedVersion {
                path,
                version: file.version,
            });
        }
        Ok(file.notices)
    }

    /// 記録noticesを書く。
    pub fn save(&self, notices: &[Notice]) -> Result<(), Error> {
        let path = self.path.clone();
        let file = NoticesFile {
            version: VERSION,
            notices: notices.to_vec(),
        };
        let mut data = match serde_json::to_vec_pretty(&file) {
            Ok(data) => data,
            Err(source) => return Err(Error::Encode { path, source }),
        };
        data.push(b'\n');
        let temp = path.with_extension("json.tmp");
        let written = path
            .parent()
            .map_or(Ok(()), fs::create_dir_all)
            .and_then(|()| write_synced(&temp, &data))
            .and_then(|()| fs::rename(&temp, &path));
        if let Err(source) = written {
            // 置き換えなかった一時ファイルは次の書き込みで上書きするため、消せなくても続けてよい。
            let _ = fs::remove_file(&temp);
            return Err(Error::Write { path, source });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn notice(kind: NoticeKind, plan: &str) -> Notice {
        Notice {
            resolved: false,
            kind,
            plan: plan.to_owned(),
            item: QueuedItem {
                location: None,
                title: format!("mewst - {plan} - 知らせ - 2026-11-17 21時04分05秒"),
                at: "2026-11-17T21:04:05"
                    .parse()
                    .expect("日時を読み取れなかった"),
                page: Some(ItemPage {
                    number: 30,
                    url: "https://wikino.app/s/shimbaco/pages/30/edit".to_owned(),
                }),
            },
        }
    }

    #[test]
    fn 書いた知らせの項目の記録を読み戻せる() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let notices = Notices::new(root.path());
        assert_eq!(
            notices.load().expect("無い記録を読めなかった"),
            [],
            "ファイルが無いのに記録があった"
        );

        let written = vec![
            notice(NoticeKind::PlanUnreadable, "デモ 作業計画書"),
            notice(NoticeKind::NotMerged, "別の 作業計画書"),
        ];
        let mut written = written;
        written[0].resolved = true;
        written[0].item.location = Some(ItemLocation {
            space: "shimbaco".to_owned(),
            topic: 12,
        });
        notices.save(&written).expect("記録を書けなかった");
        assert_eq!(notices.load().expect("記録を読めなかった"), written);
        let src = fs::read_to_string(root.path().join(NOTICES_PATH)).expect("ファイルが無い");
        assert!(
            src.starts_with("{\n  \"version\": 1,"),
            "形の版を先頭に書いていない:\n{src}"
        );
        assert!(
            src.contains("\"kind\": \"plan_unreadable\""),
            "場面の書き方が期待と異なる:\n{src}"
        );
    }

    #[test]
    fn 知らない版の記録は読まない() {
        let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = root.path().join(NOTICES_PATH);
        fs::create_dir_all(path.parent().expect("親ディレクトリが無い"))
            .expect("ディレクトリを作れなかった");
        fs::write(&path, r#"{"version": 99, "notices": []}"#).expect("ファイルを書けなかった");
        let err = Notices::new(root.path())
            .load()
            .expect_err("知らない版を読んだ");
        assert!(
            matches!(err, Error::UnsupportedVersion { version: 99, .. }),
            "エラー = {err:?}"
        );
    }

    #[test]
    fn 置き場と片付け待ちの印が無い以前の記録も読める() {
        let old: NoticesFile = serde_json::from_str(r#"{
            "version": 1,
            "notices": [{"kind":"not_merged","plan":"デモ 作業計画書","title":"知らせ","at":"2026-11-17T12:00:00"}]
        }"#).expect("以前の記録を読めなかった");
        assert!(!old.notices[0].resolved, "以前の記録を解消済みとした");
        assert!(
            old.notices[0].item.location.is_none(),
            "記録の無い置き場を推測した"
        );
        assert!(
            old.notices[0].item.page.is_none(),
            "記録の無いページを推測した"
        );
    }
}
