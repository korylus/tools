package flow

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/korylus/tools/internal/flow/state"
)

func TestParseWalkthrough(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "docs/private/plans/files/demo/walkthrough.md", "# 処理の流れ\n")
	tests := []struct {
		name    string
		input   string
		want    state.Walkthrough
		wantErr string
	}{
		{name: "URLを受け付ける", input: `{"location":"https://wikino.app/s/demo/pages/1"}`, want: state.Walkthrough{Location: "https://wikino.app/s/demo/pages/1"}},
		{name: "あるファイルのパスを受け付ける", input: `{"location":" docs/private/plans/files/demo/walkthrough.md "}`, want: state.Walkthrough{Location: "docs/private/plans/files/demo/walkthrough.md"}},
		{name: "書かなかった理由を受け付ける", input: `{"skipped":"文面だけを直したため"}`, want: state.Walkthrough{Skipped: "文面だけを直したため"}},
		{name: "末尾の空白を受け付ける", input: "{\"skipped\":\"文面だけを直したため\"} \n\t", want: state.Walkthrough{Skipped: "文面だけを直したため"}},
		{name: "空の入力は書き忘れ", input: "\n", wantErr: "標準入力からJSONで渡してください"},
		{name: "どちらも無い", input: `{}`, wantErr: "標準入力からJSONで渡してください"},
		{name: "両方ある", input: `{"location":"https://example.com","skipped":"理由"}`, wantErr: "どちらか一方だけ"},
		{name: "空のlocation", input: `{"location":" "}`, wantErr: "location に、処理の流れのURLかパスを書いてください"},
		{name: "空のskipped", input: `{"skipped":""}`, wantErr: "skipped に、処理の流れを書かなかった理由を書いてください"},
		{name: "無いファイル", input: `{"location":"docs/missing.md"}`, wantErr: "処理の流れのファイル docs/missing.md がありません"},
		{name: "ディレクトリ", input: `{"location":"docs/private"}`, wantErr: "ディレクトリです"},
		{name: "絶対パス", input: `{"location":"/etc/hosts"}`, wantErr: "https:// で始まるURLか、プロジェクトのルートからの相対パス"},
		{name: "ルートの外", input: `{"location":"../outside.md"}`, wantErr: "https:// で始まるURLか、プロジェクトのルートからの相対パス"},
		{name: "httpのURL", input: `{"location":"http://example.com/a"}`, wantErr: "がありません"},
		{name: "未知のキー", input: `{"url":"https://example.com"}`, wantErr: "読み取れません"},
		{name: "JSONでない", input: `{`, wantErr: "読み取れません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseWalkthrough(strings.NewReader(tt.input), root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("エラー = %v、期待値 = %q を含むエラー", err, tt.wantErr)
				}
				return
			}
			if err != nil || *got != tt.want {
				t.Errorf("parseWalkthrough() = %+v, %v、期待値 = %+v", got, err, tt.want)
			}
		})
	}
}

func TestFlowWalkthroughSkipped(t *testing.T) {
	t.Parallel()

	fx := newFixture(t, "- [ ] **1-1**: [shimbaco] タグを付ける")
	fx.wait(t, state.RoleImplementer)
	fx.checkTask(t, "1-1")
	if _, err := fx.done(state.RoleImplementer, ""); err != nil {
		t.Fatal(err)
	}

	// ブランチを作ったタスクが無くても、処理の流れのステップを経て作業報告書を作る。
	in := fx.wait(t, state.RoleImplementer)
	if in.Action != ActionWalkthrough || len(in.Branches) != 0 || !strings.Contains(in.String(), "- なし (ブランチを作ったタスクがありません)") {
		t.Fatalf("指示 = %+v\n%s", in, in.String())
	}
	_, err := fx.done(state.RoleImplementer, "[]")
	assertRejected(t, err, "処理の流れの記録を読み取れません")
	if _, err := fx.done(state.RoleImplementer, `{"skipped":"人が行うタスクだけの計画書のため"}`); err != nil {
		t.Fatal(err)
	}
	if out := fx.readReport(t, 1); !strings.Contains(out, "- 処理の流れ: 書いていません (人が行うタスクだけの計画書のため)\n") {
		t.Errorf("作業報告書に書かなかった理由が無い:\n%s", out)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish {
		t.Errorf("指示 = %+v、終了を期待", in)
	}
}

func TestFlowWalkthroughCompatibility(t *testing.T) {
	t.Parallel()

	// 処理の流れのステップを足す前に第1版を作り、そのあとにステップを記録した状態。
	fx := newFixture(t, "- [x] **1-1**: [shimbaco] タグを付ける", "- [x] **1-2**: [shimbaco] 確かめる")
	now := time.Now()
	for _, id := range []string{"1-1", "1-2"} {
		if err := fx.flow.Dir.AppendEvent(state.Event{Task: id, Step: state.StepAsk, Role: state.RoleImplementer, Started: now.Add(-time.Hour), Ended: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.flow.Dir.Save(&state.State{Reports: []state.ReportRecord{{Version: 1, Events: 1, At: now}}}, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.flow.Dir.ReportPath(1), []byte("# 作業報告書 第1版: demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// reportで次の版を作るときは、処理の流れを記録なしとして作る。
	path, err := fx.flow.Report(context.Background())
	if err != nil || path != fx.flow.Dir.ReportPath(2) {
		t.Fatalf("Report() = %q, %v", path, err)
	}
	if out := fx.readReport(t, 2); !strings.Contains(out, "- 処理の流れ: 記録なし\n") {
		t.Errorf("作業報告書に記録なしが無い:\n%s", out)
	}
	st, err := fx.flow.Dir.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Reports) != 2 || st.Reports[1].Walkthrough != nil {
		t.Errorf("作業報告書の記録 = %+v", st.Reports)
	}
	if in := fx.wait(t, state.RoleImplementer); in.Action != ActionFinish {
		t.Errorf("指示 = %+v、新しいステップが無いため終了を期待", in)
	}
}

func TestFlowWalkthroughRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"末尾に不正な文字列", "{\"skipped\":\"文面だけのため\"} garbage", "処理の流れの記録を読み取れません"},
		{"末尾に別のオブジェクト", "{\"skipped\":\"文面だけのため\"} {\"location\":\"https://wikino.app/s/demo/pages/1\"}", "処理の流れのJSONは1つだけ"},
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
			if _, err := os.Stat(fx.flow.Dir.ReportPath(1)); !os.IsNotExist(err) {
				t.Errorf("拒否後の報告書のStatのエラー = %v、ファイルが無いエラーを期待", err)
			}
		})
	}
}
