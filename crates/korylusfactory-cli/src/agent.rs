//! 作業者とレビュアーのセッションを確認なしで起動し、結果のJSONを受け取る。
//!
//! セッションはVMの中で確認なしに動かし、VMを隔離の境界にする。
//! pushとPRの作成・マージは、指示で禁じるのに加えて、次で止める。
//!
//! - Claude Codeの拒否ルール (`--disallowedTools`)
//! - セッションのPATHに置くGitの入口。pushとsend-packを拒否する
//! - 補助の環境変数。pushの宛先を書き換えるgitの設定と、無効な `GH_TOKEN`
//!
//! Gitの入口は、`git -C <dir> push` のグローバルオプションも読み取り、pushを拒否する。
//! セッションの起動は [`process::Launcher`] に任せる。

mod guard;

use std::env;
use std::ffi::OsString;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::AtomicBool;
use std::time::Duration;

use korylusfactory_core::state::VerificationItem;
use serde::Deserialize;

use crate::instruction::{Agent, Request};
use crate::process::{self, Ended, Launcher, Session};

/// セッションの時間の上限。
///
/// korylusflowで記録したステップの所要時間 (実装の95%が42分以内、レビューは最長54分) に余裕を持たせた。
pub const DEFAULT_TIMEOUT: Duration = Duration::from_hours(2);

/// Claude Codeで拒否するコマンド。
const CLAUDE_DENIED: [&str; 3] = [
    "Bash(git push:*)",
    "Bash(gh pr create:*)",
    "Bash(gh pr merge:*)",
];

/// pushの宛先を書き換える先の、存在しないリモートヘルパーの名前。
const PUSH_DISABLED: &str = "korylusfactory-push-disabled";

/// セッションの記録を残すファイルの名前。
const PROMPT_FILE: &str = "prompt.md";
const SCHEMA_FILE: &str = "schema.json";
const STDOUT_FILE: &str = "stdout.log";
const STDERR_FILE: &str = "stderr.log";
/// Codexが最後のメッセージ (結果のJSON) を書くファイルの名前。
const RESULT_FILE: &str = "result.json";

/// セッションを起動できないときのエラー。
#[derive(Debug, thiserror::Error)]
pub enum Error {
    /// セッションの記録を残すディレクトリかファイルを書けない。
    #[error("セッションの記録 {} を書けません", .path.display())]
    Logs {
        /// 書こうとしたパス。
        path: PathBuf,
        /// 書けなかった理由。
        #[source]
        source: io::Error,
    },

