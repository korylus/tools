//! インストールされているkorylusfactoryのビルド元の情報と、ビルド元のリポジトリの最新より古いかの確認。
//!
//! ビルド元の情報はビルドスクリプトが埋め込む。
//! 古いかどうかは、ビルド元のリポジトリの `main` で、Rustのコード ([`RUST_PATHS`]) を最後に変えたコミットが、
//! 埋め込んだコミットに含まれているかで決める。

use std::ffi::OsString;
use std::path::{Path, PathBuf};

use crate::branch;
use crate::process::Runner;

/// 古いかどうかを確かめるときに見る、Rustのコードと依存のパス。
pub const RUST_PATHS: [&str; 3] = ["crates", "Cargo.toml", "Cargo.lock"];

/// 比べる相手のブランチ。
const BRANCH: &str = "main";

/// 比べる前に取得するリモート。
const REMOTE: &str = "origin";

/// ビルドスクリプトが、gitでコミットを得られないときに埋め込む値。
const UNKNOWN_COMMIT: &str = "unknown";

/// korylusfactoryのビルド元の情報。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Build {
    /// バージョン番号。
    pub version: String,
    /// ビルド元のコミット。gitで得られなければ `unknown`。
    pub commit: String,
    /// ビルド元のコミットの短い形。
    pub commit_short: String,
    /// ビルドした日。
    pub date: String,
    /// ビルド元のリポジトリの場所。
    pub source_dir: PathBuf,
}

impl Build {
    /// 実行しているkorylusfactoryのビルド元の情報を返す。
    pub fn current() -> Self {
        Self {
            version: env!("CARGO_PKG_VERSION").to_owned(),
            commit: env!("KORYLUSFACTORY_COMMIT").to_owned(),
            commit_short: env!("KORYLUSFACTORY_COMMIT_SHORT").to_owned(),
            date: env!("KORYLUSFACTORY_BUILD_DATE").to_owned(),
            source_dir: PathBuf::from(env!("KORYLUSFACTORY_SOURCE_DIR")),
        }
    }

    /// `korylusfactory --version` の1行目と同じ形の文字列を返す。
    pub fn summary(&self) -> String {
        format!(
            "korylusfactory {} (コミット {}、{}にビルド)",
            self.version, self.commit_short, self.date
        )
    }
}

/// 比べた `main` の種類。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Compared {
    /// リモートから取得した `main`。
    Remote,
    /// リモートから取得できず、手元の `main` と比べた。
    Local,
}

impl Compared {
    /// 比べた `main` の参照の名前。
    pub fn reference(self) -> String {
        match self {
            Self::Remote => format!("{REMOTE}/{BRANCH}"),
            Self::Local => BRANCH.to_owned(),
        }
    }
}

/// ビルド元のリポジトリの最新と比べた結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Freshness {
    /// 最新のRustのコードを含んでいる。
    Current {
        /// 比べた `main`。
        compared: Compared,
    },
    /// 最新のRustのコードを含んでいない。入れ直しが要る。
    Stale {
        /// 比べた `main`。
        compared: Compared,
        /// `main` でRustのコードを最後に変えたコミット。
        latest: String,
    },
    /// 古いかどうかを確かめられない。
    Unknown {
        /// 確かめられなかった理由。
        reason: String,
    },
}

/// ビルド元の情報buildを、ビルド元のリポジトリの `main` と比べる。
///
/// 比べる前にリモートの `main` を取得し、取得できないときは手元の `main` と比べる。
/// ビルド元のリポジトリが無いなど確かめられないときは、エラーにせず [`Freshness::Unknown`] を返す。
pub fn check(runner: &dyn Runner, build: &Build) -> Freshness {
    if build.commit == UNKNOWN_COMMIT {
        return unknown("ビルド元のコミットが埋め込まれていません");
    }
    let dir = build.source_dir.as_path();
    if !dir.is_dir() {
        return unknown(format!(
            "ビルド元のリポジトリ {} がありません",
            dir.display()
        ));
    }
    let compared = if fetch(runner, dir) {
        Compared::Remote
    } else {
        Compared::Local
    };
    match compare(runner, dir, &build.commit, compared) {
        Ok(freshness) => freshness,
        Err(err) => unknown(err.to_string()),
    }
}

