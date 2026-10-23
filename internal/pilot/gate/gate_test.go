package gate

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/plan"
)

// roleConfigは、検査では使わないが設定ファイルに必須の、役割とハブの設定。
// トップレベルのキーの後ろに付ける。
const roleConfig = `
[hub]
upstream = "https://github.com/mewstcom/mewst.git"
path = "/koryluspilot/hub.git"

[roles.designer]
tool = "claude"
runner = ["designer"]

[roles.implementer]
tool = "claude"
runner = ["implementer"]

[roles.reviewer_codex]
tool = "codex"
runner = ["reviewer-codex"]

[roles.reviewer_claude]
tool = "claude"
runner = ["reviewer-claude"]
`

// testConfigは作業計画書の設定例に沿った、検査に使う設定。
const testConfig = `
project = "mewst"
base_branch = "develop"
reviewers = ["reviewer_codex", "reviewer_claude"]

[[protected]]
name = "schema"
label = "DBスキーマ・マイグレーション"
paths = ["go/db/migrations/**", "go/db/schema.sql"]

[[protected]]
name = "dependency"
label = "依存パッケージ"
paths = ["go/go.mod"]
on_undeclared = "report"

[[protected]]
name = "transaction"
label = "トランザクション境界"
patterns = ['BeginTx\(', '\.Commit\(\)']

[gate]
suppress_patterns = ['//nolint', 't\.Skip\(']
test_globs = ["**/*_test.go", "rails/spec/**"]
generated_globs = ["**/*_templ.go"]
size_factor = 2.0
` + roleConfig

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse(testConfig)
	if err != nil {
		t.Fatalf("config.Parseのエラー = %v", err)
	}
	return cfg
}

// addedは追加されたファイルの差分を作る。
func added(path string, lines ...string) File {
	f := File{Path: path, Status: Added}
	for i, l := range lines {
		f.Added = append(f.Added, Line{No: i + 1, Text: l})
	}
	return f
}

// modifiedは変更されたファイルの差分を作る。追加行・削除行とも1行目からの番号を振る。
func modified(path string, add, remove []string) File {
	f := File{Path: path, OldPath: path, Status: Modified}
	for i, l := range add {
		f.Added = append(f.Added, Line{No: i + 1, Text: l})
	}
	for i, l := range remove {
		f.Removed = append(f.Removed, Line{No: i + 1, Text: l})
	}
	return f
}

func deleted(path string, lines ...string) File {
	f := File{Path: path, OldPath: path, Status: Deleted}
	for i, l := range lines {
		f.Removed = append(f.Removed, Line{No: i + 1, Text: l})
	}
	return f
}

