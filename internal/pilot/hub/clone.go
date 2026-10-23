package hub

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/korylus/tools/internal/pilot/runner"
)

// 役割のランナーで実行するスクリプトが、取り込みの条件を満たさないことを伝える終了コード。
const (
	exitDirty         = 3
	exitMissingBranch = 4
)

// dirtyCheckは、作業ツリーに未コミットの変更 (追跡していないファイルを含む) が
// あれば止めるシェルスクリプトの断片。
const dirtyCheck = `if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
  echo "未コミットの変更が残っています:" >&2
  git status --short --untracked-files=all >&2
  exit 3
fi
`

// prepareScriptは、役割のランナーの作業ディレクトリ (役割のクローン) をハブ ($1) の
// 最新に合わせ、ブランチ ($2) に切り替えるシェルスクリプト。ブランチがハブに
// あればその先端に、無ければハブのブランチ ($3) から作って切り替える。
// ハブは読み取り専用で持ち主の違うディレクトリにマウントされるため、
// safe.directoryで所有者の検査を外す。
const prepareScript = `set -eu
hub=$1 branch=$2 start=$3
if [ ! -d .git ]; then
  git init --quiet
fi
` + dirtyCheck + `git config remote.hub.url "$hub"
git config --replace-all remote.hub.fetch '+refs/heads/*:refs/remotes/hub/*'
git -c safe.directory='*' fetch --quiet --prune --no-tags hub
if git rev-parse --verify --quiet "refs/remotes/hub/$branch^{commit}" >/dev/null; then
  from=refs/remotes/hub/$branch
else
  from=refs/remotes/hub/$start
fi
git switch --quiet --no-track --force-create "$branch" "$from"
`

// bundleScriptは、役割のランナーの作業ディレクトリ (役割のクローン) のブランチ ($1) の
// 先端のコミットを1行目に書き、ハブに無いコミットがあれば続けてbundleを書き出す
// シェルスクリプト。$2以降はハブにあるコミットで、クローンにもあるものを
// bundleの前提にして、ハブが既に持つ履歴を送らないようにする。
const bundleScript = `set -eu
branch=$1
shift
` + dirtyCheck + `tip=$(git rev-parse --verify --quiet "refs/heads/$branch^{commit}") || {
  echo "ブランチ $branch がありません" >&2
  exit 4
}
printf '%s\n' "$tip"
for c do
  shift
  if git cat-file -e "$c^{commit}" 2>/dev/null; then
    if git merge-base --is-ancestor "$tip" "$c"; then
      # 先端がハブにあるコミットに含まれるので、送るものは無い。
      exit 0
    fi
    set -- "$@" "^$c"
  fi
done
git bundle create --quiet - "refs/heads/$branch" "$@"
`

// PrepareCloneは、役割のランナーrで、役割のクローンをハブの最新に合わせて
// ブランチbranchに切り替える。branchがハブに無ければ、ハブのブランチstartから作る。
// クローンに未コミットの変更が残っているときは、消さずに*RejectedErrorを返す。
// クローンのブランチはハブの先端に合わせ直すため、ハブに取り込んでいないコミットは残らない。
func (h Hub) PrepareClone(ctx context.Context, r runner.Runner, branch, start string) error {
	for _, b := range []string{branch, start} {
		if err := h.checkBranch(ctx, b); err != nil {
			return err
		}
	}
	if _, ok, err := h.Resolve(ctx, branch); err != nil {
		return err
	} else if !ok {
		if _, ok, err := h.Resolve(ctx, start); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("ブランチ %s を作る元の %s がハブにありません", branch, start)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	cmd := r.Command(ctx, "sh", "-c", prepareScript, "koryluspilot-prepare", h.Path, branch, start)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return scriptError(err, stderr.String(), "役割のクローンを準備できません")
	}
	return nil
}

// Importは取り込みの結果。
type Import struct {
	// Oldは取り込む前のハブのブランチの先端。ブランチが無かったときは空。
	Old string
	// Newは取り込んだあとのハブのブランチの先端。
	New string
}

// Updatedはハブのブランチが更新されたかを返す。
func (i Import) Updated() bool {
	return i.Old != i.New
}

