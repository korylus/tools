package flow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/source"
	"github.com/korylus/tools/internal/flow/source/sourcetest"
	"github.com/korylus/tools/internal/flow/state"
)

// gitRunはdirでgitを実行し、標準出力を返す。失敗したらテストを止める。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.name=korylusflow", "-c", "user.email=korylusflow@example.com"}, args...)
	cmd := exec.Command("git", args...) //#nosec G204 -- テストが組み立てたgitの引数
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFileはrootの下のnameにcontentを書く。
func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// commitはrepoにファイルnameを足してコミットする。
func commit(t *testing.T, repo, name string) {
	t.Helper()
	writeFile(t, repo, name, name+"\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "--quiet", "-m", name)
}

// fixtureはテストの環境。mainにコミットを1つ持つリポジトリと、計画書・状態ディレクトリを持つ。
type fixture struct {
	repo string
	// planFileは計画書のファイルのパス。テスト用の読み取り元がこのファイルを読む。
	planFile string
	flow     *Flow
}

// newFixtureは、tasksを「## タスクリスト」節に並べた計画書demo.mdで環境を作る。
// tasksの中の {repo} はリポジトリのルートに置き換える。
func newFixture(t *testing.T, tasks ...string) fixture {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "--quiet", "--initial-branch=main")
	commit(t, repo, "README.md")

	planPath := filepath.Join(t.TempDir(), "demo.md")
	src := "# 計画書\n\n**計画書名**: demo\n\n## タスクリスト\n\n" + strings.ReplaceAll(strings.Join(tasks, "\n"), "{repo}", repo) + "\n"
	if err := os.WriteFile(planPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := fixture{repo: repo, planFile: planPath, flow: &Flow{Dir: state.NewDir(t.TempDir(), "demo")}}
	fx.reopen(t)
	return fx
}

// reopenは、同じ計画書と状態ディレクトリで新しいFlowを作り直す。リポジトリを
// プロジェクトのルート (本体) として扱い、レビュードキュメントもその下に置く。
func (fx *fixture) reopen(t *testing.T) {
	t.Helper()
	f, err := New(context.Background(), sourcetest.File{Path: fx.planFile}, filepath.Dir(fx.flow.Dir.Path()))
	if err != nil {
		t.Fatal(err)
	}
	f.Root = fx.repo
	f.PollInterval = 10 * time.Millisecond
	fx.flow = f
}

// reviewDocは、総合評価verdictと「ファイルごとのレビュー結果」節の本文findingsで
// レビュードキュメントを組み立てる。
func reviewDoc(verdict, findings string) string {
	return "# コードレビュー\n\n## 過去レビューの修正検証\n\n対象なし\n\n## ファイルごとのレビュー結果\n\n" +
		findings + "\n## 総合評価\n\n**評価**: " + verdict + "\n"
}

// writeReviewは、タスクidのn番のレビュードキュメントを本体のレビュー接頭辞で書き、
// プロジェクトのルートからの相対パスを返す。
func (fx fixture) writeReview(t *testing.T, id string, n int, content string) string {
	t.Helper()
	name := fmt.Sprintf("docs/private/reviews/demo-%s-%03d.md", id, n)
	writeFile(t, fx.flow.Root, name, content)
	return name
}

// approveは、タスクidの1回目のレビューをApproveで終え、Doneの文面を返す。
func (fx fixture) approve(t *testing.T, id string) string {
	t.Helper()
	if in := fx.wait(t, state.RoleReviewerCodex); in.Action != ActionReview || in.Task != id {
		t.Fatalf("指示 = %+v、タスク %s のレビューを期待", in, id)
	}
	fx.writeReview(t, id, 1, reviewDoc("Approve", ""))
	msg, err := fx.done(state.RoleReviewerCodex, "")
	if err != nil {
		t.Fatalf("レビューのDoneのエラー = %v", err)
	}
	return msg
}

// testReportURLは、テストで実装者が書いたことにする作業報告書のURL。
const testReportURL = "https://wikino.app/s/demo/pages/2"

// writeReportは、計画書の最後で実装者に作業報告書の指示が来ることを確かめ、
// 作業報告書のURLを渡して完了を記録する。Doneの文面を返す。
func (fx fixture) writeReport(t *testing.T) string {
	t.Helper()
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionReport {
		t.Fatalf("指示 = %+v、作業報告書の指示を期待", in)
	}
	msg, err := fx.done(state.RoleImplementer, `{"location":"`+testReportURL+`"}`)
	if err != nil {
		t.Fatalf("作業報告書のDoneのエラー = %v", err)
	}
	return msg
}

// factsは、Reportが組み立てる事実を返す。
func (fx fixture) facts(t *testing.T) string {
	t.Helper()
	out, err := fx.flow.Report(context.Background())
	if err != nil {
		t.Fatalf("Report()のエラー = %v", err)
	}
	return string(out)
}

// checkTaskは計画書のタスクidにチェックを付ける。
func (fx fixture) checkTask(t *testing.T, id string) {
	t.Helper()
	b, err := os.ReadFile(fx.planFile)
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Replace(string(b), fmt.Sprintf("- [ ] **%s**", id), fmt.Sprintf("- [x] **%s**", id), 1)
	if err := os.WriteFile(fx.planFile, []byte(src), 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		t.Fatal(err)
	}
}

// waitは役割roleの指示を受け取る。番が来なければテストを止める。
func (fx fixture) wait(t *testing.T, role state.Role) Instruction {
	t.Helper()
	in, err := fx.flow.Wait(context.Background(), role, time.Second)
	if err != nil {
		t.Fatalf("Waitのエラー = %v", err)
	}
	if in.Action == ActionWait {
		t.Fatalf("%s の番が来なかった", role)
	}
	return in
}

// doneは完了を記録する。
func (fx fixture) done(role state.Role, input string) (string, error) {
	return fx.flow.Done(context.Background(), role, strings.NewReader(input))
}

// assertRejectedは、errが理由wantsをすべて含む*RejectedErrorであることを確かめる。
func assertRejected(t *testing.T, err error, wants ...string) {
	t.Helper()
	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Doneのエラー = %v、期待値 = *RejectedError", err)
	}
	for _, want := range wants {
		if !strings.Contains(rejected.Error(), want) {
			t.Errorf("理由に %q が無い:\n%s", want, rejected.Error())
		}
	}
}

