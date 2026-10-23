package plan

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// decisionLogTitleはタスクの対応で追記してよい節の見出し。
const decisionLogTitle = "## 実装判断ログ"

// reUncheckedはチェックの入っていないチェックボックスの行頭にマッチする。
// タスク行 (インデント無し) とサブタスクの行 (インデント有り) の両方に当たる。
var reUnchecked = regexp.MustCompile(`^(\s*- )\[ \]`)

// Violationは計画書のスナップショットからの、許されない変更1件。
type Violation struct {
	// LineNoは現在の計画書での行番号 (1始まり)。行を特定できなければ0。
	LineNo int
	// Messageは変更の内容。
	Message string
}

// section は計画書の `## ` の見出しで区切った1つの節。
type section struct {
	// titleは見出しの行。最初の見出しより前は空文字。
	title string
	// startは見出しの行の、ファイル全体での添字 (0始まり)。
	start int
	lines []string
}

// CheckTaskChangesは、計画書のスナップショットsnapshotから現在の内容currentへの
// 変更が、タスクtaskIDの対応で許される変更だけかを確かめ、許されない変更を返す。
//
// 許される変更は次の2つに限る。
//   - 「タスクリスト」節で、taskIDのタスク行とその下のサブタスクのチェックボックスに
//     チェックを入れる
//   - 「実装判断ログ」節への行の追加 (節が無ければ、節ごとの追加)
func CheckTaskChanges(snapshot, current, taskID string) []Violation {
	oldSecs, oldLog := splitSections(snapshot)
	newSecs, newLog := splitSections(current)

	vs := compareSections(oldSecs, newSecs, func(o, n section) []Violation {
		return checkTaskList(o, n, taskID)
	})
	if oldLog != nil && newLog == nil {
		vs = append(vs, Violation{Message: fmt.Sprintf("「%s」節が削除されています", strings.TrimPrefix(decisionLogTitle, "## "))})
	} else if oldLog != nil {
		if j, ok := missingLine(oldLog.lines, newLog.lines); ok {
			vs = append(vs, Violation{Message: fmt.Sprintf("「%s」節の既存の行が変更・削除されています (スナップショットの%d行目: %q)",
				strings.TrimPrefix(decisionLogTitle, "## "), oldLog.start+j+1, oldLog.lines[j])})
		}
	}
	return vs
}

// CheckFeedbackPhaseは、計画書のスナップショットsnapshotから現在の内容currentへの
// 変更が、「タスクリスト」節の末尾へのフィードバック対応のフェーズfbN (Nは
// スナップショットの最大の回 + 1) の追加だけかを確かめる。追加されたタスクと、
// 許されない変更を返す。追加されたタスクは、どれもフェーズfbNの未完了のタスクでなければならない。
func CheckFeedbackPhase(snapshot, current string) ([]Task, []Violation) {
	old, err := Parse(snapshot)
	if err != nil {
		return nil, []Violation{{Message: "計画書のスナップショットを読み取れません: " + err.Error()}}
	}
	cur, err := Parse(current)
	if err != nil {
		return nil, []Violation{{Message: "計画書を読み取れません: " + err.Error()}}
	}

	oldSecs, oldLog := splitSections(snapshot)
	newSecs, newLog := splitSections(current)
	vs := compareSections(oldSecs, newSecs, checkAppended)
	switch {
	case (oldLog == nil) != (newLog == nil):
		vs = append(vs, Violation{Message: fmt.Sprintf("「%s」節が追加・削除されています", strings.TrimPrefix(decisionLogTitle, "## "))})
	case oldLog != nil:
		if j, ok := firstDiff(oldLog.lines, newLog.lines); ok {
			vs = append(vs, Violation{LineNo: newLog.start + j + 1, Message: fmt.Sprintf("「%s」節が変更されています", strings.TrimPrefix(decisionLogTitle, "## "))})
		}
	}
	if len(vs) > 0 {
		return nil, vs
	}

	// 既存の行は変わっていないため、追加されたタスクは現在の内容の末尾に並ぶ。
	// 追加した行がHTMLコメントやコードブロックを閉じて既存の行の読み方を変えた場合に備え、
	// 既存のタスクがそのまま残っているかも確かめる。
	round := old.NextFeedbackRound()
	if len(cur.Tasks) < len(old.Tasks) || !slices.EqualFunc(old.Tasks, cur.Tasks[:len(old.Tasks)], func(a, b Task) bool { return reflect.DeepEqual(a, b) }) {
		return nil, []Violation{{Message: "既存のタスクの読み取り結果が変わっています"}}
	}
	added := cur.Tasks[len(old.Tasks):]
	if len(added) == 0 {
		return nil, []Violation{{Message: fmt.Sprintf("フェーズ%s%dのタスクが追加されていません", feedbackPrefix, round)}}
	}
	for _, t := range added {
		if r, ok := t.FeedbackRound(); !ok || r != round {
			vs = append(vs, Violation{LineNo: t.LineNo, Message: fmt.Sprintf("タスク %s はフェーズ%s%dのタスクではありません", t.ID, feedbackPrefix, round)})
		}
		if t.Done {
			vs = append(vs, Violation{LineNo: t.LineNo, Message: fmt.Sprintf("追加されたタスク %s にチェックが入っています", t.ID)})
		}
	}
	if len(vs) > 0 {
		return nil, vs
	}
	return added, nil
}