    /// pushを止めるGitの入口を用意できない。本物のgitがPATHに無いときも含む。
    #[error("pushを止めるGitの入口を用意できません")]
    Guard(#[source] io::Error),

    /// セッションを起動できない。
    #[error(transparent)]
    Launch(#[from] process::Error),
}

/// セッションの結果。
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    /// 決めた形の結果を返して終わった。
    Done(Response),
    /// 人間の判断が要ると返して終わった。
    NeedsHuman(String),
    /// 結果を返さずに終わったか、時間の上限を過ぎた。同じステップをやり直す。
    Stopped(String),
    /// 取り消された。
    Cancelled,
}

/// セッションが返した結果のうち、ステップによって返すもの。返さないステップでは `None`。
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct Response {
    /// 動作確認の項目。
    #[serde(default)]
    pub verification: Option<Vec<VerificationItem>>,
    /// 作業報告書のページのURL。
    #[serde(default)]
    pub location: Option<String>,
    /// 作業報告書に処理の流れを書かなかった理由。
    #[serde(default)]
    pub walkthrough_skipped: Option<String>,
}

/// セッションが返す結果のJSONの形。
#[derive(Deserialize)]
struct Raw {
    needs_human: Option<String>,
    #[serde(flatten)]
    response: Response,
}

/// Claude Codeの `--output-format json` の出力のうち、読む項目。
#[derive(Deserialize)]
struct ClaudeOutput {
    #[serde(default)]
    is_error: bool,
    #[serde(default)]
    result: Option<String>,
    #[serde(default)]
    structured_output: Option<serde_json::Value>,
}

/// プロジェクトのルートrootで、頼むことrequestのセッションを起動し、結果を受け取る。
///
/// 指示・結果の形・標準出力・標準エラーは、ディレクトリlogsに残す。
/// timeoutを過ぎるか、cancelが真になったら、セッションを終わらせる。
pub fn run(
    launcher: &dyn Launcher,
    request: &Request,
    root: &Path,
    logs: &Path,
    timeout: Duration,
    cancel: &AtomicBool,
) -> Result<Outcome, Error> {
    let write = |name: &str, content: &[u8]| {
        let path = logs.join(name);
        fs::write(&path, content).map_err(|source| Error::Logs { path, source })
    };
    fs::create_dir_all(logs).map_err(|source| Error::Logs {
        path: logs.to_path_buf(),
        source,
    })?;
    write(PROMPT_FILE, request.prompt.as_bytes())?;
    write(SCHEMA_FILE, request.schema.to_string().as_bytes())?;

    let session = Session {
        program: program(request.agent).to_owned(),
        args: args(request, logs),
        env: guard::environment(logs, &env::var_os("PATH").unwrap_or_default())?,
        dir: root.to_path_buf(),
        input: request.prompt.as_bytes().to_vec(),
        stdout: logs.join(STDOUT_FILE),
        stderr: logs.join(STDERR_FILE),
        timeout,
        exclusive: None,
    };
    let reason = match launcher.launch(&session, cancel)? {
        Ended::Exited(0) => return Ok(read_result(request.agent, logs)),
        Ended::Exited(code) => format!("セッションが終了コード {code} で終わりました"),
        Ended::Signaled => "セッションがシグナルで終わりました".to_owned(),
        Ended::TimedOut => format!(
            "セッションが時間の上限 ({}分) を過ぎても終わりませんでした",
            timeout.as_secs() / 60
        ),
        Ended::Cancelled => return Ok(Outcome::Cancelled),
    };
    Ok(Outcome::Stopped(format!(
        "{reason}。記録: {}",
        logs.display()
    )))
}

/// エージェントagentのコマンドの名前を返す。
fn program(agent: Agent) -> &'static str {
    match agent {
        Agent::Claude => "claude",
        Agent::Codex => "codex",
    }
}

/// 頼むことrequestのセッションの引数を返す。指示は標準入力で渡す。
fn args(request: &Request, logs: &Path) -> Vec<OsString> {
    let mut args: Vec<OsString> = Vec::new();
    match request.agent {
        Agent::Claude => {
            let schema = request.schema.to_string();
            args.extend(
                [
                    "-p",
                    "--output-format",
                    "json",
                    "--json-schema",
                    schema.as_str(),
                    "--dangerously-skip-permissions",
                    "--disallowedTools",
                ]
                .map(OsString::from),
            );
            args.extend(CLAUDE_DENIED.map(OsString::from));
        }
        Agent::Codex => {
            args.extend(
                [
                    "exec",
                    "--dangerously-bypass-approvals-and-sandbox",
                    "--color",
                    "never",
                    "--json",
                    "--output-schema",
                ]
                .map(OsString::from),
            );
            args.push(logs.join(SCHEMA_FILE).into());
            args.push("-o".into());
            args.push(logs.join(RESULT_FILE).into());
            args.push("-".into());
        }
    }
    args
}

/// pushとPRの作成・マージを止めるために、セッションに渡す環境変数を返す。
fn guard_env() -> Vec<(OsString, OsString)> {
    guard_env_after(env::var("GIT_CONFIG_COUNT").ok().as_deref())
}

/// [`guard_env`] の環境変数を返す。gitの設定は、利用者が `GIT_CONFIG_COUNT` (値はcount) で渡している設定のあとに足す。
fn guard_env_after(count: Option<&str>) -> Vec<(OsString, OsString)> {
    let count: usize = count.and_then(|count| count.parse().ok()).unwrap_or(0);
    vec![
        (
            format!("GIT_CONFIG_KEY_{count}").into(),
            format!("url.{PUSH_DISABLED}::.pushInsteadOf").into(),
        ),
        (format!("GIT_CONFIG_VALUE_{count}").into(), OsString::new()),
        ("GIT_CONFIG_COUNT".into(), (count + 1).to_string().into()),
        ("GH_TOKEN".into(), PUSH_DISABLED.into()),
    ]
}

