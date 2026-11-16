//! ビルド元のコミット・ビルドした日・ビルド元のリポジトリの場所を、環境変数としてバイナリに埋め込む。
//!
//! `korylusfactory --version` で表示し、入れ直しが要るかを確かめるのに使う。

use std::path::{Path, PathBuf};
use std::process::Command;

/// gitでコミットを得られないとき (ソースの圧縮ファイルからビルドしたときなど) に埋め込む値。
const UNKNOWN_COMMIT: &str = "unknown";

fn main() {
    let manifest_dir = PathBuf::from(
        std::env::var_os("CARGO_MANIFEST_DIR")
            .expect("CARGO_MANIFEST_DIRが無い (cargoからビルドスクリプトを実行していない)"),
    );
    // このクレートは `<リポジトリ>/crates/korylusfactory-cli` に置く。
    let source_dir = manifest_dir
        .join("../..")
        .canonicalize()
        .expect("ビルド元のリポジトリの場所を求められなかった");

    let commit = git(&source_dir, &["rev-parse", "HEAD"]);
    let short_commit = git(&source_dir, &["rev-parse", "--short=7", "HEAD"]);
    let build_date = jiff::Zoned::now().date();

    println!(
        "cargo:rustc-env=KORYLUSFACTORY_COMMIT={}",
        commit.as_deref().unwrap_or(UNKNOWN_COMMIT)
    );
    println!(
        "cargo:rustc-env=KORYLUSFACTORY_COMMIT_SHORT={}",
        short_commit.as_deref().unwrap_or(UNKNOWN_COMMIT)
    );
    println!("cargo:rustc-env=KORYLUSFACTORY_BUILD_DATE={build_date}");
    println!(
        "cargo:rustc-env=KORYLUSFACTORY_SOURCE_DIR={}",
        source_dir.display()
    );

    // コミットを進めたときと、コミットせずにRustのコードを変えたときに、埋め込む値を更新する。
    for path in rerun_paths(&source_dir) {
        println!("cargo:rerun-if-changed={}", path.display());
    }
}

/// ビルド元のリポジトリでgitを実行し、標準出力の1行目を返す。失敗したら `None` を返す。
fn git(source_dir: &Path, args: &[&str]) -> Option<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(source_dir)
        .args(args)
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let stdout = String::from_utf8(output.stdout).ok()?;
    let line = stdout.lines().next()?.trim();
    (!line.is_empty()).then(|| line.to_owned())
}

/// ビルドスクリプトを実行し直すきっかけにする、既存のファイルとディレクトリを返す。
///
/// 圧縮された参照を更新すると個別の参照ファイルが作られるため、`refs` の下も監視する。
/// Rustのコードと依存を変えたときも、ビルドした日を更新する。
/// 無いパスを `rerun-if-changed` に渡すと、Cargoがビルドのたびにこのスクリプトを実行し直すため除く。
fn rerun_paths(source_dir: &Path) -> Vec<PathBuf> {
    let git_paths = ["HEAD", "packed-refs", "refs"]
        .iter()
        .filter_map(|name| git(source_dir, &["rev-parse", "--git-path", name]))
        .map(|path| source_dir.join(path));
    let source_paths = ["crates", "Cargo.toml", "Cargo.lock"]
        .iter()
        .map(|name| source_dir.join(name));
    git_paths
        .chain(source_paths)
        .filter(|path| path.exists())
        .collect()
}
