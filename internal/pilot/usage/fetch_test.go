package usage

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
)

// 偽の `claude` / `codex` の振る舞いを決める環境変数。テストのバイナリ自身を
// 偽のコマンドとして起動し、TestMainでこれらを読んで振る舞う。
const (
	envFake       = "KORYLUSPILOT_FAKE_USAGE"
	envFakeStdout = "KORYLUSPILOT_FAKE_STDOUT" // claude: 標準出力に書く内容。codex: 使用率の要求への応答
	envFakeExit   = "KORYLUSPILOT_FAKE_EXIT"   // 終了コード
	envFakeArgs   = "KORYLUSPILOT_FAKE_ARGS"   // 受け取った引数を書くファイル
)

// 偽のコマンドの振る舞い。
const (
	fakeClaude = "claude"
	// fakeCodexはapp-serverのように標準入力から要求を読み、使用率の要求に応答してから
	// 標準入力が閉じるまで待ち受ける。終了コードを指定すると、応答したあとすぐに終わる。
	fakeCodex = "codex"
	// fakeCodexSilentは要求に応答せずに待ち受け続ける。
	fakeCodexSilent = "codex-silent"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(envFake); mode != "" {
		os.Exit(fakeCommand(mode))
	}
	os.Exit(m.Run())
}

func fakeCommand(mode string) int {
	if path := os.Getenv(envFakeArgs); path != "" {
		if err := os.WriteFile(path, []byte(strings.Join(os.Args[1:], " ")), 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
			return 99
		}
	}
	code, _ := strconv.Atoi(os.Getenv(envFakeExit))
	if mode == fakeClaude {
		_, _ = os.Stdout.WriteString(os.Getenv(envFakeStdout))
		return code
	}

	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch {
		case mode == fakeCodexSilent:
		case strings.Contains(sc.Text(), `"initialize"`):
			_, _ = os.Stdout.WriteString(`{"id":1,"result":{"userAgent":"fake"}}` + "\n")
		case strings.Contains(sc.Text(), "account/rateLimits/read"):
			_, _ = os.Stdout.WriteString(`{"method":"configWarning","params":{}}` + "\n")
			_, _ = os.Stdout.WriteString(os.Getenv(envFakeStdout) + "\n")
			// 終了コードを指定されたら、待ち受けをやめて異常終了する。
			if code != 0 {
				return code
			}
		}
	}
	return code
}

// fakeEnvは偽のコマンドを起動する環境を返す。
func fakeEnv(mode, stdout string, exit int, argsPath string) []string {
	return append(os.Environ(),
		envFake+"="+mode,
		envFakeStdout+"="+stdout,
		envFakeExit+"="+strconv.Itoa(exit),
		envFakeArgs+"="+argsPath,
	)
}

func testExecutable(t *testing.T) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestCommandFetcher_Claude(t *testing.T) {
	t.Parallel()

	argsPath := filepath.Join(t.TempDir(), "args")
	f := CommandFetcher{ClaudeCommand: testExecutable(t), Claude: runner.Runner{Env: fakeEnv(fakeClaude, usageOutput, 0, argsPath)}}
	now := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)

	got, err := f.Fetch(context.Background(), agent.Claude, now)
	if err != nil {
		t.Fatalf("Fetchのエラー = %v", err)
	}
	want := []Sample{
		{At: now, Tool: agent.Claude, Window: FiveHour, Percent: 3, ResetsAt: time.Date(2026, 10, 19, 12, 10, 0, 0, time.UTC), Source: SourceUsageCommand},
		{At: now, Tool: agent.Claude, Window: Weekly, Percent: 19, ResetsAt: time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC), Source: SourceUsageCommand},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fetch() = %+v、期待値 = %+v", got, want)
	}
	args, err := os.ReadFile(argsPath) //#nosec G304 -- テストがt.TempDirの下に決めたパス
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "-p /usage --output-format json" {
		t.Errorf("引数 = %q、期待値 = %q", args, "-p /usage --output-format json")
	}
}

func TestCommandFetcher_ClaudeFailure(t *testing.T) {
	t.Parallel()

	f := CommandFetcher{ClaudeCommand: testExecutable(t), Claude: runner.Runner{Env: fakeEnv(fakeClaude, "", 1, "")}}
	if _, err := f.Fetch(context.Background(), agent.Claude, time.Now()); err == nil || !strings.Contains(err.Error(), "/usage が失敗しました") {
		t.Errorf("Fetchのエラー = %v、/usage の失敗を期待", err)
	}
}

