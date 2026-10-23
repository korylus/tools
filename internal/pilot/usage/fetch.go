package usage

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/runner"
)

// DefaultFetchTimeoutはCommandFetcher.Timeoutを省略したときの、1回の取得のタイムアウト。
const DefaultFetchTimeout = time.Minute

// DefaultStaleAfterはTracker.StaleAfterを省略したときの、記録を古いとみなす経過時間。
const DefaultStaleAfter = 10 * time.Minute

// appServerRateLimitsIDは `account/rateLimits/read` の要求に付けるID。
const appServerRateLimitsID = 2

// appServerRequestsはapp-serverに送るJSON-RPCの要求。初期化の手順を踏んでから
// 使用率を読む。
var appServerRequests = []string{
	`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"koryluspilot","version":"0"}}}`,
	`{"method":"initialized"}`,
	fmt.Sprintf(`{"id":%d,"method":"account/rateLimits/read"}`, appServerRateLimitsID),
}

// Fetcherは現在の使用率を取得する。nowは取得した観測の時刻と、年の無いリセット
// 時刻の解釈に使う。主な取得元から得られず補助の取得元で補ったときは、
// 観測と併せて*FallbackErrorを返す。
type Fetcher interface {
	Fetch(ctx context.Context, tool agent.Tool, now time.Time) ([]Sample, error)
}

// FallbackErrorは、主な取得元から使用率を得られず補助の取得元で補ったことを表す。
// 補った観測は過去のものであることがあり、使えないと判断したときに主な取得元の
// 失敗の理由を伝えるため、観測と併せて返す。
type FallbackError struct {
	Source Source
	Err    error
}

func (e *FallbackError) Error() string {
	return fmt.Sprintf("主な取得元から使用率を取得できず、%s の記録で補いました: %v", e.Source, e.Err)
}

func (e *FallbackError) Unwrap() error {
	return e.Err
}

// CommandFetcherはトークンを消費しない手段で、現在の使用率を取得する。ゼロ値は、
// PATH上の `claude` / `codex` をカレントディレクトリで現在の環境から起動する。
type CommandFetcher struct {
	// ClaudeCommandとCodexCommandは起動するコマンド。空ならツール名を使う。
	// テストでは偽のコマンドに差し替える。
	ClaudeCommand string
	CodexCommand  string
	// ClaudeとCodexは、それぞれのツールの取得に使うランナー。認証は役割の
	// コンテナ用のボリュームにあるため、そのツールを使う役割のランナーを指定する。
	Claude runner.Runner
	Codex  runner.Runner
	// CodexHomeは、Codexのランナーの中でセッションログを探すディレクトリ。
	// 空なら環境変数CODEX_HOME、それも無ければ ~/.codex を使う。
	CodexHome string
	// Timeoutは0ならDefaultFetchTimeoutを使う。
	Timeout time.Duration
}

// Fetchはtoolの使用率を取得する。Claude Codeは `claude -p "/usage"`、Codexは
// `codex app-server` から取得し、Codexの取得に失敗したときは最新のセッションログの
// 記録で補い、app-serverの失敗の理由を*FallbackErrorで併せて返す。
func (f CommandFetcher) Fetch(ctx context.Context, tool agent.Tool, now time.Time) ([]Sample, error) {
	switch tool {
	case agent.Claude:
		samples, err := withTimeout(ctx, f.Timeout, func(ctx context.Context) ([]Sample, error) {
			return f.claudeUsage(ctx, now)
		})
		if err != nil {
			return nil, err
		}
		return stamp(samples, now, tool, SourceUsageCommand), nil
	case agent.Codex:
		samples, err := withTimeout(ctx, f.Timeout, f.codexAppServer)
		if err == nil {
			return stamp(samples, now, tool, SourceAppServer), nil
		}
		// app-serverが応答せずに期限を使い切っても読めるよう、期限を分ける。
		logged, logErr := withTimeout(ctx, f.Timeout, f.codexSessionLog)
		if logErr != nil {
			return nil, fmt.Errorf("app-serverからもセッションログからも使用率を取得できません:\n%w", errors.Join(err, logErr))
		}
		// セッションログの観測の時刻は、ログに記録された時刻のまま残す。
		for i := range logged {
			logged[i].Tool = tool
			logged[i].Source = SourceSessionLog
		}
		return logged, &FallbackError{Source: SourceSessionLog, Err: err}
	}
	return nil, fmt.Errorf("不明なツール %q", tool)
}

