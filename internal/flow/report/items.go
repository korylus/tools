package report

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/korylus/tools/internal/plan"
)

// 確認項目の種類。screenとprocessは、実装の完了で受け取った動作確認の項目の値。
const (
	kindScreen    = "screen"
	kindProcess   = "process"
	kindProtected = "protected"
)

var (
	kindLabels   = map[string]string{kindScreen: "画面", kindProcess: "処理", kindProtected: "保護領域"}
	targetLabels = map[string]string{kindScreen: "パス", kindProcess: "コマンド", kindProtected: "差分を見るコマンド"}
	statusLabels = map[Status]string{"": "未確認", StatusOK: "OK", StatusNG: "NG"}
)

// itemは動作確認チェックリストの項目1件。
type item struct {
	// idは版をまたいで項目を対応付ける識別子。作業報告書にはHTMLコメントで埋め込む。
	id       string
	task     string
	kind     string
	title    string
	target   string
	steps    []string
	expected string
	// prevは前の版での結果。前の版のチェックリストに無ければnil。
	prev *Result
	// confirmedは、前の版までにOKになり、確かめ直さなくてよい項目かどうか。
	confirmed bool
	number    int
}

func (b *builder) checklist() {
	items := b.items()
	b.f("## 動作確認チェックリスト\n\n")
	if len(items) == 0 {
		b.f("確認する項目はありません。\n\n")
	} else {
		b.f("項目ごとにOKかNGのどちらかにチェックを付け、気づいたことを「%s」の下に書いてください。\n", commentLabel)
		b.f("NGの項目とコメントは、%s でフィードバック対応のフェーズにします。\n\n", code("/korylus-add-feedback-phase"))
	}
	var confirmed []item
	for _, it := range items {
		if it.confirmed {
			confirmed = append(confirmed, it)
			continue
		}
		b.writeItem(it)
	}
	if len(confirmed) > 0 {
		b.f("%s\n\n%s\n\n", confirmedTitle, confirmedIntro)
		for _, it := range confirmed {
			b.f("- #%d %s (対象タスク: %s) <!-- confirmed: %s -->\n", it.number, cell(it.title), it.task, it.id)
		}
		b.f("\n")
	}
	b.f("%s\n\n%s\n", overallTitle, overallGuide)
	if c := b.current().Comment; c != "" {
		b.f("\n%s\n", c)
	}
}

// itemsは、各タスクの動作確認の項目と保護領域の宣言ごとの項目を集め、前の版の結果をもとに
// 前の版でNGだった項目・新しい項目・前の版で未確認だった項目・確認済みの項目の順に並べて番号を振る。
func (b *builder) items() []item {
	var items []item
	for _, rec := range b.records {
		for j, v := range rec.Verification {
			items = append(items, item{
				id: fmt.Sprintf("task/%s/%d", rec.ID, j+1), task: rec.ID,
				kind: v.Kind, title: v.Title, target: v.Target, steps: v.Steps, expected: v.Expected,
			})
		}
		for _, d := range b.tasks[rec.ID].Protected {
			items = append(items, item{
				id: fmt.Sprintf("protected/%s/%s", rec.ID, d.Name), task: rec.ID, kind: kindProtected,
				title:  fmt.Sprintf("保護領域「%s」の変更が合意した設計どおりになっている", d.Name),
				target: diffCommand(rec),
				steps: []string{
					"この作業報告書の「保護領域」の節で、対象タスクの宣言を読む",
					fmt.Sprintf("計画書の「%s」節で、%sの方針を読む", constraintsSection, d.Name),
					"差分を見るコマンドで、タスクの変更を確かめる",
				},
				expected: fmt.Sprintf("宣言と「%s」節の方針から外れた変更が無い", constraintsSection),
			})
		}
	}

	if p := b.in.Previous; p != nil {
		for i := range items {
			it := &items[i]
			if r, ok := p.Items[it.id]; ok {
				it.prev = &r
			}
			it.confirmed = p.Confirmed[it.id] || (it.prev != nil && it.prev.Status == StatusOK)
		}
	}
	rank := func(it item) int {
		switch {
		case it.confirmed:
			return 3
		case it.prev == nil:
			return 1
		case it.prev.Status == StatusNG:
			return 0
		}
		return 2
	}
	slices.SortStableFunc(items, func(a, b item) int { return cmp.Compare(rank(a), rank(b)) })
	for i := range items {
		items[i].number = i + 1
	}
	return items
}

