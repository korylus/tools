package md

import (
	"bytes"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// eachLineはMarkdownの構文を解析し、コードブロック以外の行を検査へ渡す。
// フェンスの種類・長さと引用・リストの入れ子はパーサーで判定する。
// HTMLコメントはインラインコードと区別して除外し、元の行番号を維持する。
// インラインコードとHTMLコメントの範囲はdocLine.styleで半角スペースに置き換え、
// 句点改行と §3の検査がどちらもこの範囲を避けられるようにする。
func eachLine(document string, onLine func(dl docLine)) {
	source := []byte(document)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	lines := strings.Split(document, "\n")
	starts := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		starts[i] = starts[i-1] + len(lines[i-1]) + 1
	}
	lineAt := func(offset int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	}
	included := make([]bool, len(lines))
	masked := make([]bool, len(source))
	include := func(segment text.Segment) {
		if segment.Start >= segment.Stop {
			return
		}
		for i := lineAt(segment.Start); i <= lineAt(segment.Stop-1); i++ {
			included[i] = true
		}
	}
	mask := func(start, stop int) {
		for i := start; i < stop; i++ {
			if source[i] != '\n' {
				masked[i] = true
			}
		}
	}
	// maskLineはoffsetから始まる1行の、マスクした範囲を半角スペースへ置き換えて返す。
	// 多バイト文字も1つの半角スペースにすることで、元の行とルーン数が揃う。
	// 句点改行の区切り位置を元の行と突き合わせるために必要になる。
	maskLine := func(offset int, line string) string {
		var b strings.Builder
		b.Grow(len(line))
		for i, r := range line {
			if masked[offset+i] {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	var visit func(ast.Node)
	visit = func(node ast.Node) {
		switch n := node.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			return
		case *ast.HTMLBlock:
			// HTMLブロックの中ではバッククォートもHTMLの文字列となる。
			segments := n.Lines()
			for i := 0; i < segments.Len(); i++ {
				include(segments.At(i))
			}
			if n.HasClosure() {
				include(n.ClosureLine)
			}
			if segments.Len() == 0 {
				return
			}
			start := segments.At(0).Start
			stop := segments.At(segments.Len() - 1).Stop
			if n.HasClosure() {
				stop = n.ClosureLine.Stop
			}
			for start < stop {
				i := bytes.Index(source[start:stop], []byte("<!--"))
				if i < 0 {
					break
				}
				start += i
				end := stop
				if j := bytes.Index(source[start:stop], []byte("-->")); j >= 0 {
					end = start + j + 3
				}
				mask(start, end)
				start = end
			}
			return
		case *ast.CodeSpan:
			// インラインコードは §3の対象外。
			// 段落内で改行をまたぐコード片もASTの範囲で除外する。
			// 残る区切り記号はstyle.CheckMaskedLineで再解釈しないため、
			// 同じ行にある別のコード片との間の本文も検査できる。
			for child := n.FirstChild(); child != nil; child = child.NextSibling() {
				if t, ok := child.(*ast.Text); ok {
					mask(t.Segment.Start, t.Segment.Stop)
				}
			}
			return
		case *ast.RawHTML:
			if n.Segments.Len() > 0 {
				start := n.Segments.At(0).Start
				stop := n.Segments.At(n.Segments.Len() - 1).Stop
				if bytes.HasPrefix(source[start:stop], []byte("<!--")) {
					mask(start, stop)
				}
			}
		default:
			if node.Type() == ast.TypeBlock {
				for i := 0; i < node.Lines().Len(); i++ {
					include(node.Lines().At(i))
				}
			}
		}
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			visit(child)
		}
	}
	visit(root)
	for i, line := range lines {
		if !included[i] {
			continue
		}
		style := maskLine(starts[i], line)
		onLine(docLine{
			lineNo: i + 1,
			text:   line,
			// マスクだけが残る行はコメントやコードの内側なので地の文に数えない。
			prose: isProse(line) && strings.TrimSpace(style) != "",
			style: style,
		})
	}
}
