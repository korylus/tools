// Package reviewは、/korylus-review が書いたレビュードキュメントを読み取り、
// 指摘ごとの区分と推奨をもとに対応方針へ決定的に回答する。
//
// 読み取るのは総合評価と、「ファイルごとのレビュー結果」「設計改善の提案」
// 「過去レビューの修正検証」の各節にある「**対応方針**:」の選択肢に限る。
// テンプレートの書式の例はHTMLコメントの中にあるため、goldmarkのASTで
// コメントとコードブロックを読み飛ばして構造を辿る。
package review

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Dirはレビュードキュメントを置く、プロジェクトのルートからの相対パス。
const Dir = "docs/private/reviews"

// Verdictはレビューの総合評価。
type Verdict string

const (
	Approve        Verdict = "Approve"
	RequestChanges Verdict = "Request Changes"
	Comment        Verdict = "Comment"
)

// Categoryは指摘の区分。
type Category string

const (
	// AutoFixは、推奨の選択肢をそのまま回答に選んでよい指摘。
	AutoFix Category = "自動対応可"
	// NeedsHumanは、人間が判断するまで止める指摘。
	NeedsHuman Category = "要人間判断"
)

// 指摘を読み取る節の名前。
const (
	sectionFindings     = "ファイルごとのレビュー結果"
	sectionDesign       = "設計改善の提案"
	sectionVerification = "過去レビューの修正検証"
	sectionSummary      = "総合評価"
)

// recommendedMarkは推奨の選択肢の末尾に付く印。
const recommendedMark = "(推奨)"

var (
	reCategory = regexp.MustCompile(`^\*\*区分\*\*:\s*(\S+)`)
	reVerdict  = regexp.MustCompile(`^\*\*評価\*\*:\s*(.*)$`)
	reOption   = regexp.MustCompile(`^\[([ xX])\]\s*(.*)$`)
	// reDeferは、コードを変えない選択肢 (後続タスクに積む・現状維持・取り下げなど) にマッチする。
	reDefer = regexp.MustCompile(`^(後続タスクに積む|現状維持|現状のまま|指摘を取り下げる|今回は見送る|対応しない)(\s*\([^()\n]*\))*$`)
)

// Documentはレビュードキュメントから読み取った内容。
type Document struct {
	Verdict  Verdict
	Findings []Finding
	src      []byte
}

// Findingは対応方針の選択肢を持つ指摘1件。
type Finding struct {
	// Sectionは指摘がある節の名前。
	Section string
	// Titleは指摘の見出し (再検討提案は、その項目の1行目)。
	Title string
	// Categoryは区分。書かれていなければ空。
	Category Category
	// Proposalは「過去レビューの修正検証」の再検討提案かどうか。
	Proposal bool
	Options  []Option
	// LineNoは「**対応方針**:」の行番号 (1始まり)。欠けていれば、欠落を見つけた区分・見出し・項目の行番号。
	LineNo int
}

// Optionは対応方針の選択肢1つ。
type Option struct {
	// Textは「(推奨)」を含む選択肢の本文。
	Text        string
	Checked     bool
	Recommended bool
	// markは、チェックボックスの括弧の中の文字のsrcでの位置。
	mark int
}

// Defersは、この選択肢がコードを変えずに指摘を先送りするものかを返す。
func (o Option) Defers() bool {
	return reDefer.MatchString(o.Text)
}

