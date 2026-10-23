package report

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/korylus/tools/internal/pilot/agent"
	"github.com/korylus/tools/internal/pilot/config"
	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/hub"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
	"github.com/korylus/tools/internal/plan"
)

// constraintsSectionは、保護領域の合意した設計として載せる計画書の節の名前。
const constraintsSection = "合意済みの設計制約"

// eventKindsは所要時間の節で種類を並べる順序。
var eventKinds = []state.EventKind{state.EventAgent, state.EventGate, state.EventUsageWait, state.EventHumanWait}

// Inputは作業報告書を組み立てる入力。
type Input struct {
	// PlanPathは計画書の、プロジェクトのルートからの相対パス。
	PlanPath string
	Version  int
	Now      time.Time
	State    *state.State
	// Planは計画書の内容 (最後のタスクを終えたときのスナップショット)。
	Plan       string
	Config     *config.Config
	Events     []state.Event
	StepUsages []usage.StepUsage
	// Checkoutは人間のチェックアウトへの取り込みの結果。
	Checkout hub.Checkout
	// Diffsは、State.Tasksと同じ順に並べた、各タスクの元のブランチからタスクのブランチまでの差分。
	Diffs []TaskDiff
	// Finalは、最初のタスクの元のブランチから最後のタスクのブランチまでの差分の検査の結果。
	// 宣言はすべてのタスクの宣言を合わせたものとして検査する。
	Final *gate.Report
	// PreviousとPreviousResultは前の版の作業報告書と動作確認の結果。第1版か、
	// まだ保存されていなければnil。
	Previous       *Report
	PreviousResult *Verification
}

// TaskDiffはタスク1件の差分と、その差分の検査の結果。
type TaskDiff struct {
	Diff    *gate.Diff
	Inspect *gate.Report
}

// builderは作業報告書1版を組み立てる作業領域。
type builder struct {
	in        Input
	planTasks map[string]plan.Task
	// planOrderは計画書に書かれた順のタスク。
	planOrder []plan.Task
	// newFromは、この版で新しく終えた最初のタスクのState.Tasksでの添字。
	newFrom int
	// sinceはこの版の期間の始まり。第1版はゼロ値で、すべての記録をこの版に含める。
	since time.Time
}

// Buildは作業報告書1版を組み立てる。
func Build(in Input) (*Report, error) {
	p, err := plan.Parse(in.Plan)
	if err != nil {
		return nil, err
	}
	if len(in.State.Tasks) == 0 {
		return nil, fmt.Errorf("作業報告書に載せる、終えたタスクがありません")
	}
	if len(in.Diffs) != len(in.State.Tasks) {
		return nil, fmt.Errorf("タスクの差分の数 %d が、終えたタスクの数 %d と合いません", len(in.Diffs), len(in.State.Tasks))
	}
	b := builder{in: in, planTasks: map[string]plan.Task{}, planOrder: p.Tasks}
	for _, t := range p.Tasks {
		b.planTasks[t.ID] = t
	}
	if n := len(in.State.Reports); n > 0 {
		b.newFrom, b.since = in.State.Reports[n-1].Tasks, in.State.Reports[n-1].At
	}

	r := &Report{Plan: in.PlanPath, Name: plan.Name(in.PlanPath), Version: in.Version, CreatedAt: in.Now, Started: in.Now}
	for _, e := range in.Events {
		if e.Started.Before(r.Started) {
			r.Started = e.Started
		}
	}
	r.VersionStarted = r.Started
	if !b.since.IsZero() {
		r.VersionStarted = b.since
	}
	r.Usage, r.Costs = b.usage(r.VersionStarted)
	r.Branches = b.branches()
	r.Preparation = b.preparation(r.Branches)
	r.Tasks = b.tasks()
	for _, d := range in.State.Deferred {
		r.Deferred = append(r.Deferred, Deferred(d))
	}
	for _, s := range in.State.StopLog {
		r.Stops = append(r.Stops, Stop{Task: s.Task, Step: stepLabel(s.Step), Reason: s.Reason, Details: s.Details, Role: s.Role, At: s.At, ResumedAt: s.ResumedAt})
	}
	r.Timing = b.timing()
	r.Protected = b.protected()
	r.Items = b.items(r.Protected.Areas)
	if in.Previous != nil {
		r.Previous = &PrevReport{Version: in.Previous.Version}
		if in.PreviousResult != nil {
			r.Previous.Comment = in.PreviousResult.Comment
		}
	}
	return r, nil
}

