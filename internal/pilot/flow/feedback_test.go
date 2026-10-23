package flow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/plan"
)

// fb1は、第1版の確認項目#1に対応するフェーズfb1の起票の内容。protectedが空でなければ
// タスクに保護領域を宣言する。
func fb1(protected string) string {
	s := "### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n\n- [ ] **fb1-1**: [Go] 表示を直す\n  - **対応する確認項目**: 第1版 #1 「確認する」\n"
	if protected != "" {
		s += "  - **保護領域**: " + protected + "\n"
	}
	return s
}

// feedbackScenarioは、タスク1-1を終えて作業報告書の第1版を作ったあと、設計者が
// designerの振る舞いで起票し、フェーズfb1のタスクを実装・レビューする流れ。
func feedbackScenario(designer ...fakeAction) []fakeAction {
	s := []fakeAction{
		{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		{Commit: true},
		approve("p-1-1-001"),
	}
	s = append(s, designer...)
	return append(s,
		fakeAction{Files: map[string]string{"a.go": "package a\n\nvar fixed = true\n"}, CheckTask: "fb1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-fb1-1-001"),
	)
}

// writeVerificationは、作業報告書の版vの動作確認の結果を、人間が保存したものとして書く。
func (e *testEnv) writeVerification(v int, items map[string]report.ItemResult, comment string) time.Time {
	e.t.Helper()
	saved := e.now.Add(time.Hour)
	b, err := json.Marshal(report.Verification{Version: v, Items: items, Comment: comment, SavedAt: saved})
	if err != nil {
		e.t.Fatal(err)
	}
	e.write(report.VerificationPath(report.Dir(e.root, "p"), v), string(b))
	return saved
}

func TestRun_FeedbackLoop(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "", feedbackScenario(
		fakeAction{AppendPlan: fb1(""), Status: agent.StatusCompleted},
	)...)
	out, log := e.run("", false)
	if out.Stopped() || out.State.ReportVersion != 1 {
		t.Fatalf("第1版を作らずに終えた: %+v\n%s", out.State, log)
	}
	reportDir := report.Dir(e.root, "p")
	saved := e.writeVerification(1, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusNG, Comment: "表示されない"}}, "")

	out, log = e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if len(calls) != 7 {
		t.Fatalf("エージェントの呼び出し = %d回、期待値 = 7回\n%s", len(calls), log)
	}
	if got, want := promptOf(calls[3]), "/korylus-add-feedback-phase docs/private/plans/p.md docs/private/plans/files/p/verification-1.json mode=unattended"; got != want {
		t.Errorf("起票のプロンプト = %q、期待値 = %q", got, want)
	}
	if flagValue(calls[3], "--json-schema") == "" {
		t.Errorf("起票で構造化出力を求めていない: %v", calls[3])
	}
	// 設計者のクローンは、最後のタスクのブランチに合わせてから起動する。
	if got := e.git(filepath.Join(e.hubRoot, "clones", config.RoleDesigner), "symbolic-ref", "--short", "HEAD"); got != "p-1-1" {
		t.Errorf("設計者のクローンのブランチ = %q、期待値 = p-1-1", got)
	}
	if got := promptOf(calls[4]); !strings.Contains(got, " fb1-1 base=hub/p-1-1 ") {
		t.Errorf("フェーズfb1のタスクの実装のプロンプト = %q", got)
	}

	st := e.state()
	if len(st.Tasks) != 2 || st.Tasks[1].ID != "fb1-1" || st.Tasks[1].Branch != "p-fb1-1" || st.Tasks[1].Start != "p-1-1" {
		t.Errorf("終えたタスク = %+v", st.Tasks)
	}
	if st.ReportVersion != 2 || out.Report != report.HTMLPath(reportDir, 2) || st.Verified != 0 {
		t.Errorf("作業報告書 = %q (第%d版)、確認済み = %d", out.Report, st.ReportVersion, st.Verified)
	}
	if fb := outputsOf(st, state.StepFeedback); len(fb) != 1 || fb[0].Role != config.RoleDesigner {
		t.Errorf("起票の構造化出力の記録 = %+v", fb)
	}
	waits := slices.DeleteFunc(e.events(state.EventHumanWait), func(ev state.Event) bool { return ev.Step != state.StepReport })
	if len(waits) != 1 || !waits[0].Started.Equal(st.Reports[0].At) || !waits[0].Ended.Equal(saved) {
		t.Errorf("動作確認の人間の待ちの記録 = %+v", waits)
	}
	rep, err := report.Load(reportDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(rep.Items, func(it report.Item) bool { return it.ID == "task/1-1/1" })
	if i < 0 || rep.Items[i].Previous == nil || rep.Items[i].Previous.Status != report.StatusNG || !slices.Equal(rep.Items[i].Feedback, []string{"fb1-1"}) {
		t.Errorf("第2版の確認項目 = %+v", rep.Items)
	}

	// すべてOKで全体へのコメントも無ければ、確認済みとして終える。
	e.writeVerification(2, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusOK}, "task/fb1-1/1": {Status: report.StatusOK}}, "")
	for range 2 {
		out, log = e.run("", false)
		if out.Stopped() || out.Report != "" || out.State.Verified != 2 {
			t.Errorf("確認済みとして終えていない: 作業報告書 = %q、確認済み = %d\n%s", out.Report, out.State.Verified, log)
		}
	}
	if n := len(e.callArgs()); n != 7 {
		t.Errorf("確認済みのあとのエージェントの呼び出し = %d回、期待値 = 7回", n)
	}
}

