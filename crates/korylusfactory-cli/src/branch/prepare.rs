//! タスクのブランチの準備と、ブランチの状態の確認。

use std::path::Path;

use super::{Error, git, git_test, run_git};
use crate::process::Runner;

/// リポジトリの最初のタスクのブランチを作る元の候補を、優先する順に並べたもの。
const BASE_CANDIDATES: [&str; 2] = ["develop", "main"];

/// タスクのブランチが取り込まれたかを確かめるリモート。
pub const REMOTE: &str = "origin";

/// リポジトリrepoの最初のタスクのブランチを作る元のブランチを返す。developがあればdevelop、無ければmainを使う。
pub fn default_base(runner: &dyn Runner, repo: &Path) -> Result<&'static str, Error> {
    for name in BASE_CANDIDATES {
        if exists(runner, repo, name)? {
            return Ok(name);
        }
    }
    Err(Error::NoBase(repo.to_path_buf()))
}

/// リポジトリrepoのリモート [`REMOTE`] にある、取り込み先の既定のベースを返す。developがあればdevelop、無ければmainを使う。
///
/// ローカルのブランチや古い追跡ブランチの有無に影響されないよう、リモートへ問い合わせる。
pub fn remote_base(runner: &dyn Runner, repo: &Path) -> Result<&'static str, Error> {
    let output = git(
        runner,
        repo,
        [
            "ls-remote",
            "--heads",
            REMOTE,
            "refs/heads/develop",
            "refs/heads/main",
        ],
    )?;
    let mut refs = Vec::new();
    for line in output.lines() {
        let Some((_, reference)) = line.split_once('\t') else {
            return Err(Error::Output {
                command: "ls-remote --heads",
                output,
            });
        };
        refs.push(reference);
    }
    for name in BASE_CANDIDATES {
        if refs.contains(&format!("refs/heads/{name}").as_str()) {
            return Ok(name);
        }
    }
    Err(Error::NoRemoteBase(repo.to_path_buf()))
}

/// リポジトリrepoでブランチnameに切り替える。nameが無ければbaseから作る。
///
/// 作業ツリーに未コミットの変更があれば、何もせずに [`Error::Dirty`] を返す。
/// excludeに挙げたパス (リポジトリのルートからの相対パス) の変更は、未コミットの変更に数えない。
pub fn prepare(
    runner: &dyn Runner,
    repo: &Path,
    name: &str,
    base: &str,
    exclude: &[&str],
) -> Result<(), Error> {
    let files = dirty(runner, repo, exclude)?;
    if !files.is_empty() {
        return Err(Error::Dirty {
            repo: repo.to_path_buf(),
            files,
        });
    }
    if exists(runner, repo, name)? {
        git(runner, repo, ["checkout", "--quiet", name])?;
    } else {
        let start = format!("refs/heads/{base}");
        git(runner, repo, ["checkout", "--quiet", "-b", name, &start])?;
    }
    Ok(())
}

/// リポジトリrepoの作業ツリーの未コミットの変更 (未追跡のファイルを含む) を、`git status --porcelain` の形で返す。
///
/// excludeに挙げたパスの変更は除く。
pub fn dirty(runner: &dyn Runner, repo: &Path, exclude: &[&str]) -> Result<Vec<String>, Error> {
    let mut args = vec![
        "status".to_owned(),
        "--porcelain".to_owned(),
        "--".to_owned(),
        ".".to_owned(),
    ];
    args.extend(exclude.iter().map(|path| format!(":(exclude){path}")));
    let out = git(runner, repo, &args)?;
    Ok(out
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(str::to_owned)
        .collect())
}

/// リポジトリrepoで切り替えているブランチの名前を返す。
pub fn current(runner: &dyn Runner, repo: &Path) -> Result<String, Error> {
    let out = git(runner, repo, ["rev-parse", "--abbrev-ref", "HEAD"])?;
    Ok(out.trim().to_owned())
}

/// リポジトリrepoのHEADが、ローカルのブランチbaseより先に持つコミットの数を返す。
///
/// 同名のタグがあってもブランチから数えるよう、baseは完全な参照名で指す。
pub fn ahead(runner: &dyn Runner, repo: &Path, base: &str) -> Result<u64, Error> {
    let range = format!("refs/heads/{base}..HEAD");
    let out = git(runner, repo, ["rev-list", "--count", &range])?;
    out.trim().parse().map_err(|_| Error::Output {
        command: "rev-list --count",
        output: out,
    })
}

