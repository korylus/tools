#![cfg(unix)]

use std::ffi::OsString;
use std::fs::{self, File};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use korylusfactory_core::state::{Step, Store};

/// 偽物のwikinoとClaude Codeを `PATH` の先頭に置いた、作業計画書を1つ進めるプロジェクト。
struct Project {
    _dir: tempfile::TempDir,
    /// プロジェクトのルート。タスクのリポジトリを兼ねる。
    root: PathBuf,
    /// 偽物のClaude Codeが、自分のPIDを書くファイル。
    pid: PathBuf,
    /// 偽物を先頭に置いた `PATH`。
    path: OsString,
}

fn write(path: &Path, content: &str) {
    fs::create_dir_all(path.parent().expect("親ディレクトリが無い"))
        .expect("ディレクトリを作れなかった");
    fs::write(path, content).expect("ファイルを書けなかった");
}

fn script(path: &Path, content: &str) {
    write(path, content);
    fs::set_permissions(path, fs::Permissions::from_mode(0o755))
        .expect("スクリプトを実行できるようにできなかった");
}

fn git(dir: &Path, args: &[&str]) {
    let status = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args([
            "-c",
            "user.name=korylusfactory",
            "-c",
            "user.email=korylusfactory@example.com",
        ])
        .args(args)
        .stdout(Stdio::null())
        .status()
        .expect("gitを実行できなかった");
    assert!(status.success(), "git {args:?} が失敗した");
}

fn project() -> Project {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let base = dir
        .path()
        .canonicalize()
        .expect("一時ディレクトリの実体のパスを求められなかった");
    let root = base.join("project");
    fs::create_dir(&root).expect("ディレクトリを作れなかった");
    git(&root, &["init", "--quiet", "--initial-branch=main"]);
    write(&root.join("README.md"), "readme\n");
    git(&root, &["add", "."]);
    git(&root, &["commit", "--quiet", "-m", "最初のコミット"]);
    write(
        &root.join("docs/private/factory/config.json"),
        r#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214"}"#,
    );

    let pages = base.join("pages");
    write(
        &pages.join("page-1214.md"),
        "---\nwikino_topic_number: 4\n---\n\n## やってる\n\n- [[デモ 作業計画書]]\n\n## 優先度: 高\n\n## 優先度: 中\n\n## リリース待ち\n\n## やった\n",
    );
    write(
        &pages.join("draft-20.md"),
        &format!(
            "---\nwikino_number: 20\n---\n\n**計画書名**: demo\n\n## 実装判断ログ\n\n## タスクリスト\n\n- [ ] **1-1**: [{}] タスク\n",
            root.display()
        ),
    );
    write(
        &pages.join("drafts.jsonl"),
        "{\"page_number\":20,\"title\":\"デモ 作業計画書\",\"topic_number\":4,\"topic_name\":\"Mewst\"}\n",
    );

    // 下書きかページを読む `get` は、ファイルが無ければwikinoと同じく終了コード5で終わる。
    let bin = base.join("bin");
    script(
        &bin.join("wikino"),
        &format!(
            "#!/bin/sh\n\
             pages='{}'\n\
             case \"$1 $2\" in\n\
             'draft list') cat \"$pages/drafts.jsonl\" ;;\n\
             'page list') ;;\n\
             'draft get'|'page get') [ -f \"$pages/$1-$3.md\" ] || exit 5; cat \"$pages/$1-$3.md\" ;;\n\
             *) echo \"想定しない引数: $*\" >&2; exit 1 ;;\n\
             esac\n",
            pages.display()
        ),
    );
    // 終わらせられるまで動き続けるセッション。
    let pid = base.join("claude.pid");
    script(
        &bin.join("claude"),
        &format!("#!/bin/sh\necho $$ > '{}'\nexec sleep 600\n", pid.display()),
    );

    let path = std::env::join_paths(std::iter::once(bin).chain(std::env::split_paths(
        &std::env::var_os("PATH").unwrap_or_default(),
    )))
    .expect("PATHを組み立てられなかった");
    Project {
        _dir: dir,
        root,
        pid,
        path,
    }
}

/// プロジェクトで `korylusfactory start` を起動するコマンド。
///
/// ビルド元のリポジトリは開発者の手元のものになるため、gitの通信をファイルの取得だけに限り、
/// リモートへ繋がずに手元のmainと比べさせる。
fn start(project: &Project) -> Command {
    let mut command = Command::new(env!("CARGO_BIN_EXE_korylusfactory"));
    command
        .arg("start")
        .current_dir(&project.root)
        .env("PATH", &project.path)
        .env("GIT_ALLOW_PROTOCOL", "file");
    command
}

