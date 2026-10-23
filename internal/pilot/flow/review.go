package flow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
	"github.com/korylus/tools/internal/review"
)

// reviewOrCompleteは、ゲート検査を通ったあとか、修正の要らない回答のあとに、
// 次にレビューするかタスクを終えるかを決める。まだレビューしていないときと、
// 最後のレビューがRequest Changesで上限に達していないときはレビューする。
func (r *run) reviewOrComplete(ctx context.Context) (bool, error) {
	if r.st.Reviews == 0 || (review.Verdict(r.st.Verdict) == review.RequestChanges && r.st.Reviews < r.cfg.MaxReviews) {
		return r.startReview()
	}
	return r.completeTask(ctx)
}

// startReviewは次のレビューを、reviewersの役割を順に交互に使って始める。
// レビューは新しいセッションで行わせるため、前のレビュアーのセッションを消す。
func (r *run) startReview() (bool, error) {
	n := r.st.Reviews + 1
	r.st.Reviews = n
	r.st.Role = r.cfg.Reviewers[(n-1)%len(r.cfg.Reviewers)]
	delete(r.st.Sessions, state.StepReview)
	r.st.ReviewDoc, r.st.Verdict = "", ""
	r.logf("タスク %s: %d回目のレビューを %s で行います", r.st.Task, n, r.st.Role)
	return r.advance(state.StepReview)
}

// reviewは、レビュアーに無人実行モードの /korylus-review でタスクの差分をレビューさせ、
// 書かれたレビュードキュメントの総合評価を読む。
func (r *run) review(ctx context.Context) (bool, error) {
	role := r.st.Role
	if r.st.Sessions[state.StepReview] == "" {
		// レビューを新しく始めるときだけ、レビュアーのクローンをハブの先端に合わせる。
		// 再開したセッションは、同じクローンでレビューを続けさせる。
		err := r.hub.PrepareClone(ctx, r.roleRunner(role), r.st.Branch, r.st.Start)
		var rejected *hub.RejectedError
		if errors.As(err, &rejected) {
			return r.stop(role, "役割のクローンを準備できません", rejected.Reason)
		}
		if err != nil {
			return false, err
		}
	}

	res, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepReview,
		role:    role,
		kind:    agent.Normal,
		prompt:  reviewPrompt(r.tool(role), cloneRef(r.st.Start), r.p.PlanPath, r.st.Task, r.reviewAreas(), r.takeNote()),
		session: state.StepReview,
		schema:  true,
		allowed: []string{r.reviewDocGlob()},
	})
	if err != nil || stopped {
		return stopped, err
	}
	if res.Status == agent.StatusEscalated {
		return r.stop(role, fmt.Sprintf("レビューのエージェントがエスカレーションを返しました: %s", res.Summary), escalationMessages(res)...)
	}

	doc, ok, err := review.Latest(r.p.Root, r.st.Branch)
	if err != nil {
		return false, err
	}
	if !ok || slices.ContainsFunc(r.st.ReviewLog, func(rec state.ReviewRecord) bool { return rec.Task == r.st.Task && rec.Doc == doc }) {
		return r.stop(role, fmt.Sprintf("%d回目のレビューのレビュードキュメントが %s/%s-NNN.md に見つかりません", r.st.Reviews, review.Dir, r.st.Branch))
	}
	d, stopped, err := r.readReview(role, doc)
	if err != nil || stopped {
		return stopped, err
	}
	r.st.ReviewDoc, r.st.Verdict = doc, string(d.Verdict)
	r.st.ReviewLog = append(r.st.ReviewLog, state.ReviewRecord{Task: r.st.Task, Number: r.st.Reviews, Role: role, Doc: doc, Verdict: string(d.Verdict), At: r.now()})
	r.logf("タスク %s: %d回目のレビューの評価は %s です (%s)", r.st.Task, r.st.Reviews, d.Verdict, doc)

	// 回答のステップはチェック済みの選択肢を人間の回答として扱うため、レビュアーが
	// 入れたチェックで要人間判断の指摘を素通りさせないよう、ここで人間に確かめさせる。
	if checked := d.Checked(); len(checked) > 0 {
		r.st.Step = state.StepAnswer
		details := make([]string, 0, len(checked)+1)
		for _, f := range checked {
			details = append(details, f.Describe())
		}
		details = append(details, "人間の回答として残すチェック以外を外してから再開すると、残したチェックを回答として進みます")
		return r.stop(role, fmt.Sprintf("レビュードキュメント %s で、レビュアーが対応方針の選択肢にチェックを入れています", doc), details...)
	}
	if d.Verdict == review.RequestChanges && r.st.Reviews >= r.cfg.MaxReviews {
		// レビューは終えているため、再開したら回答から進める。
		r.st.Step = state.StepAnswer
		return r.stop(role, fmt.Sprintf("レビューが上限の%d回に達しても Request Changes でした", r.cfg.MaxReviews),
			"レビュードキュメント: "+doc,
			"内容を確かめて再開すると、回答と修正のあと、再レビューせずに次のタスクへ進みます")
	}
	return r.advance(state.StepAnswer)
}

