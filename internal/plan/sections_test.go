package plan

import (
	"reflect"
	"testing"
)

// sectionsPlanは節の本文と実装判断ログの論点を読む計画書の例。
const sectionsPlan = "# 例の作業計画書\n" +
	"\n" +
	"## 合意済みの設計制約\n" +
	"\n" +
	"<!--\n" +
	"書き方のガイドライン\n" +
	"-->\n" +
	"\n" +
	"### DBスキーマ\n" +
	"\n" +
	"postsの列は追加だけにする。\n" +
	"\n" +
	"## 実装判断ログ\n" +
	"\n" +
	"<!--\n" +
	"#### コメントの中の論点は読まない?\n" +
	"-->\n" +
	"\n" +
	"### タスク1-1: 投稿の公開範囲を保存する\n" +
	"\n" +
	"#### 列の型をどうするか?\n" +
	"\n" +
	"```markdown\n" +
	"#### コードブロックの中の論点は読まない?\n" +
	"```\n" +
	"\n" +
	"#### 既定値は?\n" +
	"\n" +
	"### 見出しの形が違う節\n" +
	"\n" +
	"#### タスクに属さない論点は読まない?\n" +
	"\n" +
	"### タスクfb1-1: 保存後に編集画面へ戻す\n" +
	"\n" +
	"#### 戻る先は?\n" +
	"\n" +
	"## タスクリスト\n" +
	"\n" +
	"- [x] **1-1**: [Go] 投稿の公開範囲を保存する\n" +
	"\n" +
	"#### タスクリストの見出しは読まない?\n"

func TestSection(t *testing.T) {
	t.Parallel()

	got, ok := Section(sectionsPlan, "合意済みの設計制約")
	if !ok {
		t.Fatal("「合意済みの設計制約」節が見つからない")
	}
	if want := "### DBスキーマ\n\npostsの列は追加だけにする。"; got != want {
		t.Errorf("本文 = %q、期待値 = %q", got, want)
	}
	if _, ok := Section(sectionsPlan, "実装判断ログ"); !ok {
		t.Error("「実装判断ログ」節が見つからない")
	}
	if _, ok := Section(sectionsPlan, "設計"); ok {
		t.Error("無い節が見つかった")
	}
}

func TestDecisionTopics(t *testing.T) {
	t.Parallel()

	want := map[string][]string{
		"1-1":   {"列の型をどうするか?", "既定値は?"},
		"fb1-1": {"戻る先は?"},
	}
	if got := DecisionTopics(sectionsPlan); !reflect.DeepEqual(got, want) {
		t.Errorf("論点 = %v、期待値 = %v", got, want)
	}
}
