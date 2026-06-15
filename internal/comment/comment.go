// Package comment implements the comment subcommand of koryluslint, which checks
// that bilingual code comments follow the English-first / Japanese-marker format.
// A bilingual comment is an English block (unmarked, written first; a doc comment
// on an exported symbol begins with the symbol name per Go convention) followed
// by a Japanese block led by the Japanese marker at the start of its line. There
// is no English marker; an obsolete English marker is reported so it can be
// removed. The inline (end-of-line) form is not allowed. The check targets
// recurring misuses rather than enforcing full bilingual coverage, which keeps
// false positives near zero.
//
// Two modes:
//
//	comment [paths...]              checks the near-zero-false-positive rules
//	                                tree-wide (default: "."): a Japanese block
//	                                that is not Japanese, an English block whose
//	                                line nearest the marker is Japanese, an inline
//	                                Japanese marker with Japanese before it, and an
//	                                obsolete English marker.
//	comment -base=<ref> [paths...]  also checks the English-required, inline-ban,
//	                                duplicate, and required-blank-separator rules,
//	                                limited to lines added since <ref>.
//
// .go files are parsed via go/parser so that "//" inside string literals is
// never mistaken for a comment; .templ files (not valid Go) are scanned by line.
//
// [Ja] comment パッケージは koryluslint の comment サブコマンドを実装し、コードコメントの
// 英日併記が「英語先頭 / 日本語マーカー」形式に従っているかをチェックする。併記コメントは
// 英語ブロック (無マーカーで先に書く。exported シンボルの doc コメントは Go 慣習どおり
// シンボル名で始める) の後に、行頭の日本語マーカーで始まる日本語ブロックを置く。英語マーカー
// は無く、廃止された英語マーカーは見つかれば除去できるよう報告する。インライン (行末) 形式は
// 使わない。全併記の強制ではなく再発する誤用を対象にし、誤検出をほぼゼロに保つ。
//
// モードは 2 つ:
//
//	comment [paths...]              誤検出がほぼ無い規則をツリー全体で検査する (既定は
//	                                ".")。日本語でない日本語ブロック、マーカーに最も近い行
//	                                が日本語の英語ブロック、前に日本語があるインライン日本語
//	                                マーカー、廃止された英語マーカー。
//	comment -base=<ref> [paths...]  英語必須・インライン禁止・重複・ブロック間空行 (必須) の
//	                                規則も検査する。<ref> 以降に追加された行に限定する。
//
// .go は go/parser で解析し、文字列リテラル中の "//" をコメントと誤認しない。
// .templ (Go として不正) は行単位で走査する。
package comment

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/korylus/tools/internal/cli"
)

var (
	// reJapanese matches any Hiragana, Katakana, or Han (kanji) rune.
	//
	// [Ja] reJapanese はひらがな・カタカナ・漢字のいずれかにマッチする。
	reJapanese = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)
	// reGenerated matches the standard "generated; do not edit" header.
	//
	// [Ja] reGenerated は「生成物・編集禁止」の定型ヘッダーにマッチする。
	reGenerated = regexp.MustCompile(`^//\s*Code generated .* DO NOT EDIT\.$`)
)

// commentLine is a single physical line of a comment with its 1-based line number.
//
// [Ja] commentLine はコメントの 1 物理行と、その 1 始まりの行番号。
type commentLine struct {
	line int
	text string
}

// finding is one detected violation.
//
// [Ja] finding は検出した違反 1 件。
type finding struct {
	file string
	line int
	cond int
	msg  string
}

const (
	msgCond1 = "the lines under [Ja] are not Japanese; a [Ja] block must be the Japanese translation / [Ja] [Ja] ブロックが日本語になっていない"
	msgCond2 = "the English block (the lines before [Ja]) contains Japanese; keep it English / [Ja] 英語ブロック ([Ja] より前) に日本語が入っている"
	msgCond3 = "inline [Ja] marker with Japanese before it; markers must lead their own line / [Ja] インラインの [Ja] マーカーの前に日本語がある (マーカーは行頭に置く)"
	msgCond4 = "[Ja] block has no English block before it; lead with an English block, then [Ja] / [Ja] [Ja] ブロックの前に英語ブロックが無い (英語先頭→[Ja])"
	msgCond5 = "inline (end-of-line) bilingual comment is not allowed; put the English block and [Ja] on their own lines above the code / [Ja] インライン (行末) の併記コメントは不可 (英語ブロックと [Ja] を行頭に置く)"
	msgCond6 = "duplicate [Ja] marker in one comment group; use a single [Ja] / [Ja] [Ja] マーカーが 1 群に重複 ([Ja] は 1 つ)"
	msgCond7 = "obsolete [En] marker; the English block is unmarked now (English first, then [Ja]) / [Ja] [En] マーカーは廃止 (英語ブロックは無マーカー、英語先頭→[Ja])"
	msgCond8 = "no blank line between the English and Japanese blocks; put one blank comment line before [Ja] / [Ja] 英語ブロックと日本語ブロックの間に空行が無い ([Ja] の前に空行を 1 行入れる)"
)

