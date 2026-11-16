//! 外部プロセスの起動。
//!
//! すぐ終わるコマンドの起動は [`Runner`]、エージェントのセッションのように長く動くプロセスの起動は [`Launcher`] の裏に置く。
//! 本物の実装 [`System`] は、シェルを通さずに子プロセスを起動する。

mod session;

use std::ffi::OsString;
use std::io;
use std::path::PathBuf;
use std::process::{Command, Stdio};

pub use session::{Ended, Exclusive, Launcher, SUPERVISOR_COMMAND, Session, Supervisor, supervise};

/// 外部プロセスを1回実行した結果。
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Output {
    /// 標準出力。
    pub stdout: Vec<u8>,
    /// 標準エラー。
    pub stderr: Vec<u8>,
    /// 終了コード。
    pub code: i32,
}

/// 外部プロセスを実行できなかったときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 専用監督プロセスの制御を読み書きできない。
    #[error("セッションの制御 {} を読み書きできません", .path.display())]
    Control {
        /// 制御のファイルかディレクトリ。
        path: PathBuf,
        /// 読み書きできなかった理由。
        #[source]
        source: io::Error,
    },

    /// コマンドが見つからない。
    #[error("{0}コマンドが見つかりません")]
    NotFound(String),

    /// コマンドを起動できなかった。
    #[error("{program}コマンドを実行できません")]
    Spawn {
        /// 起動しようとしたコマンド。
        program: String,
        /// 起動できなかった理由。
        #[source]
        source: io::Error,
    },

    /// コマンドがシグナルで終わり、終了コードが無い。
    #[error("{0}コマンドがシグナルで終了しました")]
    Signaled(String),

    /// セッションの出力を残すファイルを作れない。
    #[error("セッションの出力を残すファイル {} を作れません", .path.display())]
    Log {
        /// 作ろうとしたファイル。
        path: PathBuf,
        /// 作れなかった理由。
        #[source]
        source: io::Error,
    },

    /// セッションが終わったかを確かめられない。
    #[error("{program}コマンドが終わったかを確かめられません")]
    Wait {
        /// 起動したコマンド。
        program: String,
        /// 確かめられなかった理由。
        #[source]
        source: io::Error,
    },
}

/// 外部プロセスを実行する。
pub trait Runner {
    /// コマンドprogramを引数argsで実行する。
    ///
    /// 終了コードが0以外でも、起動できて終了コードで終われば `Ok` を返す。
    /// 終了コードの意味はコマンドごとに異なるため、呼び出し側で読む。
    fn run(&self, program: &str, args: &[OsString]) -> Result<Output, Error>;
}

/// korylusfactoryの実行ファイルで使う、本物の [`Runner`] と [`Launcher`]。
#[derive(Debug, Clone, Copy, Default)]
pub struct System;

impl Runner for System {
    fn run(&self, program: &str, args: &[OsString]) -> Result<Output, Error> {
        // 引数はシェルを通さずに渡すため、値に空白や記号を含んでいても1つの引数のまま届く。
        // 人の入力を待たずに進めるため、gitに認証情報を端末で尋ねさせず、失敗として返させる。
        let output = Command::new(program)
            .args(args)
            .env("GIT_TERMINAL_PROMPT", "0")
            .stdin(Stdio::null())
            .output()
            .map_err(|source| match source.kind() {
                io::ErrorKind::NotFound => Error::NotFound(program.to_owned()),
                _ => Error::Spawn {
                    program: program.to_owned(),
                    source,
                },
            })?;
        let code = output
            .status
            .code()
            .ok_or_else(|| Error::Signaled(program.to_owned()))?;
        Ok(Output {
            stdout: output.stdout,
            stderr: output.stderr,
            code,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    fn args(values: &[&str]) -> Vec<OsString> {
        values.iter().map(OsString::from).collect()
    }

    #[test]
    fn 正常に終われば終了コード0と出力を返す() {
        let output = System
            .run(
                "sh",
                &args(&[
                    "-c",
                    r#"printf '%s,' "$@"; printf err >&2"#,
                    "sh",
                    "draft",
                    "get",
                ]),
            )
            .expect("shを実行できなかった");
        assert_eq!(
            output,
            Output {
                stdout: b"draft,get,".to_vec(),
                stderr: b"err".to_vec(),
                code: 0,
            }
        );
    }

    #[test]
    fn 失敗した終了コードはエラーにせずに返す() {
        let output = System
            .run("sh", &args(&["-c", "printf out; printf err >&2; exit 5"]))
            .expect("shを実行できなかった");
        assert_eq!(
            output,
            Output {
                stdout: b"out".to_vec(),
                stderr: b"err".to_vec(),
                code: 5,
            }
        );
    }

    #[test]
    fn コマンドが見つからなければ見つからないエラー() {
        let err = System
            .run("korylusfactory-test-no-such-command", &[])
            .expect_err("見つからないコマンドでエラーにならなかった");
        assert!(
            matches!(err, Error::NotFound(ref program) if program == "korylusfactory-test-no-such-command"),
            "エラー = {err:?}"
        );
    }

    #[test]
    fn 実行の権限が無ければ起動できないエラー() {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let path = dir.path().join("wikino");
        fs::write(&path, "#!/bin/sh\n").expect("ファイルを書けなかった");
        let program = path.to_str().expect("パスがUTF-8ではない");

        let err = System
            .run(program, &[])
            .expect_err("実行の権限が無いファイルでエラーにならなかった");
        assert!(matches!(err, Error::Spawn { .. }), "エラー = {err:?}");
        assert!(
            err.to_string().ends_with("wikinoコマンドを実行できません"),
            "エラー = {err}"
        );
    }

    #[test]
    fn シグナルで終われば終了コードが無いエラー() {
        let err = System
            .run("sh", &args(&["-c", "kill -9 $$"]))
            .expect_err("シグナルで終わってもエラーにならなかった");
        assert!(matches!(err, Error::Signaled(_)), "エラー = {err:?}");
    }
}
