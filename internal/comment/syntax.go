package comment

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	treesitter "github.com/tree-sitter/go-tree-sitter"
	bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/korylus/tools/internal/style"
)

// reShellOptionはコマンドのオプションにあたる引数にマッチする。
var reShellOption = regexp.MustCompile(`^--?[^-\s]`)

// argumentExprsは引数の中でコード構文として扱うノードの種別。
// 展開と置換は実行時に値へ変わるため、地の文の一部としては読めない。
var argumentExprs = map[string]bool{
	"expansion":            true,
	"simple_expansion":     true,
	"command_substitution": true,
	"process_substitution": true,
	"arithmetic_expansion": true,
}

// checkSyntaxは構文木からコメントと文字列の本文を拾い、構文上の区切りを検査から外す。
// シェルの引用・コマンド置換とTypeScriptのテンプレート補間は入れ子になるため、
// 各文法のパーサーに本文の範囲と元ファイルの行番号を決めさせる。
func checkSyntax(src []byte, kind fileKind) ([]finding, error) {
	var language *treesitter.Language
	switch kind {
	case kindShell:
		language = treesitter.NewLanguage(bash.Language())
	case kindTypeScript:
		language = treesitter.NewLanguage(typescript.LanguageTypescript())
	default:
		return nil, fmt.Errorf("構文解析の対象外のファイル種別: %d", kind)
	}
	parser := treesitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(language); err != nil {
		return nil, fmt.Errorf("構文解析の文法を設定できない: %w", err)
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, errors.New("構文木を作成できない")
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return nil, errors.New("構文エラーがあるためコメントと文字列を抽出できない")
	}

	// 1行に複数の文字列やコメントがあっても、同じ節の違反は1件だけ報告する。
	found := make(map[finding]bool)
	add := func(text string, firstLine int, comment bool) {
		for i, line := range strings.Split(text, "\n") {
			if !style.ContainsJapanese(line) {
				continue
			}
			check := style.CheckMaskedLine
			if comment {
				check = style.CheckLine
			}
			for _, v := range check(line) {
				found[finding{line: firstLine + i, section: v.Section, msg: v.Message}] = true
			}
		}
	}

	// カーソルで走査し、入れ子の深さに応じたGoの再帰呼び出しを避ける。
	cursor := tree.RootNode().Walk()
	defer cursor.Close()
	// 引数列の取り出しに使うカーソル。走査中のcursorを動かさないよう別に持つ。
	argCursor := tree.RootNode().Walk()
	defer argCursor.Close()
walk:
	for {
		node := cursor.Node()
		firstLine, err := lineOf(node)
		if err != nil {
			return nil, err
		}
		skipChildren := false
		switch node.Kind() {
		case "comment":
			add(node.Utf8Text(src), firstLine, true)
			skipChildren = true
		case "command":
			// 引数どうしの区切りは地の文のスペース。単語ごとのノードでは
			// またぐ違反を拾えないため、引数列を1つの文面としても検査する。
			text, first, ok := commandArguments(src, node, argCursor)
			if !ok {
				break
			}
			argLine, aerr := lineOf(first)
			if aerr != nil {
				return nil, aerr
			}
			add(text, argLine, false)
		case "string_fragment", "string_content", "raw_string", "ansi_c_string", "word", "heredoc_content":
			add(node.Utf8Text(src), firstLine, false)
			skipChildren = true
		case "heredoc_body":
			// 最初の補間より前の本文は、Bash文法では子ノードにならない。
			end := node.EndByte()
			if child := node.NamedChild(0); child != nil {
				end = child.StartByte()
			}
			add(string(src[node.StartByte():end]), firstLine, false)
		case "command_name", "regex":
			// 実行対象の名前と正規表現は、表示する文面ではなくコード構文。
			skipChildren = true
		}
		if !skipChildren && cursor.GotoFirstChild() {
			continue
		}
		for !cursor.GotoNextSibling() {
			if !cursor.GotoParent() {
				break walk
			}
		}
	}
	var fs []finding
	for f := range found {
		fs = append(fs, f)
	}
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].line != fs[j].line {
			return fs[i].line < fs[j].line
		}
		return fs[i].section < fs[j].section
	})
	return fs, nil
}

// lineOfはノードの開始行を1始まりで返す。
func lineOf(node *treesitter.Node) (int, error) {
	row := node.StartPosition().Row
	if row >= uint(math.MaxInt) {
		return 0, errors.New("行番号が報告可能な範囲を超えている")
	}
	return int(row) + 1, nil
}

// commandArgumentsはコマンドの引数列を1つの文面にまとめ、先頭の引数を返す。
//
// 引用符で囲まない引数は単語ごとに別のノードになるため、ノード単位の検査では
// `echo 完了 20 件` のように単語をまたぐ §3.2違反を拾えない。
// 範囲はコマンド名を含めず最初の引数から始め、オプションと展開・置換は空白へ
// 置き換える。こうして残るのは引数どうしを隔てる地の文のスペースだけになる。
// 置き換えでも改行はそのまま残し、報告する行番号がずれないようにする。
func commandArguments(src []byte, cmd *treesitter.Node, cursor *treesitter.TreeCursor) (string, *treesitter.Node, bool) {
	args := cmd.ChildrenByFieldName("argument", cursor)
	if len(args) == 0 {
		return "", nil, false
	}
	start, end := args[0].StartByte(), args[len(args)-1].EndByte()
	buf := make([]byte, end-start)
	copy(buf, src[start:end])
	// 引数の内側の範囲しか渡らない想定だが、文法の差異で範囲が外れても
	// リンタが落ちないよう、バッファの外は無視する。
	mask := func(from, to uint) {
		for i := max(from, start); i < min(to, end); i++ {
			if buf[i-start] != '\n' {
				buf[i-start] = ' '
			}
		}
	}
	for i := range args {
		arg := &args[i]
		if reShellOption.MatchString(arg.Utf8Text(src)) {
			mask(arg.StartByte(), arg.EndByte())
			continue
		}
		maskExprs(arg, mask)
	}
	return string(buf), &args[0], true
}

// maskExprsはノードに含まれる展開・置換の範囲をmaskへ渡す。
func maskExprs(node *treesitter.Node, mask func(from, to uint)) {
	cursor := node.Walk()
	defer cursor.Close()
	for {
		skip := argumentExprs[cursor.Node().Kind()]
		if skip {
			mask(cursor.Node().StartByte(), cursor.Node().EndByte())
		}
		if !skip && cursor.GotoFirstChild() {
			continue
		}
		for !cursor.GotoNextSibling() {
			if !cursor.GotoParent() {
				return
			}
		}
	}
}
