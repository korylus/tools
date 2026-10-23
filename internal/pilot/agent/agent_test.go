package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/runner"
)

// 偽のエージェントの振る舞いを決める環境変数。テストのバイナリ自身を
// 偽の `claude` / `codex` として起動し、TestMainでこれらを読んで振る舞う。
const (
	envFake       = "KORYLUSPILOT_FAKE_AGENT"
	envFakeRecord = "KORYLUSPILOT_FAKE_RECORD" // 受け取った引数・環境・スキーマを書くファイル
	envFakeStdout = "KORYLUSPILOT_FAKE_STDOUT" // 標準出力にそのまま書く内容
	envFakeStderr = "KORYLUSPILOT_FAKE_STDERR" // 標準エラー出力にそのまま書く内容
	envFakeExit   = "KORYLUSPILOT_FAKE_EXIT"   // 終了コード
	envFakeSleep  = "KORYLUSPILOT_FAKE_SLEEP"  // 終了するまでに待つ時間
	// 標準出力の末尾に書く1行のバイト数。環境変数の値の長さの上限を超える出力を作るために使う。
	envFakeLongLine = "KORYLUSPILOT_FAKE_LONG_LINE"
)

// fakeRecordは偽のエージェントが受け取ったもの。
type fakeRecord struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	// Schemaは `--output-schema` で渡されたファイルの内容。
	Schema string `json:"schema"`
}

func TestMain(m *testing.M) {
	if os.Getenv(envFake) == "1" {
		os.Exit(fakeAgent())
	}
	os.Exit(m.Run())
}

// fakeAgentは偽の `claude` / `codex` の本体。
func fakeAgent() int {
	rec := fakeRecord{Args: os.Args[1:], Env: os.Environ()}
	if i := slices.Index(rec.Args, "--output-schema"); i >= 0 && i+1 < len(rec.Args) {
		b, err := os.ReadFile(rec.Args[i+1]) //#nosec G703 -- テスト対象のRunnerが渡したスキーマの一時ファイルを読む
		if err != nil {
			return 99
		}
		rec.Schema = string(b)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return 99
	}
	if err := os.WriteFile(os.Getenv(envFakeRecord), b, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		return 99
	}

	if d, err := time.ParseDuration(os.Getenv(envFakeSleep)); err == nil {
		time.Sleep(d)
	}
	_, _ = os.Stdout.WriteString(os.Getenv(envFakeStdout))
	if n, err := strconv.Atoi(os.Getenv(envFakeLongLine)); err == nil {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), n))
	}
	_, _ = os.Stderr.WriteString(os.Getenv(envFakeStderr))
	code, _ := strconv.Atoi(os.Getenv(envFakeExit))
	return code
}

// fakeは偽のエージェントの振る舞い。
type fake struct {
	stdout string
	stderr string
	exit   int
	sleep  time.Duration
}

// newFakeRunnerは偽のエージェントを起動するRunnerと、受け取ったものを
// 書くファイルのパスを返す。extraEnvは親の環境に足す環境変数。
func newFakeRunner(t *testing.T, f fake, extraEnv ...string) (Runner, string) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.json")
	env := append(os.Environ(),
		envFake+"=1",
		envFakeRecord+"="+record,
		envFakeStdout+"="+f.stdout,
		envFakeStderr+"="+f.stderr,
		envFakeExit+"="+strconv.Itoa(f.exit),
		envFakeSleep+"="+f.sleep.String(),
	)
	return Runner{
		ClaudeCommand: exe,
		CodexCommand:  exe,
		Exec:          runner.Runner{Env: append(env, extraEnv...)},
	}, record
}

// readRecordは偽のエージェントが受け取ったものを読む。
func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()

	b, err := os.ReadFile(path) //#nosec G304 -- テストがt.TempDirの下に決めたパス
	if err != nil {
		t.Fatalf("偽のエージェントが起動されなかった: %v", err)
	}
	var rec fakeRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// envValueは環境の一覧からkeyの値を返す。
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

