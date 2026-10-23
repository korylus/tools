// Command korylusflowは、作業計画書のタスクを役割ごとの対話セッションで進めるときに、
// 番と次の指示を決めて状態を記録するCLI。
//
//	korylusflow wait <計画書のパス> -role=<役割>  役割の番まで待ち、次の指示を返す
//	korylusflow done <計画書のパス> -role=<役割>  進めているステップの完了を記録する
//	korylusflow status <計画書のパス>              計画書の進行状況と、人を待っている理由を表示する
//	korylusflow report <計画書のパス>              作業報告書の最新の版を作り直す
//
// 各サブコマンドの本体はinternal/flow/配下のパッケージに置き、本ファイルと
// 同じパッケージのファイルはサブコマンドのディスパッチと入出力・終了コードのみを担う。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// 終了コード。doneが完了の記録を受け付けなかったときの2 (exitRejected) は、
// 役割のセッションが理由を読んで直せばやり直せるもので、1 (exitError) と区別する。
const (
	exitOK       = 0
	exitError    = 1
	exitRejected = 2
)

// usageTextは引数不正・-h時に表示する使い方。
const usageText = `korylusflow - 作業計画書のタスクを役割ごとのセッションで進めるための状態管理CLI

使い方:
  korylusflow wait <計画書のパス> -role=<役割> [-timeout=<時間>]
  korylusflow done <計画書のパス> -role=<役割>
  korylusflow status <計画書のパス>
  korylusflow report <計画書のパス>

役割: implementer / reviewer-codex / reviewer-claude

サブコマンド:
  wait      役割の番が来るまで待ち、次の指示を標準出力に書く。-timeoutを過ぎたら action: wait を返す
  done      進めているステップの完了を記録し、次のステップを決める。実装のステップでは、動作確認の項目を
            標準入力からJSONの配列で受け取る (終了コード: 0 = 記録した、2 = 条件を満たさず受け付けなかった、1 = エラー)
  status    計画書の進行状況と、現在のタスク・ステップ・番の役割・人を待っている理由・所要時間を表示する
  report    作業報告書の最新の版を作り直す。最新の版のあとに記録したステップがあれば次の版を作る
            (通常は計画書の最後で自動で作る。書き込まれた動作確認の結果は引き継ぐ)
`

// subcommandは1サブコマンドの処理。argsはサブコマンド名を除いた残りの
// 引数で、戻り値はプロセスの終了コード。
type subcommand func(args []string, stdout, stderr io.Writer) int

// subcommandsはサブコマンド名から処理への対応表。
var subcommands = map[string]subcommand{
	"wait":   runWait,
	"done":   runDone,
	"status": runStatus,
	"report": runReport,
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
		fmt.Fprintf(stderr, "korylusflow: 不明なサブコマンド %q\n\n%s", name, usageText)
		return exitError
	}
	return sub(rest, stdout, stderr)
}

// errUsageは、計画書のパスが無い・多いなど、引数の数が合わないときのエラー。
var errUsage = errors.New("引数の数が合いません")

// parsePlanArgsは、計画書のパスを1つ取るサブコマンドの引数を読み、計画書のパスを返す。
// flagパッケージは最初のフラグでない引数で読むのをやめるため、計画書のパスの後ろに
// フラグを書く形 (wait <計画書> -role=...) も受け付けるよう、パスの前後で2回読む。
func parsePlanArgs(flags *flag.FlagSet, args []string) (string, error) {
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() == 0 {
		return "", errUsage
	}
	path := flags.Arg(0)
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errUsage
	}
	return path, nil
}

// parseFailedは、parsePlanArgsのエラーを終了コードにする。-h/--helpの要求は
// エラーではなく成功扱いにする。引数の数の誤りは使い方を表示する。
func parseFailed(flags *flag.FlagSet, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if errors.Is(err, errUsage) {
		flags.Usage()
	}
	return exitError
}
