package flow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

// readReportは状態ディレクトリの版vの作業報告書を読む。
func (fx fixture) readReport(t *testing.T, v int) string {
	t.Helper()
	b, err := os.ReadFile(fx.flow.Dir.ReportPath(v))
	if err != nil {
		t.Fatalf("第%d版の作業報告書を読めません: %v", v, err)
	}
	return string(b)
}

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

	// タスクを進めている途中は作らない。
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk {
		t.Fatalf("指示 = %+v、人が行うタスクのaskを期待", in)
	}
	if _, err := fx.flow.Report(ctx); err == nil || !strings.Contains(err.Error(), "タスク 1-2 を進めている途中です") {
		t.Errorf("途中のReport()のエラー = %v", err)
	}

	// 計画書の最後で、第1版を作る。
	fx.checkTask(t, "1-2")
	first := fx.flow.Dir.ReportPath(1)
	msg, err := fx.done(state.RoleImplementer, "")
	if err != nil {
		t.Fatalf("人への確認のDoneのエラー = %v", err)
	}
	if !strings.Contains(msg, "作業報告書: "+first) {
		t.Errorf("Done() = %q", msg)
	}
	out := fx.readReport(t, 1)
	for _, want := range []string{
		"| `" + fx.repo + "` | 1-1 | `demo-1-1` | `main` |",
		"` feature_test.go\n",
		"| `feature.go` | 1 | 0 |",
		"- 変更行数: テスト以外1行、テスト1行",
		"| 1-2 | 実装者 | 人が行うタスクです |",
		"<!-- item: task/1-1/1 -->",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("第1版に %q が無い:\n%s", want, out)
		}
	}

	// 計画書の最後で何度waitしても、版を増やさない。
	for range 2 {
		in := fx.wait(t, state.RoleReviewerClaude)
		if in.Action != ActionFinish || in.Report != first || !strings.Contains(in.String(), first) {
			t.Fatalf("指示 = %+v\n%s", in, in.String())
		}
	}
	if _, err := os.Stat(fx.flow.Dir.ReportPath(2)); !os.IsNotExist(err) {
		t.Errorf("新しいステップが無いのに第2版を作った: %v", err)
	}

	// Reportは同じ版を作り直し、書き込まれた結果を引き継ぐ。
	answered := strings.Replace(out, "- [ ] OK", "- [x] OK", 1)
	if err := os.WriteFile(first, []byte(answered), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := fx.flow.Report(ctx); err != nil || path != first {
		t.Fatalf("Report() = %q, %v、期待値 = %q", path, err, first)
	}
	if out := fx.readReport(t, 1); !strings.Contains(out, "- [x] OK") {
		t.Errorf("作り直した第1版で、書き込まれた結果が消えた:\n%s", out)
	}

	// タスクを足して進めると、計画書の最後で第2版を作る。
	src, err := os.ReadFile(fx.flow.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	src = append(src, []byte("- [ ] **fb1-1**: ["+fx.repo+"] 直す\n")...)
	if err := os.WriteFile(fx.flow.PlanPath, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
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
	if msg := fx.approve(t, "fb1-1"); !strings.Contains(msg, fx.flow.Dir.ReportPath(2)) {
		t.Errorf("Done() = %q", msg)
	}
	out = fx.readReport(t, 2)
	for _, want := range []string{
		"- 前の版: 第1版",
		"### fb1-1 [" + fx.repo + "] 直す\n\nこの版で進めたタスクです。",
		"<!-- confirmed: task/1-1/1 -->",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("第2版に %q が無い:\n%s", want, out)
		}
	}
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Reports) != 2 || st.Reports[1].Version != 2 {
		t.Errorf("作業報告書の記録 = %+v", st.Reports)
	}
}

