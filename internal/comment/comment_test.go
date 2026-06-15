package comment

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reMarker matches the Japanese translation marker. It is only needed by tests,
// so it lives here rather than in the production code.
//
// [Ja] reMarker は日本語訳マーカーにマッチする。テストでのみ必要なため、本番コードでは
// なくこちらに置く。
var reMarker = regexp.MustCompile(`\[Ja\]`)

// group builds a comment group from raw "//" lines, numbering them from 1.
//
// [Ja] group は "//" 行から 1 始まりで番号付けしたコメント群を作る。
func group(texts ...string) []commentLine {
	lines := make([]commentLine, len(texts))
	for i, t := range texts {
		lines[i] = commentLine{line: i + 1, text: t}
	}
	return lines
}

// condsOf returns the list of condition numbers in findings.
//
// [Ja] condsOf は findings に含まれる条件番号の一覧を返す。
func condsOf(fs []finding) []int {
	got := make([]int, len(fs))
	for i, f := range fs {
		got[i] = f.cond
	}
	return got
}

func TestCheckGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lines     []commentLine
		wantConds []int
	}{
		{
			name: "correct pair with a blank line between the blocks",
			// One blank comment line separates the English and Japanese blocks.
			//
			// [Ja] 英語ブロックと日本語ブロックを空行 1 行で区切る。
			lines:     group("// Hash the password.", "//", "// [Ja] パスワードをハッシュ化する。"),
			wantConds: nil,
		},
		{
			name: "missing blank line between the blocks (8)",
			// The [Ja] block directly follows the English line, with no blank.
			//
			// [Ja] [Ja] ブロックが英語行の直後に続き、空行が無い。
			lines:     group("// Hash the password.", "// [Ja] パスワードをハッシュ化する。"),
			wantConds: []int{8},
		},
		{
			name: "correct multi-line block with a blank line between the blocks",
			// A blank line separates the multi-line English and Japanese blocks.
			//
			// [Ja] 複数行でも英日のブロックを空行で区切る。
			lines:     group("// First line.", "// Second line.", "//", "// [Ja] 最初の行。", "// 2 行目。"),
			wantConds: nil,
		},
		{
			name: "missing blank line in a multi-line block (8)",
			// The [Ja] block directly follows the last English line, with no blank.
			//
			// [Ja] [Ja] ブロックが英語最終行の直後に続き、空行が無い。
			lines:     group("// First line.", "// Second line.", "// [Ja] 最初の行。", "// 2 行目。"),
			wantConds: []int{8},
		},
		{
			name: "Japanese block is not Japanese (1)",
			// The lines under the Japanese marker carry English text.
			//
			// [Ja] 日本語マーカーの下の行が英文になっている。
			lines:     group("// Hash the password.", "//", "// [Ja] hash the password"),
			wantConds: []int{1},
		},
		{
			name: "English block contains Japanese, a duplicated Japanese block (2)",
			// The unmarked English block is written in Japanese (the duplication misuse).
			//
			// [Ja] 無マーカーの英語ブロックが日本語で書かれている (重複の誤用)。
			lines:     group("// 平文パスワードをハッシュ化する。", "//", "// [Ja] 平文パスワードをハッシュ化する。"),
			wantConds: []int{2},
		},
		{
			name: "Japanese on the line nearest the marker (2)",
			// The line just above the marker is Japanese (the canonical misuse).
			//
			// [Ja] マーカーの直上の行が日本語になっている (典型的な誤用)。
			lines:     group("// First line.", "// 二行目に日本語。", "//", "// [Ja] 最初の行。", "// 2 行目。"),
			wantConds: []int{2},
		},
		{
			name: "Japanese label line merged above an English block is not flagged",
			// A missing blank line merges a Japanese label into the group, but only
			// the line nearest the marker is the English side, so it is not flagged.
			//
			// [Ja] 空行漏れで日本語ラベルが群に取り込まれても、マーカーに最も近い行だけが
			// 英語側なので誤検出しない。
			lines:     group("// 設定する", "// Configure the client.", "//", "// [Ja] クライアントを設定する。"),
			wantConds: nil,
		},
		{
			name: "obsolete English marker (7)",
			// A leading [En] marker is obsolete; the English block is unmarked now.
			//
			// [Ja] 行頭の [En] マーカーは廃止。英語ブロックは無マーカーにする。
			lines:     group("// [En] Hash the password.", "//", "// [Ja] パスワードをハッシュ化する。"),
			wantConds: []int{7},
		},
		{
			name: "obsolete English marker with Japanese in the English block (2 then 7)",
			// Both the obsolete marker and the Japanese-in-English misuse are reported.
			//
			// [Ja] 廃止マーカーと、英語ブロックの日本語混入の両方を報告する。
			lines:     group("// [En] 平文パスワードをハッシュ化する。", "//", "// [Ja] 平文パスワードをハッシュ化する。"),
			wantConds: []int{2, 7},
		},
		{
			name: "two obsolete English markers (7 then 7)",
			// Each [En] marker line is reported.
			//
			// [Ja] [En] マーカー行はそれぞれ報告される。
			lines:     group("// [En] English one.", "// [En] English two.", "//", "// [Ja] 日本語。"),
			wantConds: []int{7, 7},
		},
		{
			name:      "Japanese-only comment with no English block (4)",
			lines:     group("// [Ja] 日本語のみのコメント。"),
			wantConds: []int{4},
		},
		{
			name: "Japanese marker first, English text after it (4)",
			// English after the Japanese marker does not count as an English block above it.
			//
			// [Ja] 日本語マーカーの後ろの英語は、上の英語ブロックとはみなさない。
			lines:     group("// [Ja] 日本語。", "// English."),
			wantConds: []int{4},
		},
		{
			name: "inline marker with a duplicated Japanese block (3)",
			// A duplicated Japanese block on one line, with the marker at end of line.
			//
			// [Ja] 1 行に日本語ブロックが重複し、マーカーが行末にある。
			lines:     group("// 値はゼロのまま。[Ja] 値はゼロのまま。"),
			wantConds: []int{3},
		},
		{
			name: "inline marker with a reversed Japanese-then-English pair (3)",
			// Japanese leads and English follows the end-of-line marker.
			//
			// [Ja] 日本語が先で、行末マーカーの後ろに英語が続く。
			lines:     group("// ドキュメント宣言。[Ja] document declaration"),
			wantConds: []int{3},
		},
		{
			name: "inline marker with English before it is still banned (5)",
			// A valid-looking old inline pair is banned under the current format.
			//
			// [Ja] 一見正しい旧インラインペアも現行フォーマットでは禁止。
			lines:     group("// Hash the password. [Ja] パスワードをハッシュ化する。"),
			wantConds: []int{5},
		},
		{
			name: "duplicate Japanese marker (6)",
			// Two Japanese markers in one comment.
			//
			// [Ja] 1 コメントに日本語マーカーが 2 つ。
			lines:     group("// English.", "//", "// [Ja] 日本語。", "// [Ja] 二つ目の日本語。"),
			wantConds: []int{6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := condsOf(checkGroup(tt.lines))
			if !equalInts(got, tt.wantConds) {
				t.Errorf("checkGroup conds = %v, want %v", got, tt.wantConds)
			}
		})
	}
}

