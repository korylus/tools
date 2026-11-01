package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/plan"
)

const testPlan = `# 計画書

## 実装判断ログ

### タスク 1-1: 状態を作る

#### 状態をどこに置くか?

## タスクリスト

- [x] **1-1**: [/repo/go] 状態を作る
  - **保護領域**: モジュール間の依存関係 (flowからplanへの依存)
  - **想定行数**: 約100行 (実装60行 + テスト40行)
- [x] **1-2**: [shimbaco] タグを付ける
- [x] **1-3**: [/repo/go] 表示を直す
`

var t0 = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// atはt0からm分後の時刻を返す。
func at(m int) time.Time {
	return t0.Add(time.Duration(m) * time.Minute)
}

// newInputは、タスク1-1を終えた計画書の入力を作る。
func newInput(t *testing.T, src string) Input {
	t.Helper()
	p, err := plan.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{Tasks: []state.TaskRecord{{
		ID: "1-1", Repo: "/repo", Dir: "/repo/go", Branch: "demo-1-1", Base: "develop", Prepared: true,
		ReviewTrail: state.ReviewTrail{
			Reviews: []state.ReviewRecord{
				{Number: 1, Role: state.RoleReviewerCodex, Doc: "docs/private/reviews/demo-1-1-001.md", Verdict: "Comment"},
			},
			Deferred: []state.DeferredFinding{{Doc: "docs/private/reviews/demo-1-1-001.md", Title: "名前|を改める", Choice: "後続タスクに積む (推奨)"}},
		},
		Verification: []state.VerificationItem{
			{Kind: "process", Title: "statusを表示できる", Target: "korylusflow status demo.md", Steps: []string{"実行する"}, Expected: "一覧が出る"},
			{Kind: "screen", Title: "画面を開ける", Target: "/settings", Steps: []string{"ログインする", "開く"}, Expected: "表示される"},
		},
	}}}
	return Input{
		PlanURL: "https://wikino.app/s/shimbaco/pages/12", Name: "demo", Plan: p, State: st,
		Events: []state.Event{
			{Task: "1-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(0), Ended: at(30)},
			{Task: "1-1", Step: state.StepReview, Role: state.RoleReviewerCodex, Started: at(30), Ended: at(50)},
			{Task: "1-1", Step: state.StepFix, Role: state.RoleReviewerCodex, Started: at(50), Ended: at(60)},
			{Task: "1-2", Step: state.StepAsk, Role: state.RoleImplementer, Started: at(60), Ended: at(105), Reason: "人が行うタスクです"},
		},
		Changes: map[string]Changes{"1-1": {
			Files: []branch.FileChange{
				{Path: "go/internal/state.go", Added: 50, Removed: 5},
				{Path: "go/internal/state_test.go", Added: 30},
				{Path: "go/testdata/a.png", Binary: true},
			},
		}},
	}
}

// assertContainsはoutがwantsをすべて含むことを確かめる。
func assertContains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out)
		}
	}
}

func TestBuild(t *testing.T) {
	t.Parallel()

	out := string(Build(newInput(t, testPlan)))
	assertContains(t, out,
		"# 計画書の事実: demo\n",
		"- 計画書: `https://wikino.app/s/shimbaco/pages/12`\n",
		"- 計画書の開始: 2026-10-22 09:00 +00:00\n",
		"- 作業報告書: まだ書いていません\n",
		"### マージするPR\n\nリポジトリごとに、PRにするブランチをpushしてベースへのPRを1つ作り、merge commitでマージしてください。\n",
		"| `/repo` | `demo-1-1` | `develop` | 1-1 |\n",
		"## タスクごとの変更\n\n変更行数は、元のブランチからタスクのブランチまでの追加と削除の合計です。\n\n",
		"| 1-1 [/repo/go] 状態を作る | `demo-1-1` (元: `develop`) | テスト以外55行・テスト30行 | 約100行 (実装60行 + テスト40行) | Comment |\n",
		`| 1-1 | 名前\|を改める | 後続タスクに積む (推奨) |`,
		"- 宣言: モジュール間の依存関係 (flowからplanへの依存)\n",
		"- 差分を見るコマンド: `git -C /repo diff develop...demo-1-1`\n",
		"コードを変更しない方針を選んだ指摘です。\n",
	)
	// 動作確認の項目は作業報告書の材料で、事実としては出さない。
	for _, unwanted := range []string{"1-3", "## 人を待った履歴", "## 所要時間", "状態をどこに置くか?", "a.png", "コミット:", "動作確認", "statusを表示できる", "全体へのコメント"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("出力に %q がある:\n%s", unwanted, out)
		}
	}
}

func TestBuildWithoutTasks(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	in.State.Tasks, in.Events = nil, nil
	out := string(Build(in))
	assertContains(t, out,
		"ブランチを作ったタスクはありません。\n",
		"## 先送りした指摘\n\nなし\n",
		"## タスクごとの変更\n\nなし\n",
		"保護領域を宣言したタスクはありません。\n",
	)
}

