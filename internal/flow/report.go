package flow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/report"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/plan"
)

// Reportは作業報告書の最新の版を作り直し、そのパスを返す。最新の版のあとに記録した
// ステップがあれば、次の版を作る。タスクを進めている途中なら作らない。
func (f *Flow) Report(ctx context.Context) (string, error) {
	unlock, err := f.Dir.LockContext(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = unlock() }()

	st, err := f.Dir.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("まだループを始めていません")
	}
	if err != nil {
		return "", err
	}
	if st.Pending != nil && st.Pending.Step == state.StepWalkthrough {
		return "", fmt.Errorf("%sの完了を記録している途中です。korylusflow wait か done で再開してから実行してください", st.Pending.Step.Label())
	}
	if st.Pending != nil {
		return "", fmt.Errorf("タスク %s の完了を記録している途中です。korylusflow wait か done で再開してから実行してください", st.Pending.Task)
	}
	if st.Step == state.StepWalkthrough {
		return "", errors.New("処理の流れを書いている途中です。korylusflow done で記録してから実行してください")
	}
	if st.Step != "" {
		return "", fmt.Errorf("タスク %s を進めている途中です。計画書の最後まで進んでから実行してください", st.Task)
	}
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", errors.New("記録したステップがありません")
	}

	// 同じ版を作り直すときはその版に記録した処理の流れを使う。次の版を作るときに処理の
	// 流れのステップを経ていなければ (ステップを足す前に作った状態など)、記録なしとして作る。
	v := len(st.Reports)
	var w *state.Walkthrough
	if hasNewEvents(st, events) {
		v++
		w = pendingWalkthrough(st, events)
	} else {
		w = st.Reports[v-1].Walkthrough
	}
	p, err := f.readPlan(ctx)
	if err != nil {
		return "", err
	}
	if err := f.writeReport(ctx, st, p, events, v, w); err != nil {
		return "", err
	}
	if err := f.Dir.Save(st, f.Now()); err != nil {
		return "", err
	}
	return f.Dir.ReportPath(v), nil
}

// finishPlanは、計画書pの最後まで進んだ状態stで、最新の版のあとに記録したステップが
// あれば、作業報告書の次の版に進む。まだ処理の流れを書いていなければ、作業報告書を
// 作る前に実装者が処理の流れを書くステップにする。書き終えていれば次の版を作る。
// 計画書の最後でwaitを何度呼ばれても版を増やさない。
func (f *Flow) finishPlan(ctx context.Context, st *state.State, p *plan.Plan) error {
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return err
	}
	if !hasNewEvents(st, events) {
		return nil
	}
	w := pendingWalkthrough(st, events)
	if w == nil {
		f.startWalkthrough(st)
		return nil
	}
	return f.writeReport(ctx, st, p, events, len(st.Reports)+1, w)
}

// hasNewEventsは、最新の版の作業報告書のあとに記録したステップがあるかを返す。
func hasNewEvents(st *state.State, events []state.Event) bool {
	covered := 0
	if n := len(st.Reports); n > 0 {
		covered = st.Reports[n-1].Events
	}
	return len(events) > covered
}

// writeReportは計画書pの作業報告書の版versionを組み立てて書き、状態stに記録する。
// 既にある版を作り直すときは、その版に人が書き込んだ結果を引き継ぐ。書き込みが
// 区切りを隠していて、生成した節がコメントに取り込まれていれば作り直さない。
// wは版に載せる処理の流れで、版の記録に残す。次の版を作ったら、状態の未記録の処理の流れを消す。
func (f *Flow) writeReport(ctx context.Context, st *state.State, p *plan.Plan, events []state.Event, version int, w *state.Walkthrough) error {
	var err error
	now := f.Now()
	in := report.Input{
		PlanPath: f.PlanPath, Name: f.Name, Plan: p, Version: version, Now: now,
		State: st, Events: events, Changes: map[string]report.Changes{}, Walkthrough: w,
	}
	if version > 1 {
		in.Since = st.Reports[version-2].Events
		if in.Previous, err = f.readResults(version - 1); err != nil {
			return err
		}
	}
	if version <= len(st.Reports) {
		if in.Current, err = f.readResults(version); err != nil {
			return err
		}
		if found := report.Absorbed(in); len(found) > 0 {
			return fmt.Errorf("%s の区切りを読み取れないため、作り直しません。閉じていないコードブロックや、消した項目のID (<!-- item: ... -->) が無いかを確かめ、次の箇所を直してから実行してください:\n- %s",
				f.Dir.ReportPath(version), strings.Join(found, "\n- "))
		}
	}
	for _, rec := range st.Tasks {
		if rec.Prepared {
			in.Changes[rec.ID] = readChanges(ctx, rec)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := f.Dir.WriteReport(version, report.Build(in)); err != nil {
		return fmt.Errorf("作業報告書を書けません: %w", err)
	}
	rec := state.ReportRecord{Version: version, Events: len(events), At: now, Walkthrough: w}
	if version <= len(st.Reports) {
		st.Reports[version-1] = rec
	} else {
		st.Reports = append(st.Reports, rec)
		st.Walkthrough = nil
	}
	return nil
}

// readResultsは版vの作業報告書に書き込まれた結果を読む。作業報告書が無ければnilを返す。
func (f *Flow) readResults(v int) (*report.Results, error) {
	src, err := os.ReadFile(f.Dir.ReportPath(v)) //#nosec G304 -- 状態ディレクトリの、版の番号から決めたパス
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return report.ParseResults(string(src)), nil
}

// readChangesは、タスクの記録recの元のブランチからタスクのブランチまでの変更を読む。
// ブランチが消えているなどで読めなくても作業報告書は作れるよう、理由を残して続ける。
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

// latestReportは最新の版の作業報告書のパスを返す。まだ作っていなければ空文字。
func (f *Flow) latestReport(st *state.State) string {
	n := len(st.Reports)
	if n == 0 {
		return ""
	}
	return f.Dir.ReportPath(st.Reports[n-1].Version)
}
