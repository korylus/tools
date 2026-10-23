// Package flowは、作業計画書のタスクを役割ごとのセッションで進めるときの状態遷移を扱う。
//
// 役割のセッションはWaitで自分の番を待って指示を受け取り、指示どおりに作業してから
// Doneで完了を記録する。次のステップと番の役割はDoneとWaitが決め、エージェントには
// 決めさせない。状態の読み取りから次のステップの判定・ブランチの準備・書き込みまでは、
// 1つのファイルロックの中で行う。計画書の最後まで進んだら、作業報告書を作る。
package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/plan"
)

// defaultPollIntervalは、Waitが番の確認のために状態ファイルを読み直す間隔。
const defaultPollInterval = 2 * time.Second

// excludedPathsは、未コミットの変更に数えないパス (リポジトリのルートからの相対パス)。
// 計画書・レビュードキュメント・状態ファイルの更新はタスクの変更ではなく、ループの
// 中ではコミットしないため。
var excludedPaths = []string{"docs/private"}

// RejectedErrorは、Doneが完了の記録を受け付けなかったときのエラー。
// 役割のセッションは理由を読んで直し、Doneをやり直す。
type RejectedError struct {
	Reasons []string
}

func (e *RejectedError) Error() string {
	return "完了の記録を受け付けませんでした:\n- " + strings.Join(e.Reasons, "\n- ")
}

// rejectは理由を1つ持つ*RejectedErrorを返す。
func reject(format string, args ...any) error {
	return &RejectedError{Reasons: []string{fmt.Sprintf(format, args...)}}
}

// Flowは1つの計画書の状態遷移。
type Flow struct {
	// PlanPathはプロジェクトのルートからの計画書のパス。指示の中のコマンドにもそのまま使う。
	PlanPath string
	// Rootはプロジェクトのルート。レビュードキュメントの置き場所と、本体のリポジトリを
	// 求めるのに使う。
	Root string
	Dir  state.Dir
	Now  func() time.Time
	// PollIntervalは、Waitが番の確認のために状態ファイルを読み直す間隔。
	PollInterval time.Duration
}

// Newは計画書planPathの状態遷移を、stateRootの下の状態ディレクトリで扱うFlowを返す。
// プロジェクトのルートは作業ディレクトリとする。
func New(planPath, stateRoot string) *Flow {
	return &Flow{
		PlanPath:     planPath,
		Root:         ".",
		Dir:          state.NewDir(stateRoot, plan.Name(planPath)),
		Now:          time.Now,
		PollInterval: defaultPollInterval,
	}
}

// Waitは役割roleの番が来るまで待ち、次の指示を返す。timeoutが正で、それを過ぎても
// 番が来なければActionWaitの指示を返す。待っている間はロックを持たない。
func (f *Flow) Wait(ctx context.Context, role state.Role, timeout time.Duration) (Instruction, error) {
	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		in, ok, err := f.poll(ctx, waitCtx, role)
		if err := ctx.Err(); err != nil {
			return Instruction{}, err
		}
		if timeout > 0 && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return Instruction{Action: ActionWait, Role: role, PlanPath: f.PlanPath}, nil
		}
		if err != nil || ok {
			return in, err
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(f.PollInterval):
		}
	}
}

