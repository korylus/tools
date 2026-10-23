package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/report"
	"github.com/korylus/tools/internal/pilot/runner"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
	"github.com/korylus/tools/internal/plan"
)

const planPath = "docs/private/plans/p.md"

// testConfigはテストの設定ファイル。%sにはハブのパスと、追加の設定が入る。
const testConfig = `project = "mewst"
base_branch = "develop"
max_reviews = 1
reviewers = ["reviewer_codex"]

[hub]
upstream = "https://example.com/mewst.git"
path = %q

[roles.designer]
tool = "claude"
runner = ["pilot-designer"]

[roles.implementer]
tool = "claude"
model = "claude-opus-5-5"
effort = "medium"
fix_effort = "low"
runner = ["pilot-implementer"]

[roles.reviewer_codex]
tool = "codex"
runner = ["pilot-reviewer-codex"]

[roles.reviewer_claude]
tool = "claude"
effort = "xhigh"
fix_effort = "medium"
runner = ["pilot-reviewer-claude"]

[gate]
runner = ["env"]

[gate.commands]
Go = ["test -f a.go"]

[[protected]]
name = "schema"
label = "DBスキーマ"
paths = ["db/**"]
review_points = ["ロックの長さ", "ロールバックできるか"]
%s`

// testEnvはflowのテストの環境。人間のチェックアウト・GitHubの代わりのリポジトリ・
// ハブを置くディレクトリを持つ。
type testEnv struct {
	t        *testing.T
	root     string
	hubRoot  string
	upstream string
	calls    string
	outside  string
	scenario []fakeAction
	now      time.Time
	slept    []time.Duration
	// fetchedAtCallsは使用率を取得した時点のエージェントの起動回数。
	fetchedAtCalls []int
	// highUntilは、この時刻まで使用率を上限より高く返す。highAfterCallsが正なら、
	// 偽のエージェントがその回数だけ呼ばれたあとに限る。
	highUntil      time.Time
	highAfterCalls int
}

// newTestEnvはtasksのタスクを持つ計画書と、extraConfigを足した設定ファイルを用意する。
func newTestEnv(t *testing.T, tasks []string, extraConfig string, scenario ...fakeAction) *testEnv {
	t.Helper()
	dir := t.TempDir()
	e := &testEnv{
		t:        t,
		root:     filepath.Join(dir, "checkout"),
		hubRoot:  filepath.Join(dir, "pilot"),
		upstream: filepath.Join(dir, "upstream"),
		calls:    filepath.Join(dir, "calls"),
		outside:  filepath.Join(dir, "outside"),
		scenario: scenario,
		now:      time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local),
	}
	for _, d := range []string{e.upstream, e.calls, e.outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	e.git(e.upstream, "init", "--quiet", "--initial-branch=develop")
	e.write(filepath.Join(e.upstream, "README.md"), "mewst\n")
	e.git(e.upstream, "add", "README.md")
	e.git(e.upstream, "commit", "--quiet", "-m", "最初のコミット")
	// 人間のチェックアウトはGitHubのクローンにあたる。計画書・設定ファイル・状態は
	// 未追跡のファイルとして置く。
	e.git(dir, "clone", "--quiet", e.upstream, e.root)

	// ハブは役割のコンテナの中から見たパスの代わりに、シンボリックリンクのパスで渡す。
	hubPath := filepath.Join(dir, "mounted-hub.git")
	if err := os.Symlink(filepath.Join(e.hubRoot, "hub.git"), hubPath); err != nil {
		t.Fatal(err)
	}
	extra := strings.ReplaceAll(extraConfig, "@root@", e.root)
	extra = strings.ReplaceAll(extra, "@outside@", e.outside)
	e.write(filepath.Join(e.root, config.FileName), fmt.Sprintf(testConfig, hubPath, extra))

	var b strings.Builder
	b.WriteString("# テストの計画書\n\n## 設計\n\n設計の本文。\n\n## 実装判断ログ\n\n## タスクリスト\n\n")
	for _, task := range tasks {
		b.WriteString(task + "\n\n")
	}
	e.write(e.planFile(), b.String())
	return e
}

func (e *testEnv) planFile() string {
	return filepath.Join(e.root, planPath)
}

// runは計画書を1回進める。
func (e *testEnv) run(note string, approve bool) (*Outcome, string) {
	e.t.Helper()
	var out bytes.Buffer
	res, err := e.pilot(note, approve, &out).Run(context.Background())
	if err != nil {
		e.t.Fatalf("Runのエラー = %v\n出力:\n%s", err, out.String())
	}
	return res, out.String()
}

// pilotは通常実行と中断の検証で共通の実行設定を組み立てる。
func (e *testEnv) pilot(note string, approve bool, out *bytes.Buffer) *Pilot {
	e.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		e.t.Fatal(err)
	}
	scenario, err := json.Marshal(e.scenario)
	if err != nil {
		e.t.Fatal(err)
	}
	env := append(testGitEnv(),
		envFake+"=1",
		envScenario+"="+string(scenario),
		envState+"="+e.calls,
		envPlan+"="+e.planFile(),
		envOutside+"="+e.outside,
		envPrivate+"="+filepath.Join(e.root, "docs", "private"),
	)
	p := &Pilot{
		PlanPath:   planPath,
		Root:       e.root,
		ConfigPath: filepath.Join(e.root, config.FileName),
		StateRoot:  filepath.Join(e.root, "tmp", "koryluspilot"),
		HubRoot:    e.hubRoot,
		Upstream:   e.upstream,
		Note:       note,
		Approve:    approve,
		RoleRunner: func(name string) runner.Runner {
			return runner.Runner{
				Prefix: []string{"env", "KORYLUSPILOT_ROLE=" + name},
				Dir:    filepath.Join(e.hubRoot, "clones", name),
				Env:    env,
			}
		},
		ClaudeCommand: exe,
		CodexCommand:  exe,
		Fetcher:       e,
		Now:           func() time.Time { return e.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			e.slept = append(e.slept, d)
			e.now = e.now.Add(d)
			return nil
		},
		Out: out,
	}
	return p
}

// Fetchは偽の使用率を返す。highUntilより前は上限を超える値を返す。
func (e *testEnv) Fetch(_ context.Context, tool agent.Tool, now time.Time) ([]usage.Sample, error) {
	e.fetchedAtCalls = append(e.fetchedAtCalls, len(e.callArgs()))
	percent := 10.0
	if now.Before(e.highUntil) && len(e.callArgs()) >= e.highAfterCalls {
		percent = 100
	}
	return []usage.Sample{
		{At: now, Tool: tool, Window: usage.FiveHour, Percent: percent, ResetsAt: e.highUntil, Source: usage.SourceUsageCommand},
	}, nil
}

func (e *testEnv) callArgs() [][]string {
	return fakeCalls(e.t, e.calls)
}