// Run is the entry point of the comment subcommand. args is what remains after
// the subcommand name, and the return value is the process exit code.
//
// [Ja] Run は comment サブコマンドのエントリポイント。args はサブコマンド名を除いた
// 残りの引数で、戻り値はプロセスの終了コード。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := cli.RegisterCommon(fs)
	if err := fs.Parse(args); err != nil {
		// A -h/--help request is a success, not a usage error.
		//
		// [Ja] -h/--help の要求はエラーではなく成功扱いにする。
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	roots := fs.Args()
	if len(roots) == 0 {
		roots = []string{"."}
	}

	findings, err := collectFindings(roots, opts.Base, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "koryluslint comment:", err)
		return 2
	}
	if len(findings) == 0 {
		return 0
	}
	for _, f := range findings {
		fmt.Fprintf(stdout, "%s:%d: %s\n", f.file, f.line, f.msg)
	}
	fmt.Fprintf(stderr, "\nkoryluslint comment: %d bilingual marker violation(s)\n", len(findings))
	return 1
}

// collectFindings walks the roots and returns the violations to report. In full
// mode only the near-zero-false-positive conditions (1, 2, 3, 7) are reported,
// anywhere in the tree: a Japanese block that is not Japanese, an English block
// that contains Japanese, an inline Japanese marker with Japanese before it, and
// an obsolete English marker. In diff mode every condition is reported, but only
// for lines added since base. Recoverable problems (an uncomputable diff, a
// single unparsable file) are written to stderr and skipped rather than failing
// the run.
//
// [Ja] collectFindings は roots を走査し、報告すべき違反を返す。全体モードでは誤検出が
// ほぼ無い条件 (1, 2, 3, 7) のみをツリー全体で報告する。日本語でない日本語ブロック、日本語
// が入った英語ブロック、前に日本語があるインライン日本語マーカー、廃止された英語マーカー。
// 差分モードでは全条件を報告するが、base 以降に追加された行に限定する。回復可能な問題 (差分
// を計算できない、個別ファイルの解析失敗) は stderr に出してスキップし、実行を失敗させない。
func collectFindings(roots []string, base string, stderr io.Writer) ([]finding, error) {
	diffMode := base != ""

	var added map[string]map[int]bool
	if diffMode {
		var err error
		added, err = addedLines(base)
		if err != nil {
			// Be lenient: if the diff cannot be computed, skip the diff-scoped
			// checks rather than failing the build.
			//
			// [Ja] 差分を計算できない場合はビルドを失敗させず、差分限定の検査を
			// スキップする。
			fmt.Fprintf(stderr, "koryluslint comment: skipping diff checks: %v\n", err)
			return nil, nil
		}
	}

	var all []finding
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".go" && ext != ".templ" {
				return nil
			}
			if strings.HasSuffix(path, "_templ.go") {
				return nil
			}

			groups, gerr := commentGroups(path, ext)
			if gerr != nil {
				fmt.Fprintf(stderr, "koryluslint comment: %s: %v\n", path, gerr)
				return nil
			}
			for _, g := range groups {
				for _, f := range checkGroup(g) {
					f.file = path
					if diffMode {
						abs, aerr := filepath.Abs(path)
						if aerr != nil || added[abs] == nil || !added[abs][f.line] {
							continue
						}
					} else if !isFullModeCond(f.cond) {
						continue
					}
					all = append(all, f)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].file != all[j].file {
			return all[i].file < all[j].file
		}
		if all[i].line != all[j].line {
			return all[i].line < all[j].line
		}
		return all[i].cond < all[j].cond
	})
	return all, nil
}

// isFullModeCond reports whether a condition is checked tree-wide (full mode).
// These are the near-zero-false-positive conditions; the rest are diff-only.
//
// [Ja] isFullModeCond は条件がツリー全体 (全体モード) で検査されるかを返す。これらは誤検出
// がほぼ無い条件で、残りは差分モード限定。
func isFullModeCond(cond int) bool {
	switch cond {
	case 1, 2, 3, 7:
		return true
	default:
		return false
	}
}

