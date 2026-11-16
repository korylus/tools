//! `korylusfactory resume` の処理。人間待ちを解き、作業計画書の進行を再開する。
//!
//! 待っていた場面ごとの再開の条件は、`korylusfactory-core` の状態遷移が確かめる。
//! ここでは、待っている作業計画書を選び、作業計画書と本体のリポジトリを読み、状態ファイルのロックの中で状態を進める。

use std::path::Path;

use korylusfactory_core::flow::{self, PlanDocument, Project};
use korylusfactory_core::state::{self, Store, WaitKind};

use crate::branch;
use crate::process::Runner;
use crate::source::{self, Kind, Wikino};

/// 再開できないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// 人間を待っている作業計画書が無い。
    #[error("人間を待っている作業計画書がありません")]
    NotWaiting,

    /// 人間を待っている作業計画書が複数あり、どれを再開するかを決められない。
    #[error(
        "人間を待っている作業計画書が複数あります: {}",
        .0.join("・")
    )]
    Ambiguous(Vec<String>),

    /// 状態ファイルを読み書きできない。
    #[error(transparent)]
    State(#[from] state::Error),

    /// 作業計画書を読めない。
    #[error(transparent)]
    Source(#[from] source::Error),

    /// 作業計画書を読み取れない。
    #[error("作業計画書 {url} を読み取れません")]
    Plan {
        /// 作業計画書のURL。
        url: String,
        /// 読み取れなかった理由。
        #[source]
        source: flow::PlanError,
    },

    /// 本体のリポジトリを求められない。
    #[error(transparent)]
    Branch(#[from] branch::Error),

    /// 再開の条件を満たさないか、状態を進められない。
    #[error(transparent)]
    Flow(#[from] flow::Error),
}

/// 再開した作業計画書。
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Resumed {
    /// 計画書名。
    pub name: String,
    /// 待っていた場面。
    pub kind: WaitKind,
}

/// プロジェクトのルートrootで人間を待っている作業計画書を、待っていた場面に応じて再開する。
///
/// review_limitを渡すと、レビューの上限で待っているタスクか作業報告書の上限を、その回数に延ばす。
/// 再開の条件を満たさなければ、状態を書かずにエラーを返す。
pub fn run(runner: &dyn Runner, root: &Path, review_limit: Option<u32>) -> Result<Resumed, Error> {
    let store = Store::new(root);
    let mut waiting: Vec<String> = store
        .active()?
        .into_iter()
        .filter(|(_, state)| state.wait.is_some())
        .map(|(name, _)| name)
        .collect();
    let name = match waiting.len() {
        0 => return Err(Error::NotWaiting),
        1 => waiting.remove(0),
        _ => return Err(Error::Ambiguous(waiting)),
    };
    let project = Project {
        root: root.to_path_buf(),
        repo: branch::toplevel(runner, root)?,
    };

    let lock = store.plan(&name)?.lock()?;
    // ロックを取るまでの間に、ほかのプロセスが進めていることがあるため、ロックの中で読み直す。
    let Some(mut state) = lock.load()? else {
        return Err(Error::NotWaiting);
    };
    let Some(kind) = state.wait.as_ref().map(|wait| wait.kind) else {
        return Err(Error::NotWaiting);
    };
    let body = Wikino::parse_as(Kind::Plan, &state.url)?.read(runner)?;
    let doc = PlanDocument::parse(body).map_err(|source| Error::Plan {
        url: state.url.clone(),
        source,
    })?;
    flow::resume(&mut state, &doc, &project, review_limit)?;
    lock.save(&state)?;
    Ok(Resumed { name, kind })
}

#[cfg(test)]
mod tests {
    use std::ffi::OsString;

    use korylusfactory_core::state::{PlanState, Wait};

    use super::*;
    use crate::branch::testutil::repo_on;
    use crate::process::{self, Output, System};

    /// wikinoでは決めた作業計画書を返し、gitは本物を実行するRunner。
    struct Plan(String);

    impl Runner for Plan {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            if program == "git" {
                return System.run(program, args);
            }
            assert_eq!(program, "wikino", "想定しないコマンドを実行した");
            Ok(Output {
                stdout: format!("---\nwikino_number: 1\n---\n\n{}", self.0).into_bytes(),
                ..Output::default()
            })
        }
    }

    const PLAN: &str = "**計画書名**: demo\n\n## タスクリスト\n\n- [x] **1-1**: [/korylus-tools] タスク\n- [x] **1-2**: APIキーを発行する\n  - **担当**: shimbaco\n";

    /// 計画書名nameの作業計画書を、場面kindで人間を待たせた状態で置く。
    fn save_waiting(store: &Store, name: &str, kind: WaitKind) {
        let mut state = PlanState::new(
            "https://wikino.app/s/shimbaco/pages/1",
            format!("{name} 作業計画書"),
        );
        state.task = Some("1-2".to_owned());
        state.snapshot = Some(PLAN.to_owned());
        state.wait = Some(Wait {
            kind,
            reason: "人が行うタスクです".to_owned(),
            details: Vec::new(),
        });
        store
            .plan(name)
            .expect("状態の置き場を決められなかった")
            .lock()
            .expect("ロックを取れなかった")
            .save(&state)
            .expect("状態を保存できなかった");
    }

    fn load(store: &Store, name: &str) -> PlanState {
        store
            .plan(name)
            .expect("状態の置き場を決められなかった")
            .load()
            .expect("状態を読めなかった")
            .expect("状態ファイルが無い")
    }

    #[test]
    fn 人間を待っている作業計画書を再開して状態を書く() {
        let repo = repo_on("main");
        let store = Store::new(&repo.root);
        save_waiting(&store, "demo", WaitKind::HumanTask);

        let resumed = run(&Plan(PLAN.to_owned()), &repo.root, None).expect("再開できなかった");
        assert_eq!(
            resumed,
            Resumed {
                name: "demo".to_owned(),
                kind: WaitKind::HumanTask,
            }
        );
        assert_eq!(
            load(&store, "demo").wait,
            None,
            "人間待ちを解いた状態を書いていない"
        );
    }

    #[test]
    fn 再開の条件を満たさなければ状態を書かない() {
        let repo = repo_on("main");
        let store = Store::new(&repo.root);
        save_waiting(&store, "demo", WaitKind::Report);
        let before = load(&store, "demo");

        // フィードバック対応のフェーズを足していない作業計画書では、作業報告書の確認から再開できない。
        let err = run(&Plan(PLAN.to_owned()), &repo.root, None).expect_err("再開できてしまった");
        assert!(
            matches!(err, Error::Flow(flow::Error::Rejected(_))),
            "エラー = {err:?}"
        );
        assert_eq!(load(&store, "demo"), before, "再開できないのに状態を書いた");
    }

    #[test]
    fn 人間を待っている作業計画書が無いか複数あれば再開しない() {
        let repo = repo_on("main");
        let store = Store::new(&repo.root);
        let err = run(&Plan(PLAN.to_owned()), &repo.root, None).expect_err("再開できてしまった");
        assert!(matches!(err, Error::NotWaiting), "エラー = {err:?}");

        save_waiting(&store, "first", WaitKind::HumanTask);
        save_waiting(&store, "second", WaitKind::HumanTask);
        let err = run(&Plan(PLAN.to_owned()), &repo.root, None).expect_err("再開できてしまった");
        assert_eq!(
            err.to_string(),
            "人間を待っている作業計画書が複数あります: first・second"
        );
    }
}
