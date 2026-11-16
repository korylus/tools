package comment

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	treesitter "github.com/tree-sitter/go-tree-sitter"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"

	"github.com/korylus/tools/internal/style"
)

// rustCommentは、コメントのソース範囲と、docコメントが文書化する対象を持つ。
type rustComment struct {
	startLine int
	endLine   int
	startByte uint
	endByte   uint
	text      string
	// lineは行コメント (`//`・`///`・`//!`) かどうか。偽ならブロックコメント。
	line bool
	// targetは文書化する構文ノードのID。通常のコメントでは0。
	target uintptr
}

// rustCommentGroupは、通常のコメント群か、同じ対象を文書化するdocコメント群。
type rustCommentGroup struct {
	lines []commentLine
	doc   bool
}

// checkRustは、Rustの構文木からコメントとdocコメントを拾い、本文を検査する。
//
// 文字列リテラルは検査しない。
// エラーメッセージがコマンド名やオプションをスペースで区切って含み、テストのデータが §3に反する形を意図して持つためである。
// 構文解析で拾うのは、文字列の中の "//" をコメントと誤認しないためである。
func checkRust(src []byte) ([]finding, error) {
	parser := treesitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(treesitter.NewLanguage(rust.Language())); err != nil {
		return nil, fmt.Errorf("構文解析の文法を設定できない: %w", err)
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, errors.New("構文木を作成できない")
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return nil, errors.New("構文エラーがあるためコメントを抽出できない")
	}

	var comments []rustComment
	// カーソルで走査し、入れ子の深さに応じたGoの再帰呼び出しを避ける。
	cursor := tree.RootNode().Walk()
	defer cursor.Close()
walk:
	for {
		node := cursor.Node()
		kind := node.Kind()
		if kind == "line_comment" || kind == "block_comment" {
			start, err := lineOf(node)
			if err != nil {
				return nil, err
			}
			// 行コメントのノードは、文法によって末尾の改行を含む。
			text := strings.TrimRight(node.Utf8Text(src), "\r\n")
			c := rustComment{
				startLine: start,
				endLine:   start + strings.Count(text, "\n"),
				startByte: node.StartByte(),
				endByte:   node.EndByte(),
				text:      text,
				line:      kind == "line_comment",
				target:    rustDocTarget(node),
			}
			if c.target != 0 {
				c.text = ""
				if body := node.ChildByFieldName("doc"); body != nil {
					c.text = strings.TrimRight(body.Utf8Text(src), "\r\n")
				}
			}
			comments = append(comments, c)
		} else if cursor.GotoFirstChild() {
			continue
		}
		for !cursor.GotoNextSibling() {
			if !cursor.GotoParent() {
				break walk
			}
		}
	}
	var findings []finding
	for _, group := range rustCommentGroups(src, comments) {
		if group.doc {
			findings = append(findings, checkRustDoc(group.lines)...)
		} else {
			findings = append(findings, checkGroup(group.lines)...)
		}
	}
	return findings, nil
}

// rustDocTargetは、外側のdocコメントでは次の項目、内側では所属する項目のIDを返す。
func rustDocTarget(node *treesitter.Node) uintptr {
	if node.ChildByFieldName("inner") != nil {
		parent := node.Parent()
		if parent.Kind() == "declaration_list" || parent.Kind() == "block" {
			parent = parent.Parent()
		}
		return parent.Id()
	}
	if node.ChildByFieldName("outer") == nil {
		return 0
	}
	for next := node.NextNamedSibling(); next != nil; next = next.NextNamedSibling() {
		switch next.Kind() {
		case "line_comment", "block_comment", "attribute_item":
			continue
		default:
			return next.Id()
		}
	}
	// 対象となる項目が無いdocコメントも、単独の文書として検査する。
	return node.Id()
}

// rustCommentGroupsは、docコメントを対象ごとにまとめ、通常のコメントはコードの区切りで分ける。
func rustCommentGroups(src []byte, comments []rustComment) []rustCommentGroup {
	var groups []rustCommentGroup
	docGroups := make(map[uintptr]int)
	for i, c := range comments {
		if c.target != 0 {
			index, ok := docGroups[c.target]
			if !ok {
				index = len(groups)
				docGroups[c.target] = index
				groups = append(groups, rustCommentGroup{doc: true})
			}
			groups[index].lines = append(groups[index].lines, rustDocLines(c)...)
			continue
		}
		var lines []commentLine
		for j, text := range strings.Split(c.text, "\n") {
			lines = append(lines, commentLine{line: c.startLine + j, text: text})
		}
		if i > 0 && c.line {
			previous := comments[i-1]
			if previous.target == 0 && previous.line && c.startLine == previous.endLine+1 &&
				startsLine(src, previous.startByte) &&
				strings.TrimSpace(string(src[previous.endByte:c.startByte])) == "" {
				groups[len(groups)-1].lines = append(groups[len(groups)-1].lines, lines...)
				continue
			}
		}
		groups = append(groups, rustCommentGroup{lines: lines})
	}
	return groups
}