func TestCommandFetcher_CodexAppServer(t *testing.T) {
	t.Parallel()

	argsPath := filepath.Join(t.TempDir(), "args")
	f := CommandFetcher{CodexCommand: testExecutable(t), CodexHome: t.TempDir(), Codex: runner.Runner{Env: fakeEnv(fakeCodex, appServerResponse, 0, argsPath)}}
	now := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)

	got, err := f.Fetch(context.Background(), agent.Codex, now)
	if err != nil {
		t.Fatalf("Fetchのエラー = %v", err)
	}
	want := []Sample{{At: now, Tool: agent.Codex, Window: Weekly, Percent: 5, ResetsAt: time.Unix(1792904083, 0).UTC(), Source: SourceAppServer}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fetch() = %+v、期待値 = %+v", got, want)
	}
	args, err := os.ReadFile(argsPath) //#nosec G304 -- テストがt.TempDirの下に決めたパス
	if err != nil {
		t.Fatal(err)
	}
	if string(args) != "app-server" {
		t.Errorf("引数 = %q、期待値 = %q", args, "app-server")
	}
}

// writeSessionLogはhomeの下にCodexのセッションログを書く。
func writeSessionLog(t *testing.T, home, name, src string, mod time.Time) {
	t.Helper()

	dir := filepath.Join(home, "sessions", "2026", "10", "19")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestCommandFetcher_CodexSessionLogFallback(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	// 最後に更新されたログを読む (ファイル名の日付ではなく更新時刻で選ぶ)。
	writeSessionLog(t, home, "rollout-b.jsonl", strings.ReplaceAll(sessionLog, "5.0", "9.0"), baseTime.Add(-time.Hour))
	writeSessionLog(t, home, "rollout-a.jsonl", sessionLog, baseTime)

	tests := []struct {
		name      string
		mode      string
		exit      int
		wantCause string
	}{
		{name: "app-serverが使用率を返さずに終わる", mode: fakeCodex, exit: 1, wantCause: "使用率を返さずに終了しました"},
		{name: "app-serverが応答しない", mode: fakeCodexSilent, wantCause: "応答しませんでした"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdout := `{"id":2,"error":{"message":"not logged in"}}`
			if tt.exit != 0 {
				stdout = `{"id":3,"result":{}}`
			}
			f := CommandFetcher{
				CodexCommand: testExecutable(t),
				// セッションログはランナーの中の環境 (CODEX_HOME) で探す。
				Codex: runner.Runner{
					Prefix: []string{"env", "CODEX_HOME=" + home},
					Env:    fakeEnv(tt.mode, stdout, tt.exit, ""),
				},
				Timeout: 500 * time.Millisecond,
			}
			got, err := f.Fetch(context.Background(), agent.Codex, baseTime)
			// 補ったことと、app-serverの失敗の理由を観測と併せて返す。
			var fallback *FallbackError
			if !errors.As(err, &fallback) {
				t.Fatalf("Fetchのエラー = %v、*FallbackErrorを期待", err)
			}
			if fallback.Source != SourceSessionLog || !strings.Contains(fallback.Err.Error(), tt.wantCause) {
				t.Errorf("FallbackError = %+v、取得元 = %s・理由に %q を含むことを期待", fallback, SourceSessionLog, tt.wantCause)
			}
			want := []Sample{{
				At:       time.Date(2026, 10, 19, 8, 20, 58, 317000000, time.UTC),
				Tool:     agent.Codex,
				Window:   Weekly,
				Percent:  5,
				ResetsAt: time.Unix(1792904083, 0).UTC(),
				Source:   SourceSessionLog,
			}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Fetch() = %+v、期待値 = %+v", got, want)
			}
		})
	}
}

func TestCommandFetcher_CodexFailure(t *testing.T) {
	t.Parallel()

	f := CommandFetcher{
		CodexCommand: testExecutable(t),
		CodexHome:    t.TempDir(),
		Codex:        runner.Runner{Env: fakeEnv(fakeCodex, `{"id":2,"error":{"message":"not logged in"}}`, 0, "")},
	}
	_, err := f.Fetch(context.Background(), agent.Codex, time.Now())
	if err == nil {
		t.Fatal("エラーを期待したが、nilだった")
	}
	for _, want := range []string{"not logged in", "セッションログ"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("エラー = %q、含まれることを期待した文字列 = %q", err.Error(), want)
		}
	}
}

