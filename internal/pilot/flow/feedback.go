package flow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// readVerificationは、最新の版の作業報告書の動作確認の結果を読み、未確認の項目が
// 残っていれば確認を待ち、すべてOKで全体へのコメントも無ければ計画書を確認済みとして
// 終える。NGの項目か全体へのコメントがあれば、フィードバックの起票へ進めてtrueを返す。
func (r *run) readVerification(dir string) (bool, error) {
	st := r.st
	v := st.ReportVersion
	if st.Verified == v {
		r.logf("作業報告書 第%d版 ですべての確認項目がOKになり、計画書を確認済みとして終えています", v)
		return false, nil
	}
	r.reportPath = report.HTMLPath(dir, v)
	rep, err := report.Load(dir, v)
	if err != nil {
		return false, err
	}
	res, err := report.LoadVerification(dir, v)
	if errors.Is(err, fs.ErrNotExist) {
		r.logf("作業報告書 第%d版 の確認を待っています: %s", v, r.reportPath)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pending, ng := report.Classify(rep, res)
	if len(pending) > 0 {
		r.logf("作業報告書 第%d版 に未確認の項目が%d件残っています。確認して保存してから、もう一度実行してください: %s", v, len(pending), r.reportPath)
		return false, nil
	}

	// 作業報告書を作ってから、確認を終えた最後の保存までを人間の待ちとして記録する。
	// 確認の途中の保存は数えないよう、分岐が決まったこの時点で1回だけ記録する。
	r.event(state.Event{Kind: state.EventHumanWait, Step: state.StepReport, Started: st.Reports[len(st.Reports)-1].At, Ended: res.SavedAt})
	if len(ng) == 0 && strings.TrimSpace(res.Comment) == "" {
		st.Verified = v
		r.reportPath = ""
		r.logf("作業報告書 第%d版 ですべての確認項目がOKになりました。計画書を確認済みとして終えます", v)
		return false, r.save()
	}
	r.logf("作業報告書 第%d版 にNGの項目が%d件あります (全体へのコメント: %s)。フィードバック対応のフェーズを起票します", v, len(ng), presence(res.Comment))
	st.Step = state.StepFeedback
	st.Role = config.RoleDesigner
	return true, r.save()
}

// feedbackは、設計者に無人実行モードの`/korylus-add-feedback-phase`で、最新の版の
// 動作確認の結果からフェーズfbNを起票させる。起票のあとは計画書の差分が
// タスクリスト末尾へのフェーズfbNの追加だけかを確かめ、保護領域を宣言するタスクが
// あれば人間の承認を求めて止める。無ければスナップショットを取り直して実行へ進む。
func (r *run) feedback(ctx context.Context) (bool, error) {
	role := config.RoleDesigner
	snap, err := plan.Parse(string(r.snapshot))
	if err != nil {
		return false, err
	}
	if _, ok := snap.Next(); ok {
		// 未完了のタスクがあるだけでは、人間の承認を受けたことにはならない。
		// 中断前のスナップショットに保護領域の宣言があれば、明示的な承認を求める。
		if !r.p.Approve {
			for _, t := range snap.Tasks {
				if !t.Done && len(t.Protected) > 0 {
					r.st.Step = state.StepApprove
					return r.stop(role, "フィードバック対応のフェーズの実行には、-approve での承認が要ります")
				}
			}
		}
		r.logf("計画書に未完了のタスクがあるため、フィードバックを起票せずに進めます")
		return r.startFeedbackTasks()
	}

	if r.st.Sessions[state.StepFeedback] == "" {
		// 新しく起票するときだけ、設計者のクローンを最後のタスクのブランチに合わせ、
		// 実装されたコードを読めるようにする。再開したセッションは同じクローンで続けさせる。
		last := r.st.Tasks[len(r.st.Tasks)-1]
		err := r.hub.PrepareClone(ctx, r.roleRunner(role), last.Branch, last.Start)
		var rejected *hub.RejectedError
		if errors.As(err, &rejected) {
			return r.stop(role, "役割のクローンを準備できません", rejected.Reason)
		}
		if err != nil {
			return false, err
		}
	}

	round := snap.NextFeedbackRound()
	verification := report.VerificationPath(path.Join(report.FilesDir, r.planName), r.st.ReportVersion)
	prompt := skillPrompt(r.tool(role), "korylus-add-feedback-phase", r.p.PlanPath+" "+verification+" mode=unattended")
	if note := r.takeNote(); note != "" {
		prompt += "\n\n補足: " + note
	}
	r.logf("作業報告書 第%d版 の動作確認の結果から、フェーズ%s を起票します", r.st.ReportVersion, feedbackPhase(round))
	res, stopped, err := r.runAgent(ctx, agentStep{
		step:    state.StepFeedback,
		role:    role,
		kind:    agent.Normal,
		prompt:  prompt,
		session: state.StepFeedback,
		schema:  true,
		allowed: []string{r.p.PlanPath},
	})
	if err != nil || stopped {
		return stopped, err
	}
	if res.Status == agent.StatusEscalated {
		return r.stop(role, fmt.Sprintf("フィードバックの起票のエージェントがエスカレーションを返しました: %s", res.Summary),
			append(escalationMessages(res), "合意事項を直したときは -approve を付けて再開してください。計画書にタスクを足して受け入れたときは、起票せずにそのタスクへ進みます")...)
	}

	current, err := os.ReadFile(r.planFile())
	if err != nil {
		return false, fmt.Errorf("計画書を読めません: %w", err)
	}
	added, vs := plan.CheckFeedbackPhase(string(r.snapshot), string(current))
	if len(vs) > 0 {
		return r.stop(role, fmt.Sprintf("計画書 %s に、フェーズ%s の追加以外の変更があります", r.p.PlanPath, feedbackPhase(round)), violationMessages(vs)...)
	}
	return r.acceptFeedback(current, added, round)
}

// acceptFeedbackは、承認待ちを先に永続化してから計画書の基準を更新する。
// 保存の間で中断しても、保護領域を宣言したタスクを承認なしで実行しない。
func (r *run) acceptFeedback(current []byte, added []plan.Task, round int) (bool, error) {
	var declared []string
	for _, t := range added {
		if len(t.Protected) == 0 {
			continue
		}
		names := make([]string, 0, len(t.Protected))
		for _, d := range t.Protected {
			names = append(names, d.Name)
		}
		declared = append(declared, fmt.Sprintf("タスク %s %s: %s", t.ID, t.Title, strings.Join(names, ", ")))
	}
	if len(declared) > 0 {
		// エージェントが自分で保護領域を宣言してゲートを素通りさせないよう、人間に承認を求める。
		r.st.Step = state.StepApprove
		if _, err := r.stop(config.RoleDesigner, fmt.Sprintf("フェーズ%s に保護領域を宣言するタスクがあります。内容を確かめて -approve で承認してください", feedbackPhase(round)), declared...); err != nil {
			return false, err
		}
	}
	if err := r.dir.SaveSnapshot(current); err != nil {
		return false, err
	}
	r.snapshot = current
	r.logf("フェーズ%s に%d件のタスクを起票しました", feedbackPhase(round), len(added))
	if len(declared) > 0 {
		return true, nil
	}
	return r.startFeedbackTasks()
}

// approveFeedbackは、保護領域を宣言したフェーズfbNを人間が-approveで承認したら、
// そのタスクの実行へ進む。承認されていなければ、もう一度止める。
func (r *run) approveFeedback() (bool, error) {
	if !r.p.Approve {
		return r.stop(config.RoleDesigner, "フィードバック対応のフェーズの実行には、-approve での承認が要ります")
	}
	r.logf("承認されたフィードバック対応のフェーズのタスクを実行します")
	return r.startFeedbackTasks()
}

// startFeedbackTasksは、フィードバックの起票を終え、計画書のスナップショットの
// 未完了のタスク (フェーズfbN) の選択へ戻る。
func (r *run) startFeedbackTasks() (bool, error) {
	*r.st = history(r.st)
	return false, r.save()
}

// feedbackPhaseはフィードバック対応の回roundのフェーズ番号 (fbN) を返す。
func feedbackPhase(round int) string {
	return fmt.Sprintf("fb%d", round)
}

// presenceは、全体へのコメントの有無を表示用に返す。
func presence(comment string) string {
	if strings.TrimSpace(comment) == "" {
		return "なし"
	}
	return "あり"
}