func TestFlowTasks(t *testing.T) {
	t.Parallel()

	fx := newFixture(t,
		"- [ ] **1-1**: [{repo}] 最初のタスク",
		"- [ ] **1-2**: [shimbaco] タグを付ける",
		"- [ ] **1-3**: [{repo}/sub] 2つ目のタスク",
	)
	if err := os.MkdirAll(filepath.Join(fx.repo, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 最初のタスクは、ベースブランチからブランチを作って実装の指示を返す。
	in := fx.wait(t, state.RoleImplementer)
	if in.Action != ActionImplement || in.Task != "1-1" || in.Title != "最初のタスク" {
		t.Fatalf("指示 = %+v、期待値 = タスク 1-1 の実装", in)
	}
	if in.Record.Branch != "demo-1-1" || in.Record.Base != "main" || in.Record.Repo != fx.repo {
		t.Errorf("記録 = %+v", in.Record)
	}
	if got := gitRun(t, fx.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "demo-1-1" {
		t.Errorf("ブランチ = %q、期待値 = %q", got, "demo-1-1")
	}
	text := in.String()
	for _, want := range []string{
		"action: implement\n",
		fmt.Sprintf("/korylus-do-plan-task %s 1-1 repo=%s base=main", fx.flow.PlanURL, fx.repo),
		fmt.Sprintf("/korylus-commit repo=%s", fx.repo),
		fmt.Sprintf("korylusflow done %s -role=implementer", fx.flow.PlanURL),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("指示に %q が無い:\n%s", want, text)
		}
	}

	// 実装者以外の番ではない。
	if in, err := fx.flow.Wait(ctx, state.RoleReviewerCodex, 30*time.Millisecond); err != nil || in.Action != ActionWait {
		t.Errorf("レビュアーのWait() = %+v, %v、期待値 = action: wait", in, err)
	}
	_, err := fx.done(state.RoleReviewerCodex, "[]")
	assertRejected(t, err, "reviewer-codex の番ではありません")

	// 実装の完了の条件を満たすまで受け付けない。
	_, err = fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "ベース main より先のコミットがありません", "タスク 1-1 にチェックが付いていません")

	commit(t, fx.repo, "feature.txt")
	writeFile(t, fx.repo, "scratch.txt", "x\n")
	fx.checkTask(t, "1-1")
	_, err = fx.done(state.RoleImplementer, "")
	assertRejected(t, err, "未コミットの変更があります", "動作確認の項目を、標準入力からJSONの配列で渡してください")

	if err := os.Remove(filepath.Join(fx.repo, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	_, err = fx.done(state.RoleImplementer, "null")
	assertRejected(t, err, "JSONの配列で渡してください")
	stBeforeDone, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stBeforeDone.Task != "1-1" || stBeforeDone.Step != state.StepImplement || stBeforeDone.Pending != nil {
		t.Fatalf("nullを拒否した後の状態 = %+v、タスク1-1の実装のままを期待", stBeforeDone)
	}
	// 計画書・状態ファイルの置き場所の変更は数えない。
	writeFile(t, fx.repo, "docs/private/plans/demo.md", "計画書\n")
	items := `[{"kind":"process","title":"statusを表示できる","target":"korylusflow status demo.md","steps":["実行する"],"expected":"一覧が出る"}]`
	msg, err := fx.done(state.RoleImplementer, items)
	if err != nil {
		t.Fatalf("Doneのエラー = %v", err)
	}
	if !strings.Contains(msg, "タスク 1-1 のレビューで、reviewer-codex の番") {
		t.Errorf("Done() = %q", msg)
	}
	if msg := fx.approve(t, "1-1"); !strings.Contains(msg, "タスク 1-2 の人への確認") {
		t.Errorf("Done() = %q", msg)
	}

	// 人が行うタスクは、実装者が人に尋ねる。
	in = fx.wait(t, state.RoleImplementer)
	if in.Action != ActionAsk || in.Task != "1-2" || in.Ask.Kind != state.AskHumanTask || in.Record != nil {
		t.Fatalf("指示 = %+v、期待値 = タスク 1-2 の人への確認", in)
	}
	_, err = fx.done(state.RoleImplementer, "")
	assertRejected(t, err, "タスク 1-2 にチェックが付いていません")
	fx.checkTask(t, "1-2")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatalf("Doneのエラー = %v", err)
	}

	// 同じリポジトリの2つ目のタスクは、前のタスクのブランチから作る。
	in = fx.wait(t, state.RoleImplementer)
	if in.Action != ActionImplement || in.Task != "1-3" {
		t.Fatalf("指示 = %+v、期待値 = タスク 1-3 の実装", in)
	}
	if want := (state.TaskRecord{ID: "1-3", Repo: fx.repo, Dir: filepath.Join(fx.repo, "sub"), Branch: "demo-1-3", Base: "demo-1-1"}); in.Record.ID != want.ID ||
		in.Record.Repo != want.Repo || in.Record.Dir != want.Dir || in.Record.Branch != want.Branch || in.Record.Base != want.Base {
		t.Errorf("記録 = %+v、期待値 = %+v", in.Record, want)
	}

	// 最後のタスクを終えると、待っていたほかの役割にも終了の指示を返す。
	finished := make(chan Instruction, 1)
	go func() {
		in, err := fx.flow.Wait(ctx, state.RoleReviewerClaude, 0)
		if err != nil {
			t.Errorf("レビュアーのWaitのエラー = %v", err)
		}
		finished <- in
	}()
	commit(t, fx.repo, "sub/feature.txt")
	fx.checkTask(t, "1-3")
	if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
		t.Fatalf("Doneのエラー = %v", err)
	}
	if msg := fx.approve(t, "1-3"); !strings.Contains(msg, "最後まで進みました") {
		t.Fatalf("Done() = %q", msg)
	}
	if msg := fx.writeReport(t); !strings.Contains(msg, "作業報告書: "+testReportURL) {
		t.Fatalf("作業報告書のDone() = %q", msg)
	}
	select {
	case in := <-finished:
		if in.Action != ActionFinish {
			t.Errorf("レビュアーの指示 = %+v、期待値 = action: finish", in)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("レビュアーのWaitが終わらない")
	}
	_, err = fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "完了を記録するステップがありません")

	// 記録したもの。
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rec := st.TaskRecord("1-1"); rec == nil || len(rec.Verification) != 1 || rec.Verification[0].Title != "statusを表示できる" {
		t.Errorf("タスク 1-1 の記録 = %+v", rec)
	}
	events, err := fx.flow.Dir.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, e := range events {
		steps = append(steps, e.Task+":"+string(e.Step))
	}
	if got, want := strings.Join(steps, ","), "1-1:implement,1-1:review,1-2:ask,1-3:implement,1-3:review,:report"; got != want {
		t.Errorf("所要時間の記録 = %s、期待値 = %s", got, want)
	}
}

func TestFlowBranchAsk(t *testing.T) {
	t.Parallel()

	t.Run("未コミットの変更があれば人に尋ね、片付くまで受け付けない", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
		writeFile(t, fx.repo, "wip.txt", "wip\n")

		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskBranch || !strings.Contains(in.String(), "?? wip.txt") {
			t.Fatalf("指示 = %+v\n%s", in, in.String())
		}
		_, err := fx.done(state.RoleImplementer, "")
		assertRejected(t, err, "まだブランチを準備できません", "?? wip.txt")

		if err := os.Remove(filepath.Join(fx.repo, "wip.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatalf("Doneのエラー = %v", err)
		}
		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionImplement {
			t.Errorf("指示 = %+v、期待値 = 実装", in)
		}
	})

	t.Run("重なっているパスは人に尋ねる", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}/docs/private] タスク")
		writeFile(t, fx.repo, ".gitignore", "docs/private/\n")
		gitRun(t, fx.repo, "add", ".")
		gitRun(t, fx.repo, "commit", "--quiet", "-m", "ignore")
		if err := os.MkdirAll(filepath.Join(fx.repo, "docs/private"), 0o750); err != nil {
			t.Fatal(err)
		}

		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskBranch || !strings.Contains(in.Ask.Details[0], "無視されています") {
			t.Fatalf("指示 = %+v", in)
		}
	})

	t.Run("人のタスクに変えた後は未作成のブランチを次のベースにしない", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t,
			"- [ ] **1-1**: [{repo}] 最初のタスク",
			"- [ ] **1-2**: [{repo}] 次のタスク",
		)
		writeFile(t, fx.repo, "wip.txt", "wip\n")
		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk || in.Ask.Kind != state.AskBranch {
			t.Fatalf("指示 = %+v、ブランチの準備のaskを期待", in)
		}
		src, err := os.ReadFile(fx.planFile)
		if err != nil {
			t.Fatal(err)
		}
		src = []byte(strings.Replace(string(src), "["+fx.repo+"]", "[shimbaco]", 1))
		if err := os.WriteFile(fx.planFile, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
			t.Fatal(err)
		}
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatal(err)
		}
		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk || in.Ask.Kind != state.AskHumanTask || in.Record != nil {
			t.Fatalf("指示 = %+v、人のタスクのaskと記録無しを期待", in)
		}
		if err := os.Remove(filepath.Join(fx.repo, "wip.txt")); err != nil {
			t.Fatal(err)
		}
		fx.checkTask(t, "1-1")
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatal(err)
		}
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionImplement || in.Task != "1-2" || in.Record.Base != "main" {
			t.Fatalf("指示 = %+v、mainから作ったタスク1-2の実装を期待", in)
		}
	})

	t.Run("askの間にタスクを飛ばした後は未作成のブランチを次のベースにしない", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t,
			"- [ ] **1-1**: [{repo}] 最初のタスク",
			"- [ ] **1-2**: [{repo}] 次のタスク",
		)
		writeFile(t, fx.repo, "wip.txt", "wip\n")
		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk || in.Ask.Kind != state.AskBranch {
			t.Fatalf("指示 = %+v、ブランチの準備のaskを期待", in)
		}
		if err := os.Remove(filepath.Join(fx.repo, "wip.txt")); err != nil {
			t.Fatal(err)
		}
		fx.checkTask(t, "1-1")
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatal(err)
		}
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionImplement || in.Task != "1-2" || in.Record.Base != "main" {
			t.Fatalf("指示 = %+v、mainから作ったタスク1-2の実装を期待", in)
		}
	})
}

