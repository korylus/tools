package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/state"
)

const outsideWatch = `
[[gate.watch]]
name = "claude-home"
path = "@outside@"
`

func TestRun_OutsideOnFailure(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		action fakeAction
	}{
		{name: "使用量の上限", action: fakeAction{RateLimited: true, Outside: "settings.json"}},
		{name: "通常のエラー", action: fakeAction{ExitCode: 1, Outside: "settings.json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newTestEnv(t, []string{task1}, outsideWatch, tt.action)
			out, _ := e.run("", false)
			if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "許可外") || len(out.State.Stop.Details) != 1 || !strings.Contains(out.State.Stop.Details[0], "settings.json") {
				t.Fatalf("監視違反による停止を期待: %+v", out.State.Stop)
			}
			if n := len(e.callArgs()); n != 1 {
				t.Errorf("エージェントの起動回数 = %d、期待値 = 1", n)
			}
			if len(e.slept) != 0 {
				t.Errorf("監視違反のあとに待機した: %v", e.slept)
			}
			for _, n := range e.fetchedAtCalls {
				if n > 0 {
					t.Errorf("監視違反のあとに使用率を取得した: %v", e.fetchedAtCalls)
				}
			}
		})
	}
}

func TestRun_OutsideOnCancellation(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, outsideWatch, fakeAction{Outside: "settings.json", Hang: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(filepath.Join(e.outside, "settings.json")); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	var log bytes.Buffer
	out, err := e.pilot("", false, &log).Run(ctx)
	<-done
	if err != nil {
		t.Fatalf("Runのエラー = %v\n%s", err, log.String())
	}
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "許可外") {
		t.Fatalf("中断時にも監視違反による停止を期待: %+v", out.State.Stop)
	}
	if n := len(e.callArgs()); n != 1 {
		t.Errorf("エージェントの起動回数 = %d、期待値 = 1", n)
	}
}

func TestRun_OutsideBeforeResume(t *testing.T) {
	t.Parallel()

	for _, changed := range []bool{false, true} {
		name := "保存した基準から変更なし"
		if changed {
			name = "保存した基準から許可外の変更あり"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := newTestEnv(t, []string{task1}, outsideWatch,
				fakeAction{Status: agent.StatusEscalated},
				fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
				fakeAction{Commit: true},
				approve("p-1-1-001"),
			)
			if out, _ := e.run("", false); !out.Stopped() {
				t.Fatal("準備のエスカレーションで停止しなかった")
			}
			// エージェントの実行中にプロセスが落ちた状態を、保存した基準とセッションで再現する。
			watcher := gate.OutsideWatcher{Watches: []config.Watch{{Name: "claude-home", Path: e.outside}}}
			snapshot, err := watcher.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			st := e.state()
			st.Stop = nil
			st.OutsideBefore, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.NewDir(filepath.Join(e.root, state.Root), "p").Save(st, e.now); err != nil {
				t.Fatal(err)
			}
			fetches := len(e.fetchedAtCalls)
			if changed {
				e.write(filepath.Join(e.outside, "settings.json"), "変更")
			}
			out, log := e.run("", false)
			if changed {
				if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "許可外") || !strings.Contains(out.State.Stop.Details[0], "settings.json") {
					t.Fatalf("再開前の監視違反による停止を期待: %+v", out.State.Stop)
				}
				if len(e.callArgs()) != 1 || len(e.fetchedAtCalls) != fetches {
					t.Errorf("監視違反の解消前にCLIを起動した: エージェント = %d回、使用率取得 = %v", len(e.callArgs()), e.fetchedAtCalls)
				}
			} else if out.Stopped() || len(out.State.Tasks) != 1 || len(outputsOf(out.State, state.StepImplement)) != 2 {
				t.Errorf("変更のない状態からの再開に失敗: %+v\n%s", out.State, log)
			}
		})
	}
}

