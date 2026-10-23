// Package planは作業計画書のタスクリストを読み取る。
//
// 読み取るのは「## タスクリスト」節のトップレベルのタスク行と、その下の
// `**保護領域**:` / `**想定行数**:` / `**対応する確認項目**:` の行に限る。
// HTMLコメントとコードブロックの中は、書式の例を誤って拾わないよう読み飛ばす。
package plan

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// sectionTitleはタスクを読み取る節の見出し。
const sectionTitle = "## タスクリスト"

// feedbackPrefixはフィードバック対応のフェーズ番号の接頭辞 (fb1, fb2...)。
const feedbackPrefix = "fb"

var (
	// reTaskはトップレベルのタスク行にマッチし、チェック・タスク番号・
	// 残りの本文を取り出す。
	reTask = regexp.MustCompile(`^- \[([ xX])\] \*\*([0-9A-Za-z]+(?:-[0-9A-Za-z]+)+)\*\*:\s*(.*)$`)
	// rePlatformはタスク名の先頭のプラットフォーム接頭辞 ([Go] など) にマッチする。
	rePlatform = regexp.MustCompile(`^\[([^\]]+)\]\s*(.*)$`)
	// reFieldはタスクの下の `**名前**: 値` の行にマッチする。
	reField = regexp.MustCompile(`^\s+- \*\*([^*]+)\*\*:\s*(.*)$`)
	// reTotalは想定行数の合計 (約N行) にマッチする。
	reTotal = regexp.MustCompile(`^約?([0-9,]+)行`)
	// rePartは想定行数の内訳 (実装N行) にマッチする。
	rePart = regexp.MustCompile(`([^\s(+]+?)\s*([0-9,]+)行`)
	// reFeedbackPhaseはフィードバック対応のフェーズ番号にマッチする。
	reFeedbackPhase = regexp.MustCompile(`^` + feedbackPrefix + `([0-9]+)$`)
	// reItemRefは作業報告書の確認項目への参照 (第N版 #M) にマッチする。
	reItemRef = regexp.MustCompile(`第\s*([0-9]+)\s*版\s*#([0-9]+)`)
)

// Planは作業計画書から読み取った内容。
type Plan struct {
	// Tasksは計画書に書かれた順のタスク。
	Tasks []Task
}

// Taskは1つのタスク。
type Task struct {
	// IDはタスク番号 (1-1, 2-0-1, 5a-1, fb1-1など)。
	ID string
	// Doneはチェックボックスにチェックが入っているかどうか。
	Done bool
	// Platformはプラットフォーム接頭辞 (Go / Railsなど)。無ければ空文字。
	Platform string
	// Titleはプラットフォーム接頭辞を除いたタスク名。
	Title string
	// Protectedは宣言された保護領域。宣言が無ければ空。
	Protected []Declaration
	// Linesは想定行数。記載が無ければゼロ値。
	Lines Size
	// Verifiesは、フィードバック対応のタスクが対応する作業報告書の確認項目。
	// 全体へのコメントから起こしたタスクは項目を参照しないため空になる。
	Verifies []ItemRef
	// LineNoはタスク行の行番号 (1始まり)。
	LineNo int
}

// Declarationは保護領域の宣言1件。
type Declaration struct {
	// Nameは保護領域の名前。
	Name string
	// Noteは括弧内に書かれた触れる内容。無ければ空文字。
	Note string
}

// ItemRefは作業報告書の確認項目への参照 (第Version版 #Number)。
type ItemRef struct {
	Version int
	Number  int
}

// Sizeは想定行数。
type Size struct {
	// Totalは合計の行数。
	Total int
	// Partsは内訳 (実装・テスト・ドキュメントなど) ごとの行数。
	Parts map[string]int
}

// Phaseはタスクが属するフェーズ番号 (タスク番号の先頭の区切り) を返す。
func (t Task) Phase() string {
	phase, _, _ := strings.Cut(t.ID, "-")
	return phase
}