func TestFlowDoneOnOtherBranch(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	fx.wait(t, state.RoleImplementer)
	commit(t, fx.repo, "feature.txt")
	fx.checkTask(t, "1-1")
	gitRun(t, fx.repo, "checkout", "--quiet", "main")

	_, err := fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "タスクのブランチ demo-1-1 ではなく main にいます")
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Task != "1-1" || st.Step != state.StepImplement || st.Pending != nil {
		t.Errorf("拒否した後の状態 = %+v、タスク1-1の実装のままを期待", st)
	}
}

func TestFlowDoneBeforeStart(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク")
	_, err := fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "先に korylusflow wait を実行してください")
}

func TestParseVerification(t *testing.T) {
	t.Parallel()

	valid := `{"kind":"screen","title":"t","target":"/a","steps":["s"],"expected":"e"}`
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr string
	}{
		{name: "空の配列は項目無し", input: "[]", want: 0},
		{name: "nullは配列ではない", input: "null", wantErr: "JSONの配列で渡してください"},
		{name: "項目を読む", input: "[" + valid + "]", want: 1},
		{name: "空の入力は書き忘れ", input: " \n", wantErr: "JSONの配列で渡してください"},
		{name: "JSONでなければエラー", input: "{", wantErr: "読み取れません"},
		{name: "kindの誤り", input: `[{"kind":"page","title":"t","target":"/a","steps":["s"],"expected":"e"}]`, wantErr: "screen か process"},
		{name: "項目の欠け", input: `[{"kind":"screen","title":"t"}]`, wantErr: "すべて書いてください"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items, err := parseVerification(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("エラー = %v、期待値 = %q を含むエラー", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(items) != tt.want {
				t.Errorf("parseVerification() = %d件, %v、期待値 = %d件", len(items), err, tt.want)
			}
		})
	}
}

