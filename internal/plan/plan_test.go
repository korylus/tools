package plan

import (
	"reflect"
	"strings"
	"testing"
)

// samplePlanは計画書テンプレートの書式に沿った計画書の例。タスクリスト節の
// 外と、HTMLコメント・コードブロックの中にあるタスク行は読み取らない。
const samplePlan = "# 例の作業計画書\n" +
	"\n" +
	"## 設計\n" +
	"\n" +
	"- [ ] **9-1**: [Go] 設計節のタスク行は読まない\n" +
	"\n" +
	"## タスクリスト\n" +
	"\n" +
	"<!--\n" +
	"- [ ] **1-1**: [Go] コメントの中の例は読まない\n" +
	"-->\n" +
	"\n" +
	"### フェーズ1: 基盤\n" +
	"\n" +
	"- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n" +
	"  - サブタスク\n" +
	"  - **保護領域**: schema (posts に visibility 列を追加), module-dependency (usecase → repository の新しい依存)\n" +
	"  - **想定ファイル数**: 約6ファイル (実装4 + テスト2)\n" +
	"  - **想定行数**: 約300行 (実装150行 + テスト150行)\n" +
	"\n" +
	"- [ ] **2-0-1**: 接頭辞の無いタスク\n" +
	"  - **想定行数**: 約1,050行 (実装400行 + ドキュメント200行 + テスト450行)\n" +
	"\n" +
	"```markdown\n" +
	"- [ ] **8-1**: [Go] コードブロックの中の例は読まない\n" +
	"```\n" +
	"\n" +
	"### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n" +
	"\n" +
	"- [X] **fb1-1**: [Rails] 保存後に編集画面へ戻す\n" +
	"  - **想定行数**: 約30行\n" +
	"  - **対応する確認項目**: 第1版 #2 「保存できる」、第1版 #5 「戻る」\n" +
	"\n" +
	"## 恒久判断記録への反映\n" +
	"\n" +
	"- [ ] **7-1**: [Go] 後ろの節のタスク行は読まない\n"

func TestParse(t *testing.T) {
	t.Parallel()

	p, err := Parse(samplePlan)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}

	want := []Task{
		{
			ID:       "1-1",
			Done:     true,
			Platform: "Go",
			Title:    "投稿の公開範囲を保存する",
			Protected: []Declaration{
				{Name: "schema", Note: "posts に visibility 列を追加"},
				{Name: "module-dependency", Note: "usecase → repository の新しい依存"},
			},
			Lines:  Size{Total: 300, Parts: map[string]int{"実装": 150, "テスト": 150}},
			LineNo: 15,
		},
		{
			ID:     "2-0-1",
			Title:  "接頭辞の無いタスク",
			Lines:  Size{Total: 1050, Parts: map[string]int{"実装": 400, "ドキュメント": 200, "テスト": 450}},
			LineNo: 21,
		},
		{
			ID:       "fb1-1",
			Done:     true,
			Platform: "Rails",
			Title:    "保存後に編集画面へ戻す",
			Lines:    Size{Total: 30, Parts: map[string]int{}},
			Verifies: []ItemRef{{Version: 1, Number: 2}, {Version: 1, Number: 5}},
			LineNo:   30,
		},
	}
	if !reflect.DeepEqual(p.Tasks, want) {
		t.Errorf("Tasks = %+v\n期待値 = %+v", p.Tasks, want)
	}
}

func TestParse_Invalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want string // エラーメッセージに含まれることを期待する文字列
	}{
		{
			name: "タスクリスト節が無い",
			src:  "## 設計\n\n- [ ] **1-1**: [Go] a\n",
			want: "「タスクリスト」節にタスクが見つかりません",
		},
		{
			name: "タスク番号の重複",
			src:  "## タスクリスト\n\n- [ ] **1-1**: a\n- [ ] **1-1**: b\n",
			want: "4行目: タスク番号 1-1 が重複しています (3行目と同じ)",
		},
		{
			name: "保護領域の名前が無い",
			src:  "## タスクリスト\n\n- [ ] **1-1**: a\n  - **保護領域**: (触れる場合だけ書く)\n",
			want: "4行目: タスク 1-1 の保護領域: 保護領域の名前がありません",
		},
		{
			name: "保護領域の括弧が閉じていない",
			src:  "## タスクリスト\n\n- [ ] **1-1**: a\n  - **保護領域**: schema (列を追加\n",
			want: "括弧が閉じていません",
		},
		{
			name: "想定行数が数字で始まらない",
			src:  "## タスクリスト\n\n- [ ] **1-1**: a\n  - **想定行数**: 約N行 (実装N行 + テストN行)\n",
			want: "4行目: タスク 1-1 の想定行数: 「約N行」の形で書いてください",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.src)
			if err == nil {
				t.Fatal("エラーが返らなかった")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("エラー = %q、含まれることを期待した文字列 = %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParse_TaskFieldsEndAtUnindentedLine(t *testing.T) {
	t.Parallel()

	src := "## タスクリスト\n\n- [ ] **1-1**: a\n\n地の文\n  - **想定行数**: 約10行\n"
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}
	if p.Tasks[0].Lines.Total != 0 {
		t.Errorf("地の文のあとの想定行数を拾った: %+v", p.Tasks[0].Lines)
	}
}

func TestParse_CRLF(t *testing.T) {
	t.Parallel()

	src := "## タスクリスト\r\n\r\n- [ ] **1-1**: [Go] a\r\n  - **想定行数**: 約10行 (実装5行 + テスト5行)\r\n"
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}
	if got := p.Tasks[0]; got.Title != "a" || got.Lines.Total != 10 {
		t.Errorf("Tasks[0] = %+v", got)
	}
}