const claudeOutput = `{"type":"system","subtype":"init","session_id":"claude-session","model":"claude-opus-5-5"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.06,"resetsAt":1792333200}}},"session_id":"claude-session"}
{"type":"result","subtype":"success","is_error":false,"session_id":"claude-session","result":"{\"status\":\"completed\"}","structured_output":{"status":"completed","summary":"実装した","escalations":[],"verification_items":[]}}
`

const codexOutput = `{"type":"thread.started","thread_id":"codex-thread"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"status\":\"completed\",\"summary\":\"レビューした\",\"escalations\":[],\"verification_items\":[]}"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`

func TestRunner_Claude(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      Request
		wantArgs []string
	}{
		{
			name: "新しいセッションをモデル・effort・構造化出力つきで起動する",
			req: Request{
				Tool:   Claude,
				Model:  "claude-opus-5-5",
				Effort: "medium",
				Prompt: "/korylus-do-plan-task docs/plan.md 2-2 mode=unattended",
				Schema: []byte(`{"type":"object"}`),
			},
			wantArgs: []string{
				"-p", "--output-format", "stream-json", "--verbose",
				"--permission-mode", "auto",
				"--model", "claude-opus-5-5", "--effort", "medium",
				"--json-schema", `{"type":"object"}`,
				"/korylus-do-plan-task docs/plan.md 2-2 mode=unattended",
			},
		},
		{
			name: "セッションを再開し、モデル・effortを省略するとツールの既定値に任せる",
			req: Request{
				Tool:      Claude,
				Prompt:    "続けてください",
				SessionID: "claude-session",
			},
			wantArgs: []string{
				"-p", "--output-format", "stream-json", "--verbose",
				"--permission-mode", "auto",
				"--resume", "claude-session",
				"続けてください",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, record := newFakeRunner(t, fake{stdout: claudeOutput})
			// 役割のランナーの代わりに、環境を足してからコマンドを起動する接頭辞を使う。
			r.Exec.Prefix = []string{"env", "KORYLUSPILOT_FAKE_PREFIX=implementer"}
			tt.req.LogPath = filepath.Join(t.TempDir(), "logs", "2-2-implement.jsonl")
			res, err := r.Run(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Runのエラー = %v", err)
			}

			rec := readRecord(t, record)
			if !reflect.DeepEqual(rec.Args, tt.wantArgs) {
				t.Errorf("引数 = %q\n期待値 = %q", rec.Args, tt.wantArgs)
			}
			if v, _ := envValue(rec.Env, "KORYLUSPILOT_FAKE_PREFIX"); v != "implementer" {
				t.Errorf("KORYLUSPILOT_FAKE_PREFIX = %q、ランナーの接頭辞を通して起動することを期待", v)
			}

			if res.SessionID != "claude-session" {
				t.Errorf("SessionID = %q、期待値 = %q", res.SessionID, "claude-session")
			}
			if tt.req.Schema == nil {
				if res.Output != nil {
					t.Errorf("スキーマを渡さないときのOutput = %s、期待値 = nil", res.Output)
				}
			} else if got, err := DecodeUnattended(res.Output); err != nil || got.Summary != "実装した" {
				t.Errorf("Output = %s、DecodeUnattendedのエラー = %v", res.Output, err)
			}

			log, err := os.ReadFile(tt.req.LogPath)
			if err != nil {
				t.Fatalf("ログが保存されなかった: %v", err)
			}
			if string(log) != claudeOutput {
				t.Errorf("ログ = %q、期待値 = 標準出力のそのままの内容", log)
			}
		})
	}
}

