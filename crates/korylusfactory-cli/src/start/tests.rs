//! 監督のループのテスト。
//!
//! 作業計画書とロードマップは、wikinoに答えるRunnerの差し替えで渡し、gitは一時ディレクトリのリポジトリで本物を実行する。
//! セッションは、決めた順に作業 (コミット・作業計画書のチェック・レビュードキュメントの作成) をして結果を返す偽物で起動する。
//! 時計は、待つ時間を記録するだけで実際には待たず、決めた回数だけ待ったら取り消してループを終わらせる。

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, VecDeque};
use std::ffi::OsString;
use std::fs;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use jiff::{SignedDuration, Timestamp};
use korylusfactory_core::flow::RETRY_LIMIT;
use korylusfactory_core::state::{PlanDir, Step, Verdict};
use serde_json::{Value, json};

use super::*;
use crate::branch::testutil::{Repo, git_run, repo_on, write_file};
use crate::process::{self, Ended, Output, Session, System};

/// 作業計画書のページ番号。
const PLAN_PAGE: u64 = 20;
const PLAN_URL: &str = "https://wikino.app/s/shimbaco/pages/20";
const TITLE: &str = "デモ 作業計画書";

/// wikinoの呼び出しに、持っているページで答えるRunner。gitは本物を実行する。
///
/// 下書きの一覧とページの取得だけに答え、ページはすべて下書きとして返す。
struct Wiki {
    pages: RefCell<HashMap<u64, String>>,
}

impl Wiki {
    fn page(&self, number: u64) -> String {
        self.pages
            .borrow()
            .get(&number)
            .cloned()
            .expect("ページが無い")
    }

    fn set(&self, number: u64, body: String) {
        self.pages.borrow_mut().insert(number, body);
    }

    /// 作業計画書のタスクidにチェックを付ける。
    fn check(&self, id: &str) {
        let body = self
            .page(PLAN_PAGE)
            .replace(&format!("- [ ] **{id}**"), &format!("- [x] **{id}**"));
        self.set(PLAN_PAGE, body);
    }
}

impl Runner for Wiki {
    fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
        if program == "git" {
            return System.run(program, args);
        }
        assert_eq!(program, "wikino", "想定しないコマンドを実行した");
        let args: Vec<String> = args
            .iter()
            .map(|arg| arg.to_string_lossy().into_owned())
            .collect();
        let ok = |stdout: String| Output {
            stdout: stdout.into_bytes(),
            ..Output::default()
        };
        Ok(match (args[0].as_str(), args[1].as_str()) {
            ("draft", "list") => ok(format!(
                "{{\"page_number\":{PLAN_PAGE},\"title\":\"{TITLE}\",\"topic_number\":4,\"topic_name\":\"Mewst\"}}\n"
            )),
            ("page", "list") => ok(String::new()),
            ("draft", "get") => {
                let number: u64 = args[2].parse().expect("ページ番号を読み取れなかった");
                match self.pages.borrow().get(&number) {
                    Some(body) => ok(format!(
                        "---\nwikino_number: {number}\nwikino_topic_number: 4\n---\n\n{body}"
                    )),
                    None => Output {
                        code: 5,
                        ..Output::default()
                    },
                }
            }
            _ => panic!("想定しないwikinoの引数: {args:?}"),
        })
    }
}

/// セッションが行う作業。結果のJSONを返す。`None` を返すと、結果を返さずに終了コード1で終わる。
type Work<'a> = Box<dyn FnOnce(&Wiki) -> Option<Value> + 'a>;

/// 決めた順に作業をして結果を返すLauncher。渡された指示を記録する。
struct Sessions<'a> {
    wiki: &'a Wiki,
    works: RefCell<VecDeque<Work<'a>>>,
    prompts: RefCell<Vec<String>>,
}

