//! タスクの接頭辞と担当から作業するディレクトリとリポジトリを決め、タスクのブランチを準備する。
//!
//! 担当の行の無いタスクは、接頭辞を次のとおりに読む。
//!
//! | 接頭辞                                | 対象                                                         |
//! | ------------------------------------- | ------------------------------------------------------------ |
//! | `[/workspace/go]` など `/` で始まる   | そのパスのディレクトリで作業する                             |
//! | `[Go]`・`[Rails]`・接頭辞無し         | `/workspace/go`・`/workspace/rails`・`/workspace` として読む |
//! | `[implementer]`                       | 実装者がブランチを作らずに行うタスク                         |
//! | `[shimbaco]` など上記以外             | 人が行うタスク                                               |
//!
//! 担当の行のあるタスクは、接頭辞をブランチを作るリポジトリのパスとして読む。
//!
//! | 担当          | 接頭辞       | 対象                                   |
//! | ------------- | ------------ | -------------------------------------- |
//! | `implementer` | パス         | そのパスのディレクトリで作業する       |
//! | `implementer` | 無し         | 実装者がブランチを作らずに行うタスク   |
//! | それ以外      | パスか無し   | 人が行うタスク                         |
//! | 問わない      | パス以外     | 接頭辞と担当が食い違っているエラー     |
//!
//! 上記以外の接頭辞や担当を人が行うタスクとして扱うのは、作業する人の名前の一覧を
//! 持たずに済ませ、知らない値が来たときに自動で進めない側へ倒すためである。

mod diff;
mod prepare;

use std::ffi::{OsStr, OsString};
use std::path::{Component, Path, PathBuf};

pub use diff::{FileChange, changes};
pub use prepare::{
    REMOTE, ahead, current, default_base, dirty, exists, fetch, head, merged, merged_into_remote,
    prepare, remote_base,
};

use crate::process::{self, Runner};

/// 実装者が行うタスクを表す接頭辞と担当の値。korylusflowの役割名 (implementer) に揃える。
pub const IMPLEMENTER: &str = "implementer";

/// ブランチの準備に失敗したときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 作業するディレクトリに別のリポジトリが重なっていて、gitが求めたルートのリポジトリでは無視されている。
    ///
    /// gitは重なっている側ではなく外側のリポジトリを返すため、そのままでは別のリポジトリでブランチを切ってしまう。
    #[error(
        "作業するディレクトリに別のリポジトリが重なっています: {} は {} で無視されています。実体のリポジトリのパスを書いてください",
        .dir.display(),
        .repo.display()
    )]
    Overlaid {
        /// 作業するディレクトリ。
        dir: PathBuf,
        /// gitが求めたルートのリポジトリ。
        repo: PathBuf,
    },

    /// 担当の行のあるタスクの接頭辞がパスではない。担当の行のあるタスクでは、接頭辞はブランチを作るリポジトリだけを表す。
    #[error(
        "タスクの接頭辞と担当の行が食い違っています: 担当の行があるタスクの接頭辞 [{0}] がパスではありません。接頭辞をブランチを作るリポジトリのパスにするか、取り除いてください"
    )]
    AssigneeMismatch(String),

    /// 作業するディレクトリを含むリポジトリを求められない。
    #[error("{} のリポジトリを求められません", .dir.display())]
    Toplevel {
        /// 作業するディレクトリ。
        dir: PathBuf,
        /// 求められなかった理由。
        #[source]
        source: Box<Error>,
    },

    /// パスがリポジトリで無視されているかを確かめられない。
    #[error("{} が {} で無視されているかを確かめられません", .path.display(), .repo.display())]
    IgnoreCheck {
        /// 確かめたパス。
        path: PathBuf,
        /// 確かめたリポジトリ。
        repo: PathBuf,
        /// 確かめられなかった理由。
        #[source]
        source: Box<Error>,
    },

    /// gitが失敗の終了コードで終わった。
    #[error("git {args} が終了コード {code} で失敗しました: {stderr}")]
    Git {
        /// gitに渡した引数を空白でつないだもの。
        args: String,
        /// 終了コード。
        code: i32,
        /// 前後の空白を除いた標準エラー。
        stderr: String,
    },

    /// リポジトリに、最初のタスクのブランチを作る元の候補のブランチが無い。
    #[error("{} に develop / main のどちらのブランチもありません", .0.display())]
    NoBase(PathBuf),

    /// リモートに、取り込み先の候補になるブランチが無い。
    #[error("{} のリモート origin に develop / main のどちらのブランチもありません", .0.display())]
    NoRemoteBase(PathBuf),

    /// 作業ツリーに未コミットの変更があるため、ブランチを準備しなかった。
    #[error("{} に未コミットの変更があります: {}", .repo.display(), .files.join(", "))]
    Dirty {
        /// 作業ツリーのリポジトリ。
        repo: PathBuf,
        /// `git status --porcelain` の形の、未コミットの変更の一覧。
        files: Vec<String>,
    },

    /// gitの出力を読み取れない。
    #[error("git {command} の出力を読み取れません: {output:?}")]
    Output {
        /// 出力を読み取れなかったgitのサブコマンド。
        command: &'static str,
        /// 読み取れなかった出力。
        output: String,
    },

    /// gitの成功出力がUTF-8ではなく、パスなどのデータを文字列として保持できない。
    #[error("git {args} の出力がUTF-8ではありません")]
    OutputEncoding {
        /// gitに渡した引数を表示用に空白でつないだもの。
        args: String,
        /// UTF-8として読めなかった理由と元の出力。
        #[source]
        source: std::string::FromUtf8Error,
    },

    /// gitを実行できなかった。
    #[error(transparent)]
    Run(#[from] process::Error),
}