func TestBuildUnreadableChanges(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	in.Changes["1-1"] = Changes{Err: "git log が失敗しました\nunknown revision"}
	assertContains(t, string(Build(in)), "| 読めません (git log が失敗しました unknown revision) |")
}

func TestBuildMergePRs(t *testing.T) {
	t.Parallel()

	// /repoは1-2で前のブランチが消えて最初のベースから作り直したため、2本に分かれる。
	in := newInput(t, testPlan)
	in.State.Tasks = append(in.State.Tasks,
		state.TaskRecord{ID: "1-2", Repo: "/repo", Dir: "/repo", Branch: "demo-1-2", Base: "develop", Prepared: true},
		state.TaskRecord{ID: "1-3", Repo: "/tools", Dir: "/tools", Branch: "demo-1-3", Base: "main", Prepared: true},
		state.TaskRecord{ID: "1-4", Repo: "/repo", Dir: "/repo", Branch: "demo-1-4", Base: "demo-1-2", Prepared: true},
		state.TaskRecord{ID: "1-5", Repo: "/tools", Dir: "/tools", Branch: "demo-1-5", Base: "demo-1-3", Prepared: true},
	)
	for _, id := range []string{"1-2", "1-3", "1-4", "1-5"} {
		in.Events = append(in.Events, state.Event{Task: id, Step: state.StepImplement, Role: state.RoleImplementer, Started: at(110), Ended: at(115)})
	}
	in.Changes["1-1"] = Changes{Missing: []string{"demo-1-1"}}
	out := string(Build(in))
	assertContains(t, out,
		"| `/repo` | `demo-1-1` (ローカルに残っていません) | `develop` | 1-1 |\n"+
			"| `/repo` | `demo-1-4` | `develop` | 1-2・1-4 |\n"+
			"| `/tools` | `demo-1-5` | `main` | 1-3・1-5 |\n",
		"同じリポジトリに複数の行があるときは、ブランチが1本に繋がっていません",
	)

	// 1本に繋がっていれば、繋がっていないときの説明を出さない。
	if out := string(Build(newInput(t, testPlan))); strings.Contains(out, "1本に繋がっていません") {
		t.Errorf("繋がっているのに説明を出している:\n%s", out)
	}
}

func TestBuildMissingBranches(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	in.Changes["1-1"] = Changes{Missing: []string{"demo-1-1", "develop"}}
	out := string(Build(in))
	assertContains(t, out, "| ブランチ `demo-1-1`・`develop` がローカルに残っていないため示せません |")
	if strings.Contains(out, "読めません") {
		t.Errorf("ブランチが残っていないときにgitのエラーを示している:\n%s", out)
	}
}

func TestIsTest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{"go/internal/flow/flow_test.go", true},
		{"rails/spec/models/user_spec.rb", true},
		{"rails/spec/support/helper.rb", true},
		{"web/src/app.test.ts", true},
		{"internal/md/testdata/a.md", true},
		{"go/internal/flow/flow.go", false},
		{"docs/testing.md", false},
	}
	for _, tt := range tests {
		if got := isTest(tt.path); got != tt.want {
			t.Errorf("isTest(%q) = %t、期待値 = %t", tt.path, got, tt.want)
		}
	}
}

func TestExpected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		size plan.Size
		want string
	}{
		{plan.Size{}, "記載なし"},
		{plan.Size{Total: 30, Parts: map[string]int{}}, "約30行"},
		{plan.Size{Total: 90, Parts: map[string]int{"ドキュメント": 10, "テスト": 30, "実装": 50}}, "約90行 (実装50行 + テスト30行 + ドキュメント10行)"},
	}
	for _, tt := range tests {
		if got := expected(tt.size); got != tt.want {
			t.Errorf("expected(%+v) = %q、期待値 = %q", tt.size, got, tt.want)
		}
	}
}

func TestCode(t *testing.T) {
	t.Parallel()

	if got, want := code("echo `date`"), "`` echo `date` ``"; got != want {
		t.Errorf("code() = %q、期待値 = %q", got, want)
	}
}

func TestTaskLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		task plan.Task
		want string
	}{
		{id: "1-1", task: plan.Task{}, want: "1-1"},
		{id: "1-2", task: plan.Task{Platform: "shimbaco", Title: "タグを付ける"}, want: "1-2 [shimbaco] タグを付ける"},
		{id: "1-3", task: plan.Task{Title: "下書きを公開する", Assignee: "shimbaco"}, want: "1-3 下書きを公開する (担当: shimbaco)"},
		{id: "1-4", task: plan.Task{Platform: "/korylus-tools", Title: "直す", Assignee: "implementer"}, want: "1-4 [/korylus-tools] 直す (担当: implementer)"},
	}
	for _, tt := range tests {
		if got := taskLabel(tt.id, tt.task); got != tt.want {
			t.Errorf("taskLabel(%q, %+v) = %q、期待値 = %q", tt.id, tt.task, got, tt.want)
		}
	}
}

