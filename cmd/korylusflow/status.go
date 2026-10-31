package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/korylus/tools/internal/flow/source"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/plan"
)

// timeLayoutは状態の時刻の表示形式。記録した時刻のオフセットのまま表示する。
const timeLayout = "2006-01-02 15:04 -07:00"

// runStatusはstatusサブコマンドのエントリポイント。
func runStatus(args []string, stdout, stderr io.Writer) int {
	return status(context.Background(), args, state.Root, source.Parse, time.Now(), stdout, stderr)
}

// statusは計画書の進行状況と、stateRootの下の状態・所要時間を表示する。
// nowは進めているステップの経過時間に使う。
func status(ctx context.Context, args []string, stateRoot string, parse parseSource, now time.Time, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "使い方: korylusflow status <計画書>")
	}
	planURL, err := parsePlanArgs(flags, args)
	if err != nil {
		return parseFailed(flags, err)
	}

	src, err := parse(planURL)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	body, err := src.Read(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	p, err := plan.Parse(body)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %s を読み取れません:\n%v\n", planURL, err)
		return exitError
	}
	name, err := src.Name(p)
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	dir := state.NewDir(stateRoot, name)
	st, err := dir.Load()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}
	events, err := dir.ReadEvents()
	if err != nil {
		fmt.Fprintf(stderr, "エラー: %v\n", err)
		return exitError
	}

	fmt.Fprintf(stdout, "計画書: %s\n", planURL)
	fmt.Fprintf(stdout, "タスク: %d / %d 完了\n", p.DoneCount(), len(p.Tasks))
	if next, ok := p.Next(); ok {
		fmt.Fprintf(stdout, "次のタスク: %s\n", taskLabel(next))
	} else {
		fmt.Fprintln(stdout, "次のタスク: なし (すべて完了)")
	}
	writeState(stdout, st, now)
	if st != nil && len(st.Reports) > 0 {
		last := st.Reports[len(st.Reports)-1]
		fmt.Fprintf(stdout, "作業報告書: 第%d版 (%s)\n", last.Version, dir.ReportPath(last.Version))
	}
	writeDurations(stdout, events)

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

// writeStateは状態を表示する。stがnilならまだループを動かしていない。
func writeState(w io.Writer, st *state.State, now time.Time) {
	if st == nil {
		fmt.Fprintln(w, "状態: 未実行")
		return
	}

	where := fmt.Sprintf("タスク %s の%s", st.Task, st.Step.Label())
	switch {
	case st.Pending != nil && st.Pending.Step == state.StepWalkthrough:
		fmt.Fprintf(w, "状態: %sの完了を記録中 (waitかdoneで再開できます)\n", st.Pending.Step.Label())
	case st.Pending != nil:
		fmt.Fprintf(w, "状態: タスク %s の%sの完了を記録中 (waitかdoneで再開できます)\n", st.Pending.Task, st.Pending.Step.Label())
	case st.Step == state.StepWalkthrough:
		fmt.Fprintln(w, "状態: 作業報告書を作る前に、処理の流れを書いています")
	case st.Step == "":
		fmt.Fprintln(w, "状態: 進めているタスクなし")
	case st.Ask != nil:
		fmt.Fprintf(w, "状態: 人を待っています (%s)\n", where)
	default:
		fmt.Fprintf(w, "状態: %s\n", where)
	}
	if st.Role != "" {
		fmt.Fprintf(w, "番の役割: %s (%s)\n", st.Role.Label(), st.Role)
	}
	if !st.Since.IsZero() {
		fmt.Fprintf(w, "ステップの開始: %s (経過 %s)\n", st.Since.Format(timeLayout), formatDuration(now.Sub(st.Since)))
	}
	if rec := st.TaskRecord(st.Task); rec != nil {
		fmt.Fprintf(w, "リポジトリ: %s (作業するディレクトリ: %s)\n", rec.Repo, rec.Dir)
		fmt.Fprintf(w, "ブランチ: %s (ベース: %s)\n", rec.Branch, rec.Base)
		if n := len(rec.Reviews); n > 0 {
			fmt.Fprintf(w, "レビュー: %d回 (最新の評価: %s)\n", n, rec.Reviews[n-1].Verdict)
			if rec.ReviewLimit > 0 {
				fmt.Fprintf(w, "レビューの上限: %d回\n", rec.ReviewLimit)
			}
		}
	}
	if st.Ask != nil {
		fmt.Fprintf(w, "人を待っている理由: %s\n", st.Ask.Reason)
		for _, d := range st.Ask.Details {
			fmt.Fprintf(w, "  - %s\n", d)
		}
	}
	fmt.Fprintf(w, "最終更新: %s\n", st.UpdatedAt.Format(timeLayout))
}

// writeDurationsは、記録したステップの所要時間を、役割ごとと人を待った時間に分けて表示する。
func writeDurations(w io.Writer, events []state.Event) {
	if len(events) == 0 {
		return
	}
	var total, human time.Duration
	byRole := map[state.Role]time.Duration{}
	for _, e := range events {
		total += e.Duration()
		if e.Step == state.StepAsk {
			human += e.Duration()
			continue
		}
		byRole[e.Role] += e.Duration()
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "所要時間:")
	fmt.Fprintf(w, "  合計: %s\n", formatDuration(total))
	for _, r := range state.Roles {
		if d, ok := byRole[r]; ok {
			fmt.Fprintf(w, "  %s: %s\n", r.Label(), formatDuration(d))
		}
	}
	fmt.Fprintf(w, "  人を待った時間: %s\n", formatDuration(human))
}

// formatDurationは所要時間を分単位に丸めて表す。
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%d時間%d分", h, m)
	}
	return fmt.Sprintf("%d分", m)
}

// taskLabelはタスクを1行で表す。
func taskLabel(t plan.Task) string {
	label := fmt.Sprintf("%s %s", t.ID, t.Title)
	if t.Platform != "" {
		label = fmt.Sprintf("%s [%s] %s", t.ID, t.Platform, t.Title)
	}
	if t.Assignee != "" {
		label += fmt.Sprintf(" (担当: %s)", t.Assignee)
	}
	return label
}
