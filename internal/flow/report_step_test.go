package flow

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestParseReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    state.ReportRound
		wantErr string
	}{
		{name: "URLを受け付ける", input: `{"location":" https://wikino.app/s/demo/pages/1 "}`, want: state.ReportRound{Location: "https://wikino.app/s/demo/pages/1"}},
		{
			name:  "処理の流れを書かなかった理由を受け付ける",
			input: `{"location":"https://wikino.app/s/demo/pages/1","walkthrough_skipped":" 文面だけを直したため "}`,
			want:  state.ReportRound{Location: "https://wikino.app/s/demo/pages/1", WalkthroughSkipped: "文面だけを直したため"},
		},
		{name: "末尾の空白を受け付ける", input: "{\"location\":\"https://wikino.app/s/demo/pages/1\"} \n\t", want: state.ReportRound{Location: "https://wikino.app/s/demo/pages/1"}},
		{name: "空の入力は書き忘れ", input: "\n", wantErr: "標準入力からJSONで渡してください"},
		{name: "locationが無い", input: `{}`, wantErr: "標準入力からJSONで渡してください"},
		{name: "理由だけでlocationが無い", input: `{"walkthrough_skipped":"理由"}`, wantErr: "標準入力からJSONで渡してください"},
		{name: "空のlocation", input: `{"location":" "}`, wantErr: "location に、作業報告書のURLを書いてください"},
		{name: "パス", input: `{"location":"docs/private/report.md"}`, wantErr: "https:// で始まる作業報告書のURL"},
		{name: "httpのURL", input: `{"location":"http://example.com/a"}`, wantErr: "https:// で始まる作業報告書のURL"},
		{name: "空の理由", input: `{"location":"https://wikino.app/s/demo/pages/1","walkthrough_skipped":""}`, wantErr: "walkthrough_skipped に、処理の流れを書かなかった理由を書いてください"},
		{name: "以前の形式のskipped", input: `{"skipped":"理由"}`, wantErr: "読み取れません"},
		{name: "JSONでない", input: `{`, wantErr: "読み取れません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseReport(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("エラー = %v、期待値 = %q を含むエラー", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("parseReport() = %+v, %v、期待値 = %+v", got, err, tt.want)
			}
		})
	}
}

func TestFlowReportWalkthroughSkipped(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [shimbaco] タグを付ける")
	fx.wait(t, state.RoleImplementer)
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatal(err)
	}

	// ブランチを作ったタスクが無くても、作業報告書のステップに進む。
	in := fx.wait(t, state.RoleImplementer)
	if in.Action != ActionReport || len(in.Branches) != 0 || !strings.Contains(in.String(), "- なし (ブランチを作ったタスクがありません)") {
		t.Fatalf("指示 = %+v\n%s", in, in.String())
	}
	_, err := fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "作業報告書の記録を読み取れません")
	if _, err := fx.done(state.RoleImplementer, `{"location":"`+testReportURL+`","walkthrough_skipped":"人が行うタスクだけの計画書のため"}`); err != nil {
		t.Fatal(err)
	}
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Reports) != 1 || st.Reports[0].Location != testReportURL || st.Reports[0].WalkthroughSkipped != "人が行うタスクだけの計画書のため" {
		t.Errorf("作業報告書の記録 = %+v", st.Reports)
	}
	if out := fx.facts(t); !strings.Contains(out, "(処理の流れは書いていません: 人が行うタスクだけの計画書のため)") {
		t.Errorf("事実に処理の流れを書かなかった理由が無い:\n%s", out)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish || in.Report != testReportURL {
		t.Errorf("指示 = %+v、作業報告書のURLを持つ終了を期待", in)
	}
}