/// リポジトリrepoにローカルのブランチnameがあるかどうかを返す。
pub fn exists(runner: &dyn Runner, repo: &Path, name: &str) -> Result<bool, Error> {
    let reference = format!("refs/heads/{name}");
    git_test(
        runner,
        repo,
        ["show-ref", "--verify", "--quiet", &reference],
    )
}

/// リポジトリrepoのローカルのブランチnameが、ローカルのブランチintoに取り込み済み
/// (nameの先頭のコミットがintoの祖先) かどうかを返す。
///
/// 同名のタグがあってもブランチどうしを比べるよう、完全な参照名で指す。
pub fn merged(runner: &dyn Runner, repo: &Path, name: &str, into: &str) -> Result<bool, Error> {
    let name = format!("refs/heads/{name}");
    let into = format!("refs/heads/{into}");
    git_test(runner, repo, ["merge-base", "--is-ancestor", &name, &into])
}

/// リポジトリrepoで、リモート [`REMOTE`] のブランチbaseを取得する。
///
/// 保存先を明示し、`remote.origin.fetch` の設定にかかわらず、今回取得したブランチを [`merged_into_remote`] で比べられるようにする。
pub fn fetch(runner: &dyn Runner, repo: &Path, base: &str) -> Result<(), Error> {
    let refspec = format!("+refs/heads/{base}:refs/remotes/{REMOTE}/{base}");
    git(runner, repo, ["fetch", "--quiet", REMOTE, &refspec]).map(|_| ())
}

/// リポジトリrepoのローカルのブランチnameが、[`fetch`] で取得したリモートのブランチbaseに取り込み済みかどうかを返す。
///
/// 同名のタグがあってもブランチどうしを比べるよう、完全な参照名で指す。
pub fn merged_into_remote(
    runner: &dyn Runner,
    repo: &Path,
    name: &str,
    base: &str,
) -> Result<bool, Error> {
    let name = format!("refs/heads/{name}");
    let into = format!("refs/remotes/{REMOTE}/{base}");
    git_test(runner, repo, ["merge-base", "--is-ancestor", &name, &into])
}

/// リポジトリrepoのブランチnameの先頭のコミットのハッシュを返す。ブランチがローカルに無ければ空文字を返す。
pub fn head(runner: &dyn Runner, repo: &Path, name: &str) -> Result<String, Error> {
    let reference = format!("refs/heads/{name}");
    let result = run_git(
        runner,
        repo,
        ["rev-parse", "--verify", "--quiet", &reference],
    )?;
    match result.output.code {
        0 => Ok(result.stdout()?.trim().to_owned()),
        1 => Ok(String::new()),
        _ => Err(result.error()),
    }
}

#[cfg(test)]
mod tests {
    use super::super::testutil::{Repo, branch_over_tag, git_run, repo_on, temp_dir, write_file};
    use super::*;
    use crate::process::System;

    #[test]
    fn mainだけならmainから作る() {
        let repo = repo_on("main");
        assert_eq!(
            default_base(&System, &repo.root).expect("ベースを求められなかった"),
            "main"
        );
    }

    #[test]
    fn developがあればdevelopから作る() {
        let repo = repo_on("main");
        git_run(&repo.root, &["branch", "develop"]);
        assert_eq!(
            default_base(&System, &repo.root).expect("ベースを求められなかった"),
            "develop"
        );
    }

    #[test]
    fn developもmainも無ければエラー() {
        let repo = repo_on("trunk");
        let err = default_base(&System, &repo.root).expect_err("エラーにならなかった");
        assert!(matches!(err, Error::NoBase(_)), "エラー = {err:?}");
    }

    /// リポジトリrepoで切り替えているブランチ。
    fn branch_of(repo: &Path) -> String {
        current(&System, repo).expect("ブランチを求められなかった")
    }