// claudeUsageは `claude -p "/usage"` を起動して使用率を読む。
func (f CommandFetcher) claudeUsage(ctx context.Context, now time.Time) ([]Sample, error) {
	name := cmp.Or(f.ClaudeCommand, string(agent.Claude))
	cmd := f.Claude.Command(ctx, name, "-p", "/usage", "--output-format", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s -p /usage が失敗しました: %w%s", name, err, tail(stderr.String()))
	}
	return ParseClaudeUsage(out, now)
}

// codexAppServerは `codex app-server` を起動し、JSON-RPCで使用率を読む。
// app-serverは標準入力が閉じると応答を返さずに終わるため、応答を読むまで開いたままにし、
// 読んだら標準入力を閉じて終わらせる。
func (f CommandFetcher) codexAppServer(ctx context.Context) ([]Sample, error) {
	name := cmp.Or(f.CodexCommand, string(agent.Codex))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := f.Codex.Command(runCtx, name, "app-server")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s app-server を起動できません: %w", name, err)
	}

	// 書き込みに失敗するのはプロセスが先に終わったときで、その理由は終了の結果で報告する。
	_, _ = io.WriteString(stdin, strings.Join(appServerRequests, "\n")+"\n")
	samples, found, readErr := readAppServer(stdout)
	waitErr := closeAndWait(cmd, stdin, stop)
	switch {
	case found:
		return samples, readErr
	case ctx.Err() != nil:
		return nil, fmt.Errorf("%s app-server が応答しませんでした: %w", name, ctx.Err())
	case readErr != nil:
		return nil, readErr
	}
	return nil, fmt.Errorf("%s app-server が使用率を返さずに終了しました: %v%s", name, waitErr, tail(stderr.String()))
}

// appServerExitGraceは、app-serverの標準入力を閉じてから、自分で終わるのを待つ時間。
const appServerExitGrace = 5 * time.Second

// closeAndWaitは標準入力を閉じてcmdが終わるのを待ち、appServerExitGraceのうちに
// 終わらなければstopで止める。ランナーがコンテナのときは、プロセスを止めるより
// 自分で終わらせたほうが確実にコンテナが片付くためである。
func closeAndWait(cmd *exec.Cmd, stdin io.Closer, stop context.CancelFunc) error {
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(appServerExitGrace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		stop()
		return <-done
	}
}

// readAppServerは `account/rateLimits/read` の応答が届くまでapp-serverの出力を読む。
func readAppServer(r io.Reader) (samples []Sample, found bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		samples, ok, err := parseAppServerResponse(sc.Bytes(), appServerRateLimitsID)
		if ok {
			return samples, true, err
		}
	}
	return nil, false, sc.Err()
}

// codexSessionLogScriptは、Codexのランナーの中で最後に更新されたセッションログを探し、
// 1行目にそのパス、2行目以降に内容を書き出すシェルスクリプト。$1が空でなければ
// そのディレクトリを、空なら環境変数CODEX_HOME、それも無ければ ~/.codex を使う。
// find -printfはGNUの拡張を使う。
const codexSessionLogScript = `set -eu
home=${1:-${CODEX_HOME:-$HOME/.codex}}
dir=$home/sessions
newest=$(find "$dir" -type f -name '*.jsonl' -printf '%T@ %p\n' | sort -n | tail -n 1 | cut -d ' ' -f 2-)
if [ -z "$newest" ]; then
  echo "$dir にセッションログがありません" >&2
  exit 3
fi
printf '%s\n' "$newest"
cat -- "$newest"
`

// codexSessionLogは、Codexのランナーの中で最後に更新されたセッションログから使用率を読む。
// ログは役割のコンテナ用のボリュームにあるため、koryluspilotからは直接読まない。
func (f CommandFetcher) codexSessionLog(ctx context.Context) ([]Sample, error) {
	cmd := f.Codex.Command(ctx, "sh", "-c", codexSessionLogScript, "koryluspilot-usage", f.CodexHome)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("セッションログを読めません: %w%s", err, tail(stderr.String()))
	}
	path, content, _ := strings.Cut(string(out), "\n")
	samples, err := ParseCodexSessionLog(strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return samples, nil
}

// stampは観測に時刻・ツール・取得元を入れる。
func stamp(samples []Sample, at time.Time, tool agent.Tool, source Source) []Sample {
	for i := range samples {
		samples[i].At = at
		samples[i].Tool = tool
		samples[i].Source = source
	}
	return samples
}

