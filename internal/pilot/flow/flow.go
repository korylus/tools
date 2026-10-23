// Package flowは、作業計画書の未完了のタスクを先頭から順に、役割のコンテナでの
// 実装・コミット・ハブへの取り込み・ゲート検査・レビュー・回答・修正の流れで進める状態機械。
//
// 次に何をするかはすべてkoryluspilotが決め、エージェントに次の役割を呼ばせない。
// ステップの境目ごとに状態ファイルを書くため、プロセスが落ちても次のrunで同じ
// ステップから続けられる。停止条件に当たったら、理由と解消に使う役割を状態に
// 残して止め、次のrunで止まったステップからやり直す。
//
// 計画書の最後まで進んだら作業報告書を作り、次のrunで人間が付けた動作確認の結果を読む。
// NGがあれば、設計者の役割にフィードバック対応のフェーズを起票させ、そのタスクを
// 同じ流れで進めてから作業報告書の次の版を作る。
package flow

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/runner"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
	"github.com/korylus/tools/internal/plan"
)

// baseSuffixは、計画書の最初のタスクを切る元として固定するハブのブランチの接尾辞
// ({計画書名}-base)。タスク番号は数字とハイフンを含むため、タスクのブランチと重ならない。
const baseSuffix = "-base"

// Pilotは1つの計画書を進める実行の設定。
type Pilot struct {
	// PlanPathは、プロジェクトのルートからの計画書の相対パス。koryluspilotは
	// カレントディレクトリ (人間のチェックアウトのルート) からこのパスを読み、
	// 役割のコンテナのエージェントにも同じパスを渡す。
	PlanPath string
	// Rootは人間のチェックアウトのルート。計画書はRootからPlanPathで読む。空ならカレントディレクトリ。
	Root string
	// ConfigPathは設定ファイルのパス。読むのは基準の比較と取り直しのときだけで、
	// 実行にはスナップショットを使う。
	ConfigPath string
	// StateRootは状態ディレクトリを置く場所。
	StateRoot string
	// HubRootはハブと役割のクローンを置くディレクトリ。空ならhub.DefaultRootを使う。
	HubRoot string
	// Upstreamはbase_branchを取り込むリポジトリ。空なら設定のhub.upstreamを使う。
	// テストでローカルのリポジトリに差し替える。
	Upstream string
	// Noteは、止まったステップを再開するときにエージェントへ渡す補足。
	Note string
	// Approveは、現在の計画書と設定ファイルを新しい基準として受け入れるかどうか。
	Approve bool

	// RoleRunnerは役割の名前からランナーを返す。nilなら設定のrunnerを接頭辞にする。
	// テストでは、作業ディレクトリを役割のクローンにした偽のランナーに差し替える。
	RoleRunner func(name string) runner.Runner
	// ClaudeCommandとCodexCommandは起動するエージェントのコマンド。空ならツール名を使う。
	ClaudeCommand string
	CodexCommand  string
	// Fetcherは使用率を取得する。nilなら、各ツールを使う役割のランナーで取得する。
	Fetcher usage.Fetcher
	// Nowは現在時刻。nilならtime.Nowを使う。
	Now func() time.Time
	// Sleepはdだけ待つ。nilならctxが終わらない限りdだけ待つ。
	Sleep func(ctx context.Context, d time.Duration) error
	// Outは進捗の表示先。nilなら表示しない。
	Out io.Writer
}

// Outcomeは実行の結果。
type Outcome struct {
	State *state.State
	// Configは実行に使った設定 (スナップショット)。設定を読む前に失敗したときはnil。
	Config *config.Config
	// Reportは、計画書の最後まで進んだときの、人間の確認を待つ作業報告書のパス。
	// 作業報告書を作らずに終えたときと、確認済みとして終えたときは空文字。
	Report string
}

// Stoppedは停止条件で止まったかを返す。
func (o *Outcome) Stopped() bool {
	return o.State != nil && o.State.Stop != nil
}

// run は1回の実行の作業領域。
type run struct {
	p        *Pilot
	cfg      *config.Config
	dir      state.Dir
	hub      hub.Hub
	st       *state.State
	planName string
	// snapshotは計画書のスナップショット (人間が合意した基準)。
	snapshot []byte
	tracker  usage.Tracker
	watcher  gate.OutsideWatcher
	// noteは、止まったステップを再開するエージェントにまだ渡していない補足。
	note string
	// reportPathは、人間の確認を待つ作業報告書のパス。
	reportPath string
}

