//! タスクのブランチで変更したファイルの行数。

use std::path::Path;

use super::{Error, git};
use crate::process::Runner;

/// 変更したファイル1件の行数。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct FileChange {
    /// リポジトリのルートからの相対パス。
    pub path: String,
    /// 追加した行数。
    pub added: u64,
    /// 削除した行数。
    pub removed: u64,
    /// バイナリのファイルで、行数を数えられないかどうか。
    pub binary: bool,
}

/// リポジトリrepoで、ローカルのブランチbaseとheadの分岐点からheadまでに変更したファイルを返す。
///
/// 同名のタグがあってもブランチどうしを比べるよう、baseとheadは完全な参照名で指す。
/// 名前の変更は、元のファイルの削除と新しいファイルの追加として数える。
/// 出力がUTF-8でなければ、置換した別のパスを返さず [`Error::OutputEncoding`] を返す。
pub fn changes(
    runner: &dyn Runner,
    repo: &Path,
    base: &str,
    head: &str,
) -> Result<Vec<FileChange>, Error> {
    let range = format!("refs/heads/{base}...refs/heads/{head}");
    let out = git(
        runner,
        repo,
        ["diff", "--numstat", "--no-renames", "-z", &range],
    )?;
    out.split('\0')
        .filter(|record| !record.is_empty())
        .map(parse_numstat)
        .collect()
}

/// `git diff --numstat -z` の1件の記録を読み取る。
fn parse_numstat(record: &str) -> Result<FileChange, Error> {
    let invalid = || Error::Output {
        command: "diff --numstat",
        output: record.to_owned(),
    };
    let mut fields = record.splitn(3, '\t');
    let (Some(added), Some(removed), Some(path)) = (fields.next(), fields.next(), fields.next())
    else {
        return Err(invalid());
    };
    if added == "-" && removed == "-" {
        return Ok(FileChange {
            path: path.to_owned(),
            binary: true,
            ..FileChange::default()
        });
    }
    Ok(FileChange {
        path: path.to_owned(),
        added: added.parse().map_err(|_| invalid())?,
        removed: removed.parse().map_err(|_| invalid())?,
        binary: false,
    })
}

#[cfg(test)]
mod tests {
    use super::super::testutil::{branch_over_tag, git_run, repo_on, write_file};
    use super::*;
    use crate::process::System;

    fn change(path: &str, added: u64, removed: u64) -> FileChange {
        FileChange {
            path: path.to_owned(),
            added,
            removed,
            binary: false,
        }
    }

    #[test]
    fn 分岐点からの変更したファイルの行数を返す() {
        let repo = repo_on("main");
        let root = &repo.root;
        git_run(root, &["checkout", "--quiet", "-b", "task"]);
        write_file(root, "a b.go", b"1\n2\n");
        write_file(root, "image.bin", b"\x00\x01");
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "追加する"]);
        git_run(root, &["mv", "README.md", "docs.md"]);
        write_file(root, "a b.go", b"1\n3\n");
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "名前を変える"]);
        // 分岐したあとのベースのコミットは、タスクの変更に含めない。
        git_run(root, &["checkout", "--quiet", "main"]);
        write_file(root, "main.txt", b"main\n");
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "ベースを進める"]);

        let files = changes(&System, root, "main", "task").expect("変更を求められなかった");
        let want = vec![
            change("README.md", 0, 1),
            change("a b.go", 2, 0),
            change("docs.md", 1, 0),
            FileChange {
                path: "image.bin".to_owned(),
                binary: true,
                ..FileChange::default()
            },
        ];
        assert_eq!(files, want);

        let err = changes(&System, root, "main", "missing")
            .expect_err("無いブランチでエラーにならなかった");
        assert!(matches!(err, Error::Git { .. }), "エラー = {err:?}");
    }

    #[test]
    fn 同名のタグがあってもブランチどうしを比べる() {
        let repo = repo_on("main");
        // タグprevはmainを、タグtaskはブランチprevを指し、どちらもブランチとは別のコミットになる。
        branch_over_tag(&repo.root, "prev");
        branch_over_tag(&repo.root, "task");

        let files = changes(&System, &repo.root, "prev", "task").expect("変更を求められなかった");
        assert_eq!(files, vec![change("task.txt", 1, 0)]);
    }

    #[test]
    fn 読み取れない記録はエラー() {
        for record in ["1\t2", "x\t2\ta.go", "1\t-\ta.go"] {
            let err = parse_numstat(record).expect_err("エラーにならなかった");
            assert!(
                matches!(err, Error::Output { .. }),
                "{record:?}: エラー = {err:?}"
            );
        }
    }

    #[cfg(unix)]
    #[test]
    fn utf8でないファイル名の差分はパスを置換せずエラーにする() {
        use std::ffi::OsString;
        use std::os::unix::ffi::OsStringExt;

        let repo = repo_on("main");
        let root = &repo.root;
        git_run(root, &["checkout", "--quiet", "-b", "task"]);
        let name = OsString::from_vec(b"bad-\xff.txt".to_vec());
        std::fs::write(root.join(&name), "追加\n").expect("ファイルを書けなかった");
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "追加する"]);

        let err =
            changes(&System, root, "main", "task").expect_err("置換したパスを成功として返した");
        let Error::OutputEncoding { source, .. } = err else {
            panic!("UTF-8のエラーではない: {err:?}");
        };
        assert!(
            source
                .as_bytes()
                .windows(b"bad-\xff.txt".len())
                .any(|bytes| bytes == b"bad-\xff.txt"),
            "原因に元のファイル名が無い"
        );
        assert!(root.join(name).is_file(), "元のファイルが無い");
        assert!(
            !root.join("bad-\u{fffd}.txt").exists(),
            "置換後のパスが元のファイルを指している"
        );
    }

    #[test]
    fn utf8の日本語や空白やタブや改行を含むファイル名を保つ() {
        let repo = repo_on("main");
        let root = &repo.root;
        git_run(root, &["checkout", "--quiet", "-b", "task"]);
        let mut names = ["日本語.txt", "a b.txt", "tab\tname.txt", "line\nname.txt"];
        for name in names {
            write_file(root, name, "追加\n".as_bytes());
        }
        git_run(root, &["add", "."]);
        git_run(root, &["commit", "--quiet", "-m", "追加する"]);

        let files = changes(&System, root, "main", "task").expect("UTF-8の差分を読めなかった");
        names.sort_unstable();
        let want: Vec<_> = names.iter().map(|name| change(name, 1, 0)).collect();
        assert_eq!(files, want, "ファイル名や行数が異なる");
        for file in files {
            assert_eq!(
                std::fs::read(root.join(file.path)).expect("返されたパスのファイルを読めなかった"),
                "追加\n".as_bytes(),
                "返されたパスが別のファイルを指している"
            );
        }
    }
}
