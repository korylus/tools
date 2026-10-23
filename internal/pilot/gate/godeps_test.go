package gate

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/plan"
)

func TestParseGoList(t *testing.T) {
	t.Parallel()

	out := `{
	"ImportPath": "example.com/m/internal/a",
	"Imports": [
		"example.com/m/internal/b",
		"fmt"
	]
}
{
	"ImportPath": "example.com/m/internal/b"
}
`
	got, err := parseGoList([]byte(out))
	if err != nil {
		t.Fatalf("parseGoListのエラー = %v", err)
	}
	want := []GoPackage{
		{ImportPath: "example.com/m/internal/a", Imports: []string{"example.com/m/internal/b", "fmt"}},
		{ImportPath: "example.com/m/internal/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGoList = %+v、期待値 = %+v", got, want)
	}

	if _, err := parseGoList([]byte(`{"ImportPath": `)); err == nil {
		t.Error("途中で切れた出力でエラーを期待したが、nilだった")
	}
}

func TestNewEdges(t *testing.T) {
	t.Parallel()

	const prefix = "example.com/m/internal/"
	deps := &GoDeps{
		Base: []GoPackage{
			{ImportPath: prefix + "handler", Imports: []string{prefix + "usecase", "net/http"}},
			{ImportPath: prefix + "usecase", Imports: []string{prefix + "repository"}},
			{ImportPath: prefix + "repository"},
		},
		Head: []GoPackage{
			// 既存の辺と、配下の外への依存は数えない。
			{ImportPath: prefix + "handler", Imports: []string{prefix + "usecase", prefix + "repository", "example.com/other/internal/x"}},
			{ImportPath: prefix + "usecase", Imports: []string{prefix + "repository"}},
			{ImportPath: prefix + "repository"},
			// 新しいパッケージからの依存は増えた辺として数える。
			{ImportPath: prefix + "worker", Imports: []string{prefix + "usecase"}},
			// 配下の外からの依存は数えない。
			{ImportPath: "example.com/m/cmd/server", Imports: []string{prefix + "worker"}},
		},
	}
	got := newEdges(deps, prefix)
	want := []Edge{
		{From: prefix + "handler", To: prefix + "repository"},
		{From: prefix + "worker", To: prefix + "usecase"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("newEdges = %+v、期待値 = %+v", got, want)
	}
}

func TestInspect_GoImportPrefix(t *testing.T) {
	t.Parallel()

	const prefix = "example.com/m/internal/"
	cfg := loadTestConfig(t)
	cfg.Protected[0].Paths = nil
	cfg.Protected[0].Name = "module-dependency"
	cfg.Protected[0].Label = "モジュール間の依存関係"
	cfg.Protected[0].GoImportPrefix = prefix
	diff := &Diff{Files: []File{modified("go/internal/handler/post.go", []string{`"example.com/m/internal/repository"`}, nil)}}

	if _, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-5"}, Diff: diff}); err == nil {
		t.Error("パッケージの一覧が無い入力でエラーを期待したが、nilだった")
	}

	deps := &GoDeps{
		Base: []GoPackage{{ImportPath: prefix + "handler"}, {ImportPath: prefix + "repository"}},
		Head: []GoPackage{{ImportPath: prefix + "handler", Imports: []string{prefix + "repository"}}, {ImportPath: prefix + "repository"}},
	}
	report, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-5"}, Diff: diff, GoDeps: deps})
	if err != nil {
		t.Fatalf("Inspectのエラー = %v", err)
	}
	if len(report.Touches) != 1 || report.Touches[0].Name != "module-dependency" || len(report.Touches[0].Hits) != 1 {
		t.Fatalf("Touches = %+v、module-dependencyの1件を期待", report.Touches)
	}
	if e := report.Touches[0].Hits[0].Edge; e == nil || *e != (Edge{From: prefix + "handler", To: prefix + "repository"}) {
		t.Errorf("Edge = %+v", e)
	}
	stops := findingsOf(report, KindProtected)
	if len(stops) != 1 || !strings.Contains(stops[0], "依存 "+prefix+"handler → "+prefix+"repository") {
		t.Errorf("止める理由 = %v、依存辺を含む1件を期待", stops)
	}

	// 宣言していれば止めない。
	declared := plan.Task{ID: "2-5", Protected: []plan.Declaration{{Name: "module-dependency"}}}
	report, err = Inspect(Input{Config: cfg, Task: declared, Diff: diff, GoDeps: deps})
	if err != nil {
		t.Fatalf("Inspectのエラー = %v", err)
	}
	if !report.Passed() || len(report.Touches) != 1 || !report.Touches[0].Declared {
		t.Errorf("宣言したときの結果 = %+v、止めずに触れた領域を残すことを期待", report)
	}
}

func TestCommandRunner_GoPackages(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("goコマンドが無い環境では実行できない")
	}

	r := newTestRepo(t)
	r.write("go/go.mod", "module example.com/m\n\ngo 1.22\n")
	r.write("go/internal/a/a.go", "package a\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/m/internal/b\"\n)\n\nvar _ = fmt.Sprint(b.B)\n")
	r.write("go/internal/a/a_test.go", "package a\n\nimport \"example.com/m/internal/c\"\n\nvar _ = c.C\n")
	r.write("go/internal/b/b.go", "package b\n\nconst B = 1\n")
	r.write("go/internal/c/c.go", "package c\n\nconst C = 1\n")
	// ルートのモジュールと、それに含まれない入れ子のモジュール。
	r.write("go.mod", "module example.com/root\n\ngo 1.22\n")
	r.write("root.go", "package root\n")
	commit := r.commit()
	// 作業ツリーにだけある変更は読まない。
	r.write("go/internal/b/b.go", "package b\n\nimport \"example.com/m/internal/c\"\n\nconst B = c.C\n")

	runner := CommandRunner{Repo: r.dir}
	got, err := runner.GoPackages(context.Background(), commit)
	if err != nil {
		t.Fatalf("GoPackagesのエラー = %v", err)
	}
	// テストのimport (a → c) は含めない。
	want := []GoPackage{
		{ImportPath: "example.com/root"},
		{ImportPath: "example.com/m/internal/a", Imports: []string{"example.com/m/internal/b", "fmt"}},
		{ImportPath: "example.com/m/internal/b"},
		{ImportPath: "example.com/m/internal/c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoPackages = %+v、期待値 = %+v", got, want)
	}

	if _, err := runner.GoPackages(context.Background(), strings.Repeat("0", 40)); err == nil {
		t.Error("存在しないコミットでエラーを期待したが、nilだった")
	}
}