func (e *testEnv) state() *state.State {
	e.t.Helper()
	st, err := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p").Load()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *testEnv) events(kind state.EventKind) []state.Event {
	e.t.Helper()
	all, err := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p").ReadEvents()
	if err != nil {
		e.t.Fatal(err)
	}
	var res []state.Event
	for _, ev := range all {
		if ev.Kind == kind {
			res = append(res, ev)
		}
	}
	return res
}

func (e *testEnv) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		e.t.Fatal(err)
	}
}

func (e *testEnv) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...) //#nosec G204 -- テストが組み立てたgitの引数
	cmd.Dir = dir
	cmd.Env = testGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hubFileはハブのブランチbranchのファイルnameの内容を返す。
func (e *testEnv) hubFile(branch, name string) string {
	return e.git(e.hubRoot, "--git-dir=hub.git", "show", branch+":"+name)
}

// testGitEnvは、利用者のgitの設定に左右されず、コミットの作者を固定したgitの環境。
func testGitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=koryluspilot",
		"GIT_AUTHOR_EMAIL=koryluspilot@example.com",
		"GIT_COMMITTER_NAME=koryluspilot",
		"GIT_COMMITTER_EMAIL=koryluspilot@example.com",
	)
}

var (
	task1 = "- [ ] **1-1**: [Go] 1つ目のタスク\n  - **想定行数**: 約100行 (実装50行 + テスト50行)"
	task2 = "- [ ] **1-2**: [Go] 2つ目のタスク"
)

// setReviewsは設定ファイルのレビュー回数の上限と、レビューに使う役割を書き換える。
func (e *testEnv) setReviews(max int, reviewers ...string) {
	e.t.Helper()
	path := filepath.Join(e.root, config.FileName)
	src, err := os.ReadFile(path) //#nosec G304 -- テストがt.TempDirの下に書いた設定ファイル
	if err != nil {
		e.t.Fatal(err)
	}
	quoted := make([]string, len(reviewers))
	for i, r := range reviewers {
		quoted[i] = fmt.Sprintf("%q", r)
	}
	s := strings.Replace(string(src), "max_reviews = 1\nreviewers = [\"reviewer_codex\"]",
		fmt.Sprintf("max_reviews = %d\nreviewers = [%s]", max, strings.Join(quoted, ", ")), 1)
	e.write(path, s)
}

// reviewDocは、総合評価verdictと指摘findingsを持つレビュードキュメントを返す。
func reviewDoc(verdict string, findings ...string) string {
	return "# コードレビュー\n\n## 過去レビューの修正検証\n\n対象なし\n\n## ファイルごとのレビュー結果\n\n" +
		strings.Join(findings, "") +
		"## 設計改善の提案\n\n設計改善の提案はありません。\n\n## 総合評価\n\n**評価**: " + verdict + "\n\n**総評**: 偽のレビュー\n"
}

// reviewFindingは、区分categoryで、選択肢recommended (推奨) とotherを持つ指摘を返す。
func reviewFinding(title, category, recommended, other string) string {
	return "### " + title + "\n\n**ステータス**: 要修正\n\n**区分**: " + category + "\n\n**対応方針**:\n\n" +
		"- [ ] " + recommended + " (推奨)\n- [ ] " + other + "\n- [ ] その他 (下の回答欄に記入)\n\n**回答**:\n\n```\n\n```\n\n"
}

// reviewActionは、レビュードキュメントnameを書いて構造化出力を返すレビュアーの振る舞い。
func reviewAction(name, doc string) fakeAction {
	return fakeAction{Private: map[string]string{"reviews/" + name + ".md": doc}, Status: agent.StatusCompleted}
}

// approveは指摘の無いApproveのレビュー。
func approve(name string) fakeAction {
	return reviewAction(name, reviewDoc("Approve"))
}

// reviewDocFileは人間のチェックアウトのレビュードキュメントnameの内容を返す。
func (e *testEnv) reviewDocFile(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, "docs", "private", "reviews", name+".md")) //#nosec G304 -- t.TempDirの下のファイル
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// outputsOfは、構造化出力の記録のうちステップstepのものを返す。
func outputsOf(st *state.State, step state.Step) []state.Output {
	var res []state.Output
	for _, o := range st.Outputs {
		if o.Step == step {
			res = append(res, o)
		}
	}
	return res
}

