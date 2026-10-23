package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseClaude(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want parsed
	}{
		{
			name: "セッションIDと構造化出力を取り出す",
			src:  claudeOutput,
			want: parsed{
				sessionID: "claude-session",
				output:    []byte(`{"status":"completed","summary":"実装した","escalations":[],"verification_items":[]}`),
			},
		},
		{
			name: "JSONでない行を読み飛ばす",
			src:  "warning: something\n" + `{"type":"system","subtype":"init","session_id":"s"}` + "\n\n",
			want: parsed{sessionID: "s"},
		},
		{
			name: "structured_outputがnullなら構造化出力なしとする",
			src:  `{"type":"result","is_error":false,"session_id":"s","structured_output":null}` + "\n",
			want: parsed{sessionID: "s"},
		},
		{
			name: "resultの行が無いまま上限で止まっても理由を残す",
			src:  `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"},"session_id":"s"}` + "\n",
			want: parsed{sessionID: "s", rateLimited: true, failure: "rate_limit_eventのstatusがrejected"},
		},
		{
			name: "上限の文面の失敗は上限として扱う",
			src:  `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s","result":"Claude AI usage limit reached|1792333200"}` + "\n",
			want: parsed{sessionID: "s", rateLimited: true, failure: "Claude AI usage limit reached|1792333200"},
		},
		{
			name: "最終結果のターン数とコストを取り出す",
			src:  `{"type":"result","subtype":"success","is_error":false,"session_id":"s","num_turns":4,"total_cost_usd":1.5}` + "\n",
			want: parsed{sessionID: "s", turns: 4, costUSD: 1.5},
		},
		{
			name: "rejectedの通知の後に正常に完了したら上限到達としない",
			src: `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"},"session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"session_id":"s","structured_output":{"answer":"x"}}
`,
			want: parsed{sessionID: "s", output: []byte(`{"answer":"x"}`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseClaude(strings.NewReader(tt.src))
			if err != nil {
				t.Fatalf("parseClaudeのエラー = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseClaude() = %+v\n期待値 = %+v", got, tt.want)
			}
		})
	}
}

func TestParseCodex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want parsed
	}{
		{
			name: "スレッドIDと最後のメッセージの構造化出力を取り出す",
			src:  codexOutput,
			want: parsed{
				sessionID: "codex-thread",
				output:    []byte(`{"status":"completed","summary":"レビューした","escalations":[],"verification_items":[]}`),
			},
		},
		{
			name: "モデルの切り替えの警告は失敗として扱わない",
			src: `{"type":"thread.started","thread_id":"t"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"This session was recorded with model ` + "`a`" + ` but is resuming with ` + "`b`" + `."}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"{\"answer\":\"x\"}"}}
`,
			want: parsed{sessionID: "t", output: []byte(`{"answer":"x"}`)},
		},
		{
			name: "最後のメッセージがJSONでなければ構造化出力なしとする",
			src: `{"type":"thread.started","thread_id":"t"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"x\"}"}}
{"type":"item.completed","item":{"type":"agent_message","text":"終わりました"}}
`,
			want: parsed{sessionID: "t"},
		},
		{
			name: "上限に当たらない失敗",
			src: `{"type":"thread.started","thread_id":"t"}
{"type":"error","message":"stream disconnected before completion"}
`,
			want: parsed{sessionID: "t", failure: "stream disconnected before completion"},
		},
		{
			name: "接続回復後の正常完了では途中のエラーを残さない",
			src:  "{\"type\":\"error\",\"message\":\"Reconnecting... 1/5\"}\n{\"type\":\"turn.completed\"}\n",
		},
		{
			name: "回復済みの上限通知は最終的な上限到達としない",
			src:  "{\"type\":\"error\",\"message\":\"rate limit reached\"}\n{\"type\":\"turn.completed\"}\n",
		},
		{
			name: "再試行後に失敗したら最終的な理由を残す",
			src:  "{\"type\":\"error\",\"message\":\"rate limit reached\"}\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"connection failed\"}}\n",
			want: parsed{failure: "connection failed"},
		},
		{
			name: "再試行後に上限で失敗したら上限到達とする",
			src:  "{\"type\":\"error\",\"message\":\"Reconnecting... 1/5\"}\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"usage limit reached\"}}\n",
			want: parsed{failure: "usage limit reached", rateLimited: true},
		},
		{
			name: "エラーだけで終了したら失敗を残す",
			src:  "{\"type\":\"error\",\"message\":\"rate limit reached\"}\n",
			want: parsed{failure: "rate limit reached", rateLimited: true},
		},
		{
			name: "本文の無いturn.failedでも失敗を残す",
			src:  "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"{\\\"answer\\\":\\\"x\\\"}\"}}\n{\"type\":\"turn.failed\"}\n",
			want: parsed{failure: "turn.failed", output: []byte(`{"answer":"x"}`)},
		},
		{
			name: "本文の無いturn.failedは直前のエラーの文面を残す",
			src:  "{\"type\":\"error\",\"message\":\"stream disconnected\"}\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"\"}}\n",
			want: parsed{failure: "stream disconnected"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseCodex(strings.NewReader(tt.src))
			if err != nil {
				t.Fatalf("parseCodexのエラー = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseCodex() = %+v\n期待値 = %+v", got, tt.want)
			}
		})
	}
}

