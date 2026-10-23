package usage

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// claudeStreamは実機の `claude -p --output-format stream-json --verbose` の出力に沿った例。
const claudeStream = `{"type":"system","subtype":"init","session_id":"s"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1792411800,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.02,"resetsAt":1792411800},"seven_day":{"utilization":0.18,"resetsAt":1792652400}}},"session_id":"s"}
{"type":"assistant","message":{"content":[{"type":"text","text":"rate_limit_eventという文字列を含む本文"}]},"session_id":"s"}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.07,"resetsAt":1792411800},"seven_day_opus":{"utilization":0.5,"resetsAt":1792652400}}},"session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"session_id":"s"}
`

func TestParseClaudeStream(t *testing.T) {
	t.Parallel()

	got, err := ParseClaudeStream(strings.NewReader(claudeStream))
	if err != nil {
		t.Fatalf("ParseClaudeStreamのエラー = %v", err)
	}
	// 5時間枠は最後のイベントの値、週枠は最後に含まれていたイベントの値になる。
	// モデル別の枠 (seven_day_opus) は読まない。
	want := []Sample{
		{Window: FiveHour, Percent: 7, ResetsAt: time.Unix(1792411800, 0).UTC()},
		{Window: Weekly, Percent: 18, ResetsAt: time.Unix(1792652400, 0).UTC()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseClaudeStream() = %+v、期待値 = %+v", got, want)
	}
}

func TestParseClaudeStream_NoEvent(t *testing.T) {
	t.Parallel()

	got, err := ParseClaudeStream(strings.NewReader(`{"type":"result","is_error":false}` + "\n"))
	if err != nil {
		t.Fatalf("ParseClaudeStreamのエラー = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ParseClaudeStream() = %+v、空を期待", got)
	}
}

// usageOutputは実機の `claude -p "/usage" --output-format json` の出力に沿った例。
const usageOutput = `{"is_error":false,"num_turns":0,"total_cost_usd":0,"subtype":"success","type":"result","result":"You are currently using your subscription to power your Claude Code usage\n\nCurrent session: 3% used · resets Oct 19, 12:10pm (UTC)\nCurrent week (all models): 19% used · resets Oct 22, 7am (UTC)\nCurrent week (Fable): 0% used · resets Oct 22, 7am (UTC)\n\nWhat's contributing to your limits usage?\n  61% of your usage was at >150k context\n"}`

func TestParseClaudeUsage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)
	got, err := ParseClaudeUsage([]byte(usageOutput), now)
	if err != nil {
		t.Fatalf("ParseClaudeUsageのエラー = %v", err)
	}
	want := []Sample{
		{Window: FiveHour, Percent: 3, ResetsAt: time.Date(2026, 10, 19, 12, 10, 0, 0, time.UTC)},
		{Window: Weekly, Percent: 19, ResetsAt: time.Date(2026, 10, 22, 7, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseClaudeUsage() = %+v、期待値 = %+v", got, want)
	}
}

func TestParseUsageReset(t *testing.T) {
	t.Parallel()

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 12, 30, 22, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		text string
		zone string
		want time.Time
	}{
		{name: "時刻に分を含む", text: "Dec 31, 1:30am", zone: "UTC", want: time.Date(2026, 12, 31, 1, 30, 0, 0, time.UTC)},
		{name: "年をまたぐリセットは翌年として読む", text: "Jan 2, 7am", zone: "UTC", want: time.Date(2027, 1, 2, 7, 0, 0, 0, time.UTC)},
		{name: "直前に過ぎたリセットは同じ年のまま読む", text: "Dec 30, 9pm", zone: "UTC", want: time.Date(2026, 12, 30, 21, 0, 0, 0, time.UTC)},
		{name: "日付の無い時刻は当日として読む", text: "11pm", zone: "UTC", want: time.Date(2026, 12, 30, 23, 0, 0, 0, time.UTC)},
		{name: "日付の無い時刻が過ぎていれば翌日として読む", text: "3:15am", zone: "UTC", want: time.Date(2026, 12, 31, 3, 15, 0, 0, time.UTC)},
		{name: "タイムゾーン名で読む", text: "Dec 31, 9am", zone: "Asia/Tokyo", want: time.Date(2026, 12, 31, 9, 0, 0, 0, tokyo)},
		{name: "タイムゾーンが無ければUTCとして読む", text: "Dec 31, 9am", zone: "", want: time.Date(2026, 12, 31, 9, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseUsageReset(tt.text, tt.zone, now)
			if err != nil {
				t.Fatalf("parseUsageResetのエラー = %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("parseUsageReset(%q, %q) = %v、期待値 = %v", tt.text, tt.zone, got, tt.want)
			}
		})
	}
}

func TestParseClaudeUsage_Invalid(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)
	tests := []struct {
		name   string
		output string
		want   string // エラーメッセージに含まれることを期待する文字列
	}{
		{name: "JSONでない", output: "Current session: 3% used", want: "/usage の出力を読み取れません"},
		{name: "失敗の報告", output: `{"is_error":true,"result":"Not logged in"}`, want: "Not logged in"},
		{name: "使用率の行が無い", output: `{"is_error":false,"result":"Usage is unavailable"}`, want: "使用率の行がありません"},
		{name: "読めないリセット時刻", output: `{"is_error":false,"result":"Current session: 3% used · resets soon (UTC)"}`, want: `リセット時刻 "soon"`},
		{name: "未知のタイムゾーン", output: `{"is_error":false,"result":"Current session: 3% used · resets Oct 19, 1pm (Mars/Base)"}`, want: `タイムゾーン "Mars/Base"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseClaudeUsage([]byte(tt.output), now)
			if err == nil {
				t.Fatal("エラーを期待したが、nilだった")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("エラー = %q、含まれることを期待した文字列 = %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParseClaudeUsage_NoReset(t *testing.T) {
	t.Parallel()

	got, err := ParseClaudeUsage([]byte(`{"is_error":false,"result":"Current session: 0% used\n"}`), time.Now())
	if err != nil {
		t.Fatalf("ParseClaudeUsageのエラー = %v", err)
	}
	want := []Sample{{Window: FiveHour, Percent: 0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseClaudeUsage() = %+v、期待値 = %+v", got, want)
	}
}

// appServerResponseは実機の `account/rateLimits/read` の応答に沿った例。
const appServerResponse = `{"id":2,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","primary":{"usedPercent":5,"windowDurationMins":10080,"resetsAt":1792904083},"secondary":null,"planType":"prolite"}}}`

func TestParseAppServerResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		want    []Sample
		wantOK  bool
		wantErr string // 空ならエラーを期待しない
	}{
		{
			name:   "週枠だけのprimary",
			line:   appServerResponse,
			want:   []Sample{{Window: Weekly, Percent: 5, ResetsAt: time.Unix(1792904083, 0).UTC()}},
			wantOK: true,
		},
		{
			name: "枠の長さで5時間枠と週枠に振り分ける",
			line: `{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":12.5,"windowDurationMins":300,"resetsAt":1792411800},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":1792904083}}}}`,
			want: []Sample{
				{Window: FiveHour, Percent: 12.5, ResetsAt: time.Unix(1792411800, 0).UTC()},
				{Window: Weekly, Percent: 40, ResetsAt: time.Unix(1792904083, 0).UTC()},
			},
			wantOK: true,
		},
		{name: "別の要求への応答", line: `{"id":1,"result":{"userAgent":"x"}}`},
		{name: "通知", line: `{"method":"account/updated","params":{}}`},
		{name: "失敗の応答", line: `{"id":2,"error":{"code":-32600,"message":"not logged in"}}`, wantOK: true, wantErr: "not logged in"},
		{name: "使用率が無い", line: `{"id":2,"result":{"rateLimits":null}}`, wantOK: true, wantErr: "使用率がありません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok, err := parseAppServerResponse([]byte(tt.line), 2)
			if ok != tt.wantOK {
				t.Errorf("ok = %v、期待値 = %v", ok, tt.wantOK)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("エラー = %v、含まれることを期待した文字列 = %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAppServerResponseのエラー = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAppServerResponse() = %+v、期待値 = %+v", got, tt.want)
			}
		})
	}
}

// sessionLogは実機のCodexのセッションログに沿った例。
const sessionLog = `{"timestamp":"2026-10-19T08:00:00.000Z","type":"session_meta","payload":{"id":"x"}}
{"timestamp":"2026-10-19T08:10:00.000Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":4.0,"window_minutes":10080,"resets_at":1792904083},"secondary":null}}}
{"timestamp":"2026-10-19T08:20:58.317Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":5.0,"window_minutes":10080,"resets_at":1792904083},"secondary":null}}}
{"timestamp":"2026-10-19T08:21:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}
`

func TestParseCodexSessionLog(t *testing.T) {
	t.Parallel()

	got, err := ParseCodexSessionLog(strings.NewReader(sessionLog))
	if err != nil {
		t.Fatalf("ParseCodexSessionLogのエラー = %v", err)
	}
	want := []Sample{{
		At:       time.Date(2026, 10, 19, 8, 20, 58, 317000000, time.UTC),
		Window:   Weekly,
		Percent:  5,
		ResetsAt: time.Unix(1792904083, 0).UTC(),
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseCodexSessionLog() = %+v、期待値 = %+v", got, want)
	}

	if _, err := ParseCodexSessionLog(strings.NewReader(`{"type":"session_meta"}` + "\n")); err == nil {
		t.Error("使用率の無いログでエラーを期待したが、nilだった")
	}
}