func findingsOf(r *Report, kind Kind) []string {
	var msgs []string
	for _, f := range r.Findings {
		if f.Kind == kind {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
}

func TestInspect_Protected(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	diff := &Diff{Files: []File{
		added("go/db/migrations/20261020_add_visibility.sql", "ALTER TABLE posts ADD COLUMN visibility text;"),
		modified("go/go.mod", []string{"require example.com/x v1.0.0"}, nil),
		modified("go/internal/usecase/post.go", []string{"tx, err := db.BeginTx(ctx, nil)"}, []string{"return tx.Commit()"}),
		modified("README.md", []string{"説明"}, nil),
	}}

	tests := []struct {
		name         string
		declared     []plan.Declaration
		wantTouches  []string // 触れた保護領域の名前
		wantDeclared []bool
		wantStops    []string // 止める理由に含まれることを期待する保護領域の名前
	}{
		{
			name:         "宣言が無い",
			wantTouches:  []string{"schema", "dependency", "transaction"},
			wantDeclared: []bool{false, false, false},
			// dependencyはon_undeclared = "report" なので止めない。
			wantStops: []string{"schema", "transaction"},
		},
		{
			name:         "すべて宣言している",
			declared:     []plan.Declaration{{Name: "schema", Note: "postsにvisibility列を追加"}, {Name: "transaction"}, {Name: "dependency"}},
			wantTouches:  []string{"schema", "dependency", "transaction"},
			wantDeclared: []bool{true, true, true},
		},
		{
			name:         "一部だけ宣言している",
			declared:     []plan.Declaration{{Name: "schema"}},
			wantTouches:  []string{"schema", "dependency", "transaction"},
			wantDeclared: []bool{true, false, false},
			wantStops:    []string{"transaction"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1", Protected: tt.declared}, Diff: diff})
			if err != nil {
				t.Fatalf("Inspectのエラー = %v", err)
			}
			var names []string
			var decl []bool
			for _, touch := range r.Touches {
				names = append(names, touch.Name)
				decl = append(decl, touch.Declared)
			}
			if !reflect.DeepEqual(names, tt.wantTouches) {
				t.Errorf("触れた保護領域 = %v、期待値 = %v", names, tt.wantTouches)
			}
			if !reflect.DeepEqual(decl, tt.wantDeclared) {
				t.Errorf("宣言の有無 = %v、期待値 = %v", decl, tt.wantDeclared)
			}
			stops := findingsOf(r, KindProtected)
			if len(stops) != len(tt.wantStops) {
				t.Fatalf("止める理由 = %q、期待する保護領域 = %v", stops, tt.wantStops)
			}
			for i, w := range tt.wantStops {
				if !strings.Contains(stops[i], "保護領域 "+w+" ") {
					t.Errorf("止める理由[%d] = %q、含まれることを期待した保護領域 = %q", i, stops[i], w)
				}
			}
			if r.Passed() != (len(tt.wantStops) == 0) {
				t.Errorf("Passed() = %v", r.Passed())
			}
		})
	}
}

func TestInspect_ProtectedHits(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	diff := &Diff{Files: []File{
		// 名前の変更では、変更前のパスでも保護領域に当たる。
		{Path: "go/db/old_schema.sql", OldPath: "go/db/schema.sql", Status: Renamed},
		modified("go/internal/usecase/post.go", []string{"tx, err := db.BeginTx(ctx, nil)"}, []string{"return tx.Commit()"}),
	}}
	r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1"}, Diff: diff})
	if err != nil {
		t.Fatalf("Inspectのエラー = %v", err)
	}
	want := []Touch{
		{Name: "schema", Label: "DBスキーマ・マイグレーション", Hits: []Hit{{Path: "go/db/old_schema.sql"}}},
		{Name: "transaction", Label: "トランザクション境界", Hits: []Hit{
			{Path: "go/internal/usecase/post.go", Line: 1, Text: "tx, err := db.BeginTx(ctx, nil)"},
			{Path: "go/internal/usecase/post.go", Line: 1, Removed: true, Text: "return tx.Commit()"},
		}},
	}
	if !reflect.DeepEqual(r.Touches, want) {
		t.Errorf("Touches = %+v、期待値 = %+v", r.Touches, want)
	}
	if stops := findingsOf(r, KindProtected); len(stops) != 2 || !strings.Contains(stops[1], "go/internal/usecase/post.go:1, go/internal/usecase/post.go:1") {
		t.Errorf("止める理由 = %q", stops)
	}
}

func TestInspect_Suppress(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	diff := &Diff{Files: []File{
		modified("go/a.go", []string{"x := 1 //nolint:errcheck"}, []string{"y := 2 //nolint:errcheck"}),
		modified("go/a_test.go", []string{"\tt.Skip(\"あとで直す\")"}, nil),
	}}
	r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1"}, Diff: diff})
	if err != nil {
		t.Fatalf("Inspectのエラー = %v", err)
	}
	// 削除した行の抑制は止めない。
	got := findingsOf(r, KindSuppress)
	want := []string{
		`go/a.go:1: 抑制のパターン "//nolint" を含む行が追加されています: x := 1 //nolint:errcheck`,
		`go/a_test.go:1: 抑制のパターン "t\\.Skip\\(" を含む行が追加されています: t.Skip("あとで直す")`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("抑制の検知 = %q、期待値 = %q", got, want)
	}
}

func TestInspect_TestDeletion(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	tests := []struct {
		name  string
		files []File
		want  []string
	}{
		{
			name:  "テストファイルの削除",
			files: []File{deleted("rails/spec/models/user_spec.rb", "describe User do")},
			want:  []string{"テストファイル rails/spec/models/user_spec.rb が削除されています"},
		},
		{
			name:  "テストファイルをテストではないパスへ移す",
			files: []File{{Path: "go/a/helper.go", OldPath: "go/a/helper_test.go", Status: Renamed}},
			want:  []string{"テストファイル go/a/helper_test.go がテストではないパス go/a/helper.go に移されています"},
		},
		{
			name:  "テストファイル同士の名前の変更",
			files: []File{{Path: "go/a/b_test.go", OldPath: "go/a/a_test.go", Status: Renamed}},
		},
		{
			name: "テスト関数の削除",
			files: []File{modified("go/a/a_test.go", []string{"func TestKeep(t *testing.T) {"}, []string{
				"func TestKeep(t *testing.T) {",
				"func TestGone(t *testing.T) {",
				"func BenchmarkGone(b *testing.B) {",
				"func helper(t *testing.T) {",
			})},
			want: []string{
				"テスト関数 TestGone が go/a/a_test.go から削除されています",
				"テスト関数 BenchmarkGone が go/a/a_test.go から削除されています",
			},
		},
		{
			name: "同じパッケージの別のファイルへのテスト関数の移動",
			files: []File{
				modified("go/a/a_test.go", nil, []string{"func TestMoved(t *testing.T) {"}),
				added("go/a/b_test.go", "func TestMoved(t *testing.T) {"),
			},
		},
		{
			name: "別のパッケージへのテスト関数の移動は削除とみなす",
			files: []File{
				modified("go/a/a_test.go", nil, []string{"func TestMoved(t *testing.T) {"}),
				added("go/b/b_test.go", "func TestMoved(t *testing.T) {"),
			},
			want: []string{"テスト関数 TestMoved が go/a/a_test.go から削除されています"},
		},
		{
			name:  "テストではないファイルの関数の削除",
			files: []File{modified("go/a/a.go", nil, []string{"func TestLike() {"})},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1"}, Diff: &Diff{Files: tt.files}})
			if err != nil {
				t.Fatalf("Inspectのエラー = %v", err)
			}
			if got := findingsOf(r, KindTestDeletion); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("テストの削除の検知 = %q、期待値 = %q", got, tt.want)
			}
		})
	}
}

