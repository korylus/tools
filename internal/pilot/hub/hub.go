// Package hubは、役割のクローンの変更を受け渡すハブ (ベアリポジトリ) を扱う。
//
// ハブに書き込むのはkoryluspilotだけで、役割のコンテナには読み取り専用でマウントする。
// 役割のクローンの準備 (ハブからの取り込みとブランチの切り替え) と、クローンからの
// bundleの作成は役割のランナーで行い、ハブへの取り込みはkoryluspilotがbundleを
// 検証してから行う。koryluspilotはクローンのリポジトリを直接読まないため、
// エージェントが書き換えられるクローンの設定 (`.git/config` など) に左右されない。
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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/runner"
)

// DefaultTimeoutはgitの操作1つのタイムアウト。止まったまま戻らない場合だけを拾う。
const DefaultTimeout = 30 * time.Minute

// stderrTailMaxはエラーに添える標準エラー出力の末尾のバイト数。
const stderrTailMax = 2000

// reCommitはgitのコミットID (SHA-1 / SHA-256)。
var reCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RejectedErrorは、役割のクローンの状態がハブへの取り込みの条件を満たさないことを表す。
// gitの失敗などのエラーと区別し、呼び出し側は停止条件として扱う。
type RejectedError struct {
	Reason string
}

func (e *RejectedError) Error() string {
	return e.Reason
}

func rejectf(format string, args ...any) error {
	return &RejectedError{Reason: fmt.Sprintf(format, args...)}
}

// Hubはハブと役割のクローンを置くディレクトリ。
type Hub struct {
	// Rootはkoryluspilotから見たディレクトリ。直下にハブ (hub.git) と役割の
	// クローン (clones/<役割>) を置く。
	Root string
	// Pathは役割とゲートのコンテナの中から見たハブのパス (設定のhub.path)。
	Path string
}

// DefaultRootは、projectのハブと役割のクローンを置くVM上のディレクトリを返す。
// 計画書をまたいで使うため、XDG_DATA_HOME (無ければ ~/.local/share) の下に置く。
func DefaultRoot(project string) (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "koryluspilot", project), nil
}

// GitDirはkoryluspilotから見たハブのパスを返す。
func (h Hub) GitDir() string {
	return filepath.Join(h.Root, "hub.git")
}

// CloneDirはkoryluspilotから見た役割のクローンのパスを返す。役割のコンテナには
// このディレクトリを作業ディレクトリとしてマウントする。
func (h Hub) CloneDir(role string) string {
	return filepath.Join(h.Root, "clones", role)
}

// Initは、ハブが無ければ作り、rolesのクローンを置くディレクトリを作る。
// クローンのディレクトリを先に作るのは、マウントするときにコンテナの側で
// rootの持ち物として作られないようにするためである。
func (h Hub) Init(ctx context.Context, roles []string) error {
	for _, role := range roles {
		if err := os.MkdirAll(h.CloneDir(role), 0o750); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(h.GitDir(), "HEAD")); err == nil {
		return nil
	}
	if _, err := h.git(ctx, nil, "init", "--quiet", "--bare", h.GitDir()); err != nil {
		return fmt.Errorf("ハブ %s を作れません: %w", h.GitDir(), err)
	}
	return nil
}

// FetchUpstreamは、GitHubのリポジトリurlのブランチbranchを、認証なしでハブの
// 同じ名前のブランチへ取り込む。fast-forwardにならない更新は取り込まない
// (前のタスクのブランチの祖先が書き換わらないようにするため)。
func (h Hub) FetchUpstream(ctx context.Context, url, branch string) error {
	if err := h.checkBranch(ctx, branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if _, err := h.git(ctx, nil, "-C", h.GitDir(), "fetch", "--quiet", "--no-tags", "--", url, ref+":"+ref); err != nil {
		return fmt.Errorf("%s の %s をハブへ取り込めません: %w", url, branch, err)
	}
	return nil
}

// Resolveはハブのブランチbranchの先端のコミットを返す。ブランチが無ければokはfalse。
func (h Hub) Resolve(ctx context.Context, branch string) (commit string, ok bool, err error) {
	out, err := h.git(ctx, nil, "-C", h.GitDir(), "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

// Pinは、ハブにブランチbranchが無ければブランチfromの先端を指して作り、branchの
// 先端を返す。既にあれば動かさない。計画書の最初のタスクを切る元を固定し、
// base_branchがあとからGitHubで進んでも、ブランチを切った時点のコミットを
// 祖先の確認と差分の基準に使い続けるためである。
func (h Hub) Pin(ctx context.Context, branch, from string) (string, error) {
	for _, b := range []string{branch, from} {
		if err := h.checkBranch(ctx, b); err != nil {
			return "", err
		}
	}
	if commit, ok, err := h.Resolve(ctx, branch); err != nil || ok {
		return commit, err
	}
	commit, ok, err := h.Resolve(ctx, from)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("ブランチ %s を作る元の %s がハブにありません", branch, from)
	}
	// 確かめてから作るまでに作られていれば、作らない。
	if _, err := h.git(ctx, nil, "-C", h.GitDir(), "update-ref", "refs/heads/"+branch, commit, ""); err != nil {
		return "", fmt.Errorf("ハブのブランチ %s を作れません: %w", branch, err)
	}
	return commit, nil
}

// checkBranchはbranchがブランチの名前として正しいかを確かめる。
func (h Hub) checkBranch(ctx context.Context, branch string) error {
	if _, err := h.git(ctx, nil, "check-ref-format", "refs/heads/"+branch); err != nil || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("ブランチの名前 %q が不正です", branch)
	}
	return nil
}

// isAncestorはハブでancestorがcommitの祖先 (同じコミットを含む) かを返す。
func (h Hub) isAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	_, err := h.git(ctx, nil, "-C", h.GitDir(), "merge-base", "--is-ancestor", ancestor, commit)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return false, nil
	}
	return false, err
}

// gitはkoryluspilotの環境でgitを実行し、標準出力を返す。stdinがnilでなければ
// 標準入力に渡す。
//
// ハブの操作はユーザーやシステムのgitの設定 (credential helper・url.insteadOfなど) に
// 左右されないよう、それらを読まずに実行する。GitHubから認証なしで取り込むためでもある。
func (h Hub) git(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	cmd := runner.Runner{Env: gitEnv(os.Environ())}.Command(ctx, "git", args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w%s", args[0], err, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// gitEnvはbaseから、gitの設定と挙動を変える環境変数 (GIT_で始まるもの) を外し、
// ユーザー・システムのgitの設定を読まず、認証を求めないようにした環境を返す。
func gitEnv(base []string) []string {
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

// tailはエラーに添える、標準エラー出力の末尾。
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > stderrTailMax {
		s = s[len(s)-stderrTailMax:]
	}
	return "\n" + s
}

// readLineはrから改行までの1行を読む。
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}