// Importは、役割のランナーrで役割のクローンのブランチbranchをbundleにして受け取り、
// ハブのbranchへ取り込む。次を満たさなければ*RejectedErrorを返し、ハブを変えない。
//
//   - クローンに未コミットの変更が残っていない
//   - クローンにbranchがある
//   - ハブのbranchからfast-forwardになる更新である
//   - ハブのブランチbase (前のタスクのブランチ、最初のタスクはbase_branch) を祖先に持つ
//
// 取り込むのはbranchだけで、クローンの他のブランチは取り込まない。
func (h Hub) Import(ctx context.Context, r runner.Runner, branch, base string) (Import, error) {
	for _, b := range []string{branch, base} {
		if err := h.checkBranch(ctx, b); err != nil {
			return Import{}, err
		}
	}
	old, hasOld, err := h.Resolve(ctx, branch)
	if err != nil {
		return Import{}, err
	}
	baseCommit, ok, err := h.Resolve(ctx, base)
	if err != nil {
		return Import{}, err
	}
	if !ok {
		return Import{}, fmt.Errorf("基準のブランチ %s がハブにありません", base)
	}
	known := []string{baseCommit}
	if hasOld {
		known = append([]string{old}, known...)
	}

	tip, bundlePath, err := h.receiveBundle(ctx, r, branch, known)
	if bundlePath != "" {
		defer func() { _ = os.Remove(bundlePath) }()
	}
	if err != nil {
		return Import{}, err
	}
	if bundlePath != "" {
		if err := h.fetchBundle(ctx, bundlePath, branch, tip); err != nil {
			return Import{}, err
		}
	} else if _, err := h.git(ctx, nil, "-C", h.GitDir(), "cat-file", "-e", tip+"^{commit}"); err != nil {
		return Import{}, fmt.Errorf("クローンのブランチ %s の先端 %s がハブにありません: %w", branch, tip, err)
	}

	res := Import{Old: old, New: tip}
	if hasOld {
		ff, err := h.isAncestor(ctx, old, tip)
		if err != nil {
			return Import{}, err
		}
		if !ff {
			return Import{}, rejectf("ブランチ %s の更新がfast-forwardではありません (ハブ: %s、クローン: %s)。履歴を書き換えた変更は取り込みません", branch, old, tip)
		}
	}
	onBase, err := h.isAncestor(ctx, baseCommit, tip)
	if err != nil {
		return Import{}, err
	}
	if !onBase {
		return Import{}, rejectf("ブランチ %s が %s (%s) を祖先に持ちません", branch, base, baseCommit)
	}
	if !res.Updated() {
		return res, nil
	}
	// 確かめてから更新するまでにハブが変わっていれば、更新しない (oldが空なら、
	// ブランチがまだ無いときだけ作る)。
	if _, err := h.git(ctx, nil, "-C", h.GitDir(), "update-ref", "refs/heads/"+branch, tip, old); err != nil {
		return Import{}, fmt.Errorf("ハブのブランチ %s を更新できません: %w", branch, err)
	}
	return res, nil
}

// receiveBundleは、役割のランナーでbundleScriptを実行し、クローンのブランチの先端と、
// 書き出されたbundleを保存した一時ファイルのパスを返す。送るコミットが無ければ
// パスは空。
func (h Hub) receiveBundle(ctx context.Context, r runner.Runner, branch string, known []string) (tip, bundlePath string, err error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	args := append([]string{"-c", bundleScript, "koryluspilot-bundle", branch}, known...)
	cmd := r.Command(ctx, "sh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", err
	}
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("役割のランナーを起動できません: %w", err)
	}

	file, err := os.CreateTemp("", "koryluspilot-*.bundle")
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", "", err
	}
	bundlePath = file.Name()
	br := bufio.NewReader(stdout)
	tip, readErr := readLine(br)
	var n int64
	if readErr == nil {
		n, readErr = io.Copy(file, br)
	} else {
		// 1行目を読めないときも、終了の結果を報告できるよう出力を読み切る。
		_, _ = io.Copy(io.Discard, br)
	}
	closeErr := file.Close()
	waitErr := cmd.Wait()

	switch {
	case waitErr != nil:
		return "", bundlePath, scriptError(waitErr, stderr.String(), "クローンからbundleを作れません")
	case readErr != nil:
		return "", bundlePath, fmt.Errorf("クローンからbundleを読めません: %w", readErr)
	case closeErr != nil:
		return "", bundlePath, closeErr
	case !reCommit.MatchString(tip):
		return "", bundlePath, fmt.Errorf("クローンのブランチ %s の先端 %q を読めません", branch, tip)
	case n == 0:
		_ = os.Remove(bundlePath)
		return tip, "", nil
	}
	return tip, bundlePath, nil
}

// fetchBundleはbundleを検証し、ブランチbranchのオブジェクトをハブへ取り込む。
// bundleのbranchの先端がtipと一致することを確かめる。ブランチは更新しない。
func (h Hub) fetchBundle(ctx context.Context, bundlePath, branch, tip string) error {
	if _, err := h.git(ctx, nil, "-C", h.GitDir(), "bundle", "verify", "--quiet", bundlePath); err != nil {
		return fmt.Errorf("クローンから受け取ったbundleを検証できません: %w", err)
	}
	// 壊れたオブジェクトや不正なオブジェクトを受け入れないよう、検査してから取り込む。
	if _, err := h.git(ctx, nil, "-C", h.GitDir(), "-c", "fetch.fsckObjects=true", "fetch", "--quiet", "--no-tags", "--", bundlePath, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("クローンから受け取ったbundleを取り込めません: %w", err)
	}
	out, err := h.git(ctx, nil, "-C", h.GitDir(), "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != tip {
		return fmt.Errorf("bundleのブランチ %s の先端 %s が、クローンが伝えた先端 %s と一致しません", branch, got, tip)
	}
	return nil
}

// scriptErrorは、役割のランナーで実行したスクリプトの失敗をエラーにする。
// 取り込みの条件を満たさないことを伝える終了コードは*RejectedErrorにする。
func scriptError(err error, stderr, msg string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case exitDirty, exitMissingBranch:
			return rejectf("%s: %s", msg, strings.TrimSpace(stderr))
		}
	}
	return fmt.Errorf("%s: %w%s", msg, err, tail(stderr))
}
