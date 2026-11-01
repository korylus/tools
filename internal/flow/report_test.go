package flow

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestFlowReport(t *testing.T) {
	t.Parallel()

	fx := newFixture(t,
		"- [ ] **1-1**: [{repo}] 最初のタスク",
		"- [ ] **1-2**: [shimbaco] タグを付ける",
	)
	ctx := context.Background()

	fx.wait(t, state.RoleImplementer)
	commit(t, fx.repo, "feature.go")
	commit(t, fx.repo, "feature_test.go")
	fx.checkTask(t, "1-1")
	items := `[{"kind":"process","title":"一覧を出せる","target":"korylusflow status demo.md","steps":["実行する"],"expected":"一覧が出る"}]`
	if _, err := fx.done(state.RoleImplementer, items); err != nil {
		t.Fatalf("実装のDoneのエラー = %v", err)
	}
	fx.approve(t, "1-1")

	// タスクを進めている途中でも、事実を組み立てる。
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk {
		t.Fatalf("指示 = %+v、人が行うタスクのaskを期待", in)
	}
	out := fx.facts(t)
	for _, want := range []string{
		"- 作業報告書: まだ書いていません\n",
		"| `" + fx.repo + "` | `demo-1-1` | `main` | 1-1 |",
		"| `demo-1-1` (元: `main`) | テスト以外1行・テスト1行 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("途中の事実に %q が無い:\n%s", want, out)
		}
	}

	// 計画書の最後で、実装者が作業報告書を書く。
	fx.checkTask(t, "1-2")
	msg, err := fx.done(state.RoleImplementer, "")
	if err != nil {
		t.Fatalf("人への確認のDoneのエラー = %v", err)
	}
	if !strings.Contains(msg, "次は作業報告書の作成で、implementer の番です") {
		t.Errorf("Done() = %q", msg)
	}
	if in, err := fx.flow.Wait(ctx, state.RoleReviewerCodex, 50*time.Millisecond); err != nil || in.Action != ActionWait {
		t.Errorf("レビュアーの指示 = %+v, %v、作業報告書のステップでは待つことを期待", in, err)
	}
	_, err = fx.done(state.RoleReviewerCodex, "")
	assertRejected(t, err, "reviewer-codex の番ではありません (作業報告書の作成は implementer の番です)")

	in := fx.wait(t, state.RoleImplementer)
	head := strings.TrimSpace(gitRun(t, fx.repo, "rev-parse", "demo-1-1"))
	if in.Action != ActionReport || in.Task != "" || in.PreviousReport != "" ||
		len(in.Branches) != 1 || in.Branches[0] != (ReportBranch{Repo: fx.repo, Branch: "demo-1-1", Commit: head}) {
		t.Fatalf("指示 = %+v、demo-1-1 (%s) の作業報告書の指示を期待", in, head)
	}
	for _, want := range []string{
		"action: report\n",
		"- " + fx.repo + ": `demo-1-1` (" + head + ")\n",
		"`/korylus-write-report " + fx.flow.PlanURL + "`",
		`"walkthrough_skipped":"処理の流れを書かなかった理由"`,
	} {
		if !strings.Contains(in.String(), want) {
			t.Errorf("指示に %q が無い:\n%s", want, in.String())
		}
	}
	_, err = fx.done(state.RoleImplementer, `{"location":"docs/private/plans/files/demo/report.md"}`)
	assertRejected(t, err, "https:// で始まる作業報告書のURL")
	first := "https://wikino.app/s/demo/pages/1"
	if msg, err = fx.done(state.RoleImplementer, `{"location":"`+first+`"}`); err != nil {
		t.Fatalf("作業報告書のDoneのエラー = %v", err)
	}
	if !strings.Contains(msg, "作業報告書: "+first) {
		t.Errorf("Done() = %q", msg)
	}
	if out := fx.facts(t); !strings.Contains(out, "- 作業報告書: 1回目 "+first+"\n") {
		t.Errorf("事実に作業報告書のURLが無い:\n%s", out)
	}

	// 計画書の最後で何度waitしても、作業報告書のステップを繰り返さない。
	for range 2 {
		in := fx.wait(t, state.RoleReviewerClaude)
		if in.Action != ActionFinish || in.Report != first || !strings.Contains(in.String(), first) {
			t.Fatalf("指示 = %+v\n%s", in, in.String())
		}
	}

	// タスクを足して進めると、計画書の最後で次の回の作業報告書を書く。
	src, err := os.ReadFile(fx.planFile)
	if err != nil {
		t.Fatal(err)
	}
	src = append(src, []byte("- [ ] **fb1-1**: ["+fx.repo+"] 直す\n")...)
	if err := os.WriteFile(fx.planFile, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		t.Fatal(err)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Task != "fb1-1" || in.Record.Base != "demo-1-1" {
		t.Fatalf("指示 = %+v、demo-1-1から作ったタスクfb1-1の実装を期待", in)
	}
	commit(t, fx.repo, "fix.go")
	fx.checkTask(t, "fb1-1")
	if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
		t.Fatalf("実装のDoneのエラー = %v", err)
	}
	if msg := fx.approve(t, "fb1-1"); !strings.Contains(msg, "次は作業報告書の作成") {
		t.Errorf("Done() = %q", msg)
	}
	// 2回目では、前の回で書いた作業報告書を更新してもらう。
	in = fx.wait(t, state.RoleImplementer)
	if in.Action != ActionReport || in.PreviousReport != first || len(in.Branches) != 1 || in.Branches[0].Branch != "demo-fb1-1" ||
		!strings.Contains(in.String(), "前の回の作業報告書: "+first+"\n") {
		t.Fatalf("指示 = %+v、demo-fb1-1で前の回の作業報告書を更新する指示を期待\n%s", in, in.String())
	}
	if msg, err := fx.done(state.RoleImplementer, `{"location":"`+first+`"}`); err != nil || !strings.Contains(msg, first) {
		t.Errorf("作業報告書のDone() = %q, %v", msg, err)
	}

	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	events, err := fx.flow.Dir.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Reports) != 2 || st.Reports[0].Number != 1 || st.Reports[1].Number != 2 ||
		st.Reports[1].Location != first || st.Reports[1].Events != len(events) {
		t.Errorf("作業報告書の記録 = %+v、所要時間の記録 = %d件", st.Reports, len(events))
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish {
		t.Errorf("指示 = %+v、終了を期待", in)
	}
}

func TestFlowReportBeforeStart(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	if _, err := fx.flow.Report(context.Background()); err == nil || !strings.Contains(err.Error(), "まだループを始めていません") {
		t.Errorf("Report()のエラー = %v", err)
	}
}

func TestFlowReportWithoutEvents(t *testing.T) {
	t.Parallel()

	// korylusflowを使う前に、計画書のタスクをすべて手で済ませていた。
	fx := newFixture(t, "- [x] **1-1**: [{repo}] 手で済ませたタスク")
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish || in.Report != "" {
		t.Fatalf("指示 = %+v、作業報告書の無い終了の指示を期待", in)
	}
	out := fx.facts(t)
	for _, want := range []string{"- 作業報告書: まだ書いていません\n", "ブランチを作ったタスクはありません。\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("事実に %q が無い:\n%s", want, out)
		}
	}
}