// tailはエラーに添える、標準エラー出力の末尾。
func tail(s string) string {
	const n = 1000
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return "\n" + s
}

// Trackerは使用率を記録から読み、古ければ取得し直して記録する。
type Tracker struct {
	Log     Log
	Fetcher Fetcher
	// StaleAfterは記録を古いとみなす経過時間。0ならDefaultStaleAfterを使う。
	StaleAfter time.Duration
	// Nowは現在時刻。nilならtime.Nowを使う。
	Now func() time.Time
}

func (t Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Currentはtoolの枠ごとの最新の観測を返す。記録が無い・古い・リセットを
// 過ぎた枠があるときは取得し直す。
func (t Tracker) Current(ctx context.Context, tool agent.Tool) (map[Window]Sample, error) {
	samples, _, err := t.Log.Read()
	if err != nil {
		return nil, err
	}
	latest := Latest(samples, tool)
	if !t.stale(latest) {
		return latest, nil
	}
	return t.Refresh(ctx, tool)
}

// stale は観測を取得し直すべきかを返す。
func (t Tracker) stale(latest map[Window]Sample) bool {
	if len(latest) == 0 {
		return true
	}
	now := t.now()
	staleAfter := cmp.Or(t.StaleAfter, DefaultStaleAfter)
	for _, s := range latest {
		if now.Sub(s.At) > staleAfter || s.resetPassed(now) {
			return true
		}
	}
	return false
}

// Refreshはtoolの使用率を取得し直して記録し、枠ごとの観測を返す。
// 使用量の上限に当たって失敗したステップのあとなど、記録の新しさによらず取り直すときに使う。
func (t Tracker) Refresh(ctx context.Context, tool agent.Tool) (map[Window]Sample, error) {
	now := t.now()
	samples, err := t.Fetcher.Fetch(ctx, tool, now)
	var fallback *FallbackError
	if err != nil && !errors.As(err, &fallback) {
		return nil, err
	}
	latest := Latest(samples, tool)
	if err := t.checkFetched(latest, now); err != nil {
		err = fmt.Errorf("%s の新しい使用率を取得できませんでした: %w", tool, err)
		if fallback != nil {
			err = fmt.Errorf("%w\n%w", err, fallback)
		}
		return nil, err
	}
	if err := t.Log.AppendSamples(samples); err != nil {
		return nil, err
	}
	return latest, nil
}

// checkFetchedは取得した観測latestを現在値として使えるかを確かめる。
// 補助ログは過去の観測を返すため、観測時刻の古さを見る。取得元から直接得た観測は
// リセットを過ぎていてもPercentAtが0%として扱えるが、補助ログはリセット後の使用率が
// 分からないため使わない。
func (t Tracker) checkFetched(latest map[Window]Sample, now time.Time) error {
	if len(latest) == 0 {
		return errNoRateLimits
	}
	staleAfter := cmp.Or(t.StaleAfter, DefaultStaleAfter)
	for _, w := range Windows {
		s, ok := latest[w]
		if !ok {
			continue
		}
		at := s.At.Local().Format(time.RFC3339)
		if now.Sub(s.At) > staleAfter {
			return fmt.Errorf("%sの観測 (%s, %s) が古すぎます", w.Label(), s.Source, at)
		}
		if s.Source == SourceSessionLog && s.resetPassed(now) {
			return fmt.Errorf("%sの観測 (%s, %s) はリセット (%s) より前のものです", w.Label(), s.Source, at, s.ResetsAt.Local().Format(time.RFC3339))
		}
	}
	return nil
}

// RecordClaudeStreamはClaude Codeのステップの出力 (stream-json) から使用率を
// 取り出して記録し、枠ごとの観測を返す。出力に使用率が無ければ空を返す。
func (t Tracker) RecordClaudeStream(r io.Reader) (map[Window]Sample, error) {
	samples, err := ParseClaudeStream(r)
	if err != nil {
		return nil, err
	}
	samples = stamp(samples, t.now(), agent.Claude, SourceStream)
	if err := t.Log.AppendSamples(samples); err != nil {
		return nil, err
	}
	return Latest(samples, agent.Claude), nil
}

// withTimeoutはtimeout (0ならDefaultFetchTimeout) を期限にしてfetchを呼ぶ。
func withTimeout(ctx context.Context, timeout time.Duration, fetch func(context.Context) ([]Sample, error)) ([]Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(timeout, DefaultFetchTimeout))
	defer cancel()
	return fetch(ctx)
}