func TestTaskPhase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id        string
		wantPhase string
		wantRound int
		wantFB    bool
	}{
		{id: "1-2", wantPhase: "1"},
		{id: "2-0-1", wantPhase: "2"},
		{id: "5a-1", wantPhase: "5a"},
		{id: "fb1-1", wantPhase: "fb1", wantRound: 1, wantFB: true},
		{id: "fb12-3", wantPhase: "fb12", wantRound: 12, wantFB: true},
		{id: "fbx-1", wantPhase: "fbx"},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			t.Parallel()

			task := Task{ID: tt.id}
			if got := task.Phase(); got != tt.wantPhase {
				t.Errorf("Phase() = %q、期待値 = %q", got, tt.wantPhase)
			}
			round, ok := task.FeedbackRound()
			if round != tt.wantRound || ok != tt.wantFB {
				t.Errorf("FeedbackRound() = (%d, %v)、期待値 = (%d, %v)", round, ok, tt.wantRound, tt.wantFB)
			}
		})
	}
}

func TestPlanProgress(t *testing.T) {
	t.Parallel()

	p := &Plan{Tasks: []Task{{ID: "1-1", Done: true}, {ID: "1-2"}, {ID: "1-3"}}}
	if got := p.DoneCount(); got != 1 {
		t.Errorf("DoneCount() = %d、期待値 = 1", got)
	}
	if next, ok := p.Next(); !ok || next.ID != "1-2" {
		t.Errorf("Next() = (%+v, %v)、期待値 = 1-2", next, ok)
	}

	p.Tasks[1].Done, p.Tasks[2].Done = true, true
	if _, ok := p.Next(); ok {
		t.Error("すべて完了しているのにNext() がタスクを返した")
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	if got := Name("docs/private/plans/1_doing/koryluspilot.md"); got != "koryluspilot" {
		t.Errorf("Name() = %q、期待値 = %q", got, "koryluspilot")
	}
}

func TestParse_CodeFences(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, opening, body, closing string }{
		{"短いバッククォートで終了しない", "````markdown", "```text\n- [ ] **9-1**: 例\n```", "````"},
		{"異なる文字種のチルダで終了しない", "```text", "~~~\n- [ ] **9-1**: 例", "```"},
		{"異なる文字種のバッククォートで終了しない", "~~~text", "```\n- [ ] **9-1**: 例", "~~~"},
		{"短いチルダで終了しない", "~~~~text", "~~~\n- [ ] **9-1**: 例", "~~~~"},
		{"終端の後ろの文字列を許可しない", "```text", "```text\n- [ ] **9-1**: 例", "```"},
		{"長い終端と末尾の空白を許可する", "```text", "- [ ] **9-1**: 例", "````` \t"},
		{"4スペースの行で終了しない", "```text", "    ```\n- [ ] **9-1**: 例", "```"},
		{"タブでインデントした行で終了しない", "```text", "\t```\n- [ ] **9-1**: 例", "```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := "## タスクリスト\n- [ ] **1-1**: 実タスクA\n\n" + tt.opening + "\n" + tt.body + "\n" + tt.closing + "\n\n- [ ] **1-2**: 実タスクB\n"
			p, err := Parse(src)
			if err != nil {
				t.Fatalf("Parseのエラー = %v", err)
			}
			var ids []string
			for _, task := range p.Tasks {
				ids = append(ids, task.ID)
			}
			want := []string{"1-1", "1-2"}
			if !reflect.DeepEqual(ids, want) {
				t.Errorf("タスク番号 = %v、期待値 = %v", ids, want)
			}
		})
	}
}

func TestParse_InvalidOpeningFence(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"    ~~~", "\t~~~", "```text`"} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			p, err := Parse("## タスクリスト\n" + line + "\n\n- [ ] **1-1**: 実タスク\n")
			if err != nil {
				t.Fatalf("Parseのエラー = %v", err)
			}
			if len(p.Tasks) != 1 || p.Tasks[0].ID != "1-1" {
				t.Errorf("タスク = %+v、期待値 = 1-1のみ", p.Tasks)
			}
		})
	}
}

func TestParse_CodeFenceInListItem(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, src string }{
		{
			name: "リスト項目の内容に揃えてインデントしたフェンス",
			src:  "## タスクリスト\n\n- [ ] **1-1**: A\n  - 例:\n\n    ```markdown\n    - **保護領域**: schema\n    ```\n\n- [ ] **1-2**: B\n",
		},
		{
			name: "入れ子のリスト項目の中のフェンス",
			src:  "## タスクリスト\n\n- [ ] **1-1**: A\n  - 例:\n    - 書式:\n\n      ~~~markdown\n      - **保護領域**: schema\n      ~~~\n\n- [ ] **1-2**: B\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := Parse(tt.src)
			if err != nil {
				t.Fatalf("Parseのエラー = %v", err)
			}
			if len(p.Tasks) != 2 {
				t.Fatalf("タスク数 = %d、期待値 = 2", len(p.Tasks))
			}
			if got := p.Tasks[0].Protected; len(got) != 0 {
				t.Errorf("1-1の保護領域 = %+v、期待値 = 空 (コードブロックの中の例は読まない)", got)
			}
		})
	}
}

func TestParse_IndentedFenceOutsideList(t *testing.T) {
	t.Parallel()
	// リスト項目の外では、3スペースまでインデントしたフェンスもコードブロックになる。
	p, err := Parse("## タスクリスト\n\n   ~~~text\n- [ ] **9-1**: 例\n  ~~~\n\n- [ ] **1-1**: 実タスク\n")
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}
	if len(p.Tasks) != 1 || p.Tasks[0].ID != "1-1" {
		t.Errorf("タスク = %+v、期待値 = 1-1のみ", p.Tasks)
	}
}
