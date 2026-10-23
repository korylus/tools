package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
	"github.com/korylus/tools/internal/plan"
)

// DefaultCommandTimeoutはCommandRunner.Timeoutを省略したときの、コマンド1つの
// タイムアウト。テストの実行を含むため長めにし、止まったまま戻らない場合だけを拾う。
const DefaultCommandTimeout = time.Hour

// outputTailMaxは失敗したコマンドの出力のうち、結果に残す末尾のバイト数。
// 差し戻しでエージェントに渡すため、原因が読める程度の長さにする。
const outputTailMax = 8000

// checkoutScriptはランナーの中で、ハブ ($1) からコミット ($2) を一時ディレクトリに
// 取り直し、そこでコマンド ($3) を実行するシェルスクリプト。ハブは読み取り専用で
// 持ち主の違うディレクトリにマウントされるため、safe.directoryで所有者の検査を外す。
const checkoutScript = `set -eu
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
git -c safe.directory='*' clone --quiet --no-checkout -- "$1" "$dir"
cd "$dir"
git checkout --quiet --detach --end-of-options "$2"
sh -c "$3"
`

// Commandsはタスクのプラットフォーム接頭辞に対応する、設定のゲートのコマンドを返す。
// 接頭辞が無いか、対応するコマンドが設定されていなければ空を返す。
func Commands(cfg *config.Config, task plan.Task) []string {
	return cfg.Gate.Commands[task.Platform]
}

// CommandRunnerは、ゲートのランナーで、ハブから取り直したチェックアウトに対して
// 設定のコマンドを実行する。
type CommandRunner struct {
	// Runnerはゲートのコンテナでコマンドを起動する接頭辞 (設定のgate.runner)。
	// 空ならkoryluspilotと同じ環境で直接起動する (テストで使う)。
	Runner []string
	// Repoはランナーの中から見たハブのパス。
	Repo string
	// Timeoutは0ならDefaultCommandTimeoutを使う。
	Timeout time.Duration
}

// CommandResultはコマンド1つの実行結果。
type CommandResult struct {
	Command string
	// ExitCodeは終了コード。タイムアウトで止めたときは-1。
	ExitCode int
	TimedOut bool
	Started  time.Time
	Duration time.Duration
	// LogPathは標準出力と標準エラー出力をまとめて保存したファイル。
	LogPath string
	// OutputTailは失敗したときの出力の末尾。成功したときは空。
	OutputTail string
}

// Passedはコマンドが成功したかを返す。
func (r CommandResult) Passed() bool {
	return r.ExitCode == 0 && !r.TimedOut
}

// Runはcommandsを、コミットcommitを取り直したチェックアウトで1つずつ順に実行し、
// 失敗したところで止める。コマンドの失敗は結果のExitCodeで表し、エラーは
// ランナーを起動できないなど、検査そのものができなかったときに返す。
// 各コマンドの出力はlogDirに保存する。
func (r CommandRunner) Run(ctx context.Context, commit string, commands []string, logDir string) ([]CommandResult, error) {
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, err
	}
	var results []CommandResult
	for i, command := range commands {
		res, err := r.run(ctx, commit, command, filepath.Join(logDir, fmt.Sprintf("command-%d.log", i+1)))
		if err != nil {
			return results, err
		}
		results = append(results, res)
		if !res.Passed() {
			break
		}
	}
	return results, nil
}

func (r CommandRunner) run(ctx context.Context, commit, command, logPath string) (CommandResult, error) {
	log, err := os.Create(logPath) //#nosec G304 -- 呼び出し側が状態ディレクトリの下に決めたパス
	if err != nil {
		return CommandResult{}, err
	}
	defer func() { _ = log.Close() }()

	res, err := r.runCheckout(ctx, commit, command, log, log)
	if err != nil {
		return CommandResult{}, err
	}
	res.LogPath = logPath
	if !res.Passed() {
		res.OutputTail = outputTail(logPath)
	}
	return res, nil
}

// runCheckoutは、ゲートのランナーで、コミットcommitを取り直したチェックアウトに
// 対してcommandを実行し、出力をstdoutとstderrに書く。
func (r CommandRunner) runCheckout(ctx context.Context, commit, command string, stdout, stderr io.Writer) (CommandResult, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 位置引数で渡し、ハブのパス・コミット・コマンドをスクリプトに埋め込まない。
	cmd := runner.Runner{Prefix: r.Runner}.Command(ctx, "sh", "-c", checkoutScript, "koryluspilot-gate", r.Repo, commit, command)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	res := CommandResult{Command: command, Started: time.Now()}
	if err := cmd.Start(); err != nil {
		return CommandResult{}, fmt.Errorf("ゲートのランナー %v を起動できません: %w", r.Runner, err)
	}
	runErr := cmd.Wait()
	res.Duration = time.Since(res.Started)

	var exitErr *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.TimedOut, res.ExitCode = true, -1
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case runErr != nil:
		return CommandResult{}, fmt.Errorf("ゲートのコマンド %q の終了を待てません: %w", command, runErr)
	}
	return res, nil
}

// outputTailは保存した出力の末尾を返す。
func outputTail(path string) string {
	b, err := os.ReadFile(path) //#nosec G304 -- runが書いたログファイルを読む
	if err != nil {
		return ""
	}
	b = bytes.TrimSpace(b)
	if len(b) > outputTailMax {
		b = b[len(b)-outputTailMax:]
	}
	return string(b)
}