impl<'a> Sessions<'a> {
    fn new(wiki: &'a Wiki, works: Vec<Work<'a>>) -> Self {
        Self {
            wiki,
            works: RefCell::new(works.into()),
            prompts: RefCell::new(Vec::new()),
        }
    }

    fn prompts(&self) -> Vec<String> {
        self.prompts.borrow().clone()
    }
}

impl Launcher for Sessions<'_> {
    fn launch(&self, session: &Session, cancel: &AtomicBool) -> Result<Ended, process::Error> {
        self.prompts
            .borrow_mut()
            .push(String::from_utf8(session.input.clone()).expect("指示がUTF-8ではない"));
        let work = self
            .works
            .borrow_mut()
            .pop_front()
            .expect("決めた数より多くセッションを起動した");
        let result = work(self.wiki);
        if cancel.load(Ordering::SeqCst) {
            return Ok(Ended::Cancelled);
        }
        let Some(result) = result else {
            return Ok(Ended::Exited(1));
        };
        let logs = session.stdout.parent().expect("記録のディレクトリが無い");
        match session.program.as_str() {
            "claude" => fs::write(
                &session.stdout,
                json!({"is_error": false, "structured_output": result}).to_string(),
            ),
            "codex" => fs::write(logs.join("result.json"), result.to_string()),
            program => panic!("想定しないエージェント: {program}"),
        }
        .expect("結果を書けなかった");
        Ok(Ended::Exited(0))
    }
}

/// 待つ時間を記録するだけで実際には待たず、決めた回数だけ待ったら取り消す時計。
struct Clock<'a> {
    now: Cell<Timestamp>,
    sleeps: RefCell<Vec<Duration>>,
    stop_after: usize,
    cancel: &'a AtomicBool,
}

impl<'a> Clock<'a> {
    fn new(stop_after: usize, cancel: &'a AtomicBool) -> Self {
        Self {
            now: Cell::new(
                "2026-11-16T01:00:00Z"
                    .parse()
                    .expect("日時を読み取れなかった"),
            ),
            sleeps: RefCell::new(Vec::new()),
            stop_after,
            cancel,
        }
    }

    fn sleeps(&self) -> Vec<Duration> {
        self.sleeps.borrow().clone()
    }
}

impl super::Clock for Clock<'_> {
    fn now(&self) -> Timestamp {
        self.now.get()
    }

    fn sleep(&self, duration: Duration) {
        let duration = SignedDuration::try_from(duration).expect("待つ時間が長すぎる");
        self.now.set(
            self.now
                .get()
                .checked_add(duration)
                .expect("日時を進められなかった"),
        );
        let mut sleeps = self.sleeps.borrow_mut();
        sleeps.push(duration.unsigned_abs());
        if sleeps.len() >= self.stop_after {
            self.cancel.store(true, Ordering::SeqCst);
        }
    }
}

/// 設定ファイルを置いたプロジェクト。プロジェクトのルートが、タスクのリポジトリを兼ねる。
fn project() -> Repo {
    let repo = repo_on("main");
    write_file(
        &repo.root,
        config::CONFIG_PATH,
        br#"{"roadmap": "https://wikino.app/s/shimbaco/pages/1214"}"#,
    );
    repo
}

/// 承認とみなす節「やってる」にdoingを、完了とみなす節「リリース待ち」にreleasedを書いたロードマップ。
fn roadmap(doing: &str, released: &str) -> String {
    format!(
        "## やってる\n\n{doing}\n\n## 優先度: 高\n\n## 優先度: 中\n\n## リリース待ち\n\n{released}\n\n## やった\n"
    )
}

/// リポジトリrootでブランチを作る2つのタスクを持つ作業計画書。
fn plan(root: &Path) -> String {
    format!(
        "**計画書名**: demo\n\n## 設計\n\n状態ファイルを作る。\n\n## 実装判断ログ\n\n## タスクリスト\n\n\
         - [ ] **1-1**: [{root}] 状態ファイルを作る\n  - **想定行数**: 約100行 (実装50行 + テスト50行)\n\
         - [ ] **1-2**: [{root}] 状態を表示する\n  - **想定行数**: 約100行 (実装50行 + テスト50行)\n",
        root = root.display()
    )
}