// checkGroup evaluates one comment group against the English-first / [Ja] marker
// rules. A line counts as a marker line only when its content begins with a
// marker; a mid-sentence mention in prose is ignored. The Japanese block must be
// Japanese (condition 1) and must have an English block above it (condition 4),
// and a [Ja] marker must be unique (condition 6). In a bilingual group the line
// nearest above the first marker (skipping blanks) must not be Japanese
// (condition 2). On a non-marker line an inline (end-of-line) marker is detected
// by a marker that follows a sentence end and reported as condition 3 (Japanese
// before an inline Japanese marker) or, when the English side is fine, condition
// 5 (the inline form is banned). The English marker [En] is obsolete and is
// reported as condition 7 wherever it leads a line. A missing blank line between
// the two blocks is reported as condition 8.
//
// [Ja] checkGroup は 1 コメント群を「英語先頭 / [Ja] マーカー」規則で評価する。本文がマー
// カーで「始まる」行だけをマーカー行とみなし、地の文中の言及は無視する。日本語ブロックは
// 日本語 (条件 1) で、その上に英語ブロックが必要 (条件 4)、[Ja] マーカーは一意 (条件 6)。
// 併記群では最初のマーカーの直上 (空行は飛ばす) の行が日本語であってはならない (条件 2)。非
// マーカー行では、文末に続くマーカーをインライン (行末) マーカーとして検出し、条件 3 (イン
// ライン日本語マーカーの前に日本語) か、英語側が問題なければ条件 5 (インライン形式は禁止)
// として報告する。英語マーカー [En] は廃止で、行頭にあれば条件 7 として報告する。ブロック間
// の空行が無いことは条件 8 として報告する。
func checkGroup(lines []commentLine) []finding {
	var fs []finding

	// firstJa is the index of the first line-leading [Ja] marker, or -1.
	//
	// [Ja] firstJa は行頭 [Ja] マーカーの最初の位置 (無ければ -1)。
	firstJa := -1
	for i, cl := range lines {
		if markerKind(cl.text) == "ja" {
			firstJa = i
			break
		}
	}

	jaCount := 0
	englishBefore := false
	for i, cl := range lines {
		beforeJa := firstJa < 0 || i < firstJa
		switch markerKind(cl.text) {
		case "en":
			// Condition 7: the English marker is obsolete; the English block is now unmarked.
			//
			// [Ja] 条件 7: 英語マーカーは廃止。英語ブロックは無マーカーにする。
			fs = append(fs, finding{line: cl.line, cond: 7, msg: msgCond7})
			if beforeJa {
				englishBefore = true
			}
		case "ja":
			jaCount++
			// Condition 1: the Japanese block must be Japanese.
			//
			// [Ja] 条件 1: 日本語ブロックは日本語でなければならない。
			if !hasJapanese(commentBody(cl.text)) {
				fs = append(fs, finding{line: cl.line, cond: 1, msg: msgCond1})
			}
			// Condition 4: the first Japanese block needs an English block above it.
			//
			// [Ja] 条件 4: 最初の日本語ブロックの上に英語ブロックが必要。
			if i == firstJa && !englishBefore {
				fs = append(fs, finding{line: cl.line, cond: 4, msg: msgCond4})
			}
			// Condition 8: there must be a blank line between the English and
			// Japanese blocks; the [Ja] block must not directly follow an English line.
			//
			// [Ja] 条件 8: 英語ブロックと日本語ブロックの間に空行を 1 行入れる ([Ja] が英語行の
			// 直後に続いてはならない)。
			if i == firstJa && englishBefore && i > 0 && commentBody(lines[i-1].text) != "" {
				fs = append(fs, finding{line: cl.line, cond: 8, msg: msgCond8})
			}
			// Condition 6: the Japanese marker must be unique.
			//
			// [Ja] 条件 6: 日本語マーカーは一意でなければならない。
			if jaCount > 1 {
				fs = append(fs, finding{line: cl.line, cond: 6, msg: msgCond6})
			}
		case "":
			// A non-marker line: detect an inline (end-of-line) marker.
			//
			// [Ja] 非マーカー行: インライン (行末) マーカーを検出する。
			switch {
			case inlineMarkerMisuse(cl.text):
				fs = append(fs, finding{line: cl.line, cond: 3, msg: msgCond3})
			case inlineMarkerPresent(cl.text):
				fs = append(fs, finding{line: cl.line, cond: 5, msg: msgCond5})
			}
			if beforeJa && commentBody(cl.text) != "" {
				englishBefore = true
			}
		}
	}

	// Condition 2: in a bilingual group the English block sits before the first
	// Japanese marker; the line nearest that marker (skipping blank lines) must
	// not be Japanese. Checking only the nearest line avoids flagging a separate
	// Japanese label comment that a missing blank line merged into the group above
	// the English block.
	//
	// [Ja] 条件 2: 併記群では英語ブロックは最初の日本語マーカーより前にあり、そのマーカーに
	// 最も近い行 (空行は飛ばす) が日本語であってはならない。最も近い行だけを見ることで、空行
	// 漏れで群に取り込まれた別の日本語ラベルコメントを誤検出しない。
	if firstJa > 0 {
		for i := firstJa - 1; i >= 0; i-- {
			body := commentBody(lines[i].text)
			if body == "" {
				continue
			}
			if hasJapanese(body) {
				fs = append(fs, finding{line: lines[i].line, cond: 2, msg: msgCond2})
			}
			break
		}
	}

	sort.Slice(fs, func(i, j int) bool {
		if fs[i].line != fs[j].line {
			return fs[i].line < fs[j].line
		}
		return fs[i].cond < fs[j].cond
	})
	return fs
}

