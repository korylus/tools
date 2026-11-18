//! 作業ドキュメント (作業計画書・レビュードキュメント・ロードマップ) の読み取りと、人間キューの項目の読み書き。
//!
//! 読み書きは渡された本文だけで行い、外部プロセスを起動しない。

mod error;
mod markdown;
pub mod plan;
pub mod queue;
mod quote;
pub mod review;
pub mod roadmap;

pub use error::Error;