/// タスクの対象。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    /// 人が行うタスク。
    Human,
    /// 実装者がブランチを作らずに行うタスク。
    Implementer,
    /// リポジトリでブランチを作って行うタスク。
    Repo {
        /// 作業するディレクトリ。
        dir: PathBuf,
        /// 作業するディレクトリを含むリポジトリのルート。
        repo: PathBuf,
    },
}

/// 接頭辞prefix (角括弧を除いたもの) が表す作業するディレクトリを返す。
///
/// 人が行うタスクなら `None` を返す。`implementer` は [`resolve`] が先に読むため扱わない。
pub fn work_dir(prefix: &str) -> Option<PathBuf> {
    if prefix.starts_with('/') {
        return Some(clean(prefix));
    }
    // 接頭辞がプラットフォーム名だった計画書を、そのまま進められるようにする。
    let dir = match prefix {
        "Go" => "/workspace/go",
        "Rails" => "/workspace/rails",
        "" => "/workspace",
        _ => return None,
    };
    Some(PathBuf::from(dir))
}

/// 接頭辞prefixと担当assigneeからタスクの対象を求める。
///
/// 担当の行が無ければassigneeは空文字で、接頭辞だけから求める。
/// 担当の行があるのに接頭辞がパスでなければ [`Error::AssigneeMismatch`] を返す。
/// 作業するディレクトリがgitのリポジトリの中に無ければエラーを、
/// 別のリポジトリが重なっていれば [`Error::Overlaid`] を返す。
pub fn resolve(runner: &dyn Runner, prefix: &str, assignee: &str) -> Result<Target, Error> {
    if !assignee.is_empty() {
        return resolve_assigned(runner, prefix, assignee);
    }
    if prefix == IMPLEMENTER {
        return Ok(Target::Implementer);
    }
    match work_dir(prefix) {
        Some(dir) => resolve_repo_target(runner, dir),
        None => Ok(Target::Human),
    }
}

/// 担当の行のあるタスクの対象を、接頭辞prefixと担当assigneeから求める。
fn resolve_assigned(runner: &dyn Runner, prefix: &str, assignee: &str) -> Result<Target, Error> {
    if !prefix.is_empty() && !prefix.starts_with('/') {
        return Err(Error::AssigneeMismatch(prefix.to_owned()));
    }
    if assignee != IMPLEMENTER {
        return Ok(Target::Human);
    }
    if prefix.is_empty() {
        return Ok(Target::Implementer);
    }
    resolve_repo_target(runner, clean(prefix))
}

/// 作業するディレクトリdirで、リポジトリでブランチを作って行うタスクの対象を求める。
fn resolve_repo_target(runner: &dyn Runner, dir: PathBuf) -> Result<Target, Error> {
    let (dir, repo) = resolve_dir(runner, dir)?;
    Ok(Target::Repo { dir, repo })
}