// inVersionは、時刻tの記録がこの版の期間に入るかを返す。
func (b *builder) inVersion(t time.Time) bool {
	return b.since.IsZero() || !t.Before(b.since)
}

// usageは、この版の期間 (fromからNowまで) に記録したステップの使用率の消費と、
// エージェントの実行のターン数・コストを集める。使用率の記録は計画書をまたいで
// 追記されるため、第1版でも計画書の開始より前の記録を除く。
func (b *builder) usage(from time.Time) ([]UsageRow, []CostRow) {
	type key struct {
		tool   agent.Tool
		window usage.Window
	}
	sums := map[key]*UsageRow{}
	for _, s := range b.in.StepUsages {
		if s.At.Before(from) || s.At.After(b.in.Now) {
			continue
		}
		v, ok := s.Consumed()
		if !ok {
			continue
		}
		k := key{s.Tool, s.Window}
		if sums[k] == nil {
			sums[k] = &UsageRow{Tool: string(s.Tool), Window: string(s.Window)}
		}
		sums[k].Percent += v
		sums[k].Steps++
	}
	var rows []UsageRow
	for _, k := range slices.SortedFunc(maps.Keys(sums), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.tool, b.tool), cmp.Compare(slices.Index(usage.Windows, a.window), slices.Index(usage.Windows, b.window)))
	}) {
		rows = append(rows, *sums[k])
	}

	costs := map[string]*CostRow{}
	for _, e := range b.in.Events {
		if e.Kind != state.EventAgent || !b.inVersion(e.Started) {
			continue
		}
		if costs[e.Tool] == nil {
			costs[e.Tool] = &CostRow{Tool: e.Tool}
		}
		c := costs[e.Tool]
		c.Runs++
		c.Turns += e.Turns
		c.CostUSD += e.CostUSD
	}
	var costRows []CostRow
	for _, tool := range slices.Sorted(maps.Keys(costs)) {
		costRows = append(costRows, *costs[tool])
	}
	return rows, costRows
}

func (b *builder) branches() []Branch {
	branches := make([]Branch, 0, len(b.in.State.Tasks))
	for _, t := range b.in.State.Tasks {
		branches = append(branches, Branch{Task: t.ID, Branch: t.Branch, Start: t.Start, Commit: t.Commit})
	}
	return branches
}

func (b *builder) preparation(branches []Branch) Preparation {
	p := Preparation{Branch: branches[len(branches)-1].Branch, Switched: b.in.Checkout.Switched, Setup: b.in.Config.Report.Setup}
	for _, name := range b.in.Checkout.Skipped {
		if i := slices.IndexFunc(branches, func(br Branch) bool { return br.Branch == name }); i >= 0 {
			p.Skipped = append(p.Skipped, branches[i])
		}
	}
	return p
}

