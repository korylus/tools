package md

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// msgsOfは違反のメッセージの一覧を返す。
func msgsOf(hits []hit) []string {
	got := make([]string, len(hits))
	for i, h := range hits {
		got[i] = h.msg
	}
	return got
}

// linesOfは違反の行番号の一覧を返す。
func linesOf(hits []hit) []int {
	got := make([]int, len(hits))
	for i, h := range hits {
		got[i] = h.lineNo
	}
	return got
}

// maskedLineOfはlineを1行だけのドキュメントとして走査し、breakProseへ渡す
// マスク済みの行を返す。
// マスクの組み立てはeachLineの責務なので、テストで手書きせずに実物を使う。
func maskedLineOf(t *testing.T, line string) string {
	t.Helper()

	var masked string
	eachLine(line+"\n", func(dl docLine) {
		masked = dl.style
	})
	return masked
}

func TestBreakProse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "1文だけの行はそのまま",
			in:   "一文だけ。",
			want: "一文だけ。",
		},
		{
			name: "2文は内側の「。」で改行する",
			in:   "これは一文目です。これは二文目です。",
			want: "これは一文目です。\nこれは二文目です。",
		},
		{
			name: "半角丸括弧の中の「。」は壊さない",
			in:   "括弧 (中の。は壊さない) の外。次の文。",
			want: "括弧 (中の。は壊さない) の外。\n次の文。",
		},
		{
			name: "全角丸括弧の中の「。」は壊さない",
			in:   "全角 （中の。は壊さない） の外。次。",
			want: "全角 （中の。は壊さない） の外。\n次。",
		},
		{
			name: "角括弧・鉤括弧の中の「。」は壊さない",
			in:   "角 [a。b] と鉤 「c。d」 の外。次。",
			want: "角 [a。b] と鉤 「c。d」 の外。\n次。",
		},
		{
			name: "インラインコードの中の「。」は壊さない",
			in:   "コード `a。b` の後。終わり。",
			want: "コード `a。b` の後。\n終わり。",
		},
		{
			name: "「。」の直後が閉じ鉤括弧なら改行しない",
			in:   "「文。」と続く。",
			want: "「文。」と続く。",
		},
		{
			name: "末尾の「。」では空の要素を作らない",
			in:   "末尾の文。",
			want: "末尾の文。",
		},
		{
			name: "「。」の無い英文はそのまま",
			in:   "This line has no Japanese period.",
			want: "This line has no Japanese period.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := breakProse(tt.in, maskedLineOf(t, tt.in)); got != tt.want {
				t.Errorf("breakProse(%q) = %q、期待値 = %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEachProseLineは地の文の行だけが拾われ、見出し・リスト・表・引用・
// コードフェンス・HTMLコメント・空行がすべてスキップされることを確認する。
func TestEachProseLine(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"# 見出し", // 1: 見出し
		"",      // 2: 空行
		"これは地の文。これは二文目。", // 3: 地の文
		"",                 // 4: 空行
		"- リスト項目。これは無視。",   // 5: リスト
		"",                 // 6: 空行
		"| 表 | の。行 |",      // 7: 表
		"",                 // 8: 空行
		"> 引用。無視。",         // 9: 引用
		"",                 // 10: 空行
		"```",              // 11: フェンス開始
		"fence内。無視。",       // 12: フェンス内
		"```",              // 13: フェンス終了
		"",                 // 14: 空行
		"<!-- コメント。無視 -->", // 15: 1行で閉じるコメント
		"",                 // 16: 空行
		"普通の段落。続き。",        // 17: 地の文
	}, "\n")

	var got []int
	eachProseLine(doc, func(dl docLine) {
		got = append(got, dl.lineNo)
	})

	want := []int{3, 17}
	if len(got) != len(want) {
		t.Fatalf("地の文の行 = %v、期待値 = %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("地の文の行 = %v、期待値 = %v", got, want)
		}
	}
}

// TestEachProseLineMultiLineCommentは複数行のHTMLコメントブロックが
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
	eachProseLine(doc, func(dl docLine) {
		got = append(got, dl.lineNo)
	})

	if len(got) != 1 || got[0] != 4 {
		t.Errorf("地の文の行 = %v、期待値 = [4]", got)
	}
}

