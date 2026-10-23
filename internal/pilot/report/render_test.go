package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	t.Parallel()

	r, err := Build(firstInput())
	if err != nil {
		t.Fatal(err)
	}
	r.Items[0].Title = `<script>alert("x")</script>`
	r.Preparation.Switched = false
	r.Preparation.Skipped = []Branch{{Branch: "p-1-1", Commit: "c11"}}
	v := &Verification{Version: 1, SavedAt: t0, Comment: "全体の<b>コメント</b>", Items: map[string]ItemResult{
		"task/1-1/2": {Status: StatusNG, Comment: "戻らない"},
	}}
	var buf bytes.Buffer
	if err := Render(&buf, r, v); err != nil {
		t.Fatalf("Renderのエラー = %v", err)
	}
	html := buf.String()

	for _, want := range []string{
		`<html lang="ja">`,
		`<caption>ブランチの順序</caption>`,
		`<th scope="col">ブランチ</th>`,
		`<form method="post">`,
		`<input type="hidden" name="version" value="1">`,
		`<legend>#2 の確認結果</legend>`,
		`<input type="radio" name="status:task/1-1/2" value="ng" checked>`,
		`<input type="radio" name="status:task/1-1/1" value="" checked>`,
		`<label for="comment-2">#2 へのコメント</label>`,
		`<textarea id="comment-2" name="comment:task/1-1/2" rows="2">戻らない</textarea>`,
		`<textarea id="overall" name="overall" rows="4">全体の&lt;b&gt;コメント&lt;/b&gt;</textarea>`,
		`<a href="/posts/new">/posts/new</a>`,
		`<code>make check</code>`,
		`&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;`,
		`<a href="#item-3">#3</a>`,
		`宣言の無い変更`,
		"</table>\n</section>\n\n<section aria-labelledby=\"stops\">\n<h2 id=\"stops\">止まった履歴</h2>",
		`<code>git switch p-1-1 &amp;&amp; git merge --ff-only c11</code>`,
		`<code>git switch p-1-2</code>`,
		`<code>make -C go db-migrate</code>`,
		`列の型をどうするか?`,
		`1時間16分0秒`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTMLに %q が含まれない", want)
		}
	}
	if strings.Contains(html, "<script") {
		t.Error("HTMLにscript要素が含まれる")
	}
	if strings.Contains(html, "<details>") {
		t.Error("第1版に確認済みの項目の折りたたみがある")
	}
}

func TestRender_Confirmed(t *testing.T) {
	t.Parallel()

	r, err := Build(firstInput())
	if err != nil {
		t.Fatal(err)
	}
	r.Items[0].Confirmed = true
	r.Items[0].Previous = &PrevItem{Number: 1, Status: StatusOK}
	var buf bytes.Buffer
	if err := Render(&buf, r, nil); err != nil {
		t.Fatalf("Renderのエラー = %v", err)
	}
	if !strings.Contains(buf.String(), "<summary>前の版までに確認済みの項目 (1件)</summary>") {
		t.Error("確認済みの項目が折りたたまれていない")
	}
}

func TestWriteAndLoad(t *testing.T) {
	t.Parallel()

	dir := Dir(t.TempDir(), "p")
	r, err := Build(firstInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, r, nil); err != nil {
		t.Fatalf("Writeのエラー = %v", err)
	}
	got, err := Load(dir, 1)
	if err != nil {
		t.Fatalf("Loadのエラー = %v", err)
	}
	if got.Version != 1 || len(got.Items) != len(r.Items) || got.Timing.Total != r.Timing.Total || !got.CreatedAt.Equal(r.CreatedAt) {
		t.Errorf("読んだ作業報告書 = %+v", got)
	}
	html, err := os.ReadFile(HTMLPath(dir, 1)) //#nosec G304 -- t.TempDirの下のファイル
	if err != nil || !bytes.Contains(html, []byte("作業報告書 第1版")) {
		t.Errorf("HTML = %q (%v)", html, err)
	}

	if _, err := LoadVerification(dir, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("保存前のLoadVerificationのエラー = %v、期待値 = fs.ErrNotExist", err)
	}
	v := &Verification{Version: 1, Items: map[string]ItemResult{"task/1-1/1": {Status: StatusOK}}, SavedAt: t0.Add(time.Hour)}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(VerificationPath(dir, 1), b); err != nil {
		t.Fatal(err)
	}
	gotV, err := LoadVerification(dir, 1)
	if err != nil || gotV.Items["task/1-1/1"].Status != StatusOK {
		t.Errorf("動作確認の結果 = %+v (%v)", gotV, err)
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	for d, want := range map[time.Duration]string{
		0:                                       "0秒",
		1500 * time.Millisecond:                 "1秒",
		2*time.Minute + 3*time.Second:           "2分3秒",
		time.Hour + 2*time.Minute + time.Second: "1時間2分1秒",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q、期待値 = %q", d, got, want)
		}
	}
}