func TestRun_TwoTasks(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1, task2}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
		fakeAction{Files: map[string]string{"b.go": "package a\n"}, CheckTask: "1-2", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-2-001"),
	)
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}

	st := e.state()
	want := []struct{ id, branch, start string }{{"1-1", "p-1-1", "p-base"}, {"1-2", "p-1-2", "p-1-1"}}
	if len(st.Tasks) != len(want) {
		t.Fatalf("終えたタスク = %+v、期待値 = %v", st.Tasks, want)
	}
	for i, w := range want {
		if got := st.Tasks[i]; got.ID != w.id || got.Branch != w.branch || got.Start != w.start {
			t.Errorf("Tasks[%d] = %+v、期待値 = %v", i, got, w)
		}
	}
	if st.Task != "" || st.Step != "" {
		t.Errorf("実行中のタスク = %q (%q)、期待値 = なし", st.Task, st.Step)
	}
	// 2つ目のタスクのブランチは、1つ目のタスクのブランチに積む。
	if got := e.hubFile("p-1-2", "a.go"); got != "package a" {
		t.Errorf("p-1-2のa.go = %q", got)
	}
	if got := e.hubFile("p-1-2", "README.md"); got != "mewst" {
		t.Errorf("p-1-2のREADME.md = %q", got)
	}

	calls := e.callArgs()
	if len(calls) != 6 {
		t.Fatalf("エージェントの呼び出し = %d回、期待値 = 6回", len(calls))
	}
	if got, want := promptOf(calls[0]), "/korylus-do-plan-task docs/private/plans/p.md 1-1 base=hub/p-base mode=unattended"; got != want {
		t.Errorf("実装のプロンプト = %q、期待値 = %q", got, want)
	}
	if flagValue(calls[0], "--effort") != "medium" || flagValue(calls[0], "--model") != "claude-opus-5-5" || flagValue(calls[0], "--json-schema") == "" {
		t.Errorf("実装の引数 = %v、effort・model・構造化出力のスキーマを期待", calls[0])
	}
	if slices.Contains(calls[0], "--resume") {
		t.Errorf("最初の実装でセッションを再開している: %v", calls[0])
	}
	// コミットは、実装したセッションを再開して依頼する。
	if got := promptOf(calls[1]); got != "/korylus-commit" {
		t.Errorf("コミットのプロンプト = %q", got)
	}
	if got := flagValue(calls[1], "--resume"); got != "session-1" {
		t.Errorf("コミットで再開したセッション = %q、期待値 = %q", got, "session-1")
	}
	if slices.Contains(calls[1], "--json-schema") {
		t.Errorf("コミットで構造化出力を求めている: %v", calls[1])
	}
	if got := promptOf(calls[3]); !strings.Contains(got, " 1-2 base=hub/p-1-1 ") {
		t.Errorf("2つ目のタスクの実装のプロンプト = %q", got)
	}

	if len(st.Outputs) != 4 || st.Outputs[0].Task != "1-1" || len(st.Outputs[0].Result.VerificationItems) != 1 {
		t.Errorf("構造化出力の記録 = %+v", st.Outputs)
	}
	snap, err := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p").LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse(string(snap))
	if err != nil {
		t.Fatal(err)
	}
	if p.DoneCount() != 2 {
		t.Errorf("スナップショットの完了したタスク = %d、期待値 = 2", p.DoneCount())
	}

	agents := e.events(state.EventAgent)
	if len(agents) != 6 || agents[0].Turns != 3 || agents[0].CostUSD != 0.25 || agents[0].Role != config.RoleImplementer {
		t.Errorf("エージェントの所要時間の記録 = %+v", agents)
	}
	if gates := e.events(state.EventGate); len(gates) != 2 || gates[0].Command != "test -f a.go" || gates[0].Outcome != state.OutcomeOK {
		t.Errorf("ゲートの所要時間の記録 = %+v", gates)
	}

	// 計画書の最後で、ハブのブランチを人間のチェックアウトへ取り込み、最後のブランチに切り替える。
	if got := e.git(e.root, "symbolic-ref", "--short", "HEAD"); got != "p-1-2" {
		t.Errorf("人間のチェックアウトのブランチ = %q、期待値 = p-1-2", got)
	}
	if got := e.git(e.root, "rev-parse", "p-1-1"); got != st.Tasks[0].Commit {
		t.Errorf("人間のチェックアウトのp-1-1 = %s、期待値 = %s", got, st.Tasks[0].Commit)
	}
	reportDir := report.Dir(e.root, "p")
	if out.Report != report.HTMLPath(reportDir, 1) || st.ReportVersion != 1 || len(st.Reports) != 1 || st.Reports[0].Tasks != 2 {
		t.Errorf("作業報告書 = %q、版 = %d、記録 = %+v", out.Report, st.ReportVersion, st.Reports)
	}
	rep, err := report.Load(reportDir, 1)
	if err != nil {
		t.Fatalf("作業報告書の元データを読めません: %v", err)
	}
	if len(rep.Tasks) != 2 || rep.Tasks[1].Files[0].Path != "b.go" || len(rep.Items) != 2 || rep.Items[1].ID != "task/1-2/1" || !rep.Preparation.Switched {
		t.Errorf("作業報告書の元データ = %+v", rep)
	}
	if _, err := os.Stat(out.Report); err != nil {
		t.Errorf("作業報告書のHTMLがありません: %v", err)
	}

	// 終えたあとにもう一度実行しても、エージェントを呼ばず、作業報告書の次の版も作らない。
	out, _ = e.run("", false)
	if out.Stopped() {
		t.Errorf("2回目の実行で止まった: %+v", out.State.Stop)
	}
	if n := len(e.callArgs()); n != 6 {
		t.Errorf("2回目の実行のあとのエージェントの呼び出し = %d回、期待値 = 6回", n)
	}
	if out.Report != report.HTMLPath(reportDir, 1) || out.State.ReportVersion != 1 {
		t.Errorf("2回目の実行の作業報告書 = %q (第%d版)", out.Report, out.State.ReportVersion)
	}
}

func TestRun_ReportProtected(t *testing.T) {
	t.Parallel()

	task := "- [ ] **1-1**: [Go] スキーマを変える\n  - **保護領域**: schema (postsに列を追加)"
	e := newTestEnv(t, []string{task}, "",
		fakeAction{
			Files: map[string]string{"a.go": "package a\n", "db/1.sql": "ALTER TABLE posts ADD x int;\n"}, CheckTask: "1-1", Status: agent.StatusCompleted,
			ProtectedChanges: json.RawMessage(`[{"area":"schema","summary":"postsに列を追加","reason":"設計のとおり"}]`),
		},
		fakeAction{Commit: true},
		fakeAction{
			Private: map[string]string{"reviews/p-1-1-001.md": reviewDoc("Approve")}, Status: agent.StatusCompleted,
			ProtectedChecks: json.RawMessage(`[{"area":"schema","point":"ロックの長さ","result":"ok","detail":"短い"}]`),
		},
	)
	// 人間のチェックアウトに未コミットの変更があれば、ブランチを切り替えない。
	e.write(filepath.Join(e.root, "README.md"), "手元の変更\n")
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	if got := e.git(e.root, "symbolic-ref", "--short", "HEAD"); got != "develop" {
		t.Errorf("未コミットの変更があるのに切り替えた: %q", got)
	}

	rep, err := report.Load(report.Dir(e.root, "p"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Preparation.Switched || rep.Preparation.Branch != "p-1-1" {
		t.Errorf("準備 = %+v", rep.Preparation)
	}
	if len(rep.Protected.Areas) != 1 {
		t.Fatalf("保護領域 = %+v", rep.Protected.Areas)
	}
	a := rep.Protected.Areas[0]
	if a.Name != "schema" || len(a.Final) != 1 || a.Final[0].Path != "db/1.sql" || len(a.Declarations) != 1 || len(a.Tasks) != 1 {
		t.Fatalf("schemaの変更 = %+v", a)
	}
	if at := a.Tasks[0]; !at.Declared || len(at.Changes) != 1 || at.Changes[0].Summary != "postsに列を追加" || len(at.Checks) != 1 || at.Checks[0].Detail != "短い" {
		t.Errorf("schemaのタスクごとの内訳 = %+v", at)
	}
	if n := len(rep.Items); n != 2 || rep.Items[1].ID != "protected/schema" || rep.Items[1].Kind != report.KindProtected {
		t.Errorf("確認項目 = %+v", rep.Items)
	}
}

func TestRun_ReviewApprove(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if len(calls) != 3 {
		t.Fatalf("エージェントの呼び出し = %d回、期待値 = 3回", len(calls))
	}
	// 1回目のレビューは、reviewersの最初の役割 (Codex) で新しいセッションを始める。
	rev := calls[2]
	if rev[0] != "exec" || slices.Contains(rev, "resume") || !slices.Contains(rev, "--output-schema") {
		t.Errorf("レビューの引数 = %v、Codexの新しいセッションを期待", rev)
	}
	prompt := promptOf(rev)
	for _, want := range []string{"$korylus-review hub/p-base docs/private/plans/p.md mode=unattended\n", "タスク 1-1 の差分", "宣言どおりに触れた保護領域はありません"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("レビューのプロンプト = %q、%q を含むことを期待", prompt, want)
		}
	}
	st := out.State
	if len(st.ReviewLog) != 1 {
		t.Fatalf("レビューの記録 = %+v", st.ReviewLog)
	}
	if got := st.ReviewLog[0]; got.Task != "1-1" || got.Number != 1 || got.Role != "reviewer_codex" || got.Doc != "docs/private/reviews/p-1-1-001.md" || got.Verdict != "Approve" {
		t.Errorf("レビューの記録 = %+v", got)
	}
	if len(st.Tasks) != 1 || len(st.Deferred) != 0 {
		t.Errorf("状態 = %+v", st)
	}
	if reviews := outputsOf(st, state.StepReview); len(reviews) != 1 || reviews[0].Role != "reviewer_codex" {
		t.Errorf("レビューの構造化出力の記録 = %+v", reviews)
	}
}

func TestRun_ReviewDocAllowedOutside(t *testing.T) {
	t.Parallel()

	watch := `
[[gate.watch]]
name = "private"
path = "@root@/docs/private"
project_path = "docs/private"
`
	e := newTestEnv(t, []string{task1}, watch,
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Approve")),
	)
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("タスクのレビュードキュメントの作成で止まった: %+v\n%s", out.State.Stop, log)
	}

	// 別のタスクのレビュードキュメントは、レビューのステップでも書かせない。
	e2 := newTestEnv(t, []string{task1}, watch,
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		fakeAction{Private: map[string]string{"reviews/p-1-1-001.md": reviewDoc("Approve"), "reviews/p-2-1-001.md": "x"}, Status: agent.StatusCompleted},
	)
	out, _ = e2.run("", false)
	if !out.Stopped() || out.State.Step != state.StepReview || !strings.Contains(strings.Join(out.State.Stop.Details, "\n"), "p-2-1-001.md") {
		t.Errorf("別のタスクのレビュードキュメントの作成で止まらなかった: %+v", out.State.Stop)
	}
}

