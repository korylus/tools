package md

import (
	"slices"
	"strings"
	"testing"
)

func TestCodeBlocksExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
	}{
		{"引用内", "> ```text\n> 全角（括弧）。次の文。\n> ```\n"},
		{"引用の入れ子", "> > ~~~text\n> > 全角（括弧）。次の文。\n> > ~~~\n"},
		{"リスト項目の先頭", "- ```text\n  全角（括弧）。次の文。\n  ```\n"},
		{"リスト項目の続き", "- 項目\n\n  ~~~text\n  全角（括弧）。次の文。\n  ~~~\n"},
		{"引用内のリスト", "> - ```text\n>   全角（括弧）。次の文。\n>   ```\n"},
		{"チルダ", "~~~text\n全角（括弧）。次の文。\n~~~\n"},
		{"長いフェンス内の短いフェンス", "````markdown\n```text\n全角（括弧）。次の文。\n```\n````\n"},
		{"異なる記号では閉じない", "~~~text\n```\n全角（括弧）。次の文。\n~~~\n"},
		{"閉じ記号に後続文字がある", "```text\n```text\n全角（括弧）。次の文。\n```\n"},
		{"引用が終われば未閉鎖でも終了", "> ```text\n> 全角（括弧）。次の文。\n"},
		{"リストが終われば未閉鎖でも終了", "- ```text\n  全角（括弧）。次の文。\n"},
		{"字下げによるコードブロック", "    全角（括弧）。次の文。\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc := tt.code + "\n本文（違反）。次の文。\n"
			lineNo := strings.Count(tt.code, "\n") + 2
			hits := violations(doc)
			if got := linesOf(hits); !slices.Equal(got, []int{lineNo, lineNo}) {
				t.Fatalf("違反の行番号 = %v、期待値 = [%d %d]", got, lineNo, lineNo)
			}
			want := tt.code + "\n本文（違反）。\n次の文。\n"
			if got := rewrite(doc); got != want {
				t.Errorf("書き換え結果 = %q、期待値 = %q", got, want)
			}
		})
	}
}

func TestHTMLCommentsRespectInlineCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		want []int
	}{
		{"開始記号がインラインコード内", "`<!--` を説明する。\n\n全角（括弧）。\n", []int{3}},
		{"同じ行の本文も検査する", "`<!--` と本文（違反）。\n", []int{1}},
		{"インラインコード内のコメントを消さない", "`<!--` 本文（違反） `-->`\n", []int{1}},
		{"複数バッククォートのコード", "`` `<!--` `` を説明する。\n全角（括弧）。\n", []int{2}},
		{"コードの後ろの本物の注記", "`<!--` <!-- 注記（対象外） --> 本文（違反）。\n", []int{1}},
		{"複数の注記", "本文 <!-- 注記（対象外） --> <!-- Go 版 -->\n", nil},
		{"コメントが行頭", "<!-- 注記（対象外） --> 本文（違反）。\n", []int{1}},
		{"本物の複数行コメント", "<!--\nコメント（対象外）\n-->\n本文（違反）。\n", []int{4}},
		{"コメント内のフェンス", "<!--\n```\nコメント（対象外）\n-->\n本文（違反）。\n", []int{5}},
		{"コメント内のバッククォート", "本文 <!-- `（対象外）\n-->\n本文（違反）。\n", []int{3}},
		{"コード内の閉じ記号", "`-->` と本文（違反）。\n", []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := linesOf(violations(tt.doc)); !slices.Equal(got, tt.want) {
				t.Errorf("違反の行番号 = %v、期待値 = %v", got, tt.want)
			}
		})
	}
}

func TestUnclosedCodeBlockExcluded(t *testing.T) {
	t.Parallel()

	doc := "~~~text\n全角（括弧）。次の文。\n"
	if got := violations(doc); len(got) != 0 {
		t.Errorf("違反 = %v、期待値 = なし", got)
	}
	if got := rewrite(doc); got != doc {
		t.Errorf("書き換え結果 = %q、期待値 = %q", got, doc)
	}
}

