//! セッション専用のsubreaper。再親化された孤児も、最後の子がいなくなるまで回収する。

use std::collections::HashSet;
use std::error::Error as _;
use std::fs::{self, File, OpenOptions, TryLockError};
use std::io::{self, Write as _};
use std::path::Path;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use rustix::process::{self, Pid, Signal, WaitOptions};

use super::{CANCEL, Ended, Error, POLL, REPLY, REQUEST, Reply, Request, Session, control};

/// SIGTERMのあとに後始末を待つ時間。
const GRACE: Duration = Duration::from_secs(10);

/// korylusfactoryの内部起動として、制御ディレクトリdirのセッションを監督する。
///
/// 呼び出し元から分離した専用プロセスで呼ぶ。孤児の引き取りはこのプロセスにだけ設定する。
pub fn supervise(dir: &Path) -> Result<(), Error> {
    let path = dir.join(REQUEST);
    let data = fs::read(&path).map_err(|source| control(&path, source))?;
    let request: Request =
        serde_json::from_slice(&data).map_err(|source| control(&path, io::Error::other(source)))?;
    let reply = match run(&request, dir) {
        Ok(ended) => Reply::Ended(ended),
        Err(Error::NotFound(program)) => Reply::Missing(program),
        Err(err) => {
            let mut reason = err.to_string();
            let mut cause = err.source();
            while let Some(source) = cause {
                reason.push_str(": ");
                reason.push_str(&source.to_string());
                cause = source.source();
            }
            Reply::Failed(reason)
        }
    };
    // 呼び出し元が先に終わったとき (`korylusfactory start` を止めたときなど) は、結果を読む相手がいないため、
    // 呼び出し元の代わりに制御ディレクトリを片付ける。片付けられなくても、セッションは終わらせてある。
    let owner = i32::try_from(request.owner).ok().and_then(Pid::from_raw);
    if process::getppid() != owner {
        let _ = fs::remove_dir_all(dir);
        return Ok(());
    }
    let path = dir.join(REPLY);
    let data =
        serde_json::to_vec(&reply).map_err(|source| control(&path, io::Error::other(source)))?;
    fs::write(&path, data).map_err(|source| control(&path, source))
}

fn spawn(session: &Session) -> Result<std::process::Child, Error> {
    let log = |path: &Path| {
        File::create(path).map_err(|source| Error::Log {
            path: path.to_path_buf(),
            source,
        })
    };
    let mut command = Command::new(&session.program);
    command
        .args(&session.args)
        .envs(session.env.iter().map(|(key, value)| (key, value)))
        .env("GIT_TERMINAL_PROMPT", "0")
        .current_dir(&session.dir)
        .stdin(Stdio::piped())
        .stdout(log(&session.stdout)?)
        .stderr(log(&session.stderr)?);
    std::os::unix::process::CommandExt::process_group(&mut command, 0);
    let mut child = command.spawn().map_err(|source| match source.kind() {
        io::ErrorKind::NotFound => Error::NotFound(session.program.clone()),
        _ => Error::Spawn {
            program: session.program.clone(),
            source,
        },
    })?;
    if let Some(mut stdin) = child.stdin.take() {
        let input = session.input.clone();
        thread::spawn(move || {
            // セッションが入力を読む前に終了しても、終了状態と結果は監督側で確かめる。
            let _ = stdin.write_all(&input);
        });
    }
    Ok(child)
}

fn run(request: &Request, dir: &Path) -> Result<Ended, Error> {
    // 排他を取ってから親の生存を確かめる。起動直後に親が止まっても、新しいstartと子を重ねない。
    let _exclusive = exclusive(request, dir)?;
    if cancelled(request, dir) {
        return Ok(Ended::Cancelled);
    }
    process::set_child_subreaper(Some(process::getpid()))
        .map_err(|source| control(dir, source.into()))?;
    let child = spawn(&request.session)?;
    let mut family = Family {
        root: child.id(),
        ended: None,
        empty: false,
        termed: HashSet::new(),
    };
    let begun = Instant::now();
    let mut lifecycle = Lifecycle::default();
    loop {
        family.reap().map_err(|source| control(dir, source))?;
        match lifecycle.advance(
            begun.elapsed(),
            request.session.timeout,
            cancelled(request, dir),
            family.ended,
            family.empty,
        ) {
            Action::Wait => {}
            Action::Term => family
                .signal(Signal::TERM)
                .map_err(|source| control(dir, source))?,
            Action::Kill => family
                .signal(Signal::KILL)
                .map_err(|source| control(dir, source))?,
            Action::Done(ended) => return Ok(ended),
        }
        thread::sleep(POLL);
    }
}