/// 終了コード0で終わったセッションの結果を、エージェントagentの出力から読む。
fn read_result(agent: Agent, logs: &Path) -> Outcome {
    let raw = match agent {
        Agent::Claude => read_claude(logs),
        Agent::Codex => read(&logs.join(RESULT_FILE))
            .and_then(|src| serde_json::from_str::<Raw>(&src).map_err(|err| err.to_string())),
    };
    match raw {
        Ok(Raw {
            needs_human: Some(reason),
            ..
        }) if !reason.trim().is_empty() => Outcome::NeedsHuman(reason),
        Ok(raw) => Outcome::Done(raw.response),
        Err(reason) => Outcome::Stopped(format!(
            "セッションの結果を読み取れません: {reason}。記録: {}",
            logs.display()
        )),
    }
}

/// Claude Codeの出力から、結果のJSONを読む。
fn read_claude(logs: &Path) -> Result<Raw, String> {
    let src = read(&logs.join(STDOUT_FILE))?;
    let output: ClaudeOutput = serde_json::from_str(&src).map_err(|err| err.to_string())?;
    if output.is_error {
        return Err(format!(
            "Claude Codeがエラーで終わりました: {}",
            output.result.unwrap_or_default()
        ));
    }
    let value = output
        .structured_output
        .ok_or_else(|| "Claude Codeの出力に structured_output がありません".to_owned())?;
    serde_json::from_value(value).map_err(|err| err.to_string())
}