func TestRun_FeedbackPending(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "", feedbackScenario()...)
	e.run("", false)
	// 未確認の項目が残っていれば、全体へのコメントがあっても起票せずに確認を待つ。
	e.writeVerification(1, map[string]report.ItemResult{}, "気になる点がある")

	out, log := e.run("", false)
	if out.Stopped() || out.Report != report.HTMLPath(report.Dir(e.root, "p"), 1) {
		t.Errorf("確認を待っていない: %+v、作業報告書 = %q\n%s", out.State.Stop, out.Report, log)
	}
	if !strings.Contains(log, "未確認の項目が1件残っています") {
		t.Errorf("未確認の項目を伝えていない:\n%s", log)
	}
	if n := len(e.callArgs()); n != 3 {
		t.Errorf("エージェントの呼び出し = %d回、期待値 = 3回", n)
	}
	if st := e.state(); st.Step != "" || len(e.events(state.EventHumanWait)) != 0 {
		t.Errorf("ステップ = %q、人間の待ち = %+v", st.Step, e.events(state.EventHumanWait))
	}
}

func TestRun_FeedbackApprove(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "", feedbackScenario(
		fakeAction{AppendPlan: fb1("schema (postsに列を追加)"), Status: agent.StatusCompleted},
	)...)
	e.run("", false)
	// 全体へのコメントだけでも起票する。
	e.writeVerification(1, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusOK}}, "一覧も直してほしい")

	out, _ := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepApprove || out.State.Stop.Role != config.RoleDesigner {
		t.Fatalf("承認を求めて止まっていない: %+v (%q)", out.State.Stop, out.State.Step)
	}
	if d := out.State.Stop.Details; len(d) != 1 || !strings.Contains(d[0], "タスク fb1-1 表示を直す: schema") {
		t.Errorf("止まった理由の詳細 = %q", d)
	}

	// 承認せずに実行しても、フェーズfb1のタスクを実行しない。
	out, _ = e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepApprove || !strings.Contains(out.State.Stop.Reason, "-approve") {
		t.Errorf("承認せずに進んだ: %+v (%q)", out.State.Stop, out.State.Step)
	}
	if n := len(e.callArgs()); n != 4 {
		t.Errorf("承認の前のエージェントの呼び出し = %d回、期待値 = 4回", n)
	}

	out, log := e.run("", true)
	if out.Stopped() {
		t.Fatalf("承認したのに止まった: %+v\n%s", out.State.Stop, log)
	}
	if st := e.state(); len(st.Tasks) != 2 || st.Tasks[1].ID != "fb1-1" || st.ReportVersion != 2 {
		t.Errorf("承認したあとの状態 = %+v", st)
	}
}

func TestRun_FeedbackEscalationAndResume(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "", feedbackScenario(
		fakeAction{Status: agent.StatusEscalated},
		fakeAction{AppendPlan: fb1(""), Status: agent.StatusCompleted},
	)...)
	e.run("", false)
	e.writeVerification(1, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusNG, Comment: "設計と違う"}}, "")

	out, _ := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepFeedback || out.State.Stop.Role != config.RoleDesigner {
		t.Fatalf("エスカレーションで止まっていない: %+v (%q)", out.State.Stop, out.State.Step)
	}

	out, log := e.run("合意事項は変えずに直す", false)
	if out.Stopped() {
		t.Fatalf("再開して止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if got := flagValue(calls[4], "--resume"); got != "session-4" {
		t.Errorf("再開した起票のセッション = %q、期待値 = session-4", got)
	}
	if got := promptOf(calls[4]); !strings.HasPrefix(got, "/korylus-add-feedback-phase ") || !strings.HasSuffix(got, "\n\n補足: 合意事項は変えずに直す") {
		t.Errorf("再開した起票のプロンプト = %q", got)
	}
	if st := e.state(); len(st.Tasks) != 2 || st.ReportVersion != 2 {
		t.Errorf("再開したあとの状態 = %+v", st)
	}
}

