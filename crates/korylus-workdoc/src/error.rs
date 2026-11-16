/// 作業ドキュメントを読み取れないときのエラー。
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum Error {
    /// 作業計画書の書式に誤りがある。誤りを、見つけた順にすべて持つ。
    #[error("{}", .0.join("\n"))]
    InvalidPlan(Vec<String>),

    /// 作業計画書の「タスクリスト」節にタスクが無い。
    #[error("「タスクリスト」節にタスクが見つかりません")]
    NoTasks,

    /// レビュードキュメントの書式に誤りがある。誤りを、見つけた順にすべて持つ。
    #[error("{}", .0.join("\n"))]
    InvalidReview(Vec<String>),
}