// pollは状態を1回読み、役割roleの番なら指示を返す。進めているステップが無ければ、
// 計画書から次のステップを決めて書き込む。
// lockCtxはロックを取るまでの待ちにだけ使い、ロックを取ったあとはctxで進める。
// waitの期限で、ブランチの準備の途中のgitを止めないためである。
func (f *Flow) poll(ctx, lockCtx context.Context, role state.Role) (Instruction, bool, error) {
	unlock, err := f.Dir.LockContext(lockCtx)
	if err != nil {
		return Instruction{}, false, err
	}
	defer func() { _ = unlock() }()

	st, err := f.load()
	if err != nil {
		return Instruction{}, false, err
	}
	p, err := f.readPlan()
	if err != nil {
		return Instruction{}, false, err
	}
	if st.Pending != nil {
		if err := f.resume(ctx, st, p); err != nil {
			return Instruction{}, false, err
		}
	} else if st.Task == "" {
		f.advance(ctx, st, p)
		if err := ctx.Err(); err != nil {
			return Instruction{}, false, err
		}
		if st.Task == "" {
			if err := f.finishPlan(ctx, st); err != nil {
				return Instruction{}, false, err
			}
		}
		if err := f.Dir.Save(st, f.Now()); err != nil {
			return Instruction{}, false, err
		}
	}

	if st.Task == "" {
		return Instruction{Action: ActionFinish, Role: role, PlanPath: f.PlanPath, Report: f.latestReport(st)}, true, nil
	}
	if st.Role != role {
		return Instruction{}, false, nil
	}
	in := Instruction{Role: role, PlanPath: f.PlanPath, Task: st.Task, Record: st.TaskRecord(st.Task), Ask: st.Ask}
	if t, ok := findTask(p, st.Task); ok {
		in.Title = t.Title
	}
	if rec := in.Record; rec != nil && len(rec.Reviews) > 0 {
		last := rec.Reviews[len(rec.Reviews)-1]
		in.ReviewNumber, in.ReviewDoc = last.Number, last.Doc
	}
	switch st.Step {
	case state.StepImplement:
		in.Action = ActionImplement
	case state.StepReview:
		in.Action = ActionReview
		in.ReviewNumber, in.ReviewDoc = len(in.Record.Reviews)+1, ""
	case state.StepFix:
		in.Action = ActionFix
	case state.StepAsk:
		in.Action = ActionAsk
	default:
		return Instruction{}, false, fmt.Errorf("ステップ %s の指示には対応していません", st.Step)
	}
	return in, true, nil
}

// Doneは役割roleが進めているステップの完了を記録し、次のステップを決めて書き込む。
// 完了の条件を満たさなければ、何も書き込まずに*RejectedErrorを返す。
// 実装のステップでは、inputから動作確認の項目をJSONの配列で読む。
// 戻り値は、次のステップを伝える文面。
func (f *Flow) Done(ctx context.Context, role state.Role, input io.Reader) (string, error) {
	unlock, err := f.Dir.LockContext(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = unlock() }()

	st, err := f.Dir.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return "", reject("まだループを始めていません。先に korylusflow wait を実行してください")
	}
	if err != nil {
		return "", err
	}
	if st.Pending != nil {
		if st.Pending.Role != role {
			return "", reject("%s の番ではありません (完了の記録は %s の番です)", role, st.Pending.Role)
		}
		p, err := f.readPlan()
		if err != nil {
			return "", err
		}
		if err := f.resume(ctx, st, p); err != nil {
			return "", err
		}
		return f.completionMessage(st), nil
	}
	if st.Task == "" {
		return "", reject("完了を記録するステップがありません")
	}
	if st.Role != role {
		return "", reject("%s の番ではありません (タスク %s の%sは %s の番です)", role, st.Task, st.Step.Label(), st.Role)
	}
	p, err := f.readPlan()
	if err != nil {
		return "", err
	}

	now := f.Now()
	event := state.Event{Task: st.Task, Step: st.Step, Role: st.Role, Started: st.Since, Ended: now}
	if st.Step == state.StepAsk && st.Ask != nil {
		event.Reason = st.Ask.Reason
	}
	rec := st.TaskRecord(st.Task)
	if rec == nil && (st.Step == state.StepReview || st.Step == state.StepFix) {
		return "", fmt.Errorf("タスク %s の記録がありません", st.Task)
	}

	// 各ステップの完了を確かめ、次のステップを決める。条件を満たさなければ、
	// 状態を書き込まずに*RejectedErrorを返す。
	switch st.Step {
	case state.StepImplement:
		if err := f.checkImplement(ctx, st, p, input); err != nil {
			return "", err
		}
		if err := f.startReview(ctx, st, rec, now); err != nil {
			return "", err
		}
	case state.StepReview:
		if err := f.finishReview(ctx, st, rec, now); err != nil {
			return "", err
		}
	case state.StepFix:
		if err := checkFix(ctx, rec); err != nil {
			return "", err
		}
		if err := f.afterFix(ctx, st, rec, now); err != nil {
			return "", err
		}
	case state.StepAsk:
		if err := f.finishAsk(ctx, st, p, rec, now); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("ステップ %s の完了の記録には対応していません", st.Step)
	}

	// 完了は次のブランチへ切り替える前に残し、完了条件と動作確認の
	// 項目を読み直さずに、途中から再開できるようにする。
	st.Pending = &event
	if err := f.Dir.Save(st, now); err != nil {
		return "", err
	}
	if err := f.resume(ctx, st, p); err != nil {
		return "", err
	}
	return f.completionMessage(st), nil
}