// TestRewriteは地の文の行だけが書き換えられ、構造行 (見出し・リスト・
// コードフェンス) はそのまま通ることを確認する。
func TestRewrite(t *testing.T) {
	t.Parallel()

	in := strings.Join([]string{
		"# タイトル",
		"",
		"一文目。二文目。",
		"",
		"- 項目。これはリストなので無視。",
	}, "\n")
	want := strings.Join([]string{
		"# タイトル",
		"",
		"一文目。",
		"二文目。",
		"",
		"- 項目。これはリストなので無視。",
	}, "\n")

	if got := rewrite(in); got != want {
		t.Errorf("rewrite() =\n%q\n期待値\n%q", got, want)
	}
}

// TestRewriteKeepsStyleViolationsは --writeが §3スタイルの違反を書き換えない
// ことを確認する。全角丸括弧の半角化は前後のスペースの調整を伴い、機械的に
// 直せないため報告に留める。
func TestRewriteKeepsStyleViolations(t *testing.T) {
	t.Parallel()

	in := "全角（括弧）を含む一文目。二文目。"
	want := "全角（括弧）を含む一文目。\n二文目。"

	if got := rewrite(in); got != want {
		t.Errorf("rewrite() =\n%q\n期待値\n%q", got, want)
	}
}

func TestViolations(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"一文目。二文目。", // 1行目: 違反
		"",         // 2行目
		"単独の文。",    // 3行目: 違反なし
	}, "\n")

	hits := violations(doc)
	if len(hits) != 1 {
		t.Fatalf("違反 %d件、期待値 = 1件", len(hits))
	}
	if hits[0].lineNo != 1 {
		t.Errorf("違反の行番号 = %d、期待値 = 1", hits[0].lineNo)
	}
	if !hits[0].fixable {
		t.Error("句点改行の違反は --writeで直せるものとして扱う")
	}
}

// TestViolationsStyleは §3スタイルの検査が地の文だけでなく見出し・箇条書き・
// 表・引用にも適用され、コードフェンスとHTMLコメントの中には適用されないことを
// 確認する。
func TestViolationsStyle(t *testing.T) {
	t.Parallel()

	doc := strings.Join([]string{
		"# REST API の見出し", // 1: 見出し (§3.2)
		"",                // 2
		"地の文に全角（括弧）がある。", // 3: 地の文 (§3.1)
		"",             // 4
		"- 最大 20 文字まで", // 5: 箇条書き (§3.2)
		"",             // 6
		"| 列 | Go 版 |", // 7: 表 (§3.2)
		"",             // 8
		"> 引用の Go 版",   // 9: 引用 (§3.2)
		"",             // 10
		"```",          // 11
		"fence内の（括弧）",  // 12: フェンス内なので対象外
		"```",          // 13
		"",             // 14
		"<!--",         // 15
		"コメント内の（括弧）",   // 16: コメント内なので対象外
		"-->",          // 17
	}, "\n")

	got := linesOf(violations(doc))
	want := []int{1, 3, 5, 7, 9}
	if len(got) != len(want) {
		t.Fatalf("違反の行番号 = %v、期待値 = %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("違反の行番号 = %v、期待値 = %v", got, want)
		}
	}
	for _, h := range violations(doc) {
		if h.fixable {
			t.Errorf("%d行目: §3スタイルの違反は --writeで直せるものとして扱わない", h.lineNo)
		}
	}
}

// TestViolationsInlineCommentは1行で閉じるHTMLコメントが §3の検査から外れ、
// 同じ行の残りは検査対象に残ることを確認する。
// ガイドラインは悪い例に注記としてコメントを添えるため、コメントを含む行を
// まるごと外すと悪い例の検出漏れになる。
func TestViolationsInlineComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		doc      string
		wantHits int
	}{
		{
			name:     "注記の中の全角丸括弧は対象外",
			doc:      "- 半角に直す <!-- 全角丸括弧（）は使わない -->",
			wantHits: 0,
		},
		{
			name:     "注記を除いた本文は対象に残る",
			doc:      "- 半角英数字とアンダースコアのみ（最大20文字） <!-- 全角丸括弧を使っている -->",
			wantHits: 1,
		},
		{
			name:     "インラインコードで囲んだ悪い例は対象外",
			doc:      "- `半角英数字とアンダースコアのみ（最大20文字）` <!-- 全角丸括弧を使っている -->",
			wantHits: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := violations(tt.doc); len(got) != tt.wantHits {
				t.Errorf("violations(%q) = %v、期待値 = %d件", tt.doc, msgsOf(got), tt.wantHits)
			}
		})
	}
}