func TestInspect_Size(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	lines := func(n int) []string {
		s := make([]string, n)
		for i := range s {
			s[i] = "x"
		}
		return s
	}
	files := []File{
		modified("go/a.go", lines(150), lines(51)),
		added("go/a_test.go", lines(1000)...),
		added("go/view_templ.go", lines(1000)...),
		deleted("go/old.go", lines(1000)...),
	}

	tests := []struct {
		name string
		size plan.Size
		want bool // 超過を検知することを期待するか
	}{
		{"テストを除いた想定行数の2倍を超える", plan.Size{Total: 200, Parts: map[string]int{"実装": 100, "テスト": 100}}, true},
		{"テストを除いた想定行数の2倍以内", plan.Size{Total: 201, Parts: map[string]int{"実装": 101, "テスト": 100}}, false},
		{"内訳が無ければ合計と比べる", plan.Size{Total: 101}, false},
		{"想定行数の記載が無ければ検査しない", plan.Size{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1", Lines: tt.size}, Diff: &Diff{Files: files}})
			if err != nil {
				t.Fatalf("Inspectのエラー = %v", err)
			}
			if r.ImplLines != 201 {
				t.Errorf("ImplLines = %d、期待値 = 201", r.ImplLines)
			}
			got := findingsOf(r, KindSize)
			if (len(got) > 0) != tt.want {
				t.Errorf("行数の超過の検知 = %q、検知を期待 = %v", got, tt.want)
			}
		})
	}
}

func TestInspect_Plan(t *testing.T) {
	t.Parallel()

	cfg := loadTestConfig(t)
	snapshot := "## 設計\n\n方針\n\n## タスクリスト\n\n- [ ] **2-1**: [Go] タスク\n"

	tests := []struct {
		name     string
		snapshot string
		current  string
		want     []string
	}{
		{"チェックだけ", snapshot, strings.Replace(snapshot, "[ ]", "[x]", 1), nil},
		{"設計節の書き換え", snapshot, strings.Replace(snapshot, "方針", "別の方針", 1), []string{"計画書の許可外の変更: 3行目: 「設計」節が変更されています"}},
		{"スナップショットが無ければ検査しない", "", "何でも", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-1"}, Diff: &Diff{}, PlanSnapshot: tt.snapshot, PlanCurrent: tt.current})
			if err != nil {
				t.Fatalf("Inspectのエラー = %v", err)
			}
			if got := findingsOf(r, KindPlan); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("計画書の変更の検知 = %q、期待値 = %q", got, tt.want)
			}
		})
	}
}

func TestInspect_TestFileRename(t *testing.T) {
	t.Parallel()

	const source = "package example\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tt.Log(\"最初のテスト\")\n}\n\nfunc TestTwo(t *testing.T) {\n\tt.Log(\"次のテスト\")\n}\n"
	tests := []struct {
		name     string
		dest     string
		edit     bool
		wantStop bool
	}{
		{name: "同じディレクトリで内容を保って移動", dest: "tests/b_test.go"},
		{name: "同じディレクトリで内容を変えて移動", dest: "tests/b_test.go", edit: true},
		{name: "別ディレクトリへ内容を保って移動", dest: "other/b_test.go", wantStop: true},
		{name: "別ディレクトリへ内容を変えて移動", dest: "other/b_test.go", edit: true, wantStop: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRepo(t)
			r.write("tests/a_test.go", source)
			r.write("other/keep.go", "package example\n")
			base := r.commit()
			r.git("mv", "tests/a_test.go", tt.dest)
			if tt.edit {
				r.write(tt.dest, strings.Replace(source, "最初のテスト", "変更したテスト", 1))
			}
			head := r.commit()
			diff, err := ReadDiff(context.Background(), r.gitDir(), base, head)
			if err != nil {
				t.Fatal(err)
			}
			if len(diff.Files) != 1 || diff.Files[0].Status != Renamed {
				t.Fatalf("名前変更の差分を期待したが、差分 = %+v", diff.Files)
			}
			if got := len(diff.Files[0].Added) > 0; got != tt.edit {
				t.Fatalf("追加行の有無 = %v、期待値 = %v", got, tt.edit)
			}
			report, err := Inspect(Input{Config: loadTestConfig(t), Task: plan.Task{ID: "2-4"}, Diff: diff})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(findingsOf(report, KindTestDeletion)) > 0; got != tt.wantStop {
				t.Errorf("テスト削除の検知 = %v、期待値 = %v、検知結果 = %+v", got, tt.wantStop, report.Findings)
			}
		})
	}
}