// compareSectionsは節を見出しの順に対応付けて比べる。「タスクリスト」節はtaskListで、
// それ以外の節は内容が変わっていないかで比べる。
func compareSections(oldSecs, newSecs []section, taskList func(o, n section) []Violation) []Violation {
	var vs []Violation
	for i := 0; i < max(len(oldSecs), len(newSecs)); i++ {
		if i >= len(oldSecs) {
			vs = append(vs, Violation{LineNo: newSecs[i].start + 1, Message: fmt.Sprintf("節 %q が追加されています", newSecs[i].title)})
			break
		}
		if i >= len(newSecs) {
			vs = append(vs, Violation{Message: fmt.Sprintf("節 %q が削除されています", oldSecs[i].title)})
			break
		}
		o, n := oldSecs[i], newSecs[i]
		if o.title != n.title {
			// 見出しがずれると以降の節を正しく対応付けられないため、ここで比較をやめる。
			vs = append(vs, Violation{LineNo: n.start + 1, Message: fmt.Sprintf("節 %q が %q に変わっています", o.title, n.title)})
			break
		}
		if o.title == sectionTitle {
			vs = append(vs, taskList(o, n)...)
			continue
		}
		if j, ok := firstDiff(o.lines, n.lines); ok {
			vs = append(vs, Violation{LineNo: n.start + j + 1, Message: fmt.Sprintf("%sが変更されています", sectionName(o.title))})
		}
	}
	return vs
}

// checkAppendedは「タスクリスト」節の変更が、節の末尾 (次の見出しの前の空行より前) への
// 行の追加だけで、追記がフィードバック対応のフェーズの見出しから始まるかを確かめる。
// 既存のタスクの説明行はパース結果に現れないため、見出しの前の追記を許すと、
// 最後の既存タスクの説明を書き足す変更を検知できない。
func checkAppended(o, n section) []Violation {
	ol, nl := trimTrailingBlank(o.lines), trimTrailingBlank(n.lines)
	if len(nl) < len(ol) || !slices.Equal(ol, nl[:len(ol)]) {
		j, _ := firstDiff(ol, nl)
		return []Violation{{LineNo: n.start + j + 1, Message: fmt.Sprintf("%sの既存の行が変更・削除されています", sectionName(o.title))}}
	}
	for j := len(ol); j < len(nl); j++ {
		if strings.TrimSpace(nl[j]) == "" {
			continue
		}
		if !strings.HasPrefix(nl[j], "### フェーズfb") {
			return []Violation{{LineNo: n.start + j + 1, Message: "追記はフィードバック対応のフェーズの見出しから始めてください"}}
		}
		break
	}
	return nil
}

// trimTrailingBlankはlinesの末尾の空行を除いたものを返す。
func trimTrailingBlank(lines []string) []string {
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return lines[:end]
}

// splitSectionsはsrcを `## ` の見出しで節に分ける。「実装判断ログ」節は他の節と
// 比べ方が違うため分けて返し、無ければnilを返す。コードブロックの中の見出しは
// 区切りにしない。
func splitSections(src string) ([]section, *section) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	inCode := codeLines(src, lines)

	secs := []section{{}}
	for i, line := range lines {
		if !inCode[i] && strings.HasPrefix(line, "## ") {
			secs = append(secs, section{title: strings.TrimSpace(line), start: i})
		}
		cur := &secs[len(secs)-1]
		cur.lines = append(cur.lines, line)
	}

	var log *section
	others := secs[:0:0]
	for _, s := range secs {
		if s.title == decisionLogTitle && log == nil {
			log = &s
			continue
		}
		others = append(others, s)
	}
	return others, log
}

// checkTaskListは「タスクリスト」節の変更が、taskIDのタスクのチェックボックスに
// チェックを入れる変更だけかを確かめる。
func checkTaskList(o, n section, taskID string) []Violation {
	if len(o.lines) != len(n.lines) {
		j, _ := firstDiff(o.lines, n.lines)
		return []Violation{{LineNo: n.start + j + 1, Message: fmt.Sprintf("%sの行が増減しています", sectionName(o.title))}}
	}

	var vs []Violation
	inTask := false
	for j, line := range o.lines {
		if m := reTask.FindStringSubmatch(line); m != nil {
			inTask = m[2] == taskID
		} else if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTask = false
		}
		if line == n.lines[j] {
			continue
		}
		if inTask && reUnchecked.MatchString(line) && reUnchecked.ReplaceAllString(line, "${1}[x]") == n.lines[j] {
			continue
		}
		vs = append(vs, Violation{LineNo: n.start + j + 1, Message: fmt.Sprintf("%sで、タスク %s のチェック以外の行が変更されています", sectionName(o.title), taskID)})
	}
	return vs
}

// sectionNameは節の見出しを表示用に返す。
func sectionName(title string) string {
	if title == "" {
		return "最初の見出しより前"
	}
	return fmt.Sprintf("「%s」節", strings.TrimPrefix(title, "## "))
}

// firstDiffはaとbで最初に異なる行の添字を返す。同じならfalseを返す。
func firstDiff(a, b []string) (int, bool) {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i, true
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b)), true
	}
	return 0, false
}

// missingLineは、oldの行が順序を保ったままnewに残っているか (newがoldに行を
// 足しただけか) を確かめ、残っていない最初の行の添字を返す。
func missingLine(old, new []string) (int, bool) {
	j := 0
	for i, line := range old {
		for j < len(new) && new[j] != line {
			j++
		}
		if j == len(new) {
			return i, true
		}
		j++
	}
	return 0, false
}
