// Package sourcetestは、テストで計画書を一時ファイルから読むsource.Sourceの実装を提供する。
//
// 本番の読み取り元はwikinoコマンドを呼ぶため、テストではこの実装に差し替えて、
// 計画書のファイルを書き換えながらループを進める。
package sourcetest

import (
	"context"
	"fmt"
	"os"

	"github.com/korylus/tools/internal/flow/source"
	"github.com/korylus/tools/internal/plan"
)

// URLは、テストの計画書の指定としてStringが返すURL。
const URL = "https://wikino.app/s/sourcetest/pages/1"

// Fileは、パスPathのファイルから計画書を読む読み取り元。
type File struct {
	Path string
}

var _ source.Source = File{}

// Parseは、コマンドで受け取った指定argをファイルのパスとして読むFileを返す。
// コマンドの読み取り元をテストで差し替えるときに、source.Parseの代わりに渡す。
func Parse(arg string) (source.Source, error) {
	return File{Path: arg}, nil
}

// Readは計画書のファイルを読む。
func (f File) Read(context.Context) (string, error) {
	src, err := os.ReadFile(f.Path)
	if err != nil {
		return "", fmt.Errorf("計画書を読めません: %w", err)
	}
	return string(src), nil
}

// Nameは計画書名の行の値を返す。本番の読み取り元と同じく、行を必須にする。
func (f File) Name(p *plan.Plan) (string, error) {
	if p.Name == "" {
		return "", fmt.Errorf("計画書 %s に計画書名の行がありません", f.Path)
	}
	return p.Name, nil
}

// Stringは、テストの計画書の指定としてURLを返す。
func (f File) String() string {
	return URL
}
