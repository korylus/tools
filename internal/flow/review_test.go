package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/flow/state"
)

// 「ファイルごとのレビュー結果」節に置く指摘の例。
const (
	// autoFixは推奨が修正の、自動対応可の指摘。
	autoFix = "### 1. 名前が分かりにくい\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 名前を変える (推奨)\n- [ ] 現状維持\n\n"
	// deferredは推奨が後続タスクに積むの、自動対応可の指摘。
	deferred = "### 2. 関数を分ける\n\n**区分**: 自動対応可\n\n**対応方針**:\n\n- [ ] 提案通り変更する\n- [ ] 後続タスクに積む (推奨)\n\n"
	// needsHumanは要人間判断の指摘。
	needsHuman = "### 3. 依存の向き\n\n**区分**: 要人間判断\n\n**対応方針**:\n\n- [ ] 依存を逆にする (推奨)\n- [ ] 現状維持\n\n"
)

// implementは、タスクidの実装を終えてレビューの番にする。
func (fx fixture) implement(t *testing.T, id string) {
	t.Helper()
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionImplement || in.Task != id {
		t.Fatalf("指示 = %+v、タスク %s の実装を期待", in, id)
	}
	commit(t, fx.repo, id+".txt")
	fx.checkTask(t, id)
	if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
		t.Fatalf("実装のDoneのエラー = %v", err)
	}
}

// readDocはプロジェクトのルートからの相対パスnameのファイルを読む。
func (fx fixture) readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fx.flow.Root, name)) //#nosec G304 -- テストがt.TempDirの下に書いたファイル
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// loadは状態を読む。
func (fx fixture) load(t *testing.T) *state.State {
	t.Helper()
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFlowReviewRequestChanges(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	fx.implement(t, "1-1")

	// 1回目はCodexがレビューする。本体のリポジトリでは repo= を渡さない。
	in := fx.wait(t, state.RoleReviewerCodex)
	if in.Action != ActionReview || in.ReviewNumber != 1 {
		t.Fatalf("指示 = %+v、1回目のレビューを期待", in)
	}
	text := in.String()
	if want := fmt.Sprintf("`/korylus-review main %s`", fx.flow.PlanPath); !strings.Contains(text, want) {
		t.Errorf("指示に %q が無い:\n%s", want, text)
	}
	_, err := fx.done(state.RoleReviewerCodex, "")
	assertRejected(t, err, "docs/private/reviews/demo-1-1-NNN.md に、このレビューのレビュードキュメント")

	doc := fx.writeReview(t, "1-1", 1, reviewDoc("Request Changes", autoFix+deferred))
	msg, err := fx.done(state.RoleReviewerCodex, "")
	if err != nil {
		t.Fatalf("レビューのDoneのエラー = %v", err)
	}
	if !strings.Contains(msg, "タスク 1-1 の修正で、reviewer-codex の番") {
		t.Errorf("Done() = %q", msg)
	}
	src := fx.readDoc(t, doc)
	for _, want := range []string{"- [x] 名前を変える (推奨)", "- [ ] 現状維持", "- [ ] 提案通り変更する", "- [x] 後続タスクに積む (推奨)"} {
		if !strings.Contains(src, want) {
			t.Errorf("レビュードキュメントに %q が無い:\n%s", want, src)
		}
	}
	rec := fx.load(t).TaskRecord("1-1")
	if len(rec.Deferred) != 1 || rec.Deferred[0] != (state.DeferredFinding{Doc: doc, Title: "2. 関数を分ける", Choice: "後続タスクに積む (推奨)"}) {
		t.Errorf("先送りした指摘 = %+v", rec.Deferred)
	}

	// 修正は、レビューしたレビュアーが行う。
	in = fx.wait(t, state.RoleReviewerCodex)
	if in.Action != ActionFix || in.ReviewDoc != doc || !strings.Contains(in.String(), "レビュードキュメント "+doc) {
		t.Fatalf("指示 = %+v、%s に従う修正を期待", in, doc)
	}
	writeFile(t, fx.repo, "fix.txt", "fix\n")
	_, err = fx.done(state.RoleReviewerCodex, "")
	assertRejected(t, err, "未コミットの変更があります")
	gitRun(t, fx.repo, "add", ".")
	gitRun(t, fx.repo, "commit", "--quiet", "-m", "fix")
	gitRun(t, fx.repo, "checkout", "--quiet", "-b", "other")
	_, err = fx.done(state.RoleReviewerCodex, "")
	assertRejected(t, err, "タスクのブランチ demo-1-1 ではなく other にいます")
	gitRun(t, fx.repo, "checkout", "--quiet", "demo-1-1")
	if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "タスク 1-1 のレビューで、reviewer-claude の番") {
		t.Fatalf("Done() = %q, %v", msg, err)
	}

	// 再レビューは別の役割が行い、前回のドキュメントのままでは受け付けない。
	in = fx.wait(t, state.RoleReviewerClaude)
	if in.Action != ActionReview || in.ReviewNumber != 2 {
		t.Fatalf("指示 = %+v、2回目のレビューを期待", in)
	}
	_, err = fx.done(state.RoleReviewerClaude, "")
	assertRejected(t, err, "前回より新しい連番")
	fx.writeReview(t, "1-1", 2, reviewDoc("Approve", ""))
	if msg, err := fx.done(state.RoleReviewerClaude, ""); err != nil || !strings.Contains(msg, "最後まで進みました") {
		t.Fatalf("Done() = %q, %v", msg, err)
	}

	rec = fx.load(t).TaskRecord("1-1")
	if len(rec.Reviews) != 2 || rec.Reviews[0].Role != state.RoleReviewerCodex || rec.Reviews[0].Verdict != "Request Changes" ||
		rec.Reviews[1].Role != state.RoleReviewerClaude || rec.Reviews[1].Verdict != "Approve" {
		t.Errorf("レビューの記録 = %+v", rec.Reviews)
	}
}