func TestRunner_Codex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      Request
		wantArgs func(schemaPath string) []string
	}{
		{
			name: "新しいセッションをレビューの設定で起動する",
			req: Request{
				Tool:    Codex,
				Model:   "gpt-6-astra",
				Effort:  "xhigh",
				Sandbox: "danger-full-access",
				Prompt:  "$korylus-review base=koryluspilot-2-1 mode=unattended",
				Schema:  []byte(`{"type":"object"}`),
			},
			wantArgs: func(schemaPath string) []string {
				return []string{
					"exec", "--json", "-m", "gpt-6-astra",
					"-c", `model_reasoning_effort="xhigh"`,
					"-c", `sandbox_mode="danger-full-access"`,
					"-c", `approval_policy="never"`,
					"--output-schema", schemaPath,
					"$korylus-review base=koryluspilot-2-1 mode=unattended",
				}
			},
		},
		{
			name: "再開でもモデル・サンドボックスを渡し、effortを通常の設定に下げる",
			req: Request{
				Tool:      Codex,
				Model:     "gpt-6-astra",
				Effort:    "medium",
				Sandbox:   "danger-full-access",
				Prompt:    "回答した対応方針に沿って修正してください",
				SessionID: "codex-thread",
				Schema:    []byte(`{"type":"object"}`),
			},
			wantArgs: func(schemaPath string) []string {
				return []string{
					"exec", "resume", "--json", "-m", "gpt-6-astra",
					"-c", `model_reasoning_effort="medium"`,
					"-c", `sandbox_mode="danger-full-access"`,
					"-c", `approval_policy="never"`,
					"--output-schema", schemaPath,
					"codex-thread",
					"回答した対応方針に沿って修正してください",
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, record := newFakeRunner(t, fake{stdout: codexOutput})
			tt.req.LogPath = filepath.Join(t.TempDir(), "2-2-review-1.jsonl")
			res, err := r.Run(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Runのエラー = %v", err)
			}

			rec := readRecord(t, record)
			i := slices.Index(rec.Args, "--output-schema")
			if i < 0 {
				t.Fatalf("--output-schemaが渡されなかった: %q", rec.Args)
			}
			schemaPath := rec.Args[i+1]
			if want := tt.wantArgs(schemaPath); !reflect.DeepEqual(rec.Args, want) {
				t.Errorf("引数 = %q\n期待値 = %q", rec.Args, want)
			}
			if rec.Schema != string(tt.req.Schema) {
				t.Errorf("スキーマのファイルの内容 = %q、期待値 = %q", rec.Schema, tt.req.Schema)
			}
			if _, err := os.Stat(schemaPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("スキーマの一時ファイルが残った: %s", schemaPath)
			}

			if res.SessionID != "codex-thread" {
				t.Errorf("SessionID = %q、期待値 = %q", res.SessionID, "codex-thread")
			}
			if got, err := DecodeUnattended(res.Output); err != nil || got.Summary != "レビューした" {
				t.Errorf("Output = %s、DecodeUnattendedのエラー = %v", res.Output, err)
			}
		})
	}
}

