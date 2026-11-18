//! 人間待ちを知らせる、通知のコマンドの実行。
//!
//! 通知のコマンドは、設定ファイルの `notify` に書いたコマンドである。
//! コマンドには環境変数で、プロジェクト名・作業計画書の名前・場面・人間キューの項目のURLだけを渡し、項目の本文は渡さない。
//! 通知先がプロジェクトの外のサービスになりうるためである。
//! 本物の実装は [`System`] で、テストでは [`Notifier`] を差し替えてコマンドを実行しない。

use std::io::{self, Read as _};
use std::os::unix::process::CommandExt as _;
use std::process::{Child, ChildStderr, Command, ExitStatus, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use rustix::process::{Pid, Signal, kill_process_group};

use crate::config::Notify;
use crate::process::System;
use crate::source::stderr_line;

/// 通知のコマンドを終わらせるまでの時間の上限。
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(30);

/// 通知のコマンドが終わったかを確かめる間隔。
const POLL: Duration = Duration::from_millis(50);

/// 通知のコマンドに渡す環境変数の名前。
const PROJECT_VAR: &str = "KORYLUSFACTORY_PROJECT";
const PLAN_VAR: &str = "KORYLUSFACTORY_PLAN";
const SCENE_VAR: &str = "KORYLUSFACTORY_SCENE";
const URL_VAR: &str = "KORYLUSFACTORY_URL";

/// 通知のコマンドに渡す内容。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Message {
    /// プロジェクト名。
    pub project: String,
    /// 作業計画書の名前 (計画書名。読み取れない作業計画書では、ロードマップのWikiリンクのタイトル)。
    pub plan: String,
    /// 場面。
    pub scene: String,
    /// 人間キューの項目のURL。項目を積めていないか、人間キューを使わないときは `None` で、環境変数を渡さない。
    pub url: Option<String>,
}

/// 通知のコマンドが失敗したときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// コマンドが見つからない。
    #[error("通知のコマンド {0} が見つかりません")]
    NotFound(String),

    /// コマンドを起動できなかった。
    #[error("通知のコマンド {program} を実行できません")]
    Spawn {
        /// 起動しようとしたコマンド。
        program: String,
        /// 起動できなかった理由。
        #[source]
        source: io::Error,
    },

    /// コマンドが0以外の終了コードで終わった。
    #[error("通知のコマンド {program} が終了コード {code} で失敗しました{}", stderr_line(.stderr))]
    Failed {
        /// 実行したコマンド。
        program: String,
        /// 終了コード。
        code: i32,
        /// コマンドの標準エラー。
        stderr: String,
    },

    /// コマンドがシグナルで終わった。
    #[error("通知のコマンド {0} がシグナルで終了しました")]
    Signaled(String),

    /// コマンドが時間の上限を過ぎたため、終わらせた。
    #[error("通知のコマンド {program} が時間の上限 ({}秒) を過ぎたため、終わらせました", .timeout.as_secs())]
    TimedOut {
        /// 実行したコマンド。
        program: String,
        /// 時間の上限。
        timeout: Duration,
    },

    /// コマンドが終わったかを確かめられない。
    #[error("通知のコマンド {program} が終わったかを確かめられません")]
    Wait {
        /// 実行したコマンド。
        program: String,
        /// 確かめられなかった理由。
        #[source]
        source: io::Error,
    },
}

/// 通知のコマンドを実行する。
pub trait Notifier {
    /// 通知のコマンドcommandを、内容messageを環境変数で渡して実行し、終わるまで待つ。timeoutを過ぎたら終わらせる。
    fn notify(&self, command: &Notify, message: &Message, timeout: Duration) -> Result<(), Error>;
}

impl Notifier for System {
    fn notify(&self, command: &Notify, message: &Message, timeout: Duration) -> Result<(), Error> {
        let program = command.program.clone();
        let mut cmd = Command::new(&command.program);
        cmd.args(&command.args)
            .env(PROJECT_VAR, &message.project)
            .env(PLAN_VAR, &message.plan)
            .env(SCENE_VAR, &message.scene)
            .stdin(Stdio::null())
            // 標準出力は監督のループの進捗に混ぜない。標準エラーは、失敗したときの警告に添える。
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .process_group(0);
        match &message.url {
            Some(url) => cmd.env(URL_VAR, url),
            // 呼び出し元の環境から、前の項目のURLを引き継がせない。
            None => cmd.env_remove(URL_VAR),
        };
        let mut child = cmd.spawn().map_err(|source| match source.kind() {
            io::ErrorKind::NotFound => Error::NotFound(program.clone()),
            _ => Error::Spawn {
                program: program.clone(),
                source,
            },
        })?;
        let (status, stderr) = match observe(&mut child, timeout) {
            Ok(Some(output)) => output,
            Ok(None) => {
                stop(&mut child);
                return Err(Error::TimedOut { program, timeout });
            }
            Err(source) => {
                stop(&mut child);
                return Err(Error::Wait { program, source });
            }
        };
        match status.code() {
            Some(0) => Ok(()),
            Some(code) => Err(Error::Failed {
                program,
                code,
                stderr: String::from_utf8_lossy(&stderr).into_owned(),
            }),
            None => Err(Error::Signaled(program)),
        }
    }
}

