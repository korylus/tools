package flow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// goListCacheは、go listで取ったパッケージの一覧をコミットごとに使い回す入れ物。
type goListCache map[string][]gate.GoPackage

// finishPlanは、計画書の最後まで進んだときに、ハブのブランチを人間のチェックアウトへ
// 取り込み、作業報告書の次の版を作る。前の版から新しく終えたタスクが無ければ、
// 版を作らずに最新の版の動作確認の結果を読んで分岐する (readVerification)。
// フィードバックの起票へ進むときはfeedbackにtrueを返す。
func (r *run) finishPlan(ctx context.Context) (feedback bool, err error) {
	st := r.st
	if len(st.Tasks) == 0 {
		r.logf("計画書のタスクをすべて終えました")
		return false, nil
	}
	dir := report.Dir(r.p.Root, r.planName)
	if n := len(st.Reports); n > 0 && st.Reports[n-1].Tasks == len(st.Tasks) {
		return r.readVerification(dir)
	}

	r.logf("計画書のタスクをすべて終えました。ハブのブランチを人間のチェックアウトへ取り込み、作業報告書を作ります")
	branches := make([]string, 0, len(st.Tasks))
	for _, t := range st.Tasks {
		branches = append(branches, t.Branch)
	}
	co, err := r.hub.ExportTo(ctx, r.p.Root, branches)
	if err != nil {
		return false, err
	}
	in, err := r.reportInput(ctx, dir, co)
	if err != nil {
		return false, err
	}
	rep, err := report.Build(in)
	if err != nil {
		return false, fmt.Errorf("作業報告書を組み立てられません: %w", err)
	}
	if err := report.Write(dir, rep, nil); err != nil {
		return false, fmt.Errorf("作業報告書を書けません: %w", err)
	}
	st.ReportVersion = rep.Version
	st.Reports = append(st.Reports, state.ReportRecord{Version: rep.Version, Tasks: len(st.Tasks), At: rep.CreatedAt})
	r.reportPath = report.HTMLPath(dir, rep.Version)
	r.logf("作業報告書 第%d版 を作りました: %s", rep.Version, r.reportPath)
	return false, r.save()
}

// reportInputは、状態・計画書のスナップショット・所要時間と使用率の記録・ハブの差分・
// 前の版から、作業報告書の入力を集める。
func (r *run) reportInput(ctx context.Context, dir string, co hub.Checkout) (report.Input, error) {
	st := r.st
	in := report.Input{PlanPath: r.p.PlanPath, Version: st.ReportVersion + 1, Now: r.now(), State: st, Plan: string(r.snapshot), Config: r.cfg, Checkout: co}
	var err error
	if in.Events, err = r.dir.ReadEvents(); err != nil {
		return in, err
	}
	if _, in.StepUsages, err = r.tracker.Log.Read(); err != nil {
		return in, err
	}
	snap, err := plan.Parse(in.Plan)
	if err != nil {
		return in, err
	}

	cache := goListCache{}
	var declared []plan.Declaration
	for _, rec := range st.Tasks {
		base, err := r.resolve(ctx, rec.Start)
		if err != nil {
			return in, err
		}
		task, ok := findTask(snap, rec.ID)
		if !ok {
			task = plan.Task{ID: rec.ID}
		}
		declared = append(declared, task.Protected...)
		diff, rep, err := r.inspect(ctx, gate.Input{Config: r.cfg, Task: task}, base, rec.Commit, cache)
		if err != nil {
			return in, err
		}
		in.Diffs = append(in.Diffs, report.TaskDiff{Diff: diff, Inspect: rep})
	}
	// 最終的な差分は、どれかのタスクが宣言した領域を宣言済みとして検査する。
	base, err := r.resolve(ctx, st.Tasks[0].Start)
	if err != nil {
		return in, err
	}
	if _, in.Final, err = r.inspect(ctx, gate.Input{Config: r.cfg, Task: plan.Task{Protected: declared}}, base, st.Tasks[len(st.Tasks)-1].Commit, cache); err != nil {
		return in, err
	}

	if prev := in.Version - 1; prev > 0 {
		if in.Previous, err = report.Load(dir, prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return in, err
		}
		if in.PreviousResult, err = report.LoadVerification(dir, prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return in, err
		}
	}
	return in, nil
}

// resolveはハブのブランチbranchの先端のコミットを返す。
func (r *run) resolve(ctx context.Context, branch string) (string, error) {
	commit, ok, err := r.hub.Resolve(ctx, branch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("ブランチ %s がハブにありません", branch)
	}
	return commit, nil
}

// inspectは、ハブのコミットbaseからheadまでの差分を読み、inに足して差分の検査を行う。
// cacheは、go listで取ったパッケージの一覧をコミットごとに使い回す入れ物で、
// nilならgo listの実行をゲート検査の所要時間として記録する。
func (r *run) inspect(ctx context.Context, in gate.Input, base, head string, cache goListCache) (*gate.Diff, *gate.Report, error) {
	var err error
	if in.Diff, err = gate.ReadDiff(ctx, r.hub.GitDir(), base, head); err != nil {
		return nil, nil, err
	}
	needFuncs, needDeps := goChecks(r.cfg)
	if needFuncs {
		if err := in.Diff.ReadGoSources(ctx, r.hub.GitDir(), base, head); err != nil {
			return nil, nil, err
		}
	}
	if needDeps {
		var deps gate.GoDeps
		if deps.Base, err = r.goPackages(ctx, base, cache); err != nil {
			return nil, nil, err
		}
		if deps.Head, err = r.goPackages(ctx, head, cache); err != nil {
			return nil, nil, err
		}
		in.GoDeps = &deps
	}
	rep, err := gate.Inspect(in)
	if err != nil {
		return nil, nil, err
	}
	return in.Diff, rep, nil
}

// goPackagesは、ゲートのランナーでコミットcommitのパッケージの一覧を取る。
func (r *run) goPackages(ctx context.Context, commit string, cache goListCache) ([]gate.GoPackage, error) {
	if pkgs, ok := cache[commit]; ok {
		return pkgs, nil
	}
	commands := gate.CommandRunner{Runner: r.cfg.Gate.Runner, Repo: r.cfg.Hub.Path}
	started := r.now()
	pkgs, err := commands.GoPackages(ctx, commit)
	if cache != nil {
		if err == nil {
			cache[commit] = pkgs
		}
		return pkgs, err
	}
	outcome := state.OutcomeOK
	if err != nil {
		outcome = state.OutcomeFailed
	}
	r.event(state.Event{Kind: state.EventGate, Task: r.st.Task, Step: state.StepGate, Started: started, Ended: r.now(), Outcome: outcome, Command: "go list " + commit})
	return pkgs, err
}