func TestRender_InitialAndSavedResults(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		confirmed    bool
		verification *Verification
		want         ItemResult
	}{
		{name: "確認済みは初期値がOK", confirmed: true, want: ItemResult{Status: StatusOK}},
		{name: "結果のマップがnilでも確認済みはOK", confirmed: true, verification: &Verification{}, want: ItemResult{Status: StatusOK}},
		{name: "他の項目だけ保存済みでも確認済みはOK", confirmed: true, verification: &Verification{Items: map[string]ItemResult{"other": {Status: StatusNG}}}, want: ItemResult{Status: StatusOK}},
		{name: "保存済みのOKとコメントを優先", confirmed: true, verification: &Verification{Items: map[string]ItemResult{"item": {Status: StatusOK, Comment: "確認した"}}}, want: ItemResult{Status: StatusOK, Comment: "確認した"}},
		{name: "保存済みのNGを優先", confirmed: true, verification: &Verification{Items: map[string]ItemResult{"item": {Status: StatusNG, Comment: "再確認でNG"}}}, want: ItemResult{Status: StatusNG, Comment: "再確認でNG"}},
		{name: "保存済みの未確認を優先", confirmed: true, verification: &Verification{Items: map[string]ItemResult{"item": {}}}},
		{name: "過去にOKでも再確認対象は未確認"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{Version: 2, Items: []Item{{ID: "item", Number: 1, Title: "確認項目", Confirmed: tc.confirmed, Previous: &PrevItem{Number: 1, Status: StatusOK}}}}
			var buf bytes.Buffer
			if err := Render(&buf, r, tc.verification); err != nil {
				t.Fatalf("Renderのエラー = %v", err)
			}
			html := buf.String()
			for _, status := range []string{StatusOK, StatusNG, ""} {
				input := `name="status:item" value="` + status + `" checked`
				if got, want := strings.Contains(html, input), status == tc.want.Status; got != want {
					t.Errorf("ステータス%qの選択 = %v、期待値 = %v", status, got, want)
				}
			}
			if want := `<textarea id="comment-1" name="comment:item" rows="2">` + tc.want.Comment + `</textarea>`; !strings.Contains(html, want) {
				t.Errorf("コメント欄に期待値が含まれない: %q", want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()

	rep := &Report{Items: []Item{
		{ID: "task/1-1/1", Number: 1},
		{ID: "task/1-1/2", Number: 2},
		{ID: "task/1-2/1", Number: 3},
		{ID: "protected/schema", Number: 4, Confirmed: true},
		{ID: "task/1-3/1", Number: 5, Confirmed: true},
	}}
	v := &Verification{Items: map[string]ItemResult{
		"task/1-1/1": {Status: StatusOK},
		"task/1-1/2": {Status: StatusNG, Comment: "違う"},
		// 確認済みの項目でも、NGを付け直したらNGとして扱う。
		"task/1-3/1": {Status: StatusNG},
	}}
	pending, ng := Classify(rep, v)
	numbers := func(items []Item) []int {
		var res []int
		for _, it := range items {
			res = append(res, it.Number)
		}
		return res
	}
	if got := numbers(pending); !slices.Equal(got, []int{3}) {
		t.Errorf("未確認の項目 = %v、期待値 = [3]", got)
	}
	if got := numbers(ng); !slices.Equal(got, []int{2, 5}) {
		t.Errorf("NGの項目 = %v、期待値 = [2 5]", got)
	}
}