fn alive(pid: u32) -> bool {
    fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|src| {
        src.rfind(')')
            .and_then(|end| src[end + 1..].split_whitespace().next())
            .is_some_and(|state| state != "Z" && state != "X")
    })
}

/// conditionが満たされるまで待つ。時間はハングしたテストを止める上限にだけ使う。
fn until(what: &str, condition: impl Fn() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(30);
    while !condition() {
        assert!(Instant::now() < deadline, "{what}");
        thread::yield_now();
    }
}

#[test]
fn 二重には起動できず_止めたら実行中のセッションも終わらせて同じステップを残す() {
    let project = project();
    // 出力のファイルは、未コミットの変更に数えられないよう、リポジトリの外に置く。
    let log = project.pid.with_file_name("start.log");
    let mut first = start(&project)
        .stdout(File::create(&log).expect("出力のファイルを作れなかった"))
        .stderr(Stdio::inherit())
        .spawn()
        .expect("korylusfactory start を起動できなかった");
    until("セッションが起動しなかった", || {
        fs::read_to_string(&project.pid).is_ok_and(|pid| pid.ends_with('\n'))
    });
    let session: u32 = fs::read_to_string(&project.pid)
        .expect("セッションのPIDを読めなかった")
        .trim()
        .parse()
        .expect("セッションのPIDを読み取れなかった");

    let second = start(&project)
        .output()
        .expect("2つ目の korylusfactory start を実行できなかった");
    assert_eq!(second.status.code(), Some(1), "2つ目が起動できてしまった");
    let stderr = String::from_utf8(second.stderr).expect("標準エラーがUTF-8ではない");
    assert!(
        stderr.starts_with(
            "korylusfactory: このプロジェクトでは、ほかの korylusfactory start が動いています"
        ),
        "エラーが期待と異なる: {stderr:?}"
    );

    first
        .kill()
        .expect("korylusfactory start を止められなかった");
    first
        .wait()
        .expect("korylusfactory start の終わりを待てなかった");
    until(
        "止めたあとも、セッションが動き続けている",
        || !alive(session),
    );

    let state = Store::new(&project.root)
        .plan("demo")
        .expect("状態の置き場を決められなかった")
        .load()
        .expect("状態を読めなかった")
        .expect("状態ファイルが無い");
    assert_eq!(
        (state.task.as_deref(), state.step, state.retries),
        (Some("1-1"), Some(Step::Implement), 0),
        "次に起動したときに同じステップをやり直せる状態になっていない"
    );
    let out = fs::read_to_string(&log).expect("出力を読めなかった");
    assert!(
        out.contains("作業計画書 demo を始めます: https://wikino.app/s/shimbaco/pages/20\n"),
        "作業計画書を始めたことを表示していない:\n{out}"
    );
}

/// テストが失敗しても、起動したstartを残さない。
struct Running(std::process::Child);

