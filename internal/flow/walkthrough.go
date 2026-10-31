package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/report"
	"github.com/korylus/tools/internal/flow/state"
)

// walkthroughExampleは、処理の流れのステップのdoneに渡すJSONの形の例。
const walkthroughExample = `{"location":"処理の流れのURLかプロジェクトのルートからの相対パス"} または {"skipped":"書かなかった理由"}`

// WalkthroughBranchは、処理の流れで追うコードを持つ、リポジトリごとの最後のタスクのブランチ。
type WalkthroughBranch struct {
	Repo   string
	Branch string
	// Commitはブランチの先頭のコミットのハッシュ。ブランチがローカルに残っていなければ空文字。
	Commit string
}

// startWalkthroughは、状態stを、実装者が処理の流れを書くステップにする。
func (f *Flow) startWalkthrough(st *state.State) {
	st.Task, st.Step, st.Role, st.Since, st.Ask, st.ReviewBefore = "", state.StepWalkthrough, state.RoleImplementer, f.Now(), nil, ""
}

// pendingWalkthroughは、次の版に記録する処理の流れを返す。処理の流れのステップを最後に
// 記録していて、そのあとに進めたステップが無いときだけ返し、それ以外はnilを返す。
// 処理の流れを書いたあとにタスクを足して進めたときは、書き直してもらうためである。
func pendingWalkthrough(st *state.State, events []state.Event) *state.Walkthrough {
	if st.Walkthrough == nil || len(events) == 0 || events[len(events)-1].Step != state.StepWalkthrough {
		return nil
	}
	return st.Walkthrough
}

// previousWalkthroughは、前の版までに書いた処理の流れの場所を返す。無ければ空文字。
// 2回目以降の版で、新しく作らずに同じ処理の流れを更新してもらうために指示に書く。
func previousWalkthrough(st *state.State) string {
	for i := len(st.Reports) - 1; i >= 0; i-- {
		if w := st.Reports[i].Walkthrough; w != nil && w.Location != "" {
			return w.Location
		}
	}
	return ""
}

// walkthroughBranchesは、作業報告書の「マージするPR」と同じまとめ方で、リポジトリごとの
// 最後のタスクのブランチと、その先頭のコミットを返す。
func (f *Flow) walkthroughBranches(ctx context.Context, st *state.State) ([]WalkthroughBranch, error) {
	events, err := f.Dir.ReadEvents()
	if err != nil {
		return nil, err
	}
	chains := report.Chains(st, events)
	branches := make([]WalkthroughBranch, 0, len(chains))
	for _, c := range chains {
		commit, err := branch.Head(ctx, c.Head.Repo, c.Head.Branch)
		if err != nil {
			return nil, err
		}
		branches = append(branches, WalkthroughBranch{Repo: c.Head.Repo, Branch: c.Head.Branch, Commit: commit})
	}
	return branches, nil
}

// parseWalkthroughは、処理の流れの場所か書かなかった理由をJSONで読む。
// locationのパスは、プロジェクトのルートrootからの相対パスで、ファイルがあることを確かめる。
func parseWalkthrough(r io.Reader, root string) (*state.Walkthrough, error) {
	hint := "処理の流れの場所か書かなかった理由を、標準入力からJSONで渡してください: " + walkthroughExample
	if r == nil {
		return nil, errors.New(hint)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(b)) == "" {
		return nil, errors.New(hint)
	}
	var raw struct {
		Location *string `json:"location"`
		Skipped  *string `json:"skipped"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("処理の流れの記録を読み取れません: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("処理の流れのJSONは1つだけ渡してください")
		}
		return nil, fmt.Errorf("処理の流れの記録を読み取れません: %w", err)
	}

	switch {
	case raw.Location != nil && raw.Skipped != nil:
		return nil, errors.New("location と skipped のどちらか一方だけを渡してください")
	case raw.Skipped != nil:
		skipped := strings.TrimSpace(*raw.Skipped)
		if skipped == "" {
			return nil, errors.New("skipped に、処理の流れを書かなかった理由を書いてください")
		}
		return &state.Walkthrough{Skipped: skipped}, nil
	case raw.Location == nil:
		return nil, errors.New(hint)
	}
	location := strings.TrimSpace(*raw.Location)
	switch {
	case location == "":
		return nil, errors.New("location に、処理の流れのURLかパスを書いてください")
	case strings.HasPrefix(location, "https://"):
		return &state.Walkthrough{Location: location}, nil
	case !filepath.IsLocal(location):
		return nil, fmt.Errorf("location (%s) は、https:// で始まるURLか、プロジェクトのルートからの相対パスにしてください", location)
	}
	info, err := os.Stat(filepath.Join(root, location))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("処理の流れのファイル %s がありません", location)
	case err != nil:
		return nil, err
	case info.IsDir():
		return nil, fmt.Errorf("location (%s) はディレクトリです。処理の流れのファイルを指定してください", location)
	}
	return &state.Walkthrough{Location: location}, nil
}
