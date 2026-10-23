package plan

import (
	"strings"
	"testing"
)

// snapshotPlanは計画書のスナップショットの例。
const snapshotPlan = "# 例の作業計画書\n" +
	"\n" +
	"## 設計\n" +
	"\n" +
	"```markdown\n" +
	"## コードブロックの中の見出しは節の区切りにしない\n" +
	"```\n" +
	"\n" +
	"## 実装判断ログ\n" +
	"\n" +
	"### タスク1-1: 投稿の公開範囲を保存する\n" +
	"\n" +
	"#### 列の型をどうするか?\n" +
	"\n" +
	"## タスクリスト\n" +
	"\n" +
	"- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n" +
	"\n" +
	"- [ ] **1-2**: [Go] 公開範囲を表示する\n" +
	"  - [ ] 一覧に表示する\n" +
	"  - [ ] 詳細に表示する\n" +
	"\n" +
	"- [ ] **1-3**: [Go] 公開範囲を編集する\n" +
	"\n" +
	"## 参考資料\n" +
	"\n" +
	"- なし\n"

func TestCheckTaskChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		want    []string // 違反のメッセージに含まれることを期待する文字列 (空なら違反なし)
	}{
		{
			name:    "変更なし",
			current: snapshotPlan,
		},
		{
			name: "タスクとサブタスクのチェックと、実装判断ログへの追記",
			current: strings.NewReplacer(
				"- [ ] **1-2**", "- [x] **1-2**",
				"  - [ ] 一覧に表示する", "  - [x] 一覧に表示する",
				"#### 列の型をどうするか?\n", "#### 列の型をどうするか?\n\n- [x] 文字列\n\n### タスク1-2: 公開範囲を表示する\n\n#### 表示する場所は?\n",
			).Replace(snapshotPlan),
		},
		{
			name:    "CRLFの改行に変わっても違反にしない",
			current: strings.ReplaceAll(strings.Replace(snapshotPlan, "- [ ] **1-2**", "- [x] **1-2**", 1), "\n", "\r\n"),
		},
		{
			name:    "別のタスクのチェック",
			current: strings.Replace(snapshotPlan, "- [ ] **1-3**", "- [x] **1-3**", 1),
			want:    []string{"「タスクリスト」節で、タスク 1-2 のチェック以外の行が変更されています"},
		},
		{
			name:    "完了済みのタスクのチェックを外す",
			current: strings.Replace(snapshotPlan, "- [x] **1-1**", "- [ ] **1-1**", 1),
			want:    []string{"タスク 1-2 のチェック以外"},
		},
		{
			name:    "対象のタスクの本文の変更",
			current: strings.Replace(snapshotPlan, "  - [ ] 詳細に表示する", "  - [ ] 詳細には表示しない", 1),
			want:    []string{"タスク 1-2 のチェック以外"},
		},
		{
			name:    "タスクの追加",
			current: strings.Replace(snapshotPlan, "- [ ] **1-3**: [Go] 公開範囲を編集する\n", "- [ ] **1-3**: [Go] 公開範囲を編集する\n- [ ] **1-4**: [Go] 追加\n", 1),
			want:    []string{"「タスクリスト」節の行が増減しています"},
		},
		{
			name:    "設計節の変更",
			current: strings.Replace(snapshotPlan, "## 設計\n\n", "## 設計\n\n新しい方針\n", 1),
			want:    []string{"「設計」節が変更されています"},
		},
		{
			name:    "コードブロックの中の変更も、その節の変更として扱う",
			current: strings.Replace(snapshotPlan, "## コードブロックの中の見出しは", "## コードブロックの中の見出しを", 1),
			want:    []string{"「設計」節が変更されています"},
		},
		{
			name:    "実装判断ログの既存の行の書き換え",
			current: strings.Replace(snapshotPlan, "#### 列の型をどうするか?", "#### 列の型は?", 1),
			want:    []string{"「実装判断ログ」節の既存の行が変更・削除されています"},
		},
		{
			name:    "実装判断ログの削除",
			current: strings.Replace(snapshotPlan, "## 実装判断ログ\n\n### タスク1-1: 投稿の公開範囲を保存する\n\n#### 列の型をどうするか?\n\n", "", 1),
			want:    []string{"「実装判断ログ」節が削除されています"},
		},
		{
			name:    "節の追加",
			current: snapshotPlan + "\n## 追加の節\n",
			want:    []string{`節 "## 追加の節" が追加されています`},
		},
		{
			name:    "節の見出しの変更",
			current: strings.Replace(snapshotPlan, "## 参考資料", "## 参考", 1),
			want:    []string{`節 "## 参考資料" が "## 参考" に変わっています`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vs := CheckTaskChanges(snapshotPlan, tt.current, "1-2")
			if len(tt.want) == 0 {
				if len(vs) > 0 {
					t.Errorf("違反 = %+v、違反なしを期待", vs)
				}
				return
			}
			var msgs []string
			for _, v := range vs {
				msgs = append(msgs, v.Message)
			}
			got := strings.Join(msgs, "\n")
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("違反 = %q、含まれることを期待した文字列 = %q", got, w)
				}
			}
		})
	}
}

