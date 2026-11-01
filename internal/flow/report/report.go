// Package reportは、マージするPRなど、計画書を進めた結果の事実をMarkdownで組み立てる。
//
// 状態・所要時間の記録・計画書・gitから集めた事実だけを並べ、エージェントの説明を混ぜない。
// 事実と異なる要約が混ざって、人の確認を誤らせないためである。処理の流れなどの説明は、
// 実装者がWikinoの作業報告書に書く。作業報告書の本文は読まず、そのURLだけを載せる。
package report

import (
	"cmp"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/korylus/tools/internal/flow/branch"
	"github.com/korylus/tools/internal/flow/state"
	"github.com/korylus/tools/internal/plan"
)

// timeLayoutは時刻の表示形式。記録した時刻のオフセットのまま表示する。
const timeLayout = "2006-01-02 15:04 -07:00"

// constraintsSectionは、保護領域の変更が沿うべき方針を書いた計画書の節の名前。
const constraintsSection = "合意済みの設計制約"

// partOrderは想定行数の内訳を並べる順序。
var partOrder = []string{"実装", "テスト"}

// Inputは作業報告書1版を組み立てる入力。
type Input struct {
	// PlanURLは計画書の指定 (WikinoのページのURL)。
	PlanURL string
	// Nameは計画書名。見出しに使う。
	Name string
	// Planは計画書を読み取ったもの。
	Plan  *plan.Plan
	State *state.State
	// Eventsは所要時間の記録。
	Events []state.Event
	// Changesはタスク番号ごとの、元のブランチからタスクのブランチまでの変更。
	Changes map[string]Changes
}

// Changesはタスク1件のブランチの変更。
type Changes struct {
	Files []branch.FileChange
	// Missingは、ローカルに残っていないため変更を読めなかったブランチ。
	Missing []string
	// Errは、Missing以外の理由で変更を読めなかったときの理由。読めたら空文字。
	Err string
}

// builderは事実を書き出す作業領域。
type builder struct {
	strings.Builder
	in    Input
	tasks map[string]plan.Task
	// recordsは載せるタスク (ブランチを準備し、実装を終えたもの) の記録。
	records []state.TaskRecord
	// worksは、実装者がブランチを作らずに行ったタスク ([implementer]) の番号を、進めた順に並べたもの。
	works []string
}

// Buildは事実をMarkdownで組み立てる。
func Build(in Input) []byte {
	b := newBuilder(in)
	b.overview()
	b.taskChanges()
	b.implementerTasks()
	b.deferred()
	b.protected()
	return []byte(b.String())
}

func newBuilder(in Input) *builder {
	b := &builder{in: in, tasks: map[string]plan.Task{}}
	for _, t := range in.Plan.Tasks {
		b.tasks[t.ID] = t
	}
	b.records = reported(in.State, in.Events)
	// 実装のステップを記録したのに記録を持たないタスクは、ブランチを作らずに行ったタスク。
	for _, e := range in.Events {
		if e.Step == state.StepImplement && in.State.TaskRecord(e.Task) == nil && !slices.Contains(b.works, e.Task) {
			b.works = append(b.works, e.Task)
		}
	}
	return b
}

func (b *builder) f(format string, args ...any) {
	fmt.Fprintf(b, format, args...)
}

func (b *builder) overview() {
	in := b.in
	b.f("# 計画書の事実: %s\n\n## 概要\n\n", in.Name)
	b.f("- 計画書: %s\n", code(in.PlanURL))
	if len(in.Events) > 0 {
		b.f("- 計画書の開始: %s\n", earliest(in.Events).Format(timeLayout))
	}
	b.f("- 作業報告書: %s\n", reportLocation(in.State))

	b.f("\n### マージするPR\n\n")
	if len(b.records) == 0 {
		b.f("ブランチを作ったタスクはありません。\n\n")
		return
	}
	b.f("リポジトリごとに、PRにするブランチをpushしてベースへのPRを1つ作り、merge commitでマージしてください。\n\n")
	b.f("| リポジトリ | PRにするブランチ | ベース | 含むタスク |\n| --- | --- | --- | --- |\n")
	split := false
	for _, c := range chains(b.records) {
		head := code(c.Head.Branch)
		if slices.Contains(b.in.Changes[c.Head.ID].Missing, c.Head.Branch) {
			head += " (ローカルに残っていません)"
		}
		b.f("| %s | %s | %s | %s |\n", code(c.Head.Repo), head, code(c.Base), strings.Join(c.Tasks, "・"))
		split = split || c.Split
	}
	if split {
		b.f("\n同じリポジトリに複数の行があるときは、ブランチが1本に繋がっていません (前のタスクのブランチが残っておらず、最初のベースから作り直したなど)。\n")
		b.f("上の行から順に、まだマージしていないものをマージしてください。\n")
	}
	b.f("\n")
}

