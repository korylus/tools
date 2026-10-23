package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testDocは無人実行モードのレビュードキュメントの例。テンプレートのHTMLコメントの中の
// 書式の例は読み取らない。
const testDoc = `# コードレビュー: p-1-1

<!--
**問題点のフォーマット**:

**区分**: 要人間判断

**対応方針**:

- [ ] コメントの中の選択肢 (推奨)
-->

## 過去レビューの修正検証

- [x] [p-1-1-001.md](./p-1-1-001.md) 「ログの出力」: 対応済み
- [ ] [p-1-1-001.md](./p-1-1-001.md) 「エラーの握りつぶし」: 再検討提案

  **根拠**: 呼び出し元で処理されている

  **区分**: 要人間判断

  **対応方針**:
  - [ ] 指摘を取り下げる
  - [ ] 決定済みの方針どおり修正する (推奨)

## ファイルごとのレビュー結果

### 1. ゲートの失敗を差し戻さない

**ステータス**: 要修正

**区分**: 自動対応可

**対応方針**:

<!-- 開発者が回答を記入してください -->
- [ ] 修正案の通り差し戻す (推奨)
- [ ] その他 (下の回答欄に記入)

**回答**:

` + "```" + `
**対応方針**:
- [ ] コードブロックの中の選択肢 (推奨)
` + "```" + `

### ` + "`internal/a.go`" + `

**区分**: 要人間判断

**問題点・改善提案**:

- **[go-coding.md#ログ]**: log.Printfを使っている

  **対応方針**:
  - [ ] slogに変える (推奨)
  - [ ] その他 (下の回答欄に記入)

## 設計改善の提案

### ` + "`internal/b.go`" + `: 関数を分ける

**区分**: 自動対応可
**対応方針**:

- [ ] 提案通り変更する
- [ ] 後続タスクに積む (推奨)

## 保護領域の照合

| 保護領域 | 観点 | 結果 | 根拠 |
| -------- | ---- | ---- | ---- |
| schema   | ロックの長さ | ok | 列の追加だけ |

## 総合評価

**評価**: Request Changes

**次のアクション**: 修正後に別モデルへ再レビューを依頼
`

func TestParse(t *testing.T) {
	t.Parallel()

	d, err := Parse([]byte(testDoc))
	if err != nil {
		t.Fatalf("Parseのエラー = %v", err)
	}
	if d.Verdict != RequestChanges {
		t.Errorf("Verdict = %q、期待値 = %q", d.Verdict, RequestChanges)
	}

	want := []struct {
		section  string
		title    string
		category Category
		proposal bool
		options  int
	}{
		{sectionVerification, "[ ] [p-1-1-001.md](./p-1-1-001.md) 「エラーの握りつぶし」: 再検討提案", NeedsHuman, true, 2},
		{sectionFindings, "1. ゲートの失敗を差し戻さない", AutoFix, false, 2},
		{sectionFindings, "`internal/a.go`", NeedsHuman, false, 2},
		{sectionDesign, "`internal/b.go`: 関数を分ける", AutoFix, false, 2},
	}
	if len(d.Findings) != len(want) {
		t.Fatalf("指摘 = %+v、期待値は%d件", d.Findings, len(want))
	}
	for i, w := range want {
		f := d.Findings[i]
		if f.Section != w.section || f.Title != w.title || f.Category != w.category || f.Proposal != w.proposal || len(f.Options) != w.options {
			t.Errorf("Findings[%d] = %+v、期待値 = %+v", i, f, w)
		}
	}
	if f := d.Findings[1]; !f.Options[0].Recommended || f.Options[1].Recommended || f.Options[0].Text != "修正案の通り差し戻す (推奨)" {
		t.Errorf("選択肢 = %+v", f.Options)
	}
	before, _, _ := strings.Cut(testDoc, "**区分**: 自動対応可\n**対応方針**:")
	if got, want := d.Findings[3].LineNo, strings.Count(before, "\n")+2; got != want {
		t.Errorf("段落の2行目の対応方針の行番号 = %d、期待値 = %d", got, want)
	}
}