func TestFlowReviewSequence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		before       int
		next         string
		wantRejected bool
	}{
		{name: "最新文書を移動しても古い連番を受け付けない", before: 2, wantRejected: true},
		{name: "最新文書を移動したあとに新しい連番を受け付ける", before: 2, next: "003"},
		{name: "桁数が増えても新しい連番を受け付ける", before: 999, next: "1000"},
		{name: "表記が違っても同じ連番を受け付けない", before: 2, next: "0002", wantRejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
			fx.writeReview(t, "1-1", 1, reviewDoc("Approve", ""))
			before := fx.writeReview(t, "1-1", tt.before, reviewDoc("Request Changes", autoFix))
			fx.implement(t, "1-1")
			fx.wait(t, state.RoleReviewerCodex)
			if got := fx.load(t).ReviewBefore; got != before {
				t.Fatalf("レビュー開始時の文書 = %q、期待値 = %q", got, before)
			}

			archive := filepath.Join(fx.flow.Root, "docs/private/reviews/done")
			if err := os.MkdirAll(archive, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(fx.flow.Root, before), filepath.Join(archive, filepath.Base(before))); err != nil {
				t.Fatal(err)
			}
			var next string
			if tt.next != "" {
				next = fmt.Sprintf("docs/private/reviews/demo-1-1-%s.md", tt.next)
				writeFile(t, fx.flow.Root, next, reviewDoc("Approve", ""))
			}

			msg, err := fx.done(state.RoleReviewerCodex, "")
			st := fx.load(t)
			rec := st.TaskRecord("1-1")
			if tt.wantRejected {
				assertRejected(t, err, "前回より新しい連番")
				if st.Task != "1-1" || st.Step != state.StepReview || st.ReviewBefore != before || len(rec.Reviews) != 0 {
					t.Fatalf("拒否した後の状態 = %+v、同じレビュー待ちのままを期待", st)
				}
				return
			}
			if err != nil || !strings.Contains(msg, "最後まで進みました") {
				t.Fatalf("Done() = %q, %v", msg, err)
			}
			if len(rec.Reviews) != 1 || rec.Reviews[0].Doc != next {
				t.Errorf("レビューの記録 = %+v、文書 %s の記録1件を期待", rec.Reviews, next)
			}
		})
	}
}

func TestFlowReviewComment(t *testing.T) {
	t.Parallel()

	t.Run("修正の要る指摘があれば修正してから次のタスクへ進む", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク", "- [ ] **1-2**: [{repo}] 次のタスク")
		fx.implement(t, "1-1")
		fx.wait(t, state.RoleReviewerCodex)
		fx.writeReview(t, "1-1", 1, reviewDoc("Comment", autoFix))
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "修正で、reviewer-codex の番") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
		commit(t, fx.repo, "fix.txt")
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "タスク 1-2 の実装") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})

	t.Run("先送りの指摘だけなら修正を飛ばす", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		fx.implement(t, "1-1")
		fx.wait(t, state.RoleReviewerCodex)
		fx.writeReview(t, "1-1", 1, reviewDoc("Comment", deferred))
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "最後まで進みました") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})

	t.Run("Request Changesでも修正が要らなければ再レビューする", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		fx.implement(t, "1-1")
		fx.wait(t, state.RoleReviewerCodex)
		fx.writeReview(t, "1-1", 1, reviewDoc("Request Changes", deferred))
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "レビューで、reviewer-claude の番") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})
}