func TestDecodeUnattended(t *testing.T) {
	t.Parallel()

	got, err := DecodeUnattended([]byte(`{
		"status": "escalated",
		"summary": "設計と異なるアプローチが必要",
		"escalations": [{"reason": "r", "question": "q", "options": ["a", "b"], "recommended": "a"}],
		"verification_items": [{"kind": "screen", "title": "t", "target": "/settings", "steps": ["s"], "expected": "e"}],
		"protected_changes": [{"area": "schema", "summary": "列を追加", "reason": "設計どおり"}],
		"protected_checks": [{"area": "schema", "point": "ロールバックできるか", "result": "ok", "detail": "downがある"}]
	}`))
	if err != nil {
		t.Fatalf("DecodeUnattendedのエラー = %v", err)
	}
	want := &UnattendedResult{
		Status:            StatusEscalated,
		Summary:           "設計と異なるアプローチが必要",
		Escalations:       []Escalation{{Reason: "r", Question: "q", Options: []string{"a", "b"}, Recommended: "a"}},
		VerificationItems: []VerificationItem{{Kind: "screen", Title: "t", Target: "/settings", Steps: []string{"s"}, Expected: "e"}},
		ProtectedChanges:  []ProtectedChange{{Area: "schema", Summary: "列を追加", Reason: "設計どおり"}},
		ProtectedChecks:   []ProtectedCheck{{Area: "schema", Point: "ロールバックできるか", Result: "ok", Detail: "downがある"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DecodeUnattended() = %+v\n期待値 = %+v", got, want)
	}

	for _, src := range []string{
		`{"status":"done","summary":"","escalations":[],"verification_items":[]}`,
		`{"status":"completed","extra":1}`,
		`not json`,
	} {
		if _, err := DecodeUnattended([]byte(src)); err == nil {
			t.Errorf("DecodeUnattended(%s) のエラー = nil、エラーを期待", src)
		}
	}
}

// TestUnattendedSchemaは埋め込んだスキーマが、UnattendedResultと配列の要素の型の
// フィールドと一致していることを確かめる。
func TestUnattendedSchema(t *testing.T) {
	t.Parallel()

	type object struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var schema object
	if err := json.Unmarshal(UnattendedSchema, &schema); err != nil {
		t.Fatalf("スキーマがJSONとして読めない: %v", err)
	}
	typ := reflect.TypeFor[UnattendedResult]()
	if got, want := schema.Required, jsonFields(typ); !reflect.DeepEqual(got, want) {
		t.Errorf("スキーマのrequired = %v、UnattendedResultのフィールド = %v", got, want)
	}

	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		name := f.Tag.Get("json")
		var prop struct {
			Items object `json:"items"`
		}
		if err := json.Unmarshal(schema.Properties[name], &prop); err != nil {
			t.Fatalf("スキーマの %s を読めない: %v", name, err)
		}
		if got, want := prop.Items.Required, jsonFields(f.Type.Elem()); !reflect.DeepEqual(got, want) {
			t.Errorf("スキーマの %s のrequired = %v、%sのフィールド = %v", name, got, f.Type.Elem().Name(), want)
		}
	}
}

// jsonFieldsは構造体の型typのフィールドのJSONの名前を、宣言の順に返す。
func jsonFields(typ reflect.Type) []string {
	fields := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Tag.Get("json"))
	}
	return fields
}