func TestBuildImplementerTasks(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	out := string(Build(in))
	assertContains(t, out, "## 実装者が行ったタスク\n\nなし\n")

	// 実装のステップを記録したのに記録を持たないタスクを、進めた順に1度だけ並べる。
	in.Events = append(in.Events,
		state.Event{Task: "2-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(200), Ended: at(210)},
		state.Event{Task: "2-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(210), Ended: at(220)},
	)
	in.Events = append([]state.Event{{Task: "0-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(0), Ended: at(1)}}, in.Events...)
	out = string(Build(in))
	assertContains(t, out, "## 実装者が行ったタスク\n\nブランチを作らずに実装者が行い、人が結果を確かめたタスクです。\n\n- 0-1\n- 2-1\n")
}

func TestBuildAssignee(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		old         string
		replacement string
		work        bool
		want        string
	}{
		{
			name:        "ブランチを作る実装者の担当をタスクごとの変更に表示する",
			old:         "- [x] **1-1**: [/repo/go] 状態を作る",
			replacement: "- [x] **1-1**: [/repo/go] 状態を作る\n  - **担当**: implementer",
			want:        "| 1-1 [/repo/go] 状態を作る (担当: implementer) | `demo-1-1`",
		},
		{
			name:        "ブランチを作らない実装者の担当をタスク一覧に表示する",
			old:         "- [x] **1-2**: [shimbaco] タグを付ける",
			replacement: "- [x] **1-2**: 下書きを作る\n  - **担当**: implementer",
			work:        true,
			want:        "- 1-2 下書きを作る (担当: implementer)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := strings.Replace(testPlan, tt.old, tt.replacement, 1)
			in := newInput(t, src)
			if tt.work {
				for i := range in.Events {
					if in.Events[i].Task == "1-2" {
						in.Events[i].Step = state.StepImplement
						in.Events[i].Reason = ""
					}
				}
			}
			assertContains(t, string(Build(in)), tt.want)
		})
	}
}

func TestReviewVerdicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reviews []state.ReviewRecord
		want    string
	}{
		{nil, "なし"},
		{[]state.ReviewRecord{{Number: 1, Verdict: "Comment"}}, "Comment"},
		{[]state.ReviewRecord{{Number: 1, Verdict: "Request Changes"}, {Number: 2, Verdict: "Approve"}}, "Request Changes → Approve"},
	}
	for _, tt := range tests {
		if got := reviewVerdicts(tt.reviews); got != tt.want {
			t.Errorf("reviewVerdicts(%+v) = %q、期待値 = %q", tt.reviews, got, tt.want)
		}
	}
}

func TestBuildEscapesExpectedLines(t *testing.T) {
	t.Parallel()

	src := strings.Replace(testPlan, "約100行 (実装60行 + テスト40行)", "約100行 (UI|UX100行)", 1)
	out := Build(newInput(t, src))
	var rendered bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.Table)).Convert(out, &rendered); err != nil {
		t.Fatal(err)
	}
	assertContains(t, rendered.String(), "<td>約100行 (UI|UX100行)</td>\n<td>Comment</td>")
}

func TestBuildReportLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reports []state.ReportRound
		want    string
	}{
		{"まだ書いていなければそう書く", nil, "- 作業報告書: まだ書いていません\n"},
		{
			"最新の回のURLをそのまま書く",
			[]state.ReportRound{{Number: 1, Location: "https://wikino.app/s/demo/pages/1"}, {Number: 2, Location: "https://wikino.app/s/demo/pages/2"}},
			"- 作業報告書: 2回目 https://wikino.app/s/demo/pages/2\n",
		},
		{
			"処理の流れを書かなかった理由を添える",
			[]state.ReportRound{{Number: 1, Location: "https://wikino.app/s/demo/pages/1", WalkthroughSkipped: "文面だけ|直した\nため"}},
			`- 作業報告書: 1回目 https://wikino.app/s/demo/pages/1 (処理の流れは書いていません: 文面だけ\|直した ため)` + "\n",
		},
		{
			"Markdownの作業報告書だった回はページが無いと書く",
			[]state.ReportRound{{Number: 1}},
			"- 作業報告書: 1回目 (Markdownの作業報告書で、ページはありません)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := newInput(t, testPlan)
			in.State.Reports = tt.reports
			assertContains(t, string(Build(in)), tt.want)
		})
	}
}

func TestChains(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	in.State.Tasks = append(in.State.Tasks,
		state.TaskRecord{ID: "1-2", Repo: "/repo", Dir: "/repo", Branch: "demo-1-2", Base: "demo-1-1", Prepared: true},
		// 実装を終えていないタスクは並べない。
		state.TaskRecord{ID: "1-3", Repo: "/repo", Dir: "/repo", Branch: "demo-1-3", Base: "demo-1-2", Prepared: true},
	)
	in.Events = append(in.Events, state.Event{Task: "1-2", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(110), Ended: at(115)})
	got := Chains(in.State, in.Events)
	if len(got) != 1 || got[0].Head.Branch != "demo-1-2" || got[0].Base != "develop" || strings.Join(got[0].Tasks, ",") != "1-1,1-2" {
		t.Errorf("Chains() = %+v、/repoのdemo-1-2 (ベースdevelop、1-1と1-2) の1本を期待", got)
	}
}