// answerはレビュードキュメントの対応方針に決定的に回答する。人間の判断が要る指摘か、
// 回答できない指摘があれば止める。人間がドキュメントで選択肢を選んでから再開すれば、
// その選択肢を回答として続ける。
func (r *run) answer(ctx context.Context) (bool, error) {
	role := r.st.Role
	d, stopped, err := r.readReview(role, r.st.ReviewDoc)
	if err != nil || stopped {
		return stopped, err
	}
	a := d.Answer()
	if a.Blocked() {
		details := make([]string, 0, len(a.Pending)+len(a.Invalid))
		for _, f := range a.Pending {
			details = append(details, f.Describe())
		}
		details = append(details, a.Invalid...)
		return r.stop(role, fmt.Sprintf("レビュードキュメント %s に、人間の判断が要る指摘か推奨どおりに回答できない指摘があります。対応方針を選んでから再開してください", r.st.ReviewDoc), details...)
	}
	if err := os.WriteFile(filepath.Join(r.p.Root, r.st.ReviewDoc), a.Output, 0o600); err != nil {
		return false, fmt.Errorf("レビュードキュメントに回答を書けません: %w", err)
	}

	for _, dec := range a.Decisions {
		if !dec.Defers() {
			continue
		}
		for _, o := range dec.Chosen {
			f := state.DeferredFinding{Task: r.st.Task, Doc: r.st.ReviewDoc, Title: dec.Finding.Title, Choice: o.Text}
			if !slices.Contains(r.st.Deferred, f) {
				r.st.Deferred = append(r.st.Deferred, f)
			}
		}
	}
	if a.NeedsFix() {
		return r.advance(state.StepFix)
	}
	r.logf("タスク %s: 修正の要る指摘が無いため、修正を飛ばします", r.st.Task)
	return r.reviewOrComplete(ctx)
}

// fixは、レビューを行った役割のセッションを再開して、回答した対応方針どおりに修正させる。
func (r *run) fix(ctx context.Context) (bool, error) {
	role := r.st.Role
	res, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepFix,
		role:    role,
		kind:    agent.Fix,
		prompt:  fixPrompt(r.st.ReviewDoc, r.takeNote()),
		session: state.StepReview,
		schema:  true,
		allowed: []string{r.p.PlanPath, r.reviewDocGlob()},
	})
	if err != nil || stopped {
		return stopped, err
	}
	return r.finishAgent(state.StepFix, role, res, state.StepCommit)
}

// readReviewはレビュードキュメントdocを読み取る。読み取れなければ、解消に使う役割roleを添えて止める。
func (r *run) readReview(role, doc string) (*review.Document, bool, error) {
	src, err := os.ReadFile(filepath.Join(r.p.Root, doc)) //#nosec G304 -- docはreview.Latestがレビュードキュメントの置き場所から選んだパス
	if err != nil {
		return nil, false, fmt.Errorf("レビュードキュメントを読めません: %w", err)
	}
	d, err := review.Parse(src)
	if err != nil {
		stopped, saveErr := r.stop(role, fmt.Sprintf("レビュードキュメント %s を読み取れません", doc), err.Error())
		return nil, stopped, saveErr
	}
	return d, false, nil
}

