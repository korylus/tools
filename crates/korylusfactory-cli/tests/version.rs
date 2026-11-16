use std::path::Path;
use std::process::Command;

/// ビルド元のリポジトリ (このクレートの2つ上のディレクトリ) でgitを実行し、前後の空白を除いた標準出力を返す。
fn git(args: &[&str]) -> String {
    let source_dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../..");
    let output = Command::new("git")
        .arg("-C")
        .arg(source_dir)
        .args(args)
        .output()
        .expect("コマンド git を実行できなかった");
    assert!(
        output.status.success(),
        "git {args:?} が失敗した: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout)
        .expect("gitの標準出力がUTF-8ではない")
        .trim()
        .to_owned()
}

fn korylusfactory(arg: &str) -> String {
    let output = Command::new(env!("CARGO_BIN_EXE_korylusfactory"))
        .arg(arg)
        .output()
        .expect("コマンド korylusfactory を実行できなかった");
    assert!(
        output.status.success(),
        "終了コードが成功ではない: {:?}, 標準エラー: {}",
        output.status,
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).expect("標準出力がUTF-8ではない")
}

#[test]
fn バージョンにビルド元のコミットとビルドした日とリポジトリの場所を添えて表示する() {
    let stdout = korylusfactory("--version");

    let commit = git(&["rev-parse", "--short=7", "HEAD"]);
    let source_dir = git(&["rev-parse", "--show-toplevel"]);
    let mut lines = stdout.lines();
    let first = lines.next().expect("1行目が無い");
    let prefix = format!(
        "korylusfactory {} (コミット {commit}、",
        env!("CARGO_PKG_VERSION")
    );
    let date = first
        .strip_prefix(&prefix)
        .and_then(|rest| rest.strip_suffix("にビルド)"))
        .unwrap_or_else(|| panic!("1行目の形が期待と異なる: {first:?}"));
    assert!(
        date.parse::<jiff::civil::Date>().is_ok(),
        "ビルドした日が日付ではない: {date:?}"
    );
    assert_eq!(
        lines.next(),
        Some(format!("ビルド元のリポジトリ: {source_dir}").as_str()),
        "2行目が期待と異なる"
    );
    assert_eq!(lines.next(), None, "3行目以降がある");
}

#[test]
fn 短い形のバージョンはリポジトリの場所を添えない() {
    let stdout = korylusfactory("-V");

    assert_eq!(stdout.lines().count(), 1, "1行ではない: {stdout:?}");
    assert!(
        stdout.starts_with(&format!(
            "korylusfactory {} (コミット ",
            env!("CARGO_PKG_VERSION")
        )),
        "形が期待と異なる: {stdout:?}"
    );
}