func TestRun_ReviewLoop(t *testing.T) {
	t.Parallel()

	declared := "- [ ] **1-1**: [Go] スキーマを変えるタスク\n  - **保護領域**: schema (列を追加)"
	checks := json.RawMessage(`[{"area":"schema","point":"ロックの長さ","result":"ok","detail":"列の追加だけ"}]`)
	first := reviewAction("p-1-1-001", reviewDoc("Request Changes",
		reviewFinding("1. エラーを返していない", "自動対応可", "修正案の通りエラーを返す", "現状のまま"),
		reviewFinding("2. 関数が長い", "自動対応可", "後続タスクに積む", "提案通り分ける"),
	))
	first.ProtectedChecks = checks
	e := newTestEnv(t, []string{declared}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n", "db/1.sql": "alter table t add c int;\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		first,
		// 1回目のレビュアーが、同じセッションで修正とコミットを行う。
		fakeAction{Files: map[string]string{"fix.go": "package a\n"}, Status: agent.StatusCompleted, ProtectedChanges: json.RawMessage(`[]`)},
		fakeAction{Commit: true},
		// 2回目は別の役割 (Claude Code) でレビューし、Commentで終える。
		reviewAction("p-1-1-002", reviewDoc("Comment", reviewFinding("1. 命名", "自動対応可", "現状維持", "名前を変える"))),
	)
	e.setReviews(3, "reviewer_codex", "reviewer_claude")
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}

	calls := e.callArgs()
	if len(calls) != 6 {
		t.Fatalf("エージェントの呼び出し = %d回、期待値 = 6回", len(calls))
	}
	if got := promptOf(calls[2]); !strings.Contains(got, "- schema (DBスキーマ): ロックの長さ / ロールバックできるか\n") {
		t.Errorf("レビューのプロンプトに宣言どおりに触れた保護領域と観点が無い: %q", got)
	}
	fix, commit := calls[3], calls[4]
	// 修正とコミットは、レビューを行ったCodexのセッションを再開して依頼する。
	if fix[0] != "exec" || !slices.Contains(fix, "resume") || fix[len(fix)-2] != "thread-3" {
		t.Errorf("修正の引数 = %v、レビューのセッション thread-3 の再開を期待", fix)
	}
	if got := promptOf(fix); !strings.Contains(got, "docs/private/reviews/p-1-1-001.md の対応方針に回答しました") {
		t.Errorf("修正のプロンプト = %q", got)
	}
	if commit[0] != "exec" || commit[len(commit)-2] != "thread-3" || promptOf(commit) != "$korylus-commit" {
		t.Errorf("修正のあとのコミットの引数 = %v", commit)
	}
	second := calls[5]
	if second[0] == "exec" || slices.Contains(second, "--resume") || flagValue(second, "--effort") != "xhigh" {
		t.Errorf("2回目のレビューの引数 = %v、Claude Codeの新しいセッションをxhighで期待", second)
	}

	// 1回目のレビュードキュメントには、推奨の選択肢にチェックが入る。
	doc := e.reviewDocFile("p-1-1-001")
	for _, want := range []string{"- [x] 修正案の通りエラーを返す (推奨)", "- [x] 後続タスクに積む (推奨)", "- [ ] 現状のまま\n", "- [ ] 提案通り分ける\n"} {
		if !strings.Contains(doc, want) {
			t.Errorf("回答したレビュードキュメントに %q が無い:\n%s", want, doc)
		}
	}
	if got := e.hubFile("p-1-1", "fix.go"); got != "package a" {
		t.Errorf("修正がハブに取り込まれていない: %q", got)
	}

	st := out.State
	var verdicts []string
	for _, rec := range st.ReviewLog {
		verdicts = append(verdicts, rec.Role+":"+rec.Verdict)
	}
	if want := []string{"reviewer_codex:Request Changes", "reviewer_claude:Comment"}; !slices.Equal(verdicts, want) {
		t.Errorf("レビューの記録 = %v、期待値 = %v", verdicts, want)
	}
	wantDeferred := []state.DeferredFinding{
		{Task: "1-1", Doc: "docs/private/reviews/p-1-1-001.md", Title: "2. 関数が長い", Choice: "後続タスクに積む (推奨)"},
		{Task: "1-1", Doc: "docs/private/reviews/p-1-1-002.md", Title: "1. 命名", Choice: "現状維持 (推奨)"},
	}
	if !slices.Equal(st.Deferred, wantDeferred) {
		t.Errorf("先送りした指摘 = %+v、期待値 = %+v", st.Deferred, wantDeferred)
	}
	if reviews := outputsOf(st, state.StepReview); len(reviews) != 2 || len(reviews[0].Result.ProtectedChecks) != 1 || reviews[0].Result.ProtectedChecks[0].Point != "ロックの長さ" {
		t.Errorf("レビューの構造化出力の記録 = %+v", reviews)
	}
	if fixes := outputsOf(st, state.StepFix); len(fixes) != 1 || fixes[0].Role != "reviewer_codex" {
		t.Errorf("修正の構造化出力の記録 = %+v", fixes)
	}
	if len(st.Tasks) != 1 || st.Reviews != 0 || st.ReviewDoc != "" {
		t.Errorf("タスクを終えたあとの状態 = %+v", st)
	}
	if gates := e.events(state.EventGate); len(gates) != 2 {
		t.Errorf("ゲートの所要時間の記録 = %+v、修正のあとにもゲート検査を期待", gates)
	}
}

