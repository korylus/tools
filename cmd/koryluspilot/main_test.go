package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/state"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string // stdoutに含まれることを期待する文字列 (空なら検査しない)
		wantErr  string // stderrに含まれることを期待する文字列 (空なら検査しない)
	}{
		{name: "引数なしはstderrに使い方を出す", args: nil, wantCode: 1, wantErr: "使い方:"},
		{name: "helpはstdoutに使い方を出す", args: []string{"--help"}, wantCode: 0, wantOut: "使い方:"},
		{name: "未知のサブコマンド", args: []string{"bogus"}, wantCode: 1, wantErr: "不明なサブコマンド"},
		{name: "statusは計画書のパスを必須にする", args: []string{"status"}, wantCode: 1, wantErr: "使い方: koryluspilot status"},
		{name: "statusは未知のフラグを弾く", args: []string{"status", "--bogus"}, wantCode: 1, wantErr: "bogus"},
		{name: "status -hは0で終わる", args: []string{"status", "-h"}, wantCode: 0},
		{name: "statusは存在しない計画書をエラーにする", args: []string{"status", "missing.md"}, wantCode: 1, wantErr: "計画書を読めません"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := run(tt.args, &out, &errBuf)
			if code != tt.wantCode {
				t.Errorf("run(%v) の終了コード = %d、期待値 = %d", tt.args, code, tt.wantCode)
			}
			if tt.wantOut != "" && !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("run(%v) の標準出力 = %q、含まれることを期待した文字列 = %q", tt.args, out.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("run(%v) の標準エラー = %q、含まれることを期待した文字列 = %q", tt.args, errBuf.String(), tt.wantErr)
			}
		})
	}
}

// statusPlanはstatusのテストに使う計画書。
const statusPlan = "## タスクリスト\n\n" +
	"- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n" +
	"- [ ] **1-2**: [Rails] 公開範囲を表示する\n" +
	"- [ ] **1-3**: READMEを更新する\n"

// writePlanはdirにnameの計画書を書き、そのパスを返す。
func writePlan(t *testing.T, dir, name, src string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStatus(t *testing.T) {
	t.Parallel()

	jst := time.FixedZone("JST", 9*60*60)
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, jst)

	tests := []struct {
		name    string
		plan    string
		state   *state.State // nilなら状態ファイルを書かない
		want    []string     // 標準出力に含まれることを期待する行
		notWant []string     // 標準出力に含まれないことを期待する文字列
	}{
		{
			name: "未実行の計画書",
			plan: statusPlan,
			want: []string{
				"タスク: 1 / 3 完了",
				"次のタスク: 1-2 [Rails] 公開範囲を表示する",
				"状態: 未実行",
				"  [x] 1-1 [Go] 投稿の公開範囲を保存する",
				"  [ ] 1-3 READMEを更新する",
			},
			notWant: []string{"最終更新"},
		},
		{
			name:  "実行中のタスク",
			plan:  statusPlan,
			state: &state.State{Task: "1-2", Step: state.StepReview, Reviews: 2},
			want: []string{
				"状態: タスク 1-2 のレビュー",
				"レビュー回数: 2",
				"最終更新: 2026-10-18 09:30 +09:00",
			},
			notWant: []string{"止まった理由", "作業報告書"},
		},
		{
			name: "停止中のタスク",
			plan: statusPlan,
			state: &state.State{
				Task: "1-2",
				Step: state.StepGate,
				Stop: &state.Stop{
					Reason:  "宣言の無い保護領域への変更",
					Details: []string{"schema: go/db/migrations/1.sql"},
					At:      at,
				},
				ReportVersion: 1,
			},
			want: []string{
				"状態: 停止中 (タスク 1-2 のゲート検査)",
				"止まった理由: 宣言の無い保護領域への変更",
				"  - schema: go/db/migrations/1.sql",
				"止まった時刻: 2026-10-18 09:30 +09:00",
				"作業報告書: 第1版",
			},
		},
		{
			name:    "タスクに属さないステップの実行中",
			plan:    statusPlan,
			state:   &state.State{Step: state.StepFeedback},
			want:    []string{"状態: フィードバックの起票"},
			notWant: []string{"タスク  ", "実行中のタスクなし"},
		},
		{
			name: "タスクに属さないステップでの停止",
			plan: statusPlan,
			state: &state.State{
				Step: state.StepFeedback,
				Stop: &state.Stop{Reason: "保護領域を宣言するタスクの追加", At: at},
			},
			want:    []string{"状態: 停止中 (フィードバックの起票)", "止まった理由: 保護領域を宣言するタスクの追加"},
			notWant: []string{"タスク  "},
		},
		{
			name:  "実行中のステップが無い状態",
			plan:  statusPlan,
			state: &state.State{ReportVersion: 1},
			want:  []string{"状態: 実行中のタスクなし"},
		},
		{
			name:  "確認済みとして終えた計画書",
			plan:  statusPlan,
			state: &state.State{ReportVersion: 2, Verified: 2},
			want:  []string{"作業報告書: 第2版 (確認済み)"},
		},
		{
			name: "すべて完了した計画書",
			plan: "## タスクリスト\n\n- [x] **1-1**: a\n",
			want: []string{"次のタスク: なし (すべて完了)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			planPath := writePlan(t, dir, "example.md", tt.plan)
			stateRoot := filepath.Join(dir, "state")
			if tt.state != nil {
				if err := state.NewDir(stateRoot, "example").Save(tt.state, at); err != nil {
					t.Fatal(err)
				}
			}

			var out, errBuf bytes.Buffer
			code := status([]string{planPath}, stateRoot, &out, &errBuf)
			if code != exitOK {
				t.Fatalf("終了コード = %d、期待値 = %d (標準エラー: %s)", code, exitOK, errBuf.String())
			}
			lines := strings.Split(out.String(), "\n")
			for _, w := range tt.want {
				found := false
				for _, l := range lines {
					if l == w {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("標準出力に行 %q が無い:\n%s", w, out.String())
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(out.String(), nw) {
					t.Errorf("標準出力に %q が含まれている:\n%s", nw, out.String())
				}
			}
		})
	}
}

func TestStatus_Errors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	broken := writePlan(t, dir, "broken.md", "## タスクリスト\n\n- [ ] **1-1**: a\n- [ ] **1-1**: b\n")
	valid := writePlan(t, dir, "valid.md", statusPlan)
	stateRoot := filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Join(stateRoot, "valid"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "valid", "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "計画書の書式の誤り", path: broken, wantErr: "タスク番号 1-1 が重複しています"},
		{name: "壊れた状態ファイル", path: valid, wantErr: "state.json を読み取れません"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out, errBuf bytes.Buffer
			code := status([]string{tt.path}, stateRoot, &out, &errBuf)
			if code != exitError {
				t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
			}
			if !strings.Contains(errBuf.String(), tt.wantErr) {
				t.Errorf("標準エラー = %q、含まれることを期待した文字列 = %q", errBuf.String(), tt.wantErr)
			}
		})
	}
}
