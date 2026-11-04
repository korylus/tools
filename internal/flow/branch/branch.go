// Package branchは、タスクの接頭辞と担当から作業するディレクトリとリポジトリを決める。
//
// 担当の行の無いタスクは、接頭辞を次のとおりに読む。
//
//	[/workspace/go] など / で始まるもの  そのパスのディレクトリで作業する
//	[Go]・[Rails]・接頭辞無し            /workspace/go・/workspace/rails・/workspace として読む
//	[implementer]                        実装者がブランチを作らずに行うタスク
//	[shimbaco] など上記以外              人が行うタスク
//
// 担当の行のあるタスクは、接頭辞をブランチを作るリポジトリのパスとして読む。
//
//	担当がimplementerで接頭辞がパス     そのパスのディレクトリで作業する
//	担当がimplementerで接頭辞無し       実装者がブランチを作らずに行うタスク
//	担当がそれ以外で接頭辞がパスか無し  人が行うタスク
//	接頭辞がパス以外                    接頭辞と担当が食い違っているエラー
//
// 上記以外の接頭辞や担当を人が行うタスクとして扱うのは、作業する人の名前の一覧を
// 持たずに済ませ、知らない値が来たときに自動で進めない側へ倒すためである。
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

// ErrAssigneeMismatchは、担当の行のあるタスクの接頭辞がパスでないときのエラー。
// 担当の行のあるタスクでは、接頭辞はブランチを作るリポジトリだけを表す。
var ErrAssigneeMismatch = errors.New("タスクの接頭辞と担当の行が食い違っています")

// Implementerは、実装者が行うタスクを表す接頭辞と担当の値。
// korylusflowの役割名 (implementer) に揃える。
const Implementer = "implementer"

// Targetはタスクの対象。
type Target struct {
	// Humanは人が行うタスクかどうか。trueならDirとRepoは空。
	Human bool
	// Implementerは、実装者がブランチを作らずに行うタスクかどうか。trueならDirとRepoは空。
	Implementer bool
	// Dirは作業するディレクトリ、Repoはそのディレクトリを含むリポジトリのルート。
	Dir  string
	Repo string
}

// WorkDirは接頭辞prefix (角括弧を除いたもの) が表す作業するディレクトリを返す。
// 人が行うタスクならhumanにtrueを返す。ImplementerはResolveが先に読むため扱わない。
func WorkDir(prefix string) (dir string, human bool) {
	if strings.HasPrefix(prefix, "/") {
		return filepath.Clean(prefix), false
	}
	if dir, ok := legacyDirs[prefix]; ok {
		return dir, false
	}
	return "", true
}

// Resolveは接頭辞prefixと担当assigneeからタスクの対象を求める。担当の行が無ければ
// assigneeは空文字で、接頭辞だけから求める。担当の行があるのに接頭辞がパスでなければ
// ErrAssigneeMismatchを包んだエラーを返す。作業するディレクトリがgitのリポジトリの
// 中に無ければエラーを、別のリポジトリが重なっていればErrOverlaidを包んだエラーを返す。
func Resolve(ctx context.Context, prefix, assignee string) (Target, error) {
	if assignee != "" {
		return resolveAssigned(ctx, prefix, assignee)
	}
	if prefix == Implementer {
		return Target{Implementer: true}, nil
	}
	dir, human := WorkDir(prefix)
	if human {
		return Target{Human: true}, nil
	}
	return resolveDir(ctx, dir)
}

// resolveAssignedは、担当の行のあるタスクの対象を、接頭辞prefixと担当assigneeから求める。
func resolveAssigned(ctx context.Context, prefix, assignee string) (Target, error) {
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		return Target{}, fmt.Errorf("%w: 担当の行があるタスクの接頭辞 [%s] がパスではありません。接頭辞をブランチを作るリポジトリのパスにするか、取り除いてください", ErrAssigneeMismatch, prefix)
	}
	switch {
	case assignee != Implementer:
		return Target{Human: true}, nil
	case prefix == "":
		return Target{Implementer: true}, nil
	}
	return resolveDir(ctx, filepath.Clean(prefix))
}

// resolveDirは、作業するディレクトリdirを含むリポジトリを求める。
func resolveDir(ctx context.Context, dir string) (Target, error) {
	repo, err := Toplevel(ctx, dir)
	if err != nil {
		return Target{}, err
	}

	ignored, err := isIgnored(ctx, repo, dir)
	if err != nil {
		return Target{}, err
	}
	if ignored {
		return Target{}, fmt.Errorf("%w: %s は %s で無視されています。実体のリポジトリのパスを書いてください", ErrOverlaid, dir, repo)
	}
	return Target{Dir: dir, Repo: repo}, nil
}

// Repoは、ディレクトリdirを含むリポジトリのルートを、タスクの接頭辞と同じ規則で求める。
// dirに別のリポジトリが重なっていれば、ErrOverlaidを包んだエラーを返す。
func Repo(ctx context.Context, dir string) (string, error) {
	target, err := resolveDir(ctx, dir)
	return target.Repo, err
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