// TestViolationsInlineCommentSentenceBreakは、1行で閉じるHTMLコメントを含む行
// でも句点改行を検査し、コメントの中の「。」では区切らないことを確認する。
// コメントを含む行をまるごと外すと、注記を添えた地の文の句点改行を見逃す。
func TestViolationsInlineCommentSentenceBreak(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		doc      string
		wantHits int
		want     string
	}{
		{
			name:     "注記より前の本文は句点改行の対象",
			doc:      "本文です。ここも文です。 <!-- 注記 -->\n",
			wantHits: 1,
			want:     "本文です。\nここも文です。 <!-- 注記 -->\n",
		},
		{
			name:     "注記の中の「。」では区切らない",
			doc:      "本文です。 <!-- 注記。です -->\n",
			wantHits: 0,
			want:     "本文です。 <!-- 注記。です -->\n",
		},
		{
			name:     "複数行コメントの中は対象外",
			doc:      "<!--\nコメント。二文目。\n-->\n",
			wantHits: 0,
			want:     "<!--\nコメント。二文目。\n-->\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := violations(tt.doc); len(got) != tt.wantHits {
				t.Errorf("violations(%q) = %v、期待値 = %d件", tt.doc, msgsOf(got), tt.wantHits)
			}
			if got := rewrite(tt.doc); got != tt.want {
				t.Errorf("書き換え結果 = %q、期待値 = %q", got, tt.want)
			}
		})
	}
}

// TestViolationsOrderは同じ行に句点改行と §3スタイルの違反があるとき、
// 句点改行が先に並ぶことを確認する。
func TestViolationsOrder(t *testing.T) {
	t.Parallel()

	hits := violations("全角（括弧）の一文目。二文目。")
	if len(hits) != 2 {
		t.Fatalf("違反 %d件、期待値 = 2件 (%v)", len(hits), msgsOf(hits))
	}
	if hits[0].msg != msgSentence {
		t.Errorf("1件目 = %q、期待値 = %q", hits[0].msg, msgSentence)
	}
	if !strings.HasPrefix(hits[1].msg, "§3.1") {
		t.Errorf("2件目 = %q、期待値 = §3.1の違反", hits[1].msg)
	}
}

func TestListAllMarkdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "x")
	writeFile(t, filepath.Join(dir, "sub", "b.md"), "x")
	// スキップ対象のディレクトリのファイルは含めない。
	writeFile(t, filepath.Join(dir, "node_modules", "c.md"), "x")
	writeFile(t, filepath.Join(dir, ".git", "d.md"), "x")
	writeFile(t, filepath.Join(dir, "vendor", "e.md"), "x")
	writeFile(t, filepath.Join(dir, "f.txt"), "x") // Markdownではない

	got := listAllMarkdown(dir)
	if len(got) != 2 {
		t.Fatalf("listAllMarkdownの結果 = %v、期待値 = 2ファイル (a.md, sub/b.md)", got)
	}
	for _, p := range got {
		if !strings.HasSuffix(p, "a.md") && !strings.HasSuffix(p, "b.md") {
			t.Errorf("結果に想定外のファイル %q が含まれる", p)
		}
	}
}

// TestRunCheckExplicitPathsは明示ファイルに対してサブコマンドを動かす。
// 2文を含む地の文がstdoutに報告され、要約がstderrに出て、終了コードは1。
func TestRunCheckExplicitPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	writeFile(t, bad, "一文目。二文目。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{bad}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Runの終了コード = %d、期待値 = 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), bad+":1:") {
		t.Errorf("stdout = %q、期待値 = %s:1の指摘", stdout.String(), bad)
	}
	if !strings.Contains(stderr.String(), "違反1件") {
		t.Errorf("stderr = %q、期待値 = 違反の要約", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--write") {
		t.Errorf("stderr = %q、期待値 = --writeの案内", stderr.String())
	}
}

