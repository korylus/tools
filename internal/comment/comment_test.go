package comment

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// reMarker matches the [Ja] translation marker. It is only needed by tests,
// so it lives here rather than in the production code.
//
// [Ja] reMarker は [Ja] 翻訳マーカーにマッチする。テストでのみ必要なため、
// 本番コードではなくこちらに置く。
var reMarker = regexp.MustCompile(`\[Ja\]`)

// group builds a comment group from raw "//" lines, numbering them from 1.
// [Ja] group は "//" 行から 1 始まりで番号付けしたコメント群を作る。
func group(texts ...string) []commentLine {
	lines := make([]commentLine, len(texts))
	for i, t := range texts {
		lines[i] = commentLine{line: i + 1, text: t}
	}
	return lines
}

// condsOf returns the list of condition numbers in findings.
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
			name:      "correct multi-line block",
			lines:     group("// New post form.", "//", "// [Ja] 新規投稿フォーム。"),
			wantConds: nil,
		},
		{
			name:      "correct one-line pair",
			lines:     group("// Hash the password.", "// [Ja] パスワードをハッシュ化する。"),
			wantConds: nil,
		},
		{
			name:      "correct inline pair on a single line",
			lines:     group("// Hash the password. [Ja] パスワードをハッシュ化する。"),
			wantConds: nil,
		},
		{
			name: "prose mention of [Ja] in English text is not a marker",
			// A [Ja] mention inside English prose must not be treated as a marker.
			// [Ja] 英語の地の文中の [Ja] 言及はマーカー扱いしない。
			lines:     group("// The [Ja] marker leads the Japanese translation block.", "// [Ja] 日本語訳ブロックを先導するマーカー。"),
			wantConds: nil,
		},
		{
			name: "marker on an English line (001/003 inversion)",
			// Japanese leads and the English line carries the marker (the 001/003 inversion).
			// [Ja] 英語行に [Ja] が付く誤り。日本語先・英語に [Ja]。
			lines:     group("// POST /posts は後続タスクで登録する。", "// [Ja] POST /posts is registered in a later task."),
			wantConds: []int{1},
		},
		{
			name: "Japanese-only with stray marker, no English above (002)",
			// No English block; [Ja] is misused as a separator (same shape as 002).
			// [Ja] 英語ブロックが無く [Ja] を区切りに誤用 (002 と同型)。
			lines:     group("// CSRF トークンを設定する。", "// [Ja] /new は RequireAuth 配下のため context 経由で渡る。"),
			wantConds: []int{2},
		},
		{
			name:      "marker-only Japanese comment without any English block",
			lines:     group("// [Ja] 日本語のみのコメント。"),
			wantConds: []int{2},
		},
		{
			name: "Japanese line with a Latin acronym is still Japanese (not an English block)",
			// A Japanese line stays Japanese even when it contains the Latin acronym CSRF.
			// [Ja] ラテン略語 CSRF を含んでも日本語行は英語ブロックにならない。
			lines:     group("// CSRF を検証する。", "// [Ja] これは説明である。"),
			wantConds: []int{2},
		},
		{
			name:      "more than one marker in a group",
			lines:     group("// English line.", "// [Ja] 日本語。", "// [Ja] 二つ目のマーカー。"),
			wantConds: []int{3},
		},
		{
			name: "marker on English line that is also a duplicate",
			// The second marker sits on an English line, so both condition 3 and condition 1 fire.
			// [Ja] 2 つ目のマーカーが英語行 → 条件 3 と条件 1 の両方。
			lines:     group("// English.", "// [Ja] 日本語。", "// [Ja] second english marker."),
			wantConds: []int{3, 1},
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
		{line: 766, text: "// POST /posts は後続タスクで登録する。"},
		{line: 767, text: "// [Ja] POST /posts is registered in a later task."},
	}
	fs := checkGroup(lines)
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs))
	}
	if fs[0].line != 767 {
		t.Errorf("finding line = %d, want 767", fs[0].line)
	}
}

func TestIsEnglishText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		text string
		want bool
	}{
		{"Set the CSRF token on the context.", true},
		{"CSRF トークンを設定する。", false}, // Latin acronym in Japanese is not English.
		{"// ", false}, // marker leader only (the "before" slice for "// [Ja] ...").
		{"日本語のみ。", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isEnglishText(tt.text); got != tt.want {
			t.Errorf("isEnglishText(%q) = %v, want %v", tt.text, got, tt.want)
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
		t.Errorf("found %d comment lines with [Ja], want 1 (string literal must be ignored)", markerLines)
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
		"// [Ja] 最初の群。",
		"templ Page() {",
		"\t<div>hello</div>",
		"// Second group.",
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
	if groups[1][0].line != 5 {
		t.Errorf("second group starts at line %d, want 5", groups[1][0].line)
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

// TestRunFullMode exercises the subcommand end to end over a temp tree: full
// mode reports only condition 1, writes findings to stdout and a summary to
// stderr, and exits 1.
//
// [Ja] TestRunFullMode は一時ツリー上でサブコマンドを通しで動かす。全体モードは
// 条件 1 のみを報告し、検出を stdout・要約を stderr に書き、終了コード 1 を返す。
func TestRunFullMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Condition 1: a Japanese line leads and the marker sits on an English line.
	// [Ja] 条件 1: 日本語行が先導し、マーカーが英語行に付いている。
	writeFile(t, dir, "bad.go", strings.Join([]string{
		"package sample",
		"",
		"// 日本語が先の行。",
		"// [Ja] English text on the marker line.",
		"func Bad() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.go:4:") {
		t.Errorf("stdout = %q, want a finding at bad.go:4", stdout.String())
	}
	if !strings.Contains(stderr.String(), "1 bilingual [Ja] marker violation") {
		t.Errorf("stderr = %q, want a violation summary", stderr.String())
	}
}

// TestRunNoViolations confirms a clean tree exits 0 with no output. Condition 2
// (no English block above) is not reported in full mode, so a Japanese-only
// comment must not trip the check.
//
// [Ja] TestRunNoViolations は問題のないツリーが無出力・終了コード 0 になることを
// 確認する。全体モードでは条件 2 (英語ブロック無し) を報告しないため、日本語のみの
// コメントで検査が落ちてはならない。
func TestRunNoViolations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "good.go", strings.Join([]string{
		"package sample",
		"",
		"// Greet returns a greeting.",
		"// [Ja] Greet は挨拶を返す。",
		"func Greet() string { return \"hi\" }",
		"",
		"// [Ja] 日本語のみのコメント。",
		"func JapaneseOnly() {}",
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
// [Ja] TestRunSkipsGeneratedFiles は、生成物がマーカー誤用を含んでいても検査
// 対象外になることを確認する。
func TestRunSkipsGeneratedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, dir, "gen.go", strings.Join([]string{
		"// Code generated by stringer. DO NOT EDIT.",
		"package sample",
		"",
		"// 日本語が先の行。",
		"// [Ja] English text on the marker line.",
		"func Gen() {}",
	}, "\n"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{dir}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run code = %d, want 0 (generated files are skipped); stdout: %s", code, stdout.String())
	}
}

// TestRunHelpExitsZero confirms a -h request is treated as success.
// [Ja] TestRunHelpExitsZero は -h 要求が成功扱いになることを確認する。
func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("Run(-h) code = %d, want 0", code)
	}
}

// writeFile writes content to name under dir, failing the test on error.
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
