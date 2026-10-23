package report

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestBuildKeepsMarkdownComments(t *testing.T) {
	t.Parallel()

	fence := strings.Repeat("`", 3)
	tests := []struct {
		name    string
		comment string
	}{
		{"空の見出し", "#\n\n## \n\n### ###\n\n説明"},
		{"小見出し", "再現手順\n\n### 詳細\n\n一覧を開くとボタンが消える"},
		{"項目に似た見出し", "### #10 詳細\n\n- [x] OK\n\n### 確認済みの項目\n\nまだ確認できていない"},
		{"全体コメントに似た見出し", "## 全体へのコメント\n\nボタンの名前を変えてほしい"},
		{"コードブロック", "次のMarkdownが表示される\n\n" + fence + "markdown\n# エラー\n## 全体へのコメント\n<!-- チェックリストに無い気づきを、この下に書く -->\n### #9 サンプル\n<!-- item: fake -->\n- [x] OK\n<!-- confirmed: fake -->\n" + fence + "\n\n直してほしい"},
		{"長いフェンス", "````markdown\n```\n### #9 サンプル\n<!-- item: fake -->\n```\n````\n\n再現する"},
		{"チルダのフェンス", "~~~markdown\n### #9 サンプル\n<!-- item: fake -->\n~~~"},
		{"インデントしたコード", "コード例:\n\n    ### #9 サンプル\n    <!-- item: fake -->\n\n説明"},
		{"先頭からインデントしたコード", "    ### #9 サンプル\n    <!-- item: fake -->"},
		{"改行の空白", "1行目  \n2行目  "},
		{"HTMLコメント", "<!-- 消さずに残すメモ -->\n\n詳しい説明"},
	}
	for level := 1; level <= 6; level++ {
		tests = append(tests, struct {
			name    string
			comment string
		}{fmt.Sprintf("第%dレベルの見出し", level), strings.Repeat("#", level) + " 詳細\n\n説明"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := newInput(t, testPlan)
			written := answer(t, string(Build(in)), "2", "OK", "")
			written = answer(t, written, "1", "NG", tt.comment)
			written += "\n" + tt.comment + "\n"

			assertResults := func(out string) {
				t.Helper()
				got := ParseResults(out)
				if item := got.Items["task/1-1/1"]; item.Status != StatusNG || item.Comment != tt.comment {
					t.Errorf("最初の項目 = %+v、期待するコメント = %q", item, tt.comment)
				}
				if item := got.Items["task/1-1/2"]; item.Status != StatusOK || item.Comment != "" {
					t.Errorf("次の項目 = %+v、コメント無しのOKを期待", item)
				}
				if len(got.Items) != 3 || len(got.Confirmed) != 0 {
					t.Errorf("本文中の例を項目と取り違えた: %+v", got)
				}
				if got.Comment != tt.comment {
					t.Errorf("全体コメント = %q、期待値 = %q", got.Comment, tt.comment)
				}
			}
			assertResults(written)

			in.Current = ParseResults(written)
			assertResults(string(Build(in)))

			in.Version, in.Previous, in.Current = 2, in.Current, nil
			next := string(Build(in))
			assertContains(t, next,
				"前の版のコメント:\n\n"+quote(tt.comment)+"\n",
				"前の版の全体へのコメント:\n\n"+quote(tt.comment)+"\n",
			)
			got := ParseResults(next)
			if len(got.Items) != 2 || got.Items["task/1-1/1"].Comment != "" || !got.Confirmed["task/1-1/2"] {
				t.Errorf("前版の引用と今回の回答を取り違えた: %+v", got)
			}
		})
	}
}

func TestParseResultsWithoutOverallGuide(t *testing.T) {
	t.Parallel()

	inputs := []struct {
		name string
		new  func(t *testing.T) Input
	}{
		{"第1版", func(t *testing.T) Input { return newInput(t, testPlan) }},
		{"確認済みの項目がある第2版", func(t *testing.T) Input {
			in := newInput(t, testPlan)
			prev := ParseResults(answer(t, string(Build(in)), "2", "OK", ""))
			in.Version, in.Previous = 2, prev
			return in
		}},
	}
	edits := []struct {
		name        string
		itemComment string
		edit        func(out string) string
	}{
		{"案内を消して書く", "直してほしい", func(out string) string {
			return strings.Replace(out, overallGuide+"\n", "", 1) + "\n全体のメモ\n"
		}},
		{"案内を書き換えて書く", "直してほしい", func(out string) string {
			return strings.Replace(out, overallGuide, "全体のメモ", 1)
		}},
		{"項目のコメントに同じ見出しを書き、案内を書き換えて書く", overallTitle + "\n\n項目のメモ", func(out string) string {
			return strings.Replace(out, overallGuide, "全体のメモ", 1)
		}},
	}
	for _, input := range inputs {
		for _, e := range edits {
			t.Run(input.name+"で"+e.name, func(t *testing.T) {
				t.Parallel()
				in := input.new(t)
				written := e.edit(answer(t, string(Build(in)), "1", "NG", e.itemComment))

				assertResults := func(out string) {
					t.Helper()
					got := ParseResults(out)
					if got.Comment != "全体のメモ" || got.Items["task/1-1/1"].Comment != e.itemComment {
						t.Errorf("全体へのコメント = %q、項目のコメント = %q", got.Comment, got.Items["task/1-1/1"].Comment)
					}
					if n, want := strings.Count(out, "\n"+overallTitle+"\n"), 1+strings.Count(e.itemComment, overallTitle); n != want {
						t.Errorf("「%s」の見出しの数 = %d、期待値 = %d:\n%s", overallTitle, n, want, out)
					}
				}
				assertResults(written)

				in.Current = ParseResults(written)
				if found := Absorbed(in); len(found) > 0 {
					t.Errorf("取り込まれていないのに、取り込みを検出した: %q", found)
				}
				assertResults(string(Build(in)))

				in.Version, in.Previous, in.Current = in.Version+1, in.Current, nil
				assertContains(t, string(Build(in)), "前の版の全体へのコメント:\n\n> 全体のメモ\n")
			})
		}
	}
}

