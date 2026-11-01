package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/source/sourcetest"
	"github.com/korylus/tools/internal/flow/state"
)

func TestReport(t *testing.T) {
	t.Parallel()

	t.Run("ループを始めていなければエラー", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		if code := report(context.Background(), []string{writePlan(t)}, t.TempDir(), sourcetest.Parse, &stdout, &stderr); code != exitError {
			t.Errorf("終了コード = %d、期待値 = %d", code, exitError)
		}
		assertContains(t, stderr.String(), "まだループを始めていません")
	})

	t.Run("事実を標準出力に書き、状態は書き換えない", func(t *testing.T) {
		t.Parallel()
		planPath := writePlan(t)
		stateRoot := t.TempDir()
		dir := state.NewDir(stateRoot, "korylusflow")
		now := time.Now()
		url := "https://wikino.app/s/demo/pages/2"
		if err := dir.Save(&state.State{Reports: []state.ReportRound{{Number: 1, Events: 1, At: now, Location: url}}}, now); err != nil {
			t.Fatal(err)
		}
		if err := dir.AppendEvent(state.Event{Task: "2-2", Step: state.StepAsk, Role: state.RoleImplementer, Started: now.Add(-time.Hour), Ended: now, Reason: "人が行うタスクです"}); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := report(context.Background(), []string{planPath}, stateRoot, sourcetest.Parse, &stdout, &stderr); code != exitOK {
			t.Fatalf("終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		assertContains(t, stdout.String(), "# 計画書の事実: korylusflow\n", "- 計画書の開始: ", "- 作業報告書: 1回目 "+url+"\n", "### マージするPR\n")
		st, err := dir.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !st.UpdatedAt.Equal(now) {
			t.Errorf("状態の最終更新 = %v、reportで書き換えないことを期待 (%v)", st.UpdatedAt, now)
		}

		stdout.Reset()
		if code := status(context.Background(), []string{planPath}, stateRoot, sourcetest.Parse, now, &stdout, &stderr); code != exitOK {
			t.Fatalf("statusの終了コード = %d、標準エラー = %s", code, stderr.String())
		}
		assertContains(t, stdout.String(), "作業報告書: 1回目 ("+url+")")
	})
}
