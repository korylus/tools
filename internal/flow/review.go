package flow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/review"
)

// defaultMaxReviewsは、延長していないタスクで行うレビューの上限の回数。
const defaultMaxReviews = 3

// reviewLimitは、レビューの記録trについて人が延長した上限か、既定の上限を返す。
func reviewLimit(tr *state.ReviewTrail) int {
	if tr.ReviewLimit > 0 {
		return tr.ReviewLimit
	}
	return defaultMaxReviews
}

// reportReviewPrefixは、計画書名nameの作業報告書のレビュードキュメントに使うレビュー接頭辞を返す。
// 回をまたいで同じ接頭辞にし、前の回のレビューの修正検証の対象にする。
func reportReviewPrefix(name string) string {
	return name + "-report"
}

// reviewerForはn回目 (1始まり) のレビューを行う役割を返す。修正したモデルが同じ変更を
// 再レビューしないよう、Codexから始めて交互に替える。
func reviewerFor(n int) state.Role {
	if n%2 == 1 {
		return state.RoleReviewerCodex
	}
	return state.RoleReviewerClaude
}

// startTaskReviewは状態stを、タスクの記録recの最初のレビューのステップにする。
// レビュー接頭辞は、最初のレビューを始めるときに決める。
func (f *Flow) startTaskReview(ctx context.Context, st *state.State, rec *state.TaskRecord, now time.Time) error {
	if rec.ReviewPrefix == "" {
		root, err := branch.Toplevel(ctx, f.Root)
		if err != nil {
			return err
		}
		// /korylus-review は本体と別のリポジトリのレビューが混ざらないよう、
		// 別のリポジトリではリポジトリ名を接頭辞に足す。
		rec.ReviewPrefix = rec.Branch
		if rec.Repo != root {
			rec.ReviewPrefix = filepath.Base(rec.Repo) + "-" + rec.Branch
			rec.ReviewRepo = rec.Repo
		}
	}
	return f.startReview(st, &rec.ReviewTrail, now)
}

// startReportReviewは状態stを、最新の回の作業報告書のレビューのステップにする。
func (f *Flow) startReportReview(st *state.State, now time.Time) error {
	tr := &st.Reports[len(st.Reports)-1].ReviewTrail
	tr.ReviewPrefix = reportReviewPrefix(f.Name)
	st.Task = ""
	return f.startReview(st, tr, now)
}

// startReviewは状態stを、レビューの記録trの次のレビューのステップにする。レビュー接頭辞は
// 決めてあること。始める時点の最新のレビュードキュメントを残し、それより新しい
// 連番だけをこのステップのレビューとして受け付ける。
func (f *Flow) startReview(st *state.State, tr *state.ReviewTrail, now time.Time) error {
	before, _, err := review.Latest(f.Root, tr.ReviewPrefix)
	if err != nil {
		return err
	}
	setStep(st, state.StepReview, reviewerFor(len(tr.Reviews)+1), now)
	st.ReviewBefore = before
	return nil
}

// finishReviewは、レビューのステップの完了を確かめて記録し、次のステップを決める。
// レビュードキュメントの不備はレビュアーが直せるため、受け付けずに理由を返す。
func (f *Flow) finishReview(st *state.State, rec *state.ReviewTrail, now time.Time) error {
	doc, ok, err := review.Latest(f.Root, rec.ReviewPrefix)
	if err != nil {
		return err
	}
	sequence, err := reviewSequence(rec.ReviewPrefix, doc)
	if err != nil {
		return err
	}
	before, err := reviewSequence(rec.ReviewPrefix, st.ReviewBefore)
	if err != nil {
		return err
	}
	if !ok || sequence <= before {
		return reject("%s/%s-NNN.md に、このレビューのレビュードキュメント (前回より新しい連番) がありません", review.Dir, rec.ReviewPrefix)
	}
	d, err := f.readReview(doc)
	if err != nil {
		return err
	}
	// チェック済みの選択肢は人の回答として扱うため、レビュアーが入れたチェックで
	// 要人間判断の指摘を素通りさせない。推奨どおりのチェックは回答と変わらないため許す。
	var checked []string
	for _, fd := range d.Checked() {
		if !checkedAsRecommended(fd) {
			checked = append(checked, fd.Describe())
		}
	}
	if len(checked) > 0 {
		return &RejectedError{Reasons: append([]string{fmt.Sprintf("%s で、レビュアーが対応方針の選択肢にチェックを入れています。チェックを外してください (回答はkorylusflowが行います)", doc)}, checked...)}
	}
	if a := d.Answer(); len(a.Invalid) > 0 {
		return &RejectedError{Reasons: append([]string{fmt.Sprintf("%s に、推奨どおりに回答できない指摘があります", doc)}, a.Invalid...)}
	}

	n := len(rec.Reviews) + 1
	role := st.Role
	rec.Reviews = append(rec.Reviews, state.ReviewRecord{Number: n, Role: role, Doc: doc, Verdict: string(d.Verdict), At: now})
	limit := reviewLimit(rec)
	if d.Verdict == review.RequestChanges && n >= limit {
		next := "続けると、回答と修正のあと、再レビューせずに次のタスクへ進みます"
		if st.IsReportReview() {
			next = "続けると、回答と修正のあと、再レビューせずに作業報告書のレビューを終えます"
		}
		setStep(st, state.StepAsk, role, now)
		st.Ask = &state.Ask{
			Kind:    state.AskReviewLimit,
			Reason:  fmt.Sprintf("レビューが上限の%d回に達しても Request Changes でした", limit),
			Details: []string{"レビュードキュメント: " + doc, next},
		}
		return nil
	}
	return f.answer(st, rec, d, now)
}

