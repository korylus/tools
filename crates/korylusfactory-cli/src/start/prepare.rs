//! タスクの対象を求め、ブランチを作るタスクならブランチを準備する。
//!
//! ブランチを作る元は、korylusflowと同じく次の順に決める。
//!
//! 1. 同じリポジトリで前のタスクのブランチがローカルに残っていれば、そのブランチ
//! 2. 作業計画書の `**ベースブランチ**:` の行に、そのリポジトリのブランチを書いていれば、そのブランチ
//! 3. developがあればdevelop、無ければmain

use std::path::{Path, PathBuf};

use korylusfactory_core::flow::{BaseBranch, PlanDocument, Prepared, Task};
use korylusfactory_core::state::PlanState;

use super::EXCLUDED;
use crate::branch::{self, Target};
use crate::describe;
use crate::process::Runner;

/// 状態stateで進めている作業計画書docのタスクtaskの対象を求め、ブランチを作るタスクならブランチnameを準備する。
///
/// 準備できないときは、人間に伝える理由を [`Prepared::Failed`] で返す。
pub(super) fn prepare(
    runner: &dyn Runner,
    state: &PlanState,
    doc: &PlanDocument,
    task: &Task,
    name: &str,
) -> Prepared {
    let target = match branch::resolve(runner, &task.platform, &task.assignee) {
        Ok(target) => target,
        Err(err @ branch::Error::AssigneeMismatch(_)) => {
            return failed("タスクの接頭辞と担当の行が食い違っています", &err);
        }
        Err(err) => return failed("タスクのリポジトリを求められません", &err),
    };
    let (dir, repo) = match target {
        Target::Human => return Prepared::Human,
        Target::Implementer => return Prepared::Work,
        Target::Repo { dir, repo } => (dir, repo),
    };
    let base = match base_branch(runner, state, &doc.plan().bases, &repo) {
        Ok(base) => base,
        Err(reason) => {
            return Prepared::Failed {
                reason: "ブランチを作る元のブランチを決められません".to_owned(),
                details: vec![reason],
            };
        }
    };
    match branch::prepare(runner, &repo, name, &base, &EXCLUDED) {
        Ok(()) => Prepared::Branch { repo, dir, base },
        Err(branch::Error::Dirty { repo, files }) => Prepared::Failed {
            reason: format!(
                "{} に未コミットの変更があるため、ブランチ {name} を準備できません",
                repo.display()
            ),
            details: files,
        },
        Err(err) => failed(&format!("ブランチ {name} を準備できません"), &err),
    }
}

/// 理由reasonと、エラーerrを詳細にした、準備できなかった結果を返す。
fn failed(reason: &str, err: &branch::Error) -> Prepared {
    Prepared::Failed {
        reason: reason.to_owned(),
        details: vec![describe(err)],
    }
}

/// リポジトリrepoで、次のタスクのブランチを作る元のブランチを返す。決められなければ、人間に伝える理由を返す。
fn base_branch(
    runner: &dyn Runner,
    state: &PlanState,
    bases: &[BaseBranch],
    repo: &Path,
) -> Result<String, String> {
    let exists = |name: &str| branch::exists(runner, repo, name).map_err(|err| describe(&err));
    if let Some(previous) = state.previous_branch(repo)
        && exists(previous)?
    {
        return Ok(previous.to_owned());
    }
    let Some(declared) = declared_base(runner, bases, repo)? else {
        return branch::default_base(runner, repo)
            .map(str::to_owned)
            .map_err(|err| describe(&err));
    };
    if !exists(&declared)? {
        return Err(format!(
            "作業計画書のベースブランチの行に書いたブランチ {declared} が {} にありません",
            repo.display()
        ));
    }
    Ok(declared)
}

/// 作業計画書のベースブランチの行basesに書いた、リポジトリrepoのブランチを返す。書いていなければ `None` を返す。
///
/// 打ち間違えたパスを黙って無視しないよう、repo以外のリポジトリの宣言も含めて、すべてのパスをリポジトリに揃えてから照らし合わせる。
fn declared_base(
    runner: &dyn Runner,
    bases: &[BaseBranch],
    repo: &Path,
) -> Result<Option<String>, String> {
    let mut by_repo: Vec<(PathBuf, &BaseBranch)> = Vec::new();
    for base in bases {
        let resolved = branch::repo(runner, Path::new(&base.dir)).map_err(|err| {
            format!(
                "作業計画書のベースブランチの行のパス {}: {}",
                base.dir,
                describe(&err)
            )
        })?;
        if let Some((_, previous)) = by_repo.iter().find(|(known, _)| *known == resolved) {
            if previous.branch != base.branch {
                return Err(format!(
                    "作業計画書のベースブランチの行で、リポジトリ {} に別々のブランチを書いています: {}={}, {}={}",
                    resolved.display(),
                    previous.dir,
                    previous.branch,
                    base.dir,
                    base.branch
                ));
            }
            continue;
        }
        by_repo.push((resolved, base));
    }
    Ok(by_repo
        .into_iter()
        .find(|(known, _)| known == repo)
        .map(|(_, base)| base.branch.clone()))
}
