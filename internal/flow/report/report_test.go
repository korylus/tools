package report

import (
	"strings"
	"testing"
	"time"

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

// newInputは、タスク1-1を終えた計画書の第1版の入力を作る。
func newInput(t *testing.T, src string) Input {
	t.Helper()
	p, err := plan.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	st := &state.State{Tasks: []state.TaskRecord{{
		ID: "1-1", Repo: "/repo", Dir: "/repo/go", Branch: "demo-1-1", Base: "develop", Prepared: true,
		Reviews: []state.ReviewRecord{
			{Number: 1, Role: state.RoleReviewerCodex, Doc: "docs/private/reviews/demo-1-1-001.md", Verdict: "Comment"},
		},
		Deferred: []state.DeferredFinding{{Doc: "docs/private/reviews/demo-1-1-001.md", Title: "名前|を改める", Choice: "後続タスクに積む (推奨)"}},
		Verification: []state.VerificationItem{
			{Kind: "process", Title: "statusを表示できる", Target: "korylusflow status demo.md", Steps: []string{"実行する"}, Expected: "一覧が出る"},
			{Kind: "screen", Title: "画面を開ける", Target: "/settings", Steps: []string{"ログインする", "開く"}, Expected: "表示される"},
		},
	}}}
	return Input{
		PlanPath: "docs/private/plans/1_doing/demo.md", PlanSrc: src, Plan: p, Version: 1, Now: at(120), State: st,
		Events: []state.Event{
			{Task: "1-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(0), Ended: at(30)},
			{Task: "1-1", Step: state.StepReview, Role: state.RoleReviewerCodex, Started: at(30), Ended: at(50)},
			{Task: "1-1", Step: state.StepFix, Role: state.RoleReviewerCodex, Started: at(50), Ended: at(60)},
			{Task: "1-2", Step: state.StepAsk, Role: state.RoleImplementer, Started: at(60), Ended: at(105), Reason: "人が行うタスクです"},
		},
		Changes: map[string]Changes{"1-1": {
			Commits: []branch.Commit{{Hash: "abc1234", Subject: "状態を作る"}},
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
			t.Errorf("作業報告書に %q が無い:\n%s", want, out)
		}
	}
}

// answerは作業報告書outの項目numberのチェックボックスstatusにチェックを付け、コメントを書く。
func answer(t *testing.T, out string, number, status, comment string) string {
	t.Helper()
	start := strings.Index(out, "### #"+number+" ")
	if start < 0 {
		t.Fatalf("項目 #%s が無い:\n%s", number, out)
	}
	block := out[start:]
	if end := strings.Index(block[1:], "\n### "); end >= 0 {
		block = block[:end+1]
	}
	answered := strings.Replace(block, "- [ ] "+status, "- [x] "+status, 1)
	answered = strings.Replace(answered, "コメント:\n", "コメント:\n\n"+comment+"\n", 1)
	return strings.Replace(out, block, answered, 1)
}

func TestBuild(t *testing.T) {
	t.Parallel()

	out := string(Build(newInput(t, testPlan)))
	assertContains(t, out,
		"# 作業報告書 第1版: demo\n",
		"- 計画書: `docs/private/plans/1_doing/demo.md`\n",
		"- 計画書の開始: 2026-10-22 09:00 +00:00\n",
		"| `/repo` | 1-1 | `demo-1-1` | `develop` |\n",
		"### 1-1 [/repo/go] 状態を作る\n",
		"- 変更行数: テスト以外55行、テスト30行 (追加と削除の合計)\n",
		"- 想定行数: 約100行 (実装60行 + テスト40行)\n",
		"- `abc1234` 状態を作る\n",
		"| `go/testdata/a.png` | - | - |\n",
		"| 1 | レビュアー (Codex) | Comment | `docs/private/reviews/demo-1-1-001.md` |\n",
		"- 状態をどこに置くか?\n",
		`| 1-1 | 名前\|を改める | 後続タスクに積む (推奨) |`,
		"| 1-2 | 実装者 | 人が行うタスクです | 2026-10-22 10:00 +00:00 | 45分 |\n",
		"- この版の合計: 1時間45分\n",
		"| レビュアー (Codex) | 修正 | 1 | 10分 | 10分 |\n",
		"| 1-1 | 30分 | 20分 | 10分 | 0分 | 1時間0分 |\n",
		"| 1-2 | 0分 | 0分 | 0分 | 45分 | 45分 |\n",
		"- 宣言: モジュール間の依存関係 (flowからplanへの依存)\n",
		"- 差分を見るコマンド: `git -C /repo diff develop...demo-1-1`\n",
		"### #1 statusを表示できる\n\n<!-- item: task/1-1/1 -->\n",
		"- 種類: 処理\n- コマンド: `korylusflow status demo.md`\n",
		"### #2 画面を開ける\n",
		"- パス: `/settings`\n",
		"1. ログインする\n2. 開く\n",
		"### #3 保護領域「モジュール間の依存関係」の変更が合意した設計どおりになっている\n\n<!-- item: protected/1-1/モジュール間の依存関係 -->\n",
		"\n結果:\n\n- [ ] OK\n- [ ] NG\n\nコメント:\n",
		"コードを変更しない方針を選んだ指摘です。\n",
		"## 全体へのコメント\n",
	)
	for _, unwanted := range []string{"1-3", "この版で進めた", "前の版"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("作業報告書に %q がある:\n%s", unwanted, out)
		}
	}
}

func TestBuildNextVersion(t *testing.T) {
	t.Parallel()

	first := string(Build(newInput(t, testPlan)))
	first = answer(t, first, "1", "NG", "一覧が空だった")
	first = answer(t, first, "2", "OK", "")
	first += "\nフォントが小さい\n"
	prev := ParseResults(first)

	src := testPlan + "\n### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n\n" +
		"- [x] **fb1-1**: [/repo/go] 一覧を直す\n  - **対応する確認項目**: 第1版 #1 「statusを表示できる」\n"
	in := newInput(t, src)
	in.Version, in.Since, in.Previous, in.Now = 2, len(in.Events), prev, at(300)
	in.State.Tasks = append(in.State.Tasks, state.TaskRecord{
		ID: "fb1-1", Repo: "/repo", Dir: "/repo/go", Branch: "demo-fb1-1", Base: "demo-1-1", Prepared: true,
		Verification: []state.VerificationItem{{Kind: "process", Title: "一覧が出る", Target: "korylusflow status", Steps: []string{"実行する"}, Expected: "1件以上出る"}},
	})
	in.Events = append(in.Events, state.Event{Task: "fb1-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: at(200), Ended: at(230)})

	out := string(Build(in))
	assertContains(t, out,
		"- この版の期間: 2026-10-22 12:20 +00:00 〜 2026-10-22 14:00 +00:00\n",
		"- 前の版: 第1版\n\n前の版の全体へのコメント:\n\n> フォントが小さい\n",
		"### fb1-1 [/repo/go] 一覧を直す\n\nこの版で進めたタスクです。\n",
		"| `/repo` | fb1-1 | `demo-fb1-1` | `demo-1-1` |\n",
		"- この版の合計: 30分\n- 版をまたいだ合計: 2時間15分\n",
		"### #1 statusを表示できる\n\n<!-- item: task/1-1/1 -->\n",
		"- 前の版の結果: 第1版 #1 NG\n- 対応したフィードバックのタスク: fb1-1\n\n前の版のコメント:\n\n> 一覧が空だった\n",
		"### #2 一覧が出る\n",
		"### #3 保護領域「モジュール間の依存関係」",
		"- 前の版の結果: 第1版 #3 未確認\n",
		"### 確認済みの項目\n",
		"- #4 画面を開ける (対象タスク: 1-1) <!-- confirmed: task/1-1/2 -->\n",
	)
	if strings.Contains(out, "### 1-1 [/repo/go] 状態を作る\n\nこの版で") {
		t.Errorf("前の版のタスクを、この版で進めたタスクとして表示した:\n%s", out)
	}

	// 確認済みの項目は、次の版でも確認済みのままにする。
	in.Version, in.Previous = 3, ParseResults(out)
	third := string(Build(in))
	assertContains(t, third, "<!-- confirmed: task/1-1/2 -->")
	if strings.Contains(third, "### #4 画面を開ける") {
		t.Errorf("確認済みの項目を、チェックリストに戻した:\n%s", third)
	}
}

func TestBuildKeepsCurrentResults(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	written := answer(t, string(Build(in)), "2", "NG", "ボタンが無い\n2行目")
	written += "\n全体のメモ\n"
	in.Current = ParseResults(written)

	out := string(Build(in))
	assertContains(t, out,
		"### #2 画面を開ける\n",
		"- [ ] OK\n- [x] NG\n\nコメント:\n\nボタンが無い\n2行目\n",
		"<!-- チェックリストに無い気づきを、この下に書く -->\n\n全体のメモ\n",
	)
	if got := ParseResults(out); got.Items["task/1-1/2"] != in.Current.Items["task/1-1/2"] || got.Comment != "全体のメモ" {
		t.Errorf("作り直した作業報告書の結果 = %+v、期待値 = %+v", got, in.Current)
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
		"## 人を待った履歴\n\nなし\n",
		"## 所要時間\n\n記録なし\n",
		"保護領域を宣言したタスクはありません。\n",
		"確認する項目はありません。\n",
	)
}

func TestBuildUnreadableChanges(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	in.Changes["1-1"] = Changes{Err: "git log が失敗しました\nunknown revision"}
	assertContains(t, string(Build(in)), "- 変更: 読めません (git log が失敗しました unknown revision)\n")
}

func TestParseResults(t *testing.T) {
	t.Parallel()

	src := `## 動作確認チェックリスト

### #1 一覧

<!-- item: task/1-1/1 -->

- [X] OK
- [x] NG

コメント:

<!-- 案内 -->
1行目

2行目

### #2 画面

<!-- item: task/1-1/2 -->

* [x] OK
* [ ] NG

コメント:

### #3 IDの無い項目

- [x] OK

### 確認済みの項目

- #4 保護領域 (対象タスク: 1-1) <!-- confirmed: protected/1-1/依存 -->

## 全体へのコメント

<!-- チェックリストに無い気づきを、この下に書く -->

全体の
メモ
`
	got := ParseResults(src)
	want := map[string]Result{
		"task/1-1/1": {Number: 1, Status: StatusNG, Comment: "<!-- 案内 -->\n1行目\n\n2行目"},
		"task/1-1/2": {Number: 2, Status: StatusOK, Comment: "### #3 IDの無い項目\n\n- [x] OK"},
	}
	if len(got.Items) != len(want) {
		t.Errorf("項目の数 = %d、期待値 = %d (%+v)", len(got.Items), len(want), got.Items)
	}
	for id, w := range want {
		if got.Items[id] != w {
			t.Errorf("項目 %s = %+v、期待値 = %+v", id, got.Items[id], w)
		}
	}
	if !got.Confirmed["protected/1-1/依存"] || len(got.Confirmed) != 1 {
		t.Errorf("確認済みの項目 = %v", got.Confirmed)
	}
	if got.Comment != "全体の\nメモ" {
		t.Errorf("全体へのコメント = %q", got.Comment)
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