// reviewSequenceは、レビュー接頭辞prefixのドキュメントのパスdocの連番を返す。
// 開始時に文書が無ければパスは空になるため、その場合は0とする。
func reviewSequence(prefix, doc string) (int, error) {
	if doc == "" {
		return 0, nil
	}
	n, ok := review.Sequence(prefix, doc)
	if !ok {
		return 0, fmt.Errorf("レビュードキュメント %s の連番を読み取れません", doc)
	}
	return n, nil
}

// checkedAsRecommendedは、指摘fdのチェックが、自動対応可の指摘の推奨の選択肢1つだけに
// 入っているかを返す。Answerが入れるチェックと同じなので、回答をやり直しても結果は変わらない。
func checkedAsRecommended(fd review.Finding) bool {
	if fd.Proposal || fd.Category != review.AutoFix {
		return false
	}
	n := 0
	for _, o := range fd.Options {
		if !o.Checked {
			continue
		}
		if !o.Recommended {
			return false
		}
		n++
	}
	return n == 1
}

// answerは最新のレビューのドキュメントdの対応方針に回答し、次のステップを決める。
// 要人間判断の指摘か再検討提案に回答が無ければ、そのレビュアーが人に尋ねるステップにする。
// 回答はレビュードキュメントに書き、状態を保存する前に書くため、保存に失敗して
// やり直しても同じ回答になる。
func (f *Flow) answer(st *state.State, rec *state.ReviewTrail, d *review.Document, now time.Time) error {
	doc := rec.Reviews[len(rec.Reviews)-1].Doc
	a := d.Answer()
	if len(a.Invalid) > 0 {
		return &RejectedError{Reasons: append([]string{fmt.Sprintf("%s に、推奨どおりに回答できない指摘があります", doc)}, a.Invalid...)}
	}
	role := st.Role
	if len(a.Pending) > 0 {
		details := []string{"レビュードキュメント: " + doc}
		for _, fd := range a.Pending {
			details = append(details, fd.Describe())
		}
		setStep(st, state.StepAsk, role, now)
		st.Ask = &state.Ask{Kind: state.AskReview, Reason: "レビューに、人の判断が要る指摘 (要人間判断・再検討提案) があります", Details: details}
		return nil
	}

	if slices.ContainsFunc(a.Decisions, func(dec review.Decision) bool { return dec.Auto }) {
		if err := os.WriteFile(filepath.Join(f.Root, doc), a.Output, 0o600); err != nil {
			return fmt.Errorf("レビュードキュメントに回答を書けません: %w", err)
		}
	}
	for _, dec := range a.Decisions {
		if !dec.Defers() {
			continue
		}
		for _, o := range dec.Chosen {
			df := state.DeferredFinding{Doc: doc, Title: dec.Finding.Title, Choice: o.Text}
			if !slices.Contains(rec.Deferred, df) {
				rec.Deferred = append(rec.Deferred, df)
			}
		}
	}
	if a.NeedsFix() {
		setStep(st, state.StepFix, role, now)
		return nil
	}
	return f.afterFix(st, rec, now)
}

// afterFixは、修正を終えたか修正の要らない回答のあとに、再レビューするかレビューを終えるかを
// 決める。最後のレビューがRequest Changesで上限に達していなければ、別の役割で再レビューする。
// 作業報告書のレビューを終えるときは、その回にレビューのステップの記録を含める。
func (f *Flow) afterFix(st *state.State, rec *state.ReviewTrail, now time.Time) error {
	last := rec.Reviews[len(rec.Reviews)-1]
	if review.Verdict(last.Verdict) == review.RequestChanges && last.Number < reviewLimit(rec) {
		return f.startReview(st, rec, now)
	}
	if st.IsReportReview() {
		if err := f.coverReport(st); err != nil {
			return err
		}
	}
	clearStep(st)
	return nil
}

// checkFixはタスクの修正の完了の条件を確かめる。作業報告書の修正は下書きを書き換えるだけで
// コミットしないため、この条件を確かめない。
func checkFix(ctx context.Context, rec *state.TaskRecord) error {
	var reasons []string
	cur, err := branch.Current(ctx, rec.Repo)
	if err != nil {
		return err
	}
	if cur != rec.Branch {
		reasons = append(reasons, fmt.Sprintf("%s がタスクのブランチ %s ではなく %s にいます", rec.Repo, rec.Branch, cur))
	}
	files, err := branch.Dirty(ctx, rec.Repo, excludedPaths)
	if err != nil {
		return err
	}
	if len(files) > 0 {
		reasons = append(reasons, fmt.Sprintf("%s に未コミットの変更があります: %s", rec.Repo, strings.Join(files, ", ")))
	}
	if len(reasons) > 0 {
		return &RejectedError{Reasons: reasons}
	}
	return nil
}

// readReviewはレビュードキュメントdoc (プロジェクトのルートからの相対パス) を読み取る。
// 読み取れなければ、書いた人が直せるよう*RejectedErrorを返す。
func (f *Flow) readReview(doc string) (*review.Document, error) {
	src, err := os.ReadFile(filepath.Join(f.Root, doc)) //#nosec G304 -- docはreview.Latestがレビュードキュメントの置き場所から選んだパス
	if err != nil {
		return nil, fmt.Errorf("レビュードキュメントを読めません: %w", err)
	}
	d, err := review.Parse(src)
	if err != nil {
		return nil, &RejectedError{Reasons: []string{fmt.Sprintf("レビュードキュメント %s を読み取れません", doc), err.Error()}}
	}
	return d, nil
}
