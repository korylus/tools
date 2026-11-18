//! korylusfactoryの判定と状態。作業ドキュメントの読み取りの結果から進める作業計画書を決め、
//! 作業計画書ごとの進行を状態ファイルに読み書きし、タスクを進める状態遷移を決める。
//!
//! 外部プロセスの起動はコマンドのクレート (`korylusfactory-cli`) に任せ、ここでは起動しない。

pub mod flow;
pub mod item;
pub mod queue;
pub mod state;