func TestRun_ReviewNeedsHuman(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment",
			reviewFinding("1. 合意から外れる", "要人間判断", "設計どおりに戻す", "現状のまま"),
			reviewFinding("2. 命名", "自動対応可", "名前を変える", "現状のまま"),
		)),
		fakeAction{Files: map[string]string{"fix.go": "package a\n"}, Status: agent.StatusCompleted},
		fakeAction{Commit: true},
	)
	out, _ := e.run("", false)
	if !out.Stopped() {
		t.Fatal("要人間判断の指摘で止まらなかった")
	}
	stop := out.State.Stop
	if out.State.Step != state.StepAnswer || stop.Role != "reviewer_codex" || len(stop.Details) != 1 || !strings.Contains(stop.Details[0], "「1. 合意から外れる」: 要人間判断") {
		t.Errorf("止まった理由 = %+v (ステップ %q)", stop, out.State.Step)
	}
	if doc := e.reviewDocFile("p-1-1-001"); strings.Contains(doc, "[x]") {
		t.Errorf("止めたのにレビュードキュメントに回答した:\n%s", doc)
	}

	// 人間がレビュードキュメントで選んでから再開すると、その選択肢と推奨どおりの回答で修正へ進む。
	path := filepath.Join(e.root, "docs", "private", "reviews", "p-1-1-001.md")
	e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "- [ ] 現状のまま\n", "- [x] 現状のまま\n", 1))
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("回答のあとに止まった: %+v\n%s", out.State.Stop, log)
	}
	doc := e.reviewDocFile("p-1-1-001")
	if !strings.Contains(doc, "- [ ] 設計どおりに戻す (推奨)\n- [x] 現状のまま") || !strings.Contains(doc, "- [x] 名前を変える (推奨)") {
		t.Errorf("回答したレビュードキュメント:\n%s", doc)
	}
	if n := len(e.callArgs()); n != 5 {
		t.Errorf("エージェントの呼び出し = %d回、期待値 = 5回 (Commentのため再レビューしない)", n)
	}
	if want := []state.DeferredFinding{{Task: "1-1", Doc: "docs/private/reviews/p-1-1-001.md", Title: "1. 合意から外れる", Choice: "現状のまま"}}; !slices.Equal(out.State.Deferred, want) {
		t.Errorf("先送りした指摘 = %+v", out.State.Deferred)
	}
}

func TestRun_ReviewLimit(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Request Changes", reviewFinding("1. 不具合", "自動対応可", "直す", "現状のまま"))),
		fakeAction{Files: map[string]string{"fix.go": "package a\n"}, Status: agent.StatusCompleted},
		fakeAction{Commit: true},
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "上限の1回") || out.State.Step != state.StepAnswer {
		t.Fatalf("上限のRequest Changesで止まらなかった: %+v (ステップ %q)", out.State.Stop, out.State.Step)
	}

	// 再開すると、回答と修正のあと再レビューせずにタスクを終える。
	out, log := e.run("確認しました", false)
	if out.Stopped() {
		t.Fatalf("再開したあとに止まった: %+v\n%s", out.State.Stop, log)
	}
	if n := len(e.callArgs()); n != 5 || len(out.State.Tasks) != 1 {
		t.Errorf("エージェントの呼び出し = %d回、終えたタスク = %+v", n, out.State.Tasks)
	}
	if !strings.Contains(log, "補足 (-note) は使いませんでした") {
		t.Errorf("回答のステップで補足を使わなかった警告が無い:\n%s", log)
	}
}

func TestRun_ReviewRequestChangesWithoutFix(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Request Changes", reviewFinding("1. 大きな変更", "自動対応可", "後続タスクに積む", "直す"))),
		approve("p-1-1-002"),
	)
	e.setReviews(2, "reviewer_codex", "reviewer_claude")
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	// 修正を飛ばしても、Request Changesなので別の役割で再レビューする。
	calls := e.callArgs()
	if len(calls) != 4 || calls[3][0] == "exec" {
		t.Errorf("エージェントの呼び出し = %v、修正を飛ばしてClaude Codeでの再レビューを期待", calls)
	}
	if gates := e.events(state.EventGate); len(gates) != 1 {
		t.Errorf("修正を飛ばしたのにゲート検査をやり直した: %+v", gates)
	}
}

func TestRun_ReviewDocMissing(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		fakeAction{Status: agent.StatusCompleted},
		approve("p-1-1-001"),
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "レビュードキュメントが docs/private/reviews/p-1-1-NNN.md に見つかりません") || out.State.Step != state.StepReview {
		t.Fatalf("レビュードキュメントが無いのに止まらなかった: %+v", out.State.Stop)
	}

	// 再開すると、同じレビューのセッションを再開する。
	out, log := e.run("レビュードキュメントを書いてください", false)
	if out.Stopped() {
		t.Fatalf("再開したあとに止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if resumed := calls[3]; !slices.Contains(resumed, "resume") || resumed[len(resumed)-2] != "thread-3" || !strings.HasSuffix(promptOf(resumed), "補足: レビュードキュメントを書いてください\n") {
		t.Errorf("再開したレビューの引数 = %v", resumed)
	}
}

func TestRun_ReviewDocInvalid(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", "# コードレビュー\n\n## 総合評価\n\n**評価**: [Approve / Request Changes / Comment]\n"),
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "読み取れません") || len(out.State.Stop.Details) != 1 || !strings.Contains(out.State.Stop.Details[0], "評価") {
		t.Errorf("読み取れないレビュードキュメントで止まらなかった: %+v", out.State.Stop)
	}
}