/// 作業計画書をロードマップの「やってる」に置いたWiki。
fn wiki(root: &Path) -> Wiki {
    let wiki = Wiki {
        pages: RefCell::new(HashMap::new()),
    };
    wiki.set(1214, roadmap(&format!("- [[{TITLE}]]"), ""));
    wiki.set(PLAN_PAGE, plan(root));
    wiki
}

fn build() -> Build {
    Build {
        version: "0.1.0".to_owned(),
        commit: "unknown".to_owned(),
        commit_short: "unknown".to_owned(),
        date: "2026-11-16".to_owned(),
        source_dir: std::path::PathBuf::new(),
    }
}

/// プロジェクトのルートrootで監督のループを動かし、出力を返す。
fn run_loop(root: &Path, wiki: &Wiki, sessions: &Sessions<'_>, clock: &Clock<'_>) -> String {
    let build = build();
    let (out, err) = loop_outputs(root, wiki, sessions, clock, &build);
    format!("{out}{err}")
}

/// 進捗と診断の出力を別々に取得する。
fn loop_outputs(
    root: &Path,
    runner: &dyn Runner,
    launcher: &dyn Launcher,
    clock: &Clock<'_>,
    build: &Build,
) -> (String, String) {
    let start = Start {
        runner,
        launcher,
        clock,
        build,
        root,
        timing: Timing::default(),
    };
    let mut out = Vec::new();
    let mut err = Vec::new();
    run(&start, &mut out, &mut err, clock.cancel).expect("監督のループがエラーで終わった");
    (
        String::from_utf8(out).expect("進捗がUTF-8ではない"),
        String::from_utf8(err).expect("診断がUTF-8ではない"),
    )
}

fn plan_dir(root: &Path) -> PlanDir {
    Store::new(root)
        .plan("demo")
        .expect("状態の置き場を決められなかった")
}

fn load(root: &Path) -> PlanState {
    plan_dir(root)
        .load()
        .expect("状態を読めなかった")
        .expect("状態ファイルが無い")
}

/// リポジトリrootの今のブランチに、ファイルnameを足すコミットをする。
fn commit(root: &Path, name: &str) {
    write_file(root, name, name.as_bytes());
    git_run(root, &["add", name]);
    git_run(root, &["commit", "--quiet", "-m", name]);
}

/// プロジェクトのルートrootに、タスクのブランチbranchのnumber回目のレビュードキュメントを書く。
fn review(root: &Path, branch: &str, number: u32, verdict: &str, findings: &str) {
    write_file(
        root,
        &format!("docs/private/reviews/{branch}-{number:03}.md"),
        format!(
            "# コードレビュー: {branch}\n\n## ファイルごとのレビュー結果\n\n{findings}## 総合評価\n\n**評価**: {verdict}\n"
        )
        .as_bytes(),
    );
}

const AUTO_FIX: &str = "### 1. 名前を揃える\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正案の通り直す (推奨)\n- [ ] 現状維持\n\n";

fn plain() -> Value {
    json!({"needs_human": null})
}

fn implemented() -> Value {
    json!({"needs_human": null, "verification": []})
}

