// Package mdはkoryluslintのmdサブコマンドを実装し、Markdownドキュメントを
// 2つの観点で検査する。
//
//   - 句点改行 (semantic line break): 地の文を「1文1行」にし、文末の「。」で
//     改行する (korylus-writing-doc.md)
//   - 日本語テキストのスタイル: 全角丸括弧と、半角英数字と日本語の間のスペースを
//     禁じる (korylus-lang.md §3。ルール本体はinternal/styleが持つ)
//
// 句点改行は地の文だけを対象にするが、§3のスタイルは見出し・箇条書き・表・引用
// とYAMLフロントマターの値にも適用する。
//
// モードは3つ。
//
//	md [-base=<ref>] [paths...]   検査して違反を報告する。
//	md --write [...]              句点改行をその場で修正し、残る違反を報告する。
//	md --all [...]                全 .mdファイル (全行) を対象にする。
//
// スコープ: 明示パスを渡すとそのファイルを全行検査し、--allなら全 .mdを全行
// 検査する。
// それ以外はHEAD (または -baseの <ref>) との差分行のみを検査するため、触って
// いない既存ドキュメントは指摘されない。
package md

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/korylus/tools/internal/cli"
	"github.com/korylus/tools/internal/style"
)

const (
	writeUsage = "句点改行をその場で修正し、残るスタイル違反を報告する"
	allUsage   = "差分行だけでなく全 .mdを全行検査する"
)

// msgSentenceは複数の文を1行に詰めた地の文の報告メッセージ。
const msgSentence = "1行に複数の文が詰まっている (文末の「。」で改行する)"

var (
	// reHeadingはATX見出しの行にマッチする。
	reHeading = regexp.MustCompile(`^\s*#`)
	// reListは箇条書き・番号付きリストの項目にマッチする。
	reList = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)
	// reTableは表の行にマッチする。
	reTable = regexp.MustCompile(`^\s*\|`)
	// reBlockquoteは引用の行にマッチする。
	reBlockquote = regexp.MustCompile(`^\s*>`)
	// reHunkはunified diffのハンクヘッダーにマッチし、新側の開始行と
	// (任意の) 行数を取り出す。
	reHunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
)

// docLineは走査した1行のうち、検査に使う部分。
type docLine struct {
	// lineNoは1始まりの行番号。
	lineNo int
	// textは行そのもの。報告と句点改行の検査に使う。
	text string
	// proseはtextが地の文かどうか。句点改行は地の文だけを対象にする。
	// styleに空白しか残らない行は、コメントやコードの内側なので地の文に数えない。
	// YAMLフロントマターの行もMarkdownの本文ではないため地の文に数えない。
	prose bool
	// styleは §3スタイルと句点改行の区切り位置を判定するテキスト。
	// ASTで特定したコード本文とHTMLコメントを半角スペースに置き換える。
	// textとルーン数が揃うため、位置をtextへそのまま対応づけられる。
	style string
	// emphasisClosersはASTで強調の閉じと確認した記号の位置 (0始まりのルーン数)。
	// 後続文字だけでは開きと閉じを区別できないため、構文解析の結果を使う。
	emphasisClosers map[int]bool
}

// hitは検出した違反1件。
type hit struct {
	lineNo int
	line   string
	msg    string
	// fixableは --writeで直せる違反かどうか。句点改行だけが該当する。
	fixable bool
}

// Runはmdサブコマンドのエントリポイント。argsはサブコマンド名を除いた
// 残りの引数で、戻り値はプロセスの終了コード。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("md", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := cli.RegisterCommon(fs)
	var write, all bool
	fs.BoolVar(&write, "write", false, writeUsage)
	fs.BoolVar(&all, "all", false, allUsage)
	if err := fs.Parse(args); err != nil {
		// -h/--helpの要求はエラーではなく成功扱いにする。
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	paths := fs.Args()
	if write {
		return runWrite(paths, all, opts.Base, stdout, stderr)
	}
	return runCheck(paths, all, opts.Base, stdout, stderr)
}

