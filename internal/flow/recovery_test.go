package flow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestFlowResumeAfterSaveFailure(t *testing.T) {
	t.Parallel()

	for _, viaWait := range []bool{false, true} {
		t.Run(fmt.Sprintf("waitによる再開=%t", viaWait), func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t,
				"- [ ] **1-1**: [{repo}] 最初のタスク",
				"- [ ] **1-2**: [{repo}] 次のタスク",
			)
			fx.wait(t, state.RoleImplementer)
			commit(t, fx.repo, "feature.txt")
			fx.checkTask(t, "1-1")
			items := `[{"kind":"process","title":"確認する","target":"status","steps":["実行する"],"expected":"表示される"}]`
			if _, err := fx.done(state.RoleImplementer, items); err != nil {
				t.Fatalf("実装のDoneのエラー = %v", err)
			}
			fx.wait(t, state.RoleReviewerCodex)
			fx.writeReview(t, "1-1", 1, reviewDoc("Approve", ""))

			// 次のブランチに切り替わった直後に保存先を塞ぎ、最後のrenameを失敗させる。
			statePath := filepath.Join(fx.flow.Dir.Path(), "flow.json")
			quoted := "'" + strings.ReplaceAll(statePath, "'", "'\\''") + "'"
			hook := filepath.Join(fx.repo, ".git/hooks/post-checkout")
			script := fmt.Sprintf("#!/bin/sh\nmv %s %s.saved\nmkdir %s\n", quoted, quoted, quoted)
			if err := os.WriteFile(hook, []byte(script), 0o700); err != nil { //#nosec G306 -- テストの一時リポジトリでgitにフックを実行させる
				t.Fatal(err)
			}
			if _, err := fx.done(state.RoleReviewerCodex, ""); err == nil {
				t.Fatal("保存失敗のエラーを期待したが、nilだった")
			}
			if cur := gitRun(t, fx.repo, "rev-parse", "--abbrev-ref", "HEAD"); cur != "demo-1-2" {
				t.Fatalf("保存失敗時のブランチ = %q、期待値 = demo-1-2", cur)
			}
			if err := os.Remove(hook); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(statePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(statePath+".saved", statePath); err != nil {
				t.Fatal(err)
			}
			before, err := fx.flow.Dir.Load()
			if err != nil {
				t.Fatal(err)
			}
			if before.Pending == nil || before.Pending.Task != "1-1" || before.Pending.Step != state.StepReview {
				t.Fatalf("保存失敗後の状態 = %+v、タスク1-1のレビューの完了の記録を期待", before)
			}

			// 新しいFlowでも、完了条件を確かめ直さずに復帰できる。
			fx.reopen()
			if viaWait {
				in := fx.wait(t, state.RoleImplementer)
				if in.Action != ActionImplement || in.Task != "1-2" {
					t.Fatalf("再開後の指示 = %+v、タスク1-2の実装を期待", in)
				}
			} else {
				_, err := fx.done(state.RoleImplementer, "")
				assertRejected(t, err, "implementer の番ではありません")
				if _, err := fx.done(state.RoleReviewerCodex, ""); err != nil {
					t.Fatalf("再開時のDoneのエラー = %v", err)
				}
			}
			after, err := fx.flow.Dir.Load()
			if err != nil {
				t.Fatal(err)
			}
			if after.Pending != nil || after.Task != "1-2" || after.Step != state.StepImplement {
				t.Fatalf("再開後の状態 = %+v、タスク1-2の実装を期待", after)
			}
			rec := after.TaskRecord("1-1")
			if rec == nil || len(rec.Verification) != 1 || rec.Verification[0].Title != "確認する" {
				t.Errorf("動作確認の項目が保存されていない: %+v", rec)
			}
			if rec == nil || len(rec.Reviews) != 1 || rec.Reviews[0].Verdict != "Approve" {
				t.Errorf("レビューの記録が保存されていない: %+v", rec)
			}
			events, err := fx.flow.Dir.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1] != *before.Pending {
				t.Errorf("履歴 = %+v、実装と、保存失敗前のレビューの完了の記録を期待", events)
			}
		})
	}
}

func TestFlowResumeAfterEventFailure(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] 最後のタスク")
	fx.wait(t, state.RoleImplementer)
	commit(t, fx.repo, "feature.txt")
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
		t.Fatalf("実装のDoneのエラー = %v", err)
	}
	fx.wait(t, state.RoleReviewerCodex)
	fx.writeReview(t, "1-1", 1, reviewDoc("Approve", ""))
	timeline := filepath.Join(fx.flow.Dir.Path(), "timeline.jsonl")
	if err := os.Rename(timeline, timeline+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(timeline, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.done(state.RoleReviewerCodex, ""); err == nil {
		t.Fatal("履歴の書き込み失敗のエラーを期待したが、nilだった")
	}
	if err := os.Remove(timeline); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(timeline+".saved", timeline); err != nil {
		t.Fatal(err)
	}
	fx.reopen()
	if in := fx.wait(t, state.RoleReviewerClaude); in.Action != ActionFinish {
		t.Fatalf("再開後の指示 = %+v、終了を期待", in)
	}
	events, err := fx.flow.Dir.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Task != "1-1" || events[1].Step != state.StepReview {
		t.Errorf("履歴 = %+v、タスク1-1の実装とレビューの記録を期待", events)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if in, err := fx.flow.Wait(ctx, state.RoleImplementer, 0); err != nil || in.Action != ActionFinish {
		t.Errorf("完了後のWait = %+v, %v、終了を期待", in, err)
	}
}

func TestFlowResumeBranchAsk(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	writeFile(t, fx.repo, "wip.txt", "wip\n")
	fx.wait(t, state.RoleImplementer)
	if err := os.Remove(filepath.Join(fx.repo, "wip.txt")); err != nil {
		t.Fatal(err)
	}
	timeline := filepath.Join(fx.flow.Dir.Path(), "timeline.jsonl")
	if err := os.Mkdir(timeline, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.done(state.RoleImplementer, ""); err == nil {
		t.Fatal("履歴の書き込み失敗のエラーを期待したが、nilだった")
	}
	if err := os.Remove(timeline); err != nil {
		t.Fatal(err)
	}
	fx.reopen()
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionImplement || in.Task != "1-1" {
		t.Fatalf("再開後の指示 = %+v、タスク1-1の実装を期待", in)
	}
	events, err := fx.flow.Dir.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Step != state.StepAsk {
		t.Errorf("履歴 = %+v、askの記録1件を期待", events)
	}
}
