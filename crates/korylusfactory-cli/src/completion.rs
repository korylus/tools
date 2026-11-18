//! 作業報告書の確認を待っている作業計画書の、完了 (マージ済み) の検知。
//!
//! 人間がロードマップで作業計画書の項目を完了とみなす節へ移し、タスクのブランチがリモートの既定のベースに
//! 取り込まれていれば、完了とみなす。
//! 完了とみなす節から作業計画書と同じタイトルのリンクを拾うのは `korylusfactory-core` が行い、
//! ここではロードマップの読み取り、リンクのページへの解決 (wikino)、gitの実行を行う。

use std::path::{Path, PathBuf};

use korylusfactory_core::queue;
use korylusfactory_core::state::PlanState;

use crate::branch;
use crate::config::Config;
use crate::process::Runner;
use crate::roadmap;
use crate::source::{self, Wikino};

/// 完了を検知できないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// ロードマップを読めない。
    #[error(transparent)]
    Source(#[from] source::Error),

    /// 完了とみなす節がロードマップに無い。
    #[error(transparent)]
    Queue(#[from] queue::Error),

    /// 完了とみなす節のWikiリンクをページに解決できない。
    #[error(transparent)]
    Roadmap(#[from] roadmap::Error),

    /// リモートから既定のベースを取得できない。
    #[error("{} で、リモート {} の {base} を取得できません", .repo.display(), branch::REMOTE)]
    Fetch {
        /// 取得しようとしたリポジトリ。
        repo: PathBuf,
        /// 取得しようとした既定のベース。
        base: String,
        /// 取得できなかった理由。
        #[source]
        source: branch::Error,
    },

    /// ブランチが取り込まれたかを確かめられない。
    #[error(transparent)]
    Branch(#[from] branch::Error),
}

/// 完了を検知した結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Completion {
    /// 作業計画書へのリンクが、ロードマップの完了とみなす節に無い。
    NotListed,
    /// リンクは完了とみなす節にあるが、リモートの既定のベースに取り込まれていないブランチがある。
    Unmerged(Vec<Unmerged>),
    /// 完了した。
    Done,
}

/// リモートの既定のベースに取り込まれていない、タスクのブランチ。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Unmerged {
    /// ブランチのリポジトリ。
    pub repo: PathBuf,
    /// タスクのブランチ。
    pub branch: String,
    /// 取り込み先のリモートのブランチ (`origin/develop` など)。
    pub into: String,
    /// ブランチがローカルに無く、取り込まれたかを確かめられないかどうか。
    pub missing: bool,
}

/// 設定configのロードマップと、タスクのブランチのリポジトリから、作業計画書stateの完了を検知する。
///
/// 確かめるのは、リポジトリごとの最後のタスクのブランチである (計画書のPRは、このブランチから作る)。
/// ブランチを作るタスクが無い作業計画書は、ロードマップの節だけで完了とみなす。
pub fn check(runner: &dyn Runner, config: &Config, state: &PlanState) -> Result<Completion, Error> {
    let roadmap = config.roadmap.fetch(runner)?;
    let links = queue::completion_links(&roadmap.body, &config.done_sections, &state.title)?;
    if links.is_empty() {
        return Ok(Completion::NotListed);
    }
    let plan = Wikino::parse(&state.url)?;
    if plan.space != config.roadmap.space {
        return Ok(Completion::NotListed);
    }
    let topic_number = roadmap
        .topic_number
        .ok_or_else(|| roadmap::Error::NoTopic(config.roadmap.url.clone()))?;
    if !roadmap::links_to(runner, &plan.space, topic_number, &links, plan.number)? {
        return Ok(Completion::NotListed);
    }

    let mut fetched: Vec<(&Path, &str)> = Vec::new();
    let mut unmerged = Vec::new();
    for record in state.last_branches() {
        let repo = record.repo.as_path();
        let base = if let Some((_, base)) = fetched.iter().find(|(saved, _)| *saved == repo) {
            *base
        } else {
            let base = branch::remote_base(runner, repo)?;
            branch::fetch(runner, repo, base).map_err(|source| Error::Fetch {
                repo: repo.to_path_buf(),
                base: base.to_owned(),
                source,
            })?;
            fetched.push((repo, base));
            base
        };
        let missing = !branch::exists(runner, repo, &record.branch)?;
        if missing || !branch::merged_into_remote(runner, repo, &record.branch, base)? {
            unmerged.push(Unmerged {
                repo: repo.to_path_buf(),
                branch: record.branch.clone(),
                into: format!("{}/{base}", branch::REMOTE),
                missing,
            });
        }
    }
    Ok(if unmerged.is_empty() {
        Completion::Done
    } else {
        Completion::Unmerged(unmerged)
    })
}

#[cfg(test)]
mod tests {
    use std::ffi::OsString;

    use korylusfactory_core::state::{ReviewTrail, TaskRecord};

    use super::*;
    use crate::branch::testutil::{Repo, branch_over_tag, git_run, repo_on, temp_dir};
    use crate::process::{self, Output, System};
    use crate::source::{Kind, Wikino};

    /// wikinoでは決めたロードマップを返し、gitは本物を実行するRunner。
    struct Roadmap<'a>(&'a str);

    impl Runner for Roadmap<'_> {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            if program == "git" {
                return System.run(program, args);
            }
            assert_eq!(program, "wikino", "想定しないコマンドを実行した");
            if args[1] == "list" {
                return Ok(Output {
                    stdout: concat!(
                        "{\"page_number\":1,\"title\":\"デモ 作業計画書\",\"topic_number\":4,\"topic_name\":\"Mewst\"}\n",
                        "{\"page_number\":2,\"title\":\"デモ 作業計画書\",\"topic_number\":3,\"topic_name\":\"Annict\"}\n",
                    ).as_bytes().to_vec(),
                    ..Output::default()
                });
            }
            let code = if args[0] == "draft" { 5 } else { 0 };
            Ok(Output {
                stdout: if code == 0 {
                    self.0.as_bytes().to_vec()
                } else {
                    Vec::new()
                },
                code,
                ..Output::default()
            })
        }
    }

    const LISTED: &str = "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n## リリース待ち\n\n- [[デモ 作業計画書]]\n\n## やった\n";

    const NOT_LISTED: &str = "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[デモ 作業計画書]]\n\n## リリース待ち\n\n## やった\n";

    fn config() -> Config {
        Config {
            roadmap: Wikino::page(Kind::Roadmap, "shimbaco", 1214),
            sections: vec!["やってる".to_owned()],
            done_sections: vec!["リリース待ち".to_owned(), "やった".to_owned()],
            project: None,
        }
    }

    /// リポジトリrepoでブランチbranchを準備したタスク1-1を持つ、作業計画書の状態。
    fn state(repo: Option<&Path>) -> PlanState {
        let mut state = PlanState::new("https://wikino.app/s/shimbaco/pages/1", "デモ 作業計画書");
        if let Some(repo) = repo {
            state.tasks.push(TaskRecord {
                id: "1-1".to_owned(),
                repo: repo.to_path_buf(),
                dir: repo.to_path_buf(),
                branch: "demo-1-1".to_owned(),
                base: "main".to_owned(),
                prepared: true,
                review: ReviewTrail::default(),
                verification: Vec::new(),
            });
        }
        state
    }

    /// mainを持つリモートと、それを複製してブランチ demo-1-1 にコミットを足し、リモートへ送ったリポジトリ。
    fn cloned() -> (Repo, Repo) {
        let remote = repo_on("main");
        let (dir, root) = temp_dir();
        git_run(
            &root,
            &["clone", "--quiet", &remote.root.display().to_string(), "."],
        );
        branch_over_tag(&root, "demo-1-1");
        git_run(&root, &["push", "--quiet", "origin", "refs/heads/demo-1-1"]);
        (remote, Repo { _dir: dir, root })
    }

    #[test]
    fn 完了とみなす節に無ければブランチを確かめない() {
        // リポジトリが無いパスを渡し、gitを実行すれば失敗するようにする。
        let state = state(Some(Path::new("/nonexistent")));
        assert_eq!(
            check(&Roadmap(NOT_LISTED), &config(), &state).expect("完了を検知できなかった"),
            Completion::NotListed
        );
    }

    #[test]
    fn ブランチを作るタスクが無ければ完了とみなす節にあるだけで完了する() {
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &state(None)).expect("完了を検知できなかった"),
            Completion::Done
        );
    }

    #[test]
    fn 別トピックの同名計画書だけが完了節にあっても完了しない() {
        let roadmap = "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[デモ 作業計画書]]\n\n## リリース待ち\n\n## やった\n\n- [[Annict/デモ 作業計画書]]\n";
        assert_eq!(
            check(&Roadmap(roadmap), &config(), &state(None)).expect("完了を検知できなかった"),
            Completion::NotListed,
            "ロードマップと別トピックの計画書を同じものとした"
        );
        // 進めているのはAnnictのトピックのページ2で、完了とみなす節のトピック名を省いたリンクはMewstのページ1を指す。
        let mut other = state(None);
        other.url = "https://wikino.app/s/shimbaco/pages/2".to_owned();
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &other).expect("完了を検知できなかった"),
            Completion::NotListed,
            "ロードマップのトピックを指すリンクを、別トピックの計画書に一致させた"
        );
    }

    #[test]
    fn 完了とみなす節のリンクがトピック名を書いていても同じ計画書なら完了を検知する() {
        let explicit = LISTED.replace("[[デモ 作業計画書]]", "[[Mewst/デモ 作業計画書]]");
        assert_eq!(
            check(&Roadmap(&explicit), &config(), &state(None)).expect("完了を検知できなかった"),
            Completion::Done,
            "ロードマップのトピック名を書いたリンクで完了を検知しなかった"
        );

        // 進めているのはAnnictのトピックのページ2で、完了とみなす節のリンクがそのトピック名を書いている。
        let other_topic = LISTED.replace("[[デモ 作業計画書]]", "[[Annict/デモ 作業計画書]]");
        let mut other = state(None);
        other.url = "https://wikino.app/s/shimbaco/pages/2".to_owned();
        assert_eq!(
            check(&Roadmap(&other_topic), &config(), &other).expect("完了を検知できなかった"),
            Completion::Done,
            "別トピックの計画書を、そのトピック名を書いたリンクで完了を検知しなかった"
        );
    }

    #[test]
    fn 最後のタスクのブランチがリモートの既定のベースに取り込まれるまで完了しない() {
        let (remote, local) = cloned();
        let state = state(Some(&local.root));
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &state).expect("完了を検知できなかった"),
            Completion::Unmerged(vec![Unmerged {
                repo: local.root.clone(),
                branch: "demo-1-1".to_owned(),
                into: "origin/main".to_owned(),
                missing: false,
            }])
        );

        git_run(
            &remote.root,
            &[
                "merge",
                "--quiet",
                "--no-ff",
                "-m",
                "merge demo-1-1",
                "refs/heads/demo-1-1",
            ],
        );
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &state).expect("完了を検知できなかった"),
            Completion::Done,
            "リモートで取り込んだのに完了しなかった"
        );
    }

    #[test]
    fn ローカルに無いブランチは取り込まれたかを確かめられない() {
        let (_remote, local) = cloned();
        git_run(&local.root, &["checkout", "--quiet", "main"]);
        git_run(&local.root, &["branch", "--quiet", "-D", "demo-1-1"]);
        let completion = check(&Roadmap(LISTED), &config(), &state(Some(&local.root)))
            .expect("完了を検知できなかった");
        assert!(
            matches!(&completion, Completion::Unmerged(branches) if branches[0].missing),
            "ローカルに無いブランチを取り込み済みにした: {completion:?}"
        );
    }

    #[test]
    fn ローカルにdevelopが無くてもリモートのdevelopへ取り込まれるまで完了しない() {
        let (remote, local) = cloned();
        git_run(&remote.root, &["branch", "develop"]);
        git_run(
            &remote.root,
            &[
                "merge",
                "--quiet",
                "--no-ff",
                "-m",
                "mainへの取り込み",
                "demo-1-1",
            ],
        );
        assert!(
            !branch::exists(&System, &local.root, "develop").expect("ブランチを確かめられなかった"),
            "検証用リポジトリにローカルdevelopがある"
        );
        let state = state(Some(&local.root));
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &state).expect("完了を検知できなかった"),
            Completion::Unmerged(vec![Unmerged {
                repo: local.root.clone(),
                branch: "demo-1-1".to_owned(),
                into: "origin/develop".to_owned(),
                missing: false,
            }]),
            "mainへの取り込みだけで完了した"
        );
        git_run(&remote.root, &["checkout", "--quiet", "develop"]);
        git_run(
            &remote.root,
            &[
                "merge",
                "--quiet",
                "--no-ff",
                "-m",
                "developへの取り込み",
                "demo-1-1",
            ],
        );
        assert_eq!(
            check(&Roadmap(LISTED), &config(), &state).expect("完了を検知できなかった"),
            Completion::Done,
            "リモートdevelopへ取り込んでも完了しない"
        );
    }

    #[test]
    fn 完了とみなす節がロードマップに無ければエラー() {
        let mut config = config();
        config.done_sections = vec!["完了".to_owned()];
        let err = check(&Roadmap(LISTED), &config, &state(None)).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Queue(_)), "エラー = {err:?}");
    }
}