/// 作業するディレクトリdirと、それを含むリポジトリのルートの組を返す。
fn resolve_dir(runner: &dyn Runner, dir: PathBuf) -> Result<(PathBuf, PathBuf), Error> {
    let repo = toplevel(runner, &dir)?;
    if is_ignored(runner, &repo, &dir)? {
        return Err(Error::Overlaid { dir, repo });
    }
    Ok((dir, repo))
}

/// ディレクトリdirを含むリポジトリのルートを、タスクの接頭辞と同じ規則で求める。
///
/// dirに別のリポジトリが重なっていれば、[`Error::Overlaid`] を返す。
pub fn repo(runner: &dyn Runner, dir: &Path) -> Result<PathBuf, Error> {
    resolve_dir(runner, dir.to_path_buf()).map(|(_, repo)| repo)
}

/// ディレクトリdirを含むリポジトリのルートを返す。
pub fn toplevel(runner: &dyn Runner, dir: &Path) -> Result<PathBuf, Error> {
    let out =
        git(runner, dir, ["rev-parse", "--show-toplevel"]).map_err(|source| Error::Toplevel {
            dir: dir.to_path_buf(),
            source: Box::new(source),
        })?;
    Ok(PathBuf::from(out.trim()))
}

/// リポジトリrepoがパスpathを無視しているかどうかを返す。
fn is_ignored(runner: &dyn Runner, repo: &Path, path: &Path) -> Result<bool, Error> {
    let args = [
        OsStr::new("check-ignore"),
        OsStr::new("-q"),
        OsStr::new("--"),
        path.as_os_str(),
    ];
    git_test(runner, repo, args).map_err(|source| Error::IgnoreCheck {
        path: path.to_path_buf(),
        repo: repo.to_path_buf(),
        source: Box::new(source),
    })
}

/// パスpathを、ファイルシステムを見ずに字面だけで正規化する。
///
/// 末尾のスラッシュ・`.`・`..` を除き、Goの `filepath.Clean` と同じ形にする。
fn clean(path: &str) -> PathBuf {
    let mut parts: Vec<Component<'_>> = Vec::new();
    for component in Path::new(path).components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => match parts.last() {
                Some(Component::Normal(_)) => {
                    parts.pop();
                }
                // ルートの親はルートのまま。
                Some(Component::RootDir) => {}
                _ => parts.push(component),
            },
            _ => parts.push(component),
        }
    }
    if parts.is_empty() {
        return PathBuf::from(".");
    }
    parts.iter().collect()
}

/// gitの実行の結果。
struct GitOutput {
    args: Vec<OsString>,
    output: process::Output,
}

impl GitOutput {
    /// 失敗の終了コードで終わったときのエラー。
    fn error(&self) -> Error {
        let args: Vec<_> = self.args.iter().map(|a| a.to_string_lossy()).collect();
        Error::Git {
            args: args.join(" "),
            code: self.output.code,
            stderr: String::from_utf8_lossy(&self.output.stderr)
                .trim()
                .to_owned(),
        }
    }

    /// 標準出力をUTF-8として読み、パスなどのデータのバイトを置換せずに返す。
    fn stdout(self) -> Result<String, Error> {
        String::from_utf8(self.output.stdout).map_err(|source| {
            let args: Vec<_> = self.args.iter().map(|a| a.to_string_lossy()).collect();
            Error::OutputEncoding {
                args: args.join(" "),
                source,
            }
        })
    }
}

/// ディレクトリdirでgitを実行し、終了コードにかかわらず結果を返す。
fn run_git<I, S>(runner: &dyn Runner, dir: &Path, args: I) -> Result<GitOutput, Error>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let mut all = vec![OsString::from("-C"), dir.as_os_str().to_owned()];
    all.extend(args.into_iter().map(|a| a.as_ref().to_owned()));
    let output = runner.run("git", &all)?;
    Ok(GitOutput { args: all, output })
}

/// ディレクトリdirでgitを実行し、標準出力を返す。失敗の終了コードで終わったら [`Error::Git`] を返す。
/// 成功出力がUTF-8でなければ、データを置換せず [`Error::OutputEncoding`] を返す。
pub(crate) fn git<I, S>(runner: &dyn Runner, dir: &Path, args: I) -> Result<String, Error>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let result = run_git(runner, dir, args)?;
    match result.output.code {
        0 => result.stdout(),
        _ => Err(result.error()),
    }
}