// Runは計画書の未完了のタスクを、計画書の最後か停止条件に当たるまで進める。
// 停止条件で止まったときはエラーを返さず、Outcome.Stoppedがtrueになる。
func (p *Pilot) Run(ctx context.Context) (*Outcome, error) {
	r := &run{p: p, planName: plan.Name(p.PlanPath)}
	r.dir = state.NewDir(p.StateRoot, r.planName)
	out := &Outcome{}

	st, err := r.dir.Load()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		st = &state.State{}
	case err != nil:
		return out, err
	}
	r.st = st
	out.State = st

	if st.Stop != nil {
		// 止まってから再開するまでを、人間の待ちとして記録する。
		r.event(state.Event{Kind: state.EventHumanWait, Task: st.Task, Step: st.Step, Role: st.Stop.Role, Started: st.Stop.At, Ended: r.now()})
		if n := len(st.StopLog); n > 0 && st.StopLog[n-1].ResumedAt.IsZero() {
			st.StopLog[n-1].ResumedAt = r.now()
		}
		// レビューと回答の再開では、人間がその役割のクローンで直した変更を先に検査する。
		// ゲートからの戻り先は、取り込みのあとに落ちても復元できるよう状態に残す。
		if (st.Step == state.StepReview || st.Step == state.StepAnswer) &&
			st.Stop.Role == st.Role && st.Sessions[state.StepReview] != "" {
			st.ResumeStep = st.Step
			st.Step = state.StepGate
		}
		st.Stop = nil
		r.note = p.Note
	} else if p.Note != "" {
		r.logf("警告: 止まったステップが無いため、補足 (-note) は使いません")
	}

	stopped, err := r.loadBaselines()
	out.Config = r.cfg
	if err != nil || stopped {
		return out, err
	}

	if err := r.setup(ctx); err != nil {
		return out, err
	}
	for {
		// フィードバックの起票と承認は、タスクに属さないステップとして進める。
		if st.Step == "" {
			done, err := r.selectTask(ctx)
			if err != nil {
				return out, err
			}
			if done {
				r.warnUnusedNote()
				if err := r.save(); err != nil {
					return out, err
				}
				feedback, err := r.finishPlan(ctx)
				if err != nil || !feedback {
					out.Report = r.reportPath
					return out, err
				}
				continue
			}
		}
		resumeStep := st.ResumeStep
		stopped, err := r.step(ctx)
		if err != nil || stopped {
			return out, err
		}
		if resumeStep != "" && st.Step == resumeStep {
			continue
		}
		// 補足はエージェントのステップだけが受け取る。再開したステップが
		// エージェントを使わなかったときに、後続のステップへ持ち越さない。
		r.warnUnusedNote()
	}
}

// loadBaselinesは計画書と設定ファイルのスナップショットを読み、現在の内容と比べる。
// 初めての実行と-approveのときは、現在の内容をスナップショットとして取り直す。
// 合意した基準から変わっていれば、設計者の役割を添えて止める。
func (r *run) loadBaselines() (stopped bool, err error) {
	cfgCur, err := os.ReadFile(r.p.ConfigPath)
	if err != nil {
		return false, fmt.Errorf("設定ファイルを読めません: %w", err)
	}
	cfgSnap, err := r.takeBaseline(cfgCur, r.dir.LoadConfigSnapshot, r.dir.SaveConfigSnapshot, func(src []byte) error {
		_, err := config.Parse(string(src))
		return err
	})
	if err != nil {
		return false, fmt.Errorf("設定ファイル %s: %w", r.p.ConfigPath, err)
	}
	cfg, err := config.Parse(string(cfgSnap))
	if err != nil {
		return false, fmt.Errorf("設定ファイルのスナップショットが不正です:\n%w", err)
	}
	r.cfg = cfg

	planCur, err := os.ReadFile(r.planFile())
	if err != nil {
		return false, fmt.Errorf("計画書を読めません: %w", err)
	}
	r.snapshot, err = r.takeBaseline(planCur, r.dir.LoadSnapshot, r.dir.SaveSnapshot, func(src []byte) error {
		_, err := plan.Parse(string(src))
		return err
	})
	if err != nil {
		return false, fmt.Errorf("計画書 %s: %w", r.p.PlanPath, err)
	}

	if !bytes.Equal(cfgSnap, cfgCur) {
		return r.stop(config.RoleDesigner, fmt.Sprintf("設定ファイル %s が、合意した基準 (スナップショット) から変わっています。内容を確かめて -approve で受け入れてください", r.p.ConfigPath))
	}
	// 実行中のタスクがあれば、そのタスクの対応で許される変更 (チェックと実装判断ログ) を除いて比べる。
	if vs := plan.CheckTaskChanges(string(r.snapshot), string(planCur), r.st.Task); len(vs) > 0 {
		return r.stop(config.RoleDesigner, fmt.Sprintf("計画書 %s が、合意した基準 (スナップショット) から変わっています。内容を確かめて -approve で受け入れてください", r.p.PlanPath), violationMessages(vs)...)
	}
	return false, nil
}