func TestFlowReportAfterAddedTask(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [shimbaco] タグを付ける")
	fx.wait(t, state.RoleImplementer)
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatal(err)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionReport {
		t.Fatalf("指示 = %+v、作業報告書の指示を期待", in)
	}

	// 作業報告書を書いている間に人がタスクを足すと、足したタスクのあとにもう一度書いてもらう。
	src, err := os.ReadFile(fx.planFile)
	if err != nil {
		t.Fatal(err)
	}
	src = append(src, []byte("- [ ] **1-2**: [shimbaco] 確かめる\n")...)
	if err := os.WriteFile(fx.planFile, src, 0o600); err != nil { //#nosec G703 -- テストがt.TempDirの下に決めたパス
		t.Fatal(err)
	}
	if _, err := fx.done(state.RoleImplementer, `{"location":"`+testReportURL+`"}`); err != nil {
		t.Fatal(err)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionAsk || in.Task != "1-2" {
		t.Fatalf("指示 = %+v、足したタスク1-2の人への確認を期待", in)
	}
	fx.checkTask(t, "1-2")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatal(err)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionReport || in.PreviousReport != testReportURL {
		t.Fatalf("指示 = %+v、前の回の作業報告書を更新する指示を期待", in)
	}
}

func TestFlowReportLegacyState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// stateはflow.jsonの内容。
		state string
		// eventsは所要時間の記録の件数。
		events int
	}{
		{
			name:   "Markdownの作業報告書の版のあとにステップを記録した状態",
			state:  `{"reports":[{"version":1,"events":1,"at":"2026-10-01T00:00:00Z","walkthrough":{"skipped":"理由"}}],"updated_at":"2026-10-01T00:00:00Z"}`,
			events: 2,
		},
		{
			name:   "処理の流れを書いている途中の状態",
			state:  `{"step":"walkthrough","role":"implementer","since":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`,
			events: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newFixture(t, "- [x] **1-1**: [shimbaco] タグを付ける", "- [x] **1-2**: [shimbaco] 確かめる")
			now := time.Now()
			for i := range tt.events {
				e := state.Event{Task: "1-" + string(rune('1'+i)), Step: state.StepAsk, Role: state.RoleImplementer, Started: now.Add(-time.Hour), Ended: now}
				if err := fx.flow.Dir.AppendEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			writeFile(t, fx.flow.Dir.Path(), "flow.json", tt.state)

			// 以前の版の記録はページを持たないため、新しく作ってもらう。
			if in := fx.wait(t, state.RoleImplementer); in.Action != ActionReport || in.PreviousReport != "" {
				t.Fatalf("指示 = %+v、作業報告書を新しく作る指示を期待", in)
			}
			if _, err := fx.done(state.RoleImplementer, `{"location":"`+testReportURL+`"}`); err != nil {
				t.Fatal(err)
			}
			st, err := fx.flow.Dir.Load()
			if err != nil {
				t.Fatal(err)
			}
			last := st.Reports[len(st.Reports)-1]
			if last.Number != len(st.Reports) || last.Location != testReportURL {
				t.Errorf("作業報告書の記録 = %+v", st.Reports)
			}
			b, err := os.ReadFile(fx.flow.Dir.Path() + "/flow.json")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), `"reports"`) || strings.Contains(string(b), `"walkthrough"`) {
				t.Errorf("以前の形式の記録が残っている:\n%s", b)
			}
		})
	}
}

func TestFlowReportLegacyPending(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		appended bool
	}{
		{name: "履歴の追記前"},
		{name: "履歴の追記後", appended: true},
	}
	for _, tt := range cases {
		for _, entry := range []string{"wait", "done"} {
			t.Run(tt.name+"/"+entry, func(t *testing.T) {
				t.Parallel()

				fx := newFixture(t, "- [x] **1-1**: [shimbaco] 確かめる")
				event := state.Event{
					Step: state.Step("walkthrough"), Role: state.RoleImplementer,
					Started: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
					Ended:   time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC),
				}
				if tt.appended {
					if err := fx.flow.Dir.AppendEvent(event); err != nil {
						t.Fatal(err)
					}
				}
				writeFile(t, fx.flow.Dir.Path(), "flow.json", `{"walkthrough":{"skipped":"文書だけのため"},"pending":{"step":"walkthrough","role":"implementer","started":"2026-10-01T00:00:00Z","ended":"2026-10-01T00:01:00Z"},"updated_at":"2026-10-01T00:01:00Z"}`)

				if entry == "wait" {
					if in := fx.wait(t, state.RoleImplementer); in.Action != ActionReport {
						t.Fatalf("再開後の指示 = %+v、作業報告書の指示を期待", in)
					}
				} else if _, err := fx.done(state.RoleImplementer, ""); err != nil {
					t.Fatalf("再開時のDoneのエラー = %v", err)
				}

				events, err := fx.flow.Dir.ReadEvents()
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 1 || events[0] != event {
					t.Fatalf("移行後の履歴 = %+v、元のwalkthroughの記録1件だけを期待", events)
				}
				st, err := fx.flow.Dir.Load()
				if err != nil {
					t.Fatal(err)
				}
				if st.Pending != nil || st.Step != state.StepReport || len(st.Reports) != 0 {
					t.Fatalf("移行後の状態 = %+v、未記録の作業報告書のステップを期待", st)
				}

				fx.writeReport(t)
				events, err = fx.flow.Dir.ReadEvents()
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 2 || events[0] != event || events[1].Step != state.StepReport {
					t.Errorf("作業報告書の完了後の履歴 = %+v、旧履歴とreportの履歴を1件ずつ期待", events)
				}
				if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish || in.Report != testReportURL {
					t.Errorf("作業報告書の完了後の指示 = %+v、作業報告書のURLを持つ終了を期待", in)
				}
			})
		}
	}
}

func TestFlowReportRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"末尾に不正な文字列", "{\"location\":\"" + testReportURL + "\"} garbage", "作業報告書の記録を読み取れません"},
		{"末尾に別のオブジェクト", "{\"location\":\"" + testReportURL + "\"} {\"location\":\"" + testReportURL + "\"}", "作業報告書のJSONは1つだけ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fx := newFixture(t, "- [ ] **1-1**: [shimbaco] 確かめる")
			fx.wait(t, state.RoleImplementer)
			fx.checkTask(t, "1-1")
			if _, err := fx.done(state.RoleImplementer, ""); err != nil {
				t.Fatal(err)
			}
			before, err := fx.flow.Dir.Load()
			if err != nil {
				t.Fatal(err)
			}
			_, err = fx.done(state.RoleImplementer, tt.input)
			assertRejected(t, err, tt.wantErr)
			after, err := fx.flow.Dir.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("拒否後の状態 = %+v、期待値 = %+v", after, before)
			}
			events, err := fx.flow.Dir.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Step != state.StepAsk {
				t.Errorf("拒否後の履歴 = %+v、人が行うタスクの履歴1件を期待", events)
			}
		})
	}
}
