package report

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/korylus/tools/internal/pilot/gate"
	"github.com/korylus/tools/internal/pilot/state"
	"github.com/korylus/tools/internal/pilot/usage"
)

// timeLayoutは作業報告書の時刻の表示形式。記録した時刻のオフセットのまま表示する。
const timeLayout = "2006-01-02 15:04 -07:00"

//go:embed report.html.tmpl
var templateText string

var tmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"time":         formatTime,
	"duration":     formatDuration,
	"itemKind":     itemKindLabel,
	"eventKind":    eventKindLabel,
	"window":       func(w string) string { return usage.Window(w).Label() },
	"fileKind":     fileKindLabel,
	"statusLabel":  statusLabel,
	"checkResult":  checkResultLabel,
	"join":         strings.Join,
	"percent":      func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"usd":          func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"overExpected": func(t Task) bool { return t.ExpectedImplLines > 0 && t.ImplLines > t.ExpectedImplLines },
	"withResult":   func(v view, it Item) itemView { return itemView{Item: it, Result: v.Results[it.ID]} },
}).Parse(templateText))

// itemViewは確認項目1件と、その動作確認の結果。
type itemView struct {
	Item   Item
	Result ItemResult
}

// viewは作業報告書のテンプレートに渡すデータ。
type view struct {
	*Report
	// Resultsは項目のIDごとの動作確認の結果、Commentは全体へのコメント。
	Results map[string]ItemResult
	Comment string
	// SavedAtは動作確認の結果を最後に保存した時刻。まだ保存されていなければゼロ値。
	SavedAt time.Time
}

// Pendingはこの版で確かめる項目を返す。
func (v view) Pending() []Item {
	var items []Item
	for _, it := range v.Items {
		if !it.Confirmed {
			items = append(items, it)
		}
	}
	return items
}

// Confirmedは前の版までにOKになった項目を返す。
func (v view) Confirmed() []Item {
	var items []Item
	for _, it := range v.Items {
		if it.Confirmed {
			items = append(items, it)
		}
	}
	return items
}

// ItemNumberは項目のIDから、表示する番号を返す。無ければ0を返す。
func (v view) ItemNumber(id string) int {
	for _, it := range v.Items {
		if it.ID == id {
			return it.Number
		}
	}
	return 0
}

// Renderは作業報告書rをHTMLとしてwに書く。verificationは埋め込む動作確認の結果で、
// まだ無ければnilを渡す (確認済みの項目はOK、それ以外は未確認として描く)。
func Render(w io.Writer, r *Report, verification *Verification) error {
	v := view{Report: r, Results: map[string]ItemResult{}}
	for _, it := range r.Items {
		if it.Confirmed {
			v.Results[it.ID] = ItemResult{Status: StatusOK}
		}
	}
	if verification != nil {
		// 明示的に未確認へ戻した結果も、確認済みの初期値より優先する。
		for id, result := range verification.Items {
			v.Results[id] = result
		}
		v.Comment, v.SavedAt = verification.Comment, verification.SavedAt
	}
	return tmpl.Execute(w, v)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(timeLayout)
}

// formatDurationは所要時間を「1時間2分3秒」の形にする。秒未満は切り捨てる。
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%d時間%d分%d秒", h, m, s)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, s)
	}
	return fmt.Sprintf("%d秒", s)
}

func itemKindLabel(kind string) string {
	switch kind {
	case KindScreen:
		return "画面"
	case KindProcess:
		return "処理"
	case KindProtected:
		return "保護領域"
	}
	return kind
}

func eventKindLabel(kind string) string {
	switch state.EventKind(kind) {
	case state.EventAgent:
		return "エージェント"
	case state.EventGate:
		return "ゲート"
	case state.EventUsageWait:
		return "使用量の待機"
	case state.EventHumanWait:
		return "人間の待ち"
	}
	return kind
}

func fileKindLabel(kind string) string {
	switch gate.FileKind(kind) {
	case gate.FileImpl:
		return "実装"
	case gate.FileTest:
		return "テスト"
	case gate.FileGenerated:
		return "生成物"
	case gate.FileBinary:
		return "バイナリ"
	case gate.FileDeleted:
		return "削除"
	}
	return kind
}

func statusLabel(status string) string {
	switch status {
	case StatusOK:
		return "OK"
	case StatusNG:
		return "NG"
	}
	return "未確認"
}

func checkResultLabel(result string) string {
	switch result {
	case "ok":
		return "問題なし"
	case "concern":
		return "懸念あり"
	}
	return result
}