func TestCheckTaskChanges_LineNo(t *testing.T) {
	t.Parallel()

	current := strings.Replace(snapshotPlan, "- [ ] **1-3**", "- [x] **1-3**", 1)
	vs := CheckTaskChanges(snapshotPlan, current, "1-2")
	if len(vs) != 1 {
		t.Fatalf("違反の数 = %d、期待値 = 1 (%+v)", len(vs), vs)
	}
	want := strings.Count(snapshotPlan[:strings.Index(snapshotPlan, "- [ ] **1-3**")], "\n") + 1
	if vs[0].LineNo != want {
		t.Errorf("LineNo = %d、期待値 = %d", vs[0].LineNo, want)
	}
}

func TestCheckTaskChanges_NewDecisionLog(t *testing.T) {
	t.Parallel()

	// 実装判断ログ節の無い計画書に、節ごと追加する。
	snapshot := strings.Replace(snapshotPlan, "## 実装判断ログ\n\n### タスク1-1: 投稿の公開範囲を保存する\n\n#### 列の型をどうするか?\n\n", "", 1)
	current := strings.Replace(snapshot, "## タスクリスト\n", "## 実装判断ログ\n\n### タスク1-2: 公開範囲を表示する\n\n## タスクリスト\n", 1)
	if vs := CheckTaskChanges(snapshot, current, "1-2"); len(vs) > 0 {
		t.Errorf("違反 = %+v、違反なしを期待", vs)
	}
}

// fbPhaseは、snapshotPlanのタスクリストの末尾に足すフェーズfbNの例。
func fbPhase(tasks ...string) string {
	return "### フェーズfb1: 動作確認のフィードバック対応 (第1版)\n\n" + strings.Join(tasks, "\n") + "\n\n"
}

// appendTasksは、snapshotPlanの「タスクリスト」節の末尾 (次の節の見出しの前) にaddedを足す。
func appendTasks(src, added string) string {
	return strings.Replace(src, "## 参考資料", added+"## 参考資料", 1)
}