func TestTracker_CodexSessionLogFreshness(t *testing.T) {
	t.Parallel()

	now := baseTime
	tests := []struct {
		name       string
		age        time.Duration
		staleAfter time.Duration
		reset      time.Time
		wantErr    bool
	}{
		{name: "新しい補助ログは採用する", age: 5 * time.Minute, reset: resetB},
		{name: "有効期限ちょうどの補助ログは採用する", age: DefaultStaleAfter, reset: resetB},
		{name: "古い補助ログは拒否する", age: DefaultStaleAfter + time.Second, reset: resetB, wantErr: true},
		{name: "設定した短い有効期限を過ぎた補助ログは拒否する", age: 5 * time.Minute, staleAfter: time.Minute, reset: resetB, wantErr: true},
		{name: "設定した長い有効期限内の補助ログは採用する", age: 15 * time.Minute, staleAfter: 20 * time.Minute, reset: resetB},
		{name: "リセット直後の補助ログは拒否する", age: time.Minute, reset: now.Add(-time.Second), wantErr: true},
		{name: "リセット時刻ちょうどの補助ログは拒否する", age: time.Minute, reset: now, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			at := now.Add(-tt.age)
			src := strings.ReplaceAll(sessionLog, "2026-10-19T08:20:58.317Z", at.Format(time.RFC3339Nano))
			src = strings.ReplaceAll(src, "1792904083", strconv.FormatInt(tt.reset.Unix(), 10))
			// ファイルの更新が新しくても、使用率を観測した時刻で判定する。
			writeSessionLog(t, home, "rollout.jsonl", src, now)
			fetcher := CommandFetcher{
				CodexCommand: testExecutable(t),
				CodexHome:    home,
				Codex:        runner.Runner{Env: fakeEnv(fakeCodex, `{"id":2,"error":{"message":"unavailable"}}`, 0, "")},
			}
			log := NewLog(t.TempDir())
			tracker := Tracker{Log: log, Fetcher: fetcher, StaleAfter: tt.staleAfter, Now: func() time.Time { return now }}

			got, err := tracker.Current(context.Background(), agent.Codex)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "新しい使用率を取得できませんでした") {
					t.Fatalf("Currentのエラー = %v、古い観測の拒否を期待", err)
				}
				// 拒否した観測の時刻と、app-serverの失敗の理由を伝える。
				for _, want := range []string{at.Local().Format(time.RFC3339), "unavailable"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Currentのエラー = %q、含まれることを期待した文字列 = %q", err.Error(), want)
					}
				}
				var fallback *FallbackError
				if !errors.As(err, &fallback) {
					t.Errorf("Currentのエラー = %v、*FallbackErrorを含むことを期待", err)
				}
				if got != nil {
					t.Errorf("Current() = %+v、拒否時は観測を返さないことを期待", got)
				}
				if _, err := os.Stat(log.Path()); !os.IsNotExist(err) {
					t.Errorf("拒否した観測の記録ファイルのエラー = %v、ファイルが無いことを期待", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Currentのエラー = %v", err)
			}
			want := Sample{At: at, Tool: agent.Codex, Window: Weekly, Percent: 5, ResetsAt: tt.reset, Source: SourceSessionLog}
			if got[Weekly] != want {
				t.Errorf("週枠の観測 = %+v、期待値 = %+v", got[Weekly], want)
			}
			samples, _, err := log.Read()
			if err != nil || !reflect.DeepEqual(samples, []Sample{want}) {
				t.Errorf("Read() = (%+v, %v)、期待する観測 = %+v", samples, err, want)
			}
		})
	}
}

func TestTracker_FetchedResetPassed(t *testing.T) {
	t.Parallel()

	// usageOutputの5時間枠は「resets Oct 19, 12:10pm (UTC)」。リセットまで待って
	// 取り直した直後も、/usage が過ぎたリセット時刻を表示していることがある。
	reset := time.Date(2026, 10, 19, 12, 10, 0, 0, time.UTC)
	tests := []struct {
		name string
		now  time.Time
	}{
		{name: "リセット時刻ちょうど", now: reset},
		{name: "リセットの20分後", now: reset.Add(20 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := CommandFetcher{ClaudeCommand: testExecutable(t), Claude: runner.Runner{Env: fakeEnv(fakeClaude, usageOutput, 0, "")}}
			log := NewLog(t.TempDir())
			tracker := Tracker{Log: log, Fetcher: fetcher, Now: func() time.Time { return tt.now }}

			got, err := tracker.Current(context.Background(), agent.Claude)
			if err != nil {
				t.Fatalf("Currentのエラー = %v", err)
			}
			s := got[FiveHour]
			if s.ResetsAt != reset || s.Source != SourceUsageCommand {
				t.Fatalf("5時間枠の観測 = %+v、リセット時刻 = %v・取得元 = %s を期待", s, reset, SourceUsageCommand)
			}
			if p := s.PercentAt(tt.now); p != 0 {
				t.Errorf("5時間枠のPercentAt = %g、リセット後なので0を期待", p)
			}
			samples, _, err := log.Read()
			if err != nil || len(samples) != 2 {
				t.Errorf("Read() = (%+v, %v)、2件の記録を期待", samples, err)
			}

			cfg := config.DefaultUsage()
			cfg.FiveHourCap = 1
			if d := Check(cfg, got, map[Window]float64{FiveHour: 10, Weekly: 2}, tt.now); d.Wait {
				t.Errorf("Check() = %+v、リセット後なので待たないことを期待", d)
			}
		})
	}
}