func TestRun_EscalationAndResume(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusEscalated},
		fakeAction{CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	out, _ := e.run("", false)
	if !out.Stopped() {
		t.Fatal("エスカレーションで止まらなかった")
	}
	stop := out.State.Stop
	if stop.Role != config.RoleImplementer || out.State.Step != state.StepImplement || len(stop.Details) != 1 || !strings.Contains(stop.Details[0], "どちらにするか") {
		t.Errorf("止まった理由 = %+v (ステップ %q)", stop, out.State.Step)
	}

	e.now = e.now.Add(time.Hour)
	out, log := e.run("Aで進めてください", false)
	if out.Stopped() {
		t.Fatalf("再開したあとに止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if got := promptOf(calls[1]); !strings.HasSuffix(got, " mode=unattended note=Aで進めてください") {
		t.Errorf("再開した実装のプロンプト = %q", got)
	}
	if got := flagValue(calls[1], "--resume"); got != "session-1" {
		t.Errorf("再開したセッション = %q、期待値 = %q", got, "session-1")
	}
	if waits := e.events(state.EventHumanWait); len(waits) != 1 || waits[0].Duration() != time.Hour || waits[0].Role != config.RoleImplementer {
		t.Errorf("人間の待ちの記録 = %+v", waits)
	}
	if got := e.hubFile("p-1-1", "a.go"); got != "package a" {
		t.Errorf("エスカレーションの前の変更が取り込まれていない: %q", got)
	}
	// 止まった履歴に、再開した時刻とともに残る。
	if log := out.State.StopLog; len(log) != 1 || log[0].Task != "1-1" || log[0].Step != state.StepImplement || !log[0].ResumedAt.Equal(log[0].At.Add(time.Hour)) {
		t.Errorf("止まった履歴 = %+v", log)
	}
}

func TestRun_GateRework(t *testing.T) {
	t.Parallel()

	t.Run("差し戻しで直れば先へ進む", func(t *testing.T) {
		t.Parallel()

		e := newTestEnv(t, []string{task1}, "",
			fakeAction{Files: map[string]string{"b.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
			fakeAction{Commit: true},
			fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusCompleted},
			fakeAction{Commit: true},
			approve("p-1-1-001"),
		)
		out, log := e.run("", false)
		if out.Stopped() {
			t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
		}
		calls := e.callArgs()
		if len(calls) != 5 {
			t.Fatalf("エージェントの呼び出し = %d回、期待値 = 5回", len(calls))
		}
		rework := calls[2]
		if got := promptOf(rework); !strings.Contains(got, "コマンド: test -f a.go (終了コード 1") {
			t.Errorf("差し戻しのプロンプト = %q", got)
		}
		// 差し戻しは修正のeffortで、実装したセッションを再開する。
		if flagValue(rework, "--effort") != "low" || flagValue(rework, "--resume") != "session-1" {
			t.Errorf("差し戻しの引数 = %v", rework)
		}
		if gates := e.events(state.EventGate); len(gates) != 2 || gates[0].Outcome != state.OutcomeFailed || gates[0].ExitCode != 1 {
			t.Errorf("ゲートの所要時間の記録 = %+v", gates)
		}
	})

	t.Run("差し戻しのあとも失敗したら止まる", func(t *testing.T) {
		t.Parallel()

		e := newTestEnv(t, []string{task1}, "",
			fakeAction{Files: map[string]string{"b.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
			fakeAction{Commit: true},
			fakeAction{Status: agent.StatusCompleted},
			fakeAction{Commit: true},
		)
		out, _ := e.run("", false)
		if !out.Stopped() {
			t.Fatal("止まらなかった")
		}
		if stop := out.State.Stop; stop.Role != config.RoleImplementer || !strings.Contains(stop.Reason, "差し戻しのあとも失敗") || out.State.Step != state.StepGate {
			t.Errorf("止まった理由 = %+v (ステップ %q)", stop, out.State.Step)
		}
	})
}

func TestRun_GateFinding(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n", "db/1.sql": "create table t ();\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
	)
	out, _ := e.run("", false)
	if !out.Stopped() {
		t.Fatal("宣言の無い保護領域への変更で止まらなかった")
	}
	stop := out.State.Stop
	if stop.Role != config.RoleImplementer || len(stop.Details) != 1 || !strings.Contains(stop.Details[0], "宣言していない保護領域 schema") {
		t.Errorf("止まった理由 = %+v", stop)
	}
	// 止める検知があれば、ゲートのコマンドは実行しない。
	if gates := e.events(state.EventGate); len(gates) != 0 {
		t.Errorf("ゲートのコマンドを実行した: %+v", gates)
	}
}

func TestRun_TaskNotChecked(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusCompleted},
		fakeAction{Commit: true},
	)
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "チェックボックス") {
		t.Errorf("チェックの無いタスクで止まらなかった: %+v", out.State.Stop)
	}
}

func TestRun_BaselineChanged(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusEscalated},
		fakeAction{CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	if out, _ := e.run("", false); !out.Stopped() {
		t.Fatal("エスカレーションで止まらなかった")
	}

	src, err := os.ReadFile(e.planFile())
	if err != nil {
		t.Fatal(err)
	}
	e.write(e.planFile(), strings.Replace(string(src), "設計の本文。", "人間が直した設計の本文。", 1))
	out, _ := e.run("", false)
	if !out.Stopped() || out.State.Stop.Role != config.RoleDesigner || !strings.Contains(out.State.Stop.Reason, "計画書") {
		t.Fatalf("計画書の変更で止まらなかった: %+v", out.State.Stop)
	}
	if out.State.Step != state.StepImplement {
		t.Errorf("止まったステップ = %q、期待値 = %q (元のステップのまま)", out.State.Step, state.StepImplement)
	}

	configPath := filepath.Join(e.root, config.FileName)
	cfg, err := os.ReadFile(configPath) //#nosec G304 -- テストがt.TempDirの下に書いた設定ファイル
	if err != nil {
		t.Fatal(err)
	}
	e.write(configPath, string(cfg)+"# 人間が足したコメント\n")
	out, _ = e.run("", false)
	if !out.Stopped() || !strings.Contains(out.State.Stop.Reason, "設定ファイル") {
		t.Fatalf("設定ファイルの変更で止まらなかった: %+v", out.State.Stop)
	}
	if n := len(e.callArgs()); n != 1 {
		t.Errorf("基準の変更で止まっている間のエージェントの呼び出し = %d回、期待値 = 1回", n)
	}

	// -approveで新しい基準として受け入れれば、止まったステップから続く。
	out, log := e.run("Aで進めてください", true)
	if out.Stopped() {
		t.Fatalf("-approveのあとに止まった: %+v\n%s", out.State.Stop, log)
	}
	snap, err := state.NewDir(filepath.Join(e.root, "tmp", "koryluspilot"), "p").LoadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap), "人間が直した設計の本文。") {
		t.Error("-approveで計画書のスナップショットを取り直していない")
	}
}

func TestRun_UsageWait(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	start := e.now
	e.highUntil = start.Add(time.Hour)
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	if len(e.slept) != 1 || e.slept[0] != time.Hour+resetMargin {
		t.Errorf("待った時間 = %v、期待値 = [%v]", e.slept, time.Hour+resetMargin)
	}
	waits := e.events(state.EventUsageWait)
	if len(waits) != 1 || waits[0].Window != string(usage.FiveHour) || waits[0].Tool != string(agent.Claude) || waits[0].Step != state.StepImplement {
		t.Errorf("使用量の待機の記録 = %+v", waits)
	}
	if agents := e.events(state.EventAgent); len(agents) == 0 || agents[0].Started.Before(e.highUntil) {
		t.Errorf("リセットの前にエージェントを起動した: %+v", agents)
	}
}

