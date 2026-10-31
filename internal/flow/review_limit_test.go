package flow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/flow/state"
)

func TestFlowExtendReviewLimit(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	fx.reviewToLimit(t, autoFix)
	if _, err := fx.flow.DoneWithReviewLimit(context.Background(), state.RoleReviewerCodex, strings.NewReader(""), 6); err != nil {
		t.Fatal(err)
	}
	fx.reopen(t)
	if rec := fx.load(t).TaskRecord("1-1"); rec.ReviewLimit != 6 {
		t.Fatalf("保存されたレビュー上限 = %d、期待値 = 6", rec.ReviewLimit)
	}
	commit(t, fx.repo, "fix-3.txt")
	if _, err := fx.done(state.RoleReviewerCodex, ""); err != nil {
		t.Fatal(err)
	}
	for i, role := range []state.Role{state.RoleReviewerClaude, state.RoleReviewerCodex, state.RoleReviewerClaude} {
		n := i + 4
		fx.reopen(t)
		in := fx.wait(t, role)
		if in.Action != ActionReview || in.ReviewNumber != n {
			t.Fatalf("指示 = %+v、%d回目の再レビューを期待", in, n)
		}
		doc := fx.writeReview(t, "1-1", n, reviewDoc("Request Changes", autoFix))
		if _, err := fx.done(role, ""); err != nil {
			t.Fatal(err)
		}
		in = fx.wait(t, role)
		if n == 6 {
			if in.Action != ActionAsk || in.Ask.Kind != state.AskReviewLimit || !strings.Contains(in.Ask.Reason, "上限の6回") {
				t.Fatalf("6回目の指示 = %+v、人への確認を期待", in)
			}
			if strings.Contains(fx.readDoc(t, doc), "[x]") {
				t.Fatal("6回目で人に尋ねる前に回答しています")
			}
			return
		}
		if in.Action != ActionFix {
			t.Fatalf("指示 = %+v、修正を期待", in)
		}
		commit(t, fx.repo, fmt.Sprintf("fix-%d.txt", n))
		if _, err := fx.done(role, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlowExtendedReviewCanFinishEarly(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"Approve", "Comment"} {
		t.Run(verdict+"なら次のタスクへ進む", func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク", "- [ ] **1-2**: [{repo}] 次のタスク")
			fx.reviewToLimit(t, autoFix)
			if _, err := fx.flow.DoneWithReviewLimit(context.Background(), state.RoleReviewerCodex, strings.NewReader(""), 6); err != nil {
				t.Fatal(err)
			}
			commit(t, fx.repo, "fix-3.txt")
			if _, err := fx.done(state.RoleReviewerCodex, ""); err != nil {
				t.Fatal(err)
			}
			fx.wait(t, state.RoleReviewerClaude)
			fx.writeReview(t, "1-1", 4, reviewDoc(verdict, ""))
			if _, err := fx.done(state.RoleReviewerClaude, ""); err != nil {
				t.Fatal(err)
			}
			in := fx.wait(t, state.RoleImplementer)
			if in.Task != "1-2" || in.Action != ActionImplement || in.Record.ReviewLimit != 0 {
				t.Fatalf("指示 = %+v、既定の上限の次のタスクを期待", in)
			}
		})
	}
}

func TestFlowRejectsInvalidReviewExtension(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{-1, 0, 2, 3} {
		t.Run(fmt.Sprintf("上限%dを拒否する", limit), func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
			fx.reviewToLimit(t, autoFix)
			if _, err := fx.flow.DoneWithReviewLimit(context.Background(), state.RoleReviewerCodex, strings.NewReader(""), limit); err == nil {
				t.Fatalf("上限%dの延長を受け付けました", limit)
			}
			if st := fx.load(t); st.Step != state.StepAsk || st.TaskRecord("1-1").ReviewLimit != 0 {
				t.Fatalf("拒否したのに状態が変わりました: %+v", st)
			}
		})
	}
}

func TestFlowReviewExtensionRequiresLimitAsk(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	fx.wait(t, state.RoleImplementer)
	_, err := fx.flow.DoneWithReviewLimit(context.Background(), state.RoleImplementer, strings.NewReader("[]"), 6)
	assertRejected(t, err, "上限に達して人を待っているタスクでだけ")
	if st := fx.load(t); st.Step != state.StepImplement || st.TaskRecord("1-1").ReviewLimit != 0 {
		t.Fatalf("拒否したのに状態が変わりました: %+v", st)
	}
}
