package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxLineはstream-json / JSONLの1行の最大長。ツールの出力にファイルの内容が
// そのまま載ることがあるため、bufio.Scannerの既定値 (64KiB) より大きくする。
const maxLine = 64 << 20

// claudeWindowsはClaude Codeの `unifiedWindows` のキーと枠の対応。
// モデル別の枠などここに無いキーは読まない。
var claudeWindows = map[string]Window{
	"five_hour": FiveHour,
	"seven_day": Weekly,
}

// claudeRateLimitEventはstream-jsonの `rate_limit_event` の行のうち、読む項目。
type claudeRateLimitEvent struct {
	Type          string `json:"type"`
	RateLimitInfo *struct {
		UnifiedWindows map[string]struct {
			// Utilizationは0 〜 1の使用率。
			Utilization float64 `json:"utilization"`
			// ResetsAtはUnix秒。
			ResetsAt int64 `json:"resetsAt"`
		} `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

// ParseClaudeStreamはClaude Codeのstream-jsonから、枠ごとに最後の
// `rate_limit_event` の使用率を取り出す。返す観測のAt・Tool・Sourceは呼び出し側が入れる。
func ParseClaudeStream(r io.Reader) ([]Sample, error) {
	last := map[Window]Sample{}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		// rate_limit_event以外の行はJSONとして読まずに飛ばす (ファイルの内容などで大きくなるため)。
		if !bytes.Contains(line, []byte(`"rate_limit_event"`)) {
			continue
		}
		var ev claudeRateLimitEvent
		if json.Unmarshal(line, &ev) != nil || ev.Type != "rate_limit_event" || ev.RateLimitInfo == nil {
			continue
		}
		for key, uw := range ev.RateLimitInfo.UnifiedWindows {
			w, ok := claudeWindows[key]
			if !ok {
				continue
			}
			last[w] = Sample{Window: w, Percent: roundPercent(uw.Utilization * 100), ResetsAt: unixTime(uw.ResetsAt)}
		}
	}
	return ordered(last), sc.Err()
}

// claudeUsageResultは `claude -p "/usage" --output-format json` の出力のうち、読む項目。
type claudeUsageResult struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// reUsageLineは `/usage` のテキストの使用率の行。
//
//	Current session: 3% used · resets Oct 19, 12:10pm (UTC)
//	Current week (all models): 19% used · resets Oct 22, 7am (UTC)
//
// モデル別の週枠 (`Current week (Fable)` など) は `unifiedWindows` と揃えるため読まない。
var reUsageLine = regexp.MustCompile(`^Current (session|week \(all models\)):\s*(\d+(?:\.\d+)?)% used(?:\s*·\s*resets\s+(.+?)(?:\s+\(([^()]+)\))?)?$`)

// usageWindowsは `/usage` のテキストの枠の名前と枠の対応。
var usageWindows = map[string]Window{
	"session":           FiveHour,
	"week (all models)": Weekly,
}

// ParseClaudeUsageは `claude -p "/usage" --output-format json` の出力から使用率を取り出す。
// リセット時刻には年が無いため、nowに最も近い未来 (または直前) の日時として読む。
func ParseClaudeUsage(output []byte, now time.Time) ([]Sample, error) {
	var res claudeUsageResult
	if err := json.Unmarshal(output, &res); err != nil {
		return nil, fmt.Errorf("/usage の出力を読み取れません: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("/usage が失敗を報告しました: %s", res.Result)
	}
	var samples []Sample
	for raw := range strings.SplitSeq(res.Result, "\n") {
		m := reUsageLine.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		percent, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, fmt.Errorf("/usage の使用率 %q を読み取れません: %w", m[2], err)
		}
		s := Sample{Window: usageWindows[m[1]], Percent: percent}
		if m[3] != "" {
			s.ResetsAt, err = parseUsageReset(m[3], m[4], now)
			if err != nil {
				return nil, err
			}
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("/usage の出力に使用率の行がありません: %q", head(res.Result))
	}
	return samples, nil
}

// usageResetLayoutsは `/usage` のリセット時刻の書式。日付の無い書式は当日として読む。
var usageResetLayouts = []struct {
	layout  string
	hasDate bool
}{
	{"Jan 2, 3:04pm", true},
	{"Jan 2, 3pm", true},
	{"Jan 2 3:04pm", true},
	{"Jan 2 3pm", true},
	{"3:04pm", false},
	{"3pm", false},
}

// parseUsageResetは `/usage` のリセット時刻を読む。zoneはIANAのタイムゾーン名で、
// 空ならUTCとして読む。
func parseUsageReset(text, zone string, now time.Time) (time.Time, error) {
	loc := time.UTC
	if zone != "" {
		var err error
		if loc, err = time.LoadLocation(zone); err != nil {
			return time.Time{}, fmt.Errorf("/usage のリセット時刻のタイムゾーン %q を読み取れません: %w", zone, err)
		}
	}
	now = now.In(loc)
	for _, l := range usageResetLayouts {
		t, err := time.ParseInLocation(l.layout, text, loc)
		if err != nil {
			continue
		}
		if !l.hasDate {
			t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if t.Before(now.Add(-time.Hour)) {
				t = t.AddDate(0, 0, 1)
			}
			return t, nil
		}
		t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		// 年をまたぐリセット (12月に見た1月のリセットなど) を翌年として読む。
		// 直前に過ぎたリセットを表示していることもあるため、1日の幅を持たせる。
		if t.Before(now.AddDate(0, 0, -1)) {
			t = t.AddDate(1, 0, 0)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("/usage のリセット時刻 %q を読み取れません", text)
}

// codexWindowはCodexの使用率の1つの枠。app-serverはcamelCase、セッションログは
// snake_caseで同じ内容を返すため、型を分けて同じ形に寄せる。
type codexWindow struct {
	usedPercent   float64
	windowMinutes int
	resetsAt      int64
}

// codexSamplesはprimary / secondaryの枠を、枠の長さで5時間枠と週枠に振り分ける。
// このアカウントのCodexは週枠だけをprimaryで返すため、primary / secondaryの位置では決めない。
func codexSamples(windows ...*codexWindow) []Sample {
	got := map[Window]Sample{}
	for _, cw := range windows {
		if cw == nil || cw.windowMinutes <= 0 {
			continue
		}
		w := Weekly
		if cw.windowMinutes < 24*60 {
			w = FiveHour
		}
		got[w] = Sample{Window: w, Percent: roundPercent(cw.usedPercent), ResetsAt: unixTime(cw.resetsAt)}
	}
	return ordered(got)
}

// appServerWindowは `account/rateLimits/read` の応答の1つの枠。
type appServerWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

func (w *appServerWindow) codex() *codexWindow {
	if w == nil {
		return nil
	}
	return &codexWindow{usedPercent: w.UsedPercent, windowMinutes: w.WindowDurationMins, resetsAt: w.ResetsAt}
}

// appServerMessageはapp-serverのJSON-RPCの1行のうち、読む項目。
type appServerMessage struct {
	ID     *int `json:"id"`
	Result *struct {
		RateLimits *struct {
			Primary   *appServerWindow `json:"primary"`
			Secondary *appServerWindow `json:"secondary"`
		} `json:"rateLimits"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// errNoRateLimitsは取得元の応答・ログに使用率が無かったことを表す。
var errNoRateLimits = errors.New("使用率がありません")

// parseAppServerResponseは `account/rateLimits/read` の応答の行から使用率を取り出す。
// 行がその応答でなければokをfalseにする。
func parseAppServerResponse(line []byte, id int) (samples []Sample, ok bool, err error) {
	var msg appServerMessage
	if json.Unmarshal(line, &msg) != nil || msg.ID == nil || *msg.ID != id {
		return nil, false, nil
	}
	if msg.Error != nil {
		return nil, true, fmt.Errorf("account/rateLimits/read が失敗しました: %s", msg.Error.Message)
	}
	if msg.Result == nil || msg.Result.RateLimits == nil {
		return nil, true, errNoRateLimits
	}
	rl := msg.Result.RateLimits
	samples = codexSamples(rl.Primary.codex(), rl.Secondary.codex())
	if len(samples) == 0 {
		return nil, true, errNoRateLimits
	}
	return samples, true, nil
}

// sessionLogWindowはセッションログの `rate_limits` の1つの枠。
type sessionLogWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func (w *sessionLogWindow) codex() *codexWindow {
	if w == nil {
		return nil
	}
	return &codexWindow{usedPercent: w.UsedPercent, windowMinutes: w.WindowMinutes, resetsAt: w.ResetsAt}
}

// sessionLogLineはCodexのセッションログの1行のうち、読む項目。
type sessionLogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Payload   struct {
		RateLimits *struct {
			Primary   *sessionLogWindow `json:"primary"`
			Secondary *sessionLogWindow `json:"secondary"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// ParseCodexSessionLogはCodexのセッションログから、最後に記録された使用率を
// 取り出す。返す観測のAtはログに記録された時刻で、Tool・Sourceは呼び出し側が入れる。
func ParseCodexSessionLog(r io.Reader) ([]Sample, error) {
	var last []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"rate_limits"`)) {
			continue
		}
		var ln sessionLogLine
		if json.Unmarshal(line, &ln) != nil || ln.Payload.RateLimits == nil {
			continue
		}
		rl := ln.Payload.RateLimits
		samples := codexSamples(rl.Primary.codex(), rl.Secondary.codex())
		if len(samples) == 0 {
			continue
		}
		for i := range samples {
			samples[i].At = ln.Timestamp
		}
		last = samples
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if last == nil {
		return nil, errNoRateLimits
	}
	return last, nil
}

// orderedは枠ごとの観測を、Windowsの順に並べる。
func ordered(m map[Window]Sample) []Sample {
	var samples []Sample
	for _, w := range Windows {
		if s, ok := m[w]; ok {
			samples = append(samples, s)
		}
	}
	return samples
}

// roundPercentは使用率を小数第2位までに丸める。0 〜 1の値を百分率に直したときの
// 浮動小数点の誤差 (0.07 * 100 = 7.000000000000001) を記録に残さないためである。
func roundPercent(v float64) float64 {
	return math.Round(v*100) / 100
}

// unixTimeはUnix秒を時刻に直す。0は不明としてゼロ値にする。
func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// headはエラーに添える、テキストの先頭。
func head(s string) string {
	const n = 200
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