/// plan(root) の2つのタスクを、1つ目は修正を挟んで2回のレビューで、2つ目は1回のレビューで終え、
/// 処理の流れを書かない作業報告書を書くセッションの作業。
fn whole_plan(root: &Path) -> Vec<Work<'_>> {
    vec![
        Box::new(move |wiki: &Wiki| {
            commit(root, "state.txt");
            wiki.check("1-1");
            Some(json!({"needs_human": null, "verification": [{
                "kind": "process",
                "title": "statusで状態を表示できる",
                "target": "korylusfactory status",
                "steps": ["korylusfactory status を実行する"],
                "expected": "進行中の計画書が表示される",
            }]}))
        }),
        Box::new(move |_: &Wiki| {
            review(root, "demo-1-1", 1, "Request Changes", AUTO_FIX);
            Some(plain())
        }),
        Box::new(move |_: &Wiki| {
            commit(root, "rename.txt");
            Some(plain())
        }),
        Box::new(move |_: &Wiki| {
            review(root, "demo-1-1", 2, "Approve", "");
            Some(plain())
        }),
        Box::new(move |wiki: &Wiki| {
            commit(root, "status.txt");
            wiki.check("1-2");
            Some(implemented())
        }),
        Box::new(move |_: &Wiki| {
            review(root, "demo-1-2", 1, "Approve", "");
            Some(plain())
        }),
        Box::new(|_: &Wiki| {
            Some(json!({
                "needs_human": null,
                "location": "https://wikino.app/s/shimbaco/pages/30",
                "walkthrough_skipped": "テストの計画書で、追う処理が無いため",
            }))
        }),
    ]
}

/// プロジェクトのルートrootに残した、セッションの記録のディレクトリの名前を並べて返す。
///
/// どの記録にも、セッションへの指示を残していることを確かめる。
fn session_logs(root: &Path) -> Vec<String> {
    let dir = plan_dir(root).path().join("sessions");
    let mut logs: Vec<String> = fs::read_dir(&dir)
        .expect("セッションの記録の置き場を読めなかった")
        .map(|entry| {
            entry
                .expect("セッションの記録を読めなかった")
                .file_name()
                .to_string_lossy()
                .into_owned()
        })
        .collect();
    logs.sort();
    for name in &logs {
        assert!(
            dir.join(name).join("prompt.md").is_file(),
            "{name} に、セッションへの指示を残していない"
        );
    }
    logs
}

/// 指示promptに、手順のコマンドcommandがあることを確かめる。
fn assert_command(prompt: &str, command: &str) {
    assert!(
        prompt.contains(command),
        "指示に {command} がない:\n{prompt}"
    );
}

#[test]
fn ロードマップの作業計画書をタスクの実装とレビューから作業報告書まで進めて確認を待つ() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    let sessions = Sessions::new(&wiki, whole_plan(root));
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    let out = run_loop(root, &wiki, &sessions, &clock);

    let state = load(root);
    let wait = state.wait.clone().expect("人間を待っていない");
    assert_eq!(wait.kind, WaitKind::Report, "人間を待つ場面が違う: {out}");
    assert!(
        !state.session_started,
        "完了したセッションを実行中のまま残した"
    );
    assert_eq!(
        state.title, TITLE,
        "ロードマップのリンクのタイトルを残していない"
    );
    let bases: Vec<(&str, &str)> = state
        .tasks
        .iter()
        .map(|record| (record.branch.as_str(), record.base.as_str()))
        .collect();
    assert_eq!(
        bases,
        [("demo-1-1", "main"), ("demo-1-2", "demo-1-1")],
        "2つ目のタスクのブランチを、前のタスクのブランチから作っていない"
    );
    let verdicts: Vec<Verdict> = state.tasks[0]
        .review
        .reviews
        .iter()
        .map(|review| review.verdict)
        .collect();
    assert_eq!(verdicts, [Verdict::RequestChanges, Verdict::Approve]);
    assert_eq!(
        state.tasks[0].verification.len(),
        1,
        "動作確認の項目を記録していない"
    );
    assert_eq!(
        state.latest_report(),
        Some("https://wikino.app/s/shimbaco/pages/30")
    );

    let prompts = sessions.prompts();
    assert_eq!(prompts.len(), 7, "起動したセッションの数が違う");
    let repo_arg = format!("repo={}", root.display());
    assert_command(
        &prompts[0],
        &format!("`/korylus-do-plan-task {PLAN_URL} 1-1 {repo_arg} base=main`"),
    );
    assert_command(&prompts[1], &format!("`/korylus-review main {PLAN_URL}`"));
    assert_command(
        &prompts[2],
        "レビュードキュメント docs/private/reviews/demo-1-1-001.md",
    );
    assert_command(&prompts[6], &format!("`/korylus-write-report {PLAN_URL}`"));
    assert_eq!(
        session_logs(root),
        [
            "20261116T010000Z-1-1-fix",
            "20261116T010000Z-1-1-implement",
            "20261116T010000Z-1-1-review-1",
            "20261116T010000Z-1-1-review-2",
            "20261116T010000Z-1-2-implement",
            "20261116T010000Z-1-2-review-1",
            "20261116T010000Z-report",
        ]
    );
    assert_eq!(
        clock.sleeps(),
        [Timing::default().state],
        "作業報告書の確認を、状態ファイルを読み直す間隔で待っていない"
    );
    for line in [
        "作業計画書 demo を始めます: https://wikino.app/s/shimbaco/pages/20\n",
        &format!(
            "demo: タスク 1-1 のブランチ demo-1-1 を {} に準備しました (元のブランチ: main)\n",
            root.display()
        ),
        "demo: タスク 1-1 の実装を記録しました\n",
        "demo: タスク 1-1 の2回目のレビューを記録しました\n",
        "demo: 人間を待っています (作業報告書の確認): 作業報告書の確認を待っています\n",
    ] {
        assert!(out.contains(line), "{line:?} を表示していない:\n{out}");
    }
}