// Parseはsrcをレビュードキュメントとして読み取る。
func Parse(src []byte) (*Document, error) {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	d := &Document{src: src}
	root := goldmark.DefaultParser().Parse(text.NewReader(src))

	var errs []error
	var section, title string
	// gは見出しの中の区分の状態。見出しごとに取り直す。
	var g group
	verdicts := 0
	groupStart, groupLine := 0, 1
	// 対応方針の欠落を指摘なしと取り違えないよう、見出しの境界でも検証する。
	flush := func() {
		if section != sectionFindings && section != sectionDesign {
			return
		}
		switch {
		case g.pending > 0:
			d.addMissing(section, title, g.category, false, g.pending)
		case (title != "" || g.category != "") && len(d.Findings) == groupStart:
			d.addMissing(section, title, g.category, false, groupLine)
		}
	}
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		switch n := n.(type) {
		case *ast.Heading:
			if n.Level == 2 || n.Level == 3 {
				flush()
				groupStart = len(d.Findings)
				groupLine = startLine(src, n)
			}
			switch n.Level {
			case 2:
				section, title, g = lineText(src, n), "", group{}
			case 3:
				title, g = lineText(src, n), group{}
			}
			continue
		case *ast.Paragraph:
			if section == sectionSummary {
				for _, l := range blockLines(src, n) {
					m := reVerdict.FindStringSubmatch(l.text)
					if m == nil {
						continue
					}
					verdicts++
					v, err := parseVerdict(m[1])
					if err != nil {
						errs = append(errs, fmt.Errorf("%d行目: %w", l.no, err))
					}
					d.Verdict = v
				}
				continue
			}
		}
		switch section {
		case sectionFindings, sectionDesign:
			if list, ok := n.(*ast.List); ok {
				d.collectList(list, section, title, &g)
			} else {
				d.collect(n, section, title, &g, false)
			}
		case sectionVerification:
			// 再検討提案は、修正検証の項目 (トップレベルのリスト項目) ごとに区分と対応方針を持つ。
			list, ok := n.(*ast.List)
			if !ok {
				continue
			}
			for item := list.FirstChild(); item != nil; item = item.NextSibling() {
				var ig group
				start := len(d.Findings)
				title := firstLine(src, item)
				d.collect(item, section, title, &ig, true)
				switch {
				case ig.pending > 0:
					d.addMissing(section, title, ig.category, true, ig.pending)
				case len(d.Findings) == start && strings.Contains(title, "再検討提案"):
					d.addMissing(section, title, ig.category, true, startLine(src, item))
				}
			}
		}
	}

	flush()

	switch {
	case verdicts == 0:
		errs = append(errs, fmt.Errorf("「%s」節に「**評価**:」の行がありません", sectionSummary))
	case verdicts > 1:
		errs = append(errs, fmt.Errorf("「%s」節に「**評価**:」の行が%d個あります", sectionSummary, verdicts))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d, nil
}

// groupは、1つの見出し (再検討提案は1つの項目) の中で読んだ区分の状態。
type group struct {
	// categoryは最後に読んだ区分。
	category Category
	// pendingは、まだ対応方針が続いていない区分の行番号。無ければ0。
	pending int
}

// collectはnodeの中を文書の順に辿り、「**区分**:」をgに残し、
// 「**対応方針**:」の直後のリストを指摘として加える。区分は同じ見出しの中で
// 最後に書かれたものを、そのあとの対応方針に当てる。対応方針が続かないまま
// 次の区分が書かれたら、前の区分の指摘を対応方針の欠落として加える。
func (d *Document) collect(node ast.Node, section, title string, g *group, proposal bool) {
	// 走査の関数はエラーを返さないため、Walkのエラーは常にnilになる。
	_ = ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock, *ast.HTMLBlock:
			return ast.WalkSkipChildren, nil
		case *ast.Paragraph:
		default:
			return ast.WalkContinue, nil
		}
		// 空行を挟まずに続けて書いた「**区分**:」と「**対応方針**:」は1つの段落になるため、行ごとに見る。
		for _, l := range blockLines(d.src, n) {
			if m := reCategory.FindStringSubmatch(l.text); m != nil {
				if g.pending > 0 {
					d.addMissing(section, title, g.category, proposal, g.pending)
				}
				g.category, g.pending = Category(m[1]), l.no
				continue
			}
			if !strings.HasPrefix(l.text, "**対応方針**:") {
				continue
			}
			g.pending = 0
			f := Finding{Section: section, Title: title, Category: g.category, Proposal: proposal, LineNo: l.no}
			if list := nextList(n); list != nil {
				f.Options = d.options(list)
			}
			d.Findings = append(d.Findings, f)
		}
		return ast.WalkSkipChildren, nil
	})
}

// collectListは指摘の節のトップレベルのリストを項目ごとに辿る。テンプレートは1つの見出しの
// 下に問題点を項目として並べるため、対応方針を持つ項目が1つでもあれば、対応方針を
// 持たない項目も対応方針の欠落として加える。
func (d *Document) collectList(list *ast.List, section, title string, g *group) {
	var empty []ast.Node
	found := false
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		start, before := len(d.Findings), g.pending
		d.collect(item, section, title, g, false)
		switch {
		case len(d.Findings) > start:
			found = true
		case g.pending > 0 && g.pending != before:
			// 項目の中で区分を書いたのに対応方針が続かなかった。
			d.addMissing(section, title, g.category, false, g.pending)
			g.pending = 0
		default:
			empty = append(empty, item)
		}
	}
	if !found {
		return
	}
	for _, item := range empty {
		d.addMissing(section, title, g.category, false, startLine(d.src, item))
	}
}

// addMissingは、対応方針が欠けた指摘を選択肢の無い指摘として加える。
// 回答では、回答できない指摘として止める。lineNoは欠落を見つけた場所の行番号。
func (d *Document) addMissing(section, title string, category Category, proposal bool, lineNo int) {
	d.Findings = append(d.Findings, Finding{Section: section, Title: title, Category: category, Proposal: proposal, LineNo: lineNo})
}