func (b *builder) tasks() []Task {
	topics := plan.DecisionTopics(b.in.Plan)
	tasks := make([]Task, 0, len(b.in.State.Tasks))
	for i, rec := range b.in.State.Tasks {
		pt := b.planTasks[rec.ID]
		t := Task{
			ID: rec.ID, Platform: pt.Platform, Title: pt.Title, NewInVersion: i >= b.newFrom,
			Branch: rec.Branch, Start: rec.Start, Commit: rec.Commit,
			ImplLines:         b.in.Diffs[i].Inspect.ImplLines,
			ExpectedImplLines: gate.ExpectedImplLines(pt.Lines),
			Decisions:         topics[rec.ID],
		}
		for _, f := range b.in.Diffs[i].Diff.Files {
			fc := FileChange{Path: f.Path, Status: string(rune(f.Status)), Kind: string(gate.Classify(b.in.Config.Gate, f)), Added: len(f.Added), Removed: len(f.Removed)}
			if f.OldPath != f.Path {
				fc.OldPath = f.OldPath
			}
			t.Files = append(t.Files, fc)
		}
		if o, ok := b.lastAccepted(rec.ID, state.StepImplement); ok {
			t.Summary = o.Result.Summary
		}
		for _, rv := range b.in.State.ReviewLog {
			if rv.Task == rec.ID {
				t.Reviews = append(t.Reviews, Review{Number: rv.Number, Role: rv.Role, Verdict: rv.Verdict, Doc: rv.Doc})
			}
		}
		tasks = append(tasks, t)
	}
	return tasks
}

// timingは、所要時間の記録を種類・役割ごとと、タスクごとに集める。
func (b *builder) timing() Timing {
	type key struct {
		kind state.EventKind
		role string
	}
	rows := map[key]*TimingRow{}
	perTask := map[string]*TaskTiming{}
	for _, rec := range b.in.State.Tasks {
		perTask[rec.ID] = &TaskTiming{Task: rec.ID}
	}

	var tm Timing
	for _, e := range b.in.Events {
		d := max(e.Duration(), 0)
		k := key{e.Kind, e.Role}
		if rows[k] == nil {
			rows[k] = &TimingRow{Kind: string(e.Kind), Role: e.Role}
		}
		rows[k].Total += d
		tm.Total += d
		if b.inVersion(e.Started) {
			rows[k].Count++
			rows[k].Version += d
			tm.VersionTotal += d
		}
		tt := perTask[e.Task]
		if tt == nil {
			continue
		}
		switch e.Kind {
		case state.EventAgent:
			tt.Agent += d
		case state.EventGate:
			tt.Gate += d
		case state.EventUsageWait:
			tt.UsageWait += d
		case state.EventHumanWait:
			tt.HumanWait += d
		}
	}

	for _, k := range slices.SortedFunc(maps.Keys(rows), func(a, b key) int {
		return cmp.Or(cmp.Compare(kindOrder(a.kind), kindOrder(b.kind)), cmp.Compare(a.role, b.role))
	}) {
		tm.Rows = append(tm.Rows, *rows[k])
	}
	for _, o := range b.in.State.Outputs {
		if tt := perTask[o.Task]; tt != nil && o.Step == state.StepRework && accepted(o) {
			tt.Reworks++
		}
	}
	for _, rv := range b.in.State.ReviewLog {
		if tt := perTask[rv.Task]; tt != nil {
			tt.Reviews++
		}
	}
	for _, rec := range b.in.State.Tasks {
		tm.Tasks = append(tm.Tasks, *perTask[rec.ID])
	}
	return tm
}

func kindOrder(k state.EventKind) int {
	if i := slices.Index(eventKinds, k); i >= 0 {
		return i
	}
	return len(eventKinds)
}