// reviewDocGlobは、実行中のタスクのレビュードキュメントのglob (プロジェクトのルートからの相対パス) を返す。
func (r *run) reviewDocGlob() string {
	return review.Dir + "/" + r.st.Branch + "-*.md"
}

// reviewAreaは、レビュアーに照合させる保護領域1つ。
type reviewArea struct {
	name, label string
	points      []string
}

// reviewAreasは、最後のゲート検査で宣言どおりに触れた保護領域を、設定の順に返す。
func (r *run) reviewAreas() []reviewArea {
	var areas []reviewArea
	for _, p := range r.cfg.Protected {
		if slices.Contains(r.st.Touched, p.Name) {
			areas = append(areas, reviewArea{name: p.Name, label: p.Label, points: p.ReviewPoints})
		}
	}
	return areas
}

// declaredTouchesは、ゲート検査の結果のうち宣言どおりに触れた保護領域の名前を返す。
func declaredTouches(report *gate.Report) []string {
	var names []string
	for _, t := range report.Touches {
		if t.Declared {
			names = append(names, t.Name)
		}
	}
	return names
}

// completeTaskは、計画書の現在の内容でタスクにチェックが入っていて、許されない変更が
// 無いことを確かめ、タスクを終えた記録を残す。現在の内容は検査を通っているため、
// 次のタスクの基準にする。
func (r *run) completeTask(ctx context.Context) (bool, error) {
	commit, ok, err := r.hub.Resolve(ctx, r.st.Branch)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("タスクのブランチ %s がハブにありません", r.st.Branch)
	}
	current, err := os.ReadFile(r.planFile())
	if err != nil {
		return false, fmt.Errorf("計画書を読めません: %w", err)
	}
	// ゲート検査のあとのレビューと回答のステップでも、計画書が変わっていないかを確かめる。
	if vs := plan.CheckTaskChanges(string(r.snapshot), string(current), r.st.Task); len(vs) > 0 {
		return r.stop(config.RoleDesigner, fmt.Sprintf("計画書 %s に、タスク %s の対応で許されない変更があります", r.p.PlanPath, r.st.Task), violationMessages(vs)...)
	}
	if stopped, err := r.checkTaskDone(current); err != nil || stopped {
		return stopped, err
	}
	if err := r.dir.SaveSnapshot(current); err != nil {
		return false, err
	}
	r.snapshot = current

	r.st.Tasks = append(r.st.Tasks, state.TaskRecord{ID: r.st.Task, Branch: r.st.Branch, Start: r.st.Start, Commit: commit, At: r.now()})
	r.logf("タスク %s を終えました (ブランチ %s)", r.st.Task, r.st.Branch)
	*r.st = history(r.st)
	return false, r.save()
}

// checkTaskDoneは、計画書の内容currentでタスクにチェックが入っているかを確かめる。
// チェックを入れるのは実装のスキルの手順なので、入っていなければ実装者を添えて止める。
func (r *run) checkTaskDone(current []byte) (bool, error) {
	p, err := plan.Parse(string(current))
	if err != nil {
		return r.stop(config.RoleDesigner, "計画書を読み取れません", err.Error())
	}
	if t, ok := findTask(p, r.st.Task); !ok || !t.Done {
		return r.stop(config.RoleImplementer, fmt.Sprintf("タスク %s のチェックボックスにチェックが入っていません", r.st.Task))
	}
	return false, nil
}

// historyは、stのうちタスクをまたいで残す記録だけを持つ状態を返す。
func history(st *state.State) state.State {
	return state.State{
		Tasks:         st.Tasks,
		Outputs:       st.Outputs,
		ReviewLog:     st.ReviewLog,
		Deferred:      st.Deferred,
		StopLog:       st.StopLog,
		ReportVersion: st.ReportVersion,
		Reports:       st.Reports,
		Verified:      st.Verified,
	}
}