// Chainは、1つのPRにまとめてマージする、元のブランチで1本に繋がったタスクのブランチの並び。
type Chain struct {
	// HeadはPRにするブランチを持つ、並びの最後のタスクの記録。
	Head state.TaskRecord
	// Baseは並びの最初のタスクの元のブランチで、PRのベースにする。
	Base  string
	Tasks []string
	// Splitは、同じリポジトリの前の並びと繋がっていない並びかどうか。
	Split bool
}

// Chainsは、載せるタスクを、「マージするPR」と同じ並びにまとめる。
// 作業報告書のステップの指示に、リポジトリごとの最後のタスクのブランチを書くのに使う。
func Chains(st *state.State, events []state.Event) []Chain {
	return chains(reported(st, events))
}

// reportedは、載せるタスク (ブランチを準備し、実装を終えたもの) の記録を返す。
func reported(st *state.State, events []state.Event) []state.TaskRecord {
	implemented := map[string]bool{}
	for _, e := range events {
		if e.Step == state.StepImplement {
			implemented[e.Task] = true
		}
	}
	var records []state.TaskRecord
	for _, rec := range st.Tasks {
		if rec.Prepared && implemented[rec.ID] {
			records = append(records, rec)
		}
	}
	return records
}

// chainsは、タスクの記録recordsを、リポジトリごとに元のブランチで繋がった並びにまとめる。
// リポジトリは最初にタスクを進めた順、並びはリポジトリの中でタスクを進めた順に並べる。
func chains(records []state.TaskRecord) []Chain {
	var repos []string
	for _, rec := range records {
		if !slices.Contains(repos, rec.Repo) {
			repos = append(repos, rec.Repo)
		}
	}
	var result []Chain
	for _, repo := range repos {
		first := len(result)
		for _, rec := range records {
			if rec.Repo != repo {
				continue
			}
			if last := len(result) - 1; last >= first && result[last].Head.Branch == rec.Base {
				result[last].Head = rec
				result[last].Tasks = append(result[last].Tasks, rec.ID)
				continue
			}
			result = append(result, Chain{Head: rec, Base: rec.Base, Tasks: []string{rec.ID}, Split: len(result) > first})
		}
	}
	return result
}

// reportLocationは、最新の回の作業報告書を概要の1行に入れる形で表す。
// URLはそのまま開けるようにし、処理の流れを書かなかった回はその理由を添える。
func reportLocation(st *state.State) string {
	n := len(st.Reports)
	if n == 0 {
		return "まだ書いていません"
	}
	r := st.Reports[n-1]
	if r.Location == "" {
		return fmt.Sprintf("%d回目 (Markdownの作業報告書で、ページはありません)", r.Number)
	}
	s := fmt.Sprintf("%d回目 %s", r.Number, cell(r.Location))
	if r.WalkthroughSkipped != "" {
		s += " (処理の流れは書いていません: " + cell(r.WalkthroughSkipped) + ")"
	}
	return s
}

func (b *builder) taskChanges() {
	b.f("## タスクごとの変更\n\n")
	if len(b.records) == 0 {
		b.f("なし\n\n")
		return
	}
	b.f("変更行数は、元のブランチからタスクのブランチまでの追加と削除の合計です。\n\n")
	b.f("| タスク | ブランチ | 変更行数 | 想定行数 | レビュー |\n| --- | --- | --- | --- | --- |\n")
	for _, rec := range b.records {
		t := b.tasks[rec.ID]
		label := taskLabel(rec.ID, t)
		b.f("| %s | %s (元: %s) | %s | %s | %s |\n",
			cell(label), code(rec.Branch), code(rec.Base), changedLines(b.in.Changes[rec.ID]), cell(expected(t.Lines)), reviewVerdicts(rec.Reviews))
	}
	b.f("\n")
}

// changedLinesは、タスクのブランチの変更行数を表のセルに入れる形で表す。
// 変更を読めなかったときは、その理由を書く。
func changedLines(ch Changes) string {
	switch {
	case len(ch.Missing) > 0:
		names := make([]string, len(ch.Missing))
		for i, name := range ch.Missing {
			names[i] = code(name)
		}
		return fmt.Sprintf("ブランチ %s がローカルに残っていないため示せません", strings.Join(names, "・"))
	case ch.Err != "":
		return fmt.Sprintf("読めません (%s)", cell(ch.Err))
	}
	impl, test := lineCounts(ch.Files)
	return fmt.Sprintf("テスト以外%d行・テスト%d行", impl, test)
}

// reviewVerdictsは、タスクのレビューの評価を回の順に矢印でつなぐ。
func reviewVerdicts(reviews []state.ReviewRecord) string {
	if len(reviews) == 0 {
		return "なし"
	}
	verdicts := make([]string, len(reviews))
	for i, rv := range reviews {
		verdicts[i] = cell(rv.Verdict)
	}
	return strings.Join(verdicts, " → ")
}