func TestInspect_NULContainingScript(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("check.sh", "#!/bin/sh\n# tx.Commit()\ntrue\n")
	base := r.commit()
	r.write("check.sh", "#!/bin/sh\n# \x00\n# tx.BeginTx(\n# shellcheck disable=SC2086\ntrue\n")
	head := r.commit()
	// ハブと同じbareリポジトリから読み、作業ツリーの属性に依存しない検査を確かめる。
	hub := t.TempDir()
	r.git("clone", "--quiet", "--bare", r.dir, hub)
	diff, err := ReadDiff(context.Background(), hub, base, head)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadTestConfig(t)
	cfg.Gate.SuppressPatterns = []string{`shellcheck disable`}
	report, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-4"}, Diff: diff})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []Kind{KindProtected, KindSuppress} {
		if got := findingsOf(report, kind); len(got) != 1 {
			t.Errorf("%sの検知 = %v、1件を期待", kind, got)
		}
	}
	if len(report.Touches) != 1 || len(report.Touches[0].Hits) != 2 {
		t.Fatalf("保護パターンの追加・削除の検知 = %+v、2件を期待", report.Touches)
	}
	if report.Touches[0].Hits[0].Removed || !report.Touches[0].Hits[1].Removed {
		t.Errorf("保護パターンの追加・削除の区別 = %+v", report.Touches[0].Hits)
	}
}

func TestInspect_BinaryAsset(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("go/a.go", "package a\n")
	base := r.commit()
	r.write("go/a.go", "package a\n\nfunc A() {}\n")
	// 改行を多く含むバイナリ資産。抑制パターンに当たる内容も含める。
	r.write("go/static/og-image.png", "\x89PNG\x00\n//nolint\n"+strings.Repeat("\x00\x01\n", 500))
	head := r.commit()
	diff, err := ReadDiff(context.Background(), r.gitDir(), base, head)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(Input{
		Config: loadTestConfig(t),
		Task:   plan.Task{ID: "2-4", Lines: plan.Size{Total: 1}},
		Diff:   diff,
	})
	if err != nil {
		t.Fatal(err)
	}
	// バイナリ資産の行は数えず、go/a.goの追加2行だけを実装の行数にする。
	if report.ImplLines != 2 {
		t.Errorf("ImplLines = %d、期待値 = 2", report.ImplLines)
	}
	if got := findingsOf(report, KindSize); len(got) != 0 {
		t.Errorf("行数の超過の検知 = %v、検知しないことを期待", got)
	}
	// 行数から除いても、行の検査には残す。
	if got := findingsOf(report, KindSuppress); len(got) != 1 || !strings.Contains(got[0], "og-image.png") {
		t.Errorf("抑制の検知 = %v、og-image.pngの1件を期待", got)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	g := config.Gate{TestGlobs: []string{"**/*_test.go"}, GeneratedGlobs: []string{"**/*_templ.go"}}
	tests := []struct {
		name string
		file File
		want FileKind
	}{
		{"実装", File{Path: "a/b.go", Status: Modified}, FileImpl},
		{"テスト", File{Path: "a/b_test.go", Status: Added}, FileTest},
		{"生成物", File{Path: "a/b_templ.go", Status: Modified}, FileGenerated},
		{"バイナリ", File{Path: "a/logo.png", Status: Added, Binary: true}, FileBinary},
		{"削除はテストより先に判定する", File{Path: "a/b_test.go", Status: Deleted}, FileDeleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(g, tt.file); got != tt.want {
				t.Errorf("Classify = %q、期待値 = %q", got, tt.want)
			}
		})
	}
}

func TestExpectedImplLines(t *testing.T) {
	t.Parallel()

	if got := ExpectedImplLines(plan.Size{Total: 300, Parts: map[string]int{"実装": 150, "テスト": 150}}); got != 150 {
		t.Errorf("テストを除いた行数 = %d、期待値 = 150", got)
	}
	if got := ExpectedImplLines(plan.Size{}); got != 0 {
		t.Errorf("記載が無いときの行数 = %d、期待値 = 0", got)
	}
}
