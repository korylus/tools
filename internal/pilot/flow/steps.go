package flow

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// maxReworksは、ゲートのコマンドの失敗を直前のステップの役割に差し戻す回数の上限。
const maxReworks = 1

// branchは、実装者のクローンをハブの最新に合わせ、タスクのブランチに切り替える。
// ブランチがまだハブに無ければ、元のブランチから作る。
func (r *run) branch(ctx context.Context) (bool, error) {
	err := r.hub.PrepareClone(ctx, r.roleRunner(r.st.Role), r.st.Branch, r.st.Start)
	var rejected *hub.RejectedError
	if errors.As(err, &rejected) {
		return r.stop(r.st.Role, "役割のクローンを準備できません", rejected.Reason)
	}
	if err != nil {
		return false, err
	}
	return r.advance(state.StepImplement)
}

// implementは実装者に、無人実行モードの /korylus-do-plan-task でタスクを実装させる。
// 再開したときは同じセッションで、補足を添えて同じスキルを呼び直す。
func (r *run) implement(ctx context.Context) (bool, error) {
	args := fmt.Sprintf("%s %s base=%s mode=unattended", r.p.PlanPath, r.st.Task, cloneRef(r.st.Start))
	if note := r.takeNote(); note != "" {
		// note= はスキルの引数の最後に置き、末尾までを値として渡す。
		args += " note=" + note
	}
	role := r.st.Role
	res, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepImplement,
		role:    role,
		kind:    agent.Normal,
		prompt:  skillPrompt(r.tool(role), "korylus-do-plan-task", args),
		session: sessionStep(role),
		schema:  true,
		allowed: []string{r.p.PlanPath},
	})
	if err != nil || stopped {
		return stopped, err
	}
	return r.finishAgent(state.StepImplement, role, res, state.StepCommit)
}

// commitは、変更を持つ役割に、作業したセッションのまま /korylus-commit でコミットさせる。
func (r *run) commit(ctx context.Context) (bool, error) {
	role := r.st.Role
	prompt := skillPrompt(r.tool(role), "korylus-commit", "")
	if note := r.takeNote(); note != "" {
		prompt += "\n\n補足: " + note
	}
	_, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepCommit,
		role:    role,
		kind:    agent.Normal,
		prompt:  prompt,
		session: sessionStep(role),
	})
	if err != nil || stopped {
		return stopped, err
	}
	return r.advance(state.StepGate)
}

// reworkは、ゲートのコマンドの失敗を、変更を持つ役割のセッションに差し戻して直させる。
func (r *run) rework(ctx context.Context) (bool, error) {
	role := r.st.Role
	res, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepRework,
		role:    role,
		kind:    agent.Fix,
		prompt:  reworkPrompt(r.st.GateFailure, r.takeNote()),
		session: sessionStep(role),
		schema:  true,
		allowed: []string{r.p.PlanPath},
	})
	if err != nil || stopped {
		return stopped, err
	}
	return r.finishAgent(state.StepRework, role, res, state.StepCommit)
}

// finishAgentは保存済みの構造化出力をもとに、エスカレーションなら止め、完了ならnextへ進める。
func (r *run) finishAgent(step state.Step, role string, res *agent.UnattendedResult, next state.Step) (bool, error) {
	if res.Status == agent.StatusEscalated {
		return r.stop(role, fmt.Sprintf("%sのエージェントがエスカレーションを返しました: %s", step.Label(), res.Summary), escalationMessages(res)...)
	}
	return r.advance(next)
}

