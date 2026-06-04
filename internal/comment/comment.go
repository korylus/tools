// Package comment implements the comment subcommand of koryluslint, which
// checks that bilingual code comments use the [Ja] marker correctly. The
// marker introduces the Japanese translation block, which must sit below a
// corresponding English block and appear at most once per comment group.
// When the English block spans multiple lines, one blank comment line must
// separate it from the marker; when it is a single line, no blank line is
// allowed. The check targets the recurring *misuse* of the marker rather than
// enforcing full bilingual coverage, which keeps false positives near zero;
// for the same reason the blank-line check skips comment groups that contain
// lines it cannot classify as English or Japanese (separators, code examples,
// URL-only lines, and the like).
//
// Two modes:
//
//	comment [paths...]              checks the [Ja]-on-non-Japanese rule across
//	                                the whole tree (default: ".").
//	comment -base=<ref> [paths...]  also checks the marker-placement and
//	                                blank-line rules, limited to lines added
//	                                since <ref>.
//
// .go files are parsed via go/parser so that "//" inside string literals is
// never mistaken for a comment; .templ files (not valid Go) are scanned by line.
//
// [Ja] comment パッケージは koryluslint の comment サブコマンドを実装し、
// コードコメントの英日併記で `[Ja]` マーカーが正しく使われているかをチェックする。
// `[Ja]` は日本語訳ブロックの冒頭を示すマーカーで、対応する英語ブロックの下に置き、
// 1 コメント群に 1 つだけ付ける。英語ブロックが複数行のときはマーカーとの間に
// 空行 (コメント記号のみの行) を 1 行置き、1 行のときは空行を置かない。
// 全併記の強制ではなく、再発している「マーカーの誤用」のみを対象にすることで
// 誤検出をほぼゼロに保つ。同じ理由で、空行の検査は英語とも日本語とも判定できない行
// (区切り線・コード例・URL のみの行など) を含むコメント群をスキップする。
//
// モードは 2 つ:
//
//	comment [paths...]              「[Ja] が日本語を含まない行に付く」誤用を
//	                                ツリー全体で検査する (既定は ".")。
//	comment -base=<ref> [paths...]  マーカー配置・空行の規則も検査する。<ref>
//	                                以降に追加された行に限定する。
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

	"github.com/korylus/tools/internal/cli"
)

var (
	// reJapanese matches any Hiragana, Katakana, or Han (kanji) rune.
	// [Ja] reJapanese はひらがな・カタカナ・漢字のいずれかにマッチする。
	reJapanese = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)
	// reLatin matches an ASCII Latin letter.
	// [Ja] reLatin は ASCII のラテン文字にマッチする。
	reLatin = regexp.MustCompile(`[A-Za-z]`)
	// reGenerated matches the standard "generated; do not edit" header.
	// [Ja] reGenerated は「生成物・編集禁止」の定型ヘッダーにマッチする。
	reGenerated = regexp.MustCompile(`^//\s*Code generated .* DO NOT EDIT\.$`)
	// reURLOnly matches a line whose whole content is a single URL.
	// [Ja] reURLOnly は本文全体が 1 つの URL である行にマッチする。
	reURLOnly = regexp.MustCompile(`^https?://\S+$`)
)

// commentLine is a single physical line of a comment with its 1-based line number.
// [Ja] commentLine はコメントの 1 物理行と、その 1 始まりの行番号。
type commentLine struct {
	line int
	text string
}

// finding is one detected violation.
// [Ja] finding は検出した違反 1 件。
type finding struct {
	file string
	line int
	cond int
	msg  string
}

// lineKind classifies a non-marker comment line for the blank-line check
// (condition 4).
//
// [Ja] lineKind は空行検査 (条件 4) のために非マーカー行を分類した種別。
type lineKind int

const (
	kindNone     lineKind = iota // no line seen yet. [Ja] まだ行が無い
	kindBlank                    // comment leader only, empty content. [Ja] コメント記号のみで本文が空
	kindEnglish                  // English text. [Ja] 英文
	kindJapanese                 // Japanese text. [Ja] 日本語文
	kindOther                    // neither English nor Japanese (separator, code, URL). [Ja] 英文とも日本語とも判定できない
	kindMarker                   // a [Ja] marker line. [Ja] マーカー行
)

const (
	msgCond1  = "[Ja] marker on a line with no Japanese text; it must lead the Japanese translation, not the English block / [Ja] マーカーが日本語を含まない行に付いている"
	msgCond2  = "[Ja] marker has no English block above it; write the English block first, then [Ja] / [Ja] の上に対応する英語ブロックが無い"
	msgCond3  = "[Ja] marker appears more than once in one comment block; mark only the first line of the Japanese block / [Ja] マーカーが 1 コメント群に複数ある"
	msgCond4a = "[Ja] marker after a multi-line English block needs one blank comment line right above it / [Ja] 英語ブロックが複数行のときはマーカーの直前に空行が必要"
	msgCond4b = "[Ja] marker after a one-line English comment must not have a blank line above it / [Ja] 英語ブロックが 1 行のときはマーカーの直前に空行を入れない"
)

