package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestReport(t *testing.T) {
	t.Parallel()

	t.Run("ループを始めていなければエラー", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if code := report(context.Background(), []string{writePlan(t)}, t.TempDir(), &stdout, &stderr); code != exitError {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
		}
		assertContains(t, stderr.String(), "まだループを始めていません")
	})

	t.Run("計画書の最後まで進んでいれば作業報告書を書く", func(t *testing.T) {
		t.Parallel()
		planPath := writePlan(t)
		stateRoot := t.TempDir()
		dir := state.NewDir(stateRoot, "korylusflow")
		now := time.Now()
		if err := dir.Save(&state.State{}, now); err != nil {
			t.Fatal(err)
		}
		if err := dir.AppendEvent(state.Event{Task: "2-2", Step: state.StepAsk, Role: state.RoleImplementer, Started: now.Add(-time.Hour), Ended: now, Reason: "人が行うタスクです"}); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := report(context.Background(), []string{planPath}, stateRoot, &stdout, &stderr); code != exitOK {
			t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		assertContains(t, stdout.String(), "作業報告書を書きました: "+dir.ReportPath(1))
		out, err := os.ReadFile(dir.ReportPath(1))
		if err != nil {
			t.Fatal(err)
		}
		assertContains(t, string(out), "# 作業報告書 第1版: korylusflow", "| 2-2 | 実装者 | 人が行うタスクです |")

		stdout.Reset()
		if code := status([]string{planPath}, stateRoot, now, &stdout, &stderr); code != exitOK {
			t.Fatalf("statusの終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		assertContains(t, stdout.String(), "作業報告書: 第1版 ("+dir.ReportPath(1)+")")
	})
}
