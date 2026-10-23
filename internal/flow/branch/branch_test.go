package branch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newRepoは、docs/private/ を無視するリポジトリを作り、そのルートを返す。
// macOSのt.TempDirはシンボリックリンクを含むため、gitが返すパスと揃うよう実体のパスにする。
func newRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"go", "docs/private/plans"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("docs/private/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", root) //#nosec G204 -- テストが組み立てたgitの引数
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return root
}

func TestWorkDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prefix    string
		wantDir   string
		wantHuman bool
	}{
		{name: "パスはそのディレクトリ", prefix: "/workspace/go", wantDir: "/workspace/go"},
		{name: "パスの末尾のスラッシュを除く", prefix: "/korylus-tools/", wantDir: "/korylus-tools"},
		{name: "[Go]は/workspace/go", prefix: "Go", wantDir: "/workspace/go"},
		{name: "[Rails]は/workspace/rails", prefix: "Rails", wantDir: "/workspace/rails"},
		{name: "接頭辞無しは/workspace", prefix: "", wantDir: "/workspace"},
		{name: "名前は人が行うタスク", prefix: "shimbaco", wantHuman: true},
		{name: "知らない接頭辞は人が行うタスク", prefix: "Mewst", wantHuman: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, human := WorkDir(tt.prefix)
			if dir != tt.wantDir || human != tt.wantHuman {
				t.Errorf("WorkDir(%q) = %q, %v、期待値 = %q, %v", tt.prefix, dir, human, tt.wantDir, tt.wantHuman)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := newRepo(t)

	t.Run("リポジトリのルート", func(t *testing.T) {
		t.Parallel()
		got, err := Resolve(ctx, root)
		if err != nil {
			t.Fatalf("Resolveのエラー = %v", err)
		}
		if want := (Target{Dir: root, Repo: root}); got != want {
			t.Errorf("Resolve() = %+v、期待値 = %+v", got, want)
		}
	})

	t.Run("リポジトリの中のディレクトリはルートを求める", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(root, "go")
		got, err := Resolve(ctx, dir)
		if err != nil {
			t.Fatalf("Resolveのエラー = %v", err)
		}
		if want := (Target{Dir: dir, Repo: root}); got != want {
			t.Errorf("Resolve() = %+v、期待値 = %+v", got, want)
		}
	})

	t.Run("人が行うタスクはgitを使わない", func(t *testing.T) {
		t.Parallel()
		got, err := Resolve(ctx, "shimbaco")
		if err != nil {
			t.Fatalf("Resolveのエラー = %v", err)
		}
		if want := (Target{Human: true}); got != want {
			t.Errorf("Resolve() = %+v、期待値 = %+v", got, want)
		}
	})

	t.Run("無視されているディレクトリは重なっているパスとみなす", func(t *testing.T) {
		t.Parallel()
		for _, dir := range []string{"docs/private", "docs/private/plans"} {
			_, err := Resolve(ctx, filepath.Join(root, dir))
			if !errors.Is(err, ErrOverlaid) {
				t.Errorf("Resolve(%s)のエラー = %v、期待値 = ErrOverlaid", dir, err)
			}
		}
	})

	t.Run("リポジトリの外はエラー", func(t *testing.T) {
		t.Parallel()
		outside, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve(ctx, outside); err == nil || errors.Is(err, ErrOverlaid) {
			t.Errorf("Resolveのエラー = %v、リポジトリを求められないエラーを期待", err)
		}
	})
}
