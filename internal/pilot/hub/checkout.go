package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/korylus/tools/internal/pilot/runner"
)

// Checkoutは人間のチェックアウトへの取り込みの結果。
type Checkout struct {
	// Switchedは最後のブランチに切り替えたかどうか。作業ツリーに未コミットの変更が
	// あったときは切り替えない (作業報告書に切り替えの手順を載せる)。
	Switched bool
	// Skippedは、チェックアウトしているブランチだったが、作業ツリーに未コミットの
	// 変更があったため取り込まなかったブランチ。
	Skipped []string
}

// ExportToは、ハブのブランチbranchesを人間のチェックアウトdirの同じ名前のブランチへ
// 取り込む。branchesはタスクの順に並べ、最後のブランチを最後のタスクのものとする。
// 作業ツリーに未コミットの変更が無ければ、最後のブランチに切り替える。
// チェックアウトのブランチがハブのブランチからfast-forwardにならないときは、
// 人間の変更を上書きしないようエラーにする。
//
// チェックアウトは人間のものなので、gitは人間の環境と設定のまま実行する。
func (h Hub) ExportTo(ctx context.Context, dir string, branches []string) (Checkout, error) {
	if len(branches) == 0 {
		return Checkout{}, errors.New("取り込むブランチがありません")
	}
	commits := make(map[string]string, len(branches))
	for _, b := range branches {
		if err := h.checkBranch(ctx, b); err != nil {
			return Checkout{}, err
		}
		commit, ok, err := h.Resolve(ctx, b)
		if err != nil {
			return Checkout{}, err
		}
		if !ok {
			return Checkout{}, fmt.Errorf("ブランチ %s がハブにありません", b)
		}
		commits[b] = commit
	}

	current, err := currentBranch(ctx, dir)
	if err != nil {
		return Checkout{}, err
	}
	status, err := checkoutGit(ctx, dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return Checkout{}, err
	}
	clean := len(bytes.TrimSpace(status)) == 0

	// チェックアウトしているブランチはfetchで更新できないため、オブジェクトだけを
	// 取り込み、あとでfast-forwardのマージで進める。
	refspecs := make([]string, 0, len(branches))
	for _, b := range branches {
		if b == current {
			refspecs = append(refspecs, "refs/heads/"+b)
			continue
		}
		refspecs = append(refspecs, "refs/heads/"+b+":refs/heads/"+b)
	}
	args := append([]string{"fetch", "--quiet", "--no-tags", "--", h.GitDir()}, refspecs...)
	if _, err := checkoutGit(ctx, dir, args...); err != nil {
		return Checkout{}, fmt.Errorf("ハブのブランチをチェックアウトへ取り込めません: %w", err)
	}

	var res Checkout
	if slices.Contains(branches, current) {
		if !clean {
			res.Skipped = append(res.Skipped, current)
		} else {
			_, err := checkoutGit(ctx, dir, "merge-base", "--is-ancestor", "HEAD", commits[current])
			var exitErr *exec.ExitError
			switch {
			case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
				return Checkout{}, fmt.Errorf("チェックアウトしているブランチ %s がハブの先端 %s の祖先ではありません (ハブより先に進んでいるか、分岐しています)。人間の変更を上書きしないよう取り込みません", current, commits[current])
			case err != nil:
				return Checkout{}, err
			}
			if _, err := checkoutGit(ctx, dir, "merge", "--quiet", "--ff-only", commits[current]); err != nil {
				return Checkout{}, fmt.Errorf("チェックアウトしているブランチ %s を進められません: %w", current, err)
			}
		}
	}
	if !clean {
		return res, nil
	}
	if last := branches[len(branches)-1]; last != current {
		if _, err := checkoutGit(ctx, dir, "switch", "--quiet", last); err != nil {
			return Checkout{}, fmt.Errorf("ブランチ %s に切り替えられません: %w", last, err)
		}
	}
	res.Switched = true
	return res, nil
}

// currentBranchはチェックアウトdirで切り替えているブランチを返す。
// ブランチに切り替えていなければ空を返す。
func currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := checkoutGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// checkoutGitは人間のチェックアウトdirでgitを実行し、標準出力を返す。
func checkoutGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	cmd := runner.Runner{Dir: dir}.Command(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w%s", args[0], err, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}