#[test]
fn 完了の条件を満たさなければ理由を添えて間隔を空けてやり直し_上限を超えたら人間を待つ() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    // コミットもチェックもせずに、結果だけを返す。
    let works: Vec<Work<'_>> = (0..=RETRY_LIMIT)
        .map(|_| Box::new(|_: &Wiki| Some(implemented())) as Work<'_>)
        .collect();
    let sessions = Sessions::new(&wiki, works);
    let cancel = AtomicBool::new(false);
    let timing = Timing::default();
    let clock = Clock::new(4, &cancel);

    let (out, err) = loop_outputs(root, &wiki, &sessions, &clock, &build());

    let state = load(root);
    let wait = state.wait.clone().expect("人間を待っていない");
    assert_eq!(
        wait.kind,
        WaitKind::RetryLimit,
        "人間を待つ場面が違う: {out}"
    );
    assert_eq!(
        state.step,
        Some(Step::Implement),
        "同じステップのまま待っていない"
    );
    assert_eq!(
        clock.sleeps(),
        [timing.retry, timing.retry, timing.retry, timing.state],
        "やり直しの間隔を空けていない"
    );
    let prompts = sessions.prompts();
    assert_eq!(prompts.len(), 4, "上限を超えてやり直した");
    assert!(
        !prompts[0].contains("## やり直し"),
        "初めてのセッションにやり直しを伝えた"
    );
    assert!(
        prompts[1].contains("## やり直し")
            && prompts[1]
                .contains("ブランチ demo-1-1 に、ベース main より先のコミットがありません"),
        "満たせなかった条件をやり直しのセッションに伝えていない: {}",
        prompts[1]
    );
    assert!(
        err.contains(
            "demo: タスク 1-1 の実装を終えられなかったため、5分後にやり直します (1/3回目)\n"
        ),
        "やり直すことを標準エラーへ書いていない:\n{err}"
    );
    assert!(
        !out.contains("を終えられなかったため"),
        "標準出力にやり直しの理由が混ざった:\n{out}"
    );
}