func TestNewName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, head, wantName, wantErr string
	}{
		{name: "計画書名の行の値を状態の置き場に使う", head: "**計画書名**: other-plan\n\n", wantName: "other-plan"},
		{name: "計画書を読み取れなければエラー", head: "**計画書名**: Demo\n\n", wantErr: "を読み取れません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			planPath := filepath.Join(t.TempDir(), "demo.md")
			writeFile(t, filepath.Dir(planPath), "demo.md", tt.head+"## タスクリスト\n\n- [ ] **1-1**: [shimbaco] 人のタスク\n")
			stateRoot := t.TempDir()
			f, err := New(context.Background(), sourcetest.File{Path: planPath}, stateRoot)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Newのエラー = %v、期待値 = %q を含むエラー", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Newのエラー = %v", err)
			}
			if f.Name != tt.wantName || f.Dir.Path() != filepath.Join(stateRoot, tt.wantName) {
				t.Errorf("計画書名 = %q、状態の置き場 = %q、期待値 = %q", f.Name, f.Dir.Path(), tt.wantName)
			}
		})
	}
}

func TestFlowWikinoPlan(t *testing.T) {
	t.Parallel()

	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "--quiet", "--initial-branch=main")
	commit(t, repo, "README.md")
	body := "**計画書名**: wiki-demo\n\n## タスクリスト\n\n- [ ] **1-1**: [" + repo + "] タスク\n"

	// 下書きの本文を返す。読んだ回数を数え、番でない役割が読まないことを確かめる。
	var reads int
	const url = "https://wikino.app/s/shimbaco/pages/12"
	src := &source.Wikino{URL: url, Space: "shimbaco", Number: 12, Run: func(_ context.Context, args ...string) ([]byte, []byte, int, error) {
		reads++
		return []byte("---\nwikino_number: 12\n---\n\n" + body), nil, 0, nil
	}}
	stateRoot := t.TempDir()
	f, err := New(context.Background(), src, stateRoot)
	if err != nil {
		t.Fatalf("Newのエラー = %v", err)
	}
	f.Root, f.PollInterval = repo, 10*time.Millisecond
	if f.Name != "wiki-demo" || f.Dir.Path() != filepath.Join(stateRoot, "wiki-demo") {
		t.Errorf("計画書名 = %q、状態の置き場 = %q", f.Name, f.Dir.Path())
	}

	in, err := f.Wait(context.Background(), state.RoleImplementer, time.Second)
	if err != nil || in.Action != ActionImplement || in.Title != "タスク" {
		t.Fatalf("Wait = %+v, %v、期待値 = タスク1-1の実装", in, err)
	}
	if in.Record.Branch != "wiki-demo-1-1" {
		t.Errorf("ブランチ = %q、期待値 = wiki-demo-1-1", in.Record.Branch)
	}
	text := in.String()
	for _, want := range []string{
		"plan: " + url + "\n",
		"/korylus-do-plan-task " + url + " 1-1 repo=",
		"korylusflow done " + url + " -role=implementer",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("指示に %q が無い:\n%s", want, text)
		}
	}

	before := reads
	if in, err := f.Wait(context.Background(), state.RoleReviewerCodex, 50*time.Millisecond); err != nil || in.Action != ActionWait {
		t.Errorf("レビュアーのWait = %+v, %v、期待値 = action: wait", in, err)
	}
	if reads != before {
		t.Errorf("番でない役割のWaitで計画書を %d 回読んだ", reads-before)
	}
}