// protectedは保護領域の変更の節を組み立てる。差分が触れた領域と、ステップが変更を
// 申告した領域を、設定の順 (設定に無い名前は名前の順) に並べる。
func (b *builder) protected() Protected {
	var pr Protected
	pr.Constraints, _ = plan.Section(b.in.Plan, constraintsSection)

	areas := map[string]*Area{}
	areaTasks := map[string]map[string]*AreaTask{}
	ensure := func(name string) *Area {
		if a, ok := areas[name]; ok {
			return a
		}
		a := &Area{Name: name, Label: name, Item: protectedItemID(name)}
		if i := slices.IndexFunc(b.in.Config.Protected, func(c config.Protected) bool { return c.Name == name }); i >= 0 {
			a.Label = b.in.Config.Protected[i].Label
		}
		areas[name] = a
		areaTasks[name] = map[string]*AreaTask{}
		return a
	}
	taskOf := func(name, task string) *AreaTask {
		ensure(name)
		if at, ok := areaTasks[name][task]; ok {
			return at
		}
		at := &AreaTask{Task: task}
		areaTasks[name][task] = at
		return at
	}

	if b.in.Final != nil {
		for _, t := range b.in.Final.Touches {
			ensure(t.Name).Final = hits(t.Hits)
		}
	}
	for i, rec := range b.in.State.Tasks {
		for _, t := range b.in.Diffs[i].Inspect.Touches {
			at := taskOf(t.Name, rec.ID)
			at.Declared, at.Hits = t.Declared, hits(t.Hits)
			if !t.Declared {
				pr.Undeclared = append(pr.Undeclared, UndeclaredChange{Task: rec.ID, Area: t.Name, Label: t.Label, Hits: at.Hits})
			}
		}
		for _, o := range b.in.State.Outputs {
			if o.Task != rec.ID || !accepted(o) || !slices.Contains([]state.Step{state.StepImplement, state.StepRework, state.StepFix}, o.Step) {
				continue
			}
			for _, c := range o.Result.ProtectedChanges {
				at := taskOf(c.Area, rec.ID)
				at.Changes = append(at.Changes, Change{Step: stepLabel(o.Step), Role: o.Role, Summary: c.Summary, Reason: c.Reason})
			}
		}
		if o, ok := b.lastAccepted(rec.ID, state.StepReview); ok {
			for _, c := range o.Result.ProtectedChecks {
				at := taskOf(c.Area, rec.ID)
				at.Checks = append(at.Checks, Check{Role: o.Role, Point: c.Point, Result: c.Result, Detail: c.Detail})
			}
		}
	}

	for _, name := range orderAreas(b.in.Config.Protected, slices.Collect(maps.Keys(areas))) {
		a := areas[name]
		for _, rec := range b.in.State.Tasks {
			pt := b.planTasks[rec.ID]
			declared := false
			for _, d := range pt.Protected {
				if d.Name == name {
					declared = true
					a.Declarations = append(a.Declarations, Declaration{Task: rec.ID, Note: d.Note})
				}
			}
			if at, ok := areaTasks[name][rec.ID]; ok {
				at.Declared = at.Declared || declared
				a.Tasks = append(a.Tasks, *at)
			}
		}
		pr.Areas = append(pr.Areas, *a)
	}
	return pr
}

// orderAreasは保護領域の名前namesを、設定の順に、設定に無い名前は名前の順で後ろに並べる。
func orderAreas(cfg []config.Protected, names []string) []string {
	rank := func(name string) int {
		if i := slices.IndexFunc(cfg, func(c config.Protected) bool { return c.Name == name }); i >= 0 {
			return i
		}
		return len(cfg)
	}
	slices.SortFunc(names, func(a, b string) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
	})
	return names
}

func hits(src []gate.Hit) []Hit {
	res := make([]Hit, 0, len(src))
	for _, h := range src {
		hit := Hit{Path: h.Path, Line: h.Line, Removed: h.Removed, Text: h.Text, Func: h.Func}
		if h.Edge != nil {
			hit.Edge = h.Edge.String()
		}
		res = append(res, hit)
	}
	return res
}

