package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/flow"
	"github.com/korylus/tools/internal/pilot/state"
)

func TestPilotRun_Args(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{name: "計画書のパスが無い", args: nil, wantCode: exitError, wantErr: "使い方"},
		{name: "計画書のパスが2つある", args: []string{"a.md", "b.md"}, wantCode: exitError, wantErr: "使い方"},
		{name: "絶対パス", args: []string{"/plans/a.md"}, wantCode: exitError, wantErr: "相対パス"},
		{name: "ルートの外", args: []string{"../a.md"}, wantCode: exitError, wantErr: "相対パス"},
		{name: "不明なフラグ", args: []string{"a.md", "-unknown"}, wantCode: exitError, wantErr: "-unknown"},
		{name: "ヘルプ", args: []string{"-h"}, wantCode: exitOK},
		{
			name:     "フラグを計画書のパスの後ろに書ける",
			args:     []string{"a.md", "-config=" + filepath.Join(dir, "missing.toml"), "-approve"},
			wantCode: exitError,
			wantErr:  "設定ファイルを読めません",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := pilotRun(context.Background(), tt.args, func(p *flow.Pilot) {
				p.StateRoot = filepath.Join(dir, "state")
			}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("終了コード = %d、期待値 = %d\n標準エラー出力:\n%s", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("標準エラー出力 = %q、含まれることを期待した文字列 = %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestProjectPath(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"docs/private/plans/a.md":    "docs/private/plans/a.md",
		"./docs/private/plans/a.md":  "docs/private/plans/a.md",
		"docs//private/../private/a": "docs/private/a",
	} {
		got, err := projectPath(in)
		if err != nil || got != want {
			t.Errorf("projectPath(%q) = %q, %v、期待値 = %q", in, got, err, want)
		}
	}
}

func TestWriteStop(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Roles: map[string]config.Role{
		config.RoleDesigner:    {Runner: []string{"docker", "compose", "run", "--rm", "-T", "pilot-designer"}},
		config.RoleImplementer: {Runner: []string{"docker", "compose", "run", "--rm", "-T", "pilot-implementer"}},
	}}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	t.Run("コードを直す役割", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		writeStop(&out, "docs/private/plans/p.md", &flow.Outcome{Config: cfg, State: &state.State{
			Task: "1-1",
			Step: state.StepGate,
			Stop: &state.Stop{Reason: "ゲートのコマンドが差し戻しのあとも失敗しました", Details: []string{"コマンド: make test\n出力の末尾:\nFAIL"}, Role: config.RoleImplementer, At: at},
		}})
		got := out.String()
		for _, want := range []string{
			"停止しました (タスク 1-1 のゲート検査)",
			"理由: ゲートのコマンドが差し戻しのあとも失敗しました",
			"  - コマンド: make test\n    出力の末尾:\n    FAIL\n",
			"解消に使う役割: implementer\n  コンテナ: docker compose run --rm -T pilot-implementer\n",
			"koryluspilot run docs/private/plans/p.md -note=<補足>",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("出力に %q がありません:\n%s", want, got)
			}
		}
		if strings.Contains(got, "-approve") {
			t.Errorf("コードを直す役割なのに-approveを案内している:\n%s", got)
		}
	})

	t.Run("計画書を直す設計者", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		writeStop(&out, "docs/private/plans/p.md", &flow.Outcome{Config: cfg, State: &state.State{
			Task: "1-1",
			Step: state.StepImplement,
			Stop: &state.Stop{Reason: "計画書が変わっています", Role: config.RoleDesigner, At: at},
		}})
		if got := out.String(); !strings.Contains(got, "pilot-designer") || !strings.Contains(got, "-approve を付けて") {
			t.Errorf("設計者の案内がありません:\n%s", got)
		}
	})

	t.Run("フィードバック対応の承認", func(t *testing.T) {
		t.Parallel()

		var out bytes.Buffer
		writeStop(&out, "docs/private/plans/p.md", &flow.Outcome{Config: cfg, State: &state.State{
			Step: state.StepApprove,
			Stop: &state.Stop{Reason: "フェーズfb1 に保護領域を宣言するタスクがあります", Details: []string{"タスク fb1-1 列を足す: schema"}, Role: config.RoleDesigner, At: at},
		}})
		got := out.String()
		for _, want := range []string{
			"停止しました (フィードバック対応の承認)",
			"  - タスク fb1-1 列を足す: schema",
			"  koryluspilot run docs/private/plans/p.md -approve\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("出力に %q がありません:\n%s", want, got)
			}
		}
		if strings.Contains(got, "-note=") {
			t.Errorf("承認を求める停止で-noteでの再開を案内している:\n%s", got)
		}
	})
}

func TestWriteDone(t *testing.T) {
	t.Parallel()

	tasks := []state.TaskRecord{{ID: "1-1", Branch: "p-1-1", Start: "p-base"}}
	tests := []struct {
		name    string
		out     *flow.Outcome
		want    []string
		notWant []string
	}{
		{
			name:    "確認待ち",
			out:     &flow.Outcome{Report: "docs/private/plans/files/p/report-1.html", State: &state.State{Tasks: tasks, ReportVersion: 1}},
			want:    []string{"  p-1-1 (元: p-base)", "作業報告書 (第1版) で動作確認してください: docs/private/plans/files/p/report-1.html", "もう一度 run を実行してください"},
			notWant: []string{"確認済み"},
		},
		{
			name:    "確認済み",
			out:     &flow.Outcome{State: &state.State{Tasks: tasks, ReportVersion: 2, Verified: 2}},
			want:    []string{"作業報告書 (第2版) ですべての確認項目がOKになり、計画書を確認済みとして終えました"},
			notWant: []string{"動作確認してください"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			writeDone(&b, tt.out)
			got := b.String()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("出力に %q がありません:\n%s", w, got)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("出力に %q が含まれています:\n%s", nw, got)
				}
			}
		})
	}
}
