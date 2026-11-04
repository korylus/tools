package branch

import (
	"context"
	"slices"
	"testing"
)

func TestChanges(t *testing.T) {
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

	if _, err := Changes(ctx, repo, "main", "missing"); err == nil {
		t.Error("無いブランチでエラーを期待したが、nilだった")
	}
}

func TestChangesWithSameNamedTag(t *testing.T) {
	t.Parallel()
	repo := newRepoOn(t, "main")
	// タグprevはmainを、タグtaskはブランチprevを指し、どちらもブランチとは別のコミットになる。
	branchOverTag(t, repo, "prev")
	branchOverTag(t, repo, "task")

	files, err := Changes(context.Background(), repo, "prev", "task")
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{{Path: "task.txt", Added: 1}}
	if !slices.Equal(files, want) {
		t.Errorf("変更 = %+v、期待値 = %+v (同名タグではなくブランチどうしを比べる)", files, want)
	}
}