fn unknown(reason: impl Into<String>) -> Freshness {
    Freshness::Unknown {
        reason: reason.into(),
    }
}

/// ビルド元のリポジトリdirで、リモートの `main` を取得する。取得できたかを返す。
fn fetch(runner: &dyn Runner, dir: &Path) -> bool {
    let args: Vec<OsString> = [
        OsString::from("-C"),
        dir.as_os_str().to_owned(),
        "fetch".into(),
        "--quiet".into(),
        REMOTE.into(),
        // 保存先を明示し、remote.origin.fetchの設定にかかわらず今回取得したmainを比較する。
        format!("+refs/heads/{BRANCH}:refs/remotes/{REMOTE}/{BRANCH}").into(),
    ]
    .into();
    matches!(runner.run("git", &args), Ok(output) if output.code == 0)
}

/// 埋め込んだコミットcommitが、comparedの `main` でRustのコードを最後に変えたコミットを含むかを確かめる。
fn compare(
    runner: &dyn Runner,
    dir: &Path,
    commit: &str,
    compared: Compared,
) -> Result<Freshness, branch::Error> {
    let reference = match compared {
        Compared::Remote => format!("refs/remotes/{REMOTE}/{BRANCH}"),
        Compared::Local => format!("refs/heads/{BRANCH}"),
    };
    let mut args = vec![
        "log".to_owned(),
        "-1".to_owned(),
        "--format=%H".to_owned(),
        reference,
        "--".to_owned(),
    ];
    args.extend(RUST_PATHS.map(str::to_owned));
    let latest = branch::git(runner, dir, &args)?.trim().to_owned();
    // mainでRustのコードを変えたコミットが無ければ、埋め込んだコミットより新しい変更も無い。
    if latest.is_empty() {
        return Ok(Freshness::Current { compared });
    }
    let contains = branch::git_test(
        runner,
        dir,
        ["merge-base", "--is-ancestor", latest.as_str(), commit],
    )?;
    Ok(if contains {
        Freshness::Current { compared }
    } else {
        Freshness::Stale { compared, latest }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::branch::testutil::{Repo, git_run, repo_on, temp_dir, write_file};
    use crate::process::System;
    use std::process::Command;

    fn head(dir: &Path) -> String {
        let output = Command::new("git")
            .arg("-C")
            .arg(dir)
            .args(["rev-parse", "HEAD"])
            .output()
            .expect("gitを実行できなかった");
        String::from_utf8(output.stdout)
            .expect("gitの出力がUTF-8ではない")
            .trim()
            .to_owned()
    }

    /// dirのHEADからビルドしたときの情報。
    fn built_at(dir: &Path) -> Build {
        let commit = head(dir);
        Build {
            version: "0.1.0".to_owned(),
            commit_short: commit[..7].to_owned(),
            commit,
            date: "2026-11-15".to_owned(),
            source_dir: dir.to_owned(),
        }
    }

    /// dirのnameを書き換えてコミットする。
    fn commit_file(dir: &Path, name: &str) {
        write_file(dir, name, format!("{name} {}\n", head(dir)).as_bytes());
        git_run(dir, &["add", "."]);
        git_run(dir, &["commit", "--quiet", "-m", name]);
    }

    /// Rustのコードを持つmainのリポジトリ。
    fn rust_repo() -> Repo {
        let repo = repo_on("main");
        commit_file(&repo.root, "crates/korylusfactory-cli/src/main.rs");
        repo
    }

    #[test]
    fn mainの最新からビルドしていれば古くない() {
        let repo = rust_repo();
        let build = built_at(&repo.root);
        assert_eq!(
            check(&System, &build),
            Freshness::Current {
                compared: Compared::Local
            }
        );
    }

    #[test]
    fn ビルドのあとでmainのrustのコードが変わったら古い() {
        for path in [
            "crates/korylus-workdoc/src/lib.rs",
            "Cargo.toml",
            "Cargo.lock",
        ] {
            let repo = rust_repo();
            let build = built_at(&repo.root);
            commit_file(&repo.root, path);
            assert_eq!(
                check(&System, &build),
                Freshness::Stale {
                    compared: Compared::Local,
                    latest: head(&repo.root),
                },
                "{path} を変えたのに古いとみなさなかった"
            );
        }
    }

    #[test]
    fn rust以外の変更では古くない() {
        let repo = rust_repo();
        let build = built_at(&repo.root);
        commit_file(&repo.root, "internal/flow/flow.go");
        commit_file(&repo.root, "README.md");
        assert_eq!(
            check(&System, &build),
            Freshness::Current {
                compared: Compared::Local
            }
        );
    }

    #[test]
    fn mainより先のブランチからビルドしていれば古くない() {
        let repo = rust_repo();
        git_run(&repo.root, &["checkout", "--quiet", "-b", "feature"]);
        commit_file(&repo.root, "crates/korylus-workdoc/src/lib.rs");
        let build = built_at(&repo.root);
        assert_eq!(
            check(&System, &build),
            Freshness::Current {
                compared: Compared::Local
            }
        );
    }

    #[test]
    fn リモートのmainを取得して比べる() {
        let remote = rust_repo();
        let (_dir, root) = temp_dir();
        let local = root.join("local");
        let output = Command::new("git")
            .arg("clone")
            .arg("--quiet")
            .arg(&remote.root)
            .arg(&local)
            .output()
            .expect("gitを実行できなかった");
        assert!(output.status.success(), "cloneできなかった");
        let build = built_at(&local);
        assert_eq!(
            check(&System, &build),
            Freshness::Current {
                compared: Compared::Remote
            }
        );

        // 手元のmainは古いままでも、取得したリモートのmainと比べる。
        commit_file(&remote.root, "crates/korylusfactory-cli/src/lib.rs");
        assert_eq!(
            check(&System, &build),
            Freshness::Stale {
                compared: Compared::Remote,
                latest: head(&remote.root),
            }
        );
    }

    #[test]
    fn mainを取得対象に含めない設定でも今回取得したmainと比べる() {
        for remove_tracking in [false, true] {
            let remote = rust_repo();
            let (_dir, root) = temp_dir();
            let local = root.join("local");
            let output = Command::new("git")
                .arg("clone")
                .arg("--quiet")
                .arg(&remote.root)
                .arg(&local)
                .output()
                .expect("gitを実行できなかった");
            assert!(output.status.success(), "cloneできなかった");
            let build = built_at(&local);
            git_run(
                &local,
                &[
                    "config",
                    "remote.origin.fetch",
                    "+refs/heads/release:refs/remotes/origin/release",
                ],
            );
            if remove_tracking {
                git_run(&local, &["update-ref", "-d", "refs/remotes/origin/main"]);
            }
            commit_file(&remote.root, "crates/korylusfactory-cli/src/lib.rs");
            assert_eq!(
                check(&System, &build),
                Freshness::Stale {
                    compared: Compared::Remote,
                    latest: head(&remote.root),
                },
                "今回取得したmainと比べなかった: 追跡参照が無い = {remove_tracking}"
            );
            assert_eq!(head(&local), build.commit, "作業中のHEADを変えた");
        }
    }

    #[test]
    fn mainにrustのコードの変更が無ければ古くない() {
        let repo = repo_on("main");
        let build = built_at(&repo.root);
        assert_eq!(
            check(&System, &build),
            Freshness::Current {
                compared: Compared::Local
            }
        );
    }

    #[test]
    fn 確かめられなければ理由を返す() {
        let repo = rust_repo();
        let mut no_commit = built_at(&repo.root);
        no_commit.commit = UNKNOWN_COMMIT.to_owned();
        let mut no_dir = built_at(&repo.root);
        no_dir.source_dir = repo.root.join("無いディレクトリ");
        let mut other_commit = built_at(&repo.root);
        other_commit.commit = "0123456789abcdef0123456789abcdef01234567".to_owned();
        let no_main = repo_on("develop");
        let cases = [
            (no_commit, "ビルド元のコミットが埋め込まれていません"),
            (no_dir, "ビルド元のリポジトリ "),
            (other_commit, "git -C "),
            (built_at(&no_main.root), "git -C "),
        ];
        for (build, want) in cases {
            match check(&System, &build) {
                Freshness::Unknown { reason } => {
                    assert!(reason.starts_with(want), "理由 = {reason}");
                }
                got => panic!("確かめられたことになった: {got:?}"),
            }
        }
    }
}
