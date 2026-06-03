package md

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBreakProse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single sentence is unchanged",
			in:   "一文だけ。",
			want: "一文だけ。",
		},
		{
			name: "two sentences break at the inner 。",
			in:   "これは一文目です。これは二文目です。",
			want: "これは一文目です。\nこれは二文目です。",
		},
		{
			name: "。 inside half-width parentheses is preserved",
			in:   "括弧 (中の。は壊さない) の外。次の文。",
			want: "括弧 (中の。は壊さない) の外。\n次の文。",
		},
		{
			name: "。 inside full-width parentheses is preserved",
			in:   "全角 （中の。は壊さない） の外。次。",
			want: "全角 （中の。は壊さない） の外。\n次。",
		},
		{
			name: "。 inside square and kagi brackets is preserved",
			in:   "角 [a。b] と鉤 「c。d」 の外。次。",
			want: "角 [a。b] と鉤 「c。d」 の外。\n次。",
		},
		{
			name: "。 inside inline code is preserved",
			in:   "コード `a。b` の後。終わり。",
			want: "コード `a。b` の後。\n終わり。",
		},
		{
			name: "。 immediately followed by a closing quote does not break",
			in:   "「文。」と続く。",
			want: "「文。」と続く。",
		},
		{
			name: "trailing 。 produces no empty segment",
			in:   "末尾の文。",
			want: "末尾の文。",
		},
		{
			name: "English prose with no 。 is unchanged",
			in:   "This line has no Japanese period.",
			want: "This line has no Japanese period.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := breakProse(tt.in); got != tt.want {
				t.Errorf("breakProse(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEachProseLine confirms that only rendered prose lines are surfaced:
// headings, lists, tables, blockquotes, code fences, HTML comments and blank
// lines are all skipped.
//
// [Ja] TestEachProseLine は地の文の行だけが拾われ、見出し・リスト・表・引用・
// コードフェンス・HTML コメント・空行がすべてスキップされることを確認する。
func TestEachProseLine(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"# Heading", // 1: heading
		"",          // 2: blank
		"これは地の文。これは二文目。", // 3: prose
		"",                    // 4: blank
		"- リスト項目。これは無視。",      // 5: list
		"",                    // 6: blank
		"| 表 | の。行 |",         // 7: table
		"",                    // 8: blank
		"> 引用。無視。",            // 9: blockquote
		"",                    // 10: blank
		"```",                 // 11: fence open
		"fence内。無視。",          // 12: inside fence
		"```",                 // 13: fence close
		"",                    // 14: blank
		"<!-- comment。無視 -->", // 15: single-line comment
		"",                    // 16: blank
		"普通の段落。続き。",           // 17: prose
	}, "\n")

	var got []int
	eachProseLine(doc, func(lineNo int, _ string) {
		got = append(got, lineNo)
	})

	want := []int{3, 17}
	if len(got) != len(want) {
		t.Fatalf("prose lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prose lines = %v, want %v", got, want)
		}
	}
}

// TestEachProseLineMultiLineComment confirms a multi-line HTML comment block is
// skipped entirely.
//
// [Ja] TestEachProseLineMultiLineComment は複数行の HTML コメントブロックが
// まるごとスキップされることを確認する。
func TestEachProseLineMultiLineComment(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"<!--",
		"コメント内の一文。二文目。",
		"-->",
		"本文の一文。二文目。",
	}, "\n")

	var got []int
	eachProseLine(doc, func(lineNo int, _ string) {
		got = append(got, lineNo)
	})

	if len(got) != 1 || got[0] != 4 {
		t.Errorf("prose lines = %v, want [4]", got)
	}
}

// TestRewrite confirms that only prose lines are rewritten; structural lines
// (headings, lists, code fences) pass through untouched.
//
// [Ja] TestRewrite は地の文の行だけが書き換えられ、構造行 (見出し・リスト・
// コードフェンス) はそのまま通ることを確認する。
func TestRewrite(t *testing.T) {
	t.Parallel()

	in := strings.Join([]string{
		"# Title",
		"",
		"一文目。二文目。",
		"",
		"- 項目。これはリストなので無視。",
	}, "\n")
	want := strings.Join([]string{
		"# Title",
		"",
		"一文目。",
		"二文目。",
		"",
		"- 項目。これはリストなので無視。",
	}, "\n")

	if got := rewrite(in); got != want {
		t.Errorf("rewrite() =\n%q\nwant\n%q", got, want)
	}
}

func TestViolations(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"一文目。二文目。", // line 1: violation
		"",         // line 2
		"単独の文。",    // line 3: clean
	}, "\n")

	hits := violations(doc)
	if len(hits) != 1 {
		t.Fatalf("got %d violations, want 1", len(hits))
	}
	if hits[0].lineNo != 1 {
		t.Errorf("violation line = %d, want 1", hits[0].lineNo)
	}
}

func TestListAllMarkdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "x")
	writeFile(t, filepath.Join(dir, "sub", "b.md"), "x")
	// Skipped directories must not contribute files.
	// [Ja] スキップ対象のディレクトリのファイルは含めない。
	writeFile(t, filepath.Join(dir, "node_modules", "c.md"), "x")
	writeFile(t, filepath.Join(dir, ".git", "d.md"), "x")
	writeFile(t, filepath.Join(dir, "vendor", "e.md"), "x")
	writeFile(t, filepath.Join(dir, "f.txt"), "x") // not markdown

	got := listAllMarkdown(dir)
	if len(got) != 2 {
		t.Fatalf("listAllMarkdown found %v, want 2 files (a.md, sub/b.md)", got)
	}
	for _, p := range got {
		if !strings.HasSuffix(p, "a.md") && !strings.HasSuffix(p, "b.md") {
			t.Errorf("unexpected file %q in result", p)
		}
	}
}

