package branch

import (
	"context"
	"slices"
	"testing"
)

func TestCommitsAndChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	repo := newRepoOn(t, "main")
	gitRun(t, repo, "checkout", "--quiet", "-b", "task")
	writeFile(t, repo, "a b.go", "1\n2\n")
	writeFile(t, repo, "image.bin", "\x00\x01")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "--quiet", "-m", "追加する")
	gitRun(t, repo, "mv", "README.md", "docs.md")
	writeFile(t, repo, "a b.go", "1\n3\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "--quiet", "-m", "名前を変える")
	// 分岐したあとのベースのコミットは、タスクの変更に含めない。
	gitRun(t, repo, "checkout", "--quiet", "main")
	writeFile(t, repo, "main.txt", "main\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "--quiet", "-m", "ベースを進める")

	commits, err := Commits(ctx, repo, "main", "task")
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range commits {
		if c.Hash == "" {
			t.Errorf("コミットのハッシュが空: %+v", c)
		}
		subjects = append(subjects, c.Subject)
	}
	if want := []string{"追加する", "名前を変える"}; !slices.Equal(subjects, want) {
		t.Errorf("コミット = %v、期待値 = %v", subjects, want)
	}

	files, err := Changes(ctx, repo, "main", "task")
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Path: "README.md", Removed: 1},
		{Path: "a b.go", Added: 2},
		{Path: "docs.md", Added: 1},
		{Path: "image.bin", Binary: true},
	}
	if !slices.Equal(files, want) {
		t.Errorf("変更 = %+v、期待値 = %+v", files, want)
	}

	if _, err := Commits(ctx, repo, "main", "missing"); err == nil {
		t.Error("無いブランチでエラーを期待したが、nilだった")
	}
}
