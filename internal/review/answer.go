package review

import (
	"bytes"
	"fmt"
	"slices"
)

// Answersは対応方針への回答の結果。
type Answers struct {
	// Decisionsは回答した指摘ごとの、選ばれた選択肢。
	Decisions []Decision
	// Pendingは人間の判断を待つ指摘 (要人間判断と再検討提案のうち、まだ選択肢が選ばれていないもの)。
	Pending []Finding
	// Invalidは、推奨どおりに回答できない指摘の理由。
	Invalid []string
	// Outputは、推奨の選択肢にチェックを入れたレビュードキュメントの内容。
	Output []byte
}

// Decisionは指摘1件への回答。
type Decision struct {
	Finding Finding
	// Chosenは選ばれた選択肢。
	Chosen []Option
	// Autoは、Answerが推奨の選択肢を選んだかどうか。falseなら、
	// 回答の前から選ばれていた (人間が選んだ) 選択肢を使う。
	Auto bool
}

// Defersは、選ばれた選択肢がどれもコードを変えないものかを返す。
func (d Decision) Defers() bool {
	for _, o := range d.Chosen {
		if !o.Defers() {
			return false
		}
	}
	return true
}

// Blockedは、人間の判断を待つ指摘か、回答できない指摘があるかを返す。
func (a *Answers) Blocked() bool {
	return len(a.Pending) > 0 || len(a.Invalid) > 0
}

// NeedsFixは、修正が要る回答があるかを返す。
func (a *Answers) NeedsFix() bool {
	for _, d := range a.Decisions {
		if !d.Defers() {
			return true
		}
	}
	return false
}

// Answerは各指摘の対応方針に回答する。
//
//   - すでに選択肢が選ばれている指摘は、その選択肢を回答とする (人間が止まった指摘に回答して再開したとき)
//   - 要人間判断の指摘と再検討提案は、人間の判断を待つ
//   - 自動対応可の指摘は、「(推奨)」の付いた選択肢にチェックを入れる
//
// 区分や推奨が読み取れない指摘は、推測で回答せずにInvalidに理由を残す。
func (d *Document) Answer() *Answers {
	a := &Answers{Output: bytes.Clone(d.src)}
	for _, f := range d.Findings {
		var checked, recommended []Option
		for _, o := range f.Options {
			if o.Checked {
				checked = append(checked, o)
			}
			if o.Recommended {
				recommended = append(recommended, o)
			}
		}
		switch {
		case len(f.Options) == 0:
			a.Invalid = append(a.Invalid, f.describe("対応方針にチェックボックスの選択肢がありません"))
		case len(checked) > 0:
			a.Decisions = append(a.Decisions, Decision{Finding: f, Chosen: checked})
		case f.Proposal || f.Category == NeedsHuman:
			a.Pending = append(a.Pending, f)
		case f.Category != AutoFix:
			a.Invalid = append(a.Invalid, f.describe(fmt.Sprintf("区分 (%s / %s) を読み取れません", AutoFix, NeedsHuman)))
		case len(recommended) != 1:
			a.Invalid = append(a.Invalid, f.describe(fmt.Sprintf("「%s」の付いた選択肢が%d個あります (1個を期待)", recommendedMark, len(recommended))))
		default:
			o := recommended[0]
			a.Output[o.mark] = 'x'
			o.Checked = true
			a.Decisions = append(a.Decisions, Decision{Finding: f, Chosen: []Option{o}, Auto: true})
		}
	}
	return a
}

// Checkedは、選択肢にチェックが入っている指摘を返す。レビュアーが書いた直後の
// ドキュメントでは、チェックはレビュアーが入れたものになる。
func (d *Document) Checked() []Finding {
	var res []Finding
	for _, f := range d.Findings {
		if slices.ContainsFunc(f.Options, func(o Option) bool { return o.Checked }) {
			res = append(res, f)
		}
	}
	return res
}

// describeは指摘の場所と見出しを添えたメッセージを返す。
func (f Finding) describe(msg string) string {
	return fmt.Sprintf("%d行目「%s」: %s", f.LineNo, f.Title, msg)
}

// Describeは、人間の判断を待つ指摘を停止理由の詳細に載せる形にする。
func (f Finding) Describe() string {
	kind := string(f.Category)
	if f.Proposal {
		kind = "再検討提案"
	}
	return f.describe(kind)
}
