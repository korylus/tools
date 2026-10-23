package branch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRunはdirでgitを実行し、失敗したらテストを止める。コミットの作者は固定する。
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.name=korylusflow", "-c", "user.email=korylusflow@example.com"}, args...)
	cmd := exec.Command("git", args...) //#nosec G204 -- テストが組み立てたgitの引数
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepoOnは、ブランチbaseにコミットを1つ持つリポジトリを作り、そのルートを返す。
func newRepoOn(t *testing.T, base string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "--quiet", "--initial-branch="+base)
	writeFile(t, root, "README.md", "readme\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "最初のコミット")
	return root
}

// writeFileはrootの下のnameにcontentを書く。
func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("mainだけならmain", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		if got, err := DefaultBase(ctx, repo); err != nil || got != "main" {
			t.Errorf("DefaultBase() = %q, %v、期待値 = %q", got, err, "main")
		}
	})

	t.Run("developがあればdevelop", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		gitRun(t, repo, "branch", "develop")
		if got, err := DefaultBase(ctx, repo); err != nil || got != "develop" {
			t.Errorf("DefaultBase() = %q, %v、期待値 = %q", got, err, "develop")
		}
	})

	t.Run("どちらも無ければエラー", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "trunk")
		if _, err := DefaultBase(ctx, repo); err == nil {
			t.Error("エラーを期待したが、nilだった")
		}
	})
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("ブランチが無ければベースから作って切り替える", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		if err := Prepare(ctx, repo, "plan-1-1", "main", nil); err != nil {
			t.Fatalf("Prepareのエラー = %v", err)
		}
		assertBranch(t, repo, "plan-1-1")
		if n, err := Ahead(ctx, repo, "main"); err != nil || n != 0 {
			t.Errorf("Ahead() = %d, %v、期待値 = 0", n, err)
		}
	})

	t.Run("ブランチがあれば切り替えるだけ", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		gitRun(t, repo, "checkout", "--quiet", "-b", "plan-1-1")
		writeFile(t, repo, "a.txt", "a\n")
		gitRun(t, repo, "add", ".")
		gitRun(t, repo, "commit", "--quiet", "-m", "途中の作業")
		gitRun(t, repo, "checkout", "--quiet", "main")

		if err := Prepare(ctx, repo, "plan-1-1", "main", nil); err != nil {
			t.Fatalf("Prepareのエラー = %v", err)
		}
		assertBranch(t, repo, "plan-1-1")
		if n, err := Ahead(ctx, repo, "main"); err != nil || n != 1 {
			t.Errorf("Ahead() = %d, %v、期待値 = 1 (途中の作業が残っている)", n, err)
		}
	})

	t.Run("未コミットの変更があれば準備しない", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		writeFile(t, repo, "new.txt", "new\n")

		err := Prepare(ctx, repo, "plan-1-1", "main", []string{"docs/private"})
		var dirty *DirtyError
		if !errors.As(err, &dirty) {
			t.Fatalf("Prepareのエラー = %v、期待値 = *DirtyError", err)
		}
		if len(dirty.Files) != 1 || dirty.Files[0] != "?? new.txt" {
			t.Errorf("Files = %q、期待値 = [\"?? new.txt\"]", dirty.Files)
		}
		assertBranch(t, repo, "main")
	})

	t.Run("除外したパスの変更は数えない", func(t *testing.T) {
		t.Parallel()
		repo := newRepoOn(t, "main")
		writeFile(t, repo, "docs/private/plans/plan.md", "計画書\n")

		if err := Prepare(ctx, repo, "plan-1-1", "main", []string{"docs/private"}); err != nil {
			t.Fatalf("Prepareのエラー = %v", err)
		}
		assertBranch(t, repo, "plan-1-1")
	})
}

// assertBranchは、リポジトリrepoがブランチwantにいることを確かめる。
func assertBranch(t *testing.T, repo, want string) {
	t.Helper()
	got, err := Current(context.Background(), repo)
	if err != nil {
		t.Fatalf("Currentのエラー = %v", err)
	}
	if got != want {
		t.Errorf("ブランチ = %q、期待値 = %q", got, want)
	}
}