/// 親が終わったか、取り消しが届いていれば真を返す。
fn cancelled(request: &Request, dir: &Path) -> bool {
    let owner = i32::try_from(request.owner).ok().and_then(Pid::from_raw);
    dir.join(CANCEL).exists() || !dir.exists() || process::getppid() != owner
}

/// 指定された排他を取る。待つ間に親が終わったら、子を起動せずに取り消す。
fn exclusive(request: &Request, dir: &Path) -> Result<Option<File>, Error> {
    let Some(path) = &request.session.exclusive else {
        return Ok(None);
    };
    let file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(false)
        .open(path)
        .map_err(|source| control(path, source))?;
    loop {
        if cancelled(request, dir) {
            return Ok(None);
        }
        match file.try_lock() {
            Ok(()) => return Ok(Some(file)),
            Err(TryLockError::WouldBlock) => thread::sleep(POLL),
            Err(TryLockError::Error(source)) => return Err(control(path, source)),
        }
    }
}

/// 時間と観測した状態から、次の操作を決める。時計を持たず、テストでは経過時間を渡す。
#[derive(Default)]
struct Lifecycle {
    stopping: Option<(Duration, Ended)>,
}

#[derive(Debug, PartialEq, Eq)]
enum Action {
    Wait,
    Term,
    Kill,
    Done(Ended),
}

impl Lifecycle {
    fn advance(
        &mut self,
        elapsed: Duration,
        timeout: Duration,
        cancelled: bool,
        ended: Option<Ended>,
        empty: bool,
    ) -> Action {
        if empty && let Some(ended) = ended {
            return Action::Done(self.stopping.map_or(ended, |(_, reason)| reason));
        }
        if self.stopping.is_none() {
            let reason = if cancelled {
                Some(Ended::Cancelled)
            } else if let Some(ended) = ended {
                Some(ended)
            } else if elapsed >= timeout {
                Some(Ended::TimedOut)
            } else {
                None
            };
            self.stopping = reason.map(|reason| (elapsed, reason));
        }
        match self.stopping {
            None => Action::Wait,
            Some((since, _)) if elapsed.saturating_sub(since) < GRACE => Action::Term,
            Some(_) => Action::Kill,
        }
    }
}

struct Family {
    root: u32,
    ended: Option<Ended>,
    empty: bool,
    termed: HashSet<Pid>,
}

impl Family {
    fn reap(&mut self) -> io::Result<()> {
        loop {
            match process::wait(WaitOptions::NOHANG) {
                Ok(Some((pid, status))) => {
                    if pid.as_raw_nonzero().get().unsigned_abs() == self.root {
                        self.ended =
                            Some(status.exit_status().map_or(Ended::Signaled, Ended::Exited));
                    }
                }
                Ok(None) => return Ok(()),
                Err(rustix::io::Errno::INTR) => {}
                Err(rustix::io::Errno::CHILD) => {
                    self.empty = true;
                    return Ok(());
                }
                Err(source) => return Err(source.into()),
            }
        }
    }

    fn signal(&mut self, signal: Signal) -> io::Result<()> {
        for pid in descendants()? {
            if signal == Signal::TERM && !self.termed.insert(pid) {
                continue;
            }
            match process::kill_process(pid, signal) {
                Ok(()) | Err(rustix::io::Errno::SRCH) => {}
                Err(source) => return Err(source.into()),
            }
        }
        Ok(())
    }
}

impl Drop for Family {
    fn drop(&mut self) {
        // 通常は全員を回収してから戻る。途中のエラーでも、残った子を可能な限り回収する。
        while !self.empty {
            if self.signal(Signal::KILL).is_err() || self.reap().is_err() {
                break;
            }
            thread::yield_now();
        }
    }
}