func TestRun_FeedbackViolation(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "", feedbackScenario(
		fakeAction{AppendPlan: "### フェーズfb1: 追加\n\n- [ ] **1-2**: [Go] フェーズfb1でないタスク\n", Status: agent.StatusCompleted},
	)...)
	e.run("", false)
	e.writeVerification(1, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusNG, Comment: "違う"}}, "")

	out, _ := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepFeedback || out.State.Stop.Role != config.RoleDesigner {
		t.Fatalf("フェーズfb1の追加以外の変更で止まっていない: %+v (%q)", out.State.Stop, out.State.Step)
	}
	if d := strings.Join(out.State.Stop.Details, "\n"); !strings.Contains(d, "タスク 1-2 はフェーズfb1のタスクではありません") {
		t.Errorf("止まった理由の詳細 = %q", d)
	}
	snap, err := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p").LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snap), "**1-2**") {
		t.Errorf("許されない変更をスナップショットに取り込んだ")
	}
}

func TestRun_FeedbackApprovalInterrupted(t *testing.T) {
	t.Parallel()

	for _, boundary := range []string{"スナップショット更新前", "スナップショット更新後", "起票ステップが残った状態"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, []string{task1}, "", feedbackScenario(fakeAction{AppendPlan: fb1("schema (postsに列を追加)"), Status: agent.StatusCompleted})...)
			e.run("", false)
			d := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p")
			old, err := d.LoadSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			e.writeVerification(1, map[string]report.ItemResult{"task/1-1/1": {Status: report.StatusNG}}, "")
			e.run("", false)
			st := e.state()
			st.Stop = nil
			switch boundary {
			case "スナップショット更新前":
				if err := d.SaveSnapshot(old); err != nil {
					t.Fatal(err)
				}
			case "起票ステップが残った状態":
				st.Step = state.StepFeedback
			}
			if err := d.Save(st, e.now); err != nil {
				t.Fatal(err)
			}
			out, log := e.run("", false)
			if !out.Stopped() || out.State.Step != state.StepApprove || len(e.callArgs()) != 4 {
				t.Fatalf("承認なしで進んだ: %+v\n%s", out.State, log)
			}
			out, log = e.run("", true)
			if out.Stopped() || len(out.State.Tasks) != 2 || out.State.ReportVersion != 2 {
				t.Fatalf("承認後に再開できない: %+v\n%s", out.State, log)
			}
		})
	}
}

func TestAcceptFeedback_SaveFailure(t *testing.T) {
	t.Parallel()

	for _, failed := range []string{"state.json", "plan.md"} {
		t.Run(failed, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			d := state.NewDir(root, "p")
			old := []byte("## タスクリスト\n\n- [x] **1-1**: 元タスク\n")
			if err := d.SaveSnapshot(old); err != nil {
				t.Fatal(err)
			}
			st := &state.State{Step: state.StepFeedback}
			if err := d.Save(st, time.Now()); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "p", failed)
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(file, 0o700); err != nil {
				t.Fatal(err)
			}
			r := &run{p: &Pilot{}, dir: d, st: st, snapshot: old}
			_, err := r.acceptFeedback(append(append([]byte{}, old...), []byte(fb1("schema (列を追加)"))...), []plan.Task{{ID: "fb1-1", Protected: []plan.Declaration{{Name: "schema"}}}}, 1)
			if err == nil {
				t.Fatal("保存の失敗を期待したが成功した")
			}
			if failed == "state.json" {
				snap, err := d.LoadSnapshot()
				if err != nil || string(snap) != string(old) {
					t.Fatalf("承認待ちの保存前にスナップショットを更新した: %q (%v)", snap, err)
				}
			} else {
				saved, err := d.Load()
				if err != nil || saved.Step != state.StepApprove || saved.Stop == nil || len(saved.Stop.Details) != 1 {
					t.Fatalf("承認待ちと対象タスクが保存されていない: %+v (%v)", saved, err)
				}
			}
		})
	}
}
