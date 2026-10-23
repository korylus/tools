package report

import (
	"reflect"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// testPlanは作業報告書の入力に使う計画書。
const testPlan = "# テストの計画書\n" +
	"\n" +
	"## 合意済みの設計制約\n" +
	"\n" +
	"postsの列は追加だけにする。\n" +
	"\n" +
	"## 実装判断ログ\n" +
	"\n" +
	"### タスク1-1: 公開範囲を保存する\n" +
	"\n" +
	"#### 列の型をどうするか?\n" +
	"\n" +
	"## タスクリスト\n" +
	"\n" +
	"- [x] **1-1**: [Go] 公開範囲を保存する\n" +
	"  - **保護領域**: schema (posts に visibility 列を追加)\n" +
	"  - **想定行数**: 約20行 (実装10行 + テスト10行)\n" +
	"\n" +
	"- [x] **1-2**: [Go] 公開範囲を表示する\n" +
	"\n" +
	"### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n" +
	"\n" +
	"- [x] **fb1-1**: [Go] 保存後に編集画面へ戻す\n" +
	"  - **保護領域**: schema (visibility に既定値を付ける)\n" +
	"  - **対応する確認項目**: 第1版 #2 「保存後に戻る」\n"

func testConfig() *config.Config {
	return &config.Config{
		Report: config.Report{Setup: []string{"make -C go db-migrate"}},
		Gate:   config.Gate{TestGlobs: []string{"**/*_test.go"}},
		Protected: []config.Protected{
			{Name: "schema", Label: "DBスキーマ"},
			{Name: "dependency", Label: "依存パッケージ"},
		},
	}
}

// output は構造化出力の記録を作る。
func output(task string, step state.Step, role, outcome string, res agent.UnattendedResult) state.Output {
	if res.Status == "" {
		res.Status = agent.StatusCompleted
	}
	return state.Output{Task: task, Step: step, Role: role, At: t0, Outcome: outcome, Result: res}
}

func diff(files ...gate.File) *gate.Diff {
	return &gate.Diff{Files: files}
}

func lines(n int) []gate.Line {
	res := make([]gate.Line, n)
	for i := range res {
		res[i] = gate.Line{No: i + 1, Text: "x"}
	}
	return res
}

// firstInputは、2つのタスクを終えて第1版を作るときの入力。
func firstInput() Input {
	st := &state.State{
		Tasks: []state.TaskRecord{
			{ID: "1-1", Branch: "p-1-1", Start: "p-base", Commit: "c11"},
			{ID: "1-2", Branch: "p-1-2", Start: "p-1-1", Commit: "c12"},
		},
		Outputs: []state.Output{
			output("1-1", state.StepImplement, "implementer", state.OutcomeFailed, agent.UnattendedResult{Summary: "失敗した試行"}),
			output("1-1", state.StepImplement, "implementer", state.OutcomeOK, agent.UnattendedResult{
				Summary: "公開範囲を保存した",
				VerificationItems: []agent.VerificationItem{
					{Kind: KindScreen, Title: "公開範囲を選べる", Target: "/posts/new", Steps: []string{"開く"}, Expected: "選べる"},
					{Kind: KindProcess, Title: "保存後に戻る", Target: "make check", Steps: []string{"実行する"}, Expected: "戻る"},
				},
				ProtectedChanges: []agent.ProtectedChange{{Area: "schema", Summary: "postsにvisibility列を追加", Reason: "設計のとおり"}},
			}),
			output("1-1", state.StepReview, "reviewer_codex", state.OutcomeOK, agent.UnattendedResult{
				ProtectedChecks: []agent.ProtectedCheck{{Area: "schema", Point: "ロックの長さ", Result: "ok", Detail: "列の追加だけ"}},
			}),
			output("1-2", state.StepImplement, "implementer", state.OutcomeOK, agent.UnattendedResult{Summary: "公開範囲を表示した"}),
		},
		ReviewLog: []state.ReviewRecord{{Task: "1-1", Number: 1, Role: "reviewer_codex", Doc: "docs/private/reviews/p-1-1-001.md", Verdict: "Approve"}},
		Deferred:  []state.DeferredFinding{{Task: "1-1", Doc: "docs/private/reviews/p-1-1-001.md", Title: "命名", Choice: "後続タスクに積む (推奨)"}},
		StopLog:   []state.StopRecord{{Task: "1-1", Step: state.StepImplement, Stop: state.Stop{Reason: "エスカレーション", Role: "implementer", At: t0.Add(time.Hour)}, ResumedAt: t0.Add(2 * time.Hour)}},
	}
	return Input{
		PlanPath: "docs/private/plans/p.md",
		Version:  1,
		Now:      t0.Add(5 * time.Hour),
		State:    st,
		Plan:     testPlan,
		Config:   testConfig(),
		Events: []state.Event{
			{Kind: state.EventAgent, Task: "1-1", Step: state.StepImplement, Role: "implementer", Tool: "claude", Started: t0, Ended: t0.Add(10 * time.Minute), Turns: 3, CostUSD: 0.5},
			{Kind: state.EventGate, Task: "1-1", Step: state.StepGate, Started: t0.Add(10 * time.Minute), Ended: t0.Add(11 * time.Minute)},
			{Kind: state.EventHumanWait, Task: "1-1", Step: state.StepImplement, Role: "implementer", Started: t0.Add(time.Hour), Ended: t0.Add(2 * time.Hour)},
			{Kind: state.EventAgent, Task: "1-2", Step: state.StepImplement, Role: "implementer", Tool: "claude", Started: t0.Add(3 * time.Hour), Ended: t0.Add(3*time.Hour + 5*time.Minute), Turns: 2, CostUSD: 0.25},
		},
		StepUsages: []usage.StepUsage{
			// 使用率の記録は計画書をまたぐため、計画書の開始より前の別の計画書の記録は数えない。
			{At: t0.Add(-24 * time.Hour), Tool: agent.Claude, Step: state.StepImplement, Window: usage.FiveHour, Before: 0, After: 40},
			{At: t0.Add(10 * time.Minute), Tool: agent.Claude, Step: state.StepImplement, Window: usage.FiveHour, Before: 10, After: 12},
			{At: t0.Add(3 * time.Hour), Tool: agent.Claude, Step: state.StepImplement, Window: usage.FiveHour, Before: 12, After: 13.5},
		},
		Checkout: hub.Checkout{Switched: true},
		Diffs: []TaskDiff{
			{
				Diff: diff(
					gate.File{Path: "go/post.go", Status: gate.Modified, Added: lines(8), Removed: lines(2)},
					gate.File{Path: "go/post_test.go", Status: gate.Added, Added: lines(10)},
					gate.File{Path: "db/1.sql", Status: gate.Added, Added: lines(3)},
				),
				Inspect: &gate.Report{ImplLines: 13, Touches: []gate.Touch{{Name: "schema", Label: "DBスキーマ", Declared: true, Hits: []gate.Hit{{Path: "db/1.sql"}}}}},
			},
			{
				Diff:    diff(gate.File{Path: "go.mod", Status: gate.Modified, Added: lines(1)}),
				Inspect: &gate.Report{ImplLines: 1, Touches: []gate.Touch{{Name: "dependency", Label: "依存パッケージ", Hits: []gate.Hit{{Path: "go.mod", Line: 3, Text: "require x"}}}}},
			},
		},
		Final: &gate.Report{Touches: []gate.Touch{
			{Name: "schema", Label: "DBスキーマ", Declared: true, Hits: []gate.Hit{{Path: "db/1.sql"}}},
			{Name: "dependency", Label: "依存パッケージ", Hits: []gate.Hit{{Path: "go.mod", Line: 3, Text: "require x"}}},
		}},
	}
}

func TestBuild_FirstVersion(t *testing.T) {
	t.Parallel()

	r, err := Build(firstInput())
	if err != nil {
		t.Fatalf("Buildのエラー = %v", err)
	}

	if r.Name != "p" || r.Version != 1 || !r.Started.Equal(t0) || !r.VersionStarted.Equal(t0) {
		t.Errorf("概要 = %s 第%d版 (開始 %v、この版の開始 %v)", r.Name, r.Version, r.Started, r.VersionStarted)
	}
	if want := []UsageRow{{Tool: "claude", Window: "five_hour", Percent: 3.5, Steps: 2}}; !reflect.DeepEqual(r.Usage, want) {
		t.Errorf("使用量 = %+v、期待値 = %+v", r.Usage, want)
	}
	if want := []CostRow{{Tool: "claude", Runs: 2, Turns: 5, CostUSD: 0.75}}; !reflect.DeepEqual(r.Costs, want) {
		t.Errorf("コスト = %+v、期待値 = %+v", r.Costs, want)
	}
	if want := (Preparation{Branch: "p-1-2", Switched: true, Setup: []string{"make -C go db-migrate"}}); !reflect.DeepEqual(r.Preparation, want) {
		t.Errorf("準備 = %+v、期待値 = %+v", r.Preparation, want)
	}

	task := r.Tasks[0]
	if task.Summary != "公開範囲を保存した" || task.ImplLines != 13 || task.ExpectedImplLines != 10 || !task.NewInVersion {
		t.Errorf("タスク1-1 = %+v", task)
	}
	if want := []string{"列の型をどうするか?"}; !reflect.DeepEqual(task.Decisions, want) {
		t.Errorf("論点 = %v、期待値 = %v", task.Decisions, want)
	}
	if want := (FileChange{Path: "go/post_test.go", Status: "A", Kind: "test", Added: 10}); task.Files[1] != want {
		t.Errorf("変更ファイル = %+v、期待値 = %+v", task.Files[1], want)
	}
	if len(task.Reviews) != 1 || task.Reviews[0].Verdict != "Approve" {
		t.Errorf("レビュー = %+v", task.Reviews)
	}
	if len(r.Deferred) != 1 || r.Deferred[0].Title != "命名" {
		t.Errorf("先送りした指摘 = %+v", r.Deferred)
	}
	if len(r.Stops) != 1 || r.Stops[0].Step != "実装" || !r.Stops[0].ResumedAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("止まった履歴 = %+v", r.Stops)
	}

	if want := (TaskTiming{Task: "1-1", Agent: 10 * time.Minute, Gate: time.Minute, HumanWait: time.Hour, Reviews: 1}); r.Timing.Tasks[0] != want {
		t.Errorf("1-1の所要時間 = %+v、期待値 = %+v", r.Timing.Tasks[0], want)
	}
	if r.Timing.Total != r.Timing.VersionTotal || r.Timing.Total != 76*time.Minute {
		t.Errorf("合計 = %v (この版 %v)、期待値 = 1時間16分", r.Timing.Total, r.Timing.VersionTotal)
	}
	if got := r.Timing.Rows[0]; got.Kind != "agent" || got.Role != "implementer" || got.Count != 2 || got.Total != 15*time.Minute {
		t.Errorf("最初の行 = %+v", got)
	}

	p := r.Protected
	if p.Constraints != "postsの列は追加だけにする。" {
		t.Errorf("合意済みの設計制約 = %q", p.Constraints)
	}
	if len(p.Undeclared) != 1 || p.Undeclared[0].Task != "1-2" || p.Undeclared[0].Area != "dependency" {
		t.Errorf("宣言の無い変更 = %+v", p.Undeclared)
	}
	if len(p.Areas) != 2 || p.Areas[0].Name != "schema" || p.Areas[1].Name != "dependency" {
		t.Fatalf("保護領域 = %+v", p.Areas)
	}
	schema := p.Areas[0]
	if want := []Declaration{{Task: "1-1", Note: "posts に visibility 列を追加"}}; !reflect.DeepEqual(schema.Declarations, want) {
		t.Errorf("宣言 = %+v、期待値 = %+v", schema.Declarations, want)
	}
	if want := []AreaTask{{
		Task: "1-1", Declared: true, Hits: []Hit{{Path: "db/1.sql"}},
		Changes: []Change{{Step: "実装", Role: "implementer", Summary: "postsにvisibility列を追加", Reason: "設計のとおり"}},
		Checks:  []Check{{Role: "reviewer_codex", Point: "ロックの長さ", Result: "ok", Detail: "列の追加だけ"}},
	}}; !reflect.DeepEqual(schema.Tasks, want) {
		t.Errorf("schemaのタスクごとの内訳 = %+v\n期待値 = %+v", schema.Tasks, want)
	}

	var ids []string
	for i, it := range r.Items {
		ids = append(ids, it.ID)
		if it.Number != i+1 || it.Confirmed || it.Previous != nil {
			t.Errorf("項目 %+v", it)
		}
	}
	if want := []string{"task/1-1/1", "task/1-1/2", "protected/schema", "protected/dependency"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("項目 = %v、期待値 = %v", ids, want)
	}
	if got := r.Items[3].Tasks; !reflect.DeepEqual(got, []string{"1-2"}) {
		t.Errorf("dependencyの項目の対象タスク = %v", got)
	}
}