// TestRunCheckReportsStyleは §3スタイルの違反がstdoutに報告され、--writeの
// 案内が出ないことを確認する。
func TestRunCheckReportsStyle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	writeFile(t, bad, "# 見出し\n\n全角（括弧）を使っている。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{bad}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Runの終了コード = %d、期待値 = 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), bad+":3: §3.1") {
		t.Errorf("stdout = %q、期待値 = %s:3の §3.1の指摘", stdout.String(), bad)
	}
	if strings.Contains(stderr.String(), "--write") {
		t.Errorf("stderr = %q、期待値 = --writeの案内なし (スタイル違反は自動修正できない)", stderr.String())
	}
}

// TestRunCheckCleanは1文1行で §3に従うファイルが無出力・終了コード0になることを
// 確認する。
func TestRunCheckClean(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.md")
	writeFile(t, good, "一文だけ。\n別の行も一文だけ。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{good}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Runの終了コード = %d、期待値 = 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q、期待値 = 空", stdout.String())
	}
}

// TestRunCheckReadErrorExitsNonZeroは、読めない明示パスを渡したときcheckモードが
// stderrへ報告して非ゼロ終了し、黙って成功扱いにしないことを確認する。
// --writeの明示パス読み取り失敗の扱いと揃える。
func TestRunCheckReadErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.md")

	var stdout, stderr bytes.Buffer
	code := Run([]string{missing}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("Runの終了コード = %d、期待値 = 非ゼロ (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "エラー:") {
		t.Errorf("stderr = %q、期待値 = エラー行", stderr.String())
	}
}

// TestRunAllReadErrorIsSwallowedは、--allモードで走査対象ファイルの読み取りに
// 失敗しても握りつぶされる (エラーなし・終了コード0) ことを確認し、Node版
// checkモードに忠実であることを担保する。
// modeを無視するroot実行時はスキップする。
func TestRunAllReadErrorIsSwallowed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root実行ではファイルのmodeによる権限制御が効かない")
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
		t.Fatalf("Run --allの終了コード = %d、期待値 = 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "エラー:") {
		t.Errorf("stderr = %q、期待値 = エラー行なし (--allは読み取り失敗を握りつぶす)", stderr.String())
	}
}

// TestRunWriteは --writeがファイルをその場で書き換え、終了コード0を返すことを
// 確認する。
func TestRunWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.md")
	writeFile(t, bad, "一文目。二文目。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write", bad}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("Runの終了コード = %d、期待値 = 0 (stderr: %s)", code, stderr.String())
	}
	got, err := os.ReadFile(bad) //#nosec G304
	if err != nil {
		t.Fatalf("読み戻し: %v", err)
	}
	if string(got) != "一文目。\n二文目。\n" {
		t.Errorf("書き換え後のファイル = %q、期待値 = %q", string(got), "一文目。\n二文目。\n")
	}
	if !strings.Contains(stdout.String(), "修正:") {
		t.Errorf("stdout = %q、期待値 = 修正行", stdout.String())
	}
}

// TestRunWriteReadErrorExitsNonZeroは --writeで明示パスを読めない場合に
// stderrへ報告して非ゼロ終了し、黙って成功扱いにしないことを確認する。
func TestRunWriteReadErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.md")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write", missing}, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("Run --writeの終了コード = %d、期待値 = 非ゼロ (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "エラー:") {
		t.Errorf("stderr = %q、期待値 = エラー行", stderr.String())
	}
}

// TestRunWriteWriteErrorExitsNonZeroは書き戻せない (対象が読み取り専用) 場合に
// --writeがstderrへ報告して非ゼロ終了することを確認する。
// modeを無視するroot実行時はスキップする。
func TestRunWriteWriteErrorExitsNonZero(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root実行ではファイルのmodeによる権限制御が効かない")
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
		t.Fatalf("Run --writeの終了コード = %d、期待値 = 非ゼロ (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "エラー:") {
		t.Errorf("stderr = %q、期待値 = エラー行", stderr.String())
	}
}

