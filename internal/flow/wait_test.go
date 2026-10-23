package flow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestFlowWaitDeadline(t *testing.T) {
	t.Parallel()

	for _, locked := range []bool{false, true} {
		for _, cancelParent := range []bool{false, true} {
			t.Run(fmt.Sprintf("ロック中=%t,親をキャンセル=%t", locked, cancelParent), func(t *testing.T) {
				t.Parallel()
				fx := newFixture(t, "- [ ] **1-1**: [shimbaco] 人のタスク")
				fx.wait(t, state.RoleImplementer)
				fx.flow.PollInterval = time.Hour
				if locked {
					unlock, err := fx.flow.Dir.Lock()
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = unlock() }()
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				timeout := 30 * time.Millisecond
				if cancelParent {
					timer := time.AfterFunc(30*time.Millisecond, cancel)
					defer timer.Stop()
					timeout = time.Hour
				}
				type result struct {
					in  Instruction
					err error
				}
				finished := make(chan result, 1)
				go func() {
					in, err := fx.flow.Wait(ctx, state.RoleReviewerCodex, timeout)
					finished <- result{in, err}
				}()
				select {
				case got := <-finished:
					if cancelParent {
						if !errors.Is(got.err, context.Canceled) {
							t.Errorf("Waitのエラー = %v、期待値 = context.Canceled", got.err)
						}
					} else if got.err != nil || got.in.Action != ActionWait {
						t.Errorf("Wait = %+v, %v、期待値 = action: wait", got.in, got.err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("期限かキャンセルを過ぎてもWaitが終わらない")
				}
			})
		}
	}
}

func TestFlowWaitDeadlineDuringPrepare(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	// ブランチの準備が、waitの期限より長く掛かるようにする。
	hook := filepath.Join(fx.repo, ".git/hooks/post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 0.3\n"), 0o700); err != nil { //#nosec G306 -- テストの一時リポジトリでgitにフックを実行させる
		t.Fatal(err)
	}

	in, err := fx.flow.Wait(context.Background(), state.RoleImplementer, 50*time.Millisecond)
	if err != nil || in.Action != ActionWait {
		t.Fatalf("Wait = %+v, %v、期待値 = action: wait", in, err)
	}
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatalf("期限を迎えても準備を終えて状態を保存することを期待したが、読めない: %v", err)
	}
	if st.Task != "1-1" || st.Step != state.StepImplement {
		t.Errorf("状態 = %+v、タスク1-1の実装を期待", st)
	}
	if got := gitRun(t, fx.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "demo-1-1" {
		t.Errorf("ブランチ = %q、期待値 = %q", got, "demo-1-1")
	}
}