func TestParseResultsInlineNotes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		comment string
		from    string
		to      string
		want    Result
	}{
		{"ラベルと同じ行のコメント", "", "コメント:\n", "コメント: ボタンが消える\n2行目\n", Result{Number: 1, Comment: "ボタンが消える\n2行目"}},
		{"チェックの後ろの文", "", "- [ ] NG", "- [x] NG: 一覧が崩れる", Result{Number: 1, Status: StatusNG, Comment: "NG: 一覧が崩れる"}},
		{"チェックしていない行の後ろの文", "", "- [ ] OK", "- [ ] OK まだ確かめていない", Result{Number: 1, Comment: "OK まだ確かめていない"}},
		{"チェックの後ろの文とコメント", "詳しい説明", "- [ ] NG", "- [x] NG一覧が崩れる", Result{Number: 1, Status: StatusNG, Comment: "NG一覧が崩れる\n\n詳しい説明"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := newInput(t, testPlan)
			out := string(Build(in))
			if tt.comment != "" {
				out = strings.Replace(out, "コメント:\n", "コメント:\n\n"+tt.comment+"\n", 1)
			}
			written := strings.Replace(out, tt.from, tt.to, 1)
			if got := ParseResults(written).Items["task/1-1/1"]; got != tt.want {
				t.Fatalf("書き込んだ項目 = %+v、期待値 = %+v", got, tt.want)
			}

			in.Current = ParseResults(written)
			rebuilt := string(Build(in))
			if got := ParseResults(rebuilt).Items["task/1-1/1"]; got != tt.want {
				t.Errorf("作り直した項目 = %+v、期待値 = %+v:\n%s", got, tt.want, rebuilt)
			}
			if strings.Contains(rebuilt, tt.to) {
				t.Errorf("作り直した作業報告書に、書き込んだ行 %q がそのまま残った", tt.to)
			}

			in.Version, in.Previous, in.Current = 2, in.Current, nil
			assertContains(t, string(Build(in)), "前の版のコメント:\n\n"+quote(tt.want.Comment)+"\n")
		})
	}
}

func TestAbsorbed(t *testing.T) {
	t.Parallel()

	fence := strings.Repeat("`", 3)
	overall := "「全体へのコメント」の節が、項目のコメントに取り込まれています"
	tests := []struct {
		name string
		edit func(t *testing.T, out string) string
		want []string
	}{
		{"書き込みが無い", func(_ *testing.T, out string) string { return out }, nil},
		{"コメントのコード例に区切りと同じ文がある", func(t *testing.T, out string) string {
			example := fence + "markdown\n### #2 画面を開ける\n\n<!-- item: task/1-1/2 -->\n\n" + overallTitle + "\n\n" + overallGuide + "\n" + fence
			return answer(t, out, "1", "NG", example)
		}, nil},
		{"項目のコメントのフェンスを閉じていない", func(t *testing.T, out string) string {
			return answer(t, out, "1", "NG", fence+"\nエラーの出力")
		}, []string{
			"項目 #2「画面を開ける」が、前の項目のコメントに取り込まれています",
			"項目 #3「保護領域「モジュール間の依存関係」の変更が合意した設計どおりになっている」が、前の項目のコメントに取り込まれています",
			overall,
		}},
		{"最後の項目のコメントのフェンスを閉じていない", func(t *testing.T, out string) string {
			return answer(t, out, "3", "NG", fence+"\nエラーの出力")
		}, []string{overall}},
		{"項目のIDを消した", func(_ *testing.T, out string) string {
			return strings.Replace(out, "<!-- item: task/1-1/2 -->", "", 1)
		}, []string{"項目 #2「画面を開ける」が、前の項目のコメントに取り込まれています"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := newInput(t, testPlan)
			in.Current = ParseResults(tt.edit(t, string(Build(in))))
			if got := Absorbed(in); !slices.Equal(got, tt.want) {
				t.Errorf("Absorbed() = %q、期待値 = %q", got, tt.want)
			}
		})
	}

	if got := Absorbed(newInput(t, testPlan)); got != nil {
		t.Errorf("作り直しでない入力のAbsorbed() = %q、nilを期待", got)
	}
}

func TestParseResultsWithoutBlankAfterLabel(t *testing.T) {
	t.Parallel()

	in := newInput(t, testPlan)
	written := answer(t, string(Build(in)), "1", "NG", "直してほしい\n2行目")
	written = strings.Replace(written, "コメント:\n\n", "コメント:\n", 1)
	got := ParseResults(written).Items["task/1-1/1"]
	if got.Status != StatusNG || got.Comment != "直してほしい\n2行目" {
		t.Errorf("空行無しで書いた項目 = %+v", got)
	}
}
