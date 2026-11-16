use std::process::Command;

#[test]
fn 人間を待っている作業計画書が無ければエラーで終わる() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let output = Command::new(env!("CARGO_BIN_EXE_korylusfactory"))
        .arg("resume")
        .current_dir(dir.path())
        .output()
        .expect("コマンド korylusfactory を実行できなかった");

    assert_eq!(output.status.code(), Some(1), "終了コードが1ではない");
    assert!(output.stdout.is_empty(), "標準出力に書いた");
    let stderr = String::from_utf8(output.stderr).expect("標準エラーがUTF-8ではない");
    assert_eq!(
        stderr,
        "korylusfactory: 人間を待っている作業計画書がありません\n"
    );
}