func TestBuild_SecondVersion(t *testing.T) {
	t.Parallel()

	first := firstInput()
	// 第1版はタスク1-1だけで作った。
	first.State.Tasks, first.Diffs = first.State.Tasks[:1], first.Diffs[:1]
	first.Final = &gate.Report{Touches: first.Diffs[0].Inspect.Touches}
	prev, err := Build(first)
	if err != nil {
		t.Fatalf("第1版のBuildのエラー = %v", err)
	}

	in := firstInput()
	in.Version = 2
	since := t0.Add(2 * time.Hour)
	in.State.Reports = []state.ReportRecord{{Version: 1, Tasks: 1, At: since}}
	in.State.Tasks[1] = state.TaskRecord{ID: "fb1-1", Branch: "p-fb1-1", Start: "p-1-1", Commit: "cfb"}
	in.State.Outputs = append(in.State.Outputs, output("fb1-1", state.StepImplement, "implementer", state.OutcomeOK, agent.UnattendedResult{
		VerificationItems: []agent.VerificationItem{{Kind: KindScreen, Title: "編集画面へ戻る", Target: "/posts/1/edit", Expected: "戻る"}},
	}))
	in.Diffs[1].Inspect = &gate.Report{Touches: []gate.Touch{{Name: "schema", Label: "DBスキーマ", Declared: true, Hits: []gate.Hit{{Path: "db/2.sql"}}}}}
	in.Final = &gate.Report{Touches: []gate.Touch{{Name: "schema", Label: "DBスキーマ", Declared: true, Hits: []gate.Hit{{Path: "db/1.sql"}, {Path: "db/2.sql"}}}}}
	in.Previous = prev
	in.PreviousResult = &Verification{Version: 1, Comment: "全体のコメント", Items: map[string]ItemResult{
		"task/1-1/1":       {Status: StatusOK},
		"task/1-1/2":       {Status: StatusNG, Comment: "戻らない"},
		"protected/schema": {Status: StatusOK},
	}}

	r, err := Build(in)
	if err != nil {
		t.Fatalf("Buildのエラー = %v", err)
	}
	if !r.VersionStarted.Equal(since) || r.Previous == nil || r.Previous.Comment != "全体のコメント" {
		t.Errorf("この版の開始 = %v、前の版 = %+v", r.VersionStarted, r.Previous)
	}
	if r.Tasks[0].NewInVersion || !r.Tasks[1].NewInVersion {
		t.Errorf("この版で追加したタスク = %v, %v", r.Tasks[0].NewInVersion, r.Tasks[1].NewInVersion)
	}
	// この版の期間に入るのは、前の版を作ったあとに始まった2つ目のエージェントの実行だけ。
	if r.Timing.VersionTotal != 5*time.Minute {
		t.Errorf("この版の合計 = %v、期待値 = 5分", r.Timing.VersionTotal)
	}

	type got struct {
		id        string
		confirmed bool
		prev      int
		feedback  []string
	}
	var items []got
	for i, it := range r.Items {
		if it.Number != i+1 {
			t.Errorf("%sの番号 = %d、期待値 = %d", it.ID, it.Number, i+1)
		}
		g := got{id: it.ID, confirmed: it.Confirmed, feedback: it.Feedback}
		if it.Previous != nil {
			g.prev = it.Previous.Number
		}
		items = append(items, g)
	}
	// 前の版のNGの項目、新しい項目と触れ直した保護領域、確認済みの項目の順に並ぶ。
	want := []got{
		{id: "task/1-1/2", prev: 2, feedback: []string{"fb1-1"}},
		{id: "task/fb1-1/1"},
		{id: "protected/schema", prev: 3},
		{id: "task/1-1/1", confirmed: true, prev: 1},
	}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("項目 = %+v\n期待値 = %+v", items, want)
	}
	if p := r.Items[0].Previous; p.Status != StatusNG || p.Comment != "戻らない" {
		t.Errorf("前の版の結果 = %+v", p)
	}
}

func TestBuild_Invalid(t *testing.T) {
	t.Parallel()

	in := firstInput()
	in.Diffs = in.Diffs[:1]
	if _, err := Build(in); err == nil {
		t.Error("差分の数が合わないのにエラーにならない")
	}
	in = firstInput()
	in.State.Tasks, in.Diffs = nil, nil
	if _, err := Build(in); err == nil {
		t.Error("終えたタスクが無いのにエラーにならない")
	}
}