// takeBaselineはスナップショットを読む。まだ無いか-approveのときは、validateで
// 確かめてから現在の内容curをスナップショットとして書く。
func (r *run) takeBaseline(cur []byte, load func() ([]byte, error), save func([]byte) error, validate func([]byte) error) ([]byte, error) {
	snap, err := load()
	if err == nil && !r.p.Approve {
		return snap, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := validate(cur); err != nil {
		return nil, err
	}
	if err := save(cur); err != nil {
		return nil, err
	}
	return cur, nil
}

// setupはハブと役割のクローンのディレクトリ、使用率の記録、リポジトリの外の監視を用意する。
func (r *run) setup(ctx context.Context) error {
	root := r.p.HubRoot
	if root == "" {
		var err error
		if root, err = hub.DefaultRoot(r.cfg.Project); err != nil {
			return err
		}
	}
	r.hub = hub.Hub{Root: root, Path: r.cfg.Hub.Path}
	if err := r.hub.Init(ctx, slices.Sorted(maps.Keys(r.cfg.Roles))); err != nil {
		return err
	}

	fetcher := r.p.Fetcher
	if fetcher == nil {
		var f usage.CommandFetcher
		if role, ok := r.cfg.RoleUsing(config.ToolClaude); ok {
			f.Claude = runner.Runner{Prefix: role.Runner}
		}
		if role, ok := r.cfg.RoleUsing(config.ToolCodex); ok {
			f.Codex = runner.Runner{Prefix: role.Runner}
		}
		fetcher = f
	}
	// 使用率の記録は計画書をまたいで使うため、状態ディレクトリの直下に置く。
	r.tracker = usage.Tracker{Log: usage.NewLog(r.p.StateRoot), Fetcher: fetcher, Now: r.now}
	r.watcher = gate.OutsideWatcher{Watches: r.cfg.Gate.Watch}
	return nil
}

// selectTaskは、スナップショットの最初の未完了のタスクを実行中のタスクにする。
// 未完了のタスクが無ければdoneを返す。最初のタスクは、GitHubから取り込んだ
// base_branchを固定したブランチから、2つ目以降は前のタスクのブランチから切る。
func (r *run) selectTask(ctx context.Context) (done bool, err error) {
	p, err := plan.Parse(string(r.snapshot))
	if err != nil {
		return false, err
	}
	next, ok := p.Next()
	if !ok {
		return true, nil
	}

	var start string
	if n := len(r.st.Tasks); n > 0 {
		start = r.st.Tasks[n-1].Branch
	} else {
		upstream := cmp.Or(r.p.Upstream, r.cfg.Hub.Upstream)
		if err := r.hub.FetchUpstream(ctx, upstream, r.cfg.BaseBranch); err != nil {
			return false, err
		}
		start = r.planName + baseSuffix
		if _, err := r.hub.Pin(ctx, start, r.cfg.BaseBranch); err != nil {
			return false, err
		}
	}

	*r.st = history(r.st)
	r.st.Task = next.ID
	r.st.Step = state.StepBranch
	r.st.Branch = r.planName + "-" + next.ID
	r.st.Start = start
	r.st.Role = config.RoleImplementer
	r.logf("タスク %s を始めます (ブランチ %s、元のブランチ %s)", next.ID, r.st.Branch, start)
	return false, r.save()
}

// stepは実行中のステップを1つ進める。
func (r *run) step(ctx context.Context) (stopped bool, err error) {
	switch r.st.Step {
	case state.StepBranch:
		return r.branch(ctx)
	case state.StepImplement:
		return r.implement(ctx)
	case state.StepCommit:
		return r.commit(ctx)
	case state.StepGate:
		return r.gate(ctx)
	case state.StepRework:
		return r.rework(ctx)
	case state.StepReview:
		return r.review(ctx)
	case state.StepAnswer:
		return r.answer(ctx)
	case state.StepFix:
		return r.fix(ctx)
	case state.StepFeedback:
		return r.feedback(ctx)
	case state.StepApprove:
		return r.approveFeedback()
	}
	return false, fmt.Errorf("状態のステップ %q は、この版のkoryluspilotでは扱えません", r.st.Step)
}

// advanceは次のステップへ進めて状態を書く。
func (r *run) advance(next state.Step) (bool, error) {
	r.st.Step = next
	return false, r.save()
}

// stopは停止条件に当たったことを状態に残す。ステップは止まったステップのままにし、
// 次のrunでそこからやり直す。roleは原因の解消に使う役割。
func (r *run) stop(role, reason string, details ...string) (bool, error) {
	r.st.Stop = &state.Stop{Reason: reason, Details: details, Role: role, At: r.now()}
	r.st.StopLog = append(r.st.StopLog, state.StopRecord{Task: r.st.Task, Step: r.st.Step, Stop: *r.st.Stop})
	return true, r.save()
}

func (r *run) save() error {
	return r.dir.Save(r.st, r.now())
}

// takeNoteは、まだ渡していない補足を返して消す。
func (r *run) takeNote() string {
	note := r.note
	r.note = ""
	return note
}

func (r *run) warnUnusedNote() {
	if r.takeNote() != "" {
		r.logf("警告: 再開したステップがエージェントを使わなかったため、補足 (-note) は使いませんでした")
	}
}

// planFileはkoryluspilotが読む計画書のパスを返す。
func (r *run) planFile() string {
	return filepath.Join(r.p.Root, r.p.PlanPath)
}

// roleRunnerは役割nameのランナーを返す。
func (r *run) roleRunner(name string) runner.Runner {
	if r.p.RoleRunner != nil {
		return r.p.RoleRunner(name)
	}
	return runner.Runner{Prefix: r.cfg.Roles[name].Runner}
}

// eventは所要時間の記録に追記する。記録できなくても実行は止めない。
func (r *run) event(e state.Event) {
	if err := r.dir.AppendEvent(e); err != nil {
		r.logf("警告: 所要時間を記録できません: %v", err)
	}
}

func (r *run) now() time.Time {
	if r.p.Now != nil {
		return r.p.Now()
	}
	return time.Now()
}

func (r *run) sleep(ctx context.Context, d time.Duration) error {
	if r.p.Sleep != nil {
		return r.p.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *run) logf(format string, args ...any) {
	if r.p.Out != nil {
		fmt.Fprintf(r.p.Out, format+"\n", args...)
	}
}

// violationMessagesは計画書の許されない変更を、停止理由の詳細の形にする。
func violationMessages(vs []plan.Violation) []string {
	msgs := make([]string, 0, len(vs))
	for _, v := range vs {
		if v.LineNo > 0 {
			msgs = append(msgs, fmt.Sprintf("%d行目: %s", v.LineNo, v.Message))
			continue
		}
		msgs = append(msgs, v.Message)
	}
	return msgs
}

// escalationMessagesはエスカレーションを、停止理由の詳細の形にする。
func escalationMessages(res *agent.UnattendedResult) []string {
	msgs := make([]string, 0, len(res.Escalations))
	for _, e := range res.Escalations {
		msg := fmt.Sprintf("%s / 判断してほしいこと: %s", e.Reason, e.Question)
		if len(e.Options) > 0 {
			msg += " / 選択肢: " + strings.Join(e.Options, " | ")
		}
		if e.Recommended != "" {
			msg += " / 推奨: " + e.Recommended
		}
		msgs = append(msgs, msg)
	}
	return msgs
}