// finishAskは人への確認の完了を、尋ねた場面ごとの条件で確かめ、次のステップを決める。
// recは進めているタスクの記録で、人が行うタスクではnil。
func (f *Flow) finishAsk(ctx context.Context, st *state.State, p *plan.Plan, rec *state.TaskRecord, now time.Time) error {
	if st.Ask == nil {
		return fmt.Errorf("タスク %s で人に尋ねている理由がありません", st.Task)
	}
	switch st.Ask.Kind {
	case state.AskHumanTask:
		if t, ok := findTask(p, st.Task); !ok || !t.Done {
			return reject("計画書のタスク %s にチェックが付いていません", st.Task)
		}
		clearStep(st)
	case state.AskBranch:
		// 準備できない間は完了を受け付けない。途中で落ちても元のaskから
		// 準備をやり直せるため、この場面だけは保存の前に準備を確かめる。
		prevTask := st.Task
		clearStep(st)
		f.advance(ctx, st, p)
		if err := ctx.Err(); err != nil {
			return err
		}
		if st.Task == prevTask && st.Ask != nil && st.Ask.Kind == state.AskBranch {
			return &RejectedError{Reasons: append([]string{"まだブランチを準備できません"}, st.Ask.Details...)}
		}
	case state.AskReview, state.AskReviewLimit:
		if rec == nil || len(rec.Reviews) == 0 {
			return fmt.Errorf("タスク %s のレビューの記録がありません", st.Task)
		}
		d, err := f.readReview(rec.Reviews[len(rec.Reviews)-1].Doc)
		if err != nil {
			return err
		}
		wasReviewAsk := st.Ask.Kind == state.AskReview
		if err := f.answer(ctx, st, rec, d, now); err != nil {
			return err
		}
		// 上限で尋ねたあとは、続けて要人間判断の指摘を尋ねてよい。
		if wasReviewAsk && st.Step == state.StepAsk {
			return &RejectedError{Reasons: append([]string{"まだ対応方針に回答の無い指摘があります"}, st.Ask.Details...)}
		}
	default:
		return fmt.Errorf("人に尋ねる場面 %s の完了の記録には対応していません", st.Ask.Kind)
	}
	return nil
}

