//! 作業ドキュメント (作業計画書・レビュードキュメント・ロードマップ) の読み取り。
//!
//! 読み取りは渡された本文だけで行い、外部プロセスを起動しない。

mod error;
mod markdown;
pub mod plan;
mod quote;
pub mod review;
pub mod roadmap;

pub use error::Error;
