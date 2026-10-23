package gate

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
)

// ReadGoSourcesは、差分のGoのファイル (.go) について、変更前の内容をコミットbaseから、
// 変更後の内容をコミットheadから読み、各Fileに載せる。go_funcs_callingの検知で
// 関数の範囲を取るために使う。
func (d *Diff) ReadGoSources(ctx context.Context, gitDir, base, head string) error {
	for i := range d.Files {
		f := &d.Files[i]
		if f.Status != Added && strings.HasSuffix(f.OldPath, ".go") {
			src, err := git(ctx, gitDir, "cat-file", "blob", "--end-of-options", base+":"+f.OldPath)
			if err != nil {
				return err
			}
			f.OldSource = []byte(src)
		}
		if f.Status != Deleted && strings.HasSuffix(f.Path, ".go") {
			src, err := git(ctx, gitDir, "cat-file", "blob", "--end-of-options", head+":"+f.Path)
			if err != nil {
				return err
			}
			f.NewSource = []byte(src)
		}
	}
	d.GoSourcesRead = true
	return nil
}

// goFuncHitsは、差分の追加・削除行のうち、callsのどれかを呼び出すGoの関数の中にある
// 行を返す。追加行は変更後の内容の、削除行は変更前の内容の関数と照合する。
// テストファイルはテストの準備でトランザクションなどを直接扱うことが多いため、
// testGlobsに当たるファイルは対象にしない。
func goFuncHits(d *Diff, calls, testGlobs []string) []Hit {
	var hits []Hit
	for _, f := range d.Files {
		for _, side := range []struct {
			path    string
			src     []byte
			lines   []Line
			removed bool
		}{{f.Path, f.NewSource, f.Added, false}, {f.OldPath, f.OldSource, f.Removed, true}} {
			if len(side.lines) == 0 || !strings.HasSuffix(side.path, ".go") || matchAny(testGlobs, side.path) {
				continue
			}
			funcs, err := goFuncsCalling(side.path, side.src, calls)
			if err != nil {
				// 関数の範囲を取れないときは、呼び出しの名前が現れるだけで触れたとみなす。
				if slices.ContainsFunc(calls, func(c string) bool { return bytes.Contains(side.src, []byte(c)) }) {
					hits = append(hits, Hit{Path: f.Path})
				}
				continue
			}
			for _, l := range side.lines {
				for _, fn := range funcs {
					if fn.start <= l.No && l.No <= fn.end {
						hits = append(hits, Hit{Path: f.Path, Line: l.No, Removed: side.removed, Text: l.Text, Func: fn.name})
						break
					}
				}
			}
		}
	}
	return hits
}

// goFuncは関数1つの名前と行の範囲 (両端を含む)。
type goFunc struct {
	name       string
	start, end int
}

// goFuncsCallingは、srcの関数のうちcallsのどれかを呼び出すものを返す。メソッドや
// パッケージレベルの変数に代入した関数リテラルも対象にし、関数の中の関数リテラル
// での呼び出しは外側の関数の呼び出しとして数える。
func goFuncsCalling(filename string, src []byte, calls []string) ([]goFunc, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var funcs []goFunc
	add := func(name string, n ast.Node) {
		if callsAny(n, calls) {
			// Gitの差分と照合するため、行ディレクティブの補正を含めず物理行を使う。
			funcs = append(funcs, goFunc{name: name, start: fset.PositionFor(n.Pos(), false).Line, end: fset.PositionFor(n.End(), false).Line})
		}
	}
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			add(funcDeclName(fd), fd)
			continue
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				add("関数リテラル", lit)
				return false
			}
			return true
		})
	}
	return funcs, nil
}

// callsAnyはnの中にcallsのどれかの呼び出しがあるかを返す。`BeginTx(...)` と
// `db.BeginTx(...)` のどちらも、呼び出す名前 (BeginTx) で照合する。
func callsAny(n ast.Node, calls []string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := calleeName(call.Fun); name != "" && slices.Contains(calls, name) {
			found = true
		}
		return !found
	})
	return found
}

// calleeNameは呼び出す式の名前を返す。型引数と括弧は外して読む。
func calleeName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.IndexExpr:
		return calleeName(e.X)
	case *ast.IndexListExpr:
		return calleeName(e.X)
	case *ast.ParenExpr:
		return calleeName(e.X)
	}
	return ""
}

// funcDeclNameは関数の名前を、メソッドなら `型.メソッド` の形で返す。
func funcDeclName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return fmt.Sprintf("%s.%s", recvTypeName(fd.Recv.List[0].Type), fd.Name.Name)
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	case *ast.ParenExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}
