use std::fs;
use std::path::Path;
use std::process::{Child, Command};
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use korylusfactory_cli::process::{Ended, Error, Launcher, Session, Supervisor};

fn launcher() -> Supervisor {
    Supervisor::new(env!("CARGO_BIN_EXE_korylusfactory"))
}

fn session(dir: &Path, script: &str, timeout: Duration) -> Session {
    Session {
        program: "sh".to_owned(),
        args: vec!["-c".into(), script.into()],
        env: vec![("KORYLUSFACTORY_TEST".into(), "値".into())],
        dir: dir.to_path_buf(),
        input: "指示\n".as_bytes().to_vec(),
        stdout: dir.join("stdout.log"),
        stderr: dir.join("stderr.log"),
        timeout,
        exclusive: None,
    }
}

fn read(path: &Path) -> String {
    fs::read_to_string(path).expect("出力のファイルを読めなかった")
}

fn pids(dir: &Path, file: &str) -> Vec<u32> {
    read(&dir.join(file))
        .lines()
        .map(|line| line.parse().expect("PIDを読み取れなかった"))
        .collect()
}

fn alive(pid: u32) -> bool {
    fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|src| {
        src.rfind(')')
            .and_then(|end| src[end + 1..].split_whitespace().next())
            .is_some_and(|state| state != "Z" && state != "X")
    })
}

/// 子の準備完了を待つ。時間はハングしたテストを止める上限にだけ使う。
fn ready(path: &Path) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !path.exists() {
        assert!(Instant::now() < deadline, "子の準備完了が通知されなかった");
        thread::yield_now();
    }
}

struct CancelOnDrop<'a>(&'a AtomicBool);

impl Drop for CancelOnDrop<'_> {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[test]
fn 指示と環境変数を渡し_出力を残して終了コードを返す() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let session = session(
        dir.path(),
        "cat; printf '%s\\n' \"$KORYLUSFACTORY_TEST\" \"$(pwd)\"; echo err >&2; exit 3",
        Duration::from_secs(60),
    );
    let ended = launcher()
        .launch(&session, &AtomicBool::new(false))
        .expect("起動できなかった");
    assert_eq!(ended, Ended::Exited(3));
    assert_eq!(
        read(&session.stdout),
        format!("指示\n値\n{}\n", dir.path().display())
    );
    assert_eq!(read(&session.stderr), "err\n");
}

#[test]
fn 親が失敗して終了しても別セッションの孤児まで回収する() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let session = session(
        dir.path(),
        "sleep 300 & echo $! > pids; setsid sleep 300 & echo $! >> pids; exit 7",
        Duration::from_secs(60),
    );
    assert_eq!(
        launcher()
            .launch(&session, &AtomicBool::new(false))
            .expect("起動できなかった"),
        Ended::Exited(7)
    );
    for pid in pids(dir.path(), "pids") {
        assert!(!alive(pid), "孤児のプロセス {pid} が残った");
    }
}

#[test]
fn termのハンドラーが起動した別セッションの子まで回収する() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let session = session(
        dir.path(),
        "trap 'setsid sleep 300 & echo $! > escaped; exit 0' TERM; echo $PPID > ready.tmp; mv ready.tmp ready; while :; do :; done",
        Duration::from_secs(60),
    );
    let cancel = AtomicBool::new(false);
    let ended = thread::scope(|scope| {
        let running = scope.spawn(|| launcher().launch(&session, &cancel));
        let _cancel_on_drop = CancelOnDrop(&cancel);
        ready(&dir.path().join("ready"));
        let pid: u32 = read(&dir.path().join("ready"))
            .trim()
            .parse()
            .expect("監督のPIDを読めなかった");
        let stat = read(Path::new(&format!("/proc/{pid}/stat")));
        let process_group: u32 = stat[stat.rfind(')').expect("statの括弧を読めなかった") + 1..]
            .split_whitespace()
            .nth(2)
            .expect("プロセスグループを読めなかった")
            .parse()
            .expect("プロセスグループが数値ではない");
        cancel.store(true, Ordering::SeqCst);
        assert_eq!(
            process_group, pid,
            "監督プロセスのグループが呼び出し元から分かれていない"
        );
        running.join().expect("監督のスレッドが止まった")
    })
    .expect("起動できなかった");
    assert_eq!(ended, Ended::Cancelled);
    for pid in pids(dir.path(), "escaped") {
        assert!(!alive(pid), "TERM中に起動した子 {pid} が残った");
    }
}

struct OtherChild(Child);
impl Drop for OtherChild {
    fn drop(&mut self) {
        // テストが途中で失敗しても、検証用の別プロセスを残さない。
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
fn セッションと関係の無い呼び出し元の子は回収しない() {
    let other = OtherChild(
        Command::new("sleep")
            .arg("300")
            .spawn()
            .expect("別の子を起動できなかった"),
    );
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let session = session(dir.path(), "sleep 300", Duration::ZERO);
    assert_eq!(
        launcher()
            .launch(&session, &AtomicBool::new(false))
            .expect("起動できなかった"),
        Ended::TimedOut
    );
    assert!(alive(other.0.id()), "セッションと関係の無い子を終わらせた");
}

#[test]
fn コマンドが見つからなければ見つからないエラーを返す() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let mut session = session(dir.path(), "", Duration::from_secs(60));
    session.program = "korylusfactory-test-no-such-command".to_owned();
    let err = launcher()
        .launch(&session, &AtomicBool::new(false))
        .expect_err("エラーにならなかった");
    assert!(matches!(err, Error::NotFound(_)), "エラー = {err:?}");
}