// Run is the entry point of the comment subcommand. args is what remains after
// the subcommand name, and the return value is the process exit code.
// [Ja] Run は comment サブコマンドのエントリポイント。args はサブコマンド名を除いた
// 残りの引数で、戻り値はプロセスの終了コード。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := cli.RegisterCommon(fs)
	if err := fs.Parse(args); err != nil {
		// A -h/--help request is a success, not a usage error.
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
	fmt.Fprintf(stderr, "\nkoryluslint comment: %d bilingual [Ja] marker violation(s)\n", len(findings))
	return 1
}

// collectFindings walks the roots and returns the violations to report. In full
// mode only condition 1 (marker on a non-Japanese line) is reported, anywhere in
// the tree. In diff mode all conditions are reported, but only for lines added
// since base. Recoverable problems (an uncomputable diff, a single unparsable
// file) are written to stderr and skipped rather than failing the run.
//
// [Ja] collectFindings は roots を走査し、報告すべき違反を返す。全体モードでは条件 1
// (日本語を含まない行のマーカー) のみをツリー全体で報告する。差分モードでは全条件を
// 報告するが、base 以降に追加された行に限定する。回復可能な問題 (差分を計算できない、
// 個別ファイルの解析失敗) は stderr に出してスキップし、実行を失敗させない。
func collectFindings(roots []string, base string, stderr io.Writer) ([]finding, error) {
	diffMode := base != ""

	var added map[string]map[int]bool
	if diffMode {
		var err error
		added, err = addedLines(base)
		if err != nil {
			// Be lenient: if the diff cannot be computed, skip the diff-scoped
			// checks rather than failing the build.
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
					} else if f.cond != 1 {
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

// checkGroup evaluates one comment group against the [Ja] marker rules.
// Only lines whose comment content *begins* with the [Ja] marker count as
// marker lines; a mid-sentence mention of "[Ja]" in English prose (such as this
// tool's own docs) is ignored. At most one finding per marker line is produced
// (on the same line condition 1 supersedes condition 2, which supersedes
// condition 4); condition 3 is reported independently. Condition 4 (the blank
// line between the English block and the marker) is evaluated only while every
// line above the marker classifies as English, Japanese, or blank; one
// unclassifiable line (separator, code example, URL-only) disables it for the
// rest of the group, and so does a second marker (condition 3 already reports
// the broken structure, where any blank-line judgement would be a guess).
//
// [Ja] checkGroup は 1 コメント群を [Ja] マーカー規則で評価する。
// コメント本文が [Ja] マーカーで「始まる」行だけをマーカー行とみなし、英語の地の文中で
// "[Ja]" に言及しているだけの行 (本ツール自身の説明など) は無視する。1 マーカー行あたり
// 最大 1 件 (同じ行では条件 1 が条件 2 に、条件 2 が条件 4 に優先する)。条件 3 は独立に
// 報告する。条件 4 (英語ブロックとマーカーの間の空行) は、マーカーより上の全行が
// 英文・日本語・空行のいずれかに分類できる間だけ評価し、分類できない行 (区切り線・
// コード例・URL のみの行) が 1 つでもあれば群の残りでは無効にする。2 つ目以降の
// マーカーでも同様に無効にする (壊れた構造は条件 3 で報告済みで、そこへの空行判定は
// 当て推量になるため)。
func checkGroup(lines []commentLine) []finding {
	var fs []finding
	markerCount := 0
	englishSeenAbove := false

	// State for condition 4: the count of English lines above the marker, the
	// kind of the previous line, and whether an unclassifiable line was seen.
	//
	// [Ja] 条件 4 のための状態: マーカーより上の英文行の数、直前行の種別、
	// 分類できない行が出現したかどうか。
	englishAbove := 0
	prevKind := kindNone
	unclassifiable := false

	for _, cl := range lines {
		if !markerAtStart(cl.text) {
			if isEnglishText(cl.text) {
				englishSeenAbove = true
			}
			kind := classifyLine(cl.text)
			switch kind {
			case kindEnglish:
				englishAbove++
			case kindOther:
				unclassifiable = true
			}
			prevKind = kind
			continue
		}

		markerCount++
		if markerCount > 1 {
			fs = append(fs, finding{line: cl.line, cond: 3, msg: msgCond3})
		}

		switch {
		case !hasJapanese(cl.text):
			// Condition 1: the marker leads an English (or marker-only) line.
			// [Ja] 条件 1: マーカーが英語 (またはマーカーのみ) の行を先導している。
			fs = append(fs, finding{line: cl.line, cond: 1, msg: msgCond1})
		case !englishSeenAbove:
			// Condition 2: a marker line still needs an English block above it.
			// [Ja] 条件 2: マーカー行でも、その上に英語ブロックが必要。
			fs = append(fs, finding{line: cl.line, cond: 2, msg: msgCond2})
		case unclassifiable || markerCount > 1:
			// Skip condition 4: a line above defies classification, or the
			// group has more than one marker (already reported as condition
			// 3), so the blank-line judgement would be a guess. Prefer a
			// false negative.
			//
			// [Ja] 条件 4 をスキップ: 上の行に分類できない行があるか、群に
			// マーカーが複数あり (条件 3 で報告済み)、空行の判定が当て推量に
			// なるため。偽陰性側に倒す。
		case prevKind == kindEnglish && englishAbove >= 2:
			// Condition 4 (a): a multi-line English block runs straight into
			// the marker without the separating blank line.
			//
			// [Ja] 条件 4 (a): 複数行の英語ブロックが空行を挟まずマーカーに
			// 連続している。
			fs = append(fs, finding{line: cl.line, cond: 4, msg: msgCond4a})
		case prevKind == kindBlank && englishAbove == 1:
			// Condition 4 (b): a one-line English comment is separated from
			// the marker by a blank line it must not have.
			//
			// [Ja] 条件 4 (b): 1 行の英語コメントとマーカーの間に不要な空行が
			// 入っている。
			fs = append(fs, finding{line: cl.line, cond: 4, msg: msgCond4b})
		}
		prevKind = kindMarker
	}
	return fs
}

// classifyLine classifies one non-marker comment line for the blank-line
// check (condition 4). The comment leader is stripped first — "//" or "/*"
// once, then at most one "*" continuation, so that "/**" classifies as blank
// while a row of stars ("//****") keeps its content and classifies as
// kindOther. Content indented with a tab or with two or more spaces after the
// leader is a code example (prose keeps a single space), and a line whose
// whole content is a URL is not prose, so both classify as kindOther.
//
// [Ja] classifyLine は空行検査 (条件 4) のために非マーカー行を 1 行分類する。
// 先にコメントリーダーを取り除く。"//" または "/*" を 1 回、続けて継続記号の
// "*" を高々 1 つだけ除去することで、"/**" は空行扱いにしつつ、星の並び
// ("//****") は本文が残って kindOther に分類される。リーダー直後がタブまたは
// スペース 2 つ以上で字下げされた本文はコード例 (地の文はスペース 1 つ)、
// 本文全体が URL の行は地の文ではないため、いずれも kindOther に分類する。
func classifyLine(text string) lineKind {
	s := strings.TrimSpace(text)
	for _, leader := range []string{"//", "/*"} {
		if strings.HasPrefix(s, leader) {
			s = strings.TrimPrefix(s, leader)
			break
		}
	}
	s = strings.TrimPrefix(s, "*") // at most one "*" continuation. [Ja] 継続記号の "*" は高々 1 つ
	if strings.HasPrefix(s, "\t") || strings.HasPrefix(s, "  ") {
		return kindOther
	}
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return kindBlank
	case reURLOnly.MatchString(s):
		return kindOther
	case hasJapanese(s):
		return kindJapanese
	case isEnglishText(s):
		return kindEnglish
	default:
		return kindOther
	}
}

// markerAtStart reports whether the comment content begins with the [Ja] marker,
// after stripping the comment leader ("//", "/*", or a "*" continuation) and
// surrounding whitespace. This distinguishes a marker use from a prose mention.
//
// [Ja] markerAtStart は、コメントリーダー ("//"・"/*"・継続行の "*") と前後の空白を
// 取り除いた本文が [Ja] マーカーで始まるかを返す。マーカーとしての使用と地の文中の
// 言及を区別する。
func markerAtStart(text string) bool {
	s := strings.TrimSpace(text)
	for _, leader := range []string{"//", "/*", "*"} {
		if strings.HasPrefix(s, leader) {
			s = strings.TrimSpace(strings.TrimPrefix(s, leader))
			break
		}
	}
	return strings.HasPrefix(s, "[Ja]")
}

// hasJapanese reports whether s contains any kana or kanji.
// [Ja] hasJapanese は s に仮名・漢字が含まれるかを返す。
func hasJapanese(s string) bool { return reJapanese.MatchString(s) }

// isEnglishText reports whether s looks like English: it has Latin letters and
// no Japanese. A Japanese sentence containing a Latin acronym (e.g. "CSRF") is
// therefore not treated as English.
//
// [Ja] isEnglishText は s が英語に見えるか (ラテン文字を含み日本語を含まない) を返す。
// "CSRF" のようなラテン略語を含む日本語文は英語扱いにしない。
func isEnglishText(s string) bool {
	return reLatin.MatchString(s) && !reJapanese.MatchString(s)
}

// commentGroups extracts comment groups from a file. Generated files yield none.
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