func TestFlowReportBeforeStart(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	if _, err := fx.flow.Report(context.Background()); err == nil || !strings.Contains(err.Error(), "まだループを始めていません") {
		t.Errorf("Report()のエラー = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.flow.Dir.Path(), "report-1.md")); !os.IsNotExist(err) {
		t.Errorf("作業報告書を作った: %v", err)
	}
}

func TestFlowReportWhileRecording(t *testing.T) {
	t.Parallel()

	// 計画書の最後のステップの完了を確かめたあと、履歴を記録する前に落ちた状態。
	fx := newFixture(t, "- [x] **1-1**: [{repo}] 済んだタスク")
	now := time.Now()
	pending := &state.Event{Task: "1-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: now.Add(-time.Hour), Ended: now}
	if err := fx.flow.Dir.Save(&state.State{Pending: pending}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.flow.Report(context.Background()); err == nil || !strings.Contains(err.Error(), "タスク 1-1 の完了を記録している途中です") {
		t.Errorf("Report()のエラー = %v", err)
	}
	if _, err := os.Stat(fx.flow.Dir.ReportPath(1)); !os.IsNotExist(err) {
		t.Errorf("作業報告書を作った: %v", err)
	}
}

func TestFlowReportWithoutEvents(t *testing.T) {
	t.Parallel()

	// korylusflowを使う前に、計画書のタスクをすべて手で済ませていた。
	fx := newFixture(t, "- [x] **1-1**: [{repo}] 手で済ませたタスク")
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish || in.Report != "" {
		t.Fatalf("指示 = %+v、作業報告書の無い終了の指示を期待", in)
	}
	if _, err := fx.flow.Report(context.Background()); err == nil || !strings.Contains(err.Error(), "記録したステップがありません") {
		t.Errorf("Report()のエラー = %v", err)
	}
	if _, err := os.Stat(fx.flow.Dir.ReportPath(1)); !os.IsNotExist(err) {
		t.Errorf("作業報告書を作った: %v", err)
	}
}

func TestFlowReportRefusesAbsorbedSections(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] 最初のタスク")
	fx.wait(t, state.RoleImplementer)
	commit(t, fx.repo, "feature.go")
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, `[{"kind":"process","title":"一覧を出せる","target":"status","steps":["実行する"],"expected":"一覧が出る"}]`); err != nil {
		t.Fatal(err)
	}
	fx.approve(t, "1-1")

	// コメントのフェンスを閉じ忘れると、全体へのコメントの節がコードブロックに入る。
	first := fx.flow.Dir.ReportPath(1)
	written := strings.Replace(fx.readReport(t, 1), "コメント:\n\n", "コメント:\n\n```\nエラーの出力\n\n", 1)
	if err := os.WriteFile(first, []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fx.flow.Report(context.Background())
	if err == nil || !strings.Contains(err.Error(), "作り直しません") || !strings.Contains(err.Error(), "「全体へのコメント」の節が、項目のコメントに取り込まれています") {
		t.Errorf("Report()のエラー = %v", err)
	}
	if out := fx.readReport(t, 1); out != written {
		t.Errorf("取り込みがあるのに作り直した:\n%s", out)
	}
}

func TestFlowReportKeepsMarkdownComments(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] 最初のタスク")
	fx.wait(t, state.RoleImplementer)
	commit(t, fx.repo, "feature.go")
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, `[{"kind":"process","title":"一覧を出せる","target":"status","steps":["実行する"],"expected":"一覧が出る"}]`); err != nil {
		t.Fatal(err)
	}
	fx.approve(t, "1-1")

	comment := "再現手順\n\n### 詳細\n\n```markdown\n# エラー\n```\n\n一覧が出ない"
	first := fx.flow.Dir.ReportPath(1)
	written := strings.Replace(fx.readReport(t, 1), "- [ ] NG", "- [x] NG", 1)
	written = strings.Replace(written, "コメント:\n\n", "コメント:\n\n"+comment+"\n\n", 1)
	written += "\n" + comment + "\n"
	if err := os.WriteFile(first, []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.flow.Report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := fx.readReport(t, 1); strings.Count(out, comment) != 2 || !strings.Contains(out, "- [x] NG") {
		t.Errorf("同じ版を作り直したときに結果を失った:\n%s", out)
	}

	src, err := os.ReadFile(fx.flow.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	src = append(src, []byte("- [ ] **fb1-1**: [shimbaco] 確認する\n")...)
	if err := os.WriteFile(fx.flow.PlanPath, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		t.Fatal(err)
	}
	fx.wait(t, state.RoleImplementer)
	fx.checkTask(t, "fb1-1")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatal(err)
	}
	quoted := "> " + strings.ReplaceAll(comment, "\n", "\n> ")
	if out := fx.readReport(t, 2); strings.Count(out, quoted) != 2 || !strings.Contains(out, "第1版 #1 NG") {
		t.Errorf("次の版を作ったときに前版の結果を失った:\n%s", out)
	}
}