func TestRunner_Failures(t *testing.T) {
	t.Parallel()

	schema := []byte(`{"type":"object"}`)
	tests := []struct {
		name            string
		tool            Tool
		fake            fake
		timeout         time.Duration
		wantErr         error  // errors.Isで照合するエラー。nilなら照合しない
		wantMsg         string // エラーメッセージに含まれることを期待する文字列
		wantSession     string
		wantRateLimited bool
	}{
		{
			name: "Claude Codeが上限に当たると、セッションIDを返してErrRateLimitedにする",
			tool: Claude,
			fake: fake{
				stdout: `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"},"session_id":"s1"}
{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s1","result":"You've hit your limit · resets 3pm"}
`,
				exit: 1,
			},
			wantErr:         ErrRateLimited,
			wantMsg:         "hit your limit",
			wantSession:     "s1",
			wantRateLimited: true,
		},
		{
			name: "Codexが上限に当たると、ErrRateLimitedにする",
			tool: Codex,
			fake: fake{
				stdout: `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again later."}}
`,
				exit: 1,
			},
			wantErr:         ErrRateLimited,
			wantSession:     "t1",
			wantRateLimited: true,
		},
		{
			name: "Claude Codeが報告した失敗をエラーにする",
			tool: Claude,
			fake: fake{
				stdout: `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s2","result":""}` + "\n",
			},
			wantMsg:     "error_max_turns",
			wantSession: "s2",
		},
		{
			name: "異常終了したときは標準エラー出力の末尾を添える",
			tool: Codex,
			fake: fake{
				stdout: `{"type":"thread.started","thread_id":"t2"}` + "\n",
				stderr: "Error: unexpected argument '--foo'",
				exit:   2,
			},
			wantMsg:     "unexpected argument",
			wantSession: "t2",
		},
		{
			name:        "構造化出力が無ければエラーにする",
			tool:        Claude,
			fake:        fake{stdout: `{"type":"result","subtype":"success","is_error":false,"session_id":"s3","result":"done"}` + "\n"},
			wantMsg:     "構造化出力がありません",
			wantSession: "s3",
		},
		{
			name:    "タイムアウトしたらプロセスを止めてErrTimeoutにする",
			tool:    Claude,
			fake:    fake{sleep: time.Minute},
			timeout: 200 * time.Millisecond,
			wantErr: ErrTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, _ := newFakeRunner(t, tt.fake)
			start := time.Now()
			res, err := r.Run(context.Background(), Request{
				Tool:    tt.tool,
				Prompt:  "p",
				Schema:  schema,
				LogPath: filepath.Join(t.TempDir(), "step.jsonl"),
				Timeout: tt.timeout,
			})
			if err == nil {
				t.Fatal("エラーを期待したが、nilだった")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("エラー = %v、期待値 = %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("エラー = %q、含まれることを期待した文字列 = %q", err.Error(), tt.wantMsg)
			}
			if tt.timeout > 0 && time.Since(start) > 30*time.Second {
				t.Errorf("タイムアウトから戻るまでの時間 = %v", time.Since(start))
			}
			if res == nil {
				t.Fatal("プロセスを起動できたのにResultがnilだった")
			}
			if res.SessionID != tt.wantSession {
				t.Errorf("SessionID = %q、期待値 = %q", res.SessionID, tt.wantSession)
			}
			if res.RateLimited != tt.wantRateLimited {
				t.Errorf("RateLimited = %v、期待値 = %v", res.RateLimited, tt.wantRateLimited)
			}
		})
	}
}

func TestRunner_StartFailures(t *testing.T) {
	t.Parallel()

	logPath := filepath.Join(t.TempDir(), "step.jsonl")
	tests := []struct {
		name    string
		runner  Runner
		tool    Tool
		wantMsg string
	}{
		{
			name:    "不明なツール",
			tool:    Tool("gemini"),
			wantMsg: `不明なツール "gemini"`,
		},
		{
			name:    "コマンドが見つからない",
			runner:  Runner{ClaudeCommand: filepath.Join(t.TempDir(), "missing-claude")},
			tool:    Claude,
			wantMsg: "を起動できません",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := tt.runner.Run(context.Background(), Request{Tool: tt.tool, Prompt: "p", LogPath: logPath})
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("エラー = %v、含まれることを期待した文字列 = %q", err, tt.wantMsg)
			}
			if res != nil {
				t.Errorf("起動できなかったときのResult = %+v、期待値 = nil", res)
			}
		})
	}
}

func TestNewRequest(t *testing.T) {
	t.Parallel()

	reviewer := config.Role{Tool: "codex", Model: "gpt-6-astra", Effort: "xhigh", FixEffort: "medium", Sandbox: "danger-full-access"}
	implementer := config.Role{Tool: "claude", Model: "claude-opus-5-5", Effort: "medium"}
	tests := []struct {
		name string
		role config.Role
		kind Kind
		want Request
	}{
		{"レビューはeffortを使う", reviewer, Normal, Request{Tool: Codex, Model: "gpt-6-astra", Effort: "xhigh", Sandbox: "danger-full-access"}},
		{"修正はfix_effortを使う", reviewer, Fix, Request{Tool: Codex, Model: "gpt-6-astra", Effort: "medium", Sandbox: "danger-full-access"}},
		{"実装はeffortを使う", implementer, Normal, Request{Tool: Claude, Model: "claude-opus-5-5", Effort: "medium"}},
		{"fix_effortを省略した修正はeffortを使う", implementer, Fix, Request{Tool: Claude, Model: "claude-opus-5-5", Effort: "medium"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := NewRequest(tt.role, tt.kind); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewRequest() = %+v、期待値 = %+v", got, tt.want)
			}
		})
	}
}

