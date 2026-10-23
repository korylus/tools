package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

const testPlan = `# 計画書

## タスクリスト

- [x] **1-1**: [/korylus-guidelines] 接頭辞を改める
- [ ] **2-1**: [/korylus-tools] 共有のパッケージに移す
- [ ] **2-2**: [shimbaco] タグを付ける
`

// writePlanはt.TempDirの下にtestPlanを書き、そのパスを返す。
func writePlan(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "korylusflow.md")
	if err := os.WriteFile(path, []byte(testPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertContainsはoutがwantsをすべて含むことを確かめる。
func assertContains(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out)
		}
	}
}

func TestStatusNotStarted(t *testing.T) {
	t.Parallel()

	planPath := writePlan(t)
	var stdout, stderr bytes.Buffer
	if code := status([]string{planPath}, t.TempDir(), time.Now(), &stdout, &stderr); code != exitOK {
		t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
	}
	assertContains(t, stdout.String(),
		"タスク: 1 / 3 完了",
		"次のタスク: 2-1 [/korylus-tools] 共有のパッケージに移す",
		"状態: 未実行",
		"  [x] 1-1 [/korylus-guidelines] 接頭辞を改める",
		"  [ ] 2-2 [shimbaco] タグを付ける",
	)
	if strings.Contains(stdout.String(), "所要時間:") {
		t.Errorf("記録が無いのに所要時間を表示した:\n%s", stdout.String())
	}
}

func TestStatusPendingCompletion(t *testing.T) {
	t.Parallel()

	planPath := writePlan(t)
	stateRoot := t.TempDir()
	dir := state.NewDir(stateRoot, "korylusflow")
	now := time.Now()
	st := &state.State{Pending: &state.Event{
		Task: "2-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: now.Add(-time.Minute), Ended: now,
	}}
	if err := dir.Save(st, now); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := status([]string{planPath}, stateRoot, now, &stdout, &stderr); code != exitOK {
		t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
	}
	assertContains(t, stdout.String(), "状態: タスク 2-1 の実装の完了を記録中 (waitかdoneで再開できます)")
	if strings.Contains(stdout.String(), "状態: 進めているタスクなし") {
		t.Errorf("完了の記録中なのにタスクなしと表示した:\n%s", stdout.String())
	}
}

func TestStatusWaitingForHuman(t *testing.T) {
	t.Parallel()

	planPath := writePlan(t)
	stateRoot := t.TempDir()
	dir := state.NewDir(stateRoot, "korylusflow")
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &state.State{
		Task:  "2-1",
		Step:  state.StepAsk,
		Role:  state.RoleReviewerCodex,
		Since: now.Add(-90 * time.Minute),
		Ask:   &state.Ask{Reason: "要人間判断の指摘があります", Details: []string{"internal/plan/plan.go の命名"}},
		Tasks: []state.TaskRecord{{
			ID: "2-1", Repo: "/korylus-tools", Dir: "/korylus-tools", Branch: "korylusflow-2-1", Base: "main",
			Reviews: []state.ReviewRecord{{Number: 1, Role: state.RoleReviewerCodex, Verdict: "Request Changes"}},
		}},
	}
	if err := dir.Save(st, now); err != nil {
		t.Fatal(err)
	}
	start := now.Add(-3 * time.Hour)
	for _, e := range []state.Event{
		{Task: "2-1", Step: state.StepImplement, Role: state.RoleImplementer, Started: start, Ended: start.Add(70 * time.Minute)},
		{Task: "2-1", Step: state.StepReview, Role: state.RoleReviewerCodex, Started: start.Add(70 * time.Minute), Ended: start.Add(90 * time.Minute)},
		{Task: "2-1", Step: state.StepAsk, Role: state.RoleReviewerCodex, Started: start.Add(90 * time.Minute), Ended: start.Add(100 * time.Minute)},
	} {
		if err := dir.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := status([]string{planPath}, stateRoot, now, &stdout, &stderr); code != exitOK {
		t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
	}
	assertContains(t, stdout.String(),
		"状態: 人を待っています (タスク 2-1 の人への確認)",
		"番の役割: レビュアー (Codex) (reviewer-codex)",
		"(経過 1時間30分)",
		"リポジトリ: /korylus-tools (作業するディレクトリ: /korylus-tools)",
		"ブランチ: korylusflow-2-1 (ベース: main)",
		"レビュー: 1回 (最新の評価: Request Changes)",
		"人を待っている理由: 要人間判断の指摘があります",
		"  - internal/plan/plan.go の命名",
		"  合計: 1時間40分",
		"  実装者: 1時間10分",
		"  レビュアー (Codex): 20分",
		"  人を待った時間: 10分",
	)
	if strings.Contains(stdout.String(), "レビュアー (Claude Code):") {
		t.Errorf("記録の無い役割の所要時間を表示した:\n%s", stdout.String())
	}
}

func TestStatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       func(t *testing.T) []string
		wantStderr string
	}{
		{name: "引数が無いと使い方を表示", args: func(*testing.T) []string { return nil }, wantStderr: "使い方: korylusflow status"},
		{
			name:       "計画書が無いとエラー",
			args:       func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "none.md")} },
			wantStderr: "計画書を読めません",
		},
		{
			name: "タスクリストが無いとエラー",
			args: func(t *testing.T) []string {
				path := filepath.Join(t.TempDir(), "empty.md")
				if err := os.WriteFile(path, []byte("# 計画書\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return []string{path}
			},
			wantStderr: "を読み取れません",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := status(tt.args(t), t.TempDir(), time.Now(), &stdout, &stderr); code != exitError {
				t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
			}
			assertContains(t, stderr.String(), tt.wantStderr)
		})
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 20 * time.Second, want: "0分"},
		{in: 45 * time.Minute, want: "45分"},
		{in: 2*time.Hour + 5*time.Minute + 40*time.Second, want: "2時間6分"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.in); got != tt.want {
			t.Errorf("formatDuration(%v) = %q、期待値 = %q", tt.in, got, tt.want)
		}
	}
}