/// この専用監督プロセスの子孫だけを返す。親を失った子も、このプロセスの直接の子になる。
fn descendants() -> io::Result<Vec<Pid>> {
    let mut parents = Vec::new();
    for entry in fs::read_dir("/proc")? {
        let entry = entry?;
        let Some(raw) = entry
            .file_name()
            .to_str()
            .and_then(|name| name.parse::<i32>().ok())
        else {
            continue;
        };
        let Some(pid) = Pid::from_raw(raw) else {
            continue;
        };
        let Ok(src) = fs::read_to_string(entry.path().join("stat")) else {
            continue;
        };
        let Some(rest) = src.rfind(')').and_then(|end| src.get(end + 1..)) else {
            continue;
        };
        let Some(parent) = rest
            .split_whitespace()
            .nth(1)
            .and_then(|s| s.parse::<i32>().ok())
            .and_then(Pid::from_raw)
        else {
            continue;
        };
        parents.push((pid, parent));
    }
    let mut found = vec![process::getpid()];
    let mut i = 0;
    while let Some(&parent) = found.get(i) {
        for &(pid, ppid) in &parents {
            if ppid == parent && !found.contains(&pid) {
                found.push(pid);
            }
        }
        i += 1;
    }
    Ok(found.into_iter().skip(1).collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 期限前は待ち_期限を過ぎたらtermを送り_猶予のあとにkillする() {
        let mut lifecycle = Lifecycle::default();
        let timeout = Duration::from_secs(100);
        assert_eq!(
            lifecycle.advance(Duration::from_secs(99), timeout, false, None, false),
            Action::Wait
        );
        assert_eq!(
            lifecycle.advance(timeout, timeout, false, None, false),
            Action::Term
        );
        assert_eq!(
            lifecycle.advance(
                (timeout + GRACE).saturating_sub(Duration::from_nanos(1)),
                timeout,
                false,
                None,
                false
            ),
            Action::Term
        );
        assert_eq!(
            lifecycle.advance(timeout + GRACE, timeout, false, None, false),
            Action::Kill
        );
        assert_eq!(
            lifecycle.advance(timeout + GRACE, timeout, false, Some(Ended::Signaled), true),
            Action::Done(Ended::TimedOut)
        );
    }

    #[test]
    fn 親が先に終わっても子が残っていれば終了処理を続ける() {
        let mut lifecycle = Lifecycle::default();
        let timeout = Duration::from_secs(100);
        assert_eq!(
            lifecycle.advance(
                Duration::ZERO,
                timeout,
                false,
                Some(Ended::Exited(7)),
                false
            ),
            Action::Term
        );
        assert_eq!(
            lifecycle.advance(GRACE, timeout, false, Some(Ended::Exited(7)), false),
            Action::Kill
        );
        assert_eq!(
            lifecycle.advance(GRACE, timeout, false, Some(Ended::Exited(7)), true),
            Action::Done(Ended::Exited(7))
        );
    }

    #[test]
    fn 取り消したら期限前でも終了処理を始める() {
        let mut lifecycle = Lifecycle::default();
        let timeout = Duration::from_secs(100);
        assert_eq!(
            lifecycle.advance(Duration::ZERO, timeout, true, None, false),
            Action::Term
        );
        assert_eq!(
            lifecycle.advance(
                Duration::from_secs(1),
                timeout,
                false,
                Some(Ended::Exited(0)),
                true
            ),
            Action::Done(Ended::Cancelled)
        );
    }

    /// 制御ディレクトリdirで、排他のファイルexclusiveを指定したセッションの依頼を作る。
    ///
    /// 呼び出し元は、このテストのプロセスの親にする。
    /// 起動するコマンドは存在せず、標準出力の記録はコマンドを起動する直前に作るため、起動しようとしたかを記録の有無で確かめられる。
    fn request(dir: &Path, exclusive: &Path) -> Request {
        Request {
            owner: u32::try_from(Pid::as_raw(process::getppid()))
                .expect("親のPIDを読み取れなかった"),
            session: Session {
                program: dir.join("missing").to_string_lossy().into_owned(),
                args: Vec::new(),
                env: Vec::new(),
                dir: dir.to_path_buf(),
                input: Vec::new(),
                stdout: dir.join("stdout.log"),
                stderr: dir.join("stderr.log"),
                timeout: Duration::from_secs(60),
                exclusive: Some(exclusive.to_path_buf()),
            },
        }
    }

    #[test]
    fn 排他をほかが持つ間に取り消されたら子を起動せずに取り消しを返す() {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = dir.path().join("session.lock");
        let other = File::create(&path).expect("排他のファイルを作れなかった");
        other.try_lock().expect("テストが排他を取れなかった");
        fs::write(dir.path().join(CANCEL), "").expect("取り消しを書けなかった");

        let ended = run(&request(dir.path(), &path), dir.path()).expect("取り消しで終われなかった");

        assert_eq!(ended, Ended::Cancelled, "取り消しを返していない");
        assert!(
            !dir.path().join("stdout.log").exists(),
            "排他を取れないまま子を起動しようとした"
        );
    }

    #[test]
    fn ほかが排他を外したら排他を取り_返したファイルを閉じるまで保持する() {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = dir.path().join("session.lock");
        let other = File::create(&path).expect("排他のファイルを作れなかった");
        other.try_lock().expect("テストが排他を取れなかった");
        other.unlock().expect("テストが排他を外せなかった");

        let held = exclusive(&request(dir.path(), &path), dir.path())
            .expect("排他を取れなかった")
            .expect("取り消していないのに排他を取らなかった");

        assert!(
            matches!(other.try_lock(), Err(TryLockError::WouldBlock)),
            "排他を保持していない"
        );
        drop(held);
        other
            .try_lock()
            .expect("返したファイルを閉じても排他が外れなかった");
    }
}