func TestParse_Verdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary string
		want    Verdict
		wantErr string
	}{
		{name: "Approve", summary: "**評価**: Approve\n", want: Approve},
		{name: "Comment", summary: "**評価**: Comment\n\n**総評**: 良い\n", want: Comment},
		{name: "太字の評価", summary: "**評価**: **Request Changes**\n", want: RequestChanges},
		{name: "テンプレートのまま", summary: "**評価**: [Approve / Request Changes / Comment]\n", wantErr: "読み取れません"},
		{name: "評価が無い", summary: "**総評**: 良い\n", wantErr: "「**評価**:」の行がありません"},
		{name: "評価が2つある", summary: "**評価**: Approve\n\n**評価**: Comment\n", wantErr: "2個あります"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d, err := Parse([]byte("# レビュー\n\n## 総合評価\n\n" + tt.summary))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Parseのエラー = %v、期待値は %q を含むエラー", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parseのエラー = %v", err)
			}
			if d.Verdict != tt.want {
				t.Errorf("Verdict = %q、期待値 = %q", d.Verdict, tt.want)
			}
		})
	}
}

func TestDocument_Answer(t *testing.T) {
	t.Parallel()

	t.Run("自動対応可の推奨にチェックを入れ、要人間判断と再検討提案は待つ", func(t *testing.T) {
		t.Parallel()

		d, err := Parse([]byte(testDoc))
		if err != nil {
			t.Fatal(err)
		}
		a := d.Answer()
		if len(a.Invalid) != 0 {
			t.Errorf("Invalid = %v", a.Invalid)
		}
		if len(a.Pending) != 2 || !a.Pending[0].Proposal || a.Pending[1].Title != "`internal/a.go`" {
			t.Errorf("Pending = %+v", a.Pending)
		}
		if !a.Blocked() {
			t.Error("Blocked = false、期待値 = true")
		}
		if len(a.Decisions) != 2 || !a.Decisions[0].Auto || a.Decisions[0].Defers() || !a.Decisions[1].Defers() {
			t.Errorf("Decisions = %+v", a.Decisions)
		}
		out := string(a.Output)
		for _, want := range []string{"- [x] 修正案の通り差し戻す (推奨)", "- [x] 後続タスクに積む (推奨)", "- [ ] slogに変える (推奨)", "- [ ] 決定済みの方針どおり修正する (推奨)", "- [ ] コメントの中の選択肢 (推奨)", "- [ ] コードブロックの中の選択肢 (推奨)"} {
			if !strings.Contains(out, want) {
				t.Errorf("回答したドキュメントに %q がありません", want)
			}
		}
		if got, want := strings.Count(out, "[x]"), strings.Count(testDoc, "[x]")+2; got != want {
			t.Errorf("チェックの数 = %d、期待値 = %d", got, want)
		}
	})

	t.Run("人間が選んだ選択肢を回答として使う", func(t *testing.T) {
		t.Parallel()

		src := strings.Replace(testDoc, "- [ ] 指摘を取り下げる", "- [x] 指摘を取り下げる", 1)
		src = strings.Replace(src, "- [ ] その他 (下の回答欄に記入)\n\n## 設計", "- [X] その他 (下の回答欄に記入)\n\n## 設計", 1)
		d, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		a := d.Answer()
		if a.Blocked() {
			t.Fatalf("Pending = %+v、Invalid = %v", a.Pending, a.Invalid)
		}
		if len(a.Decisions) != 4 {
			t.Fatalf("Decisions = %+v", a.Decisions)
		}
		proposal, other := a.Decisions[0], a.Decisions[2]
		if proposal.Auto || !proposal.Defers() || proposal.Chosen[0].Text != "指摘を取り下げる" {
			t.Errorf("再検討提案への回答 = %+v", proposal)
		}
		if other.Auto || other.Defers() {
			t.Errorf("その他を選んだ回答 = %+v", other)
		}
		if !a.NeedsFix() {
			t.Error("NeedsFix = false、期待値 = true")
		}
	})

	t.Run("区分と推奨を読み取れない指摘", func(t *testing.T) {
		t.Parallel()

		src := "# レビュー\n\n## ファイルごとのレビュー結果\n\n" +
			"### 区分が無い\n\n**対応方針**:\n\n- [ ] 直す (推奨)\n\n" +
			"### 推奨が2つ\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 直す (推奨)\n- [ ] 直さない (推奨)\n\n" +
			"### 選択肢が無い\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n直す\n\n" +
			"## 総合評価\n\n**評価**: Comment\n"
		d, err := Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		a := d.Answer()
		if len(a.Invalid) != 3 {
			t.Fatalf("Invalid = %v", a.Invalid)
		}
		for i, want := range []string{"「区分が無い」: 区分", "「推奨が2つ」: 「(推奨)」の付いた選択肢が2個", "「選択肢が無い」: 対応方針にチェックボックスの選択肢がありません"} {
			if !strings.Contains(a.Invalid[i], want) {
				t.Errorf("Invalid[%d] = %q、期待値は %q を含む", i, a.Invalid[i], want)
			}
		}
		if string(a.Output) != src {
			t.Error("回答できない指摘があるのにドキュメントを書き換えた")
		}
	})

	t.Run("指摘が無ければ修正は要らない", func(t *testing.T) {
		t.Parallel()

		d, err := Parse([]byte("# レビュー\n\n## ファイルごとのレビュー結果\n\n指摘はありません。\n\n## 総合評価\n\n**評価**: Approve\n"))
		if err != nil {
			t.Fatal(err)
		}
		if a := d.Answer(); a.Blocked() || a.NeedsFix() || len(a.Decisions) != 0 {
			t.Errorf("Answers = %+v", a)
		}
	})
}

