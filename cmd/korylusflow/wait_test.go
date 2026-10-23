package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWait(t *testing.T) {
	t.Parallel()

	t.Run("役割が無いとエラー", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if code := wait(context.Background(), []string{writePlan(t)}, t.TempDir(), &stdout, &stderr); code != exitError {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
		}
		assertContains(t, stderr.String(), "不明な役割")
	})

	t.Run("すべて完了していれば終了の指示を返す", func(t *testing.T) {
		t.Parallel()
		planPath := filepath.Join(t.TempDir(), "done.md")
		if err := os.WriteFile(planPath, []byte("## タスクリスト\n\n- [x] **1-1**: [shimbaco] 済み\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := wait(context.Background(), []string{planPath, "-role=reviewer-claude", "-timeout=1s"}, t.TempDir(), &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "action: finish\n") {
			t.Errorf("標準出力 = %q、action: finish で始まることを期待", stdout.String())
		}
	})

	t.Run("番が来ないまま期限を過ぎたら呼び直す指示を返す", func(t *testing.T) {
		t.Parallel()
		planPath := writeHumanPlan(t)
		var stdout, stderr bytes.Buffer
		code := wait(context.Background(), []string{planPath, "-role=reviewer-codex", "-timeout=50ms"}, t.TempDir(), &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "action: wait\n") {
			t.Errorf("標準出力 = %q、action: wait で始まることを期待", stdout.String())
		}
		assertContains(t, stdout.String(), "korylusflow wait "+planPath+" -role=reviewer-codex")
	})

	t.Run("中断されたら終了コード1を返す", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		if code := wait(ctx, []string{writeHumanPlan(t), "-role=implementer"}, t.TempDir(), &stdout, &stderr); code != exitError {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
		}
		assertContains(t, stderr.String(), "中断しました")
		if stdout.Len() != 0 {
			t.Errorf("中断したのに指示を書いた: %q", stdout.String())
		}
	})
}

// writeHumanPlanは、人が行うタスクだけを持つ計画書をt.TempDirの下に書き、そのパスを返す。
// 実装者の番のまま進まず、gitの操作も行わない計画書として使う。
func writeHumanPlan(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "human.md")
	if err := os.WriteFile(path, []byte("## タスクリスト\n\n- [ ] **1-1**: [shimbaco] タグを付ける\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDone(t *testing.T) {
	t.Parallel()

	t.Run("まだ始めていなければ受け付けない", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := done(context.Background(), []string{writePlan(t), "-role=implementer"}, t.TempDir(), strings.NewReader("[]"), &stdout, &stderr)
		if code != exitRejected {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitRejected)
		}
		assertContains(t, stderr.String(), "完了の記録を受け付けませんでした", "先に korylusflow wait を実行してください")
	})

	t.Run("役割が無いとエラー", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := done(context.Background(), []string{writePlan(t)}, t.TempDir(), strings.NewReader(""), &stdout, &stderr)
		if code != exitError {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
		}
	})
}
