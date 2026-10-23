package gate

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/plan"
)

func TestGoFuncsCalling(t *testing.T) {
	t.Parallel()

	src := `package usecase

func Plain() {
	tx, _ := BeginTx()
	_ = tx
}

func (u *PostUsecase) Create(ctx context.Context) error {
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r Repo[T]) Generic() {
	(BeginTx[T])()
}

func Closure() {
	run(func() {
		db.BeginTx(nil, nil)
	})
}

func NoTx() {
	db.Query()
}

var lit = func() {
	db.BeginTx(nil, nil)
}

// 名前を参照するだけで呼び出さない関数は対象にしない。
func Reference() {
	f := db.BeginTx
	_ = f
}
`
	got, err := goFuncsCalling("usecase.go", []byte(src), []string{"BeginTx"})
	if err != nil {
		t.Fatalf("goFuncsCallingのエラー = %v", err)
	}
	want := []goFunc{
		{name: "Plain", start: 3, end: 6},
		{name: "PostUsecase.Create", start: 8, end: 14},
		{name: "Repo.Generic", start: 16, end: 18},
		{name: "Closure", start: 20, end: 24},
		{name: "関数リテラル", start: 30, end: 32},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("goFuncsCalling = %+v、期待値 = %+v", got, want)
	}
}

func TestGoFuncHits_LineDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive string
	}{
		{name: "行コメント", directive: "//line original.go:1000"},
		{name: "ブロックコメント", directive: "/*line original.go:1000*/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := strings.Join([]string{
				"package usecase",
				tt.directive,
				"func Create() {",
				"\tBeginTx()",
				"\twork()",
				"}",
				"",
				"func List() {",
				"\tquery()",
				"}",
				"",
			}, "\n")
			lines := []Line{{No: 5, Text: "\twork()"}, {No: 9, Text: "\tquery()"}}
			diff := &Diff{GoSourcesRead: true, Files: []File{{
				Path: "post.go", OldPath: "post.go", Status: Modified,
				OldSource: []byte(src), NewSource: []byte(src),
				Added: lines, Removed: lines,
			}}}
			got := goFuncHits(diff, []string{"BeginTx"}, nil)
			want := []Hit{
				{Path: "post.go", Line: 5, Text: "\twork()", Func: "Create"},
				{Path: "post.go", Line: 5, Removed: true, Text: "\twork()", Func: "Create"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("goFuncHits = %+v、期待値 = %+v", got, want)
			}
		})
	}
}

func TestInspect_GoFuncsCalling(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("go/internal/usecase/post.go", `package usecase

func Create() {
	tx := db.BeginTx()
	tx.Commit()
}

func Update() {
	tx := db.BeginTx()
	tx.Exec("a")
	tx.Commit()
}

func List() {
	db.Query()
}
`)
	r.write("go/internal/usecase/post_test.go", `package usecase

func TestCreate(t *testing.T) {
	tx := db.BeginTx()
	tx.Rollback()
}
`)
	r.write("go/internal/usecase/broken.go", "package usecase\n")
	base := r.commit()
	r.write("go/internal/usecase/post.go", `package usecase

func Create() {
	tx := db.BeginTx()
	tx.Exec("追加")
	tx.Commit()
}

func Update() {
	tx := db.Begin()
	tx.Commit()
}

func List() {
	db.Query("変更")
}
`)
	r.write("go/internal/usecase/post_test.go", `package usecase

func TestCreate(t *testing.T) {
	tx := db.BeginTx()
	tx.Exec("テストの準備")
	tx.Rollback()
}
`)
	// 構文の誤りで関数の範囲を取れないファイル。
	r.write("go/internal/usecase/broken.go", "package usecase\n\nfunc Broken() {\n\tdb.BeginTx(\n")
	head := r.commit()

	diff, err := ReadDiff(context.Background(), r.gitDir(), base, head)
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadTestConfig(t)
	cfg.Protected[2].Patterns = nil
	cfg.Protected[2].GoFuncsCalling = []string{"BeginTx"}

	if _, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-5"}, Diff: diff}); err == nil {
		t.Error("Goのファイルの内容を読んでいない差分でエラーを期待したが、nilだった")
	}

	if err := diff.ReadGoSources(context.Background(), r.gitDir(), base, head); err != nil {
		t.Fatalf("ReadGoSourcesのエラー = %v", err)
	}
	report, err := Inspect(Input{Config: cfg, Task: plan.Task{ID: "2-5"}, Diff: diff})
	if err != nil {
		t.Fatalf("Inspectのエラー = %v", err)
	}
	if len(report.Touches) != 1 || report.Touches[0].Name != "transaction" {
		t.Fatalf("Touches = %+v、transactionだけを期待", report.Touches)
	}
	type hit struct {
		path    string
		line    int
		removed bool
		fn      string
	}
	var got []hit
	for _, h := range report.Touches[0].Hits {
		got = append(got, hit{h.Path, h.Line, h.Removed, h.Func})
	}
	// Listの変更とテストファイルの変更は当たらない。UpdateはBeginTxを消した変更を
	// 変更前の関数で拾い、変更後の関数 (BeginTxを呼ばない) での追加行は拾わない。
	want := []hit{
		{"go/internal/usecase/broken.go", 0, false, ""},
		{"go/internal/usecase/post.go", 5, false, "Create"},
		{"go/internal/usecase/post.go", 9, true, "Update"},
		{"go/internal/usecase/post.go", 10, true, "Update"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Hits = %+v、期待値 = %+v", got, want)
	}
	stops := findingsOf(report, KindProtected)
	if len(stops) != 1 || !strings.Contains(stops[0], "post.go:5 (Create)") {
		t.Errorf("止める理由 = %v、関数名を含む1件を期待", stops)
	}
}

func TestReadGoSources(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("a.go", "package a // 変更前\n")
	r.write("gone.go", "package gone\n")
	r.write("notes.txt", "メモ\n")
	base := r.commit()
	r.write("a.go", "package a // 変更後\n")
	r.git("rm", "--quiet", "gone.go")
	r.write("new.go", "package a // 追加\n")
	r.write("notes.txt", "メモを変更\n")
	head := r.commit()

	diff, err := ReadDiff(context.Background(), r.gitDir(), base, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := diff.ReadGoSources(context.Background(), r.gitDir(), base, head); err != nil {
		t.Fatalf("ReadGoSourcesのエラー = %v", err)
	}
	want := map[string][2]string{
		"a.go":      {"package a // 変更前\n", "package a // 変更後\n"},
		"gone.go":   {"package gone\n", ""},
		"new.go":    {"", "package a // 追加\n"},
		"notes.txt": {"", ""},
	}
	for _, f := range diff.Files {
		if got := [2]string{string(f.OldSource), string(f.NewSource)}; got != want[f.Path] {
			t.Errorf("%s の内容 = %q、期待値 = %q", f.Path, got, want[f.Path])
		}
	}
}