// Absorbedは、同じ版を作り直す入力inについて、作業報告書が生成した区切りが人の書き込みに
// 隠され、項目や全体へのコメントの節が前のコメントに取り込まれている箇所を返す。
// 閉じていないコードブロックや消された項目のIDがあると、作り直すたびに、取り込まれた節に
// 生成した節が重なって増えていくためである。
func Absorbed(in Input) []string {
	cur := in.Current
	if cur == nil {
		return nil
	}
	var lines []string
	for _, r := range cur.Items {
		lines = append(lines, strings.Split(r.Comment, "\n")...)
	}
	lines = append(lines, strings.Split(cur.Comment, "\n")...)
	written := func(s string) bool {
		return slices.ContainsFunc(lines, func(l string) bool { return strings.TrimSpace(l) == s })
	}

	var found []string
	for _, it := range newBuilder(in).items() {
		if _, ok := cur.Items[it.id]; ok || it.confirmed {
			continue
		}
		if written(itemHeading(it)) || written(itemMarker(it)) {
			found = append(found, fmt.Sprintf("項目 #%d「%s」が、前の項目のコメントに取り込まれています", it.number, cell(it.title)))
		}
	}
	if !cur.overall && written(overallGuide) {
		found = append(found, fmt.Sprintf("「%s」の節が、項目のコメントに取り込まれています", strings.TrimPrefix(overallTitle, "## ")))
	}
	return found
}

func itemHeading(it item) string {
	return fmt.Sprintf("### #%d %s", it.number, cell(it.title))
}

func itemMarker(it item) string {
	return fmt.Sprintf("<!-- item: %s -->", it.id)
}

func (b *builder) writeItem(it item) {
	b.f("%s\n\n%s\n\n", itemHeading(it), itemMarker(it))
	b.f("- 対象タスク: %s\n- 種類: %s\n", it.task, cmp.Or(kindLabels[it.kind], it.kind))
	if it.target != "" {
		b.f("- %s: %s\n", cmp.Or(targetLabels[it.kind], "対象"), code(it.target))
	}
	b.f("- 期待する結果: %s\n", cell(it.expected))
	if p := it.prev; p != nil {
		ref := plan.ItemRef{Version: b.in.Version - 1, Number: p.Number}
		b.f("- 前の版の結果: 第%d版 #%d %s\n", ref.Version, ref.Number, statusLabels[p.Status])
		if fb := b.feedbackFor(ref); len(fb) > 0 {
			b.f("- 対応したフィードバックのタスク: %s\n", strings.Join(fb, ", "))
		}
		if p.Comment != "" {
			b.f("\n前の版のコメント:\n\n%s\n", quote(p.Comment))
		}
	}
	b.f("\n手順:\n\n")
	for i, s := range it.steps {
		b.f("%d. %s\n", i+1, cell(s))
	}
	cur := b.current().Items[it.id]
	b.f("\n結果:\n\n- [%s] OK\n- [%s] NG\n\n%s\n\n", check(cur.Status == StatusOK), check(cur.Status == StatusNG), commentLabel)
	if cur.Comment != "" {
		b.f("%s\n\n", cur.Comment)
	}
}

// currentは、作り直す版に書き込まれていた結果を返す。新しい版では空の結果を返す。
func (b *builder) current() *Results {
	if b.in.Current != nil {
		return b.in.Current
	}
	return &Results{}
}

// feedbackForは、前の版の確認項目refに対応する計画書のタスクを返す。
func (b *builder) feedbackFor(ref plan.ItemRef) []string {
	var ids []string
	for _, t := range b.in.Plan.Tasks {
		if slices.Contains(t.Verifies, ref) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

func check(on bool) string {
	if on {
		return "x"
	}
	return " "
}