/// ディレクトリdirで、真偽を終了コードで返すgitのサブコマンドを実行する。
///
/// 終了コード0なら真、1なら偽を返し、それ以外は [`Error::Git`] を返す。
pub(crate) fn git_test<I, S>(runner: &dyn Runner, dir: &Path, args: I) -> Result<bool, Error>
where
    I: IntoIterator<Item = S>,
    S: AsRef<OsStr>,
{
    let result = run_git(runner, dir, args)?;
    match result.output.code {
        0 => Ok(true),
        1 => Ok(false),
        _ => Err(result.error()),
    }
}

#[cfg(test)]
pub(crate) mod testutil {
    use std::fs;
    use std::path::{Path, PathBuf};
    use std::process::Command;

    use tempfile::TempDir;

    /// テストのリポジトリ。一時ディレクトリを持ち、落とすと消える。
    pub(crate) struct Repo {
        pub(crate) _dir: TempDir,
        pub(crate) root: PathBuf,
    }

    /// 一時ディレクトリを作り、gitが返すパスと揃うよう実体のパスと組にして返す。
    /// macOSの一時ディレクトリはシンボリックリンクを含むためである。
    pub(crate) fn temp_dir() -> (TempDir, PathBuf) {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let root = dir
            .path()
            .canonicalize()
            .expect("一時ディレクトリの実体のパスを求められなかった");
        (dir, root)
    }

    /// dirでgitを実行し、失敗したらテストを止める。コミットの作者は固定する。
    pub(crate) fn git_run(dir: &Path, args: &[&str]) {
        let output = Command::new("git")
            .arg("-C")
            .arg(dir)
            .args([
                "-c",
                "user.name=korylusfactory",
                "-c",
                "user.email=korylusfactory@example.com",
            ])
            .args(args)
            .output()
            .expect("gitを実行できなかった");
        assert!(
            output.status.success(),
            "git {args:?} が失敗した: {}",
            String::from_utf8_lossy(&output.stderr)
        );
    }

    /// rootの下のnameにcontentを書く。
    pub(crate) fn write_file(root: &Path, name: &str, content: &[u8]) {
        let path = root.join(name);
        let parent = path.parent().expect("親ディレクトリが無い");
        fs::create_dir_all(parent).expect("ディレクトリを作れなかった");
        fs::write(&path, content).expect("ファイルを書けなかった");
    }

    /// ブランチbaseにコミットを1つ持つリポジトリを作る。
    pub(crate) fn repo_on(base: &str) -> Repo {
        let (dir, root) = temp_dir();
        git_run(
            &root,
            &["init", "--quiet", &format!("--initial-branch={base}")],
        );
        write_file(&root, "README.md", b"readme\n");
        git_run(&root, &["add", "."]);
        git_run(&root, &["commit", "--quiet", "-m", "最初のコミット"]);
        Repo { _dir: dir, root }
    }

    /// HEADに同名のタグnameを付けてから、コミットを1つ足したブランチnameを作って切り替える。
    pub(crate) fn branch_over_tag(repo: &Path, name: &str) {
        git_run(repo, &["tag", name]);
        git_run(repo, &["checkout", "--quiet", "-b", name]);
        write_file(repo, &format!("{name}.txt"), format!("{name}\n").as_bytes());
        git_run(repo, &["add", "."]);
        git_run(repo, &["commit", "--quiet", "-m", name]);
    }
}

#[cfg(test)]
mod tests {
    use super::testutil::{Repo, git_run, temp_dir};
    use super::*;
    use crate::process::System;
    use std::fs;

    /// docs/private/ を無視するリポジトリを作る。
    fn repo_ignoring_private() -> Repo {
        let (dir, root) = temp_dir();
        for sub in ["go", "docs/private/plans"] {
            fs::create_dir_all(root.join(sub)).expect("ディレクトリを作れなかった");
        }
        fs::write(root.join(".gitignore"), "docs/private/\n").expect(".gitignoreを書けなかった");
        git_run(&root, &["init", "--quiet"]);
        Repo { _dir: dir, root }
    }

    fn repo_target(dir: &Path, repo: &Path) -> Target {
        Target::Repo {
            dir: dir.to_path_buf(),
            repo: repo.to_path_buf(),
        }
    }

