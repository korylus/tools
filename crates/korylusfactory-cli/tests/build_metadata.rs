use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::{Mutex, MutexGuard, PoisonError};

fn run(command: &mut Command) -> String {
    let output = command.output().expect("コマンドを実行できなかった");
    assert!(
        output.status.success(),
        "{command:?} が失敗した: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).expect("標準出力がUTF-8ではない")
}

fn git(source_dir: &Path, args: &[&str]) -> String {
    run(Command::new("git")
        .arg("-C")
        .arg(source_dir)
        .args([
            "-c",
            "user.name=テスト",
            "-c",
            "user.email=test@example.com",
            "-c",
            "commit.gpgsign=false",
        ])
        .args(args))
}

fn copy_directory(source: &Path, destination: &Path) {
    fs::create_dir_all(destination).expect("コピー先のディレクトリを作れなかった");
    for entry in fs::read_dir(source).expect("コピー元のディレクトリを読めなかった")
    {
        let entry = entry.expect("コピー元の項目を読めなかった");
        let target = destination.join(entry.file_name());
        if entry
            .file_type()
            .expect("項目の種類を読めなかった")
            .is_dir()
        {
            copy_directory(&entry.path(), &target);
        } else {
            fs::copy(entry.path(), target).expect("ファイルをコピーできなかった");
        }
    }
}

fn copy_workspace(destination: &Path) {
    let source_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    for file in ["Cargo.toml", "Cargo.lock"] {
        fs::copy(source_dir.join(file), destination.join(file))
            .expect("ワークスペースの設定をコピーできなかった");
    }
    copy_directory(&source_dir.join("crates"), &destination.join("crates"));
}

/// 一時ディレクトリに写したワークスペースをビルドする出力先を返す。
///
/// 親のcargo testが持つロックと分け、コンパイルした依存は次のテスト実行でも使う。
fn target_dir() -> PathBuf {
    let executable = std::env::current_exe().expect("テストの実行ファイルの場所を求められなかった");
    executable
        .ancestors()
        .nth(3)
        .expect("Cargoの出力先を求められなかった")
        .join("build-metadata-test")
}

fn binary(target_dir: &Path) -> PathBuf {
    target_dir.join(format!(
        "debug/korylusfactory{}",
        std::env::consts::EXE_SUFFIX
    ))
}

/// どのテストも同じ出力先に同じ名前の実行ファイルをビルドするため、ビルドしてから実行し終えるまでを1つずつ行う。
fn lock_build() -> MutexGuard<'static, ()> {
    static BUILD: Mutex<()> = Mutex::new(());
    // ほかのテストが失敗してロックが壊れても、このテストは続ける。
    BUILD.lock().unwrap_or_else(PoisonError::into_inner)
}

fn build(source_dir: &Path, target_dir: &Path) {
    // 一時ディレクトリが別のGitのリポジトリの中にあっても、ビルドスクリプトに親のリポジトリを見つけさせない。
    let ceiling = source_dir.parent().expect("一時ディレクトリの親が無い");
    run(Command::new(env!("CARGO"))
        .current_dir(source_dir)
        .env_remove("CARGO_BUILD_TARGET")
        .env("GIT_CEILING_DIRECTORIES", ceiling)
        .args([
            "build",
            "--locked",
            "-p",
            "korylusfactory-cli",
            "--target-dir",
        ])
        .arg(target_dir));
}

#[test]
fn gitでコミットを得られないときはコミットをunknownと表示する() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let source_dir = dir.path();
    copy_workspace(source_dir);

    let _build = lock_build();
    let target_dir = target_dir();
    build(source_dir, &target_dir);
    let version = run(Command::new(binary(&target_dir)).arg("-V"));
    assert!(
        version.contains("(コミット unknown、"),
        "コミットがunknownと表示されない: {version:?}"
    );
}

#[test]
fn 圧縮した参照の更新後に再ビルドするとコミットも更新する() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let source_dir = dir.path();
    copy_workspace(source_dir);
    git(source_dir, &["init", "--initial-branch=main"]);
    git(source_dir, &["add", "."]);
    git(source_dir, &["commit", "-m", "最初のコミット"]);
    git(source_dir, &["pack-refs", "--all"]);

    let _build = lock_build();
    let target_dir = target_dir();
    let binary = binary(&target_dir);

    build(source_dir, &target_dir);
    let first_commit = git(source_dir, &["rev-parse", "--short=7", "HEAD"]);
    let first_version = run(Command::new(&binary).arg("-V"));
    assert!(
        first_version.contains(first_commit.trim()),
        "最初にビルドしたコミットが表示されない: {first_version:?}"
    );

    git(
        source_dir,
        &["commit", "--allow-empty", "-m", "次のコミット"],
    );
    let next_commit = git(source_dir, &["rev-parse", "--short=7", "HEAD"]);
    assert_ne!(first_commit, next_commit, "コミットが更新されていない");

    build(source_dir, &target_dir);
    let next_version = run(Command::new(&binary).arg("-V"));
    assert!(
        next_version.contains(next_commit.trim()),
        "再ビルドしたコミットが表示されない: {next_version:?}"
    );
}