// gateは、変更を持つ役割のクローンからハブへ取り込み、取り込んだコミットに
// ゲート検査を行う。設定のコマンドの失敗は1回だけ差し戻し、それ以外の検知と
// 差し戻しのあとの失敗では止める。検査を通ったら、レビューへ進むかタスクを終える。
func (r *run) gate(ctx context.Context) (bool, error) {
	imp, err := r.hub.Import(ctx, r.roleRunner(r.st.Role), r.st.Branch, r.st.Start)
	var rejected *hub.RejectedError
	if errors.As(err, &rejected) {
		return r.stop(r.st.Role, "役割のクローンからハブへ取り込めません", rejected.Reason)
	}
	if err != nil {
		return false, err
	}
	if r.st.ResumeStep != "" && imp.New == r.st.GateCommit {
		return r.resumeAfterGate()
	}
	base, ok, err := r.hub.Resolve(ctx, r.st.Start)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("元のブランチ %s がハブにありません", r.st.Start)
	}

	snap, err := plan.Parse(string(r.snapshot))
	if err != nil {
		return false, err
	}
	task, ok := findTask(snap, r.st.Task)
	if !ok {
		return r.stop(config.RoleDesigner, fmt.Sprintf("計画書のスナップショットにタスク %s がありません", r.st.Task))
	}
	current, err := os.ReadFile(r.planFile())
	if err != nil {
		return false, fmt.Errorf("計画書を読めません: %w", err)
	}

	in := gate.Input{Config: r.cfg, Task: task, PlanSnapshot: string(r.snapshot), PlanCurrent: string(current)}
	_, report, err := r.inspect(ctx, in, base, imp.New, nil)
	if err != nil {
		return false, err
	}
	if !report.Passed() {
		return r.stop(findingsRole(report.Findings, r.st.Role), "ゲート検査で止める変更を検知しました", findingMessages(report.Findings)...)
	}

	logDir, err := r.logPath("gate")
	if err != nil {
		return false, err
	}
	commands := gate.CommandRunner{Runner: r.cfg.Gate.Runner, Repo: r.cfg.Hub.Path}
	results, err := commands.Run(ctx, imp.New, gate.Commands(r.cfg, task), logDir)
	for _, res := range results {
		outcome := state.OutcomeOK
		if !res.Passed() {
			outcome = state.OutcomeFailed
		}
		r.event(state.Event{Kind: state.EventGate, Task: r.st.Task, Step: state.StepGate, Started: res.Started, Ended: res.Started.Add(res.Duration), Outcome: outcome, Command: res.Command, ExitCode: res.ExitCode})
	}
	if err != nil {
		return false, err
	}
	if n := len(results); n > 0 && !results[n-1].Passed() {
		failure := describeFailure(results[n-1])
		if r.st.Reworks < maxReworks {
			r.st.Reworks++
			r.st.GateFailure = failure
			r.logf("ゲートのコマンドが失敗したため、%s に差し戻します: %s", r.st.Role, results[n-1].Command)
			return r.advance(state.StepRework)
		}
		return r.stop(r.st.Role, "ゲートのコマンドが差し戻しのあとも失敗しました", failure)
	}
	// レビューの前に確かめ、チェックの漏れでレビューを無駄にしない。
	if stopped, err := r.checkTaskDone(current); err != nil || stopped {
		return stopped, err
	}
	// 差し戻しの回数はゲート検査ごとに数え、レビューのあとの修正にも1回差し戻せるようにする。
	r.st.Reworks, r.st.GateFailure = 0, ""
	r.st.Touched = declaredTouches(report)
	r.st.GateCommit = imp.New
	if r.st.ResumeStep != "" {
		return r.resumeAfterGate()
	}
	return r.reviewOrComplete(ctx)
}

// resumeAfterGateは、停止中の変更の検査を終え、止まっていたステップへ戻る。
func (r *run) resumeAfterGate() (bool, error) {
	next := r.st.ResumeStep
	r.st.ResumeStep = ""
	return r.advance(next)
}

// goChecksは、設定の保護領域が、Goの関数の検知と依存辺の検知を使うかを返す。
func goChecks(cfg *config.Config) (funcs, deps bool) {
	for _, p := range cfg.Protected {
		funcs = funcs || len(p.GoFuncsCalling) > 0
		deps = deps || p.GoImportPrefix != ""
	}
	return funcs, deps
}

// findingsRoleは検知の解消に使う役割を返す。計画書の変更だけなら計画書を直す
// 設計者、それ以外はコードを直す役割 (変更を持つ役割) にする。
func findingsRole(findings []gate.Finding, codeRole string) string {
	for _, f := range findings {
		if f.Kind != gate.KindPlan {
			return codeRole
		}
	}
	return config.RoleDesigner
}

func findingMessages(findings []gate.Finding) []string {
	msgs := make([]string, 0, len(findings))
	for _, f := range findings {
		msgs = append(msgs, f.Message)
	}
	return msgs
}

// describeFailureは、失敗したゲートのコマンドを、差し戻しのプロンプトと停止理由に載せる形にする。
func describeFailure(res gate.CommandResult) string {
	status := fmt.Sprintf("終了コード %d", res.ExitCode)
	if res.TimedOut {
		status = "タイムアウト"
	}
	return fmt.Sprintf("コマンド: %s (%s、ログ: %s)\n出力の末尾:\n%s", res.Command, status, res.LogPath, res.OutputTail)
}

func findTask(p *plan.Plan, id string) (plan.Task, bool) {
	for _, t := range p.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return plan.Task{}, false
}

// sessionStepは、役割roleが作業しているセッションを記録するステップを返す。
// コミットと差し戻しは、変更を作ったセッションを再開して依頼する。
func sessionStep(role string) state.Step {
	if role == config.RoleImplementer {
		return state.StepImplement
	}
	return state.StepReview
}

// cloneRefは、役割のクローンの中でハブのブランチbranchを指す参照を返す。
// クローンはハブのブランチを refs/remotes/hub/ の下に取り込み、手元のブランチは
// タスクのブランチしか作らない。
func cloneRef(branch string) string {
	return "hub/" + branch
}

func (r *run) tool(role string) agent.Tool {
	return agent.Tool(r.cfg.Roles[role].Tool)
}
