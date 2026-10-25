package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/korylus/tools/internal/plan"
)

func TestParse(t *testing.T) {
	t.Parallel()

	src, err := Parse("docs/private/plans/1_doing/demo.md")
	if err != nil || src != File("docs/private/plans/1_doing/demo.md") {
		t.Errorf("パスのParse() = %#v, %v", src, err)
	}

	const url = "https://wikino.app/s/shimbaco_2/pages/1214"
	src, err = Parse(url)
	if err != nil {
		t.Fatalf("URLのParse() のエラー = %v", err)
	}
	w, ok := src.(*Wikino)
	if !ok || w.URL != url || w.Space != "shimbaco_2" || w.Number != 1214 || w.Run == nil || src.String() != url {
		t.Errorf("URLのParse() = %#v", src)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, arg, want string }{
		{"ページ番号だけ", "1214", "ページ番号 1214 だけでは計画書を指定できません"},
		{"httpのURL", "http://wikino.app/s/shimbaco/pages/1", "を読み取れません"},
		{"別のホスト", "https://example.com/s/shimbaco/pages/1", "を読み取れません"},
		{"ハイフンで始まるスペース", "https://wikino.app/s/-x/pages/1", "を読み取れません"},
		{"数字でないページ番号", "https://wikino.app/s/shimbaco/pages/1a", "を読み取れません"},
		{"ページ番号の後ろに続きがある", "https://wikino.app/s/shimbaco/pages/1/edit", "を読み取れません"},
		{"ページ番号が0", "https://wikino.app/s/shimbaco/pages/0", "のページ番号を読み取れません"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tt.arg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) のエラー = %v、期待値 = %q を含むエラー", tt.arg, err, tt.want)
			}
		})
	}
}

func TestFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "demo.md")
	if err := os.WriteFile(path, []byte("# 計画書\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := File(path)
	if got, err := f.Read(context.Background()); err != nil || got != "# 計画書\n" {
		t.Errorf("Read() = %q, %v", got, err)
	}
	if got, err := f.Name(&plan.Plan{}); err != nil || got != "demo" {
		t.Errorf("Name() = %q, %v、期待値 = demo", got, err)
	}
	if _, err := File(filepath.Join(t.TempDir(), "none.md")).Read(context.Background()); err == nil || !strings.Contains(err.Error(), "計画書を読めません") {
		t.Errorf("無いファイルのRead() のエラー = %v", err)
	}
}

// resultはwikinoコマンドを1回実行した結果。
type result struct {
	stdout, stderr string
	code           int
	err            error
}

// fakeRunはresultsを順に返すRunnerと、呼ばれた引数の記録を返す。
func fakeRun(results ...result) (Runner, *[][]string) {
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, []byte, int, error) {
		calls = append(calls, args)
		r := results[len(calls)-1]
		return []byte(r.stdout), []byte(r.stderr), r.code, r.err
	}
	return run, &calls
}

func TestWikinoRead(t *testing.T) {
	t.Parallel()

	const page = "---\nwikino_title: \"計画書\"\nwikino_number: 12\n---\n\n**計画書名**: demo\n\n## タスクリスト\n"
	const body = "**計画書名**: demo\n\n## タスクリスト\n"
	draftArgs := []string{"draft", "get", "12", "--space=shimbaco", "--md"}
	pageArgs := []string{"page", "get", "12", "--space=shimbaco", "--md"}

	tests := []struct {
		name      string
		results   []result
		wantBody  string
		wantErr   []string
		wantCalls [][]string
	}{
		{
			name:      "下書きがあれば下書きを読む",
			results:   []result{{stdout: page}},
			wantBody:  body,
			wantCalls: [][]string{draftArgs},
		},
		{
			name:      "下書きが無ければページを読む",
			results:   []result{{code: 5}, {stdout: strings.ReplaceAll(page, "\n", "\r\n")}},
			wantBody:  body,
			wantCalls: [][]string{draftArgs, pageArgs},
		},
		{
			name:      "frontmatterが無ければそのまま返す",
			results:   []result{{stdout: body}},
			wantBody:  body,
			wantCalls: [][]string{draftArgs},
		},
		{
			name:      "ページも無ければ見つからないエラー",
			results:   []result{{code: 5}, {code: 5}},
			wantErr:   []string{"計画書 https://wikino.app/s/shimbaco/pages/12 が見つかりません"},
			wantCalls: [][]string{draftArgs, pageArgs},
		},
		{
			name:      "未ログインなら標準エラーを添える",
			results:   []result{{code: 3, stderr: "ログインしてください\n"}},
			wantErr:   []string{"ログインしていないか", "wikino auth status --space=shimbaco", "\nログインしてください"},
			wantCalls: [][]string{draftArgs},
		},
		{
			name:      "レート制限",
			results:   []result{{code: 5}, {code: 8}},
			wantErr:   []string{"公開APIの利用量の上限に当たりました"},
			wantCalls: [][]string{draftArgs, pageArgs},
		},
		{
			name:      "そのほかの終了コード",
			results:   []result{{code: 1}},
			wantErr:   []string{"wikino draft get が終了コード 1 で失敗しました"},
			wantCalls: [][]string{draftArgs},
		},
		{
			name:      "起動できなければそのエラー",
			results:   []result{{err: errors.New("wikinoコマンドが見つかりません")}},
			wantErr:   []string{"wikinoコマンドが見つかりません"},
			wantCalls: [][]string{draftArgs},
		},
		{
			name:      "frontmatterが閉じていなければエラー",
			results:   []result{{stdout: "---\nwikino_number: 12\n"}},
			wantErr:   []string{"frontmatterが閉じていません"},
			wantCalls: [][]string{draftArgs},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run, calls := fakeRun(tt.results...)
			w := &Wikino{URL: "https://wikino.app/s/shimbaco/pages/12", Space: "shimbaco", Number: 12, Run: run}
			got, err := w.Read(context.Background())
			if !reflect.DeepEqual(*calls, tt.wantCalls) {
				t.Errorf("wikinoの引数 = %q、期待値 = %q", *calls, tt.wantCalls)
			}
			if len(tt.wantErr) == 0 {
				if err != nil || got != tt.wantBody {
					t.Errorf("Read() = %q, %v、期待値 = %q", got, err, tt.wantBody)
				}
				return
			}
			if err == nil {
				t.Fatalf("Read() = %q、期待値 = エラー", got)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("エラーに %q が無い:\n%v", want, err)
				}
			}
		})
	}
}

func TestWikinoName(t *testing.T) {
	t.Parallel()

	w := &Wikino{URL: "https://wikino.app/s/shimbaco/pages/12"}
	if got, err := w.Name(&plan.Plan{Name: "demo"}); err != nil || got != "demo" {
		t.Errorf("Name() = %q, %v、期待値 = demo", got, err)
	}
	if _, err := w.Name(&plan.Plan{}); err == nil || !strings.Contains(err.Error(), "計画書名の行がありません") {
		t.Errorf("行が無いときのName() のエラー = %v", err)
	}
}
