package report

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Statusは動作確認の項目1件の結果。未確認は空文字。
type Status string

const (
	StatusOK Status = "ok"
	StatusNG Status = "ng"
)

// Resultsは作業報告書1版に書き込まれた動作確認の結果。
type Results struct {
	// Itemsは、チェックリストに載せた項目のIDごとの結果。
	Items map[string]Result
	// Confirmedは、確認済みとして一覧だけを載せた項目のID。
	Confirmed map[string]bool
	// Commentは全体へのコメント。
	Comment string
	// overallは、全体へのコメントの見出しを見分けられたかどうか。
	overall bool
}

// Resultは項目1件の結果。
type Result struct {
	// Numberは作業報告書に表示した項目の番号。
	Number  int
	Status  Status
	Comment string
}

const (
	overallTitle   = "## 全体へのコメント"
	overallGuide   = "<!-- チェックリストに無い気づきを、この下に書く -->"
	confirmedTitle = "### 確認済みの項目"
	confirmedIntro = "前の版までにOKになった項目です。\n確かめ直す必要はありません。"
	commentLabel   = "コメント:"
)

var (
	reItemHeading = regexp.MustCompile(`^#([0-9]+) `)
	reItemID      = regexp.MustCompile(`^<!--\s*item:\s*(.+?)\s*-->$`)
	reConfirmedID = regexp.MustCompile(`<!--\s*confirmed:\s*(.+?)\s*-->`)
	reCheckbox    = regexp.MustCompile(`^\s*[-*+] \[([ xX])\]\s*(OK|NG)(.*)$`)
)

// ParseResultsは、作業報告書srcに人が書き込んだ動作確認の結果を読む。
// 項目はHTMLコメントのIDで見分け、OKとNGの両方にチェックがあればNGとして扱う。
// 見出しに続くIDや案内で報告書の区切りを見分け、コメント中の見出しやコードは本文のまま残す。
func ParseResults(src string) *Results {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	source := []byte(src)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	res := &Results{Items: map[string]Result{}, Confirmed: map[string]bool{}}
	var (
		cur          *Result
		id           string
		body         int
		commentStart = -1
		confirmed    bool
	)
	flush := func(end int) {
		if cur != nil {
			metadataEnd := end
			if commentStart >= 0 {
				cur.Comment = commentText(src[commentStart:end])
				metadataEnd = commentStart
			}
			var notes []string
			for line := range strings.SplitSeq(src[body:metadataEnd], "\n") {
				m := reCheckbox.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				if m[1] != " " {
					if m[2] == "NG" {
						cur.Status = StatusNG
					} else if cur.Status == "" {
						cur.Status = StatusOK
					}
				}
				if strings.TrimSpace(m[3]) != "" {
					notes = append(notes, strings.TrimSpace(m[2]+m[3]))
				}
			}
			// チェックの後ろに書いた文は、作り直しでチェックの行を書き直しても消えないようコメントに移す。
			if cur.Comment != "" {
				notes = append(notes, cur.Comment)
			}
			cur.Comment = strings.Join(notes, "\n\n")
			res.Items[id] = *cur
		}
		if confirmed {
			for _, m := range reConfirmedID.FindAllStringSubmatch(src[body:end], -1) {
				res.Confirmed[m[1]] = true
			}
		}
		cur, commentStart, confirmed = nil, -1, false
	}

	// 文書直下のノードだけを見ることで、コードブロックや引用中の見出しを区切りにしない。
	overall := overallHeading(root, source)
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Lines().Len() > 0 {
			title := headingTitle(h, source)
			marker, markerEnd := resultMarker(h.NextSibling(), source)
			start := strings.LastIndexByte(src[:h.Lines().At(0).Start], '\n') + 1
			switch {
			case n == overall:
				flush(start)
				if marker != overallGuide {
					markerEnd = lineEnd(src, h.Lines().At(h.Lines().Len()-1).Stop)
				}
				res.Comment, res.overall = commentText(src[markerEnd:]), true
				return res
			case h.Level == 3 && reItemHeading.MatchString(title) && reItemID.MatchString(marker):
				flush(start)
				number, _ := strconv.Atoi(reItemHeading.FindStringSubmatch(title)[1])
				cur = &Result{Number: number}
				id = reItemID.FindStringSubmatch(marker)[1]
				body = markerEnd
			case h.Level == 3 && "### "+title == confirmedTitle && isConfirmedSection(h.NextSibling(), source):
				flush(start)
				body, confirmed = h.Lines().At(h.Lines().Len()-1).Stop, true
			}
		}
		if p, ok := n.(*ast.Paragraph); ok && cur != nil && commentStart < 0 {
			line := p.Lines().At(0)
			// ラベルと同じ行に続けて書いた文も、コメントの1行目として読む。
			if rest, found := strings.CutPrefix(string(line.Value(source)), commentLabel); found {
				commentStart = line.Stop - len(strings.TrimLeft(rest, " \t"))
			}
		}
	}
	flush(len(src))
	return res
}

// overallHeadingは、全体へのコメントの見出しを返す。案内が続く見出しを優先し、案内を
// 消して書き込まれていれば最後の見出しを使う。項目のコメントに同じ見出しを書いても、
// 全体へのコメントと取り違えないためである。見出しが無ければnilを返す。
func overallHeading(root ast.Node, source []byte) ast.Node {
	var last ast.Node
	for n := root.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok || h.Level != 2 || h.Lines().Len() == 0 || "## "+headingTitle(h, source) != overallTitle {
			continue
		}
		if marker, _ := resultMarker(h.NextSibling(), source); marker == overallGuide {
			return h
		}
		last = h
	}
	return last
}

func headingTitle(h *ast.Heading, source []byte) string {
	return strings.TrimSpace(string(h.Lines().Value(source)))
}

// lineEndは、srcのpos番目のバイトを含む行の次の行の開始位置を返す。
func lineEnd(src string, pos int) int {
	if i := strings.IndexByte(src[pos:], '\n'); i >= 0 {
		return pos + i + 1
	}
	return len(src)
}

// resultMarkerは、見出し直後のHTMLコメントと、その後の本文の開始位置を返す。
func resultMarker(n ast.Node, source []byte) (string, int) {
	h, ok := n.(*ast.HTMLBlock)
	if !ok {
		return "", 0
	}
	var b strings.Builder
	end := 0
	for i := range h.Lines().Len() {
		line := h.Lines().At(i)
		b.Write(line.Value(source))
		end = line.Stop
	}
	if h.HasClosure() {
		b.Write(h.ClosureLine.Value(source))
		end = h.ClosureLine.Stop
	}
	return strings.TrimSpace(b.String()), end
}

// isConfirmedSectionは、同名の小見出しをコメントに書いても、生成した確認済み一覧と混同しないための判定。
func isConfirmedSection(n ast.Node, source []byte) bool {
	if p, ok := n.(*ast.Paragraph); ok && strings.TrimSpace(string(p.Lines().Value(source))) == confirmedIntro {
		return true
	}
	if _, ok := n.(*ast.List); !ok {
		return false
	}
	for li := n.FirstChild(); li != nil; li = li.NextSibling() {
		if first := li.FirstChild(); first != nil && reConfirmedID.Match(first.Lines().Value(source)) {
			return true
		}
	}
	return false
}

// commentTextは前後の空行だけを除く。コードのインデントや改行を表す行末の空白は残す。
func commentText(src string) string {
	lines := strings.Split(src, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