/// 直接のプロセスの終了と標準エラーのEOFを、timeoutの範囲で待つ。期限なら `None`。
fn observe(child: &mut Child, timeout: Duration) -> io::Result<Option<(ExitStatus, Vec<u8>)>> {
    let begun = Instant::now();
    let mut pipe = child
        .stderr
        .take()
        .ok_or_else(|| io::Error::other("通知の標準エラーのパイプがありません"))?;
    rustix::io::ioctl_fionbio(&pipe, true)?;
    let mut stderr = Vec::new();
    let mut status = None;
    let mut closed = false;
    loop {
        if !closed {
            closed = drain(&mut pipe, &mut stderr);
        }
        if status.is_none() {
            status = child.try_wait()?;
        }
        if closed && let Some(status) = status {
            return Ok(Some((status, stderr)));
        }
        if begun.elapsed() >= timeout {
            return Ok(None);
        }
        thread::sleep(POLL.min(timeout.saturating_sub(begun.elapsed())));
    }
}

/// 非ブロッキングの標準エラーpipeから、今読める分をstderrに足す。EOFか読み取り不能なら真。
fn drain(pipe: &mut ChildStderr, stderr: &mut Vec<u8>) -> bool {
    let mut buffer = [0; 4096];
    // 書き続けるコマンドでも、定期的に終了状態と期限を確認する。
    for _ in 0..64 {
        match pipe.read(&mut buffer) {
            Ok(0) => return true,
            Ok(read) => stderr.extend_from_slice(&buffer[..read]),
            Err(err) if err.kind() == io::ErrorKind::WouldBlock => return false,
            Err(err) if err.kind() == io::ErrorKind::Interrupted => {}
            // 警告に添える出力なので、読めなかった部分は添えずに終了状態を待つ。
            Err(_) => return true,
        }
    }
    false
}

/// 通知のプロセスグループを終わらせ、直接の子を回収する。後始末の失敗は元のエラーに上書きしない。
fn stop(child: &mut Child) {
    if let Some(pid) = i32::try_from(child.id()).ok().and_then(Pid::from_raw) {
        let _ = kill_process_group(pid, Signal::KILL);
    }
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    fn sh(script: &str, args: &[&str]) -> Notify {
        let mut all = vec!["-c".to_owned(), script.to_owned(), "sh".to_owned()];
        all.extend(args.iter().map(|arg| (*arg).to_owned()));
        Notify {
            program: "sh".to_owned(),
            args: all,
        }
    }

    fn message(url: Option<&str>) -> Message {
        Message {
            project: "mewst".to_owned(),
            plan: "demo".to_owned(),
            scene: "人が行うタスク".to_owned(),
            url: url.map(str::to_owned),
        }
    }

    #[test]
    fn 環境変数で内容を渡し_urlが無ければ渡さない() {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = dir.path().join("out");
        let path_str = path.to_str().expect("パスがUTF-8ではない");
        let command = sh(
            r#"printf '%s|%s|%s|%s' "$KORYLUSFACTORY_PROJECT" "$KORYLUSFACTORY_PLAN" "$KORYLUSFACTORY_SCENE" "${KORYLUSFACTORY_URL-無し}" > "$1""#,
            &[path_str],
        );
        let cases = [
            (
                Some("https://wikino.app/s/shimbaco/pages/1/edit"),
                "mewst|demo|人が行うタスク|https://wikino.app/s/shimbaco/pages/1/edit",
            ),
            (None, "mewst|demo|人が行うタスク|無し"),
        ];
        for (url, want) in cases {
            System
                .notify(&command, &message(url), DEFAULT_TIMEOUT)
                .expect("通知のコマンドが失敗した");
            assert_eq!(
                fs::read_to_string(&path).expect("コマンドの出力を読めなかった"),
                want
            );
        }
    }

    #[test]
    fn 失敗した終了コードは標準エラーを添えて返す() {
        let err = System
            .notify(
                &sh("echo 送れません >&2; exit 3", &[]),
                &message(None),
                DEFAULT_TIMEOUT,
            )
            .expect_err("失敗した終了コードでエラーにならなかった");
        assert_eq!(
            err.to_string(),
            "通知のコマンド sh が終了コード 3 で失敗しました\n送れません"
        );
    }

    #[test]
    fn コマンドが見つからなければ見つからないエラー() {
        let command = Notify {
            program: "korylusfactory-test-no-such-notify".to_owned(),
            args: Vec::new(),
        };
        let err = System
            .notify(&command, &message(None), DEFAULT_TIMEOUT)
            .expect_err("見つからないコマンドでエラーにならなかった");
        assert!(matches!(err, Error::NotFound(_)), "エラー = {err:?}");
    }

    #[test]
    fn 時間の上限を過ぎたら終わらせて時間切れを返す() {
        // 上限を0にして、終わりを待たずに時間切れにする。
        let err = System
            .notify(&sh("sleep 30", &[]), &message(None), Duration::ZERO)
            .expect_err("時間切れにならなかった");
        assert!(matches!(err, Error::TimedOut { .. }), "エラー = {err:?}");
    }

    #[test]
    fn 親が先に終了して子が標準エラーを持っていても期限で戻る() {
        let err = System
            .notify(
                &sh("sleep 2 & exit 0", &[]),
                &message(None),
                Duration::from_millis(50),
            )
            .expect_err("標準エラーのEOFを期限を超えて待った");
        assert!(matches!(err, Error::TimedOut { .. }), "エラー = {err:?}");
    }

    #[test]
    fn パイプの容量を超える標準エラーも実行中に読んで終了を待つ() {
        let err = System
            .notify(
                &sh("head -c 262144 /dev/zero >&2; exit 3", &[]),
                &message(None),
                Duration::from_secs(3),
            )
            .expect_err("失敗した終了コードで成功になった");
        let Error::Failed { code, stderr, .. } = err else {
            panic!("標準エラーが詰まって終了しなかった: {err:?}");
        };
        assert_eq!(code, 3);
        assert_eq!(stderr.len(), 262_144, "標準エラーを読み残した");
    }
}
