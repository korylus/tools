// Package reportは、計画書の最後まで進んだときに作る作業報告書をMarkdownで組み立てる。
//
// 作業報告書には、状態・所要時間の記録・計画書・gitから集めた事実だけを並べ、
// 文章をLLMに新たに書かせない。事実と異なる要約が混ざって、人の確認を誤らせないためである。
// 人は動作確認チェックリストの項目ごとにOK / NGとコメントを書き込み、次の版は
// その結果 (ParseResults) をもとに項目を並べ替える。
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

// stepsは所要時間の節でステップを並べる順序。
var steps = []state.Step{state.StepImplement, state.StepReview, state.StepFix, state.StepAsk}

// partOrderは想定行数の内訳を並べる順序。
var partOrder = []string{"実装", "テスト"}

// Inputは作業報告書1版を組み立てる入力。
type Input struct {
	// PlanPathは計画書の、プロジェクトのルートからの相対パス。
	PlanPath string
	// PlanSrcは計画書の内容、Planはそれを読み取ったもの。
	PlanSrc string
	Plan    *plan.Plan
	Version int
	Now     time.Time
	State   *state.State
	// Eventsは所要時間の記録で、Since番目以降がこの版の期間の記録。
	Events []state.Event
	Since  int
	// Changesはタスク番号ごとの、元のブランチからタスクのブランチまでの変更。
	Changes map[string]Changes
	// Previousは前の版に書き込まれた動作確認の結果。第1版ではnil。
	Previous *Results
	// Currentは、同じ版を作り直すときに、その版に書き込まれていた結果。
	// 人の書き込みを消さないよう引き継ぐ。新しい版ではnil。
	Current *Results
}

// Changesはタスク1件のブランチの変更。
type Changes struct {
	Commits []branch.Commit
	Files   []branch.FileChange
	// Errは変更を読めなかった理由。読めたら空文字。
	Err string
}

// builderは作業報告書1版を書き出す作業領域。
type builder struct {
	strings.Builder
	in    Input
	tasks map[string]plan.Task
	// recordsは作業報告書に載せるタスク (ブランチを準備し、実装を終えたもの) の記録。
	records []state.TaskRecord
	// addedは、この版の期間に記録したステップがあるタスク。
	added map[string]bool
}

// Buildは作業報告書1版をMarkdownで組み立てる。
func Build(in Input) []byte {
	b := newBuilder(in)
	b.overview()
	b.taskChanges()
	b.deferred()
	b.waits()
	b.timing()
	b.protected()
	b.checklist()
	return []byte(b.String())
}

func newBuilder(in Input) *builder {
	b := &builder{in: in, tasks: map[string]plan.Task{}, added: map[string]bool{}}
	for _, t := range in.Plan.Tasks {
		b.tasks[t.ID] = t
	}
	implemented := map[string]bool{}
	for i, e := range in.Events {
		if e.Step == state.StepImplement {
			implemented[e.Task] = true
		}
		if i >= in.Since {
			b.added[e.Task] = true
		}
	}
	for _, rec := range in.State.Tasks {
		if rec.Prepared && implemented[rec.ID] {
			b.records = append(b.records, rec)
		}
	}
	return b
}

func (b *builder) f(format string, args ...any) {
	fmt.Fprintf(b, format, args...)
}

func (b *builder) overview() {
	in := b.in
	b.f("# 作業報告書 第%d版: %s\n\n## 概要\n\n", in.Version, plan.Name(in.PlanPath))
	b.f("- 計画書: %s\n- 作成: %s\n", code(in.PlanPath), in.Now.Format(timeLayout))
	if len(in.Events) > 0 {
		b.f("- 計画書の開始: %s\n", earliest(in.Events).Format(timeLayout))
	}
	if in.Since < len(in.Events) {
		b.f("- この版の期間: %s 〜 %s\n", earliest(in.Events[in.Since:]).Format(timeLayout), in.Now.Format(timeLayout))
	}
	if p := in.Previous; p != nil {
		b.f("- 前の版: 第%d版\n", in.Version-1)
		if p.Comment != "" {
			b.f("\n前の版の全体へのコメント:\n\n%s\n", quote(p.Comment))
		}
	}

	b.f("\n### ブランチの順序\n\n")
	if len(b.records) == 0 {
		b.f("ブランチを作ったタスクはありません。\n\n")
		return
	}
	b.f("pushとPRの作成は、リポジトリごとにこの順で、各ブランチの元のブランチをベースにして行ってください。\n\n")
	b.f("| リポジトリ | タスク | ブランチ | 元のブランチ |\n| --- | --- | --- | --- |\n")
	var repos []string
	for _, rec := range b.records {
		if !slices.Contains(repos, rec.Repo) {
			repos = append(repos, rec.Repo)
		}
	}
	for _, repo := range repos {
		for _, rec := range b.records {
			if rec.Repo == repo {
				b.f("| %s | %s | %s | %s |\n", code(rec.Repo), rec.ID, code(rec.Branch), code(rec.Base))
			}
		}
	}
	b.f("\n")
}

