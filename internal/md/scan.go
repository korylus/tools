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
// 強調の閉じ記号の位置はdocLine.emphasisClosersに集め、句点改行が開きの記号と
// 区別できるようにする。
// YAMLフロントマターはMarkdownの地の文ではないため、句点改行の対象から外す。
// パーサーはフロントマターを知らず、区切りの `---` に挟まれた行を見出しや段落と
// して扱うため、行の範囲は文書の先頭から自前で求める。
func eachLine(document string, onLine func(dl docLine)) {
	source := []byte(document)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	lines := strings.Split(document, "\n")
	frontMatterEnd := frontMatterEndLine(lines)
	starts := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		starts[i] = starts[i-1] + len(lines[i-1]) + 1
	}
	lineAt := func(offset int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	}
	included := make([]bool, len(lines))
	masked := make([]bool, len(source))
	emphasisClosers := map[int]bool{}
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
		case *ast.Emphasis:
			// 閉じ記号の直前が本文なら、その末尾の位置を記録する。
			// リンクやコードで終わる場合は閉じ記号が「。」に隣接しない。
			// 入れ子の強調は子ノードの走査で扱うため、ここでは直接の子だけを見る。
			if last, ok := n.LastChild().(*ast.Text); ok {
				stop := last.Segment.Stop
				if stop < len(source) && (source[stop] == '*' || source[stop] == '_') {
					emphasisClosers[stop] = true
				}
			}
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
		// ASTのバイト位置を、breakProseが使う行内のルーン位置へ変換する。
		// 強調が1つも無いドキュメントでは変換するものが無いため、走査ごと省く。
		var closers map[int]bool
		if len(emphasisClosers) > 0 {
			runeIndex := 0
			for byteOffset := range line {
				if emphasisClosers[starts[i]+byteOffset] {
					if closers == nil {
						closers = map[int]bool{}
					}
					closers[runeIndex] = true
				}
				runeIndex++
			}
		}
		onLine(docLine{
			lineNo: i + 1,
			text:   line,
			// マスクだけが残る行はコメントやコードの内側なので地の文に数えない。
			// フロントマターが無ければfrontMatterEndは-1のため、全行が対象に残る。
			prose:           i > frontMatterEnd && isProse(line) && strings.TrimSpace(style) != "",
			style:           style,
			emphasisClosers: closers,
		})
	}
}

// frontMatterEndLineはYAMLフロントマターの終端の行番号 (0始まり) を返す。
// 1行目が区切りで、閉じの区切りがあり、挟まれた中身がYAMLのマッピングとして
// 読める場合だけフロントマターとみなす。
// フロントマターが無ければ-1を返す。
func frontMatterEndLine(lines []string) int {
	if len(lines) == 0 || !isFrontMatterDelimiter(lines[0]) {
		return -1
	}
	for i := 1; i < len(lines); i++ {
		if !isFrontMatterDelimiter(lines[i]) {
			continue
		}
		if !looksLikeYAMLMapping(lines[1:i]) {
			// 区切り線で始まり、以降にもう1本の区切り線がある文書は
			// フロントマターではない。中身ごと検査から外すと違反を見逃す。
			return -1
		}
		return i
	}
	// 閉じの区切りが無いものはフロントマターではなく、ただの区切り線とみなす。
	return -1
}

// looksLikeYAMLMappingはlinesがYAMLのマッピングとして読める形かを返す。
// 空行とコメントを除く最初の行が `キー:` の形かどうかで判定する。
// YAMLとして解釈する必要は無く、区切り線に挟まれただけの地の文と区別できれば
// 足りる。
func looksLikeYAMLMapping(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// 先頭が `-` の行はマッピングではなくシーケンス。
		// フロントマターの値としての箇条書きは、先にキーの行が来る。
		if strings.HasPrefix(trimmed, "-") {
			return false
		}
		key, _, ok := strings.Cut(trimmed, ":")
		return ok && strings.TrimSpace(key) != ""
	}
	// 中身が空、またはコメントだけのものはフロントマターとみなさない。
	// 外すべき地の文が無いため、どちらに倒しても検査の結果は変わらない。
	return false
}

// isFrontMatterDelimiterはlineがYAMLフロントマターの区切り (`---`) かを返す。
// 行末の空白は区切りの一部とみなして無視する。
func isFrontMatterDelimiter(line string) bool {
	// LFで分割したCRLFの行には末尾のCRが残るため、判定時だけ取り除く。
	line = strings.TrimSuffix(line, "\r")
	return strings.TrimRight(line, " \t") == "---"
}