// commentBody strips the comment leader ("//", "/*", or a "*" continuation) and
// the surrounding whitespace, returning the bare comment content.
//
// [Ja] commentBody はコメントリーダー ("//"・"/*"・継続行の "*") と前後の空白を
// 取り除き、コメント本文だけを返す。
func commentBody(text string) string {
	s := strings.TrimSpace(text)
	for _, leader := range []string{"//", "/*", "*"} {
		if strings.HasPrefix(s, leader) {
			s = strings.TrimSpace(strings.TrimPrefix(s, leader))
			break
		}
	}
	return s
}

// markerKind returns "en" if the comment content begins with the obsolete
// English marker, "ja" if it begins with the Japanese marker, or "" otherwise. A
// marker only counts at the start of the content, which separates a marker use
// from a mid-sentence mention in prose.
//
// [Ja] markerKind はコメント本文が廃止された英語マーカーで始まれば "en"、日本語マーカーで
// 始まれば "ja"、それ以外は "" を返す。マーカーは本文の先頭にあるときだけ数え、マーカーと
// しての使用と地の文中の言及を区別する。
func markerKind(text string) string {
	body := commentBody(text)
	switch {
	case strings.HasPrefix(body, "[En]"):
		return "en"
	case strings.HasPrefix(body, "[Ja]"):
		return "ja"
	default:
		return ""
	}
}

// inlineMarkerMisuse reports whether a non-marker line carries an inline Japanese
// marker with Japanese in the block before it (a duplicated Japanese block, or a
// reversed Japanese-then-English pair). To keep false positives near zero against
// a prose mention, the token counts as an inline marker only when the text right
// before it ends a sentence (a "." / "。" and the like); a token preceded by a
// word or particle is a prose mention and is ignored.
//
// [Ja] inlineMarkerMisuse は、非マーカー行が、前のブロックに日本語があるインライン日本語
// マーカー (日本語ブロックの重複、または日本語→英語の逆順ペア) を持つかを返す。地の文中の
// 言及への誤検出をほぼゼロに保つよう、トークンの直前が文末 ("." / "。" など) のときだけ
// インラインマーカーとみなす。直前が単語や助詞のトークンは地の文の言及として無視する。
func inlineMarkerMisuse(text string) bool {
	if markerKind(text) != "" {
		return false
	}
	body := commentBody(text)
	idx := strings.Index(body, "[Ja]")
	if idx < 0 {
		return false
	}
	left := strings.TrimRight(body[:idx], " \t")
	if !hasJapanese(left) {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(left)
	return isSentenceEnd(r)
}

// inlineMarkerPresent reports whether a non-marker line carries an inline marker
// (English or Japanese), regardless of the language before it. Like
// inlineMarkerMisuse it counts a token as a marker only when a sentence end
// precedes it, so a prose mention is ignored.
//
// [Ja] inlineMarkerPresent は、非マーカー行が、前の言語によらずインラインマーカー (英語
// または日本語) を持つかを返す。inlineMarkerMisuse と同様、トークンの直前が文末のときだけ
// マーカーとみなすため、地の文中の言及は無視する。
func inlineMarkerPresent(text string) bool {
	if markerKind(text) != "" {
		return false
	}
	body := commentBody(text)
	for _, marker := range []string{"[En]", "[Ja]"} {
		idx := strings.Index(body, marker)
		if idx < 0 {
			continue
		}
		left := strings.TrimRight(body[:idx], " \t")
		if left == "" {
			continue
		}
		r, _ := utf8.DecodeLastRuneInString(left)
		if isSentenceEnd(r) {
			return true
		}
	}
	return false
}

// isSentenceEnd reports whether r is a sentence-ending mark, ASCII or full-width.
//
// [Ja] isSentenceEnd は r が文末記号 (ASCII または全角) かどうかを返す。
func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', '。', '！', '？':
		return true
	default:
		return false
	}
}

