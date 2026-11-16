//! セッションを専用の監督プロセスで起動し、その終了を待つ。
//!
//! 監督プロセスは孤児になった子孫を引き取り、セッションが終わる前に回収する。
//! 制御ファイルはセッションごとの一時ディレクトリに置き、取り消しと結果の受け渡しに使う。

mod supervisor;

use std::ffi::OsString;
use std::fmt;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use serde::{Deserialize, Serialize};

use super::{Error, System};

pub use supervisor::supervise;

/// 監督プロセスが使う内部の起動引数。
pub const SUPERVISOR_COMMAND: &str = "__session-supervisor";

/// セッションの制御を確かめる間隔。
const POLL: Duration = Duration::from_millis(100);
const REQUEST: &str = "request.json";
const REPLY: &str = "reply.json";
const CANCEL: &str = "cancel";

/// 起動するセッション。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    /// 起動するコマンド。
    pub program: String,
    /// コマンドの引数。
    pub args: Vec<OsString>,
    /// 足す環境変数。
    pub env: Vec<(OsString, OsString)>,
    /// 作業するディレクトリ。
    pub dir: PathBuf,
    /// 標準入力に渡す内容。
    pub input: Vec<u8>,
    /// 標準出力を残すファイル。
    pub stdout: PathBuf,
    /// 標準エラーを残すファイル。
    pub stderr: PathBuf,
    /// 時間の上限。
    pub timeout: Duration,
    /// 子孫の回収が終わるまで監督プロセスが保持する排他のファイル。指定しなければ排他を取らない。
    #[serde(default)]
    pub exclusive: Option<PathBuf>,
}

/// セッションの終わり方。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Ended {
    /// 終了コードで終わった。
    Exited(i32),
    /// シグナルで終わった。
    Signaled,
    /// 時間の上限を過ぎたため、終わらせた。
    TimedOut,
    /// 取り消されたため、終わらせた。
    Cancelled,
}

/// セッションを起動する。テストでは差し替える。
pub trait Launcher {
    /// セッションsessionを起動し、終わるまで待つ。
    ///
    /// cancelが真になったら、子孫まで終わらせて [`Ended::Cancelled`] を返す。
    fn launch(&self, session: &Session, cancel: &AtomicBool) -> Result<Ended, Error>;
}

/// 監督プロセスがセッションの子孫を回収するまで、ファイルの排他を保持させるLauncher。
pub struct Exclusive<'a> {
    launcher: &'a dyn Launcher,
    path: &'a Path,
}

impl<'a> Exclusive<'a> {
    /// 起動先launcherに、排他のファイルpathを指定する。
    pub fn new(launcher: &'a dyn Launcher, path: &'a Path) -> Self {
        Self { launcher, path }
    }
}

impl fmt::Debug for Exclusive<'_> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Exclusive")
            .field("path", &self.path)
            .finish_non_exhaustive()
    }
}

impl Launcher for Exclusive<'_> {
    fn launch(&self, session: &Session, cancel: &AtomicBool) -> Result<Ended, Error> {
        let mut session = session.clone();
        session.exclusive = Some(self.path.to_path_buf());
        self.launcher.launch(&session, cancel)
    }
}

/// 指定したkorylusfactoryの実行ファイルで監督プロセスを起動する。
///
/// 実行ファイルは [`SUPERVISOR_COMMAND`] の内部起動を扱うものを渡す。
/// 別プロセスにすることで、呼び出し側が起動したほかのプロセスを回収の対象に含めない。
#[derive(Debug, Clone)]
pub struct Supervisor {
    executable: PathBuf,
}

impl Supervisor {
    /// 監督プロセスを起動するkorylusfactoryの実行ファイルexecutableを指定する。
    pub fn new(executable: impl Into<PathBuf>) -> Self {
        Self {
            executable: executable.into(),
        }
    }
}

#[derive(Serialize, Deserialize)]
struct Request {
    owner: u32,
    session: Session,
}

#[derive(Serialize, Deserialize)]
enum Reply {
    Ended(Ended),
    Missing(String),
    Failed(String),
}

impl Launcher for System {
    fn launch(&self, session: &Session, cancel: &AtomicBool) -> Result<Ended, Error> {
        let executable = std::env::current_exe()
            .map_err(|source| control(Path::new("/proc/self/exe"), source))?;
        if executable.file_name() != Some(std::ffi::OsStr::new("korylusfactory")) {
            return Err(control(
                &executable,
                io::Error::new(
                    io::ErrorKind::Unsupported,
                    "この実行ファイルでは監督プロセスを起動できません。Supervisor::newでkorylusfactoryの実行ファイルを指定してください",
                ),
            ));
        }
        Supervisor::new(executable).launch(session, cancel)
    }
}

impl Launcher for Supervisor {
    fn launch(&self, session: &Session, cancel: &AtomicBool) -> Result<Ended, Error> {
        if cancel.load(Ordering::SeqCst) {
            return Ok(Ended::Cancelled);
        }
        let dir = tempfile::tempdir().map_err(|source| control(&std::env::temp_dir(), source))?;
        let request = Request {
            owner: std::process::id(),
            session: session.clone(),
        };
        let path = dir.path().join(REQUEST);
        let data = serde_json::to_vec(&request)
            .map_err(|source| control(&path, io::Error::other(source)))?;
        fs::write(&path, data).map_err(|source| control(&path, source))?;
        let mut command = Command::new(&self.executable);
        command
            .arg(SUPERVISOR_COMMAND)
            .arg(dir.path())
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit());
        // Ctrl-Cは呼び出し側が取り消しとして伝え、監督プロセス自身には直接届けない。
        std::os::unix::process::CommandExt::process_group(&mut command, 0);
        let mut child = command.spawn().map_err(|source| Error::Spawn {
            program: "セッションの監督プロセス".to_owned(),
            source,
        })?;
        let cancel_path = dir.path().join(CANCEL);
        let mut cancelled = false;
        loop {
            if let Some(status) = child.try_wait().map_err(|source| Error::Wait {
                program: "セッションの監督プロセス".to_owned(),
                source,
            })? {
                if !status.success() {
                    return Err(control(
                        dir.path(),
                        io::Error::other("セッションの監督プロセスが異常終了しました"),
                    ));
                }
                let path = dir.path().join(REPLY);
                let data = fs::read(&path).map_err(|source| control(&path, source))?;
                let reply: Reply = serde_json::from_slice(&data)
                    .map_err(|source| control(&path, io::Error::other(source)))?;
                return match reply {
                    Reply::Ended(ended) => Ok(ended),
                    Reply::Missing(program) => Err(Error::NotFound(program)),
                    Reply::Failed(reason) => Err(control(dir.path(), io::Error::other(reason))),
                };
            }
            if cancel.load(Ordering::SeqCst) && !cancelled {
                fs::write(&cancel_path, []).map_err(|source| control(&cancel_path, source))?;
                cancelled = true;
            }
            thread::sleep(POLL);
        }
    }
}

fn control(path: &Path, source: io::Error) -> Error {
    Error::Control {
        path: path.to_path_buf(),
        source,
    }
}