/// ファイルpathを読む。読めなければ理由を返す。
fn read(path: &Path) -> Result<String, String> {
    fs::read_to_string(path).map_err(|err| format!("{} を読めません ({err})", path.display()))
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::process::Command;

    use korylusfactory_core::state::VerificationKind;
    use serde_json::json;

    use super::*;
    use crate::branch::testutil::{git_run, repo_on, temp_dir};

    /// 決めた出力をファイルに書き、決めた終わり方を返すLauncher。起動したセッションを記録する。
    struct Fake {
        /// 標準出力に書く内容。
        stdout: &'static str,
        /// Codexの結果のファイルに書く内容。
        result: Option<&'static str>,
        ended: Ended,
        launched: RefCell<Option<Session>>,
    }

    impl Fake {
        fn new(stdout: &'static str, result: Option<&'static str>, ended: Ended) -> Self {
            Self {
                stdout,
                result,
                ended,
                launched: RefCell::new(None),
            }
        }

        fn session(&self) -> Session {
            self.launched
                .borrow()
                .clone()
                .expect("セッションを起動していない")
        }
    }

    impl Launcher for Fake {
        fn launch(&self, session: &Session, _cancel: &AtomicBool) -> Result<Ended, process::Error> {
            fs::write(&session.stdout, self.stdout).expect("標準出力を書けなかった");
            if let Some(result) = self.result {
                let logs = session.stdout.parent().expect("記録のディレクトリが無い");
                fs::write(logs.join(RESULT_FILE), result).expect("結果を書けなかった");
            }
            *self.launched.borrow_mut() = Some(session.clone());
            Ok(self.ended)
        }
    }

    fn request(agent: Agent) -> Request {
        Request {
            agent,
            prompt: "タスク1-1を実装する".to_owned(),
            schema: json!({"type": "object"}),
        }
    }

    fn run_with(launcher: &Fake, agent: Agent) -> (tempfile::TempDir, Outcome) {
        let dir = tempfile::tempdir().expect("一時ディレクトリを作れなかった");
        let outcome = run(
            launcher,
            &request(agent),
            Path::new("/workspace"),
            &dir.path().join("sessions/1"),
            Duration::from_secs(60),
            &AtomicBool::new(false),
        )
        .expect("セッションを起動できなかった");
        (dir, outcome)
    }

    fn strings(values: &[OsString]) -> Vec<String> {
        values
            .iter()
            .map(|value| value.to_string_lossy().into_owned())
            .collect()
    }

    #[test]
    fn claude_codeは確認なしで拒否ルールと結果の形を渡して起動し_構造化した結果を受け取る() {
        let fake = Fake::new(
            r#"{"is_error": false, "result": "", "structured_output": {"needs_human": null, "verification": [{"kind": "process", "title": "statusを表示できる", "target": "korylusfactory status", "steps": ["実行する"], "expected": "表示される"}]}}"#,
            None,
            Ended::Exited(0),
        );
        let (dir, outcome) = run_with(&fake, Agent::Claude);
        assert_eq!(
            outcome,
            Outcome::Done(Response {
                verification: Some(vec![VerificationItem {
                    kind: VerificationKind::Process,
                    title: "statusを表示できる".to_owned(),
                    target: "korylusfactory status".to_owned(),
                    steps: vec!["実行する".to_owned()],
                    expected: "表示される".to_owned(),
                }]),
                ..Response::default()
            })
        );

        let session = fake.session();
        assert_eq!(session.program, "claude");
        assert_eq!(
            strings(&session.args),
            [
                "-p",
                "--output-format",
                "json",
                "--json-schema",
                r#"{"type":"object"}"#,
                "--dangerously-skip-permissions",
                "--disallowedTools",
                "Bash(git push:*)",
                "Bash(gh pr create:*)",
                "Bash(gh pr merge:*)",
            ]
        );
        assert_eq!(
            session.input,
            "タスク1-1を実装する".as_bytes(),
            "指示を標準入力で渡していない"
        );
        assert_eq!(
            session.dir,
            Path::new("/workspace"),
            "プロジェクトのルートで起動していない"
        );
        assert!(
            session.env.iter().any(|(key, _)| key == "GH_TOKEN"),
            "PRの作成を止める環境変数を渡していない"
        );
        let logs = dir.path().join("sessions/1");
        assert_eq!(
            fs::read_to_string(logs.join(PROMPT_FILE)).expect("指示の記録を読めなかった"),
            "タスク1-1を実装する"
        );
        assert_eq!(
            session.stdout,
            logs.join(STDOUT_FILE),
            "標準出力を記録に残していない"
        );
        assert_eq!(
            session.stderr,
            logs.join(STDERR_FILE),
            "標準エラーを記録に残していない"
        );
    }

    #[test]
    fn codexは確認なしで起動し_結果のファイルから人間の判断が要ることを受け取る() {
        let fake = Fake::new(
            "{}\n",
            Some(r#"{"needs_human": "設計を変える必要がある"}"#),
            Ended::Exited(0),
        );
        let (dir, outcome) = run_with(&fake, Agent::Codex);
        assert_eq!(
            outcome,
            Outcome::NeedsHuman("設計を変える必要がある".to_owned())
        );

        let session = fake.session();
        let logs = dir.path().join("sessions/1");
        assert_eq!(session.program, "codex");
        assert_eq!(
            strings(&session.args),
            [
                "exec".to_owned(),
                "--dangerously-bypass-approvals-and-sandbox".to_owned(),
                "--color".to_owned(),
                "never".to_owned(),
                "--json".to_owned(),
                "--output-schema".to_owned(),
                logs.join(SCHEMA_FILE).display().to_string(),
                "-o".to_owned(),
                logs.join(RESULT_FILE).display().to_string(),
                "-".to_owned(),
            ]
        );
        assert_eq!(
            fs::read_to_string(logs.join(SCHEMA_FILE)).expect("結果の形の記録を読めなかった"),
            r#"{"type":"object"}"#
        );
    }

    #[test]
    fn 結果を読み取れなければ止まったものとして記録の場所を添える() {
        let cases = [
            ("JSONではない", "途中で止まった", Agent::Claude, None),
            (
                "エラーで終わった",
                r#"{"is_error": true, "result": "利用量の上限に達しました"}"#,
                Agent::Claude,
                None,
            ),
            (
                "構造化した結果が無い",
                r#"{"is_error": false, "result": "終わりました"}"#,
                Agent::Claude,
                None,
            ),
            ("結果のファイルが無い", "", Agent::Codex, None),
            (
                "結果の形が違う",
                "",
                Agent::Codex,
                Some(r#"{"needs_human": 1}"#),
            ),
        ];
        for (name, stdout, agent, result) in cases {
            let fake = Fake::new(stdout, result, Ended::Exited(0));
            let (dir, outcome) = run_with(&fake, agent);
            match outcome {
                Outcome::Stopped(reason) => assert!(
                    reason.starts_with("セッションの結果を読み取れません: ")
                        && reason.ends_with(&format!(
                            "記録: {}",
                            dir.path().join("sessions/1").display()
                        )),
                    "{name}: 理由 = {reason}"
                ),
                other => panic!("{name}: 止まったものとしなかった: {other:?}"),
            }
        }
    }

    #[test]
    fn 正常に終わらなかったセッションは止まったか取り消されたものとする() {
        let cases = [
            (
                Ended::Exited(1),
                Some("セッションが終了コード 1 で終わりました。"),
            ),
            (
                Ended::Signaled,
                Some("セッションがシグナルで終わりました。"),
            ),
            (
                Ended::TimedOut,
                Some("セッションが時間の上限 (1分) を過ぎても終わりませんでした。"),
            ),
            (Ended::Cancelled, None),
        ];
        for (ended, want) in cases {
            let fake = Fake::new("", None, ended);
            let (_dir, outcome) = run_with(&fake, Agent::Claude);
            match (want, outcome) {
                (Some(want), Outcome::Stopped(reason)) => {
                    assert!(reason.starts_with(want), "{ended:?}: 理由 = {reason}");
                }
                (None, Outcome::Cancelled) => {}
                (_, other) => panic!("{ended:?}: 結果が期待と異なる: {other:?}"),
            }
        }
    }

    #[test]
    fn 利用者が渡しているgitの設定のあとにpushを止める設定を足す() {
        let env = guard_env_after(Some("2"));
        let values: Vec<(String, String)> = env
            .iter()
            .map(|(key, value)| {
                (
                    key.to_string_lossy().into_owned(),
                    value.to_string_lossy().into_owned(),
                )
            })
            .collect();
        assert_eq!(
            values,
            [
                (
                    "GIT_CONFIG_KEY_2".to_owned(),
                    "url.korylusfactory-push-disabled::.pushInsteadOf".to_owned()
                ),
                ("GIT_CONFIG_VALUE_2".to_owned(), String::new()),
                ("GIT_CONFIG_COUNT".to_owned(), "3".to_owned()),
                (
                    "GH_TOKEN".to_owned(),
                    "korylusfactory-push-disabled".to_owned()
                ),
            ]
        );
    }

    #[test]
    fn pushを止める設定ではどの書き方のpushも失敗し_fetchはできる() {
        let (dir, root) = temp_dir();
        let _dir = dir;
        git_run(&root, &["init", "--quiet", "--bare", "remote.git"]);
        let local = repo_on("main");
        git_run(
            &local.root,
            &[
                "remote",
                "add",
                "origin",
                &root.join("remote.git").display().to_string(),
            ],
        );

        let git = |args: &[&str]| {
            Command::new("git")
                .arg("-C")
                .arg(&local.root)
                .args(args)
                .envs(
                    guard::environment(&root, &inherited_path())
                        .expect("Gitの拒否設定を作れなかった"),
                )
                .output()
                .expect("gitを実行できなかった")
        };
        let remote = root.join("remote.git").display().to_string();
        for args in [
            vec!["push", "origin", "main"],
            vec!["push", "--no-verify", "origin", "main"],
            vec!["push", remote.as_str(), "main"],
        ] {
            let output = git(&args);
            assert!(!output.status.success(), "git {args:?} が成功した");
        }
        let branches = Command::new("git")
            .arg("--git-dir")
            .arg(root.join("remote.git"))
            .args(["branch", "--list"])
            .output()
            .expect("gitを実行できなかった");
        assert!(branches.stdout.is_empty(), "リモートにブランチが増えた");
        assert!(
            git(&["fetch", "--quiet", "origin"]).status.success(),
            "fetchが失敗した"
        );
    }

    #[test]
    fn 明示的なpushurlがあってもpushを拒否し_fetchとremoteの読み取りは保つ() {
        let (guard_dir, root) = temp_dir();
        let _guard_dir = guard_dir;
        git_run(&root, &["init", "--quiet", "--bare", "remote.git"]);
        let remote = root.join("remote.git").display().to_string();
        let local = repo_on("main");
        git_run(&local.root, &["remote", "add", "origin", &remote]);
        git_run(&local.root, &["config", "remote.origin.pushurl", &remote]);
        let variables =
            guard::environment(&root, &inherited_path()).expect("Gitの拒否設定を作れなかった");
        let run = |args: &[&str]| {
            Command::new("git")
                .args(args)
                .current_dir(&local.root)
                .envs(variables.clone())
                .output()
                .expect("gitを起動できなかった")
        };
        let path = local.root.display().to_string();
        for args in [
            vec!["push", "origin", "main"],
            vec!["-C", path.as_str(), "push", "origin", "main"],
            vec![
                "-c",
                "color.ui=false",
                "-C",
                path.as_str(),
                "push",
                "--no-verify",
                "origin",
                "main",
            ],
            vec![
                "--attr-source",
                "HEAD",
                "-C",
                path.as_str(),
                "push",
                "origin",
                "main",
            ],
            vec![
                "--shallow-file",
                "/dev/null",
                "-C",
                path.as_str(),
                "push",
                "origin",
                "main",
            ],
        ] {
            let output = run(&args);
            assert!(!output.status.success(), "git {args:?} が成功した");
            assert!(
                String::from_utf8_lossy(&output.stderr)
                    .contains("セッションからのgit pushは許可されていません"),
                "Gitの入口で拒否していない: {output:?}"
            );
        }
        let login = Command::new("bash")
            .args([
                "-lc",
                "git -C \"$1\" push --no-verify origin main",
                "korylusfactory-test",
            ])
            .arg(&local.root)
            .envs(variables.clone())
            .output()
            .expect("ログインシェルを起動できなかった");
        assert!(!login.status.success(), "ログインシェルからpushできた");
        assert!(
            String::from_utf8_lossy(&login.stderr)
                .contains("セッションからのgit pushは許可されていません"),
            "ログインシェルでGitの入口を通っていない: {login:?}"
        );
        assert!(
            run(&["fetch", "--quiet", "origin"]).status.success(),
            "fetchが失敗した"
        );
        let url = run(&["remote", "get-url", "origin"]);
        assert!(url.status.success(), "remoteを読めなかった");
        assert_eq!(
            String::from_utf8_lossy(&url.stdout).trim(),
            remote,
            "remoteのURLが変わった"
        );
        let refs = Command::new("git")
            .arg("--git-dir")
            .arg(root.join("remote.git"))
            .args(["for-each-ref", "refs/heads"])
            .output()
            .expect("リモートを読めなかった");
        assert!(refs.stdout.is_empty(), "リモートにブランチが増えた");
    }

    #[test]
    fn pathにgitが無ければgitの入口を用意できないエラーを返す() {
        let (dir, root) = temp_dir();
        let _dir = dir;
        let bin = root.join("bin");
        fs::create_dir(&bin).expect("gitの無いディレクトリを作れなかった");
        let err = guard::environment(&root.join("logs"), bin.as_os_str())
            .expect_err("エラーにならなかった");
        assert!(
            matches!(&err, Error::Guard(source) if source.kind() == io::ErrorKind::NotFound),
            "エラー = {err:?}"
        );
        assert_eq!(err.to_string(), "pushを止めるGitの入口を用意できません");
    }

    /// テストを実行しているプロセスのPATH。本物のgitを探す元にする。
    fn inherited_path() -> OsString {
        env::var_os("PATH").unwrap_or_default()
    }
}