// hasJapanese reports whether s contains any kana or kanji.
//
// [Ja] hasJapanese は s に仮名・漢字が含まれるかを返す。
func hasJapanese(s string) bool { return reJapanese.MatchString(s) }

// commentGroups extracts comment groups from a file. Generated files yield none.
//
// [Ja] commentGroups はファイルからコメント群を抽出する。生成物は空を返す。
func commentGroups(path, ext string) ([][]commentLine, error) {
	// path comes from walking the user-specified roots; reading it is the whole
	// point of a file linter, so gosec's file-inclusion warning (G304) does not
	// apply here.
	//
	// [Ja] path はユーザー指定の root を走査して得たもので、それを読むことこそが
	// ファイルリンタの目的。gosec のファイル混入警告 (G304) はここでは当てはまらない。
	src, err := os.ReadFile(path) //#nosec G304
	if err != nil {
		return nil, err
	}
	if isGenerated(src) {
		return nil, nil
	}
	if ext == ".templ" {
		return templCommentGroups(src), nil
	}
	return goCommentGroups(path, src)
}

// goCommentGroups uses go/parser so that "//" inside string literals is ignored.
//
// [Ja] goCommentGroups は go/parser を使い、文字列リテラル中の "//" を無視する。
func goCommentGroups(path string, src []byte) ([][]commentLine, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var groups [][]commentLine
	for _, cg := range f.Comments {
		var lines []commentLine
		for _, c := range cg.List {
			start := fset.Position(c.Pos()).Line
			for i, sub := range strings.Split(c.Text, "\n") {
				lines = append(lines, commentLine{line: start + i, text: sub})
			}
		}
		groups = append(groups, lines)
	}
	return groups, nil
}

// templCommentGroups groups maximal runs of full-line "//" comments.
//
// [Ja] templCommentGroups は行頭 "//" コメントの連続を 1 群にまとめる。
func templCommentGroups(src []byte) [][]commentLine {
	var groups [][]commentLine
	var cur []commentLine
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
	}
	for i, raw := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "//") {
			cur = append(cur, commentLine{line: i + 1, text: t})
			continue
		}
		flush()
	}
	flush()
	return groups
}

// isGenerated reports whether src carries the standard generated-file header.
//
// [Ja] isGenerated は src が定型の生成物ヘッダーを持つかを返す。
func isGenerated(src []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(src))
	for n := 0; sc.Scan() && n < 20; n++ {
		if reGenerated.MatchString(strings.TrimSpace(sc.Text())) {
			return true
		}
	}
	return false
}

// skipDir reports whether a directory should not be walked.
//
// [Ja] skipDir は走査しないディレクトリかどうかを返す。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "bin", "tmp", "static":
		return true
	default:
		return false
	}
}

// addedLines maps absolute file paths to the set of line numbers added since
// base, parsed from "git diff --unified=0 base...HEAD".
//
// [Ja] addedLines は "git diff --unified=0 base...HEAD" を解析し、base 以降に
// 追加された行番号の集合を絶対パスごとに返す。
func addedLines(base string) (map[string]map[int]bool, error) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root = strings.TrimSpace(root)

	diff, err := gitOutput("diff", "--unified=0", "--no-color", base+"...HEAD")
	if err != nil {
		return nil, err
	}

	reFile := regexp.MustCompile(`^\+\+\+ b/(.*)$`)
	reHunk := regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

	result := map[string]map[int]bool{}
	var curAbs string
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := reFile.FindStringSubmatch(line); m != nil {
			abs, aerr := filepath.Abs(filepath.Join(root, m[1]))
			if aerr != nil {
				curAbs = ""
				continue
			}
			curAbs = abs
			if result[curAbs] == nil {
				result[curAbs] = map[int]bool{}
			}
			continue
		}
		if m := reHunk.FindStringSubmatch(line); m != nil && curAbs != "" {
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			for n := 0; n < count; n++ {
				result[curAbs][start+n] = true
			}
		}
	}
	return result, sc.Err()
}

// gitOutput runs a git command and returns its stdout.
//
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