// TestRunAllは --allが作業ディレクトリ配下の全 .mdを検査することを確認する。
// 作業ディレクトリを変更するため並列実行しない。
func TestRunAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.md"), "一文目。二文目。\n")
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--all"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("Run --allの終了コード = %d、期待値 = 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bad.md:1:") {
		t.Errorf("stdout = %q、期待値 = bad.md:1の指摘", stdout.String())
	}
}

// TestRunHelpExitsZeroは -h要求が成功扱いになることを確認する。
func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("Run(-h) の終了コード = %d、期待値 = 0", code)
	}
}

// writeFileはpathにcontentを書き出す (親ディレクトリも作る)。失敗時はテストを
// 止める。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRunWriteReportsRemainingStyleは修正が無い場合にも違反を報告し、
// 改行を挿入した場合は更新後の行番号を報告することを確認する。
func TestRunWriteReportsRemainingStyle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		line    string
		changed bool
	}{
		{"スタイル違反のみ", "全角（括弧）。\n", "全角（括弧）。\n", ":1:", false},
		{"改行とスタイル違反", "一文目。全角（括弧）。\n", "一文目。\n全角（括弧）。\n", ":2:", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			file := filepath.Join(t.TempDir(), "doc.md")
			writeFile(t, file, tt.in)
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"--write", file}, &stdout, &stderr); code != 1 {
				t.Fatalf("終了コード = %d、期待値 = 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), file+tt.line+" §3.1") {
				t.Errorf("stdout = %q、期待値 = 修正後の行番号での違反報告", stdout.String())
			}
			if strings.Contains(stdout.String(), "修正:") != tt.changed {
				t.Errorf("stdout = %q、修正報告の期待値 = %t", stdout.String(), tt.changed)
			}
			if strings.Contains(stdout.String(), "修正するものは無い") {
				t.Errorf("未修正の違反が残っているのに修正なしと報告している: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "違反1件") || strings.Contains(stderr.String(), "--write") {
				t.Errorf("stderr = %q、期待値 = 残存違反の件数のみ", stderr.String())
			}
			got, err := os.ReadFile(file) //#nosec G304
			if err != nil {
				t.Fatalf("読み戻し: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("ファイル = %q、期待値 = %q", got, tt.want)
			}
		})
	}
}

// initGitRepoはgitリポジトリを作り、nameにcontentを書いてコミットしたうえで、
// 作業ディレクトリをそのリポジトリへ移す。
// 差分スコープはgitの出力に依存するため、gitが無い環境ではスキップする。
func initGitRepo(t *testing.T, name, content string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("gitが無いため差分スコープを検証できない")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, name)
	writeFile(t, path, content)
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...) //#nosec G204
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	t.Chdir(dir)

	return path
}

// TestRunWriteIgnoresViolationsOutsideDiffScopeは、差分スコープでの --writeが
// 触っていない行の残存違反を報告しないことを確認する。
// 報告を絞らないと、既存違反を含むファイルを1行編集しただけで修正用のターゲット
// が失敗する。
// 作業ディレクトリを変更するため並列実行しない。
func TestRunWriteIgnoresViolationsOutsideDiffScope(t *testing.T) {
	path := initGitRepo(t, "doc.md", "既存の行に全角（括弧）がある。\n")
	writeFile(t, path, "既存の行に全角（括弧）がある。\n\n追加した行。もう一文。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("終了コード = %d、期待値 = 0 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "doc.md:1:") {
		t.Errorf("stdout = %q、期待値 = 触っていない1行目の違反を報告しない", stdout.String())
	}
	want := "既存の行に全角（括弧）がある。\n\n追加した行。\nもう一文。\n"
	got, err := os.ReadFile(path) //#nosec G304
	if err != nil {
		t.Fatalf("読み戻し: %v", err)
	}
	if string(got) != want {
		t.Errorf("書き換え後のファイル = %q、期待値 = %q", got, want)
	}
}