// FeedbackRoundはフィードバック対応のフェーズの回 (fbNのN) を返す。
// フィードバック対応のタスクでなければfalseを返す。
func (t Task) FeedbackRound() (int, bool) {
	m := reFeedbackPhase.FindStringSubmatch(t.Phase())
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Nameは計画書のパスから計画書名 (拡張子を除いたファイル名) を返す。
// 状態ディレクトリやブランチ名に使う。
func Name(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// Nextは最初の未完了のタスクを返す。すべて完了していればfalseを返す。
func (p *Plan) Next() (Task, bool) {
	for _, t := range p.Tasks {
		if !t.Done {
			return t, true
		}
	}
	return Task{}, false
}

// NextFeedbackRoundは、次に追加するフィードバック対応のフェーズの回
// (既存のフェーズfbNの最大のN + 1) を返す。
func (p *Plan) NextFeedbackRound() int {
	n := 0
	for _, t := range p.Tasks {
		if r, ok := t.FeedbackRound(); ok {
			n = max(n, r)
		}
	}
	return n + 1
}

// DoneCountは完了したタスクの数を返す。
func (p *Plan) DoneCount() int {
	n := 0
	for _, t := range p.Tasks {
		if t.Done {
			n++
		}
	}
	return n
}

// Parseはsrcを作業計画書として読み取る。
func Parse(src string) (*Plan, error) {
	p := &Plan{}
	var errs []error
	seen := map[string]int{}
	inSection, inComment := false, false
	var cur *Task

	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	inCode := codeLines(src, lines)
	for i, line := range lines {
		lineNo := i + 1

		if inCode[i] {
			continue
		}
		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		if start := strings.Index(line, "<!--"); start >= 0 && !strings.Contains(line[start:], "-->") {
			inComment = true
			continue
		}

		if strings.HasPrefix(line, "## ") {
			inSection = strings.TrimSpace(line) == sectionTitle
			cur = nil
			continue
		}
		if !inSection {
			continue
		}

		if m := reTask.FindStringSubmatch(line); m != nil {
			t := Task{ID: m[2], Done: m[1] != " ", Title: strings.TrimSpace(m[3]), LineNo: lineNo}
			if pm := rePlatform.FindStringSubmatch(t.Title); pm != nil {
				t.Platform, t.Title = pm[1], pm[2]
			}
			if prev, ok := seen[t.ID]; ok {
				errs = append(errs, fmt.Errorf("%d行目: タスク番号 %s が重複しています (%d行目と同じ)", lineNo, t.ID, prev))
			}
			seen[t.ID] = lineNo
			p.Tasks = append(p.Tasks, t)
			cur = &p.Tasks[len(p.Tasks)-1]
			continue
		}
		if cur == nil {
			continue
		}
		// インデントの無い行が来たら、そのタスクの記述は終わっている。
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			cur = nil
			continue
		}

		m := reField.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "保護領域":
			decls, err := parseDeclarations(m[2])
			if err != nil {
				errs = append(errs, fmt.Errorf("%d行目: タスク %s の保護領域: %w", lineNo, cur.ID, err))
			}
			cur.Protected = append(cur.Protected, decls...)
		case "想定行数":
			size, err := parseSize(m[2])
			if err != nil {
				errs = append(errs, fmt.Errorf("%d行目: タスク %s の想定行数: %w", lineNo, cur.ID, err))
			}
			cur.Lines = size
		case "対応する確認項目":
			for _, rm := range reItemRef.FindAllStringSubmatch(m[2], -1) {
				cur.Verifies = append(cur.Verifies, ItemRef{Version: atoi(rm[1]), Number: atoi(rm[2])})
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(p.Tasks) == 0 {
		return nil, fmt.Errorf("「%s」節にタスクが見つかりません", strings.TrimPrefix(sectionTitle, "## "))
	}
	return p, nil
}

// codeLinesは、srcを行に分けたlinesのうち、コードブロックの中身の行を返す。
// フェンスの種類・長さと、リスト項目の中でのインデントの数え方はパーサーで判定する。
// フェンスの行自体はタスク行にもフィールドの行にも当たらないため、中身の行だけを返す。
func codeLines(src string, lines []string) []bool {
	starts := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		starts[i] = starts[i-1] + len(lines[i-1]) + 1
	}
	lineAt := func(offset int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	}

	inCode := make([]bool, len(lines))
	root := goldmark.DefaultParser().Parse(text.NewReader([]byte(src)))
	// 走査の関数はエラーを返さないため、Walkのエラーは常にnilになる。
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			segments := n.Lines()
			for i := 0; i < segments.Len(); i++ {
				if seg := segments.At(i); seg.Start < seg.Stop {
					inCode[lineAt(seg.Start)] = true
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return inCode
}

// parseDeclarationsは「名前 (触れる内容), 名前 (触れる内容)」の形の宣言を読む。
// 触れる内容にカンマが入ることがあるため、括弧の外のカンマだけで区切る。
func parseDeclarations(s string) ([]Declaration, error) {
	var items []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("括弧が閉じていません: %q", s)
	}
	items = append(items, s[start:])

	decls := make([]Declaration, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		name, note, hasNote := strings.Cut(item, "(")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("保護領域の名前がありません: %q", item)
		}
		d := Declaration{Name: name}
		if hasNote {
			d.Note = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(note), ")"))
		}
		decls = append(decls, d)
	}
	return decls, nil
}

// parseSizeは「約N行 (実装N行 + テストN行)」の形の想定行数を読む。
func parseSize(s string) (Size, error) {
	m := reTotal.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Size{}, fmt.Errorf("「約N行」の形で書いてください: %q", s)
	}
	size := Size{Total: atoi(m[1]), Parts: map[string]int{}}
	if _, breakdown, ok := strings.Cut(s, "("); ok {
		for _, pm := range rePart.FindAllStringSubmatch(breakdown, -1) {
			size.Parts[pm[1]] += atoi(pm[2])
		}
	}
	return size, nil
}

// atoiは桁区切りのカンマを含む数字を整数にする。呼び出し元の正規表現で
// 数字だけであることを確かめているため、変換の失敗は起こらない。
func atoi(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}