// runCheckはスコープ内の違反を報告し、1件でもあれば1を返す。
//
// 明示パス指定時は、読み取り失敗を (パスのタイプミスなどの) ユーザーエラーと
// みなして非ゼロ終了させ (runWriteに揃える)、不正なパスを「指摘なし」と
// 取り違えないようにする。
// --all・差分スコープではファイル集合がファイルシステム / git由来のため、
// 読み取り失敗は無害な競合として握りつぶす (Node版checkモードに忠実)。
func runCheck(paths []string, all bool, base string, stdout, stderr io.Writer) int {
	scope := resolveScope(paths, all, base)
	explicit := len(paths) > 0
	files := sortedTargets(scope)

	total := 0
	fixable := 0
	failed := false
	for _, f := range files {
		lineSet := scope[f]
		src, err := os.ReadFile(f) //#nosec G304
		if err != nil {
			if explicit {
				fmt.Fprintf(stderr, "エラー: %s: %v\n", f, err)
				failed = true
			}
			continue
		}
		for _, h := range violations(string(src)) {
			// 行集合がnilならファイル全体がスコープ内。
			if lineSet != nil && !lineSet[h.lineNo] {
				continue
			}
			total++
			if h.fixable {
				fixable++
			}
			reportHit(stdout, f, h)
		}
	}
	if total > 0 {
		fmt.Fprintf(stderr, "\nkoryluslint md: 違反%d件\n", total)
		if fixable > 0 {
			fmt.Fprintf(stderr, "うち句点改行%d件は `koryluslint md --write` で修正できる\n", fixable)
		}
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// runWriteはスコープ内のファイルをその場で書き換え、変更点を報告する。
// 書き換えるのは句点改行だけで、§3スタイルの違反は機械的に直せないため報告に
// 留め、違反が残る場合は終了コード1を返す。
// --writeモードでも、どの行を書き換えるかは差分行で絞り込まない (ファイル単位
// でトップレベルの文末「。」だけが改行される) が、残存違反の報告はrunCheckと
// 同じ差分スコープに合わせる。
// 書き換えで行番号がずれるため、rewriteWithOriginsの対応表で元の行番号に戻して
// から行集合と突き合わせる。
// いずれかのファイルで読み取り・書き込みに失敗した場合はstderrに報告して
// 終了コードを非ゼロにし、壊れた --write実行を成功と取り違えないようにする。
func runWrite(paths []string, all bool, base string, stdout, stderr io.Writer) int {
	scope := resolveScope(paths, all, base)
	targets := sortedTargets(scope)

	changed := 0
	total := 0
	failed := false
	for _, f := range targets {
		lineSet := scope[f]
		src, err := os.ReadFile(f) //#nosec G304
		if err != nil {
			fmt.Fprintf(stderr, "エラー: %s: %v\n", f, err)
			failed = true
			continue
		}
		next, origins := rewriteWithOrigins(string(src))
		if next != string(src) {
			// modeは新規作成時のみ有効。既存ファイルの権限は維持される。
			if err := os.WriteFile(f, []byte(next), 0o600); err != nil {
				fmt.Fprintf(stderr, "エラー: %s: %v\n", f, err)
				failed = true
				continue
			}
			changed++
			fmt.Fprintf(stdout, "修正: %s\n", f)
		}
		// 修正の有無によらず、書き換え後の行番号で残存違反を報告する。
		for _, h := range violations(next) {
			if h.fixable {
				continue
			}
			// 行集合がnilならファイル全体がスコープ内。
			if lineSet != nil && !lineSet[originLine(origins, h.lineNo)] {
				continue
			}
			total++
			reportHit(stdout, f, h)
		}
	}
	switch {
	case changed > 0:
		fmt.Fprintf(stdout, "\n%dファイルを書き換えた。\n", changed)
	case !failed && total == 0:
		// 失敗で非ゼロ終了する場合は「修正なし」を出さない。stdoutの要約が
		// stderrの内容と矛盾しないようにするため。
		fmt.Fprintln(stdout, "修正するものは無い。")
	}
	if total > 0 {
		fmt.Fprintf(stderr, "\nkoryluslint md: 違反%d件\n", total)
	}

	if failed || total > 0 {
		return 1
	}
	return 0
}

// sortedTargetsはスコープに含まれるファイルをパスの順に並べて返す。
// 報告の並び順を検査モードと修正モードで揃えるために共有する。
func sortedTargets(scope map[string]map[int]bool) []string {
	files := make([]string, 0, len(scope))
	for f := range scope {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// reportHitは違反1件をwへ2行で出力する。
// 1行目はファイル・行番号・メッセージ、2行目は該当行の抜粋。
func reportHit(w io.Writer, file string, h hit) {
	fmt.Fprintf(w, "%s:%d: %s\n", file, h.lineNo, h.msg)
	fmt.Fprintf(w, "    %s\n", truncate(strings.TrimSpace(h.line), 100))
}

// originLineはrewriteWithOriginsの対応表を使い、書き換え後の行番号lineNoを元の
// 行番号へ戻す。
// 対応表の範囲外はそのままの行番号を返す。
func originLine(origins []int, lineNo int) int {
	if lineNo-1 < 0 || lineNo-1 >= len(origins) {
		return lineNo
	}
	return origins[lineNo-1]
}

// resolveScopeはどのファイルの (そしてその中のどの行を) 検査するかを決める。
// あるファイルの行集合がnilならファイル全体がスコープ内を意味する。
func resolveScope(paths []string, all bool, base string) map[string]map[int]bool {
	switch {
	case len(paths) > 0:
		scope := make(map[string]map[int]bool, len(paths))
		for _, f := range paths {
			scope[f] = nil
		}
		return scope
	case all:
		files := listAllMarkdown(".")
		scope := make(map[string]map[int]bool, len(files))
		for _, f := range files {
			scope[f] = nil
		}
		return scope
	default:
		return changedLines(base)
	}
}

// breakProseは地の文の1行を、トップレベル (括弧・角括弧・鉤括弧の外) にある
// 文末の「。」で分割し、各区切りに "\n" を挿入した行を返す。
// 「。」の後ろが空白のみ、次の非空白文字が閉じ括弧・閉じ鉤括弧のとき、または
// 「。」の直後が閉じの強調記号 (`**` / `_`) のときは区切らない。
//
// 区切り位置の判定はdl.styleで行い、出力はdl.textから組み立てる。
// dl.styleではコード片とコメントがマスクされているため、その中の「。」を避けられる。
// dl.emphasisClosersで、実際に対応する開きがある閉じ記号だけを保護する。
func breakProse(dl docLine) string {
	var out []string
	var buf strings.Builder
	depth := 0
	runes := []rune(dl.text)
	maskedRunes := []rune(dl.style)
	for i, c := range runes {
		buf.WriteRune(c)
		switch m := maskedRunes[i]; {
		case isOpen(m):
			depth++
		case isClose(m):
			if depth > 0 {
				depth--
			}
		case m == '。' && depth == 0:
			rest := maskedRunes[i+1:]
			if strings.TrimSpace(string(rest)) == "" {
				continue
			}
			// 閉じの強調記号は「。」との間に空白を置けないため、空白を落とす前に見る。
			if dl.emphasisClosers[i+1] {
				continue
			}
			trimmed := strings.TrimLeftFunc(string(rest), unicode.IsSpace)
			if isClose(firstRune(trimmed)) {
				continue
			}
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return strings.Join(out, "\n")
}

// isProseはlineが句点改行の検査対象となる地の文かを返す。
// 見出し・箇条書き・表・引用・空行は地の文ではない。
// YAMLフロントマターは1行だけでは判定できないため、eachLineが文書全体を見て
// 除外する。
func isProse(line string) bool {
	switch {
	case strings.TrimSpace(line) == "",
		reHeading.MatchString(line),
		reList.MatchString(line),
		reTable.MatchString(line),
		reBlockquote.MatchString(line):
		return false
	}
	return true
}

// eachProseLineは地の文の行ごとにonProse(dl) を呼ぶ。
func eachProseLine(text string, onProse func(dl docLine)) {
	eachLine(text, func(dl docLine) {
		if dl.prose {
			onProse(dl)
		}
	})
}

// rewriteは各地の文の行を改行済みの形に置き換えたtextを返す。
func rewrite(text string) string {
	next, _ := rewriteWithOrigins(text)
	return next
}

// rewriteWithOriginsはrewriteの結果と、書き換え後の各行に対応する元の行番号を
// 返す。
// originsは書き換え後の行番号から1を引いた位置に、その行が由来する元の行番号
// (1始まり) を持つ。
// 改行の挿入で行番号がずれるため、書き換え後に見つけた違反を差分スコープの行
// 集合と突き合わせるにはこの対応表が要る。
func rewriteWithOrigins(text string) (string, []int) {
	lines := strings.Split(text, "\n")
	replaced := map[int]string{}
	eachProseLine(text, func(dl docLine) {
		replaced[dl.lineNo] = breakProse(dl)
	})

	out := make([]string, 0, len(lines))
	origins := make([]int, 0, len(lines))
	for i, line := range lines {
		if r, ok := replaced[i+1]; ok {
			line = r
		}
		for _, part := range strings.Split(line, "\n") {
			out = append(out, part)
			origins = append(origins, i+1)
		}
	}
	return strings.Join(out, "\n"), origins
}

// violationsは句点改行と §3スタイルの違反を行番号順に返す。
// 同じ行に両方があれば句点改行を先に並べる。
func violations(text string) []hit {
	var hits []hit
	eachLine(text, func(dl docLine) {
		if dl.prose && breakProse(dl) != dl.text {
			hits = append(hits, hit{lineNo: dl.lineNo, line: dl.text, msg: msgSentence, fixable: true})
		}
		for _, v := range style.CheckMaskedLine(dl.style) {
			hits = append(hits, hit{lineNo: dl.lineNo, line: dl.text, msg: v.Message})
		}
	})
	return hits
}

// changedLinesは変更された各 .mdファイル (gitが報告するパス。作業ディレクトリ
// 基準) を、base (空ならHEAD) 以降に変更された新側の行番号集合へ対応づける。
// 集合がnilならファイル全体がスコープ内 (未追跡ファイルや差分を計算できない
// ファイル)。
// gitの失敗は握りつぶし、リポジトリ外などではエラーにせず空スコープにする。
func changedLines(base string) map[string]map[int]bool {
	rangeArg := "HEAD"
	if base != "" {
		rangeArg = base + "...HEAD"
	}
	result := map[string]map[int]bool{}

	nameOut, err := gitOutput("diff", "--name-only", rangeArg, "--", "*.md")
	if err != nil {
		return result
	}
	files := nonEmptyLines(nameOut)
	// 未追跡の .mdは全行を対象にする。
	if untracked, uerr := gitOutput("ls-files", "--others", "--exclude-standard", "--", "*.md"); uerr == nil {
		files = append(files, nonEmptyLines(untracked)...)
	}

	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true

		diff, derr := gitOutput("diff", "--unified=0", rangeArg, "--", f)
		if derr != nil || strings.TrimSpace(diff) == "" {
			// 差分が取れない (未追跡など) ファイルは全行を対象にする。
			result[f] = nil
			continue
		}
		set := map[int]bool{}
		sc := bufio.NewScanner(strings.NewReader(diff))
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			m := reHunk.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			for n := 0; n < count; n++ {
				set[start+n] = true
			}
		}
		result[f] = set
	}
	return result
}

// listAllMarkdownはroot配下の全 .mdファイルを返す。
// node_modules・.git・vendorディレクトリはスキップする。
func listAllMarkdown(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".md") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// gitOutputはgitコマンドを実行し標準出力を返す。
func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// nonEmptyLinesはsを改行で分割し空の要素を落とす (JavaScript版の
// split("\n").filter(Boolean) に対応)。
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// isOpenはrが開き括弧・開き鉤括弧 (半角・全角) かを返す。
func isOpen(r rune) bool { return strings.ContainsRune("(（[「『【〔《", r) }

// isCloseはrが閉じ括弧・閉じ鉤括弧 (半角・全角) かを返す。
func isClose(r rune) bool { return strings.ContainsRune(")）]」』】〕》", r) }

// firstRuneはsの先頭ルーンを返す。sが空なら0。
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// truncateは表示用にsを最大nルーンへ短縮する。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