func TestFlowReviewAskHuman(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	fx.implement(t, "1-1")
	fx.wait(t, state.RoleReviewerCodex)
	doc := fx.writeReview(t, "1-1", 1, reviewDoc("Comment", autoFix+needsHuman))
	if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "人への確認で、reviewer-codex の番") {
		t.Fatalf("Done() = %q, %v", msg, err)
	}
	// 人に尋ねている間は、推奨どおりの回答も書かない。
	if src := fx.readDoc(t, doc); strings.Contains(src, "[x]") {
		t.Errorf("人の判断の前に回答している:\n%s", src)
	}

	in := fx.wait(t, state.RoleReviewerCodex)
	if in.Action != ActionAsk || in.Ask.Kind != state.AskReview || !strings.Contains(in.String(), "3. 依存の向き") {
		t.Fatalf("指示 = %+v\n%s", in, in.String())
	}
	_, err := fx.done(state.RoleReviewerCodex, "")
	assertRejected(t, err, "まだ対応方針に回答の無い指摘があります", "3. 依存の向き")

	// 人が要人間判断の指摘に回答すれば、残りは推奨どおりに回答して修正に進む。
	src := strings.Replace(fx.readDoc(t, doc), "依存を逆にする (推奨)\n- [ ] 現状維持", "依存を逆にする (推奨)\n- [x] 現状維持", 1)
	writeFile(t, fx.flow.Root, doc, src)
	if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "修正で、reviewer-codex の番") {
		t.Fatalf("Done() = %q, %v", msg, err)
	}
	if src := fx.readDoc(t, doc); !strings.Contains(src, "- [x] 名前を変える (推奨)") {
		t.Errorf("推奨どおりの回答が無い:\n%s", src)
	}
	rec := fx.load(t).TaskRecord("1-1")
	if len(rec.Deferred) != 1 || rec.Deferred[0].Title != "3. 依存の向き" || rec.Deferred[0].Choice != "現状維持" {
		t.Errorf("先送りした指摘 = %+v", rec.Deferred)
	}
	events, err := fx.flow.Dir.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1]; last.Step != state.StepAsk || last.Role != state.RoleReviewerCodex {
		t.Errorf("最後の記録 = %+v、人を待った時間を期待", last)
	}
}

func TestFlowReviewRejectsDocument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		findings string
		want     string
	}{
		{
			name:     "要人間判断の指摘へのレビュアーのチェック",
			findings: strings.Replace(needsHuman, "- [ ] 依存を逆にする", "- [x] 依存を逆にする", 1),
			want:     "レビュアーが対応方針の選択肢にチェックを入れています",
		},
		{
			name:     "推奨ではない選択肢へのレビュアーのチェック",
			findings: strings.Replace(autoFix, "- [ ] 現状維持", "- [x] 現状維持", 1),
			want:     "レビュアーが対応方針の選択肢にチェックを入れています",
		},
		{
			name:     "推奨の無い指摘",
			findings: strings.Replace(autoFix, " (推奨)", "", 1),
			want:     "推奨どおりに回答できない指摘があります",
		},
		{
			name:     "評価の無いドキュメント",
			findings: autoFix + "## 総合評価\n\n**評価**: Approve\n\n",
			want:     "を読み取れません",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
			fx.implement(t, "1-1")
			fx.wait(t, state.RoleReviewerCodex)
			fx.writeReview(t, "1-1", 1, reviewDoc("Comment", tt.findings))
			_, err := fx.done(state.RoleReviewerCodex, "")
			assertRejected(t, err, tt.want)
			if st := fx.load(t); st.Step != state.StepReview || len(st.TaskRecord("1-1").Reviews) != 0 {
				t.Errorf("拒否した後の状態 = %+v、レビューのままを期待", st)
			}
		})
	}

	t.Run("推奨どおりのレビュアーのチェックは回答として受け付ける", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		fx.implement(t, "1-1")
		fx.wait(t, state.RoleReviewerCodex)
		fx.writeReview(t, "1-1", 1, reviewDoc("Comment", strings.Replace(autoFix, "- [ ] 名前を変える", "- [x] 名前を変える", 1)))
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "修正で") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})
}