    #[test]
    fn ブランチが無ければベースから作って切り替える() {
        let repo = repo_on("main");
        prepare(&System, &repo.root, "plan-1-1", "main", &[]).expect("ブランチを準備できなかった");
        assert_eq!(branch_of(&repo.root), "plan-1-1");
        assert_eq!(
            ahead(&System, &repo.root, "main").expect("数えられなかった"),
            0
        );
    }

    #[test]
    fn ブランチがあれば切り替えるだけ() {
        let repo = repo_on("main");
        let root = &repo.root;
        git_run(root, &["checkout", "--quiet", "-b", "plan-1-1"]);
        write_file(root, "a.txt", b"a\n");
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "途中の作業"]);
        git_run(root, &["checkout", "--quiet", "main"]);

        prepare(&System, root, "plan-1-1", "main", &[]).expect("ブランチを準備できなかった");
        assert_eq!(branch_of(root), "plan-1-1");
        assert_eq!(
            ahead(&System, root, "main").expect("数えられなかった"),
            1,
            "途中の作業が残っていない"
        );
    }

    #[test]
    fn 未コミットの変更があれば準備しない() {
        let repo = repo_on("main");
        write_file(&repo.root, "new.txt", b"new\n");

        let err = prepare(&System, &repo.root, "plan-1-1", "main", &["docs/private"])
            .expect_err("エラーにならなかった");
        match err {
            Error::Dirty { files, .. } => assert_eq!(files, ["?? new.txt"]),
            err => panic!("エラー = {err:?}、期待値 = Error::Dirty"),
        }
        assert_eq!(branch_of(&repo.root), "main");
    }

    #[test]
    fn 除外したパスの変更は数えない() {
        let repo = repo_on("main");
        write_file(
            &repo.root,
            "docs/private/plans/plan.md",
            "計画書\n".as_bytes(),
        );

        prepare(&System, &repo.root, "plan-1-1", "main", &["docs/private"])
            .expect("ブランチを準備できなかった");
        assert_eq!(branch_of(&repo.root), "plan-1-1");
    }

    #[test]
    fn ブランチの先頭のコミットを返し_無ければ空文字() {
        let repo = repo_on("main");
        let want = git(&System, &repo.root, ["rev-parse", "HEAD"]).expect("HEADを求められなかった");
        assert_eq!(
            head(&System, &repo.root, "main").expect("エラーになった"),
            want.trim()
        );
        assert_eq!(
            head(&System, &repo.root, "missing").expect("エラーになった"),
            ""
        );
    }

    #[test]
    fn 同名のタグがあってもブランチから数える() {
        let repo = repo_on("main");
        branch_over_tag(&repo.root, "prev");
        git_run(&repo.root, &["checkout", "--quiet", "-b", "plan-1-1"]);

        assert_eq!(
            ahead(&System, &repo.root, "prev").expect("数えられなかった"),
            0
        );
    }

    #[test]
    fn ローカルのブランチがあるかを確かめる() {
        let repo = repo_on("main");
        write_file(&repo.root, "second.txt", "次のコミット\n".as_bytes());
        git_run(&repo.root, &["add", "."]);
        git_run(&repo.root, &["commit", "--quiet", "-m", "次のコミット"]);
        git_run(&repo.root, &["branch", "prev,extra"]);

        let cases = [
            ("存在するブランチ", "main", true),
            ("カンマを含むブランチ", "prev,extra", true),
            ("存在しないブランチ", "missing", false),
            ("親コミットの式", "main~1", false),
            ("コミットを指す式", "main^{commit}", false),
            ("reflogの式", "main@{0}", false),
        ];
        for (name, branch, want) in cases {
            let got = exists(&System, &repo.root, branch)
                .unwrap_or_else(|err| panic!("{name}: エラー = {err}"));
            assert_eq!(got, want, "{name}");
        }
    }

    #[test]
    fn 取り込み済みかをブランチで確かめる() {
        let repo = repo_on("main");
        // タグprevはmainを指し、ブランチprevはmainより先のコミットを持つ。
        branch_over_tag(&repo.root, "prev");
        git_run(&repo.root, &["checkout", "--quiet", "main"]);

        assert!(
            !merged(&System, &repo.root, "prev", "main").expect("エラーになった"),
            "同名タグではなくブランチprevで確かめていない"
        );
        git_run(
            &repo.root,
            &[
                "merge",
                "--quiet",
                "--no-ff",
                "-m",
                "merge prev",
                "refs/heads/prev",
            ],
        );
        assert!(
            merged(&System, &repo.root, "prev", "main").expect("エラーになった"),
            "取り込んだ後も取り込み済みにならない"
        );
        assert!(
            merged(&System, &repo.root, "missing", "main").is_err(),
            "無いブランチでエラーにならなかった"
        );
    }

    #[test]
    fn 取得したリモートの既定のベースに取り込まれたかを確かめる() {
        let remote = repo_on("main");
        let (dir, root) = temp_dir();
        git_run(
            &root,
            &["clone", "--quiet", &remote.root.display().to_string(), "."],
        );
        let local = Repo { _dir: dir, root };
        // タグtaskはmainを指し、ブランチtaskはmainより先のコミットを持つ。
        branch_over_tag(&local.root, "task");
        git_run(
            &local.root,
            &["push", "--quiet", "origin", "refs/heads/task"],
        );
        fetch(&System, &local.root, "main").expect("リモートのmainを取得できなかった");
        assert!(
            !merged_into_remote(&System, &local.root, "task", "main").expect("エラーになった"),
            "取り込む前に取り込み済みにした"
        );

        git_run(
            &remote.root,
            &[
                "merge",
                "--quiet",
                "--no-ff",
                "-m",
                "merge task",
                "refs/heads/task",
            ],
        );
        assert!(
            !merged_into_remote(&System, &local.root, "task", "main").expect("エラーになった"),
            "取得する前のリモートのmainで取り込み済みにした"
        );
        fetch(&System, &local.root, "main").expect("リモートのmainを取得できなかった");
        assert!(
            merged_into_remote(&System, &local.root, "task", "main").expect("エラーになった"),
            "リモートで取り込んだあとも取り込み済みにならない"
        );
    }

    #[test]
    fn リモートが無ければ取得できない() {
        let repo = repo_on("main");
        assert!(
            fetch(&System, &repo.root, "main").is_err(),
            "リモートが無いのに取得できた"
        );
    }

    #[test]
    fn リモートだけにdevelopがある場合も取り込み先はdevelopになる() {
        let remote = repo_on("main");
        git_run(&remote.root, &["branch", "develop"]);
        let local = repo_on("main");
        git_run(
            &local.root,
            &[
                "remote",
                "add",
                "origin",
                &remote.root.display().to_string(),
            ],
        );
        assert_eq!(
            remote_base(&System, &local.root).expect("取り込み先を求められなかった"),
            "develop"
        );
    }

    #[test]
    fn ローカルだけにdevelopがある場合はリモートのmainを取り込み先にする() {
        let remote = repo_on("main");
        let local = repo_on("develop");
        git_run(
            &local.root,
            &[
                "remote",
                "add",
                "origin",
                &remote.root.display().to_string(),
            ],
        );
        assert_eq!(
            remote_base(&System, &local.root).expect("取り込み先を求められなかった"),
            "main"
        );
    }

    #[test]
    fn developがどちらにも無ければリモートのmainを取り込み先にする() {
        let remote = repo_on("main");
        let local = repo_on("main");
        git_run(
            &local.root,
            &[
                "remote",
                "add",
                "origin",
                &remote.root.display().to_string(),
            ],
        );
        assert_eq!(
            remote_base(&System, &local.root).expect("取り込み先を求められなかった"),
            "main"
        );
    }

    #[test]
    fn リモートを確かめられないときはローカルのmainで代用しない() {
        let local = repo_on("main");
        assert!(
            remote_base(&System, &local.root).is_err(),
            "リモートが無いのに取り込み先を決めた"
        );
    }

    #[test]
    fn リモートに候補のブランチが無ければエラー() {
        let remote = repo_on("trunk");
        let local = repo_on("main");
        git_run(
            &local.root,
            &[
                "remote",
                "add",
                "origin",
                &remote.root.display().to_string(),
            ],
        );
        let err = remote_base(&System, &local.root).expect_err("候補が無いのに取り込み先を決めた");
        assert!(matches!(err, Error::NoRemoteBase(_)), "エラー = {err:?}");
    }
}