// startsLineは、offsetより前の同じ行に空白しか無いかを返す。
// 行末のコメントは、go/parserと同じく次の行のコメントと別の群にする。
func startsLine(src []byte, offset uint) bool {
	lineStart := bytes.LastIndexByte(src[:offset], '\n') + 1
	return strings.TrimSpace(string(src[lineStart:offset])) == ""
}

// rustDocLinesはdocコメントの本文を元の行番号と対応させ、ブロックコメントの共通の星飾りを除く。
func rustDocLines(c rustComment) []commentLine {
	var lines []commentLine
	for i, line := range strings.Split(c.text, "\n") {
		lines = append(lines, commentLine{line: c.startLine + i, text: strings.TrimSuffix(line, "\r")})
	}
	if c.line {
		return lines
	}
	if strings.Trim(lines[0].text, "*") == "" && len(lines) > 1 {
		lines = lines[1:]
	}
	if last := lines[len(lines)-1].text; last != "" && strings.Trim(last, "*") == "" && len(lines) > 1 {
		lines = lines[:len(lines)-1]
	}
	candidates := lines
	if !strings.HasPrefix(strings.TrimLeft(candidates[0].text, " \t"), "*") {
		candidates = candidates[1:]
	}
	for len(candidates) > 0 && strings.TrimSpace(candidates[0].text) == "" {
		candidates = candidates[1:]
	}
	for len(candidates) > 0 && strings.TrimSpace(candidates[len(candidates)-1].text) == "" {
		candidates = candidates[:len(candidates)-1]
	}
	margin := -1
	for _, line := range candidates {
		indent := len(line.text) - len(strings.TrimLeft(line.text, " \t"))
		if indent == len(line.text) || line.text[indent] != '*' || (margin >= 0 && margin != indent) {
			return lines
		}
		margin = indent
	}
	if margin >= 0 {
		for i := range lines {
			if len(lines[i].text) < margin || strings.TrimSpace(lines[i].text[:margin]) != "" {
				continue
			}
			body := lines[i].text[margin:]
			if body == "*" || strings.HasPrefix(body, "* ") || strings.HasPrefix(body, "**") {
				body = body[1:]
			}
			lines[i].text = body
		}
	}
	return lines
}

// checkRustDocは共通の字下げを除き、CommonMarkのコードブロックとインラインコードを検査から外す。
func checkRustDoc(lines []commentLine) []finding {
	indent := -1
	for _, line := range lines {
		if strings.TrimSpace(line.text) == "" {
			continue
		}
		spaces := len(line.text) - len(strings.TrimLeft(line.text, " \t"))
		if indent < 0 || spaces < indent {
			indent = spaces
		}
	}
	var document strings.Builder
	starts := make([]int, len(lines)+1)
	for i, line := range lines {
		body := line.text
		if indent > 0 && strings.TrimSpace(body) != "" {
			body = body[indent:]
		}
		document.WriteString(body)
		document.WriteByte('\n')
		starts[i+1] = document.Len()
	}
	source := []byte(document.String())
	masked := append([]byte(nil), source...)
	included := make([]bool, len(lines))
	lineAt := func(offset int) int {
		return sort.Search(len(starts), func(i int) bool { return starts[i] > offset }) - 1
	}
	root := parser.NewParser(
		parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithInlineParsers(parser.DefaultInlineParsers()...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...),
	).Parse(text.NewReader(source))
	var visit func(ast.Node)
	visit = func(node ast.Node) {
		switch node.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			return
		case *ast.CodeSpan:
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				if text, ok := child.(*ast.Text); ok {
					for i := text.Segment.Start; i < text.Segment.Stop; i++ {
						if masked[i] != '\n' {
							masked[i] = ' '
						}
					}
				}
			}
			return
		}
		if node.Type() == ast.TypeBlock {
			for i := 0; i < node.Lines().Len(); i++ {
				segment := node.Lines().At(i)
				if segment.Start < segment.Stop {
					for line := lineAt(segment.Start); line <= lineAt(segment.Stop-1); line++ {
						included[line] = true
					}
				}
			}
		}
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			visit(child)
		}
	}
	visit(root)
	var findings []finding
	for i, line := range lines {
		if !included[i] {
			continue
		}
		for _, violation := range style.CheckMaskedLine(string(masked[starts[i]:starts[i+1]])) {
			findings = append(findings, finding{line: line.line, section: violation.Section, msg: violation.Message})
		}
	}
	return findings
}