func TestOption_Defers(t *testing.T) {
	t.Parallel()

	for text, want := range map[string]bool{
		"修正案の通り修正する (推奨)":   false,
		"提案通り変更する":          false,
		"その他 (下の回答欄に記入)":    false,
		"後続タスクに積む (推奨)":     true,
		"現状のまま (理由を回答欄に記入)": true,
		"現状維持":                true,
		"指摘を取り下げる":            true,
		"今回は見送る":              true,
		"対応しない (理由を回答欄に記入)":   true,
		"決定済みの方針どおり修正する (推奨)": false,
	} {
		if got := (Option{Text: text}).Defers(); got != want {
			t.Errorf("%q のDefers = %v、期待値 = %v", text, got, want)
		}
	}
}

func TestLatest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if _, ok, err := Latest(root, "p-1-1"); err != nil || ok {
		t.Errorf("置き場所が無いときのLatest = %v, %v", ok, err)
	}
	dir := filepath.Join(root, Dir)
	for _, name := range []string{"p-1-1-001.md", "p-1-1-010.md", "p-1-1-002.md", "p-1-10-099.md", "p-1-1-003.txt", "p-1-1-abc.md"} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := Latest(root, "p-1-1")
	if err != nil || !ok || got != "docs/private/reviews/p-1-1-010.md" {
		t.Errorf("Latest = %q, %v, %v、期待値 = docs/private/reviews/p-1-1-010.md", got, ok, err)
	}
}

func TestSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		doc    string
		want   int
		wantOK bool
	}{
		{name: "置き場所からのパスの連番を読む", doc: "docs/private/reviews/p-1-1-002.md", want: 2, wantOK: true},
		{name: "4桁以上の連番を読む", doc: "p-1-1-1000.md", want: 1000, wantOK: true},
		{name: "接頭辞が違えば読まない", doc: "p-1-10-099.md"},
		{name: "3桁に満たない連番は読まない", doc: "p-1-1-02.md"},
		{name: "数字でない連番は読まない", doc: "p-1-1-abc.md"},
		{name: "Markdownでなければ読まない", doc: "p-1-1-003.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Sequence("p-1-1", tt.doc)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("Sequence = %d, %v、期待値 = %d, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParse_MissingPolicy(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name, section, body string
	}{
		{"要人間判断", sectionFindings, "### 人間の判断\n\n**区分**: 要人間判断\n\n方針を選ぶ必要がある。"},
		{"自動対応可", sectionFindings, "### エラー処理\n\n**区分**: 自動対応可\n\nエラーを返す必要がある。"},
		{"設計改善", sectionDesign, "### 構造の見直し\n\n**区分**: 要人間判断"},
		{"区分も欠落", sectionFindings, "### エラー処理\n\nエラーを返す必要がある。"},
		{"見出しが欠落", sectionFindings, "**区分**: 要人間判断\n\n方針を選ぶ必要がある。"},
		{"再検討提案", sectionVerification, "- [ ] 以前の指摘: 再検討提案\n\n  **区分**: 要人間判断\n\n  **根拠**: 前提が変わった。"},
		{"再検討提案の区分も欠落", sectionVerification, "- [ ] 以前の指摘: 再検討提案\n\n  **根拠**: 前提が変わった。"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := "## " + tt.section + "\n\n" + tt.body + "\n\n## 総合評価\n\n**評価**: Comment\n"
			d, err := Parse([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			a := d.Answer()
			if len(d.Findings) != 1 || !a.Blocked() || len(a.Invalid) != 1 {
				t.Fatalf("対応方針の欠落を検出できなかった: Findings = %+v、Answers = %+v", d.Findings, a)
			}
			if d.Findings[0].LineNo < 1 {
				t.Errorf("指摘の行番号 = %d", d.Findings[0].LineNo)
			}
		})
	}
}

func TestParse_MissingPolicyBeforeNextFinding(t *testing.T) {
	t.Parallel()

	src := "## ファイルごとのレビュー結果\n\n### 対応方針の欠落\n\n**区分**: 要人間判断\n\n" +
		"### 正常な指摘\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\n\n" +
		"## 総合評価\n\n**評価**: Comment\n"
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a := d.Answer(); !a.Blocked() || len(a.Invalid) != 1 || len(a.Decisions) != 1 {
		t.Fatalf("次の指摘で欠落が隠れた: %+v", a)
	}
}

func TestDocument_Answer_RepairOption(t *testing.T) {
	t.Parallel()

	for _, option := range []string{
		"現状の実装を修正する", "後続処理のエラーハンドリングを追加する",
		"現状のままにせず修正する", "今回は見送る処理を削除する",
		"対応しない分岐を修正する", "後続タスクに積む前にエラー処理を直す",
	} {
		t.Run(option, func(t *testing.T) {
			t.Parallel()
			src := "## ファイルごとのレビュー結果\n\n### エラー処理\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] " + option + " (推奨)\n\n## 総合評価\n\n**評価**: Comment\n"
			d, err := Parse([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if a := d.Answer(); a.Blocked() || !a.NeedsFix() {
				t.Fatalf("修正する選択肢を先送りにした: %+v", a)
			}
		})
	}
}

func TestParse_MissingPolicyInList(t *testing.T) {
	t.Parallel()

	item := func(label, category, policy string) string {
		s := "- **[" + label + "]**: 問題点\n\n"
		if category != "" {
			s += "  **区分**: " + category + "\n\n"
		}
		if policy != "" {
			s += "  **対応方針**:\n\n  <!-- 開発者が回答を記入してください -->\n  - [ ] " + policy + " (推奨)\n  - [ ] その他 (下の回答欄に記入)\n\n"
		}
		return s + "  **修正案**: 直す\n\n"
	}
	for _, tt := range []struct {
		name, body string
	}{
		// テンプレートどおり、見出しに区分を1つ書き、問題点を項目として並べる形。
		{"見出しに区分", "**区分**: 自動対応可\n\n**問題点・改善提案**:\n\n" + item("規約", "", "修正する") + item("規約", "", "")},
		// 問題点ごとに区分を書く形。対応方針の無い要人間判断を素通りさせない。
		{"項目ごとに区分", "**問題点・改善提案**:\n\n" + item("規約", "自動対応可", "修正する") + item("設計", "要人間判断", "")},
		{"欠落した項目が先", "**問題点・改善提案**:\n\n" + item("設計", "要人間判断", "") + item("規約", "自動対応可", "修正する")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := "## ファイルごとのレビュー結果\n\n### `a.go`: 問題\n\n**ステータス**: 要修正\n\n" + tt.body + "## 総合評価\n\n**評価**: Comment\n"
			d, err := Parse([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			a := d.Answer()
			if len(d.Findings) != 2 || !a.Blocked() || len(a.Invalid) != 1 || len(a.Decisions) != 1 {
				t.Fatalf("項目の対応方針の欠落を検出できなかった: Findings = %+v、Answers = %+v", d.Findings, a)
			}
		})
	}
}

func TestParse_PolicyInList(t *testing.T) {
	t.Parallel()

	// 対応方針を持たない項目だけのリスト (チェックしたガイドライン・修正案の箇条書き) は指摘にしない。
	src := "## ファイルごとのレビュー結果\n\n### `a.go`: 問題\n\n**区分**: 自動対応可\n\n**チェックしたガイドライン**:\n\n- 規約A\n- 規約B\n\n" +
		"**修正案**:\n\n- こう直す\n- テストを足す\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\n- [ ] その他 (下の回答欄に記入)\n\n" +
		"## 総合評価\n\n**評価**: Comment\n"
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a := d.Answer(); len(d.Findings) != 1 || a.Blocked() || !a.NeedsFix() {
		t.Fatalf("Findings = %+v、Answers = %+v", d.Findings, a)
	}
}

func TestDocument_Checked(t *testing.T) {
	t.Parallel()

	src := "## ファイルごとのレビュー結果\n\n### 未回答\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 修正する (推奨)\n\n" +
		"### 回答済み\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [x] 修正する (推奨)\n- [ ] 現状のまま\n\n" +
		"## 総合評価\n\n**評価**: Comment\n"
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Checked(); len(got) != 1 || got[0].Title != "回答済み" {
		t.Errorf("Checked = %+v、期待値 = 「回答済み」の1件", got)
	}
}