// TestCodeSpansExcludedは段落内で改行をまたぐインラインコードの中身が §3の検査
// 対象から外れ、コード片の外の本文は検査対象に残ることを確認する。
// internal/styleは行単位で判定するため、閉じ記号が次の行にあるコード片を自力で
// は除外できない。
func TestCodeSpansExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		want []int
	}{
		{"行をまたぐコード片の中", "`コード\n（全角）` の後。\n", nil},
		{"行をまたぐコード片の外", "`コード\n（全角）` の後（違反）。\n", []int{2}},
		{"1行で閉じるコード片の中", "`（全角）` の後。\n", nil},
		{"1行で閉じるコード片の外", "`（全角）` と本文（違反）。\n", []int{1}},
		{"3行にまたがるコード片", "`コード\n（全角）\nの続き` の後。\n", nil},
		{"引用の中のコード片", "> `コード\n> （全角）` の後。\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := linesOf(violations(tt.doc)); !slices.Equal(got, tt.want) {
				t.Errorf("違反の行番号 = %v、期待値 = %v", got, tt.want)
			}
			if got := rewrite(tt.doc); got != tt.doc {
				t.Errorf("書き換え結果 = %q、期待値 = 入力のまま", got)
			}
		})
	}
}

// TestCodeSpansMaskOnlyCodeは隣接する複数のコード片に違反の例を含めても、
// 検査対象の本文へ漏れ出さないことを確認する。
func TestCodeSpansMaskOnlyCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
	}{
		{"後ろに別のコード片がある", "`Go 版（例）\n続き` 本文 `Go 版（例）`\n"},
		{"複数バッククォートのコード片", "``Go 版（例） `\n続き`` 本文 ``Go 版（例）``\n"},
		{"前に別のコード片がある", "`Go 版（例）` 本文 `Go 版（例）\n続き`\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := violations(tt.doc); len(got) != 0 {
				t.Errorf("violations(%q) = %v、期待値 = なし", tt.doc, msgsOf(got))
			}
		})
	}
}

// TestCodeSpansKeepSentenceBreaksはコード片の中の「。」で --writeが行を
// 分割しないことを確認する。
// 分割するとコード片の中身に改行が入り、描画される文字列が変わってしまう。
func TestCodeSpansKeepSentenceBreaks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
	}{
		{"改行をまたぐコード片の中の「。」", "`コード\n続き。もう一文。` の後。\n"},
		{"行まるごとがコード片の中", "`コード\n真ん中。の行。\n終わり` の後。\n"},
		{"1行で閉じるコード片の中の「。」", "コード `a。b` の後。\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := violations(tt.doc); len(got) != 0 {
				t.Errorf("violations(%q) = %v、期待値 = なし", tt.doc, msgsOf(got))
			}
			if got := rewrite(tt.doc); got != tt.doc {
				t.Errorf("書き換え結果 = %q、期待値 = 入力のまま", got)
			}
		})
	}
}

