package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// goListScriptは、ゲートのコマンドと同じ取り直したチェックアウトの中で、すべての
// Goのモジュール (go.modのあるディレクトリ) のパッケージとimportを一覧にする
// シェルスクリプト。checkoutScriptの $3 として渡す。テストのimportは含めない。
const goListScript = `git ls-files -z -- ':(glob)**/go.mod' | xargs -0 -r -n 1 dirname | while IFS= read -r d; do
  (cd "$d" && go list -e -json=ImportPath,Imports ./...) || exit 1
done`

// GoPackageはgo listで読んだパッケージ1つ。
type GoPackage struct {
	ImportPath string
	Imports    []string
}

// GoDepsは差分の両端のコミットのパッケージの一覧。go_import_prefixの検知に使う。
type GoDeps struct {
	Base []GoPackage
	Head []GoPackage
}

// Edgeはパッケージ間の依存辺1つ (FromがToをimportする)。
type Edge struct {
	From string
	To   string
}

func (e Edge) String() string {
	return e.From + " → " + e.To
}

// GoPackagesは、ゲートのランナーで、ハブから取り直したコミットcommitのチェックアウトに
// 対してgo listを実行し、パッケージとimportの一覧を返す。go listはモジュールの
// コードを読むため、ゲートのコマンドと同じくVM上では実行しない。
func (r CommandRunner) GoPackages(ctx context.Context, commit string) ([]GoPackage, error) {
	var stdout, stderr bytes.Buffer
	res, err := r.runCheckout(ctx, commit, goListScript, &stdout, &stderr)
	if err != nil {
		return nil, err
	}
	if !res.Passed() {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > outputTailMax {
			tail = tail[len(tail)-outputTailMax:]
		}
		if res.TimedOut {
			return nil, fmt.Errorf("コミット %s のgo listがタイムアウトしました: %s", commit, tail)
		}
		return nil, fmt.Errorf("コミット %s のgo listが失敗しました (終了コード %d): %s", commit, res.ExitCode, tail)
	}
	return parseGoList(stdout.Bytes())
}

// parseGoListは `go list -json` の出力 (JSONの値の連続) を読む。
func parseGoList(out []byte) ([]GoPackage, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var pkgs []GoPackage
	for {
		var p GoPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return pkgs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("go listの出力を読み取れません: %w", err)
		}
		pkgs = append(pkgs, p)
	}
}

// newEdgesは、両端がprefix配下にある依存辺のうち、baseに無くheadで増えたものを
// 並べ替えて返す。新しく作られたパッケージからの依存も、増えた辺として数える。
func newEdges(deps *GoDeps, prefix string) []Edge {
	baseEdges := map[Edge]bool{}
	for _, e := range edgesUnder(deps.Base, prefix) {
		baseEdges[e] = true
	}
	var res []Edge
	for _, e := range edgesUnder(deps.Head, prefix) {
		if !baseEdges[e] {
			res = append(res, e)
		}
	}
	slices.SortFunc(res, func(a, b Edge) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		return strings.Compare(a.To, b.To)
	})
	return slices.Compact(res)
}

func edgesUnder(pkgs []GoPackage, prefix string) []Edge {
	var edges []Edge
	for _, p := range pkgs {
		if !strings.HasPrefix(p.ImportPath, prefix) {
			continue
		}
		for _, imp := range p.Imports {
			if strings.HasPrefix(imp, prefix) && imp != p.ImportPath {
				edges = append(edges, Edge{From: p.ImportPath, To: imp})
			}
		}
	}
	return edges
}