func TestCheckGroupReportsLineNumber(t *testing.T) {
	t.Parallel()

	lines := []commentLine{
		{line: 766, text: "// 値はゼロのまま。[Ja] 値はゼロのまま。"},
	}
	fs := checkGroup(lines)
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs))
	}
	if fs[0].line != 766 {
		t.Errorf("finding line = %d, want 766", fs[0].line)
	}
}

func TestMarkerKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want string
	}{
		{"// [En] foo", "en"},
		{"// [Ja] バー", "ja"},
		{"//   [En] indented leader is trimmed", "en"},
		{"\t* [En] block-comment continuation", "en"},
		{"// foo", ""},
		{"// The [En] mention is not at the start", ""},
		{"// 値はゼロのまま。[Ja] 値はゼロのまま。", ""},
	}
	for _, tt := range tests {
		if got := markerKind(tt.text); got != tt.want {
			t.Errorf("markerKind(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestInlineMarkerMisuse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want bool
	}{
		// Misuse: a duplicated Japanese block on one line.
		//
		// [Ja] 誤用: 1 行に日本語ブロックが重複。
		{"// 値はゼロのまま。[Ja] 値はゼロのまま。", true},
		{"// CancelAt はゼロ値 (0) のまま. [Ja] CancelAt はゼロ値 (0) のまま", true},
		// Misuse: reversed pair, Japanese leads and English follows the marker.
		//
		// [Ja] 誤用: 逆順ペア。日本語が先でマーカーの後ろに英語。
		{"// ドキュメント宣言。[Ja] document declaration", true},
		// English before the marker: no Japanese in the block before it.
		//
		// [Ja] マーカーの前が英語: 前のブロックに日本語が無い。
		{"// Hash the password. [Ja] パスワードをハッシュ化する。", false},
		{`// "ja" or "en". [Ja] "ja" または "en"`, false},
		// Prose mention: the marker follows a particle, not a sentence end.
		//
		// [Ja] 地の文の言及: マーカーが助詞の後で文末ではない。
		{"// 本文が [Ja] マーカーで始まる行だけを対象にする。", false},
		// A line-leading marker is handled elsewhere, not here.
		//
		// [Ja] 行頭マーカーは別で扱うためここでは対象外。
		{"// [Ja] 日本語のみ。", false},
		{"// [En] English only.", false},
		// No marker at all.
		//
		// [Ja] マーカーが無い。
		{"// 値はゼロのまま。", false},
	}
	for _, tt := range tests {
		if got := inlineMarkerMisuse(tt.text); got != tt.want {
			t.Errorf("inlineMarkerMisuse(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestInlineMarkerPresent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want bool
	}{
		// An inline Japanese marker after a sentence end.
		//
		// [Ja] 文末の後にインライン日本語マーカー。
		{"// Hash the password. [Ja] パスワードをハッシュ化する。", true},
		{"// 値はゼロのまま。[Ja] 値はゼロのまま。", true},
		// An inline English marker after a sentence end.
		//
		// [Ja] 文末の後にインライン英語マーカー。
		{"// cache it. [En] cache the result", true},
		// Prose mention: no sentence end before the marker.
		//
		// [Ja] 地の文の言及: マーカーの前に文末が無い。
		{"// The [Ja] marker leads the Japanese block.", false},
		{"// 本文が [Ja] マーカーで始まる。", false},
		// Line-leading markers and lines without any marker.
		//
		// [Ja] 行頭マーカー、およびマーカーの無い行。
		{"// proper English block line", false},
		{"// [Ja] 行頭の日本語マーカー", false},
		{"// no markers here at all", false},
	}
	for _, tt := range tests {
		if got := inlineMarkerPresent(tt.text); got != tt.want {
			t.Errorf("inlineMarkerPresent(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestGoCommentGroupsIgnoresStringLiterals(t *testing.T) {
	t.Parallel()

	// The "[Ja]" below lives inside a string literal, so the parser must not
	// surface it as a comment (otherwise the tool would flag its own fixtures).
	//
	// [Ja] 下の "[Ja]" は文字列リテラル内にあるため、コメントとして抽出されては
	// ならない (さもないとツール自身のフィクスチャを誤検出する)。
	src := []byte(strings.Join([]string{
		"package sample",
		"",
		"// Greet returns a greeting.",
		"//",
		"// [Ja] Greet は挨拶を返す。",
		"func Greet() string {",
		"\treturn \"// [Ja] this is not a comment\"",
		"}",
	}, "\n"))

	groups, err := goCommentGroups("sample.go", src)
	if err != nil {
		t.Fatalf("goCommentGroups: %v", err)
	}

	var markerLines int
	for _, g := range groups {
		for _, cl := range g {
			if reMarker.MatchString(cl.text) {
				markerLines++
			}
		}
	}
	if markerLines != 1 {
		t.Errorf("found %d comment lines with the marker, want 1 (string literal must be ignored)", markerLines)
	}
}

func TestTemplCommentGroups(t *testing.T) {
	t.Parallel()

	// Two runs of full-line "//" comments separated by a non-comment line must
	// become two groups; trailing markup is not part of any group.
	//
	// [Ja] 行頭 "//" コメントの連続が非コメント行で区切られたら 2 群になり、
	// 末尾のマークアップはどの群にも含まれない。
	src := []byte(strings.Join([]string{
		"// First group.",
		"//",
		"// [Ja] 最初の群。",
		"templ Page() {",
		"\t<div>hello</div>",
		"// Second group.",
		"//",
		"// [Ja] 2 つ目の群。",
		"}",
	}, "\n"))

	groups := templCommentGroups(src)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0][0].line != 1 {
		t.Errorf("first group starts at line %d, want 1", groups[0][0].line)
	}
	if groups[1][0].line != 6 {
		t.Errorf("second group starts at line %d, want 6", groups[1][0].line)
	}
}

func TestIsGenerated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "standard generated header",
			src:  "// Code generated by protoc-gen-go. DO NOT EDIT.\npackage sample\n",
			want: true,
		},
		{
			name: "hand-written file",
			src:  "// Greet returns a greeting.\npackage sample\n",
			want: false,
		},
	}
	for _, tt := range tests {
		if got := isGenerated([]byte(tt.src)); got != tt.want {
			t.Errorf("%s: isGenerated = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestRunFullMode exercises the subcommand end to end over a temp tree: full mode
// reports a near-zero-false-positive condition (here a Japanese block that is not
// Japanese), writes findings to stdout and a summary to stderr, and exits 1.
//
// [Ja] TestRunFullMode は一時ツリー上でサブコマンドを通しで動かす。全体モードは誤検出が
// ほぼ無い条件 (ここでは日本語でない日本語ブロック) を報告し、検出を stdout・要約を
// stderr に書き、終了コード 1 を返す。
func TestRunFullMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package sample",
		"",
		"// valid english block.",
		"//",
		"// [Ja] this block is not japanese.",
		"func Bad() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.go:5:") {
		t.Errorf("stdout = %q, want a finding at bad.go:5", stdout.String())
	}
	if !strings.Contains(stderr.String(), "1 bilingual marker violation") {
		t.Errorf("stderr = %q, want a violation summary", stderr.String())
	}
}

// TestRunFullModeEnglishBlockJapanese confirms full mode reports an English block
// that contains Japanese (the duplication misuse), tree-wide.
//
// [Ja] TestRunFullModeEnglishBlockJapanese は、英語ブロックに日本語が入っている誤用
// (重複) を全体モードがツリー全体で報告することを確認する。
func TestRunFullModeEnglishBlockJapanese(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package sample",
		"",
		"// 平文パスワードをハッシュ化する。",
		"//",
		"// [Ja] 平文パスワードをハッシュ化する。",
		"func Bad() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.go:3:") {
		t.Errorf("stdout = %q, want a finding at bad.go:3", stdout.String())
	}
}

// TestRunFullModeObsoleteEnglishMarker confirms full mode reports an obsolete
// English marker, tree-wide.
//
// [Ja] TestRunFullModeObsoleteEnglishMarker は、廃止された英語マーカーを全体モードが
// ツリー全体で報告することを確認する。
func TestRunFullModeObsoleteEnglishMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package sample",
		"",
		"// [En] Greet returns a greeting.",
		"//",
		"// [Ja] Greet は挨拶を返す。",
		"func Greet() string { return \"hi\" }",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.go:3:") {
		t.Errorf("stdout = %q, want a finding at bad.go:3", stdout.String())
	}
}

// TestRunFullModeInlineMarker confirms full mode reports an inline Japanese
// marker with Japanese before it, tree-wide.
//
// [Ja] TestRunFullModeInlineMarker は、前に日本語があるインライン日本語マーカーを全体
// モードがツリー全体で報告することを確認する。
func TestRunFullModeInlineMarker(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package sample",
		"",
		"// 値はゼロのまま。[Ja] 値はゼロのまま。",
		"func Bad() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.go:3:") {
		t.Errorf("stdout = %q, want a finding at bad.go:3", stdout.String())
	}
}