// resumeは完了を確認済みのステップの履歴と、次のステップを記録する。
// 保存に失敗してもPendingが残り、履歴の追記とブランチの準備をやり直せる。
func (f *Flow) resume(ctx context.Context, st *state.State, p *plan.Plan) error {
	if err := f.Dir.AppendEvent(*st.Pending); err != nil {
		return err
	}
	if st.Task == "" {
		f.advance(ctx, st, p)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if st.Task == "" {
		if err := f.finishPlan(ctx, st); err != nil {
			return err
		}
	}
	st.Pending = nil
	return f.Dir.Save(st, f.Now())
}

// completionMessageは、完了の記録後に次のステップを伝える文面を返す。
func (f *Flow) completionMessage(st *state.State) string {
	if st.Task == "" {
		if path := f.latestReport(st); path != "" {
			return fmt.Sprintf("記録しました。計画書の最後まで進みました。作業報告書: %s", path)
		}
		return "記録しました。計画書の最後まで進みました。"
	}
	return fmt.Sprintf("記録しました。次はタスク %s の%sで、%s の番です。", st.Task, st.Step.Label(), st.Role)
}

// checkImplementは実装の完了の条件を確かめ、満たしていれば動作確認の項目を記録に入れる。
func (f *Flow) checkImplement(ctx context.Context, st *state.State, p *plan.Plan, input io.Reader) error {
	rec := st.TaskRecord(st.Task)
	if rec == nil {
		return fmt.Errorf("タスク %s の記録がありません", st.Task)
	}

	var reasons []string
	cur, err := branch.Current(ctx, rec.Repo)
	if err != nil {
		return err
	}
	if cur != rec.Branch {
		reasons = append(reasons, fmt.Sprintf("%s がタスクのブランチ %s ではなく %s にいます", rec.Repo, rec.Branch, cur))
	} else {
		n, err := branch.Ahead(ctx, rec.Repo, rec.Base)
		if err != nil {
			return err
		}
		if n == 0 {
			reasons = append(reasons, fmt.Sprintf("ブランチ %s に、ベース %s より先のコミットがありません", rec.Branch, rec.Base))
		}
	}
	files, err := branch.Dirty(ctx, rec.Repo, excludedPaths)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		reasons = append(reasons, fmt.Sprintf("%s に未コミットの変更があります: %s", rec.Repo, strings.Join(files, ", ")))
	}
	if t, ok := findTask(p, st.Task); !ok || !t.Done {
		reasons = append(reasons, fmt.Sprintf("計画書のタスク %s にチェックが付いていません", st.Task))
	}
	items, err := parseVerification(input)
	if err != nil {
		reasons = append(reasons, err.Error())
	}

	if len(reasons) > 0 {
		return &RejectedError{Reasons: reasons}
	}
	rec.Verification = items
	return nil
}

// advanceは、進めているステップが無い状態stに、計画書pの最初の未完了のタスクの
// 最初のステップを入れる。すべて完了していれば何もしない。
// ブランチを準備できないときは、実装者が人に尋ねるステップにする。
func (f *Flow) advance(ctx context.Context, st *state.State, p *plan.Plan) {
	t, ok := p.Next()
	if !ok {
		return
	}
	st.Task, st.Role, st.Since = t.ID, state.RoleImplementer, f.Now()

	target, err := branch.Resolve(ctx, t.Platform)
	if err != nil {
		setAsk(st, state.AskBranch, "タスクのリポジトリを求められません", err.Error())
		return
	}
	if target.Human {
		removeTaskRecord(st, t.ID)
		setAsk(st, state.AskHumanTask, "人が行うタスクです", fmt.Sprintf("%s %s", t.ID, t.Title))
		return
	}

	rec := st.TaskRecord(t.ID)
	// 人が接頭辞を直したときは、前に求めた記録を捨てて求め直す。
	if rec != nil && rec.Dir != target.Dir {
		removeTaskRecord(st, t.ID)
		rec = nil
	}
	if rec == nil {
		base := previousBranch(st, target.Repo)
		if base == "" {
			if base, err = branch.DefaultBase(ctx, target.Repo); err != nil {
				setAsk(st, state.AskBranch, "ブランチを作る元のブランチを決められません", err.Error())
				return
			}
		}
		st.Tasks = append(st.Tasks, state.TaskRecord{
			ID: t.ID, Repo: target.Repo, Dir: target.Dir, Branch: plan.Name(f.PlanPath) + "-" + t.ID, Base: base,
		})
		rec = &st.Tasks[len(st.Tasks)-1]
	}

	if err := branch.Prepare(ctx, rec.Repo, rec.Branch, rec.Base, excludedPaths); err != nil {
		var dirty *branch.DirtyError
		if errors.As(err, &dirty) {
			setAsk(st, state.AskBranch, fmt.Sprintf("%s に未コミットの変更があるため、ブランチ %s を準備できません", rec.Repo, rec.Branch), dirty.Files...)
			return
		}
		setAsk(st, state.AskBranch, fmt.Sprintf("ブランチ %s を準備できません", rec.Branch), err.Error())
		return
	}
	rec.Prepared = true
	st.Step = state.StepImplement
}

