package plan

import (
	"regexp"
	"strings"
)

var (
	// reCommentはHTMLコメント (テンプレートの書き方のガイドライン) にマッチする。
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	// reDecisionTaskは「実装判断ログ」節のタスクごとの見出しにマッチし、タスク番号を取り出す。
	reDecisionTask = regexp.MustCompile(`^### タスク\s*([0-9A-Za-z]+(?:-[0-9A-Za-z]+)+)\s*[:：]`)
	// reDecisionTopicは「実装判断ログ」節の論点の見出しにマッチする。
	reDecisionTopic = regexp.MustCompile(`^#### (.+)$`)
)

// Sectionは見出しが `## name` の節の本文 (見出しの行を除く) を返す。
// HTMLコメントは除き、前後の空行を詰める。節が無ければfalseを返す。
func Section(src, name string) (string, bool) {
	secs, log := splitSections(src)
	if log != nil {
		secs = append(secs, *log)
	}
	for _, s := range secs {
		if s.title != "## "+name {
			continue
		}
		body := strings.Join(s.lines[1:], "\n")
		return strings.TrimSpace(reComment.ReplaceAllString(body, "")), true
	}
	return "", false
}

// DecisionTopicsは「実装判断ログ」節から、タスク番号ごとの論点の見出しを書かれた順に返す。
// 同じタスクの見出しが複数あれば (同じタスクで複数回実行したとき)、論点をつなげる。
func DecisionTopics(src string) map[string][]string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")
	inCode := codeLines(src, lines)

	topics := map[string][]string{}
	inLog, inComment := false, false
	task := ""
	for i, line := range lines {
		if inCode[i] {
			continue
		}
		if inComment {
			inComment = !strings.Contains(line, "-->")
			continue
		}
		if start := strings.Index(line, "<!--"); start >= 0 && !strings.Contains(line[start:], "-->") {
			inComment = true
			continue
		}
		if strings.HasPrefix(line, "## ") {
			inLog = strings.TrimSpace(line) == decisionLogTitle
			task = ""
			continue
		}
		if !inLog {
			continue
		}
		if m := reDecisionTask.FindStringSubmatch(line); m != nil {
			task = m[1]
			continue
		}
		if strings.HasPrefix(line, "### ") {
			task = ""
			continue
		}
		if m := reDecisionTopic.FindStringSubmatch(line); m != nil && task != "" {
			topics[task] = append(topics[task], strings.TrimSpace(m[1]))
		}
	}
	return topics
}