// itemsは動作確認チェックリストを組み立てる。各タスクの実装のステップが返した確認項目と、
// 保護領域ごとの確認項目を並べ、前の版の結果をもとに確かめる項目を先に、
// 前の版までにOKになった項目を後ろに並べて番号を振る。
func (b *builder) items(areas []Area) []Item {
	var items []Item
	for _, rec := range b.in.State.Tasks {
		o, ok := b.lastAccepted(rec.ID, state.StepImplement)
		if !ok {
			continue
		}
		for j, v := range o.Result.VerificationItems {
			items = append(items, Item{ID: fmt.Sprintf("task/%s/%d", rec.ID, j+1), Tasks: []string{rec.ID}, Kind: v.Kind, Title: v.Title, Target: v.Target, Steps: v.Steps, Expected: v.Expected})
		}
	}

	// この版で新しく終えたタスクが触れた保護領域は、前の版でOKでも確かめ直す。
	changed := map[string]bool{}
	for _, a := range areas {
		var tasks []string
		for _, at := range a.Tasks {
			if len(at.Hits) == 0 && len(at.Changes) == 0 {
				continue
			}
			tasks = append(tasks, at.Task)
			if i := slices.IndexFunc(b.in.State.Tasks, func(t state.TaskRecord) bool { return t.ID == at.Task }); i >= b.newFrom {
				changed[a.Item] = true
			}
		}
		items = append(items, Item{
			ID:    a.Item,
			Tasks: tasks,
			Kind:  KindProtected,
			Title: fmt.Sprintf("保護領域 %s (%s) の変更が合意した設計どおりになっている", a.Name, a.Label),
			Steps: []string{
				"「保護領域の変更」の節で、この領域の合意した設計と最終的な差分を見比べる",
				"タスクごとの内訳で、変更の説明 (自己申告) とレビュアーの照合が差分と食い違っていないかを確かめる",
			},
			Expected: "合意した設計 (保護領域の宣言と「" + constraintsSection + "」節) から外れた変更が無い",
		})
	}
	return b.arrange(items, changed)
}

// arrangeは前の版の項目と結果を項目に結び付け、並べ替えて番号を振る。
func (b *builder) arrange(items []Item, changed map[string]bool) []Item {
	prev := map[string]Item{}
	if b.in.Previous != nil {
		for _, it := range b.in.Previous.Items {
			prev[it.ID] = it
		}
	}
	for i := range items {
		it := &items[i]
		pi, ok := prev[it.ID]
		if !ok {
			continue
		}
		var res ItemResult
		if b.in.PreviousResult != nil {
			res = b.in.PreviousResult.Items[it.ID]
		}
		it.Previous = &PrevItem{Number: pi.Number, Status: res.Status, Comment: res.Comment}
		it.Confirmed = !changed[it.ID] && (res.Status == StatusOK || (pi.Confirmed && res.Status != StatusNG))
		if !it.Confirmed {
			it.Feedback = b.feedbackFor(plan.ItemRef{Version: b.in.Previous.Version, Number: pi.Number})
		}
	}

	rank := func(it Item) int {
		switch {
		case it.Confirmed:
			return 2
		case it.Previous != nil && it.Previous.Status == StatusNG:
			return 0
		}
		return 1
	}
	slices.SortStableFunc(items, func(a, b Item) int { return cmp.Compare(rank(a), rank(b)) })
	for i := range items {
		items[i].Number = i + 1
	}
	return items
}

// feedbackForは、確認項目refに対応する計画書のフィードバックのタスクを返す。
func (b *builder) feedbackFor(ref plan.ItemRef) []string {
	var ids []string
	for _, t := range b.planOrder {
		if slices.Contains(t.Verifies, ref) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// lastAcceptedは、タスクtaskのステップstepの構造化出力のうち、採用した最後のものを返す。
func (b *builder) lastAccepted(task string, step state.Step) (state.Output, bool) {
	outs := b.in.State.Outputs
	for i := len(outs) - 1; i >= 0; i-- {
		if o := outs[i]; o.Task == task && o.Step == step && accepted(o) {
			return o, true
		}
	}
	return state.Output{}, false
}

// acceptedは、構造化出力を返した試行が成功し、監視で止められず、エージェントが作業を終えたかを返す。
// 失敗した試行やエスカレーションの出力は、そのあとに再開したステップの出力で置き換わる。
func accepted(o state.Output) bool {
	return o.Outcome == state.OutcomeOK && !o.Stopped && o.Result.Status == agent.StatusCompleted
}

func protectedItemID(area string) string {
	return "protected/" + area
}

func stepLabel(s state.Step) string {
	if s == "" {
		return ""
	}
	return s.Label()
}
