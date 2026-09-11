// Package commentはkoryluslintのcommentサブコマンドを実装し、コードコメントが
// korylus-lang.md §3の日本語テキストスタイルに従っているかを検査する。
// ルール本体はinternal/styleが持ち、本パッケージはコメントの抽出・検査範囲の
// 決定・報告を担う。
//
// モードは2つ。
//
//	comment [paths...]              ツリー全体のコメントを検査する (既定は ".")。
//	comment -base=<ref> [paths...]  <ref> 以降に追加された行に検査を限定する。
//
// .goファイルはgo/parserで解析し、文字列リテラル中の "//" をコメントと誤認しない。
// .templファイル (Goとして不正) は行単位で走査する。
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
	"github.com/korylus/tools/internal/style"
)

// reGeneratedは「生成物・編集禁止」の定型ヘッダーにマッチする。
var reGenerated = regexp.MustCompile(`^//\s*Code generated .* DO NOT EDIT\.$`)

// commentLineはコメントの1物理行と、その1始まりの行番号。
type commentLine struct {
	line int
	text string
}

// findingは検出した違反1件。
type finding struct {
	file    string
	line    int
	section string
	msg     string
}

// Runはcommentサブコマンドのエントリポイント。argsはサブコマンド名を除いた
// 残りの引数で、戻り値はプロセスの終了コード。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := cli.RegisterCommon(fs)
	if err := fs.Parse(args); err != nil {
		// -h/--helpの要求はエラーではなく成功扱いにする。
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
	fmt.Fprintf(stderr, "\nkoryluslint comment: 日本語スタイル違反%d件\n", len(findings))
	return 1
}

// collectFindingsはrootsを走査し、報告すべき違反を返す。
// 既定ではツリー全体のコメントを検査し、差分モードではbase以降に追加された行に
// 限定する。
// 回復可能な問題 (差分を計算できない、個別ファイルの解析失敗) はstderrに出して
// スキップし、実行を失敗させない。
func collectFindings(roots []string, base string, stderr io.Writer) ([]finding, error) {
	diffMode := base != ""

	var added map[string]map[int]bool
	if diffMode {
		var err error
		added, err = addedLines(base)
		if err != nil {
			// 差分を計算できない場合はビルドを失敗させず、差分限定の検査を
			// スキップする。
			fmt.Fprintf(stderr, "koryluslint comment: 差分を計算できないため検査をスキップする: %v\n", err)
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

			// 差分モードでは、追加行を持たないファイルをコメントの抽出前に打ち切る。
			// 絶対パスの解決はファイルごとに1度で足りる。
			var addedInFile map[int]bool
			if diffMode {
				abs, aerr := filepath.Abs(path)
				if aerr != nil {
					return nil
				}
				addedInFile = added[abs]
				if addedInFile == nil {
					return nil
				}
			}

			groups, gerr := commentGroups(path, ext)
			if gerr != nil {
				fmt.Fprintf(stderr, "koryluslint comment: %s: %v\n", path, gerr)
				return nil
			}
			for _, g := range groups {
				for _, f := range checkGroup(g) {
					if diffMode && !addedInFile[f.line] {
						continue
					}
					f.file = path
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
		return all[i].section < all[j].section
	})
	return all, nil
}

// checkGroupはコメント群の地の文を検査する。
// コードフェンスの状態は群の中で保持し、コード例とツールへの指示を除外する。
func checkGroup(lines []commentLine) []finding {
	bodies := make([]string, len(lines))
	blockStart := -1
	for i, cl := range lines {
		bodies[i] = commentBody(cl.text)
		if strings.HasPrefix(strings.TrimSpace(cl.text), "/*") {
			blockStart = i
		}
		if blockStart >= 0 && strings.HasSuffix(strings.TrimSpace(cl.text), "*/") {
			trimCommonIndent(bodies[blockStart : i+1])
			blockStart = -1
		}
	}

	var fs []finding
	var fence byte
	var fenceSize int
	inList := false
	for i, cl := range lines {
		body := bodies[i]
		marker, size, rest := fenceDelimiter(body)
		if fence != 0 {
			if marker == fence && size >= fenceSize && strings.TrimSpace(rest) == "" {
				fence = 0
			}
			continue
		}
		if isDirective(cl.text) {
			continue
		}
		if marker != 0 && (marker != '`' || !strings.ContainsRune(rest, '`')) {
			fence, fenceSize = marker, size
			continue
		}
		indented := strings.HasPrefix(body, " ") || strings.HasPrefix(body, "\t")
		trimmed := strings.TrimSpace(body)
		// 箇条書きは字下げのない行で終わる。
		// 空行では終わらないため、項目の中に空行を挟んだ2段落目も項目の続きとして扱う
		// (godocは箇条書きの中にコード例を置けない)。
		if !indented && trimmed != "" {
			inList = false
		}
		if reListItem.MatchString(trimmed) {
			inList = true
		}
		if indented && !inList {
			// 箇条書きとその折り返し以外の字下げはgodocのコード例を表す。
			continue
		}
		for _, v := range style.CheckLine(body) {
			fs = append(fs, finding{line: cl.line, section: v.Section, msg: v.Message})
		}
	}
	return fs
}

// commentBodyはコメント記号と直後のスペース1個を除き、本文の字下げを保持する。
func commentBody(text string) string {
	s := strings.TrimLeft(text, " \t")
	if s == "*/" {
		return ""
	}
	for _, leader := range []string{"//", "/*", "*"} {
		if strings.HasPrefix(s, leader) {
			s = strings.TrimPrefix(s, leader)
			s = strings.TrimPrefix(s, " ")
			return strings.TrimRight(strings.TrimSuffix(s, "*/"), " \t\r")
		}
	}
	return strings.TrimRight(strings.TrimSuffix(text, "*/"), " \t\r")
}

// trimCommonIndentはブロックコメント全体の字下げを取り除く。
// 空行は基準に含めず、本文に対して余分に字下げされたコード例はそのまま残す。
func trimCommonIndent(lines []string) {
	prefix := ""
	found := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if !found {
			prefix, found = indent, true
		}
		for !strings.HasPrefix(indent, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, prefix)
	}
}

// fenceDelimiterはコードフェンスの記号・連続数・後続文字列を返す。
// コード例の字下げと区別するため、先頭のスペースは3個まで許容する。
func fenceDelimiter(text string) (byte, int, string) {
	s := strings.TrimLeft(text, " ")
	if len(text)-len(s) > 3 || len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, ""
	}
	n := 1
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	return s[0], n, s[n:]
}