// optionsはリストの項目のうち、チェックボックスで始まるものを選択肢として返す。
func (d *Document) options(list *ast.List) []Option {
	var opts []Option
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		block := item.FirstChild()
		if block == nil || block.Lines().Len() == 0 {
			continue
		}
		start := block.Lines().At(0).Start
		m := reOption.FindStringSubmatch(blockText(d.src, block))
		if m == nil || !bytes.HasPrefix(d.src[start:], []byte("[")) {
			continue
		}
		body := strings.TrimSpace(m[2])
		opts = append(opts, Option{
			Text:        body,
			Checked:     m[1] != " ",
			Recommended: strings.HasSuffix(body, recommendedMark),
			mark:        start + 1,
		})
	}
	return opts
}

// nextListは、段落nのあとに続くリストを返す。間のHTMLコメント
// (「開発者が回答を記入してください」など) は読み飛ばす。
func nextList(n ast.Node) *ast.List {
	for s := n.NextSibling(); s != nil; s = s.NextSibling() {
		switch s := s.(type) {
		case *ast.HTMLBlock:
			continue
		case *ast.List:
			return s
		}
		return nil
	}
	return nil
}

// parseVerdictは「**評価**:」の値を総合評価にする。
func parseVerdict(s string) (Verdict, error) {
	v := strings.Trim(strings.TrimSpace(s), "*`")
	for _, want := range []Verdict{Approve, RequestChanges, Comment} {
		if strings.EqualFold(v, string(want)) {
			return want, nil
		}
	}
	return "", fmt.Errorf("評価 %q を読み取れません (Approve / Request Changes / Commentのどれか1つを書きます)", s)
}

// lineTextはブロックの1行目の本文を返す。
func lineText(src []byte, n ast.Node) string {
	lines := n.Lines()
	if lines.Len() == 0 {
		return ""
	}
	seg := lines.At(0)
	return strings.TrimSpace(string(seg.Value(src)))
}

// lineはブロックの1行。
type line struct {
	text string
	// noは行番号 (1始まり)。
	no int
}

// blockLinesはブロックの各行を、前後の空白を除いて返す。
func blockLines(src []byte, n ast.Node) []line {
	lines := n.Lines()
	res := make([]line, 0, lines.Len())
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		res = append(res, line{
			text: strings.TrimSpace(string(seg.Value(src))),
			no:   bytes.Count(src[:seg.Start], []byte("\n")) + 1,
		})
	}
	return res
}

// blockTextはブロックの全行を空白でつないだ本文を返す。
func blockText(src []byte, n ast.Node) string {
	lines := blockLines(src, n)
	parts := make([]string, 0, len(lines))
	for _, l := range lines {
		parts = append(parts, l.text)
	}
	return strings.Join(parts, " ")
}

// startLineはブロックnの1行目の行番号 (1始まり) を返す。行を持たないブロック
// (リスト項目など) は、最初の子孫のブロックで数える。
func startLine(src []byte, n ast.Node) int {
	for n != nil && n.Lines().Len() == 0 {
		n = n.FirstChild()
	}
	if n == nil {
		return 0
	}
	return bytes.Count(src[:n.Lines().At(0).Start], []byte("\n")) + 1
}

// firstLineはリスト項目の最初のブロックの1行目を返す。
func firstLine(src []byte, item ast.Node) string {
	if c := item.FirstChild(); c != nil {
		return lineText(src, c)
	}
	return ""
}

// Latestは、rootのレビュードキュメントの置き場所 (Dir) から、レビュー接頭辞prefixの
// ドキュメント ({prefix}-{3桁連番}.md) のうち連番が最大のものを、ルートからの
// 相対パスで返す。見つからなければokはfalse。
func Latest(root, prefix string) (path string, ok bool, err error) {
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	re := sequencePattern(prefix)
	type doc struct {
		name string
		n    int
	}
	var docs []doc
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, ok := sequence(re, e.Name())
		if !ok {
			continue
		}
		docs = append(docs, doc{e.Name(), n})
	}
	if len(docs) == 0 {
		return "", false, nil
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].n < docs[j].n })
	return filepath.ToSlash(filepath.Join(Dir, docs[len(docs)-1].name)), true, nil
}

// Sequenceは、レビュー接頭辞prefixのドキュメントのパスdocから連番を返す。
// ファイル名が {prefix}-{3桁以上の連番}.md の形でなければokはfalse。
func Sequence(prefix, doc string) (n int, ok bool) {
	return sequence(sequencePattern(prefix), filepath.Base(doc))
}

// sequencePatternは、レビュー接頭辞prefixのドキュメントのファイル名にマッチし、
// 連番を1つ目のグループに持つ正規表現を返す。
func sequencePattern(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-([0-9]{3,})\.md$`)
}

// sequenceは、ファイル名nameがreにマッチすれば、その連番を返す。
func sequence(re *regexp.Regexp, name string) (int, bool) {
	m := re.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
