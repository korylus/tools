package flow

import (
	"context"
	"errors"
	"io/fs"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/report"
	"github.com/korylus/tools/internal/flow/state"
)

// Reportは、マージするPR・タスクごとの変更・実装者が行ったタスク・先送りした指摘・
// 保護領域を、状態・計画書・gitから組み立てて返す。状態を書き換えないため、
// ロックを取らず、タスクを進めている途中でも組み立てる。
func (f *Flow) Report(ctx context.Context) ([]byte, error) {
	st, err := f.Dir.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("まだループを始めていません")
	}
	if err != nil {
		return nil, err
	}
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return nil, err
	}
	p, err := f.readPlan(ctx)
	if err != nil {
		return nil, err
	}
	in := report.Input{
		PlanURL: f.PlanURL, Name: f.Name, Plan: p,
		State: st, Events: events, Changes: map[string]report.Changes{},
	}
	for _, rec := range st.Tasks {
		if rec.Prepared {
			in.Changes[rec.ID] = readChanges(ctx, rec)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return report.Build(in), nil
}

// finishPlanは、計画書の最後まで進んだ状態stで、最新の回の作業報告書のあとに記録した
// ステップがあれば、実装者が作業報告書を書くステップにする。
// 計画書の最後でwaitを何度呼ばれても、作業報告書のステップを繰り返さない。
func (f *Flow) finishPlan(st *state.State) error {
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return err
	}
	if hasNewEvents(st, events) {
		f.startReport(st)
	}
	return nil
}

// hasNewEventsは、最新の回の作業報告書のあとに記録したステップがあるかを返す。
func hasNewEvents(st *state.State, events []state.Event) bool {
	covered := 0
	if n := len(st.Reports); n > 0 {
		covered = st.Reports[n-1].Events
	}
	return len(events) > covered
}

// readChangesは、タスクの記録recの元のブランチからタスクのブランチまでの変更を読む。
// ブランチが消えているなどで読めなくても事実を組み立てられるよう、理由を残して続ける。
func readChanges(ctx context.Context, rec state.TaskRecord) report.Changes {
	var missing []string
	for _, name := range []string{rec.Branch, rec.Base} {
		ok, err := branch.Exists(ctx, rec.Repo, name)
		if err != nil {
			return report.Changes{Err: err.Error()}
		}
		if !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return report.Changes{Missing: missing}
	}

	files, err := branch.Changes(ctx, rec.Repo, rec.Base, rec.Branch)
	if err != nil {
		return report.Changes{Err: err.Error()}
	}
	return report.Changes{Files: files}
}