// reListItemはgodocの箇条書きの先頭にマッチする。
var reListItem = regexp.MustCompile(`^(?:[-+*]|[0-9]+[.)])[ \t]+`)

// reDirectiveはGoのコメント指示構文と、引数なしのnolint指示にマッチする。
var reDirective = regexp.MustCompile(`^//(?:[a-z0-9]+:[a-z0-9]|(?:line|extern|export)[ \t]|nolint(?:$|[ \t]))`)

// isDirectiveはコメント記号の直後から始まるツールへの指示かを返す。
// 記号の後ろにスペースがある通常の説明文は除外しない。
func isDirective(text string) bool {
	return reDirective.MatchString(strings.TrimSpace(text))
}

// commentGroupsはファイルからコメント群を抽出する。生成物は空を返す。
func commentGroups(path, ext string) ([][]commentLine, error) {
	// pathはユーザー指定のrootを走査して得たもので、それを読むことこそが
	// ファイルリンタの目的。gosecのファイル混入警告 (G304) はここでは当てはまらない。
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

// goCommentGroupsはgo/parserを使い、文字列リテラル中の "//" を無視する。
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

// templCommentGroupsは行頭 "//" コメントの連続を1群にまとめる。
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

// isGeneratedはsrcが定型の生成物ヘッダーを持つかを返す。
func isGenerated(src []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(src))
	for n := 0; sc.Scan() && n < 20; n++ {
		if reGenerated.MatchString(strings.TrimSpace(sc.Text())) {
			return true
		}
	}
	return false
}

// skipDirは走査しないディレクトリかどうかを返す。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "bin", "tmp", "static":
		return true
	default:
		return false
	}
}

// addedLinesは "git diff --unified=0 base...HEAD" を解析し、base以降に
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