// previousBranchは、同じリポジトリrepoで最後にブランチを準備できたタスクのブランチを返す。
// 無ければ空文字。
func previousBranch(st *state.State, repo string) string {
	for i := len(st.Tasks) - 1; i >= 0; i-- {
		if st.Tasks[i].Repo == repo && st.Tasks[i].Prepared {
			return st.Tasks[i].Branch
		}
	}
	return ""
}

// setAskは、状態stを実装者が人に尋ねるステップにする。
func setAsk(st *state.State, kind state.AskKind, reason string, details ...string) {
	st.Step = state.StepAsk
	st.Ask = &state.Ask{Kind: kind, Reason: reason, Details: details}
}

// setStepは、状態stを同じタスクの次のステップにする。
func setStep(st *state.State, step state.Step, role state.Role, now time.Time) {
	st.Step, st.Role, st.Since, st.Ask, st.ReviewBefore = step, role, now, nil, ""
}

// clearStepは、状態stから進めているステップを外す。
func clearStep(st *state.State) {
	st.Task, st.Step, st.Role, st.Since, st.Ask, st.ReviewBefore = "", "", "", time.Time{}, nil, ""
}

// removeTaskRecordは、状態stからタスク番号idの記録を除く。
func removeTaskRecord(st *state.State, id string) {
	for i := range st.Tasks {
		if st.Tasks[i].ID == id {
			st.Tasks = append(st.Tasks[:i], st.Tasks[i+1:]...)
			return
		}
	}
}

// findTaskは計画書pからタスク番号idのタスクを探す。
func findTask(p *plan.Plan, id string) (plan.Task, bool) {
	for _, t := range p.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return plan.Task{}, false
}

// loadは状態を読む。まだ状態が無ければ空の状態を返す。
func (f *Flow) load() (*state.State, error) {
	st, err := f.Dir.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return &state.State{}, nil
	}
	return st, err
}

// readPlanは計画書を読み直す。実行中に人が足したタスクやチェックを、次のタスクを選ぶときに反映する。
func (f *Flow) readPlan() (*plan.Plan, error) {
	src, err := os.ReadFile(f.PlanPath) //#nosec G304 G703 -- 人が指定した計画書を読む
	if err != nil {
		return nil, fmt.Errorf("計画書を読めません: %w", err)
	}
	p, err := plan.Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s を読み取れません:\n%w", f.PlanPath, err)
	}
	return p, nil
}

// parseVerificationは、動作確認の項目をJSONの配列で読む。項目が無い変更でも
// 書き忘れと区別できるよう、空の配列 [] を明示してもらう。
func parseVerification(r io.Reader) ([]state.VerificationItem, error) {
	const hint = "動作確認の項目を、標準入力からJSONの配列で渡してください (無ければ [])"
	if r == nil {
		return nil, errors.New(hint)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, errors.New(hint)
	}
	var items []state.VerificationItem
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, fmt.Errorf("動作確認の項目を読み取れません: %w", err)
	}
	if items == nil {
		return nil, errors.New(hint)
	}
	var problems []string
	for i, it := range items {
		if it.Kind != "screen" && it.Kind != "process" {
			problems = append(problems, fmt.Sprintf("%d件目のkindは screen か process にしてください (%q)", i+1, it.Kind))
		}
		if it.Title == "" || it.Target == "" || it.Expected == "" || len(it.Steps) == 0 {
			problems = append(problems, fmt.Sprintf("%d件目のtitle・target・steps・expectedをすべて書いてください", i+1))
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n- "))
	}
	return items, nil
}
