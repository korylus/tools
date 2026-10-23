package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/flow"
	"github.com/korylus/tools/internal/pilot/state"
)

// runPilotはrunサブコマンドのエントリポイント。
func runPilot(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return pilotRun(ctx, args, nil, stdout, stderr)
}

// pilotRunは引数を読んで計画書を進め、結果を表示する。customizeは組み立てた
// Pilotを実行の前に差し替える (テストで偽のランナーなどを入れる)。nilなら差し替えない。
func pilotRun(ctx context.Context, args []string, customize func(*flow.Pilot), stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	note := flags.String("note", "", "止まったステップを再開するときに、エージェントへ渡す補足")
	approve := flags.Bool("approve", false, "現在の計画書と設定ファイルを、新しい基準として受け入れる")
	configPath := flags.String("config", config.FileName, "設定ファイル")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: koryluspilot run <計画書のパス> [-note=<補足>] [-approve] [-config=<設定ファイル>]")
		flags.PrintDefaults()
	}
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if len(positional) != 1 {
		flags.Usage()
		return exitError
	}
	planPath, err := projectPath(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	p := &flow.Pilot{PlanPath: planPath, ConfigPath: *configPath, Note: *note, Approve: *approve, StateRoot: state.Root, Out: stdout}
	if customize != nil {
		customize(p)
	}
	out, err := p.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	if out.Stopped() {
		writeStop(stdout, planPath, out)
		return exitStopped
	}
	writeDone(stdout, out)
	return exitOK
}

// parseInterspersedはargsをflagsで読み、位置引数を返す。フラグは計画書のパスの
// 後ろにも書けるよう、位置引数を取り出しながら読む。
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for rest := args; ; {
		if err := flags.Parse(rest); err != nil {
			return nil, err
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
}

// projectPathは計画書のパスを、プロジェクトのルートからの相対パスに正規化する。
// 役割のコンテナのエージェントにも同じパスで渡すため、絶対パスとルートの外は受け付けない。
func projectPath(p string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(p))
	if filepath.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("計画書のパス %q は、プロジェクトのルートからの相対パスで指定してください", p)
	}
	return clean, nil
}

// writeStopは止まった理由と、解消に使う役割・再開の方法を表示する。
func writeStop(w io.Writer, planPath string, out *flow.Outcome) {
	st := out.State
	where := st.Step.Label()
	if st.Task != "" {
		where = fmt.Sprintf("タスク %s の%s", st.Task, where)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "停止しました (%s)\n", where)
	fmt.Fprintf(w, "理由: %s\n", st.Stop.Reason)
	for _, d := range st.Stop.Details {
		fmt.Fprintf(w, "  - %s\n", strings.ReplaceAll(d, "\n", "\n    "))
	}
	if role := st.Stop.Role; role != "" {
		fmt.Fprintf(w, "解消に使う役割: %s\n", role)
		if out.Config != nil {
			if r, ok := out.Config.Roles[role]; ok {
				fmt.Fprintf(w, "  コンテナ: %s\n", strings.Join(r.Runner, " "))
			}
		}
	}
	if st.Step == state.StepApprove {
		fmt.Fprintln(w, "計画書に追加されたタスクを確かめ、次のコマンドで承認して続けてください:")
		fmt.Fprintf(w, "  koryluspilot run %s -approve\n", planPath)
		return
	}
	fmt.Fprintln(w, "解消したら、次のコマンドで止まったステップから再開してください:")
	fmt.Fprintf(w, "  koryluspilot run %s -note=<補足>\n", planPath)
	if st.Stop.Role == config.RoleDesigner {
		fmt.Fprintln(w, "計画書や設定ファイルを直したときは、-approve を付けて新しい基準として受け入れてください")
	}
}

// writeDoneは計画書のタスクを終えたときに、ブランチの順序と、確認を待つ作業報告書か
// 確認済みとして終えたことを表示する。
func writeDone(w io.Writer, out *flow.Outcome) {
	st := out.State
	if len(st.Tasks) == 0 {
		return
	}
	fmt.Fprintln(w, "ブランチの順序 (pushとPRの作成に使う):")
	for _, t := range st.Tasks {
		fmt.Fprintf(w, "  %s (元: %s)\n", t.Branch, t.Start)
	}
	switch {
	case out.Report != "":
		fmt.Fprintf(w, "作業報告書 (第%d版) で動作確認してください: %s\n", st.ReportVersion, out.Report)
		fmt.Fprintln(w, "確認結果を保存したら、もう一度 run を実行してください。NGの項目か全体へのコメントがあれば、フィードバック対応のフェーズを起票して実行します")
	case st.Verified > 0 && st.Verified == st.ReportVersion:
		fmt.Fprintf(w, "作業報告書 (第%d版) ですべての確認項目がOKになり、計画書を確認済みとして終えました\n", st.Verified)
	}
}