// TestRunNoViolations confirms a clean tree exits 0 with no output. The
// English-required, inline-ban, duplicate, and blank-separator rules are diff-mode
// only, so none of them must trip in full mode.
//
// [Ja] TestRunNoViolations は問題のないツリーが無出力・終了コード 0 になることを確認する。
// 英語必須・インライン禁止・重複・ブロック間空行の規則は差分モード限定のため、全体モードでは
// 発火してはならない。
func TestRunNoViolations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "good.go", strings.Join([]string{
		"package sample",
		"",
		"// Greet returns a greeting.",
		"//",
		"// [Ja] Greet は挨拶を返す。",
		"func Greet() string { return \"hi\" }",
		"",
		"// Wave waves at the user.",
		"// It never returns an error.",
		"//",
		"// [Ja] Wave はユーザーに手を振る。",
		"// エラーは返さない。",
		"func Wave() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run code = %d, want 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

// TestRunSkipsGeneratedFiles confirms generated files are not checked even when
// they contain a marker misuse.
//
// [Ja] TestRunSkipsGeneratedFiles は、生成物がマーカー誤用を含んでいても検査対象外に
// なることを確認する。
func TestRunSkipsGeneratedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "gen.go", strings.Join([]string{
		"// Code generated by stringer. DO NOT EDIT.",
		"package sample",
		"",
		"// 日本語が英語ブロックに入っている。",
		"//",
		"// [Ja] 日本語。",
		"func Gen() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run code = %d, want 0 (generated files are skipped); stdout: %s", code, stdout.String())
	}
}

// TestRunHelpExitsZero confirms a -h request is treated as success.
//
// [Ja] TestRunHelpExitsZero は -h 要求が成功扱いになることを確認する。
func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("Run(-h) code = %d, want 0", code)
	}
}

// writeFile writes content to name under dir, failing the test on error.
//
// [Ja] writeFile は dir 配下の name に content を書き出し、失敗時にテストを止める。
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