impl Drop for Running {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

/// 準備完了のディレクトリの名前から、偽物のセッションのPIDを読む。
fn agents(base: &Path) -> Vec<u32> {
    fs::read_dir(base)
        .expect("準備完了の置き場を読めなかった")
        .map(|entry| entry.expect("準備完了の項目を読めなかった"))
        .filter_map(|entry| {
            entry
                .file_name()
                .to_str()?
                .strip_prefix("ready-")?
                .parse()
                .ok()
        })
        .collect()
}

struct SessionCleanup(PathBuf);
impl Drop for SessionCleanup {
    fn drop(&mut self) {
        // startが先に終わった場合の子は監督プロセスが回収する。失敗時も猶予を待たせず終わらせる。
        for raw in agents(&self.0) {
            if let Some(pid) = i32::try_from(raw)
                .ok()
                .and_then(rustix::process::Pid::from_raw)
            {
                let _ = rustix::process::kill_process(pid, rustix::process::Signal::KILL);
            }
        }
    }
}

#[test]
fn 停止直後は子孫の回収を待ち_回収後の再開には中断と続行を伝える() {
    let project = project();
    let base = project.pid.parent().expect("一時ディレクトリが無い");
    pausing_session(base);
    let _cleanup = SessionCleanup(base.to_path_buf());
    let mut first = Running(
        start(&project)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("最初のstartを起動できなかった"),
    );
    until(
        "最初のセッションが準備できなかった",
        || agents(base).len() == 1,
    );
    let previous = agents(base)[0];
    let initial_prompt = fs::read_to_string(base.join(format!("prompt-{previous}.md")))
        .expect("最初の指示を読めなかった");
    assert!(
        !initial_prompt.contains("## やり直し"),
        "初回を中断扱いした"
    );
    write(
        &project.root.join("partial.txt"),
        "前のセッションの途中の作業\n",
    );
    first.0.kill().expect("最初のstartを止められなかった");
    first.0.wait().expect("最初のstartの終了を待てなかった");
    until(
        "セッションに終了処理が届かなかった",
        || base.join("terminating").exists(),
    );

    let second = start(&project)
        .output()
        .expect("終了処理中にstartを実行できなかった");
    assert_eq!(second.status.code(), Some(1), "終了処理中に再起動できた");
    let err = String::from_utf8(second.stderr).expect("診断がUTF-8ではない");
    assert!(
        err.contains("前のセッションを終了処理中です"),
        "終了処理中であることを伝えていない: {err}"
    );
    assert!(alive(previous), "終了処理の途中を確かめていない");
    assert_eq!(agents(base).len(), 1, "回収前に次のセッションを起動した");

    finish_previous(&project, base, previous);
    let mut restarted = Running(
        start(&project)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .expect("回収後にstartを再起動できなかった"),
    );
    until(
        "回収後のセッションが準備できなかった",
        || agents(base).len() == 2,
    );
    let resumed = agents(base)
        .into_iter()
        .find(|pid| *pid != previous)
        .expect("再開したPIDが無い");
    let prompt = fs::read_to_string(base.join(format!("prompt-{resumed}.md")))
        .expect("再開時の指示を読めなかった");
    assert!(
        prompt.contains("## やり直し"),
        "再開であることを伝えていない: {prompt}"
    );
    assert!(
        prompt.contains("結果の確認と記録が終わる前に中断されました"),
        "中断を伝えていない: {prompt}"
    );
    assert!(
        prompt.contains("未コミットの変更があれば読み、続きから進めてください"),
        "続行の指示が無い: {prompt}"
    );
    assert!(
        project.root.join("partial.txt").exists(),
        "途中の作業を失った"
    );
    let state = Store::new(&project.root)
        .plan("demo")
        .expect("状態の置き場を決められなかった")
        .load()
        .expect("状態を読めなかった")
        .expect("状態が無い");
    assert!(state.session_started, "開始を永続化していない");
    assert_eq!(state.retries, 0, "手動の停止をやり直しの回数に数えた");
    assert_eq!(
        state.step,
        Some(Step::Implement),
        "同じステップを再開していない"
    );
    restarted
        .0
        .kill()
        .expect("再起動したstartを止められなかった");
    restarted
        .0
        .wait()
        .expect("再起動したstartの終了を待てなかった");
    until("再開したセッションを回収できなかった", || !alive(resumed));
}

/// TERMを受けたあとも、テストが許可するまで終了処理中に留めるセッションを置く。
fn pausing_session(base: &Path) {
    script(
        &base.join("bin/claude"),
        &format!(
            "#!/bin/sh\n\
             cat > '{base}/prompt-'$$'.md'\n\
             trap ': > \"{base}/terminating\"; while [ ! -f \"{base}/release\" ]; do :; done; exit 0' TERM\n\
             mkdir '{base}/ready-'$$\n\
             while :; do :; done\n",
            base = base.display(),
        ),
    );
}

/// 通知で終了を許可し、監督プロセスが排他を外すまで確認する。
fn finish_previous(project: &Project, base: &Path, previous: u32) {
    write(&base.join("release"), "終了してよい\n");
    until("前のセッションが終了しなかった", || {
        !alive(previous)
    });
    let session_lock = File::options()
        .write(true)
        .open(
            project
                .root
                .join(korylusfactory_cli::start::SESSION_LOCK_PATH),
        )
        .expect("セッションの排他のファイルを開けなかった");
    until(
        "子孫の回収後に排他が外れなかった",
        || match session_lock.try_lock() {
            Ok(()) => {
                session_lock.unlock().expect("確認用の排他を外せなかった");
                true
            }
            Err(std::fs::TryLockError::WouldBlock) => false,
            Err(err) => panic!("排他を確認できなかった: {err}"),
        },
    );
}
