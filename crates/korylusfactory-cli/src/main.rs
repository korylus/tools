use std::env;
use std::error::Error;
use std::io::{self, IsTerminal as _, Write};
use std::process::ExitCode;
use std::sync::atomic::AtomicBool;

use clap::{Parser, Subcommand};
use jiff::tz::TimeZone;
use korylusfactory_cli::human_queue::{WikinoStore, command as queue};
use korylusfactory_cli::process::System;
use korylusfactory_cli::start::{self, Start, SystemClock, Timing};
use korylusfactory_cli::version::{self, Build};
use korylusfactory_cli::{completion, config, describe, resume, roadmap, status};
use korylusfactory_core::state::{Store, Wait, WaitKind};

/// バージョン番号に、ビルド元のコミットとビルドした日を添えた文字列。
macro_rules! version {
    () => {
        concat!(
            env!("CARGO_PKG_VERSION"),
            " (コミット ",
            env!("KORYLUSFACTORY_COMMIT_SHORT"),
            "、",
            env!("KORYLUSFACTORY_BUILD_DATE"),
            "にビルド)"
        )
    };
}

/// 作業計画書を、人が付き添わずに進めるためのコマンド。
#[derive(Parser)]
#[command(
    name = "korylusfactory",
    version = version!(),
    // 入れ直すときに使う、ビルド元のリポジトリの場所を `--version` に添える。
    long_version = concat!(version!(), "\nビルド元のリポジトリ: ", env!("KORYLUSFACTORY_SOURCE_DIR")),
    arg_required_else_help = true
)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// ロードマップで承認された作業計画書を1本ずつ、止めるまで進め続ける。プロジェクトのルートで実行する
    Start,
    /// korylusfactoryのバージョン、これから進める作業計画書の並び、進行中の作業計画書を表示する。プロジェクトのルートで実行する
    Status {
        /// JSONで出す
        #[arg(long)]
        json: bool,
    },
    /// 人間を待っている作業計画書を、待っていた場面に応じて再開する。プロジェクトのルートで実行する
    Resume {
        /// レビューの上限で待っているときに、上限をこの回数に延ばして再開する
        #[arg(long)]
        review_limit: Option<u32>,
    },
    /// 人間キューの項目の一覧と内容を表示し、回答を項目の下書きに書く。プロジェクトのルートで実行する
    Queue {
        #[command(subcommand)]
        command: QueueCommand,
    },
}

#[derive(Subcommand)]
enum QueueCommand {
    /// 人間待ちの項目の一覧を、積んだ順に表示する。既定はこのプロジェクトの項目
    List {
        /// 全プロジェクトの項目を表示する
        #[arg(long)]
        all: bool,
        /// JSONで出す
        #[arg(long)]
        json: bool,
    },
    /// 項目の内容と、回答の欄を表示する
    Show {
        /// 項目のページ番号
        number: u64,
        /// JSONで出す
        #[arg(long)]
        json: bool,
    },
    /// 回答の形を確かめて、書き手と時刻を記して項目の下書きに書く。公開はしない
    Answer {
        /// 項目のページ番号
        number: u64,
        /// 欄の順に並べた回答。選択の欄にはラベル (A など)、自由記述の欄には回答の文を渡す。`-` で始まる文は `--` のあとに渡す
        responses: Vec<String>,
        /// JSONで出す
        #[arg(long)]
        json: bool,
    },
}

fn main() -> ExitCode {
    let mut args = env::args_os().skip(1);
    if args.next().as_deref()
        == Some(std::ffi::OsStr::new(
            korylusfactory_cli::process::SUPERVISOR_COMMAND,
        ))
    {
        return match args.next() {
            Some(dir) => match korylusfactory_cli::process::supervise(std::path::Path::new(&dir)) {
                Ok(()) => ExitCode::SUCCESS,
                Err(err) => {
                    eprintln!("korylusfactory: {}", describe(&err));
                    ExitCode::FAILURE
                }
            },
            None => ExitCode::FAILURE,
        };
    }
    let cli = Cli::parse();
    let result = match cli.command {
        Command::Start => run_start(),
        Command::Status { json } => run_status(json),
        Command::Resume { review_limit } => run_resume(review_limit),
        Command::Queue { command } => run_queue(command),
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("korylusfactory: {}", describe(err.as_ref()));
            ExitCode::FAILURE
        }
    }
}