// TestRunWriteReportsViolationsInDiffScopeは、差分スコープでの --writeが変更した
// 行の残存違反を、書き換えでずれた後の行番号で報告することを確認する。
// 作業ディレクトリを変更するため並列実行しない。
func TestRunWriteReportsViolationsInDiffScope(t *testing.T) {
	path := initGitRepo(t, "doc.md", "既存の行に全角（括弧）がある。\n")
	writeFile(t, path, "既存の行に全角（括弧）がある。\n\n追加した行。全角（括弧）の二文目。\n")

	var stdout, stderr bytes.Buffer
	code := Run([]string{"--write"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("終了コード = %d、期待値 = 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	// 3行目が2行に分かれるため、残った違反は4行目になる。
	if !strings.Contains(stdout.String(), "doc.md:4: §3.1") {
		t.Errorf("stdout = %q、期待値 = doc.md:4の §3.1の指摘", stdout.String())
	}
	if strings.Contains(stdout.String(), "doc.md:1:") {
		t.Errorf("stdout = %q、期待値 = 触っていない1行目の違反を報告しない", stdout.String())
	}
	if !strings.Contains(stderr.String(), "違反1件") {
		t.Errorf("stderr = %q、期待値 = 残存違反の件数", stderr.String())
	}
}

// TestRunCheckIgnoresViolationsOutsideDiffScopeは、checkモードの差分スコープが
// スタイル違反にも効くことを確認する。
// 作業ディレクトリを変更するため並列実行しない。
func TestRunCheckIgnoresViolationsOutsideDiffScope(t *testing.T) {
	path := initGitRepo(t, "doc.md", "既存の行に全角（括弧）がある。\n")
	writeFile(t, path, "既存の行に全角（括弧）がある。\n\n追加した行に全角（括弧）がある。\n")

	var stdout, stderr bytes.Buffer
	code := Run(nil, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("終了コード = %d、期待値 = 1 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "doc.md:3: §3.1") {
		t.Errorf("stdout = %q、期待値 = doc.md:3の §3.1の指摘", stdout.String())
	}
	if strings.Contains(stdout.String(), "doc.md:1:") {
		t.Errorf("stdout = %q、期待値 = 触っていない1行目の違反を報告しない", stdout.String())
	}
}

// TestRunReportsProseBetweenCodeSpansは、ASTでコード片を除外した後も本文の
// 違反を検出し、検査・修正の両モードで失敗として報告することを確認する。
func TestRunReportsProseBetweenCodeSpans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		line string
	}{
		{"複数行のコード片の後ろ", "`コード\n続き` 本文（違反） Go 版 `コード（除外） Go 版`\n", ":2:"},
		{"複数バッククォートのコード片の後ろ", "``コード `\n続き`` 本文（違反） Go 版 ``コード（除外） Go 版``\n", ":2:"},
		{"エスケープされたバッククォート", "\\`本文（違反） Go 版\\`\n", ":1:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, write := range []bool{false, true} {
				file := filepath.Join(t.TempDir(), "doc.md")
				writeFile(t, file, tt.doc)
				args := []string{file}
				if write {
					args = append([]string{"--write"}, args...)
				}
				var stdout, stderr bytes.Buffer
				if code := Run(args, &stdout, &stderr); code != 1 {
					t.Fatalf("write=%t: 終了コード = %d、期待値 = 1 (stdout: %s, stderr: %s)", write, code, stdout.String(), stderr.String())
				}
				for _, section := range []string{"3.1", "3.2"} {
					if !strings.Contains(stdout.String(), file+tt.line+" §"+section) {
						t.Errorf("write=%t: stdout = %q、期待値 = %s行の §%sの指摘", write, stdout.String(), tt.line, section)
					}
				}
				if !strings.Contains(stderr.String(), "違反2件") {
					t.Errorf("write=%t: stderr = %q、期待値 = 本文の違反2件", write, stderr.String())
				}
				got, err := os.ReadFile(file) //#nosec G304
				if err != nil {
					t.Fatalf("読み戻し: %v", err)
				}
				if string(got) != tt.doc {
					t.Errorf("write=%t: ファイル = %q、期待値 = %q", write, got, tt.doc)
				}
			}
		})
	}
}