#[test]
fn セッションの記録を作れなければ開始を残さず_次の回を中断として伝えない() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    // セッションの記録の置き場を同じ名前のファイルでふさぎ、セッションを起動する前に失敗させる。
    let blocker = plan_dir(root).path().join("sessions");
    fs::create_dir_all(plan_dir(root).path()).expect("状態の置き場を作れなかった");
    fs::write(&blocker, "").expect("記録の置き場をふさげなかった");
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    let (_, err) = loop_outputs(root, &wiki, &sessions, &clock, &build());

    assert!(
        err.contains("セッションの記録 "),
        "記録を作れないことを伝えていない:\n{err}"
    );
    assert!(sessions.prompts().is_empty(), "セッションを起動した");
    let state = load(root);
    assert_eq!(
        state.step,
        Some(Step::Implement),
        "実装のステップに進んでいない"
    );
    assert!(
        !state.session_started,
        "起動していないセッションの開始を残した"
    );

    fs::remove_file(&blocker).expect("記録の置き場を戻せなかった");
    let sessions = Sessions::new(&wiki, vec![Box::new(|_: &Wiki| None)]);
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    run_loop(root, &wiki, &sessions, &clock);

    let prompts = sessions.prompts();
    assert_eq!(prompts.len(), 1, "セッションを1回だけ起動していない");
    assert!(
        !prompts[0].contains("## やり直し"),
        "起動していないセッションを中断として伝えた:\n{}",
        prompts[0]
    );
}

#[test]
fn セッションが結果を返さずに終わったら途中で止まったものとしてやり直す() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    let sessions = Sessions::new(&wiki, vec![Box::new(|_: &Wiki| None)]);
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    run_loop(root, &wiki, &sessions, &clock);

    let state = load(root);
    assert_eq!(state.retries, 1, "やり直しの回数を数えていない");
    assert_eq!(state.retry_reasons.len(), 1, "止まった理由を残していない");
    assert!(
        state.retry_reasons[0].starts_with("セッションが終了コード 1 で終わりました。記録: "),
        "止まった理由が違う: {:?}",
        state.retry_reasons
    );
}

#[test]
fn セッションが人間の判断を求めたら同じステップのまま質問の人間待ちにする() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    let sessions = Sessions::new(
        &wiki,
        vec![Box::new(|_: &Wiki| {
            Some(json!({"needs_human": "設計を変えてよいか", "verification": []}))
        })],
    );
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(2, &cancel);

    let out = run_loop(root, &wiki, &sessions, &clock);

    let state = load(root);
    let wait = state.wait.clone().expect("人間を待っていない");
    assert_eq!(wait.kind, WaitKind::Question, "人間を待つ場面が違う");
    assert_eq!(wait.details[0], "設計を変えてよいか");
    assert_eq!(
        state.step,
        Some(Step::Implement),
        "同じステップのまま待っていない"
    );
    assert_eq!(
        out.matches("demo: 人間を待っています (セッションからの質問): 実装のセッションが、人間の判断を求めて終わりました\n  設計を変えてよいか\n").count(),
        1,
        "待っている理由を1回だけ表示していない:\n{out}"
    );
    assert!(
        out.contains("対応したら korylusfactory resume を実行してください"),
        "再開のしかたを表示していない:\n{out}"
    );
}

#[test]
fn 取り消したらセッションの結果を記録せずに終える() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    let cancel = AtomicBool::new(false);
    let sessions = Sessions::new(
        &wiki,
        vec![Box::new(|_: &Wiki| {
            cancel.store(true, Ordering::SeqCst);
            Some(implemented())
        })],
    );
    let clock = Clock::new(usize::MAX, &cancel);

    run_loop(root, &wiki, &sessions, &clock);

    let state = load(root);
    assert_eq!(state.step, Some(Step::Implement), "ステップが変わった");
    assert_eq!(state.retries, 0, "やり直しの回数に数えた");
    assert_eq!(clock.sleeps(), [], "取り消したのに待った");
}

