package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// timeLayoutは状態の時刻の表示形式。記録した時刻のオフセットのまま表示する。
const timeLayout = "2006-01-02 15:04 -07:00"

// runStatusはstatusサブコマンドのエントリポイント。
func runStatus(args []string, stdout, stderr io.Writer) int {
	return status(args, state.Root, stdout, stderr)
}

// statusは計画書の進行状況と、stateRootの下の実行状態を表示する。
func status(args []string, stateRoot string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: koryluspilot status <計画書のパス>")
	}
	if err := flags.Parse(args); err != nil {
		// -h/--helpの要求はエラーではなく成功扱いにする。
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitError
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return exitError
	}
	planPath := flags.Arg(0)

	src, err := os.ReadFile(planPath) //#nosec G304 -- 人間が指定した計画書を読む
	if err != nil {
		fmt.Fprintf(stderr, "エラー: 計画書を読めません: %v\n", err)
		return exitError
	}
	p, err := plan.Parse(string(src))
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %s を読み取れません:\n%v\n", planPath, err)
		return exitError
	}
	st, err := state.NewDir(stateRoot, plan.Name(planPath)).Load()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "計画書: %s\n", planPath)
	fmt.Fprintf(stdout, "タスク: %d / %d 完了\n", p.DoneCount(), len(p.Tasks))
	if next, ok := p.Next(); ok {
		fmt.Fprintf(stdout, "次のタスク: %s\n", taskLabel(next))
	} else {
		fmt.Fprintln(stdout, "次のタスク: なし (すべて完了)")
	}
	writeState(stdout, st)

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "タスク一覧:")
	for _, t := range p.Tasks {
		mark := " "
		if t.Done {
			mark = "x"
		}
		fmt.Fprintf(stdout, "  [%s] %s\n", mark, taskLabel(t))
	}
	return exitOK
}

// writeStateは実行状態を表示する。stがnilならまだ実行していない。
func writeState(w io.Writer, st *state.State) {
	if st == nil {
		fmt.Fprintln(w, "状態: 未実行")
		return
	}

	// フィードバックの起票などタスクに属さないステップでは、タスク番号を省く。
	where := st.Step.Label()
	if st.Task != "" {
		where = fmt.Sprintf("タスク %s の%s", st.Task, where)
	}
	switch {
	case st.Stop != nil:
		fmt.Fprintf(w, "状態: 停止中 (%s)\n", where)
	case st.Task != "" || st.Step != "":
		fmt.Fprintf(w, "状態: %s\n", where)
	default:
		fmt.Fprintln(w, "状態: 実行中のタスクなし")
	}
	if st.Branch != "" {
		fmt.Fprintf(w, "ブランチ: %s (元: %s)\n", st.Branch, st.Start)
	}
	if st.Reviews > 0 {
		fmt.Fprintf(w, "レビュー回数: %d\n", st.Reviews)
	}
	if st.Stop != nil {
		fmt.Fprintf(w, "止まった理由: %s\n", st.Stop.Reason)
		for _, d := range st.Stop.Details {
			fmt.Fprintf(w, "  - %s\n", d)
		}
		if st.Stop.Role != "" {
			fmt.Fprintf(w, "解消に使う役割: %s\n", st.Stop.Role)
		}
		fmt.Fprintf(w, "止まった時刻: %s\n", st.Stop.At.Format(timeLayout))
	}
	if st.ReportVersion > 0 {
		verified := ""
		if st.Verified == st.ReportVersion {
			verified = " (確認済み)"
		}
		fmt.Fprintf(w, "作業報告書: 第%d版%s\n", st.ReportVersion, verified)
	}
	fmt.Fprintf(w, "最終更新: %s\n", st.UpdatedAt.Format(timeLayout))
}

// taskLabelはタスクを1行で表す。
func taskLabel(t plan.Task) string {
	if t.Platform == "" {
		return fmt.Sprintf("%s %s", t.ID, t.Title)
	}
	return fmt.Sprintf("%s [%s] %s", t.ID, t.Platform, t.Title)
}