func TestNewWikinoWithoutName(t *testing.T) {
	t.Parallel()

	src := &source.Wikino{URL: "https://wikino.app/s/shimbaco/pages/12", Space: "shimbaco", Number: 12, Run: func(context.Context, ...string) ([]byte, []byte, int, error) {
		return []byte("## タスクリスト\n\n- [ ] **1-1**: [shimbaco] 人のタスク\n"), nil, 0, nil
	}}
	if _, err := New(context.Background(), src, t.TempDir()); err == nil || !strings.Contains(err.Error(), "計画書名の行がありません") {
		t.Errorf("Newのエラー = %v、期待値 = 計画書名の行が無いエラー", err)
	}
}

// mergeAndDeleteは、人がブランチnameをmainにマージして消したときの状態をrepoに作る。
func mergeAndDelete(t *testing.T, repo, name string) {
	t.Helper()
	gitRun(t, repo, "checkout", "--quiet", "main")
	gitRun(t, repo, "merge", "--quiet", "--no-ff", "-m", "merge "+name, name)
	gitRun(t, repo, "branch", "--quiet", "-D", name)
}

func TestFlowMissingPreviousBranch(t *testing.T) {
	t.Parallel()

	t.Run("前のタスクのブランチが消えていれば最初のベースから作り、作業報告書でその旨を示す", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t,
			"- [ ] **1-1**: [{repo}] 最初のタスク",
			"- [ ] **1-2**: [shimbaco] マージする",
			"- [ ] **1-3**: [{repo}] 次のタスク",
		)
		fx.wait(t, state.RoleImplementer)
		commit(t, fx.repo, "first.txt")
		fx.checkTask(t, "1-1")
		if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
			t.Fatalf("実装のDoneのエラー = %v", err)
		}
		fx.approve(t, "1-1")

		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk || in.Ask.Kind != state.AskHumanTask {
			t.Fatalf("指示 = %+v、人が行うタスクのaskを期待", in)
		}
		mergeAndDelete(t, fx.repo, "demo-1-1")
		fx.checkTask(t, "1-2")
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatalf("人への確認のDoneのエラー = %v", err)
		}

		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionImplement || in.Task != "1-3" || in.Record.Base != "main" {
			t.Fatalf("指示 = %+v、mainから作ったタスク1-3の実装を期待", in)
		}
		commit(t, fx.repo, "second.txt")
		fx.checkTask(t, "1-3")
		if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
			t.Fatalf("実装のDoneのエラー = %v", err)
		}
		fx.approve(t, "1-3")
		fx.writeReport(t)

		out := fx.facts(t)
		for _, want := range []string{
			"| ブランチ `demo-1-1` がローカルに残っていないため示せません |",
			"| `demo-1-3` (元: `main`) | テスト以外",
			"| `" + fx.repo + "` | `demo-1-1` (ローカルに残っていません) | `main` | 1-1 |\n| `" + fx.repo + "` | `demo-1-3` | `main` | 1-3 |\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("事実に %q が無い:\n%s", want, out)
			}
		}
		if strings.Contains(out, "読めません") {
			t.Errorf("消えたブランチをgitのエラーのまま示している:\n%s", out)
		}
	})

	t.Run("準備を待つ間に前のタスクのブランチが消えれば元のブランチを求め直す", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t,
			"- [ ] **1-1**: [{repo}] 最初のタスク",
			"- [ ] **1-2**: [{repo}] 次のタスク",
		)
		fx.wait(t, state.RoleImplementer)
		commit(t, fx.repo, "first.txt")
		fx.checkTask(t, "1-1")
		if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
			t.Fatalf("実装のDoneのエラー = %v", err)
		}
		writeFile(t, fx.repo, "wip.txt", "wip\n")
		fx.approve(t, "1-1")

		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskBranch || in.Record.Base != "demo-1-1" {
			t.Fatalf("指示 = %+v、demo-1-1を元にしたブランチの準備のaskを期待", in)
		}
		if err := os.Remove(filepath.Join(fx.repo, "wip.txt")); err != nil {
			t.Fatal(err)
		}
		mergeAndDelete(t, fx.repo, "demo-1-1")
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatalf("人への確認のDoneのエラー = %v", err)
		}
		in = fx.wait(t, state.RoleImplementer)
		if in.Action != ActionImplement || in.Task != "1-2" || in.Record.Base != "main" {
			t.Fatalf("指示 = %+v、mainから作ったタスク1-2の実装を期待", in)
		}
	})
}

