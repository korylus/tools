package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testRepoはテスト用のGitリポジトリ。
type testRepo struct {
	t   *testing.T
	dir string
}

// newTestRepoは空のGitリポジトリを作る。利用者のGitの設定に左右されないよう、
// 署名などを無効にしてコミットする。
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	args = append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", args...) //#nosec G204 -- テストが組み立てたgitの引数
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s のエラー = %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeはファイルを書く。
func (r *testRepo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// commitはすべての変更をコミットし、コミットのIDを返す。
func (r *testRepo) commit() string {
	r.t.Helper()
	r.git("add", "--all")
	r.git("commit", "--quiet", "--allow-empty", "-m", "commit")
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) gitDir() string {
	return filepath.Join(r.dir, ".git")
}

func TestReadDiff(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("keep.go", "package a\n\nfunc A() {}\n\nfunc B() {}\n")
	r.write("gone.txt", "消える\n")
	r.write("move/old.go", "package move\n\n// 名前を変えても内容はほぼ同じ。\nfunc Moved() {}\n\nfunc Other() {}\n")
	r.write("link", "ファイル\n")
	r.write("image.bin", "\x00\x01\x02")
	base := r.commit()

	r.write("keep.go", "package a\n\nfunc A() { _ = 1 }\n\nfunc B() {}\n\nfunc C() {}\n")
	if err := os.Remove(filepath.Join(r.dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "move"), 0o750); err != nil {
		t.Fatal(err)
	}
	r.git("mv", "move/old.go", "move/new.go")
	r.write("move/new.go", "package move\n\n// 名前を変えても内容はほぼ同じ。\nfunc Moved() {}\n\nfunc Other() { _ = 2 }\n")
	r.write("日本語 と空白.md", "# 見出し\n")
	r.write("image.bin", "\x00\x03")
	if err := os.Remove(filepath.Join(r.dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep.go", filepath.Join(r.dir, "link")); err != nil {
		t.Fatal(err)
	}
	head := r.commit()

	d, err := ReadDiff(context.Background(), r.gitDir(), base, head)
	if err != nil {
		t.Fatalf("ReadDiffのエラー = %v", err)
	}

	byPath := map[string]File{}
	for _, f := range d.Files {
		byPath[f.Path] = f
	}
	if len(byPath) != 6 {
		t.Fatalf("ファイル数 = %d、期待値 = 6 (%+v)", len(byPath), d.Files)
	}

	keep := byPath["keep.go"]
	if keep.Status != Modified || keep.OldPath != "keep.go" {
		t.Errorf("keep.go = %+v", keep)
	}
	wantAdded := []Line{{No: 3, Text: "func A() { _ = 1 }"}, {No: 6, Text: ""}, {No: 7, Text: "func C() {}"}}
	if !reflect.DeepEqual(keep.Added, wantAdded) {
		t.Errorf("keep.goの追加行 = %+v、期待値 = %+v", keep.Added, wantAdded)
	}
	wantRemoved := []Line{{No: 3, Text: "func A() {}"}}
	if !reflect.DeepEqual(keep.Removed, wantRemoved) {
		t.Errorf("keep.goの削除行 = %+v、期待値 = %+v", keep.Removed, wantRemoved)
	}

	gone := byPath["gone.txt"]
	if gone.Status != Deleted || gone.OldPath != "gone.txt" || len(gone.Removed) != 1 || len(gone.Added) != 0 {
		t.Errorf("gone.txt = %+v", gone)
	}

	moved := byPath["move/new.go"]
	if moved.Status != Renamed || moved.OldPath != "move/old.go" {
		t.Errorf("move/new.go = %+v", moved)
	}
	if got := moved.Paths(); !reflect.DeepEqual(got, []string{"move/old.go", "move/new.go"}) {
		t.Errorf("move/new.goのPaths() = %v", got)
	}
	if len(moved.Added) != 1 || moved.Added[0].No != 6 {
		t.Errorf("move/new.goの追加行 = %+v", moved.Added)
	}

	jp := byPath["日本語 と空白.md"]
	if jp.Status != Added || jp.OldPath != "" || len(jp.Added) != 1 {
		t.Errorf("日本語 と空白.md = %+v", jp)
	}

	if bin := byPath["image.bin"]; !bin.Binary || !reflect.DeepEqual(bin.Added, []Line{{No: 1, Text: "\x00\x03"}}) ||
		!reflect.DeepEqual(bin.Removed, []Line{{No: 1, Text: "\x00\x01\x02"}}) {
		t.Errorf("image.bin = %+v", bin)
	}

	// 種類の変更はパッチで削除と追加に分かれるが、1ファイルとして読む。
	link := byPath["link"]
	if link.Status != TypeChanged || len(link.Removed) != 1 || len(link.Added) != 1 {
		t.Errorf("link = %+v", link)
	}
}

func TestReadDiff_NoChanges(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	base := r.commit()
	head := r.commit()

	d, err := ReadDiff(context.Background(), r.gitDir(), base, head)
	if err != nil {
		t.Fatalf("ReadDiffのエラー = %v", err)
	}
	if len(d.Files) != 0 {
		t.Errorf("ファイル = %+v、空を期待", d.Files)
	}
}

func TestReadDiff_UnknownCommit(t *testing.T) {
	t.Parallel()

	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	base := r.commit()

	if _, err := ReadDiff(context.Background(), r.gitDir(), base, "--output=/tmp/x"); err == nil {
		t.Error("オプションに見えるコミットでエラーを期待したが、nilだった")
	}
}