func TestRun_OutputOnOutsideStop(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, outsideWatch,
		fakeAction{Outside: "settings.json", Status: agent.StatusCompleted, ProtectedChanges: json.RawMessage(`[{"area":"schema","summary":"列を追加","reason":"設計どおり"}]`)},
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "許可外") {
		t.Fatalf("監視違反による停止を期待: %+v", out.State.Stop)
	}
	st := e.state()
	if len(st.Outputs) != 1 {
		t.Fatalf("保存した構造化出力 = %d件、期待値 = 1件", len(st.Outputs))
	}
	result := st.Outputs[0].Result
	if result.Summary != "偽のエージェントの要約" || len(result.VerificationItems) != 1 || len(result.ProtectedChanges) != 1 || result.ProtectedChanges[0].Summary != "列を追加" {
		t.Errorf("停止時の構造化出力の内容が保存されていない: %+v", result)
	}
	if st.Step != state.StepImplement || len(st.Tasks) != 0 {
		t.Errorf("出力を保存しただけでステップが進んだ: %+v", st)
	}
	if st.OutsideBefore != nil {
		t.Error("監視違反を確認して停止したあとに古い基準が残っている")
	}
	out, log := e.run("変更を確認しました", false)
	implements := outputsOf(out.State, state.StepImplement)
	if out.Stopped() || len(out.State.Tasks) != 1 || len(implements) != 2 {
		t.Fatalf("再開後の状態 = %+v\n%s", out.State, log)
	}
	if len(implements[0].Result.ProtectedChanges) != 1 || len(implements[1].Result.ProtectedChanges) != 0 {
		t.Errorf("停止前の出力を失ったか、重複して保存した: %+v", out.State.Outputs)
	}
	if first, second := implements[0], implements[1]; first.Outcome != state.OutcomeOK || !first.Stopped || second.Outcome != state.OutcomeOK || second.Stopped {
		t.Errorf("試行の結果が記録されていない: 1件目 = %s/%v、2件目 = %s/%v", first.Outcome, first.Stopped, second.Outcome, second.Stopped)
	}
}

func TestRun_RetryWithAllowedChanges(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name        string
		action      fakeAction
		wantOutputs int
	}{
		{name: "使用量上限のあとに再開", action: fakeAction{RateLimited: true, CheckTask: "1-1"}, wantOutputs: 1},
		{name: "通常エラーのあとに再開", action: fakeAction{ExitCode: 1, CheckTask: "1-1", Status: agent.StatusCompleted}, wantOutputs: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			watch := `
[[gate.watch]]
name = "private"
path = "@root@/docs/private"
project_path = "docs/private"
`
			e := newTestEnv(t, []string{task1}, watch,
				tt.action,
				fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusCompleted},
				fakeAction{Commit: true},
				approve("p-1-1-001"),
			)
			out, log := e.run("", false)
			if out.Stopped() || len(out.State.Tasks) != 1 {
				t.Fatalf("許可された計画書の変更で停止した: %+v\n%s", out.State, log)
			}
			calls := e.callArgs()
			if len(calls) != 4 || flagValue(calls[1], "--resume") != "session-1" {
				t.Errorf("再開したエージェントの引数 = %v", calls)
			}
			outputs := outputsOf(out.State, state.StepImplement)
			if len(outputs) != tt.wantOutputs {
				t.Fatalf("保存した実装の構造化出力 = %d件、期待値 = %d件", len(outputs), tt.wantOutputs)
			}
			// 失敗した試行の出力は、成功した試行の出力と区別できる。
			for i, o := range outputs {
				want := state.OutcomeOK
				if i < len(outputs)-1 {
					want = state.OutcomeFailed
				}
				if o.Outcome != want || o.Stopped {
					t.Errorf("%d件目の試行の結果 = %s/%v、期待値 = %s/false", i+1, o.Outcome, o.Stopped, want)
				}
			}
		})
	}
}

func TestRun_OutsideBaselineClearedOnFailureStop(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, outsideWatch,
		fakeAction{ExitCode: 1},
		fakeAction{ExitCode: 1},
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "続けて失敗") {
		t.Fatalf("連続失敗による停止を期待: %+v", out.State.Stop)
	}
	if out.State.OutsideBefore != nil {
		t.Error("監視の検査を通ったあとの停止で基準が残っている")
	}
	// 止まっている間に人間が役割のコンテナで監視するパスを変えても、再開では止めない。
	e.write(filepath.Join(e.outside, "settings.json"), "人間の変更")
	out, log := e.run("直しました", false)
	if out.Stopped() || len(out.State.Tasks) != 1 {
		t.Fatalf("人間の変更で再開が止まった: %+v\n%s", out.State.Stop, log)
	}
}