func TestFlowAssignee(t *testing.T) {
	t.Parallel()

	t.Run("担当がimplementerでパスの接頭辞ならブランチを作る", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] タスク\n  - **担当**: implementer")
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionImplement || in.Record == nil || in.Record.Branch != "demo-1-1" {
			t.Fatalf("指示 = %+v、ブランチ demo-1-1 の実装を期待", in)
		}
	})

	t.Run("担当がimplementerで接頭辞無しならブランチを作らずに行う", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: 下書きを作る\n  - **担当**: implementer")
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionWork || in.Task != "1-1" || in.Record != nil {
			t.Fatalf("指示 = %+v、記録を持たないタスク1-1の作業を期待", in)
		}
	})

	t.Run("担当が人の名前ならパスの接頭辞でも人が行う", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [{repo}] 下書きを公開する\n  - **担当**: shimbaco")
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskHumanTask || in.Record != nil {
			t.Fatalf("指示 = %+v、人のタスクのaskと記録無しを期待", in)
		}
		if got := gitRun(t, fx.repo, "branch", "--list", "demo-1-1"); got != "" {
			t.Errorf("ブランチ %q を作った", got)
		}
	})

	t.Run("接頭辞がパスでなければ人に尋ね、直すまで受け付けない", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t, "- [ ] **1-1**: [implementer] 下書きを作る\n  - **担当**: implementer")
		in := fx.wait(t, state.RoleImplementer)
		if in.Action != ActionAsk || in.Ask.Kind != state.AskBranch || in.Ask.Reason != "タスクの接頭辞と担当の行が食い違っています" {
			t.Fatalf("指示 = %+v、接頭辞と担当の食い違いのaskを期待", in)
		}
		if !strings.Contains(in.String(), "接頭辞か担当の行を直したら続けられる") {
			t.Errorf("指示に直し方が無い:\n%s", in.String())
		}
		_, err := fx.done(state.RoleImplementer, "")
		assertRejected(t, err, "まだブランチを準備できません", "接頭辞 [implementer] がパスではありません")

		src, err := os.ReadFile(fx.planFile)
		if err != nil {
			t.Fatal(err)
		}
		src = []byte(strings.Replace(string(src), "[implementer] ", "", 1))
		if err := os.WriteFile(fx.planFile, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
			t.Fatal(err)
		}
		if _, err := fx.done(state.RoleImplementer, ""); err != nil {
			t.Fatalf("人への確認のDoneのエラー = %v", err)
		}
		if in := fx.wait(t, state.RoleImplementer); in.Action != ActionWork || in.Task != "1-1" {
			t.Fatalf("指示 = %+v、タスク1-1の作業を期待", in)
		}
	})
}