// TestRunCheckExplicitPaths runs the subcommand over an explicit file: a prose
// line with two sentences is reported on stdout with a summary on stderr, and
// the exit code is 1.
//
// [Ja] TestRunCheckExplicitPaths は明示ファイルに対してサブコマンドを動かす。
// 2 文を含む地の文が stdout に報告され、要約が stderr に出て、終了コードは 1。
func TestRunCheckExplicitPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	writeFile(t, bad, "一文目。二文目。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{bad}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), bad+":1:") {
		t.Errorf("stdout = %q, want a finding at %s:1", stdout.String(), bad)
	}
	if !strings.Contains(stderr.String(), "semantic-line-break violation") {
		t.Errorf("stderr = %q, want a violation summary", stderr.String())
	}
}

// TestRunCheckClean confirms a one-sentence-per-line file exits 0 with no output.
// [Ja] TestRunCheckClean は 1 文 1 行のファイルが無出力・終了コード 0 になることを確認する。
func TestRunCheckClean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.md")
	writeFile(t, good, "一文だけ。\n別の行も一文だけ。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{good}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run code = %d, want 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

// TestRunCheckReadErrorExitsNonZero confirms that, with an explicit path that
// cannot be read, check mode reports the error on stderr and exits non-zero
// instead of silently passing. This mirrors --write's treatment of a bad
// explicit path.
//
// [Ja] TestRunCheckReadErrorExitsNonZero は、読めない明示パスを渡したとき
// check モードが stderr へ報告して非ゼロ終了し、黙って成功扱いにしないことを
// 確認する。--write の明示パス読み取り失敗の扱いと揃える。
func TestRunCheckReadErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.md")

	var stdout, stderr bytes.Buffer
	code := Run([]string{missing}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("Run code = %d, want non-zero (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want an 'error:' line", stderr.String())
	}
}

// TestRunAllReadErrorIsSwallowed confirms that in --all mode a read failure on a
// walked file is swallowed (no error, exit 0), staying faithful to the Node
// original's check mode. It is skipped as root, which bypasses mode bits.
//
// [Ja] TestRunAllReadErrorIsSwallowed は、--all モードで走査対象ファイルの
// 読み取りに失敗しても握りつぶされる (エラーなし・終了コード 0) ことを確認し、
// Node 版 check モードに忠実であることを担保する。mode を無視する root 実行時は
// スキップする。
func TestRunAllReadErrorIsSwallowed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file mode permissions")
	}

	dir := t.TempDir()
	unreadable := filepath.Join(dir, "unreadable.md")
	writeFile(t, unreadable, "一文だけ。\n")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", unreadable, err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--all"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run --all code = %d, want 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want no 'error:' line (--all swallows read failures)", stderr.String())
	}
}

// TestRunWrite confirms --write rewrites the file in place and exits 0.
// [Ja] TestRunWrite は --write がファイルをその場で書き換え、終了コード 0 を返すことを確認する。
func TestRunWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	writeFile(t, bad, "一文目。二文目。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write", bad}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Run code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	got, err := os.ReadFile(bad) //#nosec G304
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "一文目。\n二文目。\n" {
		t.Errorf("rewritten file = %q, want %q", string(got), "一文目。\n二文目。\n")
	}
	if !strings.Contains(stdout.String(), "fixed:") {
		t.Errorf("stdout = %q, want a 'fixed:' line", stdout.String())
	}
}

// TestRunWriteReadErrorExitsNonZero confirms a --write run that cannot read an
// explicit path reports the error on stderr and exits non-zero, instead of
// silently succeeding.
//
// [Ja] TestRunWriteReadErrorExitsNonZero は --write で明示パスを読めない場合に
// stderr へ報告して非ゼロ終了し、黙って成功扱いにしないことを確認する。
func TestRunWriteReadErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.md")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write", missing}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("Run --write code = %d, want non-zero (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want an 'error:' line", stderr.String())
	}
}

// TestRunWriteWriteErrorExitsNonZero confirms a --write run that cannot write
// back (the target is read-only) reports the error on stderr and exits
// non-zero. It is skipped when running as root, which bypasses mode bits.
//
// [Ja] TestRunWriteWriteErrorExitsNonZero は書き戻せない (対象が読み取り専用) 場合に
// --write が stderr へ報告して非ゼロ終了することを確認する。mode を無視する root
// 実行時はスキップする。
func TestRunWriteWriteErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file mode permissions")
	}

	dir := t.TempDir()
	readonly := filepath.Join(dir, "readonly.md")
	writeFile(t, readonly, "一文目。二文目。\n")
	if err := os.Chmod(readonly, 0o400); err != nil {
		t.Fatalf("chmod %s: %v", readonly, err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write", readonly}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("Run --write code = %d, want non-zero (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want an 'error:' line", stderr.String())
	}
}

// TestRunAll confirms --all checks every .md under the working directory.
// It changes the working directory, so it does not run in parallel.
//
// [Ja] TestRunAll は --all が作業ディレクトリ配下の全 .md を検査することを確認する。
// 作業ディレクトリを変更するため並列実行しない。
func TestRunAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "一文目。二文目。\n")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--all"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run --all code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.md:1:") {
		t.Errorf("stdout = %q, want a finding at bad.md:1", stdout.String())
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

// writeFile writes content to path (creating parent dirs), failing on error.
// [Ja] writeFile は path に content を書き出す (親ディレクトリも作る)。失敗時はテストを止める。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
