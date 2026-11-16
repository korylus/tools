use std::path::Path;
use std::process::{Command, Output};

/// プロジェクトのルートdirで `korylusfactory` を引数argsで実行する。
///
/// ビルド元のリポジトリは開発者の手元のものになるため、gitの通信をファイルの取得だけに限り、
/// リモートへ繋がずに手元のmainと比べさせる。
fn korylusfactory(dir: &Path, args: &[&str], path: Option<&std::ffi::OsStr>) -> Output {
    let mut command = Command::new(env!("CARGO_BIN_EXE_korylusfactory"));
    command
        .args(args)
        .current_dir(dir)
        .env("GIT_ALLOW_PROTOCOL", "file");
    if let Some(path) = path {
        command.env("PATH", path);
    }
    command
        .output()
        .expect("コマンド korylusfactory を実行できなかった")
}

#[test]
fn 設定ファイルが無ければバージョンを表示してからエラーで終わる() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let output = korylusfactory(dir.path(), &["status"], None);

    assert_eq!(output.status.code(), Some(1), "終了コードが1ではない");
    let stdout = String::from_utf8(output.stdout).expect("標準出力がUTF-8ではない");
    assert!(
        stdout.starts_with(&format!(
            "korylusfactory {} (コミット ",
            env!("CARGO_PKG_VERSION")
        )),
        "バージョンを表示していない: {stdout:?}"
    );
    let stderr = String::from_utf8(output.stderr).expect("標準エラーがUTF-8ではない");
    assert!(
        stderr.starts_with("korylusfactory: 設定ファイル "),
        "エラーが期待と異なる: {stderr:?}"
    );
    assert!(
        stderr.contains("/docs/private/factory/config.json がありません"),
        "エラーが期待と異なる: {stderr:?}"
    );
}

#[test]
fn jsonで出すときは設定ファイルが無ければ標準出力に何も書かない() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let output = korylusfactory(dir.path(), &["status", "--json"], None);

    assert_eq!(output.status.code(), Some(1), "終了コードが1ではない");
    assert!(output.stdout.is_empty(), "標準出力に書いた");
}

/// ロードマップと作業計画書の下書きを返すwikinoのスクリプトを置いたプロジェクト。
#[cfg(unix)]
struct Project {
    root: tempfile::TempDir,
    /// スクリプトを先頭に置いた `PATH`。
    path: std::ffi::OsString,
}

#[cfg(unix)]
fn project() -> Project {
    use std::fs;
    use std::os::unix::fs::PermissionsExt;

    let root = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let config = root.path().join("docs/private/factory/config.json");
    fs::create_dir_all(config.parent().expect("親ディレクトリが無い"))
        .expect("ディレクトリを作れなかった");
    fs::write(
        &config,
        r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214"}"#,
    )
    .expect("設定ファイルを書けなかった");

    let pages = root.path().join("pages");
    fs::create_dir(&pages).expect("ディレクトリを作れなかった");
    fs::write(
        pages.join("page-1214.md"),
        "---\nwikino_number: 1214\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[デモ 作業計画書]]\n\n## 優先度: 高\n\n## 優先度: 中\n",
    )
    .expect("ロードマップを書けなかった");
    fs::write(
        pages.join("draft-20.md"),
        "---\nwikino_number: 20\n---\n\n**計画書名**: demo-plan\n\n## タスクリスト\n\n- [ ] **1-1**: タスク\n",
    )
    .expect("作業計画書を書けなかった");
    fs::write(
        pages.join("drafts.jsonl"),
        "{\"page_number\":20,\"title\":\"デモ 作業計画書\",\"topic_number\":4,\"topic_name\":\"Mewst\"}\n",
    )
    .expect("下書きの一覧を書けなかった");

    // 下書きかページを読む `get` は、ファイルが無ければwikinoと同じく終了コード5で終わる。
    let bin = root.path().join("bin");
    fs::create_dir(&bin).expect("ディレクトリを作れなかった");
    let script = bin.join("wikino");
    fs::write(
        &script,
        format!(
            "#!/bin/sh\n\
             pages='{}'\n\
             case \"$1 $2\" in\n\
             'draft list') cat \"$pages/drafts.jsonl\" ;;\n\
             'draft get'|'page get') [ -f \"$pages/$1-$3.md\" ] || exit 5; cat \"$pages/$1-$3.md\" ;;\n\
             *) echo \"想定しない引数: $*\" >&2; exit 1 ;;\n\
             esac\n",
            pages.display()
        ),
    )
    .expect("wikinoのスクリプトを書けなかった");
    fs::set_permissions(&script, fs::Permissions::from_mode(0o755))
        .expect("wikinoのスクリプトを実行できるようにできなかった");

    let path = std::env::join_paths(std::iter::once(bin).chain(std::env::split_paths(
        &std::env::var_os("PATH").unwrap_or_default(),
    )))
    .expect("PATHを組み立てられなかった");
    Project { root, path }
}

#[cfg(unix)]
#[test]
fn バージョンの後に空行を挟んで作業計画書の並びを表示する() {
    let project = project();
    let output = korylusfactory(
        project.root.path(),
        &["status"],
        Some(project.path.as_os_str()),
    );

    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(0), "失敗した: {stderr}");
    let stdout = String::from_utf8(output.stdout).expect("標準出力がUTF-8ではない");
    // バージョンの後の行は、ビルド元のリポジトリの状態によって変わるため確かめない。
    let (version, plans) = stdout
        .split_once("\n\n")
        .expect("バージョンと並びの間に空行が無い");
    assert!(
        version.starts_with(&format!(
            "korylusfactory {} (コミット ",
            env!("CARGO_PKG_VERSION")
        )),
        "先にバージョンを表示していない: {stdout:?}"
    );
    assert_eq!(
        plans,
        "進める作業計画書:\n1. demo-plan [やってる]\n   https://wikino.app/s/shimbaco/pages/20\n\n進行中の作業計画書はありません\n",
        "作業計画書の並びか進行中の作業計画書の表示が期待と異なる"
    );
}

#[cfg(unix)]
#[test]
fn jsonで出すときは標準出力の全体を1つのjsonにする() {
    let project = project();
    let output = korylusfactory(
        project.root.path(),
        &["status", "--json"],
        Some(project.path.as_os_str()),
    );

    let stderr = String::from_utf8_lossy(&output.stderr);
    assert_eq!(output.status.code(), Some(0), "失敗した: {stderr}");
    assert!(output.stdout.ends_with(b"}\n"), "JSONの後に改行が無い");
    let json: serde_json::Value =
        serde_json::from_slice(&output.stdout).expect("標準出力の全体を1つのJSONとして読めない");
    assert_eq!(
        json["korylusfactory"]["version"],
        env!("CARGO_PKG_VERSION"),
        "バージョンが期待と異なる"
    );
    assert_eq!(
        json["plans"],
        serde_json::json!([{
            "section": "やってる",
            "topic": null,
            "title": "デモ 作業計画書",
            "state": "ready",
            "url": "https://wikino.app/s/shimbaco/pages/20",
            "name": "demo-plan",
        }]),
        "作業計画書の並びが期待と異なる"
    );
    assert_eq!(
        json["progress"],
        serde_json::json!([]),
        "進行中の作業計画書が期待と異なる"
    );
}
