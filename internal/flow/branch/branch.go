// Package branchは、タスクの接頭辞から作業するディレクトリとリポジトリを決める。
//
// 接頭辞は次のとおりに読む。
//
//	[/workspace/go] など / で始まるもの  そのパスのディレクトリで作業する
//	[Go]・[Rails]・接頭辞無し            /workspace/go・/workspace/rails・/workspace として読む
//	[shimbaco] など上記以外              人が行うタスク
//
// 上記以外を人が行うタスクとして扱うのは、作業する人の名前の一覧を持たずに済ませ、
// 知らない接頭辞が来たときに自動で進めない側へ倒すためである。
package branch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// legacyDirsは、パスではない接頭辞のうち、作業するディレクトリとして読むもの。
// 接頭辞がプラットフォーム名だった計画書を、そのまま進められるようにする。
var legacyDirs = map[string]string{
	"Go":    "/workspace/go",
	"Rails": "/workspace/rails",
	"":      "/workspace",
}

// ErrOverlaidは、作業するディレクトリに別のリポジトリが重なっていて、
// gitが求めたルートのリポジトリでは無視されているときのエラー。
// gitは重なっている側ではなく外側のリポジトリを返すため、そのままでは別の
// リポジトリでブランチを切ってしまう。
var ErrOverlaid = errors.New("作業するディレクトリに別のリポジトリが重なっています")

// Targetはタスクの対象。
type Target struct {
	// Humanは人が行うタスクかどうか。trueならDirとRepoは空。
	Human bool
	// Dirは作業するディレクトリ、Repoはそのディレクトリを含むリポジトリのルート。
	Dir  string
	Repo string
}

// WorkDirは接頭辞prefix (角括弧を除いたもの) が表す作業するディレクトリを返す。
// 人が行うタスクならhumanにtrueを返す。
func WorkDir(prefix string) (dir string, human bool) {
	if strings.HasPrefix(prefix, "/") {
		return filepath.Clean(prefix), false
	}
	if dir, ok := legacyDirs[prefix]; ok {
		return dir, false
	}
	return "", true
}

// Resolveは接頭辞prefixからタスクの対象を求める。作業するディレクトリが
// gitのリポジトリの中に無ければエラーを、別のリポジトリが重なっていれば
// ErrOverlaidを包んだエラーを返す。
func Resolve(ctx context.Context, prefix string) (Target, error) {
	dir, human := WorkDir(prefix)
	if human {
		return Target{Human: true}, nil
	}
	repo, err := Toplevel(ctx, dir)
	if err != nil {
		return Target{}, err
	}

	ignored, err := isIgnored(ctx, repo, dir)
	if err != nil {
		return Target{}, err
	}
	if ignored {
		return Target{}, fmt.Errorf("%w: %s は %s で無視されています。実体のリポジトリのパスを接頭辞にしてください", ErrOverlaid, dir, repo)
	}
	return Target{Dir: dir, Repo: repo}, nil
}

// Toplevelは、ディレクトリdirを含むリポジトリのルートを返す。
func Toplevel(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%s のリポジトリを求められません: %w", dir, err)
	}
	return strings.TrimSpace(out), nil
}

// isIgnoredは、リポジトリrepoがパスpathを無視しているかどうかを返す。
func isIgnored(ctx context.Context, repo, path string) (bool, error) {
	_, err := git(ctx, repo, "check-ignore", "-q", "--", path)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("%s が %s で無視されているかを確かめられません: %w", path, repo, err)
	}
}

// gitはdirでgitを実行し、標準出力を返す。失敗したときのエラーは*exec.ExitErrorを包む。
func git(ctx context.Context, dir string, args ...string) (string, error) {
	args = append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", args...) //#nosec G204 -- 引数はkorylusflowが組み立てたもの
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s が失敗しました: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
