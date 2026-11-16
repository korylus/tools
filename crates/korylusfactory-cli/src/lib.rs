//! korylusfactoryのコマンドが使う、外部とのやり取り。
//!
//! 外部プロセス (git・wikino・Claude Code・Codex) の起動は [`process`] にまとめ、ほかのモジュールは
//! [`process::Runner`] と [`process::Launcher`] を通して起動する。テストではこれらを差し替えて、実際のAPIやエージェントを呼ばない。

pub mod agent;
pub mod branch;
pub mod completion;
pub mod config;
pub mod instruction;
pub mod process;
pub mod resume;
pub mod roadmap;
pub mod source;
pub mod start;
pub mod status;
pub mod version;

/// エラーerrのメッセージに、理由を順に添える。
pub fn describe(err: &dyn std::error::Error) -> String {
    let mut message = err.to_string();
    let mut source = err.source();
    while let Some(err) = source {
        message.push_str(": ");
        message.push_str(&err.to_string());
        source = err.source();
    }
    message
}
