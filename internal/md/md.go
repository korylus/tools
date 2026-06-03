// Package md implements the md subcommand of koryluslint, which checks (and can
// fix) semantic line breaks in Markdown documents: body prose is written one
// sentence per line, breaking at each sentence-final "。", per
// korylus-writing-doc.md.
//
// Modes:
//
//	md [-base=<ref>] [paths...]   report prose lines that pack multiple sentences.
//	md --write [...]              rewrite those lines in place instead of reporting.
//	md --all [...]                target every .md file (full files).
//
// Scope: with explicit paths the given files are checked in full; with --all
// every .md under the tree is checked in full; otherwise only lines changed vs
// HEAD (or vs <ref> with -base) are checked, so untouched legacy documents are
// never flagged.
//
// [Ja] md パッケージは koryluslint の md サブコマンドを実装し、Markdown ドキュメントの
// 句点改行 (semantic line break) を検査・修正する。地の文を「1 文 1 行」にし、文末の
// 「。」で改行する。ルールは korylus-writing-doc.md を参照。
//
// モード:
//
//	md [-base=<ref>] [paths...]   複数文が 1 行に詰まった地の文を報告する。
//	md --write [...]              報告ではなく該当行をその場で書き換える。
//	md --all [...]                全 .md ファイル (全行) を対象にする。
//
// スコープ: 明示パスを渡すとそのファイルを全行検査し、--all なら全 .md を全行検査する。
// それ以外は HEAD (または -base の <ref>) との差分行のみを検査するため、触っていない
// 既存ドキュメントは指摘されない。
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
)

const (
	writeUsage = "rewrite files in place instead of reporting / 報告ではなくファイルをその場で書き換える"
	allUsage   = "check every .md file in full, not just changed lines / 差分行だけでなく全 .md を全行検査する"
)

// msgViolation is the report line for a prose line that packs multiple
// sentences. Program output is English only (see the implementation decision
// log), unlike the comment block above the offending line.
//
// [Ja] msgViolation は複数文を 1 行に詰めた地の文の報告メッセージ。プログラム出力は
// 英語のみとする (実装判断ログ参照)。
const msgViolation = `multiple sentences on one line; break at each "。" (semantic line break)`

var (
	// reFence matches a code-fence delimiter line (``` optionally indented).
	// [Ja] reFence はコードフェンス区切り行 (``` 。インデント可) にマッチする。
	reFence = regexp.MustCompile("^\\s*```")
	// reHeading matches an ATX heading line.
	// [Ja] reHeading は ATX 見出し行にマッチする。
	reHeading = regexp.MustCompile(`^\s*#`)
	// reList matches a bullet or ordered list item.
	// [Ja] reList は箇条書き・番号付きリストの項目にマッチする。
	reList = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)
	// reTable matches a table row.
	// [Ja] reTable は表の行にマッチする。
	reTable = regexp.MustCompile(`^\s*\|`)
	// reBlockquote matches a blockquote line.
	// [Ja] reBlockquote は引用行にマッチする。
	reBlockquote = regexp.MustCompile(`^\s*>`)
	// reHunk matches a unified-diff hunk header, capturing the new-side start
	// line and (optional) line count.
	//
	// [Ja] reHunk は unified diff のハンクヘッダーにマッチし、新側の開始行と
	// (任意の) 行数を取り出す。
	reHunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)
)

// proseHit is one prose line that violates the one-sentence-per-line rule.
// [Ja] proseHit は「1 文 1 行」規則に違反した地の文の行 1 件。
type proseHit struct {
	lineNo int
	line   string
}