    #[test]
    fn 接頭辞から作業するディレクトリを求める() {
        let cases = [
            (
                "パスはそのディレクトリ",
                "/workspace/go",
                Some("/workspace/go"),
            ),
            (
                "パスの末尾のスラッシュを除く",
                "/korylus-tools/",
                Some("/korylus-tools"),
            ),
            ("パスの.と..を除く", "/a/./b/../c", Some("/a/c")),
            ("ルートの親はルート", "/../x", Some("/x")),
            ("[Go]は/workspace/go", "Go", Some("/workspace/go")),
            (
                "[Rails]は/workspace/rails",
                "Rails",
                Some("/workspace/rails"),
            ),
            ("接頭辞無しは/workspace", "", Some("/workspace")),
            ("名前は人が行うタスク", "shimbaco", None),
            ("知らない接頭辞は人が行うタスク", "Mewst", None),
        ];
        for (name, prefix, want) in cases {
            assert_eq!(work_dir(prefix), want.map(PathBuf::from), "{name}");
        }
    }

    #[test]
    fn リポジトリのルートはそのリポジトリ() {
        let repo = repo_ignoring_private();
        let root = repo.root.to_str().expect("パスがUTF-8ではない");
        let got = resolve(&System, root, "").expect("対象を求められなかった");
        assert_eq!(got, repo_target(&repo.root, &repo.root));
    }

    #[test]
    fn リポジトリの中のディレクトリはルートを求める() {
        let repo = repo_ignoring_private();
        let dir = repo.root.join("go");
        let got = resolve(&System, dir.to_str().expect("パスがUTF-8ではない"), "")
            .expect("対象を求められなかった");
        assert_eq!(got, repo_target(&dir, &repo.root));
        assert_eq!(repo_of(&dir), repo.root, "repoが求めたルートが異なる");
    }

    fn repo_of(dir: &Path) -> PathBuf {
        repo(&System, dir).expect("リポジトリを求められなかった")
    }

    /// 呼ばれたら失敗するRunner。gitを使わないことを確かめる。
    struct NoRun;

    impl Runner for NoRun {
        fn run(&self, program: &str, args: &[OsString]) -> Result<process::Output, process::Error> {
            panic!("{program} {args:?} を実行した");
        }
    }

    #[test]
    fn 人が行うタスクと実装者がブランチを作らずに行うタスクはgitを使わない() {
        assert_eq!(
            resolve(&NoRun, "shimbaco", "").expect("エラーになった"),
            Target::Human
        );
        assert_eq!(
            resolve(&NoRun, "implementer", "").expect("エラーになった"),
            Target::Implementer
        );
    }

    #[test]
    fn 無視されているディレクトリは重なっているパスとみなす() {
        let repo = repo_ignoring_private();
        for sub in ["docs/private", "docs/private/plans"] {
            for assignee in ["", "implementer"] {
                let dir = repo.root.join(sub);
                let err = resolve(
                    &System,
                    dir.to_str().expect("パスがUTF-8ではない"),
                    assignee,
                )
                .expect_err("エラーにならなかった");
                assert!(
                    matches!(err, Error::Overlaid { .. }),
                    "{sub} ({assignee:?}): エラー = {err:?}"
                );
            }
        }
    }

    #[test]
    fn リポジトリの外はリポジトリを求められないエラー() {
        let (_dir, outside) = temp_dir();
        let err = resolve(&System, outside.to_str().expect("パスがUTF-8ではない"), "")
            .expect_err("エラーにならなかった");
        assert!(matches!(err, Error::Toplevel { .. }), "エラー = {err:?}");
    }

    #[test]
    fn 担当の行のあるタスクの対象を求める() {
        let repo = repo_ignoring_private();
        let dir = repo.root.join("go");
        let dir_str = dir.to_str().expect("パスがUTF-8ではない");
        let cases = [
            (
                "implementerでパスはそのリポジトリでブランチを作る",
                format!("{dir_str}/"),
                "implementer",
                repo_target(&dir, &repo.root),
            ),
            (
                "implementerで接頭辞無しはブランチを作らずに行う",
                String::new(),
                "implementer",
                Target::Implementer,
            ),
            (
                "人の名前でパスは人が行う",
                dir_str.to_owned(),
                "shimbaco",
                Target::Human,
            ),
            (
                "人の名前で接頭辞無しは人が行う",
                String::new(),
                "shimbaco",
                Target::Human,
            ),
            (
                "知らない担当は人が行う",
                String::new(),
                "Implementer",
                Target::Human,
            ),
        ];
        for (name, prefix, assignee, want) in cases {
            let got = resolve(&System, &prefix, assignee)
                .unwrap_or_else(|err| panic!("{name}: エラー = {err}"));
            assert_eq!(got, want, "{name}");
        }
    }