// TestFrontMatterExcludedFromSentenceBreaksはYAMLフロントマターが句点改行の
// 対象から外れることを確認する。
// フロントマターの値を「。」で改行するとYAMLが壊れ、続きの行が新しいキーとして
// 解釈されてしまう。
func TestFrontMatterExcludedFromSentenceBreaks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
	}{
		{"複数の文を持つ値", "---\nname: sample\ndescription: サンプルです。2つ目の文です。\n---\n\n本文です。\n"},
		{"コメントで始まる", "---\n# コメント\ndescription: サンプルです。2つ目の文です。\n---\n\n本文です。\n"},
		{"CRLFで複数の文を持つ値", "---\r\nname: sample\r\ndescription: サンプルです。2つ目の文です。\r\n---\r\n\r\n本文です。\r\n"},
		{"CRLFで両方の区切りの行末に空白がある", "--- \t\r\ndescription: サンプルです。2つ目の文です。\r\n---\t \r\n\r\n本文です。\r\n"},
		{"値が1文でも触らない", "---\ndescription: サンプルです。\n---\n\n本文です。\n"},
		{"区切りの行末に空白がある", "---\ndescription: サンプルです。2つ目の文です。\n--- \n\n本文です。\n"},
		{"空行を挟む", "---\nname: sample\n\ndescription: サンプルです。2つ目の文です。\n---\n\n本文です。\n"},
		{"箇条書きの値がある", "---\nallowed-tools:\n  - Read\ndescription: サンプルです。2つ目の文です。\n---\n\n本文です。\n"},
		{"字下げした値がある", "---\ndescription: |\n    サンプルです。2つ目の文です。\n---\n\n本文です。\n"},
		{"フロントマターだけの文書", "---\ndescription: サンプルです。2つ目の文です。\n---\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := violations(tt.doc); len(got) != 0 {
				t.Errorf("violations(%q) = %v、期待値 = なし", tt.doc, msgsOf(got))
			}
			if got := rewrite(tt.doc); got != tt.doc {
				t.Errorf("書き換え結果 = %q、期待値 = 入力のまま", got)
			}
		})
	}
}

// TestFrontMatterKeepsStyleChecksはフロントマターの値にも §3のスタイルを
// 適用し続けることを確認する。
// §3は日本語テキスト全般のルールで、対象から外すのは句点改行だけである。
func TestFrontMatterKeepsStyleChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		want []int
	}{
		{"全角丸括弧", "---\ndescription: サンプル（例）です。\n---\n", []int{2}},
		{"英数字と日本語の間のスペース", "---\ndescription: Go のサンプルです。\n---\n", []int{2}},
		{"インラインコードの中は対象外", "---\ndescription: サンプル `Go の例` です。\n---\n", nil},
		{"本文の違反も続けて検出する", "---\ndescription: サンプル（例）です。\n---\n\n本文（違反）です。\n", []int{2, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := linesOf(violations(tt.doc)); !slices.Equal(got, tt.want) {
				t.Errorf("違反の行番号 = %v、期待値 = %v", got, tt.want)
			}
		})
	}
}

// TestFrontMatterRequiresClosingDelimiterは閉じの区切りが無い `---` を
// フロントマターとみなさないことを確認する。
// 文書の先頭の区切り線が、以降のすべての行を検査から外す事故を防ぐ。
func TestFrontMatterRequiresClosingDelimiter(t *testing.T) {
	t.Parallel()

	doc := "---\n\n本文です。2つ目の文です。\n"
	if got := linesOf(violations(doc)); !slices.Equal(got, []int{3}) {
		t.Errorf("違反の行番号 = %v、期待値 = [3]", got)
	}
	want := "---\n\n本文です。\n2つ目の文です。\n"
	if got := rewrite(doc); got != want {
		t.Errorf("書き換え結果 = %q、期待値 = %q", got, want)
	}
}

// TestFrontMatterRequiresYAMLMappingは区切りに挟まれた中身がYAMLのマッピングで
// ないものをフロントマターとみなさないことを確認する。
// 区切り線で始まる文書に区切り線がもう1本あるだけで、その間の地の文が句点改行の
// 検査から静かに外れるのを防ぐ。
func TestFrontMatterRequiresYAMLMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		want []int
	}{
		{"区切り線に挟まれた地の文", "---\n\n本文です。2つ目の文です。\n\n---\n\n続きです。3つ目の文です。\n", []int{3, 7}},
		{"キーを持たない中身", "---\n本文です。2つ目の文です。\n---\n", []int{2}},
		{"シーケンスで始まる中身", "---\n- リスト\n\n本文です。2つ目の文です。\n---\n", []int{4}},
		{"コメントだけの中身", "---\n# コメント\n\n本文です。2つ目の文です。\n---\n", []int{4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := linesOf(violations(tt.doc)); !slices.Equal(got, tt.want) {
				t.Errorf("違反の行番号 = %v、期待値 = %v", got, tt.want)
			}
		})
	}
}
