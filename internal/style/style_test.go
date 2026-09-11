package style

import (
	"strings"
	"testing"
)

// sectionsOfは違反の節番号の一覧を返す。
func sectionsOf(vs []Violation) []string {
	got := make([]string, len(vs))
	for i, v := range vs {
		got[i] = v.Section
	}
	return got
}

func TestCheckLineParen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		text         string
		wantSections []string
	}{
		{
			name:         "全角丸括弧を検出する",
			text:         "半角英数字とアンダースコアのみ（最大20文字）",
			wantSections: []string{"3.1"},
		},
		{
			name:         "同じ行に複数あっても1件に畳む",
			text:         "テスト（1）とテスト（2）",
			wantSections: []string{"3.1"},
		},
		{
			name:         "開き括弧だけでも検出する",
			text:         "テスト（最大20文字",
			wantSections: []string{"3.1"},
		},
		{
			name:         "半角丸括弧は違反ではない",
			text:         "半角英数字とアンダースコアのみ (最大20文字)",
			wantSections: nil,
		},
		{
			name:         "インラインコードで囲んだ悪い例は対象外",
			text:         "- `半角英数字とアンダースコアのみ（最大20文字）` <!-- 全角丸括弧を使っている -->",
			wantSections: nil,
		},
		{
			name:         "閉じていないバッククォートは以降を対象外にしない",
			text:         "`テスト（最大20文字）",
			wantSections: []string{"3.1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := sectionsOf(CheckLine(tt.text))
			if !equalStrings(got, tt.wantSections) {
				t.Errorf("CheckLine(%q) が検出した節番号 = %v、期待値 = %v", tt.text, got, tt.wantSections)
			}
		})
	}
}

func TestCheckLineSpacing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		text         string
		wantSections []string
	}{
		{
			name:         "英字の後ろのスペースを検出する",
			text:         "REST API の認証",
			wantSections: []string{"3.2"},
		},
		{
			name:         "英字の前のスペースを検出する",
			text:         "認証に REST APIを使う",
			wantSections: []string{"3.2"},
		},
		{
			name:         "数字の後ろのスペースを検出する",
			text:         "最大 20 文字まで入力できる",
			wantSections: []string{"3.2"},
		},
		{
			name:         "長音符で終わる語の後ろのスペースを検出する",
			text:         "サーバー 3台を並べる",
			wantSections: []string{"3.2"},
		},
		{
			name:         "スペースを詰めた表記は違反ではない",
			text:         "REST APIの認証で最大20文字まで入力できる",
			wantSections: nil,
		},
		{
			name:         "半角英数字どうしの間のスペースは残す",
			text:         "REST API V1の仕様",
			wantSections: nil,
		},
		{
			name:         "半角丸括弧の両端のスペースは §3.1が優先する",
			text:         "ユーザーIDを取得 (削除済みユーザーは0を返す)",
			wantSections: nil,
		},
		{
			name:         "地の文にそのまま置くURLの後ろのスペースは対象外",
			text:         "https://example.com/ で配信している",
			wantSections: nil,
		},
		{
			name:         "パスで終わらないURLの後ろのスペースも対象外",
			text:         "https://example.com で配信している",
			wantSections: nil,
		},
		{
			name:         "URLの前のスペースも対象外",
			text:         "配信元は https://example.com である",
			wantSections: nil,
		},
		{
			name:         "Markdownのリンクに含まれるURLも対象外",
			text:         "詳細は [ガイドライン](https://example.com/guide) を参照する",
			wantSections: nil,
		},
		{
			name:         "インラインコードの中は対象外",
			text:         "悪い例は `最大 20 文字まで入力できる` と書いたもの",
			wantSections: nil,
		},
		{
			name:         "インラインコードの直後のスペースは対象外",
			text:         "`-base=<ref>` を渡すと差分だけを検査する",
			wantSections: nil,
		},
		{
			name:         "見出しの番号と見出し文の間は対象外",
			text:         "### 3.2 英数字と日本語の間のスペース",
			wantSections: nil,
		},
		{
			name:         "番号付きリストの番号と項目の間は対象外",
			text:         "1. 英数字と日本語の間のスペースを詰める",
			wantSections: nil,
		},
		{
			name:         "括弧付きの番号も対象外",
			text:         "(1) 英数字と日本語の間のスペースを詰める",
			wantSections: nil,
		},
		{
			name:         "数字だけのトークンは番号とみなさない",
			text:         "- 3 つの理由がある",
			wantSections: []string{"3.2"},
		},
		{
			name:         "行の途中の番号は見出しの番号とみなさない",
			text:         "詳細は §3.2 英数字と日本語の間のスペースを参照する",
			wantSections: []string{"3.2"},
		},
		{
			name:         "連続したスペースは整列とみなして対象外",
			text:         "md --all    全ファイルを対象にする",
			wantSections: nil,
		},
		{
			name:         "タブ区切りは整列とみなして対象外",
			text:         "md --all\t全ファイルを対象にする",
			wantSections: nil,
		},
		{
			name:         "行頭・行末のスペースは対象外",
			text:         " 日本語のみの行 ",
			wantSections: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := sectionsOf(CheckLine(tt.text))
			if !equalStrings(got, tt.wantSections) {
				t.Errorf("CheckLine(%q) が検出した節番号 = %v、期待値 = %v", tt.text, got, tt.wantSections)
			}
		})
	}
}

