package branch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// baseCandidatesは、リポジトリの最初のタスクのブランチを作る元の候補を、優先する順に並べたもの。
var baseCandidates = []string{"develop", "main"}

// DirtyErrorは、作業ツリーに未コミットの変更があるため、ブランチを準備しなかったときのエラー。
type DirtyError struct {
	Repo string
	// Filesは `git status --porcelain` の形の、未コミットの変更の一覧。
	Files []string
}

func (e *DirtyError) Error() string {
	return fmt.Sprintf("%s に未コミットの変更があります: %s", e.Repo, strings.Join(e.Files, ", "))
}

// DefaultBaseは、リポジトリrepoの最初のタスクのブランチを作る元のブランチを返す。
// developがあればdevelop、無ければmainを使う。
func DefaultBase(ctx context.Context, repo string) (string, error) {
	for _, name := range baseCandidates {
		ok, err := branchExists(ctx, repo, name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s に %s のどちらのブランチもありません", repo, strings.Join(baseCandidates, " / "))
}

// Prepareは、リポジトリrepoでブランチnameに切り替える。nameが無ければbaseから作る。
// 作業ツリーに未コミットの変更があれば、何もせずに*DirtyErrorを返す。
// excludeに挙げたパス (リポジトリのルートからの相対パス) の変更は、未コミットの変更に数えない。
func Prepare(ctx context.Context, repo, name, base string, exclude []string) error {
	files, err := Dirty(ctx, repo, exclude)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		return &DirtyError{Repo: repo, Files: files}
	}

	exists, err := branchExists(ctx, repo, name)
	if err != nil {
		return err
	}
	if exists {
		_, err = git(ctx, repo, "checkout", "--quiet", name)
	} else {
		_, err = git(ctx, repo, "checkout", "--quiet", "-b", name, base)
	}
	return err
}

// Dirtyは、リポジトリrepoの作業ツリーの未コミットの変更 (未追跡のファイルを含む) を、
// `git status --porcelain` の形で返す。excludeに挙げたパスの変更は除く。
func Dirty(ctx context.Context, repo string, exclude []string) ([]string, error) {
	args := []string{"status", "--porcelain", "--", "."}
	for _, path := range exclude {
		args = append(args, ":(exclude)"+path)
	}
	out, err := git(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Currentは、リポジトリrepoで切り替えているブランチの名前を返す。
func Current(ctx context.Context, repo string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// Aheadは、リポジトリrepoのHEADがbaseより先に持つコミットの数を返す。
func Ahead(ctx context.Context, repo, base string) (int, error) {
	out, err := git(ctx, repo, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// branchExistsは、リポジトリrepoにブランチnameがあるかどうかを返す。
func branchExists(ctx context.Context, repo, name string) (bool, error) {
	_, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	default:
		return false, err
	}
}
