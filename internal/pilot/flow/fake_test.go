package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 偽のエージェントの振る舞いを決める環境変数。テストのバイナリ自身を偽の `claude` として
// 起動し、TestMainでこれらを読んで振る舞う。
const (
	envFake     = "KORYLUSPILOT_FAKE_AGENT"
	envScenario = "KORYLUSPILOT_FAKE_SCENARIO" // 呼び出しごとの振る舞い (fakeActionの配列のJSON)
	envState    = "KORYLUSPILOT_FAKE_STATE"    // 呼び出した回数と受け取った引数を書くディレクトリ
	envPlan     = "KORYLUSPILOT_FAKE_PLAN"     // チェックを入れる計画書のパス
	envOutside  = "KORYLUSPILOT_FAKE_OUTSIDE"  // リポジトリの外として書き込むディレクトリ
	envPrivate  = "KORYLUSPILOT_FAKE_PRIVATE"  // 人間のチェックアウトのdocs/private
)

// fakeActionは偽のエージェントの1回の呼び出しの振る舞い。
type fakeAction struct {
	// Filesはクローンに書くファイル (クローンからの相対パス → 内容)。
	Files map[string]string `json:"files,omitempty"`
	// Commitは、書いたあとですべての変更をコミットするかどうか。
	Commit bool `json:"commit,omitempty"`
	// CheckTaskは、計画書でチェックを入れるタスク番号。
	CheckTask string `json:"check_task,omitempty"`
	// Outsideは、リポジトリの外のディレクトリに書くファイル名。
	Outside string `json:"outside,omitempty"`
	// Statusは構造化出力のstatus。空なら構造化出力を返さない (コミットのステップ)。
	Status string `json:"status,omitempty"`
	// RateLimitedは、使用量の上限に当たって失敗するかどうか。
	RateLimited bool `json:"rate_limited,omitempty"`
	// ExitCodeは出力を書いたあとの終了コード。
	ExitCode int `json:"exit_code,omitempty"`
	// Hangは出力を書いたあと終了せず、中断まで待つかどうか。
	Hang bool `json:"hang,omitempty"`
	// ProtectedChangesは構造化出力へ含める保護領域の変更。
	ProtectedChanges json.RawMessage `json:"protected_changes,omitempty"`
	// ProtectedChecksは構造化出力へ含める保護領域の照合。
	ProtectedChecks json.RawMessage `json:"protected_checks,omitempty"`
	// Privateは、人間のチェックアウトのdocs/privateに書くファイル (docs/privateからの相対パス → 内容)。
	// レビュードキュメントの作成に使う。
	Private map[string]string `json:"private,omitempty"`
	// AppendPlanは、計画書の末尾 (テストの計画書では「タスクリスト」節の末尾) に足す内容。
	// フィードバック対応のフェーズの起票に使う。
	AppendPlan string `json:"append_plan,omitempty"`
}