func TestRun_RateLimited(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{RateLimited: true},
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		approve("p-1-1-001"),
	)
	// 1回目の呼び出しで上限に当たったあとは、リセットまで上限を超えた使用率を返す。
	e.highUntil = e.now.Add(2 * time.Hour)
	e.highAfterCalls = 1
	out, log := e.run("", false)
	if out.Stopped() {
		t.Fatalf("止まった: %+v\n%s", out.State.Stop, log)
	}
	calls := e.callArgs()
	if len(calls) != 4 {
		t.Fatalf("エージェントの呼び出し = %d回、期待値 = 4回", len(calls))
	}
	if flagValue(calls[1], "--resume") != "session-1" || !strings.Contains(promptOf(calls[1]), "中断したところから作業を続けて") {
		t.Errorf("上限のあとの再開の引数 = %v", calls[1])
	}
	if len(e.slept) == 0 {
		t.Error("上限に当たったあとにリセットを待たなかった")
	}
	if agents := e.events(state.EventAgent); len(agents) != 4 || agents[0].Outcome != state.OutcomeRateLimited {
		t.Errorf("エージェントの所要時間の記録 = %+v", agents)
	}
}

func TestRun_Outside(t *testing.T) {
	t.Parallel()

	watch := `
[[gate.watch]]
name = "private"
path = "@root@/docs/private"
project_path = "docs/private"

[[gate.watch]]
name = "claude-home"
path = "@outside@"
`
	e := newTestEnv(t, []string{task1}, watch,
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Outside: "settings.json", Status: agent.StatusCompleted},
	)
	out, _ := e.run("", false)
	if !out.Stopped() {
		t.Fatal("リポジトリの外の変更で止まらなかった")
	}
	stop := out.State.Stop
	// 計画書へのチェックは許可されたパスなので検知せず、監視するパスの外への書き込みだけを検知する。
	if stop.Role != config.RoleImplementer || len(stop.Details) != 1 || !strings.Contains(stop.Details[0], "claude-home") || !strings.Contains(stop.Details[0], "settings.json (追加)") {
		t.Errorf("止まった理由 = %+v", stop)
	}
	if out.State.OutsideBefore != nil {
		t.Error("比べ終えたステップの前の状態が残っている")
	}
}

func TestSkillPrompt(t *testing.T) {
	t.Parallel()

	if got := skillPrompt(agent.Claude, "korylus-commit", ""); got != "/korylus-commit" {
		t.Errorf("Claude Codeのプロンプト = %q", got)
	}
	if got := skillPrompt(agent.Codex, "korylus-review", "a b"); got != "$korylus-review a b" {
		t.Errorf("Codexのプロンプト = %q", got)
	}
}

func TestRun_AnswerResumeImportsHumanCommit(t *testing.T) {
	t.Parallel()

	for _, verdict := range []string{"Comment", "Request Changes"} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, []string{task1}, "",
				fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
				fakeAction{Commit: true},
				reviewAction("p-1-1-001", reviewDoc(verdict, reviewFinding("人間の判断", "要人間判断", "現状のまま", "修正する"))),
				approve("p-1-1-002"),
			)
			e.setReviews(2, "reviewer_codex", "reviewer_claude")
			if out, _ := e.run("", false); !out.Stopped() || out.State.Step != state.StepAnswer {
				t.Fatalf("回答待ちで停止しなかった: %+v", out.State)
			}
			clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
			e.write(filepath.Join(clone, "human.go"), "package a\n")
			e.git(clone, "add", "human.go")
			e.git(clone, "commit", "-qm", "停止した原因を修正")
			commit := e.git(clone, "rev-parse", "HEAD")
			path := filepath.Join(e.root, "docs/private/reviews/p-1-1-001.md")
			e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "[ ] 現状のまま", "[x] 現状のまま", 1))
			out, log := e.run("修正しました", false)
			if out.Stopped() || len(out.State.Tasks) != 1 || out.State.Tasks[0].Commit != commit {
				t.Fatalf("人間のコミットを取り込んで完了しなかった: %+v\n%s", out.State, log)
			}
			if got := e.hubFile("p-1-1", "human.go"); got != "package a" {
				t.Errorf("人間の修正 = %q", got)
			}
			if gates := e.events(state.EventGate); len(gates) != 2 {
				t.Errorf("ゲート検査 = %d回、期待値 = 2回", len(gates))
			}
			if verdict == "Request Changes" {
				other := filepath.Join(e.hubRoot, "clones", "reviewer_claude")
				if got := e.git(other, "show", "HEAD:human.go"); got != "package a" {
					t.Errorf("次のレビュアーに人間の修正が届いていない: %q", got)
				}
			}
		})
	}
}

func TestRun_AnswerResumeRejectsDirtyClone(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment", reviewFinding("人間の判断", "要人間判断", "現状のまま", "修正する"))),
	)
	if out, _ := e.run("", false); !out.Stopped() {
		t.Fatal("回答待ちで停止しなかった")
	}
	clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
	e.write(filepath.Join(clone, "human.go"), "package a\n")
	path := filepath.Join(e.root, "docs/private/reviews/p-1-1-001.md")
	e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "[ ] 現状のまま", "[x] 現状のまま", 1))
	out, _ := e.run("", false)
	if !out.Stopped() || !strings.Contains(strings.Join(out.State.Stop.Details, "\n"), "未コミット") || len(out.State.Tasks) != 0 {
		t.Fatalf("未コミットの修正を検出しなかった: %+v", out.State)
	}
	e.git(clone, "add", "human.go")
	e.git(clone, "commit", "-qm", "停止した原因を修正")
	out, log := e.run("", false)
	if out.Stopped() || len(out.State.Tasks) != 1 || out.State.ResumeStep != "" {
		t.Fatalf("コミット後に回答へ戻れなかった: %+v\n%s", out.State, log)
	}
}

func TestRun_AnswerResumeGateReworkReturnsToAnswer(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment", reviewFinding("人間の判断", "要人間判断", "現状のまま", "修正する"))),
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, Status: agent.StatusCompleted},
		fakeAction{Commit: true},
	)
	if out, _ := e.run("", false); !out.Stopped() {
		t.Fatal("回答待ちで停止しなかった")
	}
	clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
	e.git(clone, "rm", "a.go")
	e.git(clone, "commit", "-qm", "ゲートを失敗させる変更")
	out, log := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepAnswer || len(out.State.Tasks) != 0 {
		t.Fatalf("ゲートから回答待ちへ戻らなかった: %+v\n%s", out.State, log)
	}
	if gates := e.events(state.EventGate); len(gates) != 3 || gates[1].Outcome != state.OutcomeFailed || gates[2].Outcome != state.OutcomeOK {
		t.Errorf("再開後のゲートと差し戻し = %+v", gates)
	}
	path := filepath.Join(e.root, "docs/private/reviews/p-1-1-001.md")
	e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "[ ] 現状のまま", "[x] 現状のまま", 1))
	out, log = e.run("", false)
	if out.Stopped() || len(out.State.Tasks) != 1 {
		t.Fatalf("回答後に完了しなかった: %+v\n%s", out.State, log)
	}
	if gates := e.events(state.EventGate); len(gates) != 3 {
		t.Errorf("変更が無いのにゲート検査を繰り返した: %d回", len(gates))
	}
}