#[test]
fn 作業報告書の確認を待つ間にロードマップで完了が伝えられたら完了を記録して次を待つ() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    wiki.set(1214, roadmap("", &format!("- [[{TITLE}]]")));
    let mut state = PlanState::new(PLAN_URL, TITLE);
    state.wait = Some(Wait {
        kind: WaitKind::Report,
        reason: "作業報告書の確認を待っています".to_owned(),
        details: Vec::new(),
    });
    plan_dir(root)
        .lock()
        .expect("ロックを取れなかった")
        .save(&state)
        .expect("状態を保存できなかった");
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    let out = run_loop(root, &wiki, &sessions, &clock);

    assert_eq!(
        load(root).completed_at,
        Some(
            clock
                .now
                .get()
                .checked_sub(SignedDuration::from_mins(5))
                .expect("日時を戻せなかった")
        ),
        "完了を記録していない:\n{out}"
    );
    assert_eq!(clock.sleeps(), [Timing::default().roadmap]);
    for line in [
        "作業計画書 demo の完了を記録しました\n",
        "進める作業計画書がありません。ロードマップの承認の節に作業計画書が現れるのを待ちます\n",
    ] {
        assert!(out.contains(line), "{line:?} を表示していない:\n{out}");
    }
}

#[test]
fn 作業報告書の確認を待つ間に完了を確かめられなければ標準エラーへ一度だけ書く() {
    /// ロードマップの読み取りだけを失敗させ、ほかの呼び出しはwikiに任せるRunner。読み取りを試みた回数を数える。
    struct RoadmapDown<'a> {
        wiki: &'a Wiki,
        reads: Cell<usize>,
    }
    impl Runner for RoadmapDown<'_> {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            if program == "wikino" && args.get(2).is_some_and(|arg| arg == "1214") {
                self.reads.set(self.reads.get().saturating_add(1));
                return Ok(Output {
                    code: 1,
                    ..Output::default()
                });
            }
            self.wiki.run(program, args)
        }
    }

    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    let mut state = PlanState::new(PLAN_URL, TITLE);
    state.wait = Some(Wait {
        kind: WaitKind::Report,
        reason: "作業報告書の確認を待っています".to_owned(),
        details: Vec::new(),
    });
    plan_dir(root)
        .lock()
        .expect("ロックを取れなかった")
        .save(&state)
        .expect("状態を保存できなかった");
    let runner = RoadmapDown {
        wiki: &wiki,
        reads: Cell::new(0),
    };
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let timing = Timing::default();
    // 状態を読み直す間隔で待ち続け、ロードマップを読み直す間隔が過ぎた次の回まで動かす。
    let checks = timing.roadmap.as_secs() / timing.state.as_secs();
    let clock = Clock::new(
        usize::try_from(checks).expect("回数が大きすぎる") + 1,
        &cancel,
    );

    let (out, err) = loop_outputs(root, &runner, &sessions, &clock, &build());

    assert_eq!(runner.reads.get(), 2, "完了を2回確かめていない");
    assert_eq!(
        err.matches("demo: 完了を確かめられません: ").count(),
        1,
        "完了を確かめられないことを標準エラーへ1回だけ書いていない:\n{err}"
    );
    assert!(
        !out.contains("完了を確かめられません"),
        "標準出力にエラーが混ざった:\n{out}"
    );
    assert!(
        out.contains("demo: 人間を待っています (作業報告書の確認): 作業報告書の確認を待っています"),
        "待っている理由を表示していない:\n{out}"
    );
}