func TestCheckFeedbackPhase(t *testing.T) {
	t.Parallel()

	fb11 := "- [ ] **fb1-1**: [Go] 一覧の表示を直す\n  - **対応する確認項目**: 第1版 #2"
	fb12 := "- [ ] **fb1-2**: [Go] 列を足す\n  - **保護領域**: schema (postsに列を追加)"
	tests := []struct {
		name      string
		snapshot  string
		current   string
		wantAdded []string
		want      []string // 違反のメッセージに含まれることを期待する文字列 (空なら違反なし)
	}{
		{
			name:      "フェーズfb1の追加",
			snapshot:  snapshotPlan,
			current:   appendTasks(snapshotPlan, fbPhase(fb11, fb12)),
			wantAdded: []string{"fb1-1", "fb1-2"},
		},
		{
			name:      "既存のフェーズfb1があればfb2を足す",
			snapshot:  appendTasks(snapshotPlan, fbPhase(strings.Replace(fb11, "[ ]", "[x]", 1))),
			current:   appendTasks(snapshotPlan, fbPhase(strings.Replace(fb11, "[ ]", "[x]", 1))+"### フェーズfb2: 動作確認のフィードバック対応 (第2版)\n\n- [ ] **fb2-1**: [Go] 再度直す\n\n"),
			wantAdded: []string{"fb2-1"},
		},
		{
			name:     "タスクの追加が無い",
			snapshot: snapshotPlan,
			current:  snapshotPlan,
			want:     []string{"フェーズfb1のタスクが追加されていません"},
		},
		{
			name:     "フェーズfbNでないタスクの追加",
			snapshot: snapshotPlan,
			current:  appendTasks(snapshotPlan, fbPhase("- [ ] **1-4**: [Go] 追加")),
			want:     []string{"タスク 1-4 はフェーズfb1のタスクではありません"},
		},
		{
			name:     "回を飛ばしたフェーズの追加",
			snapshot: snapshotPlan,
			current:  appendTasks(snapshotPlan, fbPhase("- [ ] **fb2-1**: [Go] 追加")),
			want:     []string{"タスク fb2-1 はフェーズfb1のタスクではありません"},
		},
		{
			name:     "チェック済みのタスクの追加",
			snapshot: snapshotPlan,
			current:  appendTasks(snapshotPlan, fbPhase(strings.Replace(fb11, "[ ]", "[x]", 1))),
			want:     []string{"追加されたタスク fb1-1 にチェックが入っています"},
		},
		{
			name:     "既存のタスクの変更",
			snapshot: snapshotPlan,
			current:  appendTasks(strings.Replace(snapshotPlan, "- [ ] **1-3**", "- [x] **1-3**", 1), fbPhase(fb11)),
			want:     []string{"「タスクリスト」節の既存の行が変更・削除されています"},
		},
		{
			name:     "タスクリストの途中への追加",
			snapshot: snapshotPlan,
			current:  strings.Replace(snapshotPlan, "- [ ] **1-3**", fbPhase(fb11)+"- [ ] **1-3**", 1),
			want:     []string{"「タスクリスト」節の既存の行が変更・削除されています"},
		},
		{
			name:     "設計節の変更",
			snapshot: snapshotPlan,
			current:  appendTasks(strings.Replace(snapshotPlan, "## 設計\n\n", "## 設計\n\n新しい方針\n", 1), fbPhase(fb11)),
			want:     []string{"「設計」節が変更されています"},
		},
		{
			name:     "実装判断ログへの追記",
			snapshot: snapshotPlan,
			current:  appendTasks(strings.Replace(snapshotPlan, "#### 列の型をどうするか?\n", "#### 列の型をどうするか?\n\n- [x] 文字列\n", 1), fbPhase(fb11)),
			want:     []string{"「実装判断ログ」節が変更されています"},
		},
		{
			name:     "節の追加",
			snapshot: snapshotPlan,
			current:  appendTasks(snapshotPlan, fbPhase(fb11)) + "\n## 追加の節\n",
			want:     []string{`節 "## 追加の節" が追加されています`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			added, vs := CheckFeedbackPhase(tt.snapshot, tt.current)
			if len(tt.want) == 0 {
				if len(vs) > 0 {
					t.Fatalf("違反 = %+v、違反なしを期待", vs)
				}
				var ids []string
				for _, a := range added {
					ids = append(ids, a.ID)
				}
				if strings.Join(ids, ",") != strings.Join(tt.wantAdded, ",") {
					t.Errorf("追加されたタスク = %v、期待値 = %v", ids, tt.wantAdded)
				}
				return
			}
			if added != nil {
				t.Errorf("違反があるのに追加されたタスクを返した: %+v", added)
			}
			var msgs []string
			for _, v := range vs {
				msgs = append(msgs, v.Message)
			}
			got := strings.Join(msgs, "\n")
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("違反 = %q、含まれることを期待した文字列 = %q", got, w)
				}
			}
		})
	}
}

func TestCheckFeedbackPhase_ExtendsExistingTask(t *testing.T) {
	t.Parallel()

	old := "## タスクリスト\n\n- [x] **1-1**: [Go] 元タスク\n"
	for _, extra := range []string{
		"  - **保護領域**: schema (列を追加)",
		"  - **想定行数**: 約9999行 (実装9999行)",
		"  - **対応する確認項目**: 第1版 #1",
		"  - 元タスクの説明を追加する",
	} {
		t.Run(extra, func(t *testing.T) {
			t.Parallel()
			added, vs := CheckFeedbackPhase(old, old+extra+"\n\n"+fbPhase("- [ ] **fb1-1**: 修正"))
			if len(vs) == 0 || added != nil {
				t.Fatalf("既存タスクへの追記を受け入れた: %+v (%+v)", added, vs)
			}
		})
	}
}