// reviewToLimitは、タスク1-1の実装のあと、Request Changesのレビューと修正を上限の
// 回数まで繰り返す。最後のレビューの指摘はfindingsにし、上限で人に尋ねる指示を
// 確かめてから、最後のレビュードキュメントを返す。
func (fx fixture) reviewToLimit(t *testing.T, findings string) string {
	t.Helper()
	fx.implement(t, "1-1")
	roles := []state.Role{state.RoleReviewerCodex, state.RoleReviewerClaude, state.RoleReviewerCodex}
	var doc string
	for i, role := range roles {
		n := i + 1
		if in := fx.wait(t, role); in.Action != ActionReview || in.ReviewNumber != n {
			t.Fatalf("指示 = %+v、%s による%d回目のレビューを期待", in, role, n)
		}
		body := autoFix
		if n == len(roles) {
			body = findings
		}
		doc = fx.writeReview(t, "1-1", n, reviewDoc("Request Changes", body))
		if _, err := fx.done(role, ""); err != nil {
			t.Fatalf("%d回目のレビューのDoneのエラー = %v", n, err)
		}
		if n == len(roles) {
			break
		}
		fx.wait(t, role)
		commit(t, fx.repo, fmt.Sprintf("fix-%d.txt", n))
		if _, err := fx.done(role, ""); err != nil {
			t.Fatalf("%d回目の修正のDoneのエラー = %v", n, err)
		}
	}

	// 上限に達したら、回答する前に人に尋ねる。
	in := fx.wait(t, state.RoleReviewerCodex)
	if in.Action != ActionAsk || in.Ask.Kind != state.AskReviewLimit || in.ReviewDoc != doc {
		t.Fatalf("指示 = %+v、上限での人への確認を期待", in)
	}
	if src := fx.readDoc(t, doc); strings.Contains(src, "[x]") {
		t.Errorf("人に尋ねる前に回答している:\n%s", src)
	}
	return doc
}

func TestFlowReviewLimit(t *testing.T) {
	t.Parallel()

	t.Run("人が続けてよいと答えたら、修正のあと再レビューせずに次のタスクへ進む", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		fx.reviewToLimit(t, autoFix)
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "修正で、reviewer-codex の番") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
		commit(t, fx.repo, "fix-3.txt")
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "最後まで進みました") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})

	t.Run("要人間判断の指摘があれば、上限の確認に続けて人に尋ねる", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		doc := fx.reviewToLimit(t, autoFix+needsHuman)
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "人への確認で、reviewer-codex の番") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
		in := fx.wait(t, state.RoleReviewerCodex)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskReview || !strings.Contains(in.String(), "3. 依存の向き") {
			t.Fatalf("指示 = %+v\n%s", in, in.String())
		}

		src := strings.Replace(fx.readDoc(t, doc), "依存を逆にする (推奨)\n- [ ] 現状維持", "依存を逆にする (推奨)\n- [x] 現状維持", 1)
		writeFile(t, fx.flow.Root, doc, src)
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "修正で、reviewer-codex の番") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
		commit(t, fx.repo, "fix-3.txt")
		if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "最後まで進みました") {
			t.Fatalf("Done() = %q, %v", msg, err)
		}
	})
}

func TestFlowReviewOtherRepository(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	// プロジェクトのルートを別のリポジトリにし、タスクのリポジトリを本体と別にする。
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	fx.flow.Root = root
	fx.implement(t, "1-1")

	in := fx.wait(t, state.RoleReviewerCodex)
	if want := fmt.Sprintf("`/korylus-review main %s repo=%s`", fx.flow.PlanPath, fx.repo); !strings.Contains(in.String(), want) {
		t.Errorf("指示に %q が無い:\n%s", want, in.String())
	}
	prefix := filepath.Base(fx.repo) + "-demo-1-1"
	if in.Record.ReviewPrefix != prefix {
		t.Errorf("レビュー接頭辞 = %q、期待値 = %q", in.Record.ReviewPrefix, prefix)
	}
	writeFile(t, root, "docs/private/reviews/"+prefix+"-001.md", reviewDoc("Approve", ""))
	if msg, err := fx.done(state.RoleReviewerCodex, ""); err != nil || !strings.Contains(msg, "最後まで進みました") {
		t.Fatalf("Done() = %q, %v", msg, err)
	}
}