func TestMain(m *testing.M) {
	if os.Getenv(envFake) == "1" {
		if err := fakeAgent(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(99)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAgentは偽の `claude` の本体。呼ばれた順にシナリオの振る舞いを1つずつ使い、
// 受け取った引数を呼び出しの番号ごとのファイルに残す。
func fakeAgent() error {
	stateDir := os.Getenv(envState)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	n := len(entries)
	args, err := json.Marshal(os.Args[1:])
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stateDir, fmt.Sprintf("%03d.json", n)), args, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたディレクトリ
		return err
	}

	var scenario []fakeAction
	if err := json.Unmarshal([]byte(os.Getenv(envScenario)), &scenario); err != nil {
		return err
	}
	if n >= len(scenario) {
		return fmt.Errorf("シナリオに無い%d回目の呼び出し: %v", n+1, os.Args[1:])
	}
	a := scenario[n]

	for name, content := range a.Files {
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			return err
		}
	}
	if a.Commit {
		for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "--allow-empty", "-m", "偽のエージェントのコミット"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //#nosec G204 -- テストの偽のエージェントが決めたgitの引数
				return fmt.Errorf("git %v: %w\n%s", args, err, out)
			}
		}
	}
	if a.CheckTask != "" {
		path := os.Getenv(envPlan)
		b, err := os.ReadFile(path) //#nosec G304 G703 -- テストがt.TempDirの下に決めたパス
		if err != nil {
			return err
		}
		re := regexp.MustCompile(`(?m)^- \[ \] \*\*` + regexp.QuoteMeta(a.CheckTask) + `\*\*`)
		b = re.ReplaceAll(b, []byte("- [x] **"+a.CheckTask+"**"))
		if err := os.WriteFile(path, b, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
			return err
		}
	}
	if a.AppendPlan != "" {
		f, err := os.OpenFile(os.Getenv(envPlan), os.O_APPEND|os.O_WRONLY, 0o600) //#nosec G304 G703 -- テストがt.TempDirの下に決めたパス
		if err != nil {
			return err
		}
		_, err = f.WriteString(a.AppendPlan)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	if a.Outside != "" {
		if err := os.WriteFile(filepath.Join(os.Getenv(envOutside), a.Outside), []byte("x"), 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたディレクトリ
			return err
		}
	}

	for name, content := range a.Private {
		path := filepath.Join(os.Getenv(envPrivate), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたディレクトリ
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
			return err
		}
	}

	output := "null"
	if a.Status != "" {
		output = fmt.Sprintf(`{"status":%q,"summary":"偽のエージェントの要約","escalations":%s,"verification_items":[{"kind":"screen","title":"確認する","target":"/","steps":["開く"],"expected":"表示される"}],"protected_changes":%s,"protected_checks":%s}`,
			a.Status, fakeEscalations(a.Status), orEmpty(a.ProtectedChanges), orEmpty(a.ProtectedChecks))
	}
	var lines []string
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		lines = fakeCodexLines(n, a, output)
	} else {
		lines = fakeClaudeLines(n, a, output)
	}
	_, err = os.Stdout.WriteString(strings.Join(lines, "\n") + "\n")
	if a.Hang {
		time.Sleep(time.Hour)
	}
	if a.RateLimited {
		os.Exit(1)
	}
	if a.ExitCode != 0 {
		os.Exit(a.ExitCode)
	}
	return err
}

// fakeClaudeLinesは、n回目の呼び出しで偽の `claude` が出すstream-jsonの行を返す。
func fakeClaudeLines(n int, a fakeAction, output string) []string {
	session := "session-" + strconv.Itoa(n+1)
	if i := slices.Index(os.Args, "--resume"); i >= 0 {
		session = os.Args[i+1]
	}
	lines := []string{
		fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q}`, session),
		fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.2,"resetsAt":1792333200},"seven_day":{"utilization":0.1,"resetsAt":1792900000}}},"session_id":%q}`, session),
	}
	if a.RateLimited {
		return append(lines, fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"},"session_id":%q}`, session))
	}
	return append(lines, fmt.Sprintf(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"num_turns":3,"total_cost_usd":0.25,"structured_output":%s}`, session, output))
}

// fakeCodexLinesは、n回目の呼び出しで偽の `codex exec` が出すJSONLの行を返す。
// 再開 (`codex exec resume ... <セッションID> <プロンプト>`) では、受け取ったセッションIDを使う。
func fakeCodexLines(n int, a fakeAction, output string) []string {
	session := "thread-" + strconv.Itoa(n+1)
	if slices.Contains(os.Args, "resume") {
		session = os.Args[len(os.Args)-2]
	}
	lines := []string{fmt.Sprintf(`{"type":"thread.started","thread_id":%q}`, session)}
	if a.RateLimited {
		return append(lines, `{"type":"turn.failed","error":{"message":"You've hit your usage limit."}}`)
	}
	text, _ := json.Marshal(output) // 文字列のエンコードは失敗しない。
	return append(lines,
		fmt.Sprintf(`{"type":"item.completed","item":{"type":"agent_message","text":%s}}`, text),
		`{"type":"turn.completed"}`,
	)
}

func orEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "[]"
	}
	return string(raw)
}

func fakeEscalations(status string) string {
	if status != "escalated" {
		return "[]"
	}
	return `[{"reason":"設計と異なるアプローチが要る","question":"どちらにするか","options":["A","B"],"recommended":"A"}]`
}

// fakeCallsは偽のエージェントが受け取った引数を、呼ばれた順に返す。
func fakeCalls(t *testing.T, stateDir string) [][]string {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	calls := make([][]string, 0, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(stateDir, e.Name())) //#nosec G304 -- t.TempDirの下のファイル
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		if err := json.Unmarshal(b, &args); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, args)
	}
	return calls
}

// promptOfは引数の最後 (プロンプト) を返す。
func promptOf(args []string) string {
	return args[len(args)-1]
}

// flagValueは引数の中のフラグnameの値を返す。無ければ空を返す。
func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}