    #[test]
    fn 担当の行のあるタスクの接頭辞がパスでなければ食い違いのエラー() {
        for prefix in ["implementer", "shimbaco", "Go", "Rails"] {
            for assignee in ["implementer", "shimbaco"] {
                let err = resolve(&NoRun, prefix, assignee).expect_err("エラーにならなかった");
                assert!(
                    matches!(err, Error::AssigneeMismatch(ref p) if p == prefix),
                    "({prefix:?}, {assignee:?}): エラー = {err:?}"
                );
            }
        }
    }

    /// 決めたgitの出力を返し、実行した回数を数えるRunner。
    struct GitOutputRunner {
        output: process::Output,
        calls: std::cell::Cell<usize>,
    }

    impl GitOutputRunner {
        fn new(code: i32) -> Self {
            Self {
                output: process::Output {
                    stdout: b"1\t0\tbad-\xff.txt\0".to_vec(),
                    stderr: b"error-\xff".to_vec(),
                    code,
                },
                calls: std::cell::Cell::new(0),
            }
        }
    }

    impl Runner for GitOutputRunner {
        fn run(
            &self,
            program: &str,
            _args: &[OsString],
        ) -> Result<process::Output, process::Error> {
            assert_eq!(program, "git", "git以外のコマンドを実行した");
            self.calls.set(self.calls.get() + 1);
            Ok(self.output.clone())
        }
    }

    #[test]
    fn gitの成功出力がutf8でなければ原因付きのエラーで止める() {
        let dir = Path::new("/repo");
        for op in ["current", "head", "ahead", "dirty", "changes", "prepare"] {
            let runner = GitOutputRunner::new(0);
            let result = match op {
                "current" => current(&runner, dir).map(|_| ()),
                "head" => head(&runner, dir, "main").map(|_| ()),
                "ahead" => ahead(&runner, dir, "main").map(|_| ()),
                "dirty" => dirty(&runner, dir, &[]).map(|_| ()),
                "changes" => changes(&runner, dir, "main", "task").map(|_| ()),
                "prepare" => prepare(&runner, dir, "task", "main", &[]),
                _ => unreachable!("テストに無い操作"),
            };
            let err = result.expect_err("パスを置換して成功した");
            assert!(
                matches!(err, Error::OutputEncoding { .. }),
                "{op}: エラー = {err:?}"
            );
            let source = std::error::Error::source(&err).expect("原因を失った");
            let source = source
                .downcast_ref::<std::string::FromUtf8Error>()
                .expect("UTF-8の原因ではない");
            assert_eq!(source.as_bytes(), runner.output.stdout, "元の出力を失った");
            assert!(
                err.to_string().contains("git -C /repo ")
                    && err.to_string().ends_with("の出力がUTF-8ではありません"),
                "操作が分からないエラー: {err}"
            );
            assert_eq!(runner.calls.get(), 1, "{op}: エラーの後もgitを実行した");
        }
    }

    #[test]
    fn リポジトリのルートを読めなければ原因を包んで止める() {
        let runner = GitOutputRunner::new(0);
        let err = toplevel(&runner, Path::new("/repo")).expect_err("パスを置換して返した");
        let Error::Toplevel { source, .. } = err else {
            panic!("リポジトリを求めるエラーではない: {err:?}");
        };
        assert!(
            matches!(*source, Error::OutputEncoding { .. }),
            "UTF-8の原因を失った: {source:?}"
        );
        assert_eq!(runner.calls.get(), 1, "ルートを読めないのに処理を続けた");
    }

    #[test]
    fn gitが失敗したときは終了コードのエラーを返す() {
        let runner = GitOutputRunner::new(128);
        let err = current(&runner, Path::new("/repo")).expect_err("gitが失敗したのに成功した");
        assert!(
            matches!(err, Error::Git { code: 128, .. }),
            "終了コードのエラーではない: {err:?}"
        );
        assert!(err.to_string().contains("error-"), "表示用のstderrを失った");
    }
}