// Run is the entry point of the md subcommand. args is what remains after the
// subcommand name, and the return value is the process exit code.
// [Ja] Run は md サブコマンドのエントリポイント。args はサブコマンド名を除いた
// 残りの引数で、戻り値はプロセスの終了コード。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("md", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := cli.RegisterCommon(fs)
	var write, all bool
	fs.BoolVar(&write, "write", false, writeUsage)
	fs.BoolVar(&all, "all", false, allUsage)
	if err := fs.Parse(args); err != nil {
		// A -h/--help request is a success, not a usage error.
		// [Ja] -h/--help の要求はエラーではなく成功扱いにする。
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

// runCheck reports violations within scope and returns 1 when any are found.
//
// With explicit paths, a read failure is treated as a user error (e.g. a
// mistyped path) and makes the command exit non-zero, mirroring runWrite, so a
// bad path is never mistaken for "clean". For --all and diff scope the file set
// comes from the filesystem / git, so a read failure is a benign race and is
// swallowed (faithful to the Node original's check mode).
//
// [Ja] runCheck はスコープ内の違反を報告し、1 件でもあれば 1 を返す。
//
// 明示パス指定時は、読み取り失敗を (パスのタイプミスなどの) ユーザーエラーと
// みなして非ゼロ終了させ (runWrite に揃える)、不正なパスを「指摘なし」と
// 取り違えないようにする。--all・差分スコープではファイル集合がファイル
// システム / git 由来のため、読み取り失敗は無害な競合として握りつぶす
// (Node 版 check モードに忠実)。
func runCheck(paths []string, all bool, base string, stdout, stderr io.Writer) int {
	scope := resolveScope(paths, all, base)
	explicit := len(paths) > 0

	files := make([]string, 0, len(scope))
	for f := range scope {
		files = append(files, f)
	}
	sort.Strings(files)

	total := 0
	failed := false
	for _, f := range files {
		lineSet := scope[f]
		src, err := os.ReadFile(f) //#nosec G304
		if err != nil {
			if explicit {
				fmt.Fprintf(stderr, "error: %s: %v\n", f, err)
				failed = true
			}
			continue
		}
		for _, h := range violations(string(src)) {
			// A nil line set means the whole file is in scope.
			// [Ja] 行集合が nil ならファイル全体がスコープ内。
			if lineSet != nil && !lineSet[h.lineNo] {
				continue
			}
			total++
			fmt.Fprintf(stdout, "%s:%d: %s\n", f, h.lineNo, msgViolation)
			fmt.Fprintf(stdout, "    %s\n", truncate(strings.TrimSpace(h.line), 100))
		}
	}
	if total > 0 {
		fmt.Fprintf(stderr, "\nkoryluslint md: %d semantic-line-break violation(s); run `koryluslint md --write` to fix\n", total)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// runWrite rewrites the in-scope files in place and reports what changed.
// In --write mode the scope is file-granular (changed lines are not used to
// limit which lines are rewritten; only sentence-final "。" at top level break).
// A read or write failure on any file is reported to stderr and makes the
// command exit non-zero, so a broken --write run is never mistaken for success.
//
// [Ja] runWrite はスコープ内のファイルをその場で書き換え、変更点を報告する。
// --write モードではスコープはファイル単位で、どの行を書き換えるかを差分行で
// 絞り込まない (トップレベルの文末「。」だけが改行される)。
// いずれかのファイルで読み取り・書き込みに失敗した場合は stderr に報告して
// 終了コードを非ゼロにし、壊れた --write 実行を成功と取り違えないようにする。
func runWrite(paths []string, all bool, base string, stdout, stderr io.Writer) int {
	var targets []string
	switch {
	case len(paths) > 0:
		targets = paths
	case all:
		targets = listAllMarkdown(".")
	default:
		for f := range changedLines(base) {
			targets = append(targets, f)
		}
	}
	sort.Strings(targets)

	changed := 0
	failed := false
	for _, f := range targets {
		src, err := os.ReadFile(f) //#nosec G304
		if err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", f, err)
			failed = true
			continue
		}
		next := rewrite(string(src))
		if next == string(src) {
			continue
		}
		// The mode is only honored on create; existing files keep their mode,
		// so writing back the rewritten content does not change permissions.
		// [Ja] mode は新規作成時のみ有効。既存ファイルは元の mode を保つため、
		// 書き戻しても権限は変わらない。
		if err := os.WriteFile(f, []byte(next), 0o600); err != nil {
			fmt.Fprintf(stderr, "error: %s: %v\n", f, err)
			failed = true
			continue
		}
		changed++
		fmt.Fprintf(stdout, "fixed: %s\n", f)
	}
	switch {
	case changed > 0:
		fmt.Fprintf(stdout, "\n%d file(s) rewritten.\n", changed)
	case !failed:
		// Stay silent about "nothing to fix" when a failure already explains
		// the non-zero exit, so the stdout summary never contradicts stderr.
		// [Ja] 失敗で非ゼロ終了する場合は「修正なし」を出さない。stdout の要約が
		// stderr の内容と矛盾しないようにするため。
		fmt.Fprintln(stdout, "Nothing to fix.")
	}
	if failed {
		return 1
	}
	return 0
}

// resolveScope decides which files (and which lines within them) to check.
// A nil line set for a file means the whole file is in scope.
//
// [Ja] resolveScope はどのファイルの (そしてその中のどの行を) 検査するかを決める。
// あるファイルの行集合が nil ならファイル全体がスコープ内を意味する。
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

// breakProse splits a single prose line at sentence-final "。" that sit at top
// level — outside parentheses, brackets, quotes and inline code — and returns
// the line with "\n" inserted at each such break. A "。" is not a break point
// when it is the last non-space content, or when the next non-space rune is a
// closing bracket/quote.
//
// [Ja] breakProse は地の文の 1 行を、トップレベル (括弧・角括弧・鉤括弧・インライン
// コードの外) にある文末の「。」で分割し、各区切りに "\n" を挿入した行を返す。
// 「。」の後ろが空白のみ、または次の非空白文字が閉じ括弧・閉じ鉤括弧のときは
// 区切らない。
func breakProse(line string) string {
	var out []string
	var buf strings.Builder
	depth := 0
	inCode := false
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		buf.WriteRune(c)
		if c == '`' {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		switch {
		case isOpen(c):
			depth++
		case isClose(c):
			if depth > 0 {
				depth--
			}
		case c == '。' && depth == 0:
			rest := string(runes[i+1:])
			if strings.TrimSpace(rest) == "" {
				continue
			}
			trimmed := strings.TrimLeftFunc(rest, unicode.IsSpace)
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

// eachProseLine walks the document with block state and calls onProse(lineNo,
// line) for each rendered prose line, skipping code fences, HTML comments,
// lists, headings, tables and blockquotes.
//
// [Ja] eachProseLine はブロック状態を追いながら文書を走査し、コードフェンス・
// HTML コメント・リスト・見出し・表・引用を除いた地の文の行ごとに onProse(lineNo,
// line) を呼ぶ。
func eachProseLine(text string, onProse func(lineNo int, line string)) {
	lines := strings.Split(text, "\n")
	inFence := false
	inComment := false
	for i, line := range lines {
		switch {
		case reFence.MatchString(line):
			inFence = !inFence
		case inFence:
			// inside a fenced code block.
			// [Ja] フェンス内のコードブロック。
		case inComment:
			if strings.Contains(line, "-->") {
				inComment = false
			}
		case strings.Contains(line, "<!--") && !strings.Contains(line, "-->"):
			inComment = true
		case strings.Contains(line, "<!--"):
			// single-line HTML comment.
			// [Ja] 1 行で閉じる HTML コメント。
		case strings.TrimSpace(line) == "":
		case reHeading.MatchString(line):
		case reList.MatchString(line):
		case reTable.MatchString(line):
		case reBlockquote.MatchString(line):
		default:
			onProse(i+1, line)
		}
	}
}

// rewrite returns text with each prose line replaced by its broken form.
// [Ja] rewrite は各地の文の行を改行済みの形に置き換えた text を返す。
func rewrite(text string) string {
	lines := strings.Split(text, "\n")
	replaced := map[int]string{}
	eachProseLine(text, func(lineNo int, line string) {
		replaced[lineNo] = breakProse(line)
	})
	for i := range lines {
		if r, ok := replaced[i+1]; ok {
			lines[i] = r
		}
	}
	return strings.Join(lines, "\n")
}

// violations returns the prose lines whose broken form differs from the input.
// [Ja] violations は改行後の形が入力と異なる地の文の行を返す。
func violations(text string) []proseHit {
	var hits []proseHit
	eachProseLine(text, func(lineNo int, line string) {
		if breakProse(line) != line {
			hits = append(hits, proseHit{lineNo: lineNo, line: line})
		}
	})
	return hits
}

// changedLines maps each changed .md file (relative to the working directory,
// as git reports it) to the set of new-side line numbers changed since base
// (HEAD when base is empty). A nil set means the whole file is in scope:
// untracked files, or files whose diff cannot be computed. Git failures are
// swallowed so a non-repo or detached state yields an empty scope rather than
// an error.
//
// [Ja] changedLines は変更された各 .md ファイル (git が報告するパス。作業
// ディレクトリ基準) を、base (空なら HEAD) 以降に変更された新側の行番号集合へ
// 対応づける。集合が nil ならファイル全体がスコープ内 (未追跡ファイルや差分を
// 計算できないファイル)。git の失敗は握りつぶし、リポジトリ外などではエラーに
// せず空スコープにする。
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
	// Untracked .md files are checked in full. [Ja] 未追跡の .md は全行を対象。
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
			// No diff vs range (e.g. untracked): the whole file is in scope.
			// [Ja] 差分が取れない (未追跡など) ファイルは全行を対象にする。
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

// listAllMarkdown returns every .md file under root, skipping node_modules,
// .git and vendor directories.
//
// [Ja] listAllMarkdown は root 配下の全 .md ファイルを返す。node_modules・.git・
// vendor ディレクトリはスキップする。
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

// gitOutput runs a git command and returns its stdout.
// [Ja] gitOutput は git コマンドを実行し標準出力を返す。
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

// nonEmptyLines splits s on newlines and drops empty entries (matching the
// JavaScript original's split("\n").filter(Boolean)).
//
// [Ja] nonEmptyLines は s を改行で分割し空の要素を落とす (JavaScript 版の
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

// isOpen reports whether r is an opening bracket or quote (half- or full-width).
// [Ja] isOpen は r が開き括弧・開き鉤括弧 (半角・全角) かを返す。
func isOpen(r rune) bool { return strings.ContainsRune("(（[「『【〔《", r) }

// isClose reports whether r is a closing bracket or quote (half- or full-width).
// [Ja] isClose は r が閉じ括弧・閉じ鉤括弧 (半角・全角) かを返す。
func isClose(r rune) bool { return strings.ContainsRune(")）]」』】〕》", r) }

// firstRune returns the first rune of s, or 0 when s is empty.
// [Ja] firstRune は s の先頭ルーンを返す。s が空なら 0。
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

// truncate shortens s to at most n runes for display.
// [Ja] truncate は表示用に s を最大 n ルーンへ短縮する。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
