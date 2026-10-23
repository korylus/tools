// Command koryluspilotは作業計画書のタスクを、実装・レビュー・修正のループで
// 自動的に進めるオーケストレーター。
//
//	koryluspilot run <計画書のパス>     計画書の未完了のタスクを先頭から順に自動で進める
//	koryluspilot report <計画書のパス>  作業報告書を配信し、動作確認の結果を保存する
//	koryluspilot status <計画書のパス>  計画書の進行状況と止まった理由を表示する
//	koryluspilot usage                  Claude Code / Codexの使用率と上限に達する見込みを表示する
//
// 各サブコマンドの本体はinternal/pilot/配下のパッケージに置き、本ファイルと
// 同じパッケージのファイルはサブコマンドのディスパッチと入出力・終了コードのみを担う。
package main

import (
	"fmt"
	"io"
	"os"
)

// 終了コード。runサブコマンドが停止条件で止まったときの2 (exitStopped) と
// 区別するため、引数の誤りも1で終える。
const (
	exitOK      = 0
	exitError   = 1
	exitStopped = 2
)

// usageTextは引数不正・-h時に表示する使い方。
const usageText = `koryluspilot - 作業計画書のタスクを自動で進めるオーケストレーター

使い方:
  koryluspilot run <計画書のパス> [-note=<補足>] [-approve] [-config=<設定ファイル>]
  koryluspilot report <計画書のパス> [-addr=<待ち受けるアドレス>]
  koryluspilot status <計画書のパス>
  koryluspilot usage [-config=<設定ファイル>]

サブコマンド:
  run       計画書の未完了のタスクを先頭から順に、実装・コミット・ゲート検査の流れで進め、最後に作業報告書を作る
            作業報告書の動作確認でNGがあれば、フィードバック対応のフェーズを起票して実行し、次の版を作る
            (終了コード: 0 = 作業報告書の確認待ちになったか確認済みとして終えた、2 = 停止条件で止まった、1 = エラー)
  report    最新の版の作業報告書を配信し、ブラウザで付けた動作確認の結果 (OK / NG・コメント) を保存する
  status    計画書の進行状況と、実行の状態 (実行中のステップ・止まった理由) を表示する
  usage     Claude Code / Codexの使用率と、直近の消費速度・上限に達する見込みを表示する
`

// subcommandは1サブコマンドの処理。argsはサブコマンド名を除いた残りの
// 引数で、戻り値はプロセスの終了コード。
type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommandsはサブコマンド名から処理への対応表。
var subcommands = map[string]subcommand{
	"run":    runPilot,
	"report": runReport,
	"status": runStatus,
	"usage":  runUsage,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// runはサブコマンドをディスパッチする。テストから直接呼べるようmainから分離している。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitError
	}

	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	}

	sub, ok := subcommands[name]
	if !ok {
		fmt.Fprintf(stderr, "koryluspilot: 不明なサブコマンド %q\n\n%s", name, usageText)
		return exitError
	}
	return sub(rest, stdout, stderr)
}
