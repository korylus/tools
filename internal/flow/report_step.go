package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/report"
	"github.com/korylus/tools/internal/flow/state"
)

// reportExampleは、作業報告書のステップのdoneに渡すJSONの形の例。
const reportExample = `{"location":"作業報告書のURL"} または {"location":"作業報告書のURL","walkthrough_skipped":"処理の流れを書かなかった理由"}`

// ReportBranchは、作業報告書の処理の流れで追うコードを持つ、リポジトリごとの最後のタスクのブランチ。
type ReportBranch struct {
	Repo   string
	Branch string
	// Commitはブランチの先頭のコミットのハッシュ。ブランチがローカルに残っていなければ空文字。
	Commit string
}

// startReportは、状態stを、実装者が作業報告書を書くステップにする。
func (f *Flow) startReport(st *state.State) {
	st.Task, st.Step, st.Role, st.Since, st.Ask, st.ReviewBefore = "", state.StepReport, state.RoleImplementer, f.Now(), nil, ""
}

// recordReportは、作業報告書のステップで受け取った記録rを、次の回として状態stに加える。
// 回に含める所要時間の記録の件数には、まだ追記していないこのステップの記録を数える。
// 完了の記録 (Pending) を残してから追記するため、途中で落ちて追記をやり直しても件数は変わらない。
func (f *Flow) recordReport(st *state.State, r state.ReportRound, now time.Time) error {
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return err
	}
	r.Number, r.Events, r.At = len(st.Reports)+1, len(events)+1, now
	st.Reports = append(st.Reports, r)
	return nil
}

// reportBranchesは、「マージするPR」と同じまとめ方で、リポジトリごとの最後のタスクの
// ブランチと、その先頭のコミットを返す。
func (f *Flow) reportBranches(ctx context.Context, st *state.State) ([]ReportBranch, error) {
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return nil, err
	}
	chains := report.Chains(st, events)
	branches := make([]ReportBranch, 0, len(chains))
	for _, c := range chains {
		commit, err := branch.Head(ctx, c.Head.Repo, c.Head.Branch)
		if err != nil {
			return nil, err
		}
		branches = append(branches, ReportBranch{Repo: c.Head.Repo, Branch: c.Head.Branch, Commit: commit})
	}
	return branches, nil
}

// parseReportは、作業報告書のURLと、処理の流れを書かなかった理由をJSONで読む。
// URLは必須で、処理の流れを書いたときは理由を渡さない。
func parseReport(r io.Reader) (state.ReportRound, error) {
	hint := "作業報告書のURLを、標準入力からJSONで渡してください: " + reportExample
	if r == nil {
		return state.ReportRound{}, errors.New(hint)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return state.ReportRound{}, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return state.ReportRound{}, errors.New(hint)
	}
	var raw struct {
		Location           *string `json:"location"`
		WalkthroughSkipped *string `json:"walkthrough_skipped"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return state.ReportRound{}, fmt.Errorf("作業報告書の記録を読み取れません: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return state.ReportRound{}, errors.New("作業報告書のJSONは1つだけ渡してください")
		}
		return state.ReportRound{}, fmt.Errorf("作業報告書の記録を読み取れません: %w", err)
	}

	if raw.Location == nil {
		return state.ReportRound{}, errors.New(hint)
	}
	var rec state.ReportRound
	rec.Location = strings.TrimSpace(*raw.Location)
	switch {
	case rec.Location == "":
		return state.ReportRound{}, errors.New("location に、作業報告書のURLを書いてください")
	case !strings.HasPrefix(rec.Location, "https://"):
		return state.ReportRound{}, fmt.Errorf("location (%s) は、https:// で始まる作業報告書のURLにしてください", rec.Location)
	}
	if raw.WalkthroughSkipped != nil {
		rec.WalkthroughSkipped = strings.TrimSpace(*raw.WalkthroughSkipped)
		if rec.WalkthroughSkipped == "" {
			return state.ReportRound{}, errors.New("walkthrough_skipped に、処理の流れを書かなかった理由を書いてください。書いたときは walkthrough_skipped を渡さないでください")
		}
	}
	return rec, nil
}