// TestCheckLineReportsBothSectionsは1行が両方の節に違反したとき、節番号の順に
// 2件返すことを確認する。
func TestCheckLineReportsBothSections(t *testing.T) {
	t.Parallel()

	got := sectionsOf(CheckLine("フェーズ8-1（i18n キー命名規則）"))
	if !equalStrings(got, []string{"3.1", "3.2"}) {
		t.Errorf("検出した節番号 = %v、期待値 = [3.1 3.2]", got)
	}
}

// TestCheckLineMessagesはメッセージに節番号と直し方が入っていることを確認する。
func TestCheckLineMessages(t *testing.T) {
	t.Parallel()

	vs := CheckLine("フェーズ8-1（i18n キー命名規則）")
	if len(vs) != 2 {
		t.Fatalf("違反数 = %d、期待値 = 2", len(vs))
	}
	if !strings.Contains(vs[0].Message, "§3.1") || !strings.Contains(vs[0].Message, "全角丸括弧") {
		t.Errorf("§3.1のメッセージ = %q、節番号と「全角丸括弧」を含むことを期待", vs[0].Message)
	}
	if !strings.Contains(vs[1].Message, "§3.2") || !strings.Contains(vs[1].Message, "半角スペース") {
		t.Errorf("§3.2のメッセージ = %q、節番号と「半角スペース」を含むことを期待", vs[1].Message)
	}
}

func TestCheckLineEmpty(t *testing.T) {
	t.Parallel()

	for _, text := range []string{"", " ", "`", "englishのみ"} {
		if vs := CheckLine(text); len(vs) != 0 {
			t.Errorf("CheckLine(%q) = %v、違反なしを期待", text, vs)
		}
	}
}