func TestFlowImplementerTask(t *testing.T) {
	t.Parallel()

	fx := newFixture(t,
		"- [ ] **1-1**: [implementer] 下書きを作る",
		"- [ ] **1-2**: [{repo}] 次のタスク",
	)

	// ブランチを準備せずに、実装者へ作業を指示する。
	in := fx.wait(t, state.RoleImplementer)
	if in.Action != ActionWork || in.Task != "1-1" || in.Record != nil {
		t.Fatalf("指示 = %+v、記録を持たないタスク1-1の作業を期待", in)
	}
	text := in.String()
	for _, want := range []string{
		"action: work\n",
		"ブランチの作成・切り替えとコミットはしない",
		"行う前に対象の一覧を人に示し、その場で了承を得る",
		"チェックは自分で付けない",
		"人が差分を確かめてコミットするよう伝える",
		fmt.Sprintf("korylusflow done %s -role=implementer", fx.flow.PlanURL),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("指示に %q が無い:\n%s", want, text)
		}
	}
	if strings.Contains(text, "branch:") || strings.Contains(text, "/korylus-do-plan-task") {
		t.Errorf("ブランチを作らないタスクの指示に、ブランチや実装のスキルがある:\n%s", text)
	}
	if got := gitRun(t, fx.repo, "branch", "--list", "demo-1-1"); got != "" {
		t.Errorf("ブランチ %q を作った", got)
	}

	// 人がチェックを付けるまで受け付けない。作業ツリーの変更は確かめない。
	writeFile(t, fx.repo, "draft.txt", "draft\n")
	_, err := fx.done(state.RoleImplementer, "")
	assertRejected(t, err, "計画書のタスク 1-1 にチェックが付いていません")
	if err := os.Remove(filepath.Join(fx.repo, "draft.txt")); err != nil {
		t.Fatal(err)
	}
	fx.checkTask(t, "1-1")
	msg, err := fx.done(state.RoleImplementer, "")
	if err != nil {
		t.Fatalf("Doneのエラー = %v", err)
	}

	// レビューを挟まず、次のタスクを最初のベースから作る。
	if !strings.Contains(msg, "タスク 1-2 の実装で、implementer の番") {
		t.Errorf("Done() = %q", msg)
	}
	in = fx.wait(t, state.RoleImplementer)
	if in.Action != ActionImplement || in.Task != "1-2" || in.Record.Base != "main" {
		t.Fatalf("指示 = %+v、mainから作ったタスク1-2の実装を期待", in)
	}
	commit(t, fx.repo, "next.txt")
	fx.checkTask(t, "1-2")
	if _, err := fx.done(state.RoleImplementer, "[]"); err != nil {
		t.Fatalf("実装のDoneのエラー = %v", err)
	}
	fx.approve(t, "1-2")
	fx.writeReport(t)

	out := fx.facts(t)
	for _, want := range []string{
		"## 実装者が行ったタスク\n\nブランチを作らずに実装者が行い、人が結果を確かめたタスクです。\n\n- 1-1 [implementer] 下書きを作る\n",
		"| `" + fx.repo + "` | `demo-1-2` | `main` | 1-2 |\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("事実に %q が無い:\n%s", want, out)
		}
	}
	if strings.Contains(out, "### 1-1 ") {
		t.Errorf("ブランチを作らないタスクに変更の欄がある:\n%s", out)
	}
}