/// `korylusfactory start` を、今のディレクトリをプロジェクトのルートとして実行する。
///
/// 止めるときはシグナルの既定の動作で終わり、実行中のセッションはセッションの監督プロセスが終わらせる。
fn run_start() -> Result<(), Box<dyn Error>> {
    let root = env::current_dir()?;
    let build = Build::current();
    let start = Start {
        runner: &System,
        launcher: &System,
        clock: &SystemClock,
        queues: &System,
        notifier: &System,
        terminal: io::stderr().is_terminal(),
        // 項目の日時は、実行している環境のタイムゾーン (環境変数TZか、システムの設定) で書く。
        time_zone: &TimeZone::system(),
        build: &build,
        root: &root,
        timing: Timing::default(),
    };
    start::run(
        &start,
        &mut io::stdout().lock(),
        &mut io::stderr().lock(),
        &AtomicBool::new(false),
    )?;
    Ok(())
}

/// `korylusfactory status` を、今のディレクトリをプロジェクトのルートとして実行する。
fn run_status(json: bool) -> Result<(), Box<dyn Error>> {
    let runner = System;
    let build = Build::current();
    let freshness = version::check(&runner, &build);
    let mut stdout = io::stdout().lock();
    // ロードマップを読めないときも、古いかどうかは先に見せる。
    if !json {
        stdout.write_all(status::version_text(&build, &freshness).as_bytes())?;
        stdout.flush()?;
    }
    let root = env::current_dir()?;
    let config = config::load(&root)?;
    let store = Store::new(&root);
    let entries = roadmap::load(&runner, &config, &store)?;
    let progress: Vec<status::Progress> = store
        .active()?
        .into_iter()
        .map(|(name, state)| {
            // 完了を確かめられないときも、ほかの表示は続けられるよう、理由を表示に残す。
            let completion = matches!(
                state.wait,
                Some(Wait {
                    kind: WaitKind::Report,
                    ..
                })
            )
            .then(|| completion::check(&runner, &config, &state).map_err(|err| describe(&err)));
            status::Progress {
                name,
                state,
                completion,
            }
        })
        .collect();
    if json {
        serde_json::to_writer_pretty(
            &mut stdout,
            &status::Report::new(&build, &freshness, &entries, &progress),
        )?;
        stdout.write_all(b"\n")?;
    } else {
        stdout.write_all(b"\n")?;
        stdout.write_all(status::plans_text(&entries).as_bytes())?;
        stdout.write_all(b"\n")?;
        stdout.write_all(status::progress_text(&progress).as_bytes())?;
    }
    Ok(())
}

/// `korylusfactory resume` を、今のディレクトリをプロジェクトのルートとして実行する。
fn run_resume(review_limit: Option<u32>) -> Result<(), Box<dyn Error>> {
    let resumed = resume::run(&System, &env::current_dir()?, review_limit)?;
    println!(
        "作業計画書 {} を、{}の人間待ちから再開しました",
        resumed.name,
        resumed.kind.label()
    );
    Ok(())
}

/// `korylusfactory queue` を、今のディレクトリをプロジェクトのルートとして実行する。
fn run_queue(command: QueueCommand) -> Result<(), Box<dyn Error>> {
    let root = env::current_dir()?;
    let config = config::load(&root)?;
    let (project, location) = config
        .project
        .and_then(|project| Some((project.name, project.queue?)))
        .ok_or("設定ファイルに「project」と「queue」がありません。人間キューの置き場を「queue」に書いてください")?;
    let store = WikinoStore::new(&System, &location);
    let mut stdout = io::stdout().lock();
    match command {
        QueueCommand::List { all, json } => {
            let entries = queue::list(&store, &location, &project, all)?;
            if json {
                serde_json::to_writer_pretty(&mut stdout, &entries)?;
                stdout.write_all(b"\n")?;
            } else {
                stdout.write_all(queue::list_text(&entries).as_bytes())?;
            }
        }
        QueueCommand::Show { number, json } => {
            let shown = queue::show(&store, &location, number)?;
            if json {
                serde_json::to_writer_pretty(&mut stdout, &shown)?;
                stdout.write_all(b"\n")?;
            } else {
                stdout.write_all(queue::show_text(&shown).as_bytes())?;
            }
        }
        QueueCommand::Answer {
            number,
            responses,
            json,
        } => {
            let writer = queue::writer(|name| env::var_os(name));
            // 書き手の日時は、実行している環境のタイムゾーンで書く。
            let at = jiff::Zoned::now().datetime();
            let answered = queue::answer(&store, &location, number, &responses, writer, at)?;
            if json {
                serde_json::to_writer_pretty(&mut stdout, &answered)?;
                stdout.write_all(b"\n")?;
            } else {
                stdout.write_all(queue::answered_text(&answered).as_bytes())?;
            }
        }
    }
    Ok(())
}