func TestRunner_CodexFinalOutcome(t *testing.T) {
	t.Parallel()

	transient := "{\"type\":\"error\",\"message\":\"Reconnecting... 1/5 (rate limit reached)\"}"
	recovered := strings.Replace(codexOutput, "{\"type\":\"turn.started\"}", "{\"type\":\"turn.started\"}\n"+transient, 1)
	failed := "{\"type\":\"thread.started\",\"thread_id\":\"codex-thread\"}\n" + transient + "\n"
	tests := []struct {
		name            string
		stdout          string
		exit            int
		wantMsg         string
		wantRateLimited bool
	}{
		{
			name:   "接続回復後に正常終了したら成功を返す",
			stdout: recovered,
		},
		{
			name:    "再試行後にターンが失敗したら最終的な理由を返す",
			stdout:  failed + "{\"type\":\"turn.failed\",\"error\":{\"message\":\"connection failed\"}}\n",
			exit:    1,
			wantMsg: "connection failed",
		},
		{
			name:            "完了せずに上限通知だけで終わったら上限のエラーを返す",
			stdout:          failed,
			wantMsg:         "rate limit reached",
			wantRateLimited: true,
		},
		{
			name:    "正常完了イベントの後でも異常終了はエラーを返す",
			stdout:  recovered,
			exit:    1,
			wantMsg: "異常終了",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, _ := newFakeRunner(t, fake{stdout: tt.stdout, exit: tt.exit})
			res, err := r.Run(context.Background(), Request{
				Tool:    Codex,
				Prompt:  "p",
				Schema:  UnattendedSchema,
				LogPath: filepath.Join(t.TempDir(), "step.jsonl"),
			})
			if tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("Runのエラー = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Runのエラー = %v、期待する文面 = %q", err, tt.wantMsg)
			}
			if res == nil {
				t.Fatal("Resultがnilだった")
			}
			if res.SessionID != "codex-thread" {
				t.Errorf("SessionID = %q", res.SessionID)
			}
			if res.RateLimited != tt.wantRateLimited || errors.Is(err, ErrRateLimited) != tt.wantRateLimited {
				t.Errorf("RateLimited = %v、エラー = %v、上限到達の期待値 = %v", res.RateLimited, err, tt.wantRateLimited)
			}
			if tt.wantMsg == "" {
				if output, err := DecodeUnattended(res.Output); err != nil || output.Summary != "レビューした" {
					t.Errorf("Output = %s、エラー = %v", res.Output, err)
				}
			}
		})
	}
}

func TestRunner_ReadFailure(t *testing.T) {
	t.Parallel()

	r, _ := newFakeRunner(t, fake{stdout: `{"type":"system","subtype":"init","session_id":"s1"}` + "\n"}, envFakeLongLine+"="+strconv.Itoa(maxLine+1))
	logPath := filepath.Join(t.TempDir(), "step.jsonl")
	res, err := r.Run(context.Background(), Request{Tool: Claude, Prompt: "p", LogPath: logPath})
	if err == nil || !strings.Contains(err.Error(), "を読み取れません") {
		t.Fatalf("Runのエラー = %v、出力を読み取れないエラーを期待", err)
	}
	if res == nil {
		t.Fatal("出力を読み取れなかったときのResultがnilだった")
	}
	if res.SessionID != "s1" {
		t.Errorf("SessionID = %q、期待値 = %q", res.SessionID, "s1")
	}
}