func (b *builder) taskChanges() {
	b.f("## タスクごとの変更\n\n")
	if len(b.records) == 0 {
		b.f("なし\n\n")
		return
	}
	topics := plan.DecisionTopics(b.in.PlanSrc)
	for _, rec := range b.records {
		t := b.tasks[rec.ID]
		b.f("### %s\n\n", cell(taskLabel(rec.ID, t)))
		if b.in.Version > 1 && b.added[rec.ID] {
			b.f("この版で進めたタスクです。\n\n")
		}
		b.f("- リポジトリ: %s (作業するディレクトリ: %s)\n", code(rec.Repo), code(rec.Dir))
		b.f("- ブランチ: %s (元: %s)\n", code(rec.Branch), code(rec.Base))
		ch := b.in.Changes[rec.ID]
		if ch.Err != "" {
			b.f("- 変更: 読めません (%s)\n", cell(ch.Err))
		} else {
			impl, test := lineCounts(ch.Files)
			b.f("- 変更行数: テスト以外%d行、テスト%d行 (追加と削除の合計)\n", impl, test)
		}
		b.f("- 想定行数: %s\n", expected(t.Lines))

		if len(ch.Commits) > 0 {
			b.f("\nコミット:\n\n")
			for _, c := range ch.Commits {
				b.f("- %s %s\n", code(c.Hash), cell(c.Subject))
			}
		}
		if len(ch.Files) > 0 {
			b.f("\n変更ファイル:\n\n| ファイル | 追加 | 削除 |\n| --- | --: | --: |\n")
			for _, fc := range ch.Files {
				if fc.Binary {
					b.f("| %s | - | - |\n", code(fc.Path))
				} else {
					b.f("| %s | %d | %d |\n", code(fc.Path), fc.Added, fc.Removed)
				}
			}
		}
		if len(rec.Reviews) > 0 {
			b.f("\nレビュー:\n\n| 回 | 役割 | 評価 | レビュードキュメント |\n| --: | --- | --- | --- |\n")
			for _, rv := range rec.Reviews {
				b.f("| %d | %s | %s | %s |\n", rv.Number, rv.Role.Label(), rv.Verdict, code(rv.Doc))
			}
		}
		if ts := topics[rec.ID]; len(ts) > 0 {
			b.f("\n実装判断ログの論点:\n\n")
			for _, topic := range ts {
				b.f("- %s\n", cell(topic))
			}
		}
		b.f("\n")
	}
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

func (b *builder) waits() {
	b.f("## 人を待った履歴\n\n")
	var rows []string
	for _, e := range b.in.Events {
		if e.Step != state.StepAsk {
			continue
		}
		reason := cmp.Or(cell(e.Reason), "(記録なし)")
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s |\n", e.Task, e.Role.Label(), reason, e.Started.Format(timeLayout), formatDuration(e.Duration())))
	}
	if len(rows) == 0 {
		b.f("なし\n\n")
		return
	}
	b.f("| タスク | 尋ねた役割 | 理由 | 開始 | 待った時間 |\n| --- | --- | --- | --- | --: |\n%s\n", strings.Join(rows, ""))
}

func (b *builder) timing() {
	b.f("## 所要時間\n\n")
	if len(b.in.Events) == 0 {
		b.f("記録なし\n\n")
		return
	}
	type key struct {
		role state.Role
		step state.Step
	}
	type sum struct {
		count          int
		version, total time.Duration
	}
	sums := map[key]*sum{}
	perTask := map[string]map[state.Step]time.Duration{}
	var order []string
	var versionTotal, total time.Duration
	for i, e := range b.in.Events {
		d := max(e.Duration(), 0)
		k := key{e.Role, e.Step}
		if sums[k] == nil {
			sums[k] = &sum{}
		}
		s := sums[k]
		s.total += d
		total += d
		if i >= b.in.Since {
			s.count++
			s.version += d
			versionTotal += d
		}
		if perTask[e.Task] == nil {
			perTask[e.Task] = map[state.Step]time.Duration{}
			order = append(order, e.Task)
		}
		perTask[e.Task][e.Step] += d
	}

	b.f("- この版の合計: %s\n- 版をまたいだ合計: %s\n\n", formatDuration(versionTotal), formatDuration(total))
	b.f("### 役割・ステップごとの合計\n\n| 役割 | ステップ | 回数 (この版) | この版 | 版をまたいだ合計 |\n| --- | --- | --: | --: | --: |\n")
	for _, r := range state.Roles {
		for _, st := range steps {
			if s := sums[key{r, st}]; s != nil {
				b.f("| %s | %s | %d | %s | %s |\n", r.Label(), st.Label(), s.count, formatDuration(s.version), formatDuration(s.total))
			}
		}
	}
	b.f("\n### タスクごとの内訳\n\n人を待った時間は、人への確認のステップの時間です。\n\n")
	b.f("| タスク | 実装 | レビュー | 修正 | 人を待った時間 | 合計 |\n| --- | --: | --: | --: | --: | --: |\n")
	for _, id := range order {
		var sum time.Duration
		cols := make([]string, 0, len(steps))
		for _, st := range steps {
			sum += perTask[id][st]
			cols = append(cols, formatDuration(perTask[id][st]))
		}
		b.f("| %s | %s | %s |\n", id, strings.Join(cols, " | "), formatDuration(sum))
	}
	b.f("\n")
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
	switch {
	case t.Title == "":
		return id
	case t.Platform == "":
		return id + " " + t.Title
	}
	return fmt.Sprintf("%s [%s] %s", id, t.Platform, t.Title)
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

// formatDurationは所要時間を分単位に丸めて表す。
func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h > 0 {
		return fmt.Sprintf("%d時間%d分", h, m)
	}
	return fmt.Sprintf("%d分", m)
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

// quoteは複数行の文字列をMarkdownの引用にする。
func quote(s string) string {
	return "> " + strings.ReplaceAll(s, "\n", "\n> ")
}
