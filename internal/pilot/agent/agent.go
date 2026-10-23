// Package agentはClaude Code (`claude -p`) とCodex (`codex exec`) を役割のランナーで
// 起動し、出力からセッションIDと構造化出力を取り出す。
//
// 役割のコンテナには秘密情報を渡さないため、子プロセスの環境は調整しない。
// 各ステップの出力 (stream-json / JSONL) は止まった理由を後から追えるよう、
// 呼び出し側が指定したログファイルに保存する。
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
)

// Toolはエージェントとして起動するCLI。
type Tool string

const (
	Claude Tool = config.ToolClaude
	Codex  Tool = config.ToolCodex
)

// Kindはステップの種類。種類によって使うeffortが変わる。
type Kind int

const (
	// Normalは実装・コミット・レビュー・フィードバックの起票などの通常のステップ。
	Normal Kind = iota
	// Fixはレビューのあとの修正と、ゲートからの差し戻しへの対応。
	Fix
)

// DefaultTimeoutはRequest.Timeoutを省略したときのタイムアウト。実装のステップは
// テストの実行を含めて長くかかるため、止まったまま戻らない場合だけを拾う長さにする。
const DefaultTimeout = 3 * time.Hour

// stderrTailMaxはエラーに添える標準エラー出力の末尾のバイト数。
const stderrTailMax = 2000

var (
	// ErrRateLimitedは使用量の上限に当たってステップが失敗したことを表す。
	// 呼び出し側はリセットを待ってから同じセッションを再開する。
	ErrRateLimited = errors.New("使用量の上限に達しました")
	// ErrTimeoutはステップがタイムアウトしたことを表す。
	ErrTimeout = errors.New("タイムアウトしました")
)

// NewRequestは役割の設定とステップの種類から、ツール・モデル・effort・サンドボックスを
// 埋めたRequestを返す。修正のステップはfix_effortを使い、省略されていればeffortを使う。
func NewRequest(role config.Role, kind Kind) Request {
	effort := role.Effort
	if kind == Fix && role.FixEffort != "" {
		effort = role.FixEffort
	}
	return Request{Tool: Tool(role.Tool), Model: role.Model, Effort: effort, Sandbox: role.Sandbox}
}

// Requestは1回のエージェントの起動。
type Request struct {
	Tool Tool
	// ModelとEffortはツールに渡すモデルとeffort。空文字はツール側の既定値に任せる。
	Model  string
	Effort string
	// SandboxはCodexのサンドボックスのモード。空ならCodexの既定値に任せる。
	Sandbox string
	Prompt  string
	// SessionIDが空でなければ、そのセッションを再開する。
	SessionID string
	// Schemaは構造化出力の形を強制するJSON Schema。nilなら強制しない。
	Schema []byte
	// LogPathは標準出力を保存するファイル。標準エラー出力はLogPathに
	// ".stderr" を足したファイルに保存する。
	LogPath string
	// Timeoutは0ならDefaultTimeoutを使う。
	Timeout time.Duration
}

// Resultはエージェントの実行結果。
type Result struct {
	// SessionIDは再開に使うセッションID。出力から取り出せなければ空。
	SessionID string
	// Outputは構造化出力 (JSON)。Request.Schemaを渡さなかったときはnil。
	Output []byte
	// RateLimitedは使用量の上限に当たったかどうか。
	RateLimited bool
	// TurnsとCostUSDは、取れる場合のターン数とコスト (米ドル)。取れなければ0。
	Turns   int
	CostUSD float64
}

// Runnerはエージェントを起動する。ゼロ値は、PATH上の `claude` / `codex` を
// カレントディレクトリで現在の環境から直接起動する。
type Runner struct {
	// Execは役割のランナー。
	Exec runner.Runner
	// ClaudeCommandとCodexCommandは起動するコマンド。空ならツール名を使う。
	// テストでは偽のコマンドに差し替える。
	ClaudeCommand string
	CodexCommand  string
}

// Runはエージェントを起動し、終了を待って結果を返す。プロセスを起動できた
// ときは、エラーを返す場合も取り出せた範囲の結果を返す (再開にセッションIDを使うため)。
// ただし、保存した出力を読み直せなかったときは結果を返さない。
func (r Runner) Run(ctx context.Context, req Request) (*Result, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name, args, err := r.command(req)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(req.LogPath), 0o750); err != nil {
		return nil, err
	}
	stdout, err := os.Create(req.LogPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stdout.Close() }()
	stderrPath := req.LogPath + ".stderr"
	stderr, err := os.Create(stderrPath) //#nosec G304 -- 呼び出し側が状態ディレクトリの下に決めたパス
	if err != nil {
		return nil, err
	}
	defer func() { _ = stderr.Close() }()

	cmd := r.Exec.Command(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s を起動できません: %w", name, err)
	}
	runErr := cmd.Wait()

	if _, err := stdout.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	parse := parseClaude
	if req.Tool == Codex {
		parse = parseCodex
	}
	out, err := parse(stdout)
	res := &Result{SessionID: out.sessionID, RateLimited: out.rateLimited, Turns: out.turns, CostUSD: out.costUSD}
	if err != nil {
		return res, fmt.Errorf("%s を読み取れません: %w", req.LogPath, err)
	}
	if req.Schema != nil {
		res.Output = out.output
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return res, fmt.Errorf("%s が%sで終わらず、%w", req.Tool, timeout, ErrTimeout)
	case out.rateLimited:
		return res, fmt.Errorf("%s: %w (%s)", req.Tool, ErrRateLimited, out.failure)
	case out.failure != "":
		return res, fmt.Errorf("%s が失敗を報告しました: %s", req.Tool, out.failure)
	case runErr != nil:
		return res, fmt.Errorf("%s が異常終了しました: %w%s", req.Tool, runErr, stderrTail(stderrPath))
	case req.Schema != nil && res.Output == nil:
		return res, fmt.Errorf("%s の出力に構造化出力がありません (ログ: %s)", req.Tool, req.LogPath)
	}
	return res, nil
}

// stderrTailは標準エラー出力の末尾を、エラーに添える形で返す。
func stderrTail(path string) string {
	b, err := os.ReadFile(path) //#nosec G304 -- Runが書いたログファイルを読む
	if err != nil {
		return ""
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	if len(b) > stderrTailMax {
		b = b[len(b)-stderrTailMax:]
	}
	return "\n" + string(b)
}
