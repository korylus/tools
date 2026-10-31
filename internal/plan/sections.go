package plan

import (
	"regexp"
	"strings"
)

// reCommentはHTMLコメント (テンプレートの書き方のガイドライン) にマッチする。
var reComment = regexp.MustCompile(`(?s)<!--.*?-->`)

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