#[test]
fn 並びの先頭の作業計画書を読み取れなければ飛ばさずに待ち_理由を繰り返し表示しない() {
    let repo = project();
    let root = repo.root.as_path();
    let wiki = wiki(root);
    wiki.set(
        1214,
        roadmap(&format!("- [[まだ無い 作業計画書]]\n- [[{TITLE}]]"), ""),
    );
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(2, &cancel);

    let out = run_loop(root, &wiki, &sessions, &clock);

    assert_eq!(
        out.matches("ロードマップの「やってる」の節の先頭の作業計画書 [[まだ無い 作業計画書]] は、下書きもページも見つかりません。飛ばさずに、直されるのを待ちます\n").count(),
        1,
        "待っている理由を1回だけ表示していない:\n{out}"
    );
    assert_eq!(
        clock.sleeps(),
        [Timing::default().roadmap, Timing::default().roadmap]
    );
    assert!(
        plan_dir(root).load().expect("状態を読めなかった").is_none(),
        "読み取れない計画書を飛ばして、次の計画書を始めた"
    );
}

#[test]
fn 未コミットの変更があればブランチを準備せずに人間を待つ() {
    let repo = project();
    let root = repo.root.as_path();
    write_file(root, "memo.txt", b"memo\n");
    let wiki = wiki(root);
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    run_loop(root, &wiki, &sessions, &clock);

    let wait = load(root).wait.expect("人間を待っていない");
    assert_eq!(wait.kind, WaitKind::Branch, "人間を待つ場面が違う");
    assert_eq!(
        wait.reason,
        format!(
            "{} に未コミットの変更があるため、ブランチ demo-1-1 を準備できません",
            root.display()
        )
    );
    assert_eq!(wait.details, ["?? memo.txt"]);
}

#[test]
fn 同じプロジェクトでは二重に起動できない() {
    let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
    let _first = hold(dir.path()).expect("ロックを取れなかった");
    let err = hold(dir.path()).expect_err("二重にロックを取れてしまった");
    assert!(matches!(err, Error::Running(_)), "エラー = {err:?}");
}

#[test]
fn 読み取り失敗は標準エラーへ一度だけ書き_進捗へ混ぜない() {
    struct Failed;
    impl Runner for Failed {
        fn run(&self, program: &str, args: &[OsString]) -> Result<Output, process::Error> {
            if program == "git" {
                return System.run(program, args);
            }
            Ok(Output {
                code: 1,
                ..Output::default()
            })
        }
    }
    let repo = project();
    let wiki = wiki(&repo.root);
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(2, &cancel);
    let (out, err) = loop_outputs(&repo.root, &Failed, &sessions, &clock, &build());

    assert!(
        out.contains("ロードマップで承認された作業計画書を進めます"),
        "進捗が無い: {out}"
    );
    assert!(
        !out.contains("失敗しました"),
        "標準出力にエラーが混ざった: {out}"
    );
    assert_eq!(
        err.matches("wikino draft get が終了コード 1 で失敗しました")
            .count(),
        1,
        "読み取り失敗を1回だけ標準エラーへ書いていない: {err}"
    );
    assert!(
        err.contains("5分後に読み直します"),
        "読み直す間隔が無い: {err}"
    );
}

#[test]
fn 古いビルドの警告は標準エラーへ書き_進捗へ混ぜない() {
    let repo = project();
    let mut build = build();
    build.source_dir = repo.root.clone();
    build.commit =
        branch::head(&System, &repo.root, "main").expect("ビルドのコミットを読めなかった");
    commit(&repo.root, "crates/demo.rs");
    let wiki = wiki(&repo.root);
    wiki.set(1214, roadmap("", ""));
    let sessions = Sessions::new(&wiki, Vec::new());
    let cancel = AtomicBool::new(false);
    let clock = Clock::new(1, &cancel);

    let (out, err) = loop_outputs(&repo.root, &wiki, &sessions, &clock, &build);

    assert!(!out.contains("警告:"), "標準出力に警告が混ざった: {out}");
    assert!(
        out.contains("進める作業計画書がありません"),
        "進捗が無い: {out}"
    );
    assert!(
        err.contains("警告: このkorylusfactoryは"),
        "古いビルドの警告が無い: {err}"
    );
    assert!(
        err.contains("install-korylusfactory で入れ直してください"),
        "入れ直す方法が無い: {err}"
    );
}
