// Command korylusflowは、作業計画書のタスクを役割ごとの対話セッションで進めるときに、
// 番と次の指示を決めて状態を記録するCLI。
//
//	korylusflow wait <計画書> -role=<役割>  役割の番まで待ち、次の指示を返す
//	korylusflow done <計画書> -role=<役割>  進めているステップの完了を記録する
//	korylusflow status <計画書>              計画書の進行状況と、人を待っている理由を表示する
//	korylusflow report <計画書>              マージするPRなど、計画書を進めた結果の事実を表示する
//
// 各サブコマンドの本体はinternal/flow/配下のパッケージに置き、本ファイルと
// 同じパッケージのファイルはサブコマンドのディスパッチと入出力・終了コードのみを担う。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/korylus/tools/internal/flow"
	"github.com/korylus/tools/internal/flow/source"
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
  korylusflow wait <計画書> -role=<役割> [-timeout=<時間>]
  korylusflow done <計画書> -role=<役割> [-review-limit=<新しい上限>]
  korylusflow status <計画書>
  korylusflow report <計画書>

計画書: WikinoのページのURL (https://wikino.app/s/<スペース>/pages/<ページ番号>)
役割: implementer / reviewer-codex / reviewer-claude

サブコマンド:
  wait      役割の番が来るまで待ち、次の指示を標準出力に書く。-timeoutを過ぎたら action: wait を返す
  done      進めているステップの完了を記録し、次のステップを決める。実装のステップでは動作確認の項目を、
            作業報告書のステップではURLと処理の流れを書かなかった理由を、標準入力からJSONで受け取る (終了コード: 0 = 記録した、2 = 条件を満たさず受け付けなかった、1 = エラー)
            上限に達して人を待っているタスクは、-review-limitで上限を延ばして再レビューを続けられる
  status    計画書の進行状況と、現在のタスク・ステップ・番の役割・人を待っている理由・所要時間を表示する
  report    マージするPR・タスクごとの変更・実装者が行ったタスク・先送りした指摘・保護領域を、
            状態・計画書・gitから組み立てて標準出力に書く。状態は書き換えない
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

// errUsageは、計画書の指定が無い・多いなど、引数の数が合わないときのエラー。
var errUsage = errors.New("引数の数が合いません")

// parsePlanArgsは、計画書の指定を1つ取るサブコマンドの引数を読み、計画書の指定を返す。
// flagパッケージは最初のフラグでない引数で読むのをやめるため、計画書の指定の後ろに
// フラグを書く形 (wait <計画書> -role=...) も受け付けるよう、指定の前後で2回読む。
func parsePlanArgs(flags *flag.FlagSet, args []string) (string, error) {
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() == 0 {
		return "", errUsage
	}
	planURL := flags.Arg(0)
	if err := flags.Parse(flags.Args()[1:]); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errUsage
	}
	return planURL, nil
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

// parseSourceは、コマンドで受け取った計画書の指定から読み取り元を返す。
// 本番ではsource.Parseを渡し、テストでは一時ファイルを読む実装に差し替える。
type parseSource func(arg string) (source.Source, error)

// openFlowは、計画書の指定argをparseで読み取り元にし、stateRootの下の状態で扱うFlowを返す。
func openFlow(ctx context.Context, parse parseSource, arg, stateRoot string) (*flow.Flow, error) {
	src, err := parse(arg)
	if err != nil {
		return nil, err
	}
	return flow.New(ctx, src, stateRoot)
}