func TestRun_AnswerResumeChecksAlreadyImportedCommit(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment", reviewFinding("人間の判断", "要人間判断", "現状のまま", "修正する"))),
	)
	if out, _ := e.run("", false); !out.Stopped() {
		t.Fatal("回答待ちで停止しなかった")
	}
	clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
	e.write(filepath.Join(clone, "human.go"), "package a\n")
	e.git(clone, "add", "human.go")
	e.git(clone, "commit", "-qm", "停止した原因を修正")
	// 取り込み後、検査前にプロセスが落ちた状態を再現する。
	st := e.state()
	st.ResumeStep, st.Step, st.Stop = state.StepAnswer, state.StepGate, nil
	if err := state.NewDir(filepath.Join(e.root, "tmp/koryluspilot"), "p").Save(st, e.now); err != nil {
		t.Fatal(err)
	}
	e.git(e.hubRoot, "--git-dir=hub.git", "fetch", clone, "p-1-1:p-1-1")
	path := filepath.Join(e.root, "docs/private/reviews/p-1-1-001.md")
	e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "[ ] 現状のまま", "[x] 現状のまま", 1))
	out, log := e.run("", false)
	if out.Stopped() || len(out.State.Tasks) != 1 {
		t.Fatalf("中断後に完了しなかった: %+v\n%s", out.State, log)
	}
	if gates := e.events(state.EventGate); len(gates) != 2 {
		t.Errorf("取り込み済みの未検査コミットを検査しなかった: ゲート = %d回", len(gates))
	}
}

func TestRun_AnswerResumeChecksProtectedChanges(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment", reviewFinding("人間の判断", "要人間判断", "現状のまま", "修正する"))),
	)
	if out, _ := e.run("", false); !out.Stopped() {
		t.Fatal("回答待ちで停止しなかった")
	}
	clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
	e.write(filepath.Join(clone, "db/human.sql"), "alter table t add c int;\n")
	e.git(clone, "add", "db/human.sql")
	e.git(clone, "commit", "-qm", "宣言の無いスキーマ変更")
	path := filepath.Join(e.root, "docs/private/reviews/p-1-1-001.md")
	e.write(path, strings.Replace(e.reviewDocFile("p-1-1-001"), "[ ] 現状のまま", "[x] 現状のまま", 1))
	out, log := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepGate || !strings.Contains(strings.Join(out.State.Stop.Details, "\n"), "schema") || len(out.State.Tasks) != 0 {
		t.Fatalf("再開時の宣言外の変更を検出しなかった: %+v\n%s", out.State, log)
	}
}

func TestRun_ReviewCheckedByReviewer(t *testing.T) {
	t.Parallel()

	finding := strings.Replace(reviewFinding("合意から外れる", "要人間判断", "設計どおりに戻す", "現状のまま"), "- [ ] 設計どおりに戻す", "- [x] 設計どおりに戻す", 1)
	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		reviewAction("p-1-1-001", reviewDoc("Comment", finding)),
	)
	out, _ := e.run("", false)
	if !out.Stopped() || out.State.Step != state.StepAnswer || !strings.Contains(out.State.Stop.Reason, "レビュアーが対応方針の選択肢にチェックを入れています") ||
		!strings.Contains(strings.Join(out.State.Stop.Details, "\n"), "「合意から外れる」: 要人間判断") {
		t.Fatalf("レビュアーのチェックで止まらなかった: %+v (ステップ %q)", out.State.Stop, out.State.Step)
	}
	if n := len(e.callArgs()); n != 3 {
		t.Errorf("エージェントの呼び出し = %d回、期待値 = 3回 (修正へ進まない)", n)
	}

	// 人間がチェックを選び直して再開すると、そのチェックを回答として進む。
	path := filepath.Join(e.root, "docs", "private", "reviews", "p-1-1-001.md")
	doc := strings.Replace(e.reviewDocFile("p-1-1-001"), "- [x] 設計どおりに戻す", "- [ ] 設計どおりに戻す", 1)
	e.write(path, strings.Replace(doc, "- [ ] 現状のまま\n", "- [x] 現状のまま\n", 1))
	out, log := e.run("", false)
	if out.Stopped() || len(out.State.Tasks) != 1 {
		t.Fatalf("再開したあとに完了しなかった: %+v\n%s", out.State, log)
	}
	if n := len(e.callArgs()); n != 3 {
		t.Errorf("エージェントの呼び出し = %d回、期待値 = 3回 (先送りのため修正しない)", n)
	}
}

func TestRun_ReviewResumeImportsHumanCommit(t *testing.T) {
	t.Parallel()

	e := newTestEnv(t, []string{task1}, "",
		fakeAction{Files: map[string]string{"a.go": "package a\n"}, CheckTask: "1-1", Status: agent.StatusCompleted},
		fakeAction{Commit: true},
		fakeAction{Status: agent.StatusCompleted},
		approve("p-1-1-001"),
	)
	if out, _ := e.run("", false); !out.Stopped() || out.State.Step != state.StepReview {
		t.Fatalf("レビューのステップで止まらなかった: %+v", out.State)
	}
	clone := filepath.Join(e.hubRoot, "clones", "reviewer_codex")
	e.write(filepath.Join(clone, "human.go"), "package a\n")
	e.git(clone, "add", "human.go")
	e.git(clone, "commit", "-qm", "停止した原因を修正")
	commit := e.git(clone, "rev-parse", "HEAD")

	out, log := e.run("レビュードキュメントを書いてください", false)
	if out.Stopped() || len(out.State.Tasks) != 1 || out.State.Tasks[0].Commit != commit {
		t.Fatalf("人間のコミットを取り込んで完了しなかった: %+v\n%s", out.State, log)
	}
	if got := e.hubFile("p-1-1", "human.go"); got != "package a" {
		t.Errorf("人間の修正 = %q", got)
	}
	if gates := e.events(state.EventGate); len(gates) != 2 {
		t.Errorf("ゲート検査 = %d回、期待値 = 2回", len(gates))
	}
	calls := e.callArgs()
	if resumed := calls[3]; !slices.Contains(resumed, "resume") || resumed[len(resumed)-2] != "thread-3" || !strings.HasSuffix(promptOf(resumed), "補足: レビュードキュメントを書いてください\n") {
		t.Errorf("再開したレビューの引数 = %v", resumed)
	}
}