func TestIsJapanese(t *testing.T) {
	t.Parallel()

	tests := []struct {
		r    rune
		want bool
	}{
		{'あ', true},
		{'ア', true},
		{'漢', true},
		{'ー', true},
		{'々', true},
		{'a', false},
		{'1', false},
		{'(', false},
		{'。', false},
	}
	for _, tt := range tests {
		if got := isJapanese(tt.r); got != tt.want {
			t.Errorf("isJapanese(%q) = %v、期待値 = %v", tt.r, got, tt.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCheckLineExcludedRanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		text         string
		wantSections []string
	}{
		{name: "2個のバッククォート", text: "悪い例は ``最大 20 文字（例）`` と書く"},
		{name: "異なる連続数を内包する", text: "悪い例は ```最大 `20` ``文字（例）`` ``` と書く"},
		{name: "閉じ記号の連続数が異なる", text: "``最大 20 文字（例）```", wantSections: []string{"3.1", "3.2"}},
		{name: "閉じていない区切りの後ろに別のコードがある", text: "``前置き `最大 20 文字（例）` 本文は正常"},
		{name: "コードの前後の違反は残す", text: "最大 20 文字は ``最大 20 文字（例）`` の例（要確認）", wantSections: []string{"3.1", "3.2"}},
		{name: "日本語のURLパス", text: "詳細は https://example.com/資料2026 を参照する"},
		{name: "日本語のホストとクエリ", text: "詳細は https://例え.example/資料?q=項目2026 を参照する"},
		{name: "URL内の全角丸括弧", text: "詳細は https://example.com/資料（例）2026 を参照する"},
		{name: "URL後の空白で地の文に戻る", text: "https://example.com/資料2026 最大 20 文字（例）", wantSections: []string{"3.1", "3.2"}},
		{name: "全角スペースでもURLを終了する", text: "https://example.com/資料2026　最大 20 文字（例）", wantSections: []string{"3.1", "3.2"}},
		{name: "リンクの外の違反は残す", text: "[資料](https://example.com/資料2026)は最大 20 文字（例）", wantSections: []string{"3.1", "3.2"}},
		{name: "句点の後ろの違反は残す", text: "https://example.com/資料2026。最大 20 文字（例）", wantSections: []string{"3.1", "3.2"}},
		{name: "山括弧の外の違反は残す", text: "<https://example.com/資料2026>は最大 20 文字（例）", wantSections: []string{"3.1", "3.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sectionsOf(CheckLine(tt.text)); !equalStrings(got, tt.wantSections) {
				t.Errorf("検出した節番号 = %v、期待値 = %v", got, tt.wantSections)
			}
		})
	}
}

// TestCheckLineNumberTokenLimitは、行頭の数量トークンを見出しの番号とみなす
// 既知の制限を固定する。
// 番号とみなすのは区切りか末尾の記号を持つトークンだけなので、`1.5` や `8-1` の
// ように数量・範囲が同じ形を取ると、行頭に置かれたときだけ検出できない。
// 見出し文らしさで番号と数量を見分ける条件は誤検出を招くため、現状維持とした。
func TestCheckLineNumberTokenLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		text         string
		wantSections []string
	}{
		{name: "行頭の小数は番号とみなして検出しない", text: "1.5 倍の速度で処理する"},
		{name: "行頭の範囲は番号とみなして検出しない", text: "8-1 件の課題が残る"},
		{name: "行の途中の小数は検出する", text: "速度は 1.5 倍になる", wantSections: []string{"3.2"}},
		{name: "行の途中の範囲は検出する", text: "残りは 8-1 件になる", wantSections: []string{"3.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sectionsOf(CheckLine(tt.text)); !equalStrings(got, tt.wantSections) {
				t.Errorf("検出した節番号 = %v、期待値 = %v", got, tt.wantSections)
			}
		})
	}
}

// TestCheckMaskedLineは構文解析済みの本文にコード除外を重ねず、URLと番号の
// 除外・節番号順での報告をCheckLineと共有することを確認する。
func TestCheckMaskedLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		text         string
		wantSections []string
	}{
		{"区切り記号の間の本文を検査する", "` 本文（違反） Go 版 `   `", []string{"3.1", "3.2"}},
		{"エスケープされたバッククォート", "\\`本文（違反） Go 版\\`", []string{"3.1", "3.2"}},
		{"マスク済みの本文と区切り記号", "`        `", nil},
		{"URL内の全角丸括弧とURL前後の空白は除外する", "参照先は https://example.com/資料（例）2026 である", nil},
		{"URLの後ろの本文は検査する", "https://example.com/資料2026 本文（違反） Go 版", []string{"3.1", "3.2"}},
		{"見出し番号の後ろの空白は除外する", "### 3.2 英数字と日本語", nil},
		{"見出し番号以外の空白は検査する", "### 3.2 英数字と Go 版", []string{"3.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sectionsOf(CheckMaskedLine(tt.text)); !equalStrings(got, tt.wantSections) {
				t.Errorf("検出した節番号 = %v、期待値 = %v", got, tt.wantSections)
			}
		})
	}
}