// implementerTasksは、実装者がブランチを作らずに行ったタスクを並べる。ブランチの差分が
// 無いため変更の欄を持たず、結果は人が確かめて計画書にチェックを付けている。
func (b *builder) implementerTasks() {
	b.f("## 実装者が行ったタスク\n\n")
	if len(b.works) == 0 {
		b.f("なし\n\n")
		return
	}
	b.f("ブランチを作らずに実装者が行い、人が結果を確かめたタスクです。\n\n")
	for _, id := range b.works {
		b.f("- %s\n", cell(taskLabel(id, b.tasks[id])))
	}
	b.f("\n")
}

func (b *builder) deferred() {
	b.f("## 先送りした指摘\n\n")
	var rows []string
	for _, rec := range b.records {
		for _, d := range rec.Deferred {
			rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s |\n", rec.ID, cell(d.Title), cell(d.Choice), code(d.Doc)))
		}
	}
	if len(rows) == 0 {
		b.f("なし\n\n")
		return
	}
	b.f("コードを変更しない方針を選んだ指摘です。\n後続のタスクにするかは人が決めます。\n\n")
	b.f("| タスク | 指摘 | 選んだ対応方針 | レビュードキュメント |\n| --- | --- | --- | --- |\n%s\n", strings.Join(rows, ""))
}

func (b *builder) protected() {
	b.f("## 保護領域\n\n")
	n := 0
	for _, rec := range b.records {
		decls := b.tasks[rec.ID].Protected
		if len(decls) == 0 {
			continue
		}
		if n == 0 {
			b.f("保護領域を宣言したタスクです。\n差分が、宣言の内容と計画書の「%s」節に沿っているかを確かめてください。\n\n", constraintsSection)
		}
		n++
		b.f("### %s\n\n", cell(taskLabel(rec.ID, b.tasks[rec.ID])))
		for _, d := range decls {
			b.f("- 宣言: %s\n", cell(declLabel(d)))
		}
		b.f("- 差分を見るコマンド: %s\n\n", code(diffCommand(rec)))
	}
	if n == 0 {
		b.f("保護領域を宣言したタスクはありません。\n\n")
	}
}

// taskLabelはタスクを1行で表す。計画書から消えたタスクは番号だけにする。
func taskLabel(id string, t plan.Task) string {
	if t.Title == "" {
		return id
	}
	label := id + " " + t.Title
	if t.Platform != "" {
		label = fmt.Sprintf("%s [%s] %s", id, t.Platform, t.Title)
	}
	if t.Assignee != "" {
		label += fmt.Sprintf(" (担当: %s)", t.Assignee)
	}
	return label
}

func declLabel(d plan.Declaration) string {
	if d.Note == "" {
		return d.Name
	}
	return fmt.Sprintf("%s (%s)", d.Name, d.Note)
}

func diffCommand(rec state.TaskRecord) string {
	return fmt.Sprintf("git -C %s diff %s...%s", rec.Repo, rec.Base, rec.Branch)
}

// lineCountsは変更ファイルの行数 (追加と削除の合計) を、テスト以外とテストに分けて数える。
func lineCounts(files []branch.FileChange) (impl, test int) {
	for _, fc := range files {
		if isTest(fc.Path) {
			test += fc.Added + fc.Removed
		} else {
			impl += fc.Added + fc.Removed
		}
	}
	return impl, test
}

// isTestは、ファイルがテストかどうかを、KorylusのプロジェクトのGo・Ruby・TypeScriptの
// テストの名前の付け方で見分ける。生成物などは見分けず、テスト以外に数える。
func isTest(p string) bool {
	base := path.Base(p)
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_spec.rb") || strings.HasSuffix(base, "_test.rb") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	return slices.ContainsFunc(strings.Split(path.Dir(p), "/"), func(dir string) bool {
		return dir == "testdata" || dir == "spec" || dir == "test"
	})
}

// expectedは計画書の想定行数を表す。内訳はpartOrderの順に、ほかは名前の順に並べる。
func expected(s plan.Size) string {
	if s.Total == 0 {
		return "記載なし"
	}
	rank := func(name string) int {
		if i := slices.Index(partOrder, name); i >= 0 {
			return i
		}
		return len(partOrder)
	}
	names := slices.SortedFunc(maps.Keys(s.Parts), func(a, b string) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
	})
	if len(names) == 0 {
		return fmt.Sprintf("約%d行", s.Total)
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s%d行", name, s.Parts[name]))
	}
	return fmt.Sprintf("約%d行 (%s)", s.Total, strings.Join(parts, " + "))
}

// earliestは記録のうち最も早い開始の時刻を返す。
func earliest(events []state.Event) time.Time {
	t := events[0].Started
	for _, e := range events[1:] {
		if e.Started.Before(t) {
			t = e.Started
		}
	}
	return t
}

// cellは、表のセルやリストの1行に入れる文字列から、改行と表の区切りを除く。
func cell(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "|", `\|`).Replace(s)
}

// codeはsをMarkdownのインラインコードにする。sのバッククォートより長い区切りで囲む。
func code(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if len(fence) > 1 {
		return fence + " " + cell(s) + " " + fence
	}
	return fence + cell(s) + fence
}
